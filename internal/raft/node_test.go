package raft

import (
	"context"
	"errors"
	"hash/crc32"
	"sync"
	"testing"
	"time"
)

type testTransport struct {
	local  NodeID
	nodes  map[NodeID]*Node
	closed bool
}

func (t *testTransport) RequestVote(ctx context.Context, target NodeID, request RequestVoteRequest) (RequestVoteResponse, error) {
	if t.closed {
		return RequestVoteResponse{}, ErrClosed
	}
	node := t.nodes[target]
	if node == nil {
		return RequestVoteResponse{}, ErrUnavailable
	}
	return node.HandleRequestVote(ctx, request)
}

func (t *testTransport) AppendEntries(ctx context.Context, target NodeID, request AppendEntriesRequest) (AppendEntriesResponse, error) {
	if t.closed {
		return AppendEntriesResponse{}, ErrClosed
	}
	node := t.nodes[target]
	if node == nil {
		return AppendEntriesResponse{}, ErrUnavailable
	}
	return node.HandleAppendEntries(ctx, request)
}

func (t *testTransport) InstallSnapshot(ctx context.Context, target NodeID, request InstallSnapshotRequest) (InstallSnapshotResponse, error) {
	if t.closed {
		return InstallSnapshotResponse{}, ErrClosed
	}
	node := t.nodes[target]
	if node == nil {
		return InstallSnapshotResponse{}, ErrUnavailable
	}
	return node.HandleInstallSnapshot(ctx, request)
}

func (t *testTransport) Close() error {
	t.closed = true
	return nil
}

type testStore struct {
	mu    sync.Mutex
	state PersistentState
	fail  error
}

func (s *testStore) State() (PersistentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clonePersistentState(s.state), nil
}

func (s *testStore) Persist(batch PersistBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if batch.HardState != nil {
		s.state.HardState = *batch.HardState
	}
	if batch.TruncateFrom != nil {
		position := len(s.state.Entries)
		for i, entry := range s.state.Entries {
			if entry.Index >= *batch.TruncateFrom {
				position = i
				break
			}
		}
		s.state.Entries = s.state.Entries[:position]
	}
	s.state.Entries = append(s.state.Entries, cloneEntries(batch.Entries)...)
	return nil
}

func (s *testStore) SaveSnapshot(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.state.Snapshot = cloneSnapshot(&snapshot)
	base := snapshot.Index
	for position, entry := range s.state.Entries {
		if entry.Index > base {
			s.state.Entries = cloneEntries(s.state.Entries[position:])
			return nil
		}
	}
	s.state.Entries = nil
	return nil
}

func (s *testStore) InstallSnapshot(snapshot Snapshot, hardState HardState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.state.HardState = hardState
	s.state.Snapshot = cloneSnapshot(&snapshot)
	retained := make([]Entry, 0, len(s.state.Entries))
	for _, entry := range s.state.Entries {
		if entry.Index > snapshot.Index {
			retained = append(retained, cloneEntry(entry))
		}
	}
	s.state.Entries = retained
	return nil
}

func (s *testStore) Close() error { return nil }

func clonePersistentState(state PersistentState) PersistentState {
	return PersistentState{HardState: state.HardState, Entries: cloneEntries(state.Entries), Snapshot: cloneSnapshot(state.Snapshot)}
}

func makeTestCluster(t *testing.T) (map[NodeID]*Node, *ManualClock, map[NodeID][]string) {
	t.Helper()
	clock := NewManualClock(time.Unix(0, 0))
	ids := []NodeID{"node-1", "node-2", "node-3"}
	nodes := make(map[NodeID]*Node, len(ids))
	applied := make(map[NodeID][]string, len(ids))
	var appliedMu sync.Mutex
	for _, id := range ids {
		localID := id
		node, err := New(Config{
			ID:                 id,
			Peers:              ids,
			Clock:              clock,
			Store:              &testStore{},
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 100 * time.Millisecond,
			HeartbeatInterval:  10 * time.Millisecond,
			RPCTimeout:         100 * time.Millisecond,
			Apply: func(entry Entry) error {
				appliedMu.Lock()
				applied[localID] = append(applied[localID], string(entry.Command))
				appliedMu.Unlock()
				return nil
			},
		})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		nodes[id] = node
	}
	for _, id := range ids {
		if err := nodes[id].AttachTransport(&testTransport{local: id, nodes: nodes}); err != nil {
			t.Fatalf("attach %s transport: %v", id, err)
		}
	}
	return nodes, clock, applied
}

