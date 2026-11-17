package storage

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"viper/internal/raft"
)

func TestBackupRoundTripAndSafeRestore(t *testing.T) {
	sourceDirectory := t.TempDir()
	source := openTestStore(t, sourceDirectory)
	hardState := raft.HardState{CurrentTerm: 2, VotedFor: "node-1", CommitIndex: 2}
	if err := source.Persist(raft.PersistBatch{
		HardState: &hardState,
		Entries: []raft.Entry{
			{Index: 1, Term: 1, Command: []byte("one")},
			{Index: 2, Term: 2, Command: []byte("two")},
		},
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	snapshot := raft.Snapshot{Index: 1, Term: 1, Data: []byte("state-at-one")}
	if err := source.SaveSnapshot(snapshot); err != nil {
		t.Fatalf("save source snapshot: %v", err)
	}
	backup, err := source.ExportBackup()
	if err != nil {
		t.Fatalf("export backup: %v", err)
	}
	verified, err := VerifyBackup(backup)
	if err != nil {
		t.Fatalf("verify backup: %v", err)
	}
	want := raft.PersistentState{HardState: hardState, Snapshot: &snapshot, Entries: []raft.Entry{{Index: 2, Term: 2, Command: []byte("two")}}}
	if !reflect.DeepEqual(verified, want) {
		t.Fatalf("verified backup = %#v, want %#v", verified, want)
	}

	restoreDirectory := t.TempDir()
	restored, err := RestoreBackup(restoreDirectory, backup)
	if err != nil {
		t.Fatalf("restore backup: %v", err)
	}
	t.Cleanup(func() { _ = restored.Close(); _ = source.Close() })
	restoredState, err := restored.State()
	if err != nil {
		t.Fatalf("read restored state: %v", err)
	}
	if !reflect.DeepEqual(restoredState, want) {
		t.Fatalf("restored state = %#v, want %#v", restoredState, want)
	}
	if _, err := RestoreBackup(restoreDirectory, backup); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overwrite restore error = %v, want ErrInvalid", err)
	}

	corrupt := bytes.Clone(backup)
	corrupt[len(corrupt)-2] ^= 1
	if _, err := VerifyBackup(corrupt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt backup error = %v, want ErrInvalid", err)
	}
}

func TestRestoreBackupWithLargeUnsnapshottedLog(t *testing.T) {
	source := openTestStore(t, t.TempDir())
	defer source.Close()
	for i := 1; i <= 17; i++ {
		hardState := raft.HardState{CurrentTerm: 1, CommitIndex: raft.Index(i)}
		if err := source.Persist(raft.PersistBatch{HardState: &hardState, Entries: []raft.Entry{{Index: raft.Index(i), Term: 1, Command: make([]byte, 1024*1024)}}}); err != nil {
			t.Fatal(err)
		}
	}
	backup, err := source.ExportBackup()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	restored, err := RestoreBackup(directory, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	restored = openTestStore(t, directory)
	defer restored.Close()
	state, err := restored.State()
	if err != nil || len(state.Entries) != 17 || state.HardState.CommitIndex != 17 {
		t.Fatalf("restored entries = %d, hard state = %#v, error = %v", len(state.Entries), state.HardState, err)
	}
}
