package raft

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const (
	defaultElectionTimeoutMin = 300 * time.Millisecond
	defaultElectionTimeoutMax = 600 * time.Millisecond
	defaultHeartbeatInterval  = 100 * time.Millisecond
	defaultRPCTimeout         = 1 * time.Second
	maxAppendEntries          = 256
	maxCommandBytes           = 3 * 1024 * 1024
	maxSnapshotChunk          = 1 * 1024 * 1024
	maxSnapshotBytes          = 64 * 1024 * 1024
)

// Role is the externally observable role of a consensus node.
type Role uint8

const (
	RoleFollower Role = iota + 1
	RoleCandidate
	RoleLeader
)

func (r Role) String() string {
	switch r {
	case RoleFollower:
		return "follower"
	case RoleCandidate:
		return "candidate"
	case RoleLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// Config contains all infrastructure ports used by the deterministic core.
// The store and transport are optional so the core can be exercised in a
// pure in-memory model; production nodes should provide both.
type Config struct {
	ID                 NodeID
	Peers              []NodeID
	Transport          PeerTransport
	Store              StableStore
	Clock              Clock
	Apply              func(Entry) error
	Restore            func(Snapshot) error
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration
	RPCTimeout         time.Duration
	RandomSeed         int64
}

// RuntimeState is an isolated snapshot of consensus progress.
type RuntimeState struct {
	ID           NodeID
	Role         Role
	Term         Term
	VotedFor     NodeID
	LeaderID     NodeID
	CommitIndex  Index
	LastApplied  Index
	LastLogIndex Index
	LastLogTerm  Term
	Healthy      bool
	Closed       bool
}

// Node is a deterministic, single-writer Raft state machine. Callers drive it
// with Tick and proposals; inbound RPC methods are safe for concurrent callers.
type Node struct {
	mu sync.Mutex

	id        NodeID
	peers     []NodeID
	peerSet   map[NodeID]struct{}
	transport PeerTransport
	store     StableStore
	clock     Clock
	apply     func(Entry) error
	restore   func(Snapshot) error

	role        Role
	term        Term
	votedFor    NodeID
	leaderID    NodeID
	log         []Entry
	snapshot    *Snapshot
	commitIndex Index
	lastApplied Index

	nextIndex  map[NodeID]Index
	matchIndex map[NodeID]Index
	votes      map[NodeID]bool

	electionMin       time.Duration
	electionMax       time.Duration
	heartbeat         time.Duration
	rpcTimeout        time.Duration
	rng               *rand.Rand
	electionDeadline  time.Time
	heartbeatDeadline time.Time
	incomingSnapshot  *snapshotAssembly

	healthy   bool
	closed    bool
	lastError error
}

type snapshotAssembly struct {
	index  Index
	term   Term
	leader NodeID
	data   []byte
}

// New constructs a Raft node and validates recovered durable state.
func New(config Config) (*Node, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	clock := config.Clock
	if clock == nil {
		clock = wallClock{}
	}
	electionMin := config.ElectionTimeoutMin
	if electionMin == 0 {
		electionMin = defaultElectionTimeoutMin
	}
	electionMax := config.ElectionTimeoutMax
	if electionMax == 0 {
		electionMax = defaultElectionTimeoutMax
	}
	heartbeat := config.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = defaultHeartbeatInterval
	}
	rpcTimeout := config.RPCTimeout
	if rpcTimeout == 0 {
		rpcTimeout = defaultRPCTimeout
	}
	seed := config.RandomSeed
	if seed == 0 {
		seed = 1
	}
	node := &Node{
		id:          config.ID,
		peers:       append([]NodeID(nil), config.Peers...),
		peerSet:     make(map[NodeID]struct{}, len(config.Peers)),
		transport:   config.Transport,
		store:       config.Store,
		clock:       clock,
		apply:       config.Apply,
		restore:     config.Restore,
		role:        RoleFollower,
		nextIndex:   make(map[NodeID]Index),
		matchIndex:  make(map[NodeID]Index),
		votes:       make(map[NodeID]bool),
		electionMin: electionMin,
		electionMax: electionMax,
		heartbeat:   heartbeat,
		rpcTimeout:  rpcTimeout,
		rng:         rand.New(rand.NewSource(seed)),
		healthy:     true,
	}
	for _, peer := range config.Peers {
		node.peerSet[peer] = struct{}{}
	}
	if config.Store != nil {
		state, err := config.Store.State()
		if err != nil {
			return nil, fmt.Errorf("recover raft state: %w", err)
		}
		if err := node.loadState(state); err != nil {
			return nil, err
		}
	}
	if node.snapshot != nil && node.restore != nil {
		if err := node.restore(*cloneSnapshot(node.snapshot)); err != nil {
			return nil, fmt.Errorf("restore raft snapshot: %w", err)
		}
	}
	if node.snapshot != nil && node.apply != nil && node.restore == nil {
		return nil, fmt.Errorf("%w: recovered snapshot requires a restore callback", ErrInvalidConfig)
	}
	node.resetElectionLocked()
	if node.commitIndex > node.lastApplied {
		if err := node.applyCommittedLocked(); err != nil {
			return nil, err
		}
	}
	return node, nil
}

func validateConfig(config Config) error {
	if !validNodeID(config.ID) {
		return fmt.Errorf("%w: invalid node ID", ErrInvalidConfig)
	}
	if len(config.Peers) == 0 {
		return fmt.Errorf("%w: peer set is empty", ErrInvalidConfig)
	}
	seen := make(map[NodeID]struct{}, len(config.Peers))
	for _, peer := range config.Peers {
		if !validNodeID(peer) {
			return fmt.Errorf("%w: invalid peer ID", ErrInvalidConfig)
		}
		if _, exists := seen[peer]; exists {
			return fmt.Errorf("%w: duplicate peer ID", ErrInvalidConfig)
		}
		seen[peer] = struct{}{}
	}
	if _, exists := seen[config.ID]; !exists {
		return fmt.Errorf("%w: local node is not a peer", ErrInvalidConfig)
	}
	min := config.ElectionTimeoutMin
	max := config.ElectionTimeoutMax
	if min != 0 && min <= 0 || max != 0 && max <= 0 {
		return fmt.Errorf("%w: election timeouts must be positive", ErrInvalidConfig)
	}
	if min != 0 && max != 0 && max < min {
		return fmt.Errorf("%w: election timeout maximum precedes minimum", ErrInvalidConfig)
	}
	if config.HeartbeatInterval < 0 || config.RPCTimeout < 0 {
		return fmt.Errorf("%w: intervals cannot be negative", ErrInvalidConfig)
	}
	return nil
}

func validNodeID(id NodeID) bool {
	value := string(id)
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character)) {
			return false
		}
	}
	return true
}

