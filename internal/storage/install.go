package storage

import (
	"fmt"
	"path/filepath"

	"viper/internal/raft"
)

var _ raft.SnapshotStore = (*Store)(nil)

// InstallSnapshot atomically activates a leader-provided snapshot even when
// the follower does not retain the covered log prefix. Matching entries after
// the snapshot are retained only when their local covered term agrees.
func (s *Store) InstallSnapshot(snapshot raft.Snapshot, hardState raft.HardState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthError(); err != nil {
		return err
	}
	if snapshot.Index == 0 || snapshot.Term == 0 || len(snapshot.Data) > maxSnapshotBytes {
		return invalid("invalid installed snapshot")
	}
	if hardState.CommitIndex < snapshot.Index {
		return invalid("installed snapshot is beyond the commit index")
	}
	if err := validateHardStateTransition(s.state.hardState, hardState); err != nil {
		return err
	}
	if snapshot.Index < s.state.baseIndex {
		return invalid("installed snapshot regresses durable progress")
	}

	next := s.state
	next.hardState = hardState
	next.baseIndex = snapshot.Index
	next.baseTerm = snapshot.Term
	if localTerm, found := termAt(s.state, snapshot.Index); found && localTerm == snapshot.Term {
		next.entries = retainedEntriesAfter(s.state.entries, snapshot.Index)
	} else {
		next.entries = nil
	}
	if err := writeSnapshot(filepath.Join(s.directory, snapshotFilename), snapshot, s.inject); err != nil {
		return s.poison(err)
	}
	if err := s.rewriteWAL(next); err != nil {
		return s.poison(fmt.Errorf("install snapshot WAL: %w", err))
	}
	s.state = next
	s.snapshot = cloneSnapshot(&snapshot)
	return nil
}
