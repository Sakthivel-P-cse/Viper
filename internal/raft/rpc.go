package raft

import "context"

// RequestVoteRequest asks a peer to grant a vote in an election term.
type RequestVoteRequest struct {
	CandidateID  NodeID
	LastLogIndex Index
	LastLogTerm  Term
	Term         Term
}

// RequestVoteResponse reports the receiver's term and vote decision.
type RequestVoteResponse struct {
	Term        Term
	VoteGranted bool
}

// AppendEntriesRequest carries a heartbeat or a contiguous log suffix.
type AppendEntriesRequest struct {
	LeaderID     NodeID
	Entries      []Entry
	LeaderCommit Index
	PrevLogIndex Index
	PrevLogTerm  Term
	Term         Term
}

// AppendEntriesResponse reports replication progress or a conflict hint.
type AppendEntriesResponse struct {
	ConflictIndex Index
	ConflictTerm  Term
	MatchIndex    Index
	Term          Term
	Success       bool
}

// InstallSnapshotRequest carries one independently checksummed snapshot chunk.
type InstallSnapshotRequest struct {
	LeaderID          NodeID
	Data              []byte
	LastIncludedIndex Index
	LastIncludedTerm  Term
	Offset            uint64
	Term              Term
	Checksum          uint32
	Done              bool
}

// InstallSnapshotResponse identifies the next byte offset expected by a peer.
type InstallSnapshotResponse struct {
	NextOffset uint64
	Term       Term
}

// PeerHandler is implemented by a node's consensus runtime. Transport
// adapters call it only after validating and bounding an inbound request.
type PeerHandler interface {
	HandleRequestVote(context.Context, RequestVoteRequest) (RequestVoteResponse, error)
	HandleAppendEntries(context.Context, AppendEntriesRequest) (AppendEntriesResponse, error)
	HandleInstallSnapshot(context.Context, InstallSnapshotRequest) (InstallSnapshotResponse, error)
}

// PeerTransport is the outbound transport port consumed by the Raft runtime.
// Implementations must honor context cancellation and make Close idempotent.
type PeerTransport interface {
	RequestVote(context.Context, NodeID, RequestVoteRequest) (RequestVoteResponse, error)
	AppendEntries(context.Context, NodeID, AppendEntriesRequest) (AppendEntriesResponse, error)
	InstallSnapshot(context.Context, NodeID, InstallSnapshotRequest) (InstallSnapshotResponse, error)
	Close() error
}
