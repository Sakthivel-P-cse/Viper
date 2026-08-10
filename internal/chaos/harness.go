package chaos

import (
	"context"
	"errors"
	"sync"
	"time"

	"viper/internal/raft"
	"viper/internal/transport"
)

// ErrFaulted marks a node or transport deliberately disabled by a test
// schedule. It is never returned by production transports.
var ErrFaulted = errors.New("chaos target is faulted")

// Harness is a deterministic, process-local cluster used by fault scenarios.
// It preserves production Raft and transport checks while injecting faults at
// the test transport boundary.
type Harness struct {
	mu         sync.Mutex
	clock      *raft.ManualClock
	network    *transport.InMemoryNetwork
	clusters   map[raft.NodeID]*transport.StaticCluster
	nodes      map[raft.NodeID]*raft.Node
	transports map[raft.NodeID]*faultTransport
	handlers   map[raft.NodeID]*faultHandler
	order      []raft.NodeID
	cursor     *Cursor
	closed     bool
}

// NewHarness creates a three- or five-node static cluster with an injectable
// clock and the supplied reproducible schedule.
func NewHarness(ids []raft.NodeID, schedule Schedule) (*Harness, error) {
	if len(ids) != 3 && len(ids) != 5 {
		return nil, ErrInvalidSchedule
	}
	cursor, err := NewCursor(schedule)
	if err != nil {
		return nil, err
	}
	peers := make([]transport.Peer, len(ids))
	for index, id := range ids {
		peers[index] = transport.Peer{ID: id, Address: "memory://" + string(id)}
	}
	clusters := make(map[raft.NodeID]*transport.StaticCluster, len(ids))
	for _, id := range ids {
		cluster, clusterErr := transport.NewStaticCluster(id, peers)
		if clusterErr != nil {
			return nil, clusterErr
		}
		clusters[id] = cluster
	}
	network, err := transport.NewInMemoryNetwork(clusters[ids[0]], 64)
	if err != nil {
		return nil, err
	}
	harness := &Harness{
		clock:      raft.NewManualClock(time.Unix(0, 0)),
		network:    network,
		clusters:   clusters,
		nodes:      make(map[raft.NodeID]*raft.Node, len(ids)),
		transports: make(map[raft.NodeID]*faultTransport, len(ids)),
		handlers:   make(map[raft.NodeID]*faultHandler, len(ids)),
		order:      append([]raft.NodeID(nil), ids...),
		cursor:     cursor,
	}
	for _, id := range ids {
		node, nodeErr := raft.New(raft.Config{
			ID: id, Peers: ids, Clock: harness.clock,
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 100 * time.Millisecond,
			HeartbeatInterval:  10 * time.Millisecond,
			RPCTimeout:         100 * time.Millisecond,
		})
		if nodeErr != nil {
			_ = network.Close()
			return nil, nodeErr
		}
		harness.nodes[id] = node
	}
	for _, id := range ids {
		handler := &faultHandler{node: harness.nodes[id]}
		base, bindErr := network.Bind(clusters[id], handler)
		if bindErr != nil {
			_ = harness.Close()
			return nil, bindErr
		}
		faulted := &faultTransport{base: base}
		harness.handlers[id] = handler
		harness.transports[id] = faulted
		if attachErr := harness.nodes[id].AttachTransport(faulted); attachErr != nil {
			_ = harness.Close()
			return nil, attachErr
		}
	}
	return harness, nil
}

// Apply executes one validated action against the deterministic harness.
func (h *Harness) Apply(action Action) error {
	if h == nil {
		return ErrFaulted
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return transport.ErrClosed
	}
	if action.Target != "" {
		if _, ok := h.nodes[raft.NodeID(action.Target)]; !ok {
			return ErrInvalidSchedule
		}
	}
	switch action.Kind {
	case ActionClockAdvance:
		h.clock.Advance(action.Duration)
	case ActionReorder:
		targets := make([]string, len(h.order))
		for index, id := range h.order {
			targets[index] = string(id)
		}
		shuffled := h.cursor.ShuffleTargets(targets)
		h.order = h.order[:0]
		for _, target := range shuffled {
			h.order = append(h.order, raft.NodeID(target))
		}
	case ActionDelay, ActionDrop, ActionDuplicate:
		for _, id := range h.targets(action.Target) {
			fault := h.transports[id]
			switch action.Kind {
			case ActionDelay:
				fault.setDelay(action.Duration)
			case ActionDrop:
				fault.addDrops(action.Count)
			case ActionDuplicate:
				fault.addDuplicates(action.Count)
			}
		}
	case ActionCrash, ActionDiskFailure:
		for _, id := range h.targets(action.Target) {
			h.handlers[id].setFaulted(true)
			h.transports[id].setFaulted(true)
		}
	default:
		return ErrInvalidSchedule
	}
	return nil
}

// Step applies the next schedule action, then advances every live node once.
func (h *Harness) Step() error {
	if h == nil {
		return ErrFaulted
	}
	if action, ok := h.cursor.Next(); ok {
		if err := h.Apply(action); err != nil {
			return err
		}
	}
	return h.Tick()
}

// Tick advances live nodes in the current deterministic order.
func (h *Harness) Tick() error {
	if h == nil {
		return ErrFaulted
	}
	for _, id := range h.order {
		if h.handlers[id].faulted() {
			continue
		}
		if err := h.nodes[id].Tick(); err != nil && !errors.Is(err, raft.ErrNoQuorum) && !errors.Is(err, raft.ErrUnavailable) {
			return err
		}
	}
	return nil
}