func (n *Node) loadState(state PersistentState) error {
	if state.HardState.CurrentTerm == 0 && state.HardState.VotedFor != "" {
		return fmt.Errorf("%w: vote exists without a term", ErrInvalidConfig)
	}
	if state.HardState.VotedFor != "" && !validNodeID(state.HardState.VotedFor) {
		return fmt.Errorf("%w: recovered vote is invalid", ErrInvalidConfig)
	}
	if state.HardState.VotedFor != "" && !n.isPeerLocked(state.HardState.VotedFor) {
		return fmt.Errorf("%w: recovered vote is outside the static membership", ErrInvalidConfig)
	}
	n.snapshot = cloneSnapshot(state.Snapshot)
	baseIndex := Index(0)
	if n.snapshot != nil {
		if n.snapshot.Index == 0 || n.snapshot.Term == 0 {
			return fmt.Errorf("%w: recovered snapshot is invalid", ErrInvalidConfig)
		}
		if n.snapshot.Term > state.HardState.CurrentTerm {
			return fmt.Errorf("%w: recovered snapshot term exceeds current term", ErrInvalidConfig)
		}
		baseIndex = n.snapshot.Index
	}
	previous := baseIndex
	previousTerm := func() Term {
		if n.snapshot != nil {
			return n.snapshot.Term
		}
		return 0
	}()
	for _, entry := range state.Entries {
		if entry.Index == 0 || entry.Term == 0 || entry.Index != previous+1 || entry.Term < previousTerm || entry.Term > state.HardState.CurrentTerm || len(entry.Command) > maxCommandBytes {
			return fmt.Errorf("%w: recovered log is invalid", ErrInvalidConfig)
		}
		n.log = append(n.log, cloneEntry(entry))
		previous = entry.Index
		previousTerm = entry.Term
	}
	if state.HardState.CommitIndex > previous {
		return fmt.Errorf("%w: recovered commit index exceeds log", ErrInvalidConfig)
	}
	n.term = state.HardState.CurrentTerm
	n.votedFor = state.HardState.VotedFor
	n.commitIndex = state.HardState.CommitIndex
	if n.snapshot != nil {
		n.lastApplied = n.snapshot.Index
	}
	return nil
}