func TestThreeNodeElectionAndMajorityCommit(t *testing.T) {
	nodes, clock, applied := makeTestCluster(t)
	clock.Advance(100 * time.Millisecond)
	if err := nodes["node-1"].Tick(); err != nil {
		t.Fatalf("start election: %v", err)
	}
	leader := nodes["node-1"].State()
	if leader.Role != RoleLeader || leader.Term != 1 || leader.LeaderID != "node-1" {
		t.Fatalf("leader state = %#v", leader)
	}
	if nodes["node-2"].State().Term != 1 || nodes["node-3"].State().Term != 1 {
		t.Fatal("followers did not observe the election term")
	}
	index, err := nodes["node-1"].Propose([]byte("set:key=value"))
	if err != nil || index != 1 {
		t.Fatalf("proposal index = %d, error = %v", index, err)
	}
	for _, id := range []NodeID{"node-1", "node-2", "node-3"} {
		state := nodes[id].State()
		if state.CommitIndex != 1 || state.LastApplied != 1 {
			t.Fatalf("%s state = %#v", id, state)
		}
		if len(applied[id]) != 1 || applied[id][0] != "set:key=value" {
			t.Fatalf("%s applied commands = %#v", id, applied[id])
		}
	}
}

func TestRequestVoteRestrictionAndAppendConflictReplacement(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	node, err := New(Config{ID: "node-2", Peers: []NodeID{"node-1", "node-2", "node-3"}, Clock: clock, Store: &testStore{}, ElectionTimeoutMin: time.Second, ElectionTimeoutMax: time.Second})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	response, err := node.HandleRequestVote(context.Background(), RequestVoteRequest{Term: 1, CandidateID: "node-1"})
	if err != nil || !response.VoteGranted {
		t.Fatalf("first vote = %#v, error = %v", response, err)
	}
	response, err = node.HandleRequestVote(context.Background(), RequestVoteRequest{Term: 1, CandidateID: "node-3"})
	if err != nil || response.VoteGranted {
		t.Fatalf("second vote = %#v, error = %v", response, err)
	}
	appendRequest := AppendEntriesRequest{Term: 1, LeaderID: "node-1", Entries: []Entry{{Index: 1, Term: 1, Command: []byte("old")}}}
	responseAppend, err := node.HandleAppendEntries(context.Background(), appendRequest)
	if err != nil || !responseAppend.Success {
		t.Fatalf("initial append = %#v, error = %v", responseAppend, err)
	}
	conflict := AppendEntriesRequest{Term: 2, LeaderID: "node-1", Entries: []Entry{{Index: 1, Term: 2, Command: []byte("new")}}}
	responseAppend, err = node.HandleAppendEntries(context.Background(), conflict)
	if err != nil || !responseAppend.Success {
		t.Fatalf("conflicting append = %#v, error = %v", responseAppend, err)
	}
	entries := node.Entries()
	if len(entries) != 1 || entries[0].Term != 2 || string(entries[0].Command) != "new" {
		t.Fatalf("replaced log = %#v", entries)
	}
}

func TestPersistenceFailureMakesNodeUnavailableBeforeAcknowledgement(t *testing.T) {
	failure := errors.New("disk full")
	store := &testStore{fail: failure}
	node, err := New(Config{ID: "node-1", Peers: []NodeID{"node-1"}, Store: store})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	response, err := node.HandleRequestVote(context.Background(), RequestVoteRequest{Term: 1, CandidateID: "node-1"})
	if !errors.Is(err, ErrUnavailable) || response.VoteGranted {
		t.Fatalf("failed vote = %#v, error = %v", response, err)
	}
	state := node.State()
	if state.Healthy || state.Term != 0 || state.VotedFor != "" {
		t.Fatalf("node acknowledged unsafe state: %#v", state)
	}
}

