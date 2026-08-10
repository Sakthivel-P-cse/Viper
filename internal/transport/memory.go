package transport

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"viper/internal/raft"
)

const defaultMaxInFlightPerPeer = 64

type memoryEndpoint struct {
	cluster *StaticCluster
	handler raft.PeerHandler
	limit   chan struct{}
}

func (e *memoryEndpoint) acquire(ctx context.Context) error {
	select {
	case e.limit <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrOverloaded
	}
}

func (e *memoryEndpoint) release() {
	<-e.limit
}

// InMemoryNetwork is a bounded, process-local network for static clusters. It
// is suitable for local runtimes and deterministic integration harnesses.
type InMemoryNetwork struct {
	cluster   *StaticCluster
	endpoints map[raft.NodeID]*memoryEndpoint
	mu        sync.RWMutex
	closed    bool
	limit     int
}

// NewInMemoryNetwork creates an empty network for one immutable membership.
func NewInMemoryNetwork(cluster *StaticCluster, maxInFlightPerPeer int) (*InMemoryNetwork, error) {
	if cluster == nil {
		return nil, invalidCluster("cluster is nil")
	}
	if maxInFlightPerPeer == 0 {
		maxInFlightPerPeer = defaultMaxInFlightPerPeer
	}
	if maxInFlightPerPeer < 1 || maxInFlightPerPeer > 4096 {
		return nil, invalidCluster("max in-flight requests per peer must be between 1 and 4096")
	}
	return &InMemoryNetwork{
		cluster:   cluster,
		endpoints: make(map[raft.NodeID]*memoryEndpoint, len(cluster.Members())),
		limit:     maxInFlightPerPeer,
	}, nil
}

// Bind registers a node handler and returns that node's outbound transport.
// The supplied cluster must describe the same immutable membership and use the
// bound node as its local ID.
func (n *InMemoryNetwork) Bind(cluster *StaticCluster, handler raft.PeerHandler) (*InMemoryTransport, error) {
	if n == nil {
		return nil, ErrClosed
	}
	if cluster == nil || !sameMembership(n.cluster, cluster) {
		return nil, invalidCluster("bound node does not share the network membership")
	}
	if handler == nil {
		return nil, invalidCluster("peer handler is nil")
	}
	localID := cluster.LocalID()
	endpoint := &memoryEndpoint{
		cluster: cluster,
		handler: handler,
		limit:   make(chan struct{}, n.limit),
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, ErrClosed
	}
	if _, exists := n.endpoints[localID]; exists {
		return nil, invalidCluster("node already has an active in-memory endpoint")
	}
	n.endpoints[localID] = endpoint
	return &InMemoryTransport{
		cluster:  cluster,
		endpoint: endpoint,
		localID:  localID,
		network:  n,
	}, nil
}

// Close disconnects all endpoints and prevents future binds.
func (n *InMemoryNetwork) Close() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	clear(n.endpoints)
	return nil
}

func (n *InMemoryNetwork) lookup(target raft.NodeID) (*memoryEndpoint, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.closed {
		return nil, ErrClosed
	}
	endpoint, exists := n.endpoints[target]
	if !exists {
		return nil, ErrPeerUnavailable
	}
	return endpoint, nil
}

func (n *InMemoryNetwork) unbind(id raft.NodeID, endpoint *memoryEndpoint) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if current, exists := n.endpoints[id]; exists && current == endpoint {
		delete(n.endpoints, id)
	}
}

func sameMembership(left, right *StaticCluster) bool {
	if left == nil || right == nil {
		return false
	}
	leftMembers := left.Members()
	rightMembers := right.Members()
	if len(leftMembers) != len(rightMembers) {
		return false
	}
	for i := range leftMembers {
		if leftMembers[i] != rightMembers[i] {
			return false
		}
	}
	return true
}

// InMemoryTransport is one node's outbound view of an InMemoryNetwork.
type InMemoryTransport struct {
	cluster  *StaticCluster
	endpoint *memoryEndpoint
	localID  raft.NodeID
	network  *InMemoryNetwork
	closed   atomic.Bool
}