// AttachTransport connects an already constructed node to its outbound port.
func (n *Node) AttachTransport(transport PeerTransport) error {
	if n == nil {
		return ErrUnavailable
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return ErrClosed
	}
	if !n.healthy {
		return fmt.Errorf("%w: %v", ErrUnavailable, n.lastError)
	}
	n.transport = transport
	return nil
}

// SetApply attaches the ordered state-machine apply callback before the node
// begins serving proposals. It also replays any committed entries recovered
// from durable storage.
func (n *Node) SetApply(apply func(Entry) error) error {
	if n == nil {
		return ErrUnavailable
	}
	if apply == nil {
		return fmt.Errorf("%w: apply callback is nil", ErrInvalidConfig)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return err
	}
	if n.apply != nil {
		return fmt.Errorf("%w: apply callback is already attached", ErrInvalidConfig)
	}
	if n.snapshot != nil && n.restore == nil {
		return fmt.Errorf("%w: snapshot restore callback is required before application", ErrInvalidConfig)
	}
	n.apply = apply
	return n.applyCommittedLocked()
}

// State returns an isolated runtime snapshot.
func (n *Node) State() RuntimeState {
	if n == nil {
		return RuntimeState{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return RuntimeState{
		ID: n.id, Role: n.role, Term: n.term, VotedFor: n.votedFor,
		LeaderID: n.leaderID, CommitIndex: n.commitIndex,
		LastApplied: n.lastApplied, LastLogIndex: n.lastLogIndexLocked(),
		LastLogTerm: n.lastLogTermLocked(), Healthy: n.healthy, Closed: n.closed,
	}
}

// Entries returns an isolated copy of the current log suffix.
func (n *Node) Entries() []Entry {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	entries := make([]Entry, len(n.log))
	for i, entry := range n.log {
		entries[i] = cloneEntry(entry)
	}
	return entries
}

// Snapshot returns an isolated copy of the active snapshot, if any.
func (n *Node) Snapshot() *Snapshot {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return cloneSnapshot(n.snapshot)
}

// SaveSnapshot durably compacts the applied log at a committed index. The
// state-machine image must have been produced from the same applied index and
// term; the durable store validates the matching log position before it
// changes its checkpoint.
func (n *Node) SaveSnapshot(snapshot Snapshot) error {
	if n == nil {
		return ErrUnavailable
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return err
	}
	if snapshot.Index == 0 || snapshot.Term == 0 || snapshot.Index > n.lastApplied || snapshot.Index > n.commitIndex {
		return fmt.Errorf("%w: snapshot must cover an applied committed index", ErrInvalidMessage)
	}
	term, found := n.termAtLocked(snapshot.Index)
	if !found || term != snapshot.Term {
		return fmt.Errorf("%w: snapshot term does not match the local log", ErrInvalidMessage)
	}
	if n.snapshot != nil && snapshot.Index <= n.snapshot.Index {
		return fmt.Errorf("%w: snapshot regresses the local checkpoint", ErrInvalidMessage)
	}
	if n.store != nil {
		if err := n.store.SaveSnapshot(snapshot); err != nil {
			return n.failLocked(err)
		}
	}
	n.log = retainedEntriesAfterLocked(n.log, n.snapshot, snapshot.Index)
	n.snapshot = cloneSnapshot(&snapshot)
	return nil
}

// Tick advances election and heartbeat timers using the configured clock.
func (n *Node) Tick() error {
	if n == nil {
		return ErrUnavailable
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return err
	}
	now := n.clock.Now()
	if n.role == RoleLeader {
		if !now.Before(n.heartbeatDeadline) {
			n.heartbeatDeadline = now.Add(n.heartbeat)
			return n.broadcastReplicationLocked()
		}
		return nil
	}
	if !now.Before(n.electionDeadline) {
		return n.startElectionLocked(now)
	}
	return nil
}

// Propose appends and replicates one command. It returns after a majority has
// committed the entry, or ErrNoQuorum while leaving the entry available for a
// future retry/heartbeat.
func (n *Node) Propose(command []byte) (Index, error) {
	if n == nil {
		return 0, ErrUnavailable
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return 0, err
	}
	if n.role != RoleLeader {
		return 0, ErrNotLeader
	}
	if len(command) > maxCommandBytes {
		return 0, fmt.Errorf("%w: command exceeds maximum size", ErrInvalidMessage)
	}
	index := n.lastLogIndexLocked()
	if index == Index(math.MaxUint64) {
		return 0, fmt.Errorf("%w: log index overflow", ErrInvalidMessage)
	}
	entry := Entry{Index: index + 1, Term: n.term, Command: append([]byte(nil), command...)}
	hardState := n.hardStateLocked(n.commitIndex)
	if err := n.persistLocked(PersistBatch{HardState: &hardState, Entries: []Entry{entry}}); err != nil {
		return 0, err
	}
	n.log = append(n.log, entry)
	n.matchIndex[n.id] = entry.Index
	n.nextIndex[n.id] = entry.Index + 1
	if err := n.broadcastReplicationLocked(); err != nil {
		return entry.Index, err
	}
	if n.commitIndex < entry.Index {
		return entry.Index, ErrNoQuorum
	}
	// The replication round that established the majority carried the prior
	// commit index. Send one follow-up round so followers learn the newly
	// durable commit without waiting for the next heartbeat.
	for _, peer := range n.peers {
		if peer != n.id {
			_ = n.replicatePeerLocked(peer)
		}
	}
	return entry.Index, nil
}

// ReadIndex confirms that the current leader still has contact with a
// majority and returns the committed index observed by that quorum. Followers
// must not claim this linearizable read guarantee.
func (n *Node) ReadIndex(ctx context.Context) (Index, error) {
	if n == nil {
		return 0, ErrUnavailable
	}
	if ctx == nil {
		return 0, fmt.Errorf("%w: context is nil", ErrInvalidMessage)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return 0, err
	}
	if n.role != RoleLeader {
		return 0, ErrNotLeader
	}
	if n.quorumLocked() > 1 && n.transport == nil {
		return 0, ErrUnavailable
	}
	confirmed := 1
	for _, peer := range n.peers {
		if peer == n.id || confirmed >= n.quorumLocked() {
			continue
		}
		request, err := n.appendRequestLocked(peer)
		if err != nil {
			continue
		}
		callContext, cancel := context.WithTimeout(ctx, n.rpcTimeout)
		response, callErr := n.transport.AppendEntries(callContext, peer, request)
		cancel()
		if callErr != nil {
			continue
		}
		if response.Term > n.term {
			if err := n.becomeFollowerTermLocked(response.Term); err != nil {
				return 0, err
			}
			return 0, ErrStaleTerm
		}
		if response.Term != n.term || !response.Success {
			continue
		}
		expected := request.PrevLogIndex + Index(len(request.Entries))
		if response.MatchIndex != expected {
			continue
		}
		n.matchIndex[peer] = response.MatchIndex
		n.nextIndex[peer] = response.MatchIndex + 1
		confirmed++
	}
	if confirmed < n.quorumLocked() {
		return 0, ErrNoQuorum
	}
	if err := n.advanceCommitLocked(); err != nil {
		return 0, err
	}
	return n.commitIndex, nil
}

func (n *Node) readyLocked() error {
	if n.closed {
		return ErrClosed
	}
	if !n.healthy {
		if n.lastError == nil {
			return ErrUnavailable
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, n.lastError)
	}
	return nil
}

func (n *Node) resetElectionLocked() {
	now := n.clock.Now()
	duration := n.electionMin
	if n.electionMax > n.electionMin {
		delta := n.electionMax - n.electionMin
		duration += time.Duration(n.rng.Int63n(int64(delta) + 1))
	}
	n.electionDeadline = now.Add(duration)
}

func (n *Node) startElectionLocked(now time.Time) error {
	if n.term == Term(math.MaxUint64) {
		return n.failLocked(fmt.Errorf("%w: term overflow", ErrUnavailable))
	}
	nextTerm := n.term + 1
	hardState := n.hardStateLocked(n.commitIndex)
	hardState.CurrentTerm = nextTerm
	hardState.VotedFor = n.id
	if err := n.persistLocked(PersistBatch{HardState: &hardState}); err != nil {
		return err
	}
	n.term = nextTerm
	n.votedFor = n.id
	n.role = RoleCandidate
	n.leaderID = ""
	n.votes = map[NodeID]bool{n.id: true}
	if countVotes(n.votes) >= n.quorumLocked() {
		n.becomeLeaderLocked()
		return n.broadcastReplicationLocked()
	}
	n.resetElectionLocked()
	request := RequestVoteRequest{Term: n.term, CandidateID: n.id, LastLogIndex: n.lastLogIndexLocked(), LastLogTerm: n.lastLogTermLocked()}
	for _, peer := range n.peers {
		if peer == n.id || n.role != RoleCandidate {
			continue
		}
		response, err := n.sendRequestVoteLocked(peer, request)
		if err != nil {
			continue
		}
		if err := n.handleVoteResponseLocked(request.Term, peer, response); err != nil {
			return err
		}
	}
	if n.role == RoleLeader {
		return n.broadcastReplicationLocked()
	}
	_ = now
	return nil
}

func (n *Node) sendRequestVoteLocked(target NodeID, request RequestVoteRequest) (RequestVoteResponse, error) {
	if n.transport == nil {
		return RequestVoteResponse{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.rpcTimeout)
	defer cancel()
	return n.transport.RequestVote(ctx, target, request)
}

func (n *Node) handleVoteResponseLocked(term Term, peer NodeID, response RequestVoteResponse) error {
	if response.Term > n.term {
		return n.becomeFollowerTermLocked(response.Term)
	}
	if n.role != RoleCandidate || term != n.term || response.Term != n.term {
		return nil
	}
	if response.VoteGranted {
		n.votes[peer] = true
	}
	if countVotes(n.votes) >= n.quorumLocked() {
		n.becomeLeaderLocked()
	}
	return nil
}

func countVotes(votes map[NodeID]bool) int {
	count := 0
	for _, granted := range votes {
		if granted {
			count++
		}
	}
	return count
}

func (n *Node) becomeLeaderLocked() {
	n.role = RoleLeader
	n.leaderID = n.id
	n.nextIndex = make(map[NodeID]Index, len(n.peers))
	n.matchIndex = make(map[NodeID]Index, len(n.peers))
	last := n.lastLogIndexLocked()
	for _, peer := range n.peers {
		n.nextIndex[peer] = last + 1
		n.matchIndex[peer] = 0
	}
	n.matchIndex[n.id] = last
	n.nextIndex[n.id] = last + 1
	n.heartbeatDeadline = n.clock.Now()
}

func (n *Node) becomeFollowerTermLocked(term Term) error {
	if term < n.term {
		return nil
	}
	if term > n.term {
		hardState := n.hardStateLocked(n.commitIndex)
		hardState.CurrentTerm = term
		hardState.VotedFor = ""
		if err := n.persistLocked(PersistBatch{HardState: &hardState}); err != nil {
			return err
		}
		n.term = term
		n.votedFor = ""
	}
	n.role = RoleFollower
	n.leaderID = ""
	n.resetElectionLocked()
	return nil
}

func (n *Node) broadcastReplicationLocked() error {
	if n.role != RoleLeader {
		return nil
	}
	for _, peer := range n.peers {
		if peer == n.id || n.role != RoleLeader {
			continue
		}
		if err := n.replicatePeerLocked(peer); err != nil && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrStaleTerm) {
			return err
		}
	}
	return n.advanceCommitLocked()
}

func (n *Node) replicatePeerLocked(peer NodeID) error {
	if n.transport == nil {
		return ErrUnavailable
	}
	for attempt := 0; attempt < len(n.log)+2; attempt++ {
		if n.role != RoleLeader {
			return ErrStaleTerm
		}
		if n.snapshot != nil && n.nextIndex[peer] <= n.snapshot.Index {
			if err := n.installSnapshotPeerLocked(peer); err != nil {
				return err
			}
			n.matchIndex[peer] = n.snapshot.Index
			n.nextIndex[peer] = n.snapshot.Index + 1
			continue
		}
		request, err := n.appendRequestLocked(peer)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), n.rpcTimeout)
		response, callErr := n.transport.AppendEntries(ctx, peer, request)
		cancel()
		if callErr != nil {
			return callErr
		}
		if response.Term > n.term {
			if err := n.becomeFollowerTermLocked(response.Term); err != nil {
				return err
			}
			return ErrStaleTerm
		}
		if n.role != RoleLeader || response.Term != n.term {
			return ErrStaleTerm
		}
		if response.Success {
			expected := request.PrevLogIndex + Index(len(request.Entries))
			if response.MatchIndex != expected {
				return fmt.Errorf("%w: peer returned an invalid match index", ErrInvalidMessage)
			}
			n.matchIndex[peer] = response.MatchIndex
			n.nextIndex[peer] = response.MatchIndex + 1
			return n.advanceCommitLocked()
		}
		next := n.nextIndex[peer]
		if response.ConflictIndex != 0 {
			next = response.ConflictIndex
		} else if next > 1 {
			next--
		}
		if next < 1 {
			next = 1
		}
		n.nextIndex[peer] = next
	}
	return ErrNoQuorum
}

