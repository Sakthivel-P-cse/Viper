package raft

// Term is a monotonically increasing Raft election term.
type Term uint64

// Index is a one-based Raft log index. Zero denotes no log entry.
type Index uint64

// NodeID is the stable identity of a configured Raft peer.
type NodeID string

// HardState is consensus metadata that must be durable before an RPC response.
type HardState struct {
	VotedFor    NodeID
	CurrentTerm Term
	CommitIndex Index
}

// Entry is one durable Raft log entry.
type Entry struct {
	Command []byte
	Index   Index
	Term    Term
}

// Snapshot contains an opaque state-machine image at an applied log position.
type Snapshot struct {
	Data  []byte
	Index Index
	Term  Term
}

// PersistentState is the complete state recovered when a node starts.
type PersistentState struct {
	Entries   []Entry
	Snapshot  *Snapshot
	HardState HardState
}

// PersistBatch atomically changes hard state and/or the log. TruncateFrom
// removes entries at and after the supplied index before Entries are appended.
type PersistBatch struct {
	HardState    *HardState
	TruncateFrom *Index
	Entries      []Entry
}

// StableStore is the durable storage port consumed by the Raft core.
type StableStore interface {
	State() (PersistentState, error)
	Persist(PersistBatch) error
	SaveSnapshot(Snapshot) error
	Close() error
}
