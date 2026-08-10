package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	apiv1 "viper/api/gen/v1"
	"viper/internal/chaos"
	"viper/internal/checker"
	"viper/internal/observability"
	"viper/internal/raft"
	"viper/internal/security"
	"viper/internal/service"
	"viper/internal/statemachine"
	"viper/internal/storage"
	"viper/internal/transport"
	viperclient "viper/pkg/client"
	"viper/pkg/contracts"

	"google.golang.org/grpc/credentials"
)

type tlsFiles struct {
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
	CA          string `json:"ca"`
}

type serveConfig struct {
	NodeID                  raft.NodeID      `json:"node_id"`
	DataDir                 string           `json:"data_dir"`
	PeerListen              string           `json:"peer_listen"`
	ClientListen            string           `json:"client_listen"`
	Peers                   []transport.Peer `json:"peers"`
	PeerTLS                 tlsFiles         `json:"peer_tls"`
	ClientTLS               tlsFiles         `json:"client_tls"`
	AllowedClients          []string         `json:"allowed_clients"`
	AllowedAdministrators   []string         `json:"allowed_administrators"`
	TickIntervalMillis      int              `json:"tick_interval_millis"`
	MaxConcurrentRequests   int              `json:"max_concurrent_requests"`
	MaxRequestsPerSecond    int              `json:"max_requests_per_second"`
	MaxInFlightPeerRequests int              `json:"max_in_flight_peer_requests"`
	MetricsListen           string           `json:"metrics_listen"`
	ShutdownTimeoutMillis   int              `json:"shutdown_timeout_millis"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo()
	case "serve":
		err = runServe(os.Args[2:])
	case "check-history":
		err = runCheckHistory(os.Args[2:])
	case "validate-schedule":
		err = runValidateSchedule(os.Args[2:])
	case "verify-backup":
		err = runVerifyBackup(os.Args[2:])
	case "export-backup":
		err = runExportBackup(os.Args[2:])
	case "restore-backup":
		err = runRestoreBackup(os.Args[2:])
	case "version":
		err = runVersion()
	case "probe":
		err = runProbe(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: viper demo | serve -config path | check-history path | validate-schedule path | verify-backup path | export-backup -data-dir path -output path | restore-backup -data-dir path -backup path | probe -target host:port -ca path -certificate path -key path -server-name name | version")
}

var (
	version     = "dev"
	buildCommit = "unknown"
	buildDate   = "unknown"
)

func runDemo() error {
	ids := []raft.NodeID{"node-1", "node-2", "node-3"}
	clock := raft.NewManualClock(time.Unix(0, 0))
	peers := []transport.Peer{
		{ID: ids[0], Address: "memory://node-1"},
		{ID: ids[1], Address: "memory://node-2"},
		{ID: ids[2], Address: "memory://node-3"},
	}
	clusters := make(map[raft.NodeID]*transport.StaticCluster, len(ids))
	for _, id := range ids {
		cluster, err := transport.NewStaticCluster(id, peers)
		if err != nil {
			return err
		}
		clusters[id] = cluster
	}
	network, err := transport.NewInMemoryNetwork(clusters[ids[0]], 16)
	if err != nil {
		return err
	}
	defer network.Close()
	nodes := make(map[raft.NodeID]*raft.Node, len(ids))
	servers := make(map[raft.NodeID]*service.KVServer, len(ids))
	for _, id := range ids {
		node, nodeErr := raft.New(raft.Config{
			ID: id, Peers: ids, Clock: clock,
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 100 * time.Millisecond,
			HeartbeatInterval:  10 * time.Millisecond,
			RPCTimeout:         100 * time.Millisecond,
		})
		if nodeErr != nil {
			return nodeErr
		}
		nodes[id] = node
		machine := statemachine.New()
		server, serverErr := service.NewKVServer(node, machine)
		if serverErr != nil {
			return serverErr
		}
		servers[id] = server
	}
	for _, id := range ids {
		peerTransport, bindErr := network.Bind(clusters[id], nodes[id])
		if bindErr != nil {
			return bindErr
		}
		if attachErr := nodes[id].AttachTransport(peerTransport); attachErr != nil {
			return attachErr
		}
	}
	defer func() {
		for _, node := range nodes {
			_ = node.Close()
		}
	}()
	clock.Advance(100 * time.Millisecond)
	if err := nodes[ids[0]].Tick(); err != nil {
		return err
	}
	if nodes[ids[0]].State().Role != raft.RoleLeader {
		return fmt.Errorf("demo election did not produce a leader")
	}
	if _, err := servers[ids[0]].Put(context.Background(), &apiv1.PutRequest{
		ClientId: "demo-client", RequestId: "demo-put-1", Key: []byte("demo-key"), Value: []byte("demo-value"),
	}); err != nil {
		return err
	}
	read, err := servers[ids[0]].Get(context.Background(), &apiv1.GetRequest{Key: []byte("demo-key"), Consistency: apiv1.ConsistencyMode_CONSISTENCY_MODE_LINEARIZABLE})
	if err != nil {
		return err
	}
	result := map[string]any{"leader": string(ids[0]), "found": read.GetFound(), "revision": read.GetRevision()}
	encoded, _ := json.Marshal(result)
	fmt.Println(string(encoded))
	return nil
}

func runServe(arguments []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to a JSON server configuration")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("serve requires -config")
	}
	configData, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read server configuration: %w", err)
	}
	if len(configData) > 1<<20 {
		return fmt.Errorf("server configuration exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(configData))
	decoder.DisallowUnknownFields()
	var config serveConfig
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode server configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("server configuration contains trailing data")
	}
	if err := validateServeConfig(config); err != nil {
		return err
	}
	applyServeDefaults(&config)
	cluster, err := transport.NewStaticCluster(config.NodeID, config.Peers)
	if err != nil {
		return err
	}
	store, err := storage.Open(config.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	machine := statemachine.New()
	node, err := raft.New(raft.Config{ID: config.NodeID, Peers: memberNodeIDs(cluster), Store: store, Restore: func(snapshot raft.Snapshot) error { return machine.Restore(snapshot.Data) }})
	if err != nil {
		return err
	}
	server, err := service.NewKVServer(node, machine)
	if err != nil {
		return err
	}
	peerCertificate, peerRoots, err := loadTLSFiles(config.PeerTLS)
	if err != nil {
		return fmt.Errorf("load peer TLS: %w", err)
	}
	peerServerConfig, err := security.PeerServerTLSConfig(peerCertificate, peerRoots, memberIDs(cluster))
	if err != nil {
		return err
	}
	peerGRPC, err := transport.NewPeerGRPCServer(cluster, node, transport.GRPCServerConfig{
		Credentials:   credentials.NewTLS(peerServerConfig),
		MaxConcurrent: config.MaxInFlightPeerRequests,
	})
	if err != nil {
		return err
	}
	peerListener, err := net.Listen("tcp", config.PeerListen)
	if err != nil {
		return fmt.Errorf("listen for peers: %w", err)
	}
	peerServeErrors := make(chan error, 1)
	go func() { peerServeErrors <- peerGRPC.Serve(peerListener) }()
	peerTransport, err := transport.DialGRPC(context.Background(), cluster, transport.GRPCDialConfig{
		CredentialsForPeer: func(peer transport.Peer) (credentials.TransportCredentials, error) {
			peerConfig, configErr := security.PeerTLSConfig(peerCertificate, peerRoots, string(peer.ID), false)
			if configErr != nil {
				return nil, configErr
			}
			return credentials.NewTLS(peerConfig), nil
		},
	})
	if err != nil {
		peerGRPC.Stop()
		_ = peerListener.Close()
		return err
	}
	if err := node.AttachTransport(peerTransport); err != nil {
		_ = peerTransport.Close()
		peerGRPC.Stop()
		_ = peerListener.Close()
		return err
	}
	defer func() {
		peerGRPC.Stop()
		_ = peerListener.Close()
		_ = peerTransport.Close()
	}()
	publicCertificate, publicRoots, err := loadTLSFiles(config.ClientTLS)
	if err != nil {
		return fmt.Errorf("load client TLS: %w", err)
	}
	policy, err := buildPolicy(config.AllowedClients, config.AllowedAdministrators)
	if err != nil {
		return err
	}
	health := observability.NewHealthRegistry(string(config.NodeID))
	statusServer, err := service.NewNodeStatusServer(node, health)
	if err != nil {
		return err
	}
	publicTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{publicCertificate}, ClientCAs: publicRoots, ClientAuth: tls.RequireAndVerifyClientCert}
	metrics := observability.NewCounterSet()
	publicGRPC, err := service.NewGRPCServer(service.GRPCServerConfig{
		Credentials:          credentials.NewTLS(publicTLS),
		Policy:               policy,
		MaxConcurrent:        config.MaxConcurrentRequests,
		MaxRequestsPerSecond: config.MaxRequestsPerSecond,
		Metrics:              metrics,
	}, server, statusServer)
	if err != nil {
		return err
	}
	publicListener, err := net.Listen("tcp", config.ClientListen)
	if err != nil {
		peerGRPC.Stop()
		_ = peerListener.Close()
		_ = peerTransport.Close()
		return fmt.Errorf("listen for clients: %w", err)
	}
	publicServeErrors := make(chan error, 1)
	go func() { publicServeErrors <- publicGRPC.Serve(publicListener) }()

	var metricsServer *http.Server
	var metricsListener net.Listener
	metricsServeErrors := make(chan error, 1)
	shutdownTimeout := time.Duration(config.ShutdownTimeoutMillis) * time.Millisecond
	defer func() {
		gracefulStop(publicGRPC, publicListener, shutdownTimeout)
		gracefulStop(peerGRPC, peerListener, shutdownTimeout)
		if metricsServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_ = metricsServer.Shutdown(ctx)
			cancel()
			if metricsListener != nil {
				_ = metricsListener.Close()
			}
		}
		_ = node.Close()
	}()
	if config.MetricsListen != "" {
		metricsServer = &http.Server{
			Handler:           observability.MetricsHandler(metrics, health),
			ReadHeaderTimeout: 2 * time.Second,
			MaxHeaderBytes:    4 * 1024,
		}
		metricsListener, err = net.Listen("tcp", config.MetricsListen)
		if err != nil {
			return fmt.Errorf("listen for metrics: %w", err)
		}
		go func() { metricsServeErrors <- metricsServer.Serve(metricsListener) }()
	}

	interval := time.Duration(config.TickIntervalMillis) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	shutdown, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignal()
	for {
		select {
		case <-shutdown.Done():
			return nil
		case serveErr := <-peerServeErrors:
			if serveErr != nil {
				return fmt.Errorf("peer server stopped: %w", serveErr)
			}
			return nil
		case serveErr := <-publicServeErrors:
			if serveErr != nil {
				return fmt.Errorf("client server stopped: %w", serveErr)
			}
			return nil
		case serveErr := <-metricsServeErrors:
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return fmt.Errorf("metrics server stopped: %w", serveErr)
			}
			return nil
		case <-ticker.C:
			if tickErr := node.Tick(); tickErr != nil && !errors.Is(tickErr, raft.ErrNoQuorum) && !errors.Is(tickErr, raft.ErrUnavailable) {
				health.Update(observability.HealthSnapshot{NodeID: string(config.NodeID), Health: observability.HealthNotServing, Role: node.State().Role.String(), Reason: "consensus tick failed"})
			} else {
				health.Update(runtimeHealth(node.State()))
			}
		}
	}
}

func validateServeConfig(config serveConfig) error {
	if err := contracts.NodeID(config.NodeID).Validate(); err != nil || config.DataDir == "" || config.PeerListen == "" || config.ClientListen == "" {
		return fmt.Errorf("invalid server configuration")
	}
	if !filepath.IsAbs(config.DataDir) || config.PeerListen == config.ClientListen {
		return fmt.Errorf("invalid server paths or listener addresses")
	}
	if err := validateTCPListenAddress(config.PeerListen); err != nil {
		return fmt.Errorf("invalid server listener address")
	}
	if err := validateTCPListenAddress(config.ClientListen); err != nil {
		return fmt.Errorf("invalid server listener address")
	}
	if config.TickIntervalMillis < 10 || config.TickIntervalMillis > 10_000 {
		if config.TickIntervalMillis != 0 {
			return fmt.Errorf("invalid tick interval")
		}
	}
	if config.MaxConcurrentRequests < 0 || config.MaxConcurrentRequests > 4096 || config.MaxRequestsPerSecond < 0 || config.MaxRequestsPerSecond > 1_000_000 || config.MaxInFlightPeerRequests < 0 || config.MaxInFlightPeerRequests > 4096 {
		return fmt.Errorf("invalid concurrency limits")
	}
	if config.ShutdownTimeoutMillis < 0 || config.ShutdownTimeoutMillis > 60_000 {
		return fmt.Errorf("invalid shutdown timeout")
	}
	if config.MetricsListen != "" {
		if err := validateTCPListenAddress(config.MetricsListen); err != nil {
			return fmt.Errorf("invalid metrics listener address")
		}
	}
	if err := validateTLSFilePaths(config.PeerTLS); err != nil {
		return fmt.Errorf("invalid peer TLS paths: %w", err)
	}
	if err := validateTLSFilePaths(config.ClientTLS); err != nil {
		return fmt.Errorf("invalid client TLS paths: %w", err)
	}
	if _, err := transport.NewStaticCluster(config.NodeID, config.Peers); err != nil {
		return fmt.Errorf("invalid cluster configuration: %w", err)
	}
	return nil
}

func applyServeDefaults(config *serveConfig) {
	if config.TickIntervalMillis == 0 {
		config.TickIntervalMillis = 50
	}
	if config.MaxConcurrentRequests == 0 {
		config.MaxConcurrentRequests = 256
	}
	if config.MaxRequestsPerSecond == 0 {
		config.MaxRequestsPerSecond = 1000
	}
	if config.MaxInFlightPeerRequests == 0 {
		config.MaxInFlightPeerRequests = 64
	}
	if config.ShutdownTimeoutMillis == 0 {
		config.ShutdownTimeoutMillis = 5_000
	}
}

func validateTCPListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" || portText == "" {
		return fmt.Errorf("address must be host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65_535 {
		return fmt.Errorf("port is outside the supported range")
	}
	return nil
}

func validateTLSFilePaths(files tlsFiles) error {
	paths := []string{files.Certificate, files.Key, files.CA}
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("TLS files must use absolute paths")
		}
	}
	return nil
}

func gracefulStop(server interface {
	GracefulStop()
	Stop()
}, listener net.Listener, timeout time.Duration) {
	if server == nil {
		return
	}
	done := make(chan struct{})
	go func() { server.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		server.Stop()
	}
	if listener != nil {
		_ = listener.Close()
	}
}

func memberIDs(cluster *transport.StaticCluster) []string {
	members := cluster.Members()
	ids := make([]string, len(members))
	for index, member := range members {
		ids[index] = string(member.ID)
	}
	return ids
}

func memberNodeIDs(cluster *transport.StaticCluster) []raft.NodeID {
	members := cluster.Members()
	ids := make([]raft.NodeID, len(members))
	for index, member := range members {
		ids[index] = member.ID
	}
	return ids
}

func loadTLSFiles(files tlsFiles) (tls.Certificate, *x509.CertPool, error) {
	if files.Certificate == "" || files.Key == "" || files.CA == "" {
		return tls.Certificate{}, nil, fmt.Errorf("certificate, key, and CA paths are required")
	}
	keyInfo, err := os.Stat(files.Key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o037 != 0 {
		return tls.Certificate{}, nil, fmt.Errorf("TLS private key must be a regular file with no group write or other permissions")
	}
	certificate, err := tls.LoadX509KeyPair(files.Certificate, files.Key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caData, err := os.ReadFile(files.CA)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if len(caData) == 0 || len(caData) > 4<<20 {
		return tls.Certificate{}, nil, fmt.Errorf("CA file is empty or too large")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return tls.Certificate{}, nil, fmt.Errorf("CA file contains no certificates")
	}
	return certificate, roots, nil
}

func buildPolicy(clients, administrators []string) (*security.Policy, error) {
	permissions := make(map[security.Principal][]security.Operation, len(clients)+len(administrators))
	for _, id := range clients {
		permissions[security.Principal{ID: id, Kind: security.PrincipalClient}] = []security.Operation{security.OperationRead, security.OperationMutate}
	}
	for _, id := range administrators {
		permissions[security.Principal{ID: id, Kind: security.PrincipalAdmin}] = []security.Operation{security.OperationAdmin}
	}
	return security.NewPolicy(permissions)
}

func runtimeHealth(state raft.RuntimeState) observability.HealthSnapshot {
	health := observability.HealthServing
	if !state.Healthy {
		health = observability.HealthNotServing
	}
	return observability.HealthSnapshot{NodeID: string(state.ID), Health: health, Role: state.Role.String(), Term: uint64(state.Term), CommitIndex: uint64(state.CommitIndex), AppliedIndex: uint64(state.LastApplied)}
}

func runCheckHistory(arguments []string) error {
	if len(arguments) != 1 {
		return fmt.Errorf("check-history requires one path")
	}
	data, err := os.ReadFile(arguments[0])
	if err != nil {
		return err
	}
	result := checker.CheckJSON(data)
	encoded, _ := json.Marshal(result)
	fmt.Println(string(encoded))
	if !result.Linearizable {
		return fmt.Errorf("history is not linearizable: %s", result.Reason)
	}
	return nil
}

func runValidateSchedule(arguments []string) error {
	if len(arguments) != 1 {
		return fmt.Errorf("validate-schedule requires one path")
	}
	data, err := os.ReadFile(arguments[0])
	if err != nil {
		return err
	}
	schedule, err := chaos.Decode(data)
	if err != nil {
		return err
	}
	fmt.Printf("valid schedule: version=%d actions=%d seed=%d\n", schedule.Version, len(schedule.Actions), schedule.Seed)
	return nil
}

func runVerifyBackup(arguments []string) error {
	if len(arguments) != 1 {
		return fmt.Errorf("verify-backup requires one path")
	}
	data, err := os.ReadFile(arguments[0])
	if err != nil {
		return err
	}
	state, err := storage.VerifyBackup(data)
	if err != nil {
		return err
	}
	fmt.Printf("valid backup: term=%d commit_index=%d entries=%d snapshot=%t\n", state.HardState.CurrentTerm, state.HardState.CommitIndex, len(state.Entries), state.Snapshot != nil)
	return nil
}

func runExportBackup(arguments []string) error {
	flags := flag.NewFlagSet("export-backup", flag.ContinueOnError)
	dataDir := flags.String("data-dir", "", "offline Viper data directory")
	output := flags.String("output", "", "backup output path")
	force := flags.Bool("force", false, "replace an existing backup file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if !filepath.IsAbs(*dataDir) || !filepath.IsAbs(*output) {
		return fmt.Errorf("data directory and output must use absolute paths")
	}
	store, err := storage.Open(*dataDir)
	if err != nil {
		return fmt.Errorf("open backup source: %w", err)
	}
	defer store.Close()
	backup, err := store.ExportBackup()
	if err != nil {
		return fmt.Errorf("export backup: %w", err)
	}
	if err := writePrivateFile(*output, backup, *force); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	return nil
}

func runRestoreBackup(arguments []string) error {
	flags := flag.NewFlagSet("restore-backup", flag.ContinueOnError)
	dataDir := flags.String("data-dir", "", "pre-provisioned empty Viper data directory")
	backupPath := flags.String("backup", "", "verified backup path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if !filepath.IsAbs(*dataDir) || !filepath.IsAbs(*backupPath) {
		return fmt.Errorf("data directory and backup must use absolute paths")
	}
	backup, err := os.ReadFile(*backupPath)
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}
	if len(backup) > storage.MaxBackupBytes {
		return fmt.Errorf("backup exceeds size limit")
	}
	store, err := storage.RestoreBackup(*dataDir, backup)
	if err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}
	return store.Close()
}

func runVersion() error {
	encoded, err := json.Marshal(map[string]string{"version": version, "commit": buildCommit, "build_date": buildDate})
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func runProbe(arguments []string) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	target := flags.String("target", "", "client gRPC target")
	serverName := flags.String("server-name", "", "TLS server name")
	caPath := flags.String("ca", "", "client CA path")
	certificatePath := flags.String("certificate", "", "client certificate path")
	keyPath := flags.String("key", "", "client private key path")
	clientID := flags.String("client-id", "probe-client", "bounded client identity")
	requestID := flags.String("request-id", "probe-request-1", "idempotent request ID")
	key := flags.String("key-name", "viper-probe-key", "key to write")
	value := flags.String("value", "viper-probe-value", "value to write")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *target == "" || *serverName == "" || !filepath.IsAbs(*caPath) || !filepath.IsAbs(*certificatePath) || !filepath.IsAbs(*keyPath) {
		return fmt.Errorf("probe requires target, server name, and absolute TLS paths")
	}
	certificate, roots, err := loadTLSFiles(tlsFiles{Certificate: *certificatePath, Key: *keyPath, CA: *caPath})
	if err != nil {
		return fmt.Errorf("load probe TLS: %w", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: *serverName}
	probeClient, connection, err := viperclient.Dial(context.Background(), *target, viperclient.DialConfig{Credentials: credentials.NewTLS(tlsConfig)})
	if err != nil {
		return fmt.Errorf("dial probe target: %w", err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := probeClient.Put(ctx, contracts.ClientID(*clientID), contracts.RequestID(*requestID), []byte(*key), []byte(*value)); err != nil {
		return fmt.Errorf("probe write: %w", err)
	}
	read, err := probeClient.Get(ctx, []byte(*key), contracts.ConsistencyLinearizable)
	if err != nil {
		return fmt.Errorf("probe read: %w", err)
	}
	encoded, err := json.Marshal(map[string]any{"found": read.Found, "revision": read.Revision, "value": string(read.Value)})
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func writePrivateFile(path string, data []byte, force bool) error {
	parent := filepath.Dir(path)
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return fmt.Errorf("output directory is unavailable")
	}
	temporary, err := os.CreateTemp(parent, ".viper-private-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if force {
		if err := os.Rename(temporaryName, path); err != nil {
			return err
		}
	} else if err := os.Link(temporaryName, path); err != nil {
		return err
	} else if err := os.Remove(temporaryName); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
