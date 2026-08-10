package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	apiv1 "viper/api/gen/v1"
	"viper/internal/raft"
	"viper/pkg/contracts"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const defaultMaxConcurrentPeerRequests = 256

// GRPCServerConfig configures the TLS-protected Raft peer RPC server.
// Certificate issuance and peer identity authorization are owned by the
// process wiring and security layers.
type GRPCServerConfig struct {
	Credentials   credentials.TransportCredentials
	MaxConcurrent int
	Options       []grpc.ServerOption
}

// NewPeerGRPCServer builds a TLS-only, size-bounded peer RPC server.
func NewPeerGRPCServer(cluster *StaticCluster, handler raft.PeerHandler, config GRPCServerConfig) (*grpc.Server, error) {
	if cluster == nil {
		return nil, invalidCluster("cluster is nil")
	}
	if handler == nil {
		return nil, invalidCluster("peer handler is nil")
	}
	if config.Credentials == nil {
		return nil, invalidCluster("peer server TLS credentials are required")
	}
	if config.Credentials.Info().SecurityProtocol != "tls" {
		return nil, invalidCluster("peer server credentials must use TLS")
	}
	maxConcurrent := config.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = defaultMaxConcurrentPeerRequests
	}
	if maxConcurrent < 1 || maxConcurrent > 4096 {
		return nil, invalidCluster("max concurrent peer requests must be between 1 and 4096")
	}
	options := append([]grpc.ServerOption(nil), config.Options...)
	options = append(options,
		grpc.Creds(config.Credentials),
		grpc.MaxRecvMsgSize(contracts.MaxRPCMessageBytes),
		grpc.MaxSendMsgSize(contracts.MaxRPCMessageBytes),
		grpc.ChainUnaryInterceptor(boundedPeerUnaryInterceptor(maxConcurrent)),
	)
	server := grpc.NewServer(options...)
	apiv1.RegisterRaftPeerServiceServer(server, &peerRPCServer{cluster: cluster, handler: handler})
	return server, nil
}

func boundedPeerUnaryInterceptor(maxConcurrent int) grpc.UnaryServerInterceptor {
	limit := make(chan struct{}, maxConcurrent)
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		select {
		case limit <- struct{}{}:
			defer func() { <-limit }()
		default:
			return nil, status.Error(codes.ResourceExhausted, "peer resource limit exceeded")
		}
		return handler(ctx, request)
	}
}

type peerRPCServer struct {
	apiv1.UnimplementedRaftPeerServiceServer
	cluster *StaticCluster
	handler raft.PeerHandler
}

func (s *peerRPCServer) RequestVote(ctx context.Context, message *apiv1.RequestVoteRequest) (*apiv1.RequestVoteResponse, error) {
	request, err := requestVoteFromProto(message)
	if err != nil {
		return nil, peerStatus(err)
	}
	if err := validateRequestVote(s.cluster, request); err != nil {
		return nil, peerStatus(err)
	}
	response, err := s.handler.HandleRequestVote(ctx, request)
	if err != nil {
		return nil, peerStatus(err)
	}
	if err := validateRequestVoteResponse(request, response); err != nil {
		return nil, status.Error(codes.Internal, "peer handler returned an invalid response")
	}
	return requestVoteResponseToProto(response), nil
}

func (s *peerRPCServer) AppendEntries(ctx context.Context, message *apiv1.AppendEntriesRequest) (*apiv1.AppendEntriesResponse, error) {
	request, err := appendEntriesFromProto(message)
	if err != nil {
		return nil, peerStatus(err)
	}
	if err := validateAppendEntries(s.cluster, request); err != nil {
		return nil, peerStatus(err)
	}
	response, err := s.handler.HandleAppendEntries(ctx, request)
	if err != nil {
		return nil, peerStatus(err)
	}
	if err := validateAppendEntriesResponse(request, response); err != nil {
		return nil, status.Error(codes.Internal, "peer handler returned an invalid response")
	}
	return appendEntriesResponseToProto(response), nil
}

func (s *peerRPCServer) InstallSnapshot(ctx context.Context, message *apiv1.InstallSnapshotRequest) (*apiv1.InstallSnapshotResponse, error) {
	request, err := installSnapshotFromProto(message)
	if err != nil {
		return nil, peerStatus(err)
	}
	if err := validateInstallSnapshot(s.cluster, request); err != nil {
		return nil, peerStatus(err)
	}
	response, err := s.handler.HandleInstallSnapshot(ctx, request)
	if err != nil {
		return nil, peerStatus(err)
	}
	if err := validateInstallSnapshotResponse(request, response); err != nil {
		return nil, status.Error(codes.Internal, "peer handler returned an invalid response")
	}
	return installSnapshotResponseToProto(response), nil
}

func peerStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "peer request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "peer request deadline exceeded")
	case errors.Is(err, ErrUnknownPeer):
		return status.Error(codes.PermissionDenied, "node is not a cluster member")
	case errors.Is(err, ErrInvalidMessage), errors.Is(err, ErrInvalidCluster):
		return status.Error(codes.InvalidArgument, "invalid peer request")
	case errors.Is(err, raft.ErrInvalidMessage):
		return status.Error(codes.InvalidArgument, "invalid peer request")
	case errors.Is(err, ErrOverloaded):
		return status.Error(codes.ResourceExhausted, "peer is overloaded")
	case errors.Is(err, ErrClosed), errors.Is(err, ErrPeerUnavailable), errors.Is(err, raft.ErrUnavailable), errors.Is(err, raft.ErrNoQuorum):
		return status.Error(codes.Unavailable, "peer is unavailable")
	default:
		return status.Error(codes.Internal, "peer request failed")
	}
}

// GRPCDialConfig configures static outbound peer connections.
type GRPCDialConfig struct {
	Credentials        credentials.TransportCredentials
	CredentialsForPeer func(Peer) (credentials.TransportCredentials, error)
	MaxInFlightPerPeer int
	Options            []grpc.DialOption
}

type grpcPeer struct {
	client apiv1.RaftPeerServiceClient
	conn   *grpc.ClientConn
	limit  chan struct{}
}

// GRPCTransport is a TLS-only outbound transport for immutable cluster peers.
type GRPCTransport struct {
	cluster *StaticCluster
	localID raft.NodeID
	peers   map[raft.NodeID]*grpcPeer
	closed  atomic.Bool
	closeMu sync.Mutex
}

var _ raft.PeerTransport = (*GRPCTransport)(nil)

// DialGRPC creates lazy gRPC connections for every configured remote peer.
func DialGRPC(ctx context.Context, cluster *StaticCluster, config GRPCDialConfig) (*GRPCTransport, error) {
	if ctx == nil {
		return nil, invalidMessage("context", "is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cluster == nil {
		return nil, invalidCluster("cluster is nil")
	}
	if config.Credentials == nil && config.CredentialsForPeer == nil {
		return nil, invalidCluster("peer client TLS credentials are required")
	}
	if config.Credentials != nil && config.Credentials.Info().SecurityProtocol != "tls" {
		return nil, invalidCluster("peer client credentials must use TLS")
	}
	limit := config.MaxInFlightPerPeer
	if limit == 0 {
		limit = defaultMaxInFlightPerPeer
	}
	if limit < 1 || limit > 4096 {
		return nil, invalidCluster("max in-flight requests per peer must be between 1 and 4096")
	}

	transport := &GRPCTransport{
		cluster: cluster,
		localID: cluster.LocalID(),
		peers:   make(map[raft.NodeID]*grpcPeer, len(cluster.RemotePeers())),
	}
	for _, peer := range cluster.RemotePeers() {
		var peerCredentials credentials.TransportCredentials
		if config.CredentialsForPeer != nil {
			var credentialErr error
			peerCredentials, credentialErr = config.CredentialsForPeer(peer)
			if credentialErr != nil || peerCredentials == nil {
				transport.closeConnections()
				return nil, invalidCluster(fmt.Sprintf("peer %q has invalid TLS credentials", peer.ID))
			}
		} else {
			peerCredentials = config.Credentials.Clone()
			if peer.ServerName != "" {
				if err := peerCredentials.OverrideServerName(peer.ServerName); err != nil {
					transport.closeConnections()
					return nil, invalidCluster(fmt.Sprintf("peer %q has an invalid TLS server name", peer.ID))
				}
			}
		}
		if peerCredentials.Info().SecurityProtocol != "tls" {
			transport.closeConnections()
			return nil, invalidCluster(fmt.Sprintf("peer %q credentials must use TLS", peer.ID))
		}
		options := append([]grpc.DialOption(nil), config.Options...)
		options = append(options,
			grpc.WithTransportCredentials(peerCredentials),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(contracts.MaxRPCMessageBytes),
				grpc.MaxCallSendMsgSize(contracts.MaxRPCMessageBytes),
			),
		)
		connection, err := grpc.NewClient(peer.Address, options...)
		if err != nil {
			transport.closeConnections()
			return nil, fmt.Errorf("configure peer %q: %w", peer.ID, ErrPeerUnavailable)
		}
		transport.peers[peer.ID] = &grpcPeer{
			client: apiv1.NewRaftPeerServiceClient(connection),
			conn:   connection,
			limit:  make(chan struct{}, limit),
		}
	}
	return transport, nil
}