func (n *Node) installSnapshotPeerLocked(peer NodeID) error {
	if n.transport == nil || n.snapshot == nil {
		return ErrUnavailable
	}
	offset := uint64(0)
	for offset <= uint64(len(n.snapshot.Data)) {
		end := offset + maxSnapshotChunk
		if end > uint64(len(n.snapshot.Data)) {
			end = uint64(len(n.snapshot.Data))
		}
		chunk := append([]byte(nil), n.snapshot.Data[offset:end]...)
		request := InstallSnapshotRequest{
			Term:              n.term,
			LeaderID:          n.id,
			LastIncludedIndex: n.snapshot.Index,
			LastIncludedTerm:  n.snapshot.Term,
			Offset:            offset,
			Data:              chunk,
			Done:              end == uint64(len(n.snapshot.Data)),
			Checksum:          crc32.Checksum(chunk, raftChecksumTable),
		}
		ctx, cancel := context.WithTimeout(context.Background(), n.rpcTimeout)
		response, err := n.transport.InstallSnapshot(ctx, peer, request)
		cancel()
		if err != nil {
			return err
		}
		if response.Term > n.term {
			if err := n.becomeFollowerTermLocked(response.Term); err != nil {
				return err
			}
			return ErrStaleTerm
		}
		if response.Term != n.term || response.NextOffset > end {
			return fmt.Errorf("%w: invalid snapshot response", ErrInvalidMessage)
		}
		if response.NextOffset < offset {
			return fmt.Errorf("%w: snapshot response regressed offset", ErrInvalidMessage)
		}
		if response.NextOffset == offset && len(chunk) != 0 {
			return ErrNoQuorum
		}
		offset = response.NextOffset
		if request.Done && offset == end {
			return nil
		}
	}
	return nil
}

