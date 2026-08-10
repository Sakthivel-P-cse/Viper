package storage

import (
	"errors"
	"fmt"
)

var (
	// ErrClosed indicates use after the store was closed.
	ErrClosed = errors.New("storage is closed")
	// ErrInvalid indicates a persistence request that violates storage invariants.
	ErrInvalid = errors.New("invalid persistence request")
	// ErrCorrupt indicates durable data that cannot be recovered safely.
	ErrCorrupt = errors.New("storage corruption detected")
	// ErrUnhealthy indicates an uncertain filesystem result that requires reopen.
	ErrUnhealthy = errors.New("storage requires recovery")
	// ErrLocked indicates that another process currently owns the store directory.
	ErrLocked = errors.New("storage directory is already in use")
	// ErrLockUnsupported indicates that the current platform cannot enforce
	// single-process store ownership.
	ErrLockUnsupported = errors.New("storage directory locking is unsupported")
)

// CorruptionError reports a safe location and reason without exposing values.
type CorruptionError struct {
	Reason string
	Offset int64
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("%v at offset %d: %s", ErrCorrupt, e.Offset, e.Reason)
}

// Unwrap supports errors.Is(err, ErrCorrupt).
func (e *CorruptionError) Unwrap() error {
	return ErrCorrupt
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, reason)
}