func TestLeaderDoesNotCommitWithoutQuorum(t *testing.T) {
	nodes, clock, _ := makeTestCluster(t)
	clock.Advance(100 * time.Millisecond)
	if err := nodes["node-1"].Tick(); err != nil {
		t.Fatalf("elect leader: %v", err)
	}
	if err := nodes["node-1"].AttachTransport(&testTransport{local: "node-1", nodes: map[NodeID]*Node{"node-1": nodes["node-1"]}}); err != nil {
		t.Fatalf("isolate leader: %v", err)
	}
	index, err := nodes["node-1"].Propose([]byte("minority-command"))
	if !errors.Is(err, ErrNoQuorum) || index != 1 {
		t.Fatalf("minority proposal index = %d, error = %v", index, err)
	}
	if state := nodes["node-1"].State(); state.CommitIndex != 0 || state.LastApplied != 0 {
		t.Fatalf("minority committed state = %#v", state)
	}
}

func TestSnapshotChunksInstallAndRestoreAtomically(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	store := &testStore{}
	var restored Snapshot
	node, err := New(Config{
		ID: "node-2", Peers: []NodeID{"node-1", "node-2", "node-3"}, Clock: clock,
		Store: store, Restore: func(snapshot Snapshot) error { restored = *cloneSnapshot(&snapshot); return nil },
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	first := []byte("state-")
	response, err := node.HandleInstallSnapshot(context.Background(), InstallSnapshotRequest{
		Term: 1, LeaderID: "node-1", LastIncludedIndex: 5, LastIncludedTerm: 1,
		Offset: 0, Data: first, Checksum: crc32.Checksum(first, raftChecksumTable),
	})
	if err != nil || response.NextOffset != uint64(len(first)) {
		t.Fatalf("first snapshot chunk = %#v, error = %v", response, err)
	}
	second := []byte("ready")
	response, err = node.HandleInstallSnapshot(context.Background(), InstallSnapshotRequest{
		Term: 1, LeaderID: "node-1", LastIncludedIndex: 5, LastIncludedTerm: 1,
		Offset: response.NextOffset, Data: second, Done: true,
		Checksum: crc32.Checksum(second, raftChecksumTable),
	})
	if err != nil || response.NextOffset != uint64(len(first)+len(second)) {
		t.Fatalf("final snapshot chunk = %#v, error = %v", response, err)
	}
	if restored.Index != 5 || string(restored.Data) != "state-ready" {
		t.Fatalf("restored snapshot = %#v", restored)
	}
	state := node.State()
	if state.CommitIndex != 5 || state.LastApplied != 5 || state.LastLogIndex != 5 {
		t.Fatalf("node after snapshot = %#v", state)
	}
	if snapshot := node.Snapshot(); snapshot == nil || string(snapshot.Data) != "state-ready" {
		t.Fatalf("active snapshot = %#v", snapshot)
	}
	if entries := node.Entries(); len(entries) != 0 {
		t.Fatalf("snapshot did not compact log: %#v", entries)
	}
}

func TestLeaderReplicatesSnapshotToLaggingPeer(t *testing.T) {
	nodes, clock, _ := makeTestCluster(t)
	clock.Advance(100 * time.Millisecond)
	if err := nodes["node-1"].Tick(); err != nil {
		t.Fatalf("elect leader: %v", err)
	}
	if _, err := nodes["node-1"].Propose([]byte("snapshot-command")); err != nil {
		t.Fatalf("seed committed command: %v", err)
	}
	if err := nodes["node-1"].SaveSnapshot(Snapshot{Index: 1, Term: 1, Data: []byte("snapshot-state")}); err != nil {
		t.Fatalf("save leader snapshot: %v", err)
	}
	var restored Snapshot
	fresh, err := New(Config{
		ID: "node-2", Peers: []NodeID{"node-1", "node-2", "node-3"}, Clock: clock,
		Store: &testStore{}, Restore: func(snapshot Snapshot) error { restored = *cloneSnapshot(&snapshot); return nil },
	})
	if err != nil {
		t.Fatalf("create lagging peer: %v", err)
	}
	nodes["node-2"] = fresh
	if err := fresh.AttachTransport(&testTransport{local: "node-2", nodes: nodes}); err != nil {
		t.Fatalf("attach lagging peer transport: %v", err)
	}
	nodes["node-1"].nextIndex["node-2"] = 1
	clock.Advance(10 * time.Millisecond)
	if err := nodes["node-1"].Tick(); err != nil {
		t.Fatalf("replicate leader snapshot: %v", err)
	}
	state := fresh.State()
	if state.CommitIndex != 1 || state.LastApplied != 1 || state.LastLogIndex != 1 || string(restored.Data) != "snapshot-state" {
		t.Fatalf("lagging peer after snapshot = %#v, restored=%#v", state, restored)
	}
}