func (n *Node) appendRequestLocked(peer NodeID) (AppendEntriesRequest, error) {
	next := n.nextIndex[peer]
	if next == 0 {
		next = n.lastLogIndexLocked() + 1
		n.nextIndex[peer] = next
	}
	prev := next - 1
	prevTerm, ok := n.termAtLocked(prev)
	if !ok && prev != 0 {
		return AppendEntriesRequest{}, fmt.Errorf("%w: missing previous log term", ErrInvalidMessage)
	}
	entries := n.entriesFromLocked(next, maxAppendEntries)
	return AppendEntriesRequest{
		Term: n.term, LeaderID: n.id, PrevLogIndex: prev, PrevLogTerm: prevTerm,
		Entries: entries, LeaderCommit: n.commitIndex,
	}, nil
}

func (n *Node) entriesFromLocked(index Index, limit int) []Entry {
	if index == 0 || n.snapshot == nil && index > Index(len(n.log)) || n.snapshot != nil && index <= n.snapshot.Index {
		return nil
	}
	base := Index(0)
	if n.snapshot != nil {
		base = n.snapshot.Index
	}
	start := int(index - base - 1)
	if start < 0 || start >= len(n.log) {
		return nil
	}
	end := start + limit
	if end > len(n.log) {
		end = len(n.log)
	}
	entries := make([]Entry, end-start)
	for i := range entries {
		entries[i] = cloneEntry(n.log[start+i])
	}
	return entries
}

