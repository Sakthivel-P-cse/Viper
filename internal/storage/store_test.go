package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"viper/internal/raft"
)

func TestPersistAndRecover(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	want := raft.PersistentState{
		HardState: raft.HardState{CurrentTerm: 2, VotedFor: "node-02", CommitIndex: 2},
		Entries: []raft.Entry{
			{Index: 1, Term: 1, Command: []byte("set:a=1")},
			{Index: 2, Term: 2, Command: []byte("set:b=2")},
		},
	}
	if err := store.Persist(raft.PersistBatch{HardState: &want.HardState, Entries: want.Entries}); err != nil {
		t.Fatalf("persist state: %v", err)
	}

	got, err := store.State()
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state mismatch: got %#v, want %#v", got, want)
	}
	got.Entries[0].Command[0] = 'X'
	isolated, err := store.State()
	if err != nil {
		t.Fatalf("read isolated state: %v", err)
	}
	if string(isolated.Entries[0].Command) != "set:a=1" {
		t.Fatal("State exposed mutable durable command memory")
	}

	if err = store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	recovered := openTestStore(t, directory)
	t.Cleanup(func() { _ = recovered.Close() })
	got, err = recovered.State()
	if err != nil {
		t.Fatalf("read recovered state: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered state mismatch: got %#v, want %#v", got, want)
	}
}

