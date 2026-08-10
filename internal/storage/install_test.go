package storage

import (
	"errors"
	"reflect"
	"testing"

	"viper/internal/raft"
)

func TestInstallSnapshotCompactsMissingPrefixAndRecovers(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	hardState := raft.HardState{CurrentTerm: 2, VotedFor: "node-1", CommitIndex: 3}
	if err := store.Persist(raft.PersistBatch{
		HardState: &hardState,
		Entries: []raft.Entry{
			{Index: 1, Term: 1, Command: []byte("one")},
			{Index: 2, Term: 1, Command: []byte("two")},
			{Index: 3, Term: 2, Command: []byte("three")},
		},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	snapshot := raft.Snapshot{Index: 2, Term: 1, Data: []byte("state-at-two")}
	if err := store.InstallSnapshot(snapshot, hardState); err != nil {
		t.Fatalf("install snapshot: %v", err)
	}
	state, err := store.State()
	if err != nil {
		t.Fatalf("read installed state: %v", err)
	}
	want := raft.PersistentState{
		HardState: hardState,
		Snapshot:  &snapshot,
		Entries:   []raft.Entry{{Index: 3, Term: 2, Command: []byte("three")}},
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("installed state = %#v, want %#v", state, want)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	recovered := openTestStore(t, directory)
	t.Cleanup(func() { _ = recovered.Close() })
	recoveredState, err := recovered.State()
	if err != nil {
		t.Fatalf("read recovered installed state: %v", err)
	}
	if !reflect.DeepEqual(recoveredState, want) {
		t.Fatalf("recovered installed state = %#v, want %#v", recoveredState, want)
	}
}

func TestInstallSnapshotCanCreateCheckpointWithoutLocalPrefix(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	hardState := raft.HardState{CurrentTerm: 3, CommitIndex: 5}
	snapshot := raft.Snapshot{Index: 5, Term: 2, Data: []byte("remote-state")}
	if err := store.InstallSnapshot(snapshot, hardState); err != nil {
		t.Fatalf("install remote snapshot: %v", err)
	}
	state, err := store.State()
	if err != nil {
		t.Fatalf("read remote snapshot state: %v", err)
	}
	if !reflect.DeepEqual(state.Snapshot, &snapshot) || len(state.Entries) != 0 || state.HardState != hardState {
		t.Fatalf("remote snapshot state = %#v", state)
	}
	if err := store.InstallSnapshot(raft.Snapshot{Index: 4, Term: 2, Data: []byte("older")}, hardState); !errors.Is(err, ErrInvalid) {
		t.Fatalf("regressing snapshot error = %v, want ErrInvalid", err)
	}
}