var _ raft.PeerTransport = (*InMemoryTransport)(nil)

// RequestVote sends a bounded, isolated vote request to a configured peer.
func (t *InMemoryTransport) RequestVote(ctx context.Context, target raft.NodeID, request raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	if err := t.prepare(ctx, target); err != nil {
		return raft.RequestVoteResponse{}, err
	}
	if request.CandidateID != t.localID {
		return raft.RequestVoteResponse{}, invalidMessage("candidate_id", "must match the sending node")
	}
	if err := validateRequestVote(t.cluster, request); err != nil {
		return raft.RequestVoteResponse{}, err
	}
	endpoint, err := t.network.lookup(target)
	if err != nil {
		return raft.RequestVoteResponse{}, unavailableError(err)
	}
	if err := endpoint.acquire(ctx); err != nil {
		return raft.RequestVoteResponse{}, err
	}
	defer endpoint.release()
	response, err := endpoint.handler.HandleRequestVote(ctx, cloneRequestVote(request))
	if err != nil {
		if errors.Is(err, ErrPeerUnavailable) {
			return raft.RequestVoteResponse{}, unavailableError(err)
		}
		return raft.RequestVoteResponse{}, err
	}
	if err := validateRequestVoteResponse(request, response); err != nil {
		return raft.RequestVoteResponse{}, err
	}
	return response, nil
}

// AppendEntries sends a heartbeat or contiguous log suffix to a peer.
func (t *InMemoryTransport) AppendEntries(ctx context.Context, target raft.NodeID, request raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	if err := t.prepare(ctx, target); err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	if request.LeaderID != t.localID {
		return raft.AppendEntriesResponse{}, invalidMessage("leader_id", "must match the sending node")
	}
	if err := validateAppendEntries(t.cluster, request); err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	endpoint, err := t.network.lookup(target)
	if err != nil {
		return raft.AppendEntriesResponse{}, unavailableError(err)
	}
	if err := endpoint.acquire(ctx); err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	defer endpoint.release()
	response, err := endpoint.handler.HandleAppendEntries(ctx, cloneAppendEntries(request))
	if err != nil {
		if errors.Is(err, ErrPeerUnavailable) {
			return raft.AppendEntriesResponse{}, unavailableError(err)
		}
		return raft.AppendEntriesResponse{}, err
	}
	if err := validateAppendEntriesResponse(request, response); err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	return response, nil
}

// InstallSnapshot sends one checksummed snapshot chunk to a peer.
func (t *InMemoryTransport) InstallSnapshot(ctx context.Context, target raft.NodeID, request raft.InstallSnapshotRequest) (raft.InstallSnapshotResponse, error) {
	if err := t.prepare(ctx, target); err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	if request.LeaderID != t.localID {
		return raft.InstallSnapshotResponse{}, invalidMessage("leader_id", "must match the sending node")
	}
	if err := validateInstallSnapshot(t.cluster, request); err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	endpoint, err := t.network.lookup(target)
	if err != nil {
		return raft.InstallSnapshotResponse{}, unavailableError(err)
	}
	if err := endpoint.acquire(ctx); err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	defer endpoint.release()
	response, err := endpoint.handler.HandleInstallSnapshot(ctx, cloneInstallSnapshot(request))
	if err != nil {
		if errors.Is(err, ErrPeerUnavailable) {
			return raft.InstallSnapshotResponse{}, unavailableError(err)
		}
		return raft.InstallSnapshotResponse{}, err
	}
	if err := validateInstallSnapshotResponse(request, response); err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	return response, nil
}

func (t *InMemoryTransport) prepare(ctx context.Context, target raft.NodeID) error {
	if t == nil || t.closed.Load() {
		return ErrClosed
	}
	if ctx == nil {
		return invalidMessage("context", "is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return validateTarget(t.cluster, t.localID, target)
}

// Close disconnects this node without closing other network endpoints.
func (t *InMemoryTransport) Close() error {
	if t == nil || !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	t.network.unbind(t.localID, t.endpoint)
	return nil
}