func TestInvalidBatchDoesNotMutateState(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	hardState := raft.HardState{CurrentTerm: 1, CommitIndex: 1}
	if err := store.Persist(raft.PersistBatch{
		HardState: &hardState,
		Entries:   []raft.Entry{{Index: 1, Term: 1, Command: []byte("one")}},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	want, err := store.State()
	if err != nil {
		t.Fatalf("read seeded state: %v", err)
	}

	badEntry := raft.Entry{Index: 3, Term: 1, Command: []byte("gap")}
	if err = store.Persist(raft.PersistBatch{Entries: []raft.Entry{badEntry}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-contiguous entry error = %v, want ErrInvalid", err)
	}
	truncateCommitted := raft.Index(1)
	if err = store.Persist(raft.PersistBatch{TruncateFrom: &truncateCommitted}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("committed truncation error = %v, want ErrInvalid", err)
	}
	got, err := store.State()
	if err != nil {
		t.Fatalf("read unchanged state: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("invalid writes mutated state: got %#v, want %#v", got, want)
	}
}

func TestRecoveryTruncatesOnlyIncompleteTail(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	seedTwoEntries(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	walPath := filepath.Join(directory, walFilename)
	before, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	payload, err := encodeBatch(raft.PersistBatch{Entries: []raft.Entry{{Index: 3, Term: 2}}})
	if err != nil {
		t.Fatalf("encode partial batch: %v", err)
	}
	record, err := encodeRecord(recordBatch, payload)
	if err != nil {
		t.Fatalf("encode partial record: %v", err)
	}
	appendFile, err := os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open WAL for partial append: %v", err)
	}
	if _, err = appendFile.Write(record[:recordHeaderSize+2]); err != nil {
		_ = appendFile.Close()
		t.Fatalf("append partial record: %v", err)
	}
	if err = appendFile.Close(); err != nil {
		t.Fatalf("close partial WAL: %v", err)
	}

	recovered := openTestStore(t, directory)
	t.Cleanup(func() { _ = recovered.Close() })
	got, err := recovered.State()
	if err != nil {
		t.Fatalf("read recovered state: %v", err)
	}
	if len(got.Entries) != 2 || got.HardState.CommitIndex != 2 {
		t.Fatalf("partial tail changed recovered state: %#v", got)
	}
	after, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat recovered WAL: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("WAL size after recovery = %d, want %d", after.Size(), before.Size())
	}
}

func TestRecoveryFailsClosedOnHeaderFragments(t *testing.T) {
	payload, err := encodeBatch(raft.PersistBatch{Entries: []raft.Entry{{Index: 2, Term: 1}}})
	if err != nil {
		t.Fatalf("encode fragment batch: %v", err)
	}
	record, err := encodeRecord(recordBatch, payload)
	if err != nil {
		t.Fatalf("encode fragment record: %v", err)
	}
	for _, fragmentLength := range []int{1, 4, recordHeaderSize - 1} {
		for _, afterValidRecord := range []bool{false, true} {
			name := fmt.Sprintf("length_%d_after_valid_%t", fragmentLength, afterValidRecord)
			t.Run(name, func(t *testing.T) {
				directory := t.TempDir()
				walPath := filepath.Join(directory, walFilename)
				if afterValidRecord {
					store := openTestStore(t, directory)
					seedTwoEntries(t, store)
					if closeErr := store.Close(); closeErr != nil {
						t.Fatalf("close seeded store: %v", closeErr)
					}
					file, openErr := os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, 0o600)
					if openErr != nil {
						t.Fatalf("open WAL for fragment: %v", openErr)
					}
					if _, writeErr := file.Write(record[:fragmentLength]); writeErr != nil {
						_ = file.Close()
						t.Fatalf("append header fragment: %v", writeErr)
					}
					if closeErr := file.Close(); closeErr != nil {
						t.Fatalf("close fragmented WAL: %v", closeErr)
					}
				} else if writeErr := os.WriteFile(walPath, record[:fragmentLength], 0o600); writeErr != nil {
					t.Fatalf("write header fragment: %v", writeErr)
				}
				before, statErr := os.Stat(walPath)
				if statErr != nil {
					t.Fatalf("stat fragmented WAL: %v", statErr)
				}
				if _, openErr := Open(directory); !errors.Is(openErr, ErrCorrupt) {
					t.Fatalf("Open header fragment error = %v, want ErrCorrupt", openErr)
				}
				after, statErr := os.Stat(walPath)
				if statErr != nil {
					t.Fatalf("stat rejected WAL: %v", statErr)
				}
				if after.Size() != before.Size() {
					t.Fatalf("corrupt WAL was mutated: size %d became %d", before.Size(), after.Size())
				}
			})
		}
	}
}

func TestRecoveryFailsClosedOnChecksumCorruption(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	seedTwoEntries(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	walPath := filepath.Join(directory, walFilename)
	file, err := os.OpenFile(walPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open WAL for corruption: %v", err)
	}
	byteAtOffset := []byte{0}
	if _, err = file.ReadAt(byteAtOffset, recordHeaderSize+4); err != nil {
		_ = file.Close()
		t.Fatalf("read WAL byte: %v", err)
	}
	byteAtOffset[0] ^= 0xff
	if _, err = file.WriteAt(byteAtOffset, recordHeaderSize+4); err != nil {
		_ = file.Close()
		t.Fatalf("corrupt WAL byte: %v", err)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("close corrupted WAL: %v", err)
	}
	if _, err = Open(directory); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open corrupted WAL error = %v, want ErrCorrupt", err)
	}
}

func TestSnapshotCompactsAndRecovers(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	seedThreeEntries(t, store)
	snapshot := raft.Snapshot{Index: 2, Term: 1, Data: []byte("state-at-two")}
	if err := store.SaveSnapshot(snapshot); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	got, err := store.State()
	if err != nil {
		t.Fatalf("read compacted state: %v", err)
	}
	if !reflect.DeepEqual(got.Snapshot, &snapshot) || len(got.Entries) != 1 || got.Entries[0].Index != 3 {
		t.Fatalf("unexpected compacted state: %#v", got)
	}
	got.Snapshot.Data[0] = 'X'
	isolated, err := store.State()
	if err != nil {
		t.Fatalf("read isolated snapshot: %v", err)
	}
	if string(isolated.Snapshot.Data) != "state-at-two" {
		t.Fatal("State exposed mutable snapshot memory")
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	recovered := openTestStore(t, directory)
	t.Cleanup(func() { _ = recovered.Close() })
	got, err = recovered.State()
	if err != nil {
		t.Fatalf("read recovered compacted state: %v", err)
	}
	if !reflect.DeepEqual(got.Snapshot, &snapshot) || len(got.Entries) != 1 || got.Entries[0].Index != 3 {
		t.Fatalf("unexpected recovered compacted state: %#v", got)
	}
}

func TestRecoveryHandlesSnapshotActivationBeforeWALCompaction(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	seedThreeEntries(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	snapshot := raft.Snapshot{Index: 2, Term: 1, Data: []byte("activated")}
	if err := writeSnapshot(filepath.Join(directory, snapshotFilename), snapshot, nil); err != nil {
		t.Fatalf("activate snapshot without WAL compaction: %v", err)
	}

	recovered := openTestStore(t, directory)
	t.Cleanup(func() { _ = recovered.Close() })
	got, err := recovered.State()
	if err != nil {
		t.Fatalf("read recovered crash-window state: %v", err)
	}
	if !reflect.DeepEqual(got.Snapshot, &snapshot) || len(got.Entries) != 1 || got.Entries[0].Index != 3 {
		t.Fatalf("unexpected crash-window recovery: %#v", got)
	}
}

func TestRecoveryFailsClosedOnSnapshotCorruption(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	seedThreeEntries(t, store)
	if err := store.SaveSnapshot(raft.Snapshot{Index: 2, Term: 1, Data: []byte("snapshot")}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	snapshotPath := filepath.Join(directory, snapshotFilename)
	file, err := os.OpenFile(snapshotPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open snapshot for corruption: %v", err)
	}
	byteAtOffset := []byte{0}
	if _, err = file.ReadAt(byteAtOffset, snapshotHeaderSize); err != nil {
		_ = file.Close()
		t.Fatalf("read snapshot byte: %v", err)
	}
	byteAtOffset[0] ^= 0xff
	if _, err = file.WriteAt(byteAtOffset, snapshotHeaderSize); err != nil {
		_ = file.Close()
		t.Fatalf("corrupt snapshot byte: %v", err)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("close corrupted snapshot: %v", err)
	}
	if _, err = Open(directory); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open corrupted snapshot error = %v, want ErrCorrupt", err)
	}
}

func TestCloseIsIdempotentAndRejectsFurtherUse(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	if err := store.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := store.State(); !errors.Is(err, ErrClosed) {
		t.Fatalf("State after close error = %v, want ErrClosed", err)
	}
	if err := store.Persist(raft.PersistBatch{Entries: []raft.Entry{{Index: 1, Term: 1}}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Persist after close error = %v, want ErrClosed", err)
	}
}

func TestOpenRequiresPreProvisionedDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "not-created")
	if _, err := Open(directory); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Open missing directory error = %v, want ErrInvalid", err)
	}
}

func TestOversizedBatchIsRejectedBeforePersistence(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	command := make([]byte, 3*1024*1024)
	entries := make([]raft.Entry, 6)
	for index := range entries {
		entries[index] = raft.Entry{Command: command, Index: raft.Index(index + 1), Term: 1}
	}
	hardState := raft.HardState{CurrentTerm: 1, CommitIndex: 6}
	if err := store.Persist(raft.PersistBatch{HardState: &hardState, Entries: entries}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized batch error = %v, want ErrInvalid", err)
	}
	state, err := store.State()
	if err != nil {
		t.Fatalf("read state after oversized batch: %v", err)
	}
	if len(state.Entries) != 0 || state.HardState != (raft.HardState{}) {
		t.Fatalf("oversized batch mutated state: %#v", state)
	}
}

func TestPersistenceFailurePoisonsStore(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	store.inject = failAt(failureWALSync)
	hardState := raft.HardState{CurrentTerm: 1, CommitIndex: 1}
	err := store.Persist(raft.PersistBatch{
		HardState: &hardState,
		Entries:   []raft.Entry{{Index: 1, Term: 1, Command: []byte("one")}},
	})
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("sync failure error = %v, want ErrUnhealthy", err)
	}
	if _, err = store.State(); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("State after sync failure error = %v, want ErrUnhealthy", err)
	}
	if err = store.Persist(raft.PersistBatch{Entries: []raft.Entry{{Index: 1, Term: 1}}}); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("second Persist error = %v, want ErrUnhealthy", err)
	}
}

