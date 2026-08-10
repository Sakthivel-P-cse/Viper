package transport

import (
	"errors"
	"fmt"

	"viper/internal/raft"
)

func unavailableError(err error) error {
	if err == nil {
		return errors.Join(ErrPeerUnavailable, raft.ErrUnavailable)
	}
	return errors.Join(err, ErrPeerUnavailable, raft.ErrUnavailable)
}

var (
	// ErrClosed indicates that a transport or network no longer accepts work.
	ErrClosed = errors.New("transport is closed")
	// ErrInvalidCluster indicates an invalid static membership definition.
	ErrInvalidCluster = errors.New("invalid static cluster")
	// ErrInvalidMessage indicates a malformed or unsafe peer message.
	ErrInvalidMessage = errors.New("invalid peer message")
	// ErrOverloaded indicates that a bounded peer concurrency limit was reached.
	ErrOverloaded = errors.New("peer transport is overloaded")
	// ErrPeerUnavailable indicates that a configured peer has no active endpoint.
	ErrPeerUnavailable = errors.New("peer is unavailable")
	// ErrUnknownPeer indicates that a node is not part of the static membership.
	ErrUnknownPeer = errors.New("unknown peer")
)

func invalidCluster(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidCluster, reason)
}

func invalidMessage(field, reason string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalidMessage, field, reason)
}