func (n *Node) advanceCommitLocked() error {
	if n.role != RoleLeader {
		return nil
	}
	target := n.commitIndex
	last := n.lastLogIndexLocked()
	for index := last; index > target; index-- {
		term, ok := n.termAtLocked(index)
		if !ok || term != n.term {
			continue
		}
		votes := 0
		for _, peer := range n.peers {
			if peer == n.id && n.matchIndex[peer] >= index || peer != n.id && n.matchIndex[peer] >= index {
				votes++
			}
		}
		if votes >= n.quorumLocked() {
			target = index
			break
		}
	}
	if target == n.commitIndex {
		return n.applyCommittedLocked()
	}
	hardState := n.hardStateLocked(target)
	if err := n.persistLocked(PersistBatch{HardState: &hardState}); err != nil {
		return err
	}
	n.commitIndex = target
	return n.applyCommittedLocked()
}

func (n *Node) quorumLocked() int { return len(n.peers)/2 + 1 }

func (n *Node) hardStateLocked(commit Index) HardState {
	return HardState{CurrentTerm: n.term, VotedFor: n.votedFor, CommitIndex: commit}
}

func (n *Node) persistLocked(batch PersistBatch) error {
	if n.store == nil {
		return nil
	}
	if err := n.store.Persist(batch); err != nil {
		return n.failLocked(err)
	}
	return nil
}