func TestWALWriteFailuresRecoverOnlyCompleteRecords(t *testing.T) {
	testCases := []struct {
		configure      func(*Store)
		name           string
		wantEntryCount int
	}{
		{
			name: "zero bytes",
			configure: func(store *Store) {
				store.inject = failAt(failureWALWrite)
			},
			wantEntryCount: 1,
		},
		{
			name: "partial record",
			configure: func(store *Store) {
				store.writeWAL = func(file *os.File, record []byte) error {
					if _, err := file.Write(record[:len(record)/2]); err != nil {
						return err
					}
					return errInjectedFailure
				}
			},
			wantEntryCount: 1,
		},
		{
			name: "complete record with error",
			configure: func(store *Store) {
				store.writeWAL = func(file *os.File, record []byte) error {
					if err := writeAll(file, record); err != nil {
						return err
					}
					return errInjectedFailure
				}
			},
			wantEntryCount: 2,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			store := openTestStore(t, directory)
			firstHardState := raft.HardState{CurrentTerm: 1, CommitIndex: 1}
			if err := store.Persist(raft.PersistBatch{
				HardState: &firstHardState,
				Entries:   []raft.Entry{{Index: 1, Term: 1, Command: []byte("one")}},
			}); err != nil {
				t.Fatalf("seed WAL: %v", err)
			}
			testCase.configure(store)
			secondHardState := raft.HardState{CurrentTerm: 1, CommitIndex: 2}
			err := store.Persist(raft.PersistBatch{
				HardState: &secondHardState,
				Entries:   []raft.Entry{{Index: 2, Term: 1, Command: []byte("two")}},
			})
			if !errors.Is(err, ErrUnhealthy) {
				t.Fatalf("Persist error = %v, want ErrUnhealthy", err)
			}
			if _, err = store.State(); !errors.Is(err, ErrUnhealthy) {
				t.Fatalf("State after write failure error = %v, want ErrUnhealthy", err)
			}
			_ = store.Close()

			recovered := openTestStore(t, directory)
			t.Cleanup(func() { _ = recovered.Close() })
			state, stateErr := recovered.State()
			if stateErr != nil {
				t.Fatalf("read recovered state: %v", stateErr)
			}
			if len(state.Entries) != testCase.wantEntryCount || state.HardState.CommitIndex != raft.Index(testCase.wantEntryCount) {
				t.Fatalf("recovered state = %#v, want %d complete entries", state, testCase.wantEntryCount)
			}
		})
	}
}

