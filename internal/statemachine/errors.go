package statemachine

import "errors"

var (
	// ErrInvalidCommand indicates malformed or unsupported replicated input.
	ErrInvalidCommand = errors.New("invalid state-machine command")
	// ErrOutOfOrder indicates that an apply index was skipped or replayed.
	ErrOutOfOrder = errors.New("state-machine apply index is out of order")
	// ErrInvalidSnapshot indicates corrupt or unsupported snapshot bytes.
	ErrInvalidSnapshot = errors.New("invalid state-machine snapshot")
	// ErrSnapshotTooLarge indicates that a snapshot exceeds the memory bound.
	ErrSnapshotTooLarge = errors.New("state-machine snapshot exceeds the size limit")
	// ErrSnapshotRegression prevents replacing newer applied state with older state.
	ErrSnapshotRegression = errors.New("state-machine snapshot regresses the applied index")
)