func (t *GRPCTransport) RequestVote(ctx context.Context, target raft.NodeID, request raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	peer, release, err := t.prepare(ctx, target)
	if err != nil {
		return raft.RequestVoteResponse{}, err
	}
	defer release()
	if request.CandidateID != t.localID {
		return raft.RequestVoteResponse{}, invalidMessage("candidate_id", "must match the sending node")
	}
	if err := validateRequestVote(t.cluster, request); err != nil {
		return raft.RequestVoteResponse{}, err
	}
	message, err := peer.client.RequestVote(ctx, requestVoteToProto(request))
	if err != nil {
		return raft.RequestVoteResponse{}, normalizeClientError(err)
	}
	response, err := requestVoteResponseFromProto(message)
	if err != nil {
		return raft.RequestVoteResponse{}, err
	}
	if err := validateRequestVoteResponse(request, response); err != nil {
		return raft.RequestVoteResponse{}, err
	}
	return response, nil
}

func (t *GRPCTransport) AppendEntries(ctx context.Context, target raft.NodeID, request raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	peer, release, err := t.prepare(ctx, target)
	if err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	defer release()
	if request.LeaderID != t.localID {
		return raft.AppendEntriesResponse{}, invalidMessage("leader_id", "must match the sending node")
	}
	if err := validateAppendEntries(t.cluster, request); err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	message, err := peer.client.AppendEntries(ctx, appendEntriesToProto(request))
	if err != nil {
		return raft.AppendEntriesResponse{}, normalizeClientError(err)
	}
	response, err := appendEntriesResponseFromProto(message)
	if err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	if err := validateAppendEntriesResponse(request, response); err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	return response, nil
}

func (t *GRPCTransport) InstallSnapshot(ctx context.Context, target raft.NodeID, request raft.InstallSnapshotRequest) (raft.InstallSnapshotResponse, error) {
	peer, release, err := t.prepare(ctx, target)
	if err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	defer release()
	if request.LeaderID != t.localID {
		return raft.InstallSnapshotResponse{}, invalidMessage("leader_id", "must match the sending node")
	}
	if err := validateInstallSnapshot(t.cluster, request); err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	message, err := peer.client.InstallSnapshot(ctx, installSnapshotToProto(request))
	if err != nil {
		return raft.InstallSnapshotResponse{}, normalizeClientError(err)
	}
	response, err := installSnapshotResponseFromProto(message)
	if err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	if err := validateInstallSnapshotResponse(request, response); err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	return response, nil
}

func (t *GRPCTransport) prepare(ctx context.Context, target raft.NodeID) (*grpcPeer, func(), error) {
	if t == nil || t.closed.Load() {
		return nil, nil, ErrClosed
	}
	if ctx == nil {
		return nil, nil, invalidMessage("context", "is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateTarget(t.cluster, t.localID, target); err != nil {
		return nil, nil, err
	}
	peer, exists := t.peers[target]
	if !exists {
		return nil, nil, ErrUnknownPeer
	}
	select {
	case peer.limit <- struct{}{}:
		return peer, func() { <-peer.limit }, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
		return nil, nil, ErrOverloaded
	}
}

func normalizeClientError(err error) error {
	switch status.Code(err) {
	case codes.Canceled:
		return context.Canceled
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.ResourceExhausted:
		return ErrOverloaded
	case codes.PermissionDenied:
		return ErrUnknownPeer
	case codes.InvalidArgument:
		return ErrInvalidMessage
	case codes.Unavailable:
		return unavailableError(ErrPeerUnavailable)
	default:
		return err
	}
}

// Close closes all peer connections. It is safe to call more than once.
func (t *GRPCTransport) Close() error {
	if t == nil || !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	return t.closeConnections()
}

func (t *GRPCTransport) closeConnections() error {
	var closeError error
	for _, peer := range t.peers {
		if err := peer.conn.Close(); err != nil {
			closeError = errors.Join(closeError, err)
		}
	}
	return closeError
}