func TestNoOpTruncationDoesNotCopyLog(t *testing.T) {
	entries := make([]raft.Entry, 10_000, 10_001)
	for index := range entries {
		entries[index] = raft.Entry{Index: raft.Index(index + 1), Term: 1}
	}
	state := recoveredState{
		entries:   entries,
		hardState: raft.HardState{CurrentTerm: 1, CommitIndex: 10_000},
	}
	truncateFrom := raft.Index(10_001)
	next, err := applyBatch(state, raft.PersistBatch{TruncateFrom: &truncateFrom})
	if err != nil {
		t.Fatalf("apply no-op truncation: %v", err)
	}
	if &next.entries[0] != &state.entries[0] {
		t.Fatal("no-op truncation copied the durable log")
	}
}

func TestRepeatedTailReplacementReusesOwnedLog(t *testing.T) {
	entries := make([]raft.Entry, 10_000)
	for index := range entries {
		entries[index] = raft.Entry{Index: raft.Index(index + 1), Term: 1}
	}
	state := recoveredState{
		entries:   entries,
		hardState: raft.HardState{CurrentTerm: 1, CommitIndex: 9_999},
	}
	firstEntry := &state.entries[0]
	truncateFrom := raft.Index(10_000)
	for iteration := 0; iteration < 1_000; iteration++ {
		next, err := applyBatch(state, raft.PersistBatch{
			TruncateFrom: &truncateFrom,
			Entries:      []raft.Entry{{Index: 10_000, Term: 1, Command: []byte("replacement")}},
		})
		if err != nil {
			t.Fatalf("replace tail at iteration %d: %v", iteration, err)
		}
		state = next
	}
	if &state.entries[0] != firstEntry {
		t.Fatal("tail replacement copied the retained log prefix")
	}
}

func TestTruncationClearsDiscardedCommands(t *testing.T) {
	entries := []raft.Entry{
		{Index: 1, Term: 1, Command: []byte("keep")},
		{Index: 2, Term: 1, Command: []byte("discard-two")},
		{Index: 3, Term: 1, Command: []byte("discard-three")},
	}
	state := recoveredState{
		entries:   entries,
		hardState: raft.HardState{CurrentTerm: 1, CommitIndex: 1},
	}
	firstEntry := &state.entries[0]
	truncateFrom := raft.Index(2)
	next, err := applyBatch(state, raft.PersistBatch{TruncateFrom: &truncateFrom})
	if err != nil {
		t.Fatalf("truncate suffix: %v", err)
	}
	if &next.entries[0] != firstEntry {
		t.Fatal("suffix truncation replaced the owned backing array")
	}
	backing := next.entries[:cap(next.entries)]
	for position := len(next.entries); position < len(backing); position++ {
		if backing[position].Index != 0 || backing[position].Term != 0 || backing[position].Command != nil {
			t.Fatalf("discarded entry %d remains reachable: %#v", position, backing[position])
		}
	}
}