func (n *Node) failLocked(err error) error {
	n.healthy = false
	n.lastError = err
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

func (n *Node) applyCommittedLocked() error {
	if n.apply == nil {
		return nil
	}
	for n.lastApplied < n.commitIndex {
		index := n.lastApplied + 1
		entry, ok := n.entryAtLocked(index)
		if !ok {
			return n.failLocked(fmt.Errorf("%w: committed entry %d is unavailable", ErrInvalidMessage, index))
		}
		if n.apply != nil {
			if err := n.apply(cloneEntry(entry)); err != nil {
				return n.failLocked(err)
			}
		}
		n.lastApplied = index
	}
	return nil
}

// Close stops the node and closes its outbound transport.
func (n *Node) Close() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	if n.transport != nil {
		return n.transport.Close()
	}
	return nil
}

func cloneEntry(entry Entry) Entry {
	entry.Command = append([]byte(nil), entry.Command...)
	return entry
}

func cloneSnapshot(snapshot *Snapshot) *Snapshot {
	if snapshot == nil {
		return nil
	}
	clone := *snapshot
	clone.Data = append([]byte(nil), snapshot.Data...)
	return &clone
}

func (n *Node) lastLogIndexLocked() Index {
	if len(n.log) > 0 {
		return n.log[len(n.log)-1].Index
	}
	if n.snapshot != nil {
		return n.snapshot.Index
	}
	return 0
}

func (n *Node) lastLogTermLocked() Term {
	if len(n.log) > 0 {
		return n.log[len(n.log)-1].Term
	}
	if n.snapshot != nil {
		return n.snapshot.Term
	}
	return 0
}

func (n *Node) termAtLocked(index Index) (Term, bool) {
	if index == 0 {
		return 0, true
	}
	if n.snapshot != nil && index == n.snapshot.Index {
		return n.snapshot.Term, true
	}
	base := Index(0)
	if n.snapshot != nil {
		base = n.snapshot.Index
	}
	if index <= base || index-base-1 >= Index(len(n.log)) {
		return 0, false
	}
	return n.log[index-base-1].Term, true
}

func (n *Node) entryAtLocked(index Index) (Entry, bool) {
	if n.snapshot != nil && index == n.snapshot.Index {
		return Entry{Index: n.snapshot.Index, Term: n.snapshot.Term, Command: append([]byte(nil), n.snapshot.Data...)}, true
	}
	base := Index(0)
	if n.snapshot != nil {
		base = n.snapshot.Index
	}
	if index <= base || index-base-1 >= Index(len(n.log)) {
		return Entry{}, false
	}
	return cloneEntry(n.log[index-base-1]), true
}
