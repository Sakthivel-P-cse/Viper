package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"viper/internal/raft"
)

const (
	BackupVersion      = 1
	MaxBackupBytes     = 128 * 1024 * 1024
	backupChecksumSize = sha256.Size * 2
)

// backupPayload is deliberately separate from the live storage record format;
// a backup is an operator artifact and has its own compatibility version.
type backupPayload struct {
	Version int                  `json:"version"`
	State   raft.PersistentState `json:"state"`
}

type backupDocument struct {
	Version  int                  `json:"version"`
	State    raft.PersistentState `json:"state"`
	Checksum string               `json:"checksum"`
}

// ExportBackup serializes a verified point-in-time durable state with a
// content checksum. It never includes filesystem paths or process metadata.
func (s *Store) ExportBackup() ([]byte, error) {
	if s == nil {
		return nil, ErrClosed
	}
	state, err := s.State()
	if err != nil {
		return nil, err
	}
	if err := validateBackupState(state); err != nil {
		return nil, err
	}
	payload := backupPayload{Version: BackupVersion, State: state}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode backup payload: %w", err)
	}
	document := backupDocument{Version: BackupVersion, State: state, Checksum: checksumText(encodedPayload)}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode backup: %w", err)
	}
	if len(encoded) > MaxBackupBytes {
		return nil, ErrInvalid
	}
	return encoded, nil
}

// VerifyBackup validates a backup without opening or changing a store.
func VerifyBackup(data []byte) (raft.PersistentState, error) {
	if len(data) == 0 || len(data) > MaxBackupBytes {
		return raft.PersistentState{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document backupDocument
	if err := decoder.Decode(&document); err != nil {
		return raft.PersistentState{}, fmt.Errorf("%w: invalid backup JSON", ErrInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return raft.PersistentState{}, fmt.Errorf("%w: trailing backup data", ErrInvalid)
	}
	if document.Version != BackupVersion || document.Checksum == "" {
		return raft.PersistentState{}, fmt.Errorf("%w: unsupported backup version", ErrInvalid)
	}
	payload, err := json.Marshal(backupPayload{Version: document.Version, State: document.State})
	if err != nil || !equalChecksum(document.Checksum, checksumText(payload)) {
		return raft.PersistentState{}, fmt.Errorf("%w: backup checksum mismatch", ErrInvalid)
	}
	if err := validateBackupState(document.State); err != nil {
		return raft.PersistentState{}, err
	}
	return clonePersistentState(document.State), nil
}

// RestoreBackup creates a store from a verified backup only in a directory
// that does not already contain Viper WAL or snapshot files. Existing durable
// state is never overwritten by this API.
func RestoreBackup(directory string, data []byte) (*Store, error) {
	state, err := VerifyBackup(data)
	if err != nil {
		return nil, err
	}
	if directory == "" {
		return nil, ErrInvalid
	}
	if info, statErr := os.Stat(directory); statErr != nil || !info.IsDir() {
		return nil, ErrInvalid
	}
	for _, name := range []string{walFilename, snapshotFilename} {
		if _, statErr := os.Lstat(filepath.Join(directory, name)); statErr == nil {
			return nil, fmt.Errorf("%w: restore target already contains durable state", ErrInvalid)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: inspect restore target", ErrInvalid)
		}
	}
	store, err := Open(directory)
	if err != nil {
		return nil, err
	}
	if state.Snapshot == nil {
		if state.HardState != (raft.HardState{}) || len(state.Entries) != 0 {
			if err := store.Persist(raft.PersistBatch{HardState: &state.HardState, Entries: state.Entries}); err != nil {
				_ = store.Close()
				return nil, err
			}
		}
		return store, nil
	}
	next := recoveredState{
		entries:   cloneEntries(state.Entries),
		hardState: state.HardState,
		baseIndex: state.Snapshot.Index,
		baseTerm:  state.Snapshot.Term,
	}
	if err := writeSnapshot(filepath.Join(directory, snapshotFilename), *cloneSnapshot(state.Snapshot), nil); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.rewriteWAL(next); err != nil {
		_ = store.Close()
		return nil, err
	}
	store.state = next
	store.snapshot = cloneSnapshot(state.Snapshot)
	return store, nil
}

func validateBackupState(state raft.PersistentState) error {
	baseIndex, baseTerm := raft.Index(0), raft.Term(0)
	if state.Snapshot != nil {
		if state.Snapshot.Index == 0 || state.Snapshot.Term == 0 || len(state.Snapshot.Data) > maxSnapshotBytes {
			return fmt.Errorf("%w: invalid backup snapshot", ErrInvalid)
		}
		baseIndex, baseTerm = state.Snapshot.Index, state.Snapshot.Term
	}
	if err := validateRecoveredState(recoveredState{
		entries: state.Entries, hardState: state.HardState, baseIndex: baseIndex, baseTerm: baseTerm,
	}); err != nil {
		return fmt.Errorf("%w: backup state violates storage invariants", ErrInvalid)
	}
	if state.Snapshot != nil {
		term, found := termAt(recoveredState{entries: state.Entries, baseIndex: baseIndex, baseTerm: baseTerm}, state.Snapshot.Index)
		if !found || term != state.Snapshot.Term || state.HardState.CommitIndex < state.Snapshot.Index {
			return fmt.Errorf("%w: backup snapshot does not match durable state", ErrInvalid)
		}
	}
	return nil
}

func checksumText(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func equalChecksum(left, right string) bool {
	if len(left) != backupChecksumSize || len(right) != backupChecksumSize {
		return false
	}
	return left == right
}

func clonePersistentState(state raft.PersistentState) raft.PersistentState {
	return raft.PersistentState{HardState: state.HardState, Entries: cloneEntries(state.Entries), Snapshot: cloneSnapshot(state.Snapshot)}
}
