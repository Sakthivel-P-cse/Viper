package raft

import "errors"

var (
	// ErrInvalidConfig indicates an invalid consensus-node configuration.
	ErrInvalidConfig = errors.New("invalid raft configuration")
	// ErrNotLeader indicates that a proposal was sent to a non-leader node.
	ErrNotLeader = errors.New("raft node is not the leader")
	// ErrNoQuorum indicates that a proposal has not reached a majority.
	ErrNoQuorum = errors.New("raft quorum is unavailable")
	// ErrUnavailable indicates that the node cannot safely serve work.
	ErrUnavailable = errors.New("raft node is unavailable")
	// ErrClosed indicates that the node has been shut down.
	ErrClosed = errors.New("raft node is closed")
	// ErrInvalidMessage indicates malformed consensus input.
	ErrInvalidMessage = errors.New("invalid raft message")
	// ErrStaleTerm indicates that an outbound response was superseded by a newer term.
	ErrStaleTerm = errors.New("raft response belongs to a stale term")
)
