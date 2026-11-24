package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	apiv1 "viper/api/gen/v1"
	"viper/pkg/contracts"
)

//go:embed dashboard/*
var dashboardFiles embed.FS

type dashboardNode struct {
	ID         string `json:"id"`
	Target     string `json:"target"`
	ServerName string `json:"server_name"`
}
type dashboardConfig struct {
	Listen string          `json:"listen"`
	TLS    tlsFiles        `json:"tls"`
	Nodes  []dashboardNode `json:"nodes"`
}
type dashboardStatusClient interface {
	GetNodeStatus(context.Context, *apiv1.GetNodeStatusRequest, ...grpc.CallOption) (*apiv1.GetNodeStatusResponse, error)
}
type dashboardObservation struct {
	ID        string         `json:"id"`
	Liveness  dashboardScope `json:"liveness"`
	Readiness dashboardScope `json:"readiness"`
}
type dashboardScope struct {
	ObservedAt time.Time            `json:"observed_at"`
	Error      string               `json:"error,omitempty"`
	Status     *dashboardNodeStatus `json:"status,omitempty"`
}
type dashboardNodeStatus struct {
	NodeID       string `json:"node_id"`
	Health       string `json:"health"`
	Role         string `json:"role"`
	Term         string `json:"term"`
	CommitIndex  string `json:"commit_index"`
	AppliedIndex string `json:"applied_index"`
	Reason       string `json:"reason,omitempty"`
}

func readDashboardConfig(path string) (dashboardConfig, error) {
	var config dashboardConfig
	file, err := os.Open(path)
	if err != nil {
		return config, fmt.Errorf("open dashboard configuration: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<16)+1))
	if err != nil || len(data) > 1<<16 {
		return config, errors.New("invalid dashboard configuration size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, errors.New("invalid dashboard configuration")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return config, errors.New("invalid dashboard configuration trailing data")
	}
	if err := validateDashboardConfig(config); err != nil {
		return config, err
	}
	return config, nil
}

func validateDashboardConfig(config dashboardConfig) error {
	host, port, err := net.SplitHostPort(config.Listen)
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || n < 1 || n > 65535 {
		return errors.New("dashboard listen must use a loopback IP and port")
	}
	if validateTLSFilePaths(config.TLS) != nil || len(config.Nodes) < 1 || len(config.Nodes) > 5 {
		return errors.New("invalid dashboard TLS or node count")
	}
	ids, targets := map[string]bool{}, map[string]bool{}
	for _, node := range config.Nodes {
		host, port, err := net.SplitHostPort(node.Target)
		n, portErr := strconv.Atoi(port)
		if contracts.NodeID(node.ID).Validate() != nil || ids[node.ID] || targets[node.Target] || err != nil || host == "" || portErr != nil || n < 1 || n > 65535 || node.ServerName == "" || len(node.ServerName) > 253 || strings.ContainsAny(node.ServerName, " \t\r\n") {
			return errors.New("invalid dashboard node")
		}
		ids[node.ID], targets[node.Target] = true, true
	}
	return nil
}

func dashboardError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return "timeout"
	case codes.Unauthenticated, codes.PermissionDenied:
		return "unauthorized"
	case codes.Unavailable:
		return "unavailable"
	default:
		return "error"
	}
}

func dashboardQuery(ctx context.Context, client dashboardStatusClient, scope apiv1.HealthScope, expectedID string) dashboardScope {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := client.GetNodeStatus(ctx, &apiv1.GetNodeStatusRequest{Scope: scope})
	observedAt := time.Now().UTC()
	if err != nil {
		return dashboardScope{ObservedAt: observedAt, Error: dashboardError(err)}
	}
	if result == nil || result.GetNodeId() != expectedID {
		return dashboardScope{ObservedAt: observedAt, Error: "error"}
	}
	return dashboardScope{ObservedAt: observedAt, Status: &dashboardNodeStatus{NodeID: result.GetNodeId(), Health: result.GetHealth().String(), Role: result.GetRole().String(), Term: strconv.FormatUint(result.GetTerm(), 10), CommitIndex: strconv.FormatUint(result.GetCommitIndex(), 10), AppliedIndex: strconv.FormatUint(result.GetAppliedIndex(), 10), Reason: result.GetReason()}}
}

func dashboardHandler(host string, nodes []dashboardNode, clients []dashboardStatusClient) http.Handler {
	// Limit simultaneous refreshes, and therefore outstanding gRPC calls.
	slots := make(chan struct{}, 2)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.Header.Get("Sec-Fetch-Site") == "cross-site" || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+host) {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			data, _ := dashboardFiles.ReadFile("dashboard/index.html")
			_, _ = w.Write(data)
		case "/style.css", "/app.js":
			data, err := dashboardFiles.ReadFile("dashboard" + r.URL.Path)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/style.css" {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			} else {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			}
			_, _ = w.Write(data)
		case "/api/status":
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			observations := make([]dashboardObservation, len(nodes))
			var wg sync.WaitGroup
			for i := range nodes {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					observations[i] = dashboardObservation{ID: nodes[i].ID, Liveness: dashboardQuery(r.Context(), clients[i], apiv1.HealthScope_HEALTH_SCOPE_LIVENESS, nodes[i].ID), Readiness: dashboardQuery(r.Context(), clients[i], apiv1.HealthScope_HEALTH_SCOPE_READINESS, nodes[i].ID)}
				}(i)
			}
			wg.Wait()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Nodes []dashboardObservation `json:"nodes"`
			}{observations})
		default:
			http.NotFound(w, r)
		}
	})
}

func runDashboard(args []string) error {
	flags := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	path := flags.String("config", "", "dashboard JSON configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("dashboard requires -config path")
	}
	config, err := readDashboardConfig(*path)
	if err != nil {
		return err
	}
	cert, roots, err := loadTLSFiles(config.TLS)
	if err != nil {
		return fmt.Errorf("load dashboard TLS: %w", err)
	}
	clients := make([]dashboardStatusClient, 0, len(config.Nodes))
	var connections []*grpc.ClientConn
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for _, node := range config.Nodes {
		credentials := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots, ServerName: node.ServerName})
		conn, err := grpc.NewClient(node.Target, grpc.WithTransportCredentials(credentials), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(contracts.MaxRPCMessageBytes), grpc.MaxCallSendMsgSize(contracts.MaxRPCMessageBytes)))
		if err != nil {
			return errors.New("invalid dashboard node connection")
		}
		connections = append(connections, conn)
		clients = append(clients, apiv1.NewNodeStatusServiceClient(conn))
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return fmt.Errorf("listen for dashboard: %w", err)
	}
	defer listener.Close()
	server := &http.Server{Handler: dashboardHandler(config.Listen, config.Nodes, clients), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 14}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("Dashboard listening on http://%s\n", config.Listen)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