func TestRecoveryRejectsDuplicateMaximumIndex(t *testing.T) {
	directory := t.TempDir()
	maximumIndex := ^raft.Index(0)
	payload, err := encodeCheckpoint(checkpoint{
		baseIndex: maximumIndex - 1,
		baseTerm:  1,
		hardState: raft.HardState{CurrentTerm: 1, CommitIndex: maximumIndex},
		entries: []raft.Entry{
			{Index: maximumIndex, Term: 1},
			{Index: maximumIndex, Term: 1},
		},
	})
	if err != nil {
		t.Fatalf("encode corrupt checkpoint: %v", err)
	}
	record, err := encodeRecord(recordCheckpoint, payload)
	if err != nil {
		t.Fatalf("encode corrupt record: %v", err)
	}
	if err = os.WriteFile(filepath.Join(directory, walFilename), record, 0o600); err != nil {
		t.Fatalf("write corrupt checkpoint: %v", err)
	}
	_, err = Open(directory)
	var corruption *CorruptionError
	if !errors.As(err, &corruption) || corruption.Reason != "checkpoint violates storage invariants" {
		t.Fatalf("Open duplicate maximum index error = %v, want checkpoint corruption", err)
	}
}

func TestSnapshotFailuresFailClosedAndRecover(t *testing.T) {
	testCases := []struct {
		name           string
		point          failurePoint
		snapshotActive bool
	}{
		{name: "snapshot write", point: failureSnapshotWrite},
		{name: "snapshot sync", point: failureSnapshotSync},
		{name: "snapshot rename", point: failureSnapshotRename},
		{name: "snapshot directory sync", point: failureSnapshotDirectorySync, snapshotActive: true},
		{name: "WAL compact write", point: failureWALCompactWrite, snapshotActive: true},
		{name: "WAL compact sync", point: failureWALCompactSync, snapshotActive: true},
		{name: "WAL rename", point: failureWALRename, snapshotActive: true},
		{name: "WAL directory sync", point: failureWALDirectorySync, snapshotActive: true},
		{name: "WAL reopen", point: failureWALReopen, snapshotActive: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			store := openTestStore(t, directory)
			seedThreeEntries(t, store)
			store.inject = failAt(testCase.point)
			snapshot := raft.Snapshot{Index: 2, Term: 1, Data: []byte("state-at-two")}
			err := store.SaveSnapshot(snapshot)
			if !errors.Is(err, ErrUnhealthy) {
				t.Fatalf("SaveSnapshot error = %v, want ErrUnhealthy", err)
			}
			if _, err = store.State(); !errors.Is(err, ErrUnhealthy) {
				t.Fatalf("State after snapshot failure error = %v, want ErrUnhealthy", err)
			}
			_ = store.Close()

			recovered := openTestStore(t, directory)
			t.Cleanup(func() { _ = recovered.Close() })
			state, stateErr := recovered.State()
			if stateErr != nil {
				t.Fatalf("read recovered state: %v", stateErr)
			}
			if testCase.snapshotActive {
				if !reflect.DeepEqual(state.Snapshot, &snapshot) || len(state.Entries) != 1 || state.Entries[0].Index != 3 {
					t.Fatalf("unexpected active-snapshot recovery: %#v", state)
				}
			} else if state.Snapshot != nil || len(state.Entries) != 3 {
				t.Fatalf("unexpected pre-activation recovery: %#v", state)
			}
		})
	}
}

func TestWriteAllRejectsShortWrite(t *testing.T) {
	if err := writeAll(zeroWriter{}, []byte("data")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeAll error = %v, want io.ErrShortWrite", err)
	}
}

var errInjectedFailure = errors.New("injected filesystem failure")

func failAt(want failurePoint) failureInjector {
	return func(got failurePoint) error {
		if got == want {
			return errInjectedFailure
		}
		return nil
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) {
	return 0, nil
}

func openTestStore(t *testing.T, directory string) *Store {
	t.Helper()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func seedTwoEntries(t *testing.T, store *Store) {
	t.Helper()
	hardState := raft.HardState{CurrentTerm: 2, VotedFor: "node-01", CommitIndex: 2}
	entries := []raft.Entry{
		{Index: 1, Term: 1, Command: []byte("one")},
		{Index: 2, Term: 1, Command: []byte("two")},
	}
	if err := store.Persist(raft.PersistBatch{HardState: &hardState, Entries: entries}); err != nil {
		t.Fatalf("seed two entries: %v", err)
	}
}

func seedThreeEntries(t *testing.T, store *Store) {
	t.Helper()
	hardState := raft.HardState{CurrentTerm: 2, VotedFor: "node-01", CommitIndex: 3}
	entries := []raft.Entry{
		{Index: 1, Term: 1, Command: []byte("one")},
		{Index: 2, Term: 1, Command: []byte("two")},
		{Index: 3, Term: 2, Command: []byte("three")},
	}
	if err := store.Persist(raft.PersistBatch{HardState: &hardState, Entries: entries}); err != nil {
		t.Fatalf("seed three entries: %v", err)
	}
}