// Propose submits a command to the currently elected live leader.
func (h *Harness) Propose(command []byte) (raft.Index, error) {
	if h == nil {
		return 0, ErrFaulted
	}
	for _, id := range h.order {
		if h.handlers[id].faulted() {
			continue
		}
		if h.nodes[id].State().Role == raft.RoleLeader {
			return h.nodes[id].Propose(command)
		}
	}
	return 0, raft.ErrNotLeader
}

// Leader returns the current live leader, if one exists.
func (h *Harness) Leader() (raft.NodeID, bool) {
	if h == nil {
		return "", false
	}
	for _, id := range h.order {
		if !h.handlers[id].faulted() && h.nodes[id].State().Role == raft.RoleLeader {
			return id, true
		}
	}
	return "", false
}

// State returns a node's isolated consensus state.
func (h *Harness) State(id raft.NodeID) (raft.RuntimeState, bool) {
	if h == nil {
		return raft.RuntimeState{}, false
	}
	node, ok := h.nodes[id]
	if !ok {
		return raft.RuntimeState{}, false
	}
	return node.State(), true
}

// Close releases all harness resources. It is idempotent.
func (h *Harness) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	var closeErr error
	for _, node := range h.nodes {
		closeErr = errors.Join(closeErr, node.Close())
	}
	closeErr = errors.Join(closeErr, h.network.Close())
	return closeErr
}

func (h *Harness) targets(target string) []raft.NodeID {
	if target != "" {
		return []raft.NodeID{raft.NodeID(target)}
	}
	targets := make([]raft.NodeID, 0, len(h.nodes))
	for id := range h.nodes {
		targets = append(targets, id)
	}
	return targets
}

type faultHandler struct {
	mu           sync.RWMutex
	node         *raft.Node
	faultedState bool
}

func (h *faultHandler) setFaulted(value bool) {
	h.mu.Lock()
	h.faultedState = value
	h.mu.Unlock()
}

func (h *faultHandler) faulted() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.faultedState
}

func (h *faultHandler) check() error {
	if h.faulted() {
		return transport.ErrPeerUnavailable
	}
	return nil
}

func (h *faultHandler) HandleRequestVote(ctx context.Context, request raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	if err := h.check(); err != nil {
		return raft.RequestVoteResponse{}, err
	}
	return h.node.HandleRequestVote(ctx, request)
}

func (h *faultHandler) HandleAppendEntries(ctx context.Context, request raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	if err := h.check(); err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	return h.node.HandleAppendEntries(ctx, request)
}

func (h *faultHandler) HandleInstallSnapshot(ctx context.Context, request raft.InstallSnapshotRequest) (raft.InstallSnapshotResponse, error) {
	if err := h.check(); err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	return h.node.HandleInstallSnapshot(ctx, request)
}

type faultTransport struct {
	mu           sync.Mutex
	base         *transport.InMemoryTransport
	delay        time.Duration
	drops        uint32
	duplicates   uint32
	faultedState bool
}

func (t *faultTransport) setFaulted(value bool) {
	t.mu.Lock()
	t.faultedState = value
	t.mu.Unlock()
}

func (t *faultTransport) setDelay(delay time.Duration) {
	t.mu.Lock()
	t.delay = delay
	t.mu.Unlock()
}

func (t *faultTransport) addDrops(count uint32) {
	t.mu.Lock()
	t.drops += count
	t.mu.Unlock()
}

func (t *faultTransport) addDuplicates(count uint32) {
	t.mu.Lock()
	t.duplicates += count
	t.mu.Unlock()
}

func (t *faultTransport) before(ctx context.Context) (uint32, error) {
	t.mu.Lock()
	if t.faultedState {
		t.mu.Unlock()
		return 0, transport.ErrPeerUnavailable
	}
	if t.drops > 0 {
		t.drops--
		t.mu.Unlock()
		return 0, transport.ErrPeerUnavailable
	}
	delay, duplicates := t.delay, t.duplicates
	t.duplicates = 0
	t.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return duplicates, nil
}

func (t *faultTransport) RequestVote(ctx context.Context, target raft.NodeID, request raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	extra, err := t.before(ctx)
	if err != nil {
		return raft.RequestVoteResponse{}, err
	}
	response, err := t.base.RequestVote(ctx, target, request)
	for index := uint32(0); index < extra; index++ {
		_, _ = t.base.RequestVote(ctx, target, request)
	}
	return response, err
}

func (t *faultTransport) AppendEntries(ctx context.Context, target raft.NodeID, request raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	extra, err := t.before(ctx)
	if err != nil {
		return raft.AppendEntriesResponse{}, err
	}
	response, err := t.base.AppendEntries(ctx, target, request)
	for index := uint32(0); index < extra; index++ {
		_, _ = t.base.AppendEntries(ctx, target, request)
	}
	return response, err
}

func (t *faultTransport) InstallSnapshot(ctx context.Context, target raft.NodeID, request raft.InstallSnapshotRequest) (raft.InstallSnapshotResponse, error) {
	extra, err := t.before(ctx)
	if err != nil {
		return raft.InstallSnapshotResponse{}, err
	}
	response, err := t.base.InstallSnapshot(ctx, target, request)
	for index := uint32(0); index < extra; index++ {
		_, _ = t.base.InstallSnapshot(ctx, target, request)
	}
	return response, err
}

func (t *faultTransport) Close() error {
	if t == nil || t.base == nil {
		return nil
	}
	return t.base.Close()
}
