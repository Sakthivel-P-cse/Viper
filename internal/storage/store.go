package storage

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"viper/internal/raft"
	"viper/pkg/contracts"
)

const (
	walFilename      = "raft.wal"
	snapshotFilename = "snapshot.bin"
)

type recoveredState struct {
	entries   []raft.Entry
	hardState raft.HardState
	baseIndex raft.Index
	baseTerm  raft.Term
}

type preparedBatch struct {
	entries          []raft.Entry
	hardState        raft.HardState
	truncatePosition int
	hasTruncate      bool
}

// Store is a checksummed, fsync-backed implementation of raft.StableStore.
// A Store becomes unhealthy after an uncertain filesystem result and must be
// reopened before it accepts more writes.
type Store struct {
	directory string
	lock      *os.File
	wal       *os.File
	snapshot  *raft.Snapshot
	unhealthy error
	inject    failureInjector
	writeWAL  walWriteFunc
	state     recoveredState
	mu        sync.RWMutex
	closed    bool
}

var _ raft.StableStore = (*Store)(nil)

// Open recovers a durable store rooted at a pre-provisioned directory.
func Open(directory string) (*Store, error) {
	if directory == "" {
		return nil, invalid("storage directory is empty")
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, invalid("storage directory must already exist")
	}
	if err != nil {
		return nil, fmt.Errorf("stat storage directory: %w", err)
	}
	if !info.IsDir() {
		return nil, invalid("storage path is not a directory")
	}
	lock, err := acquireStoreLock(directory)
	if err != nil {
		return nil, err
	}
	failOpen := func(openErr error) (*Store, error) {
		return nil, errors.Join(openErr, releaseStoreLock(lock))
	}

	snapshot, err := loadSnapshot(filepath.Join(directory, snapshotFilename))
	if err != nil {
		return failOpen(err)
	}
	walPath := filepath.Join(directory, walFilename)
	if walInfo, statErr := os.Lstat(walPath); statErr == nil && !walInfo.Mode().IsRegular() {
		return failOpen(invalid("write-ahead log is not a regular file"))
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return failOpen(fmt.Errorf("stat write-ahead log: %w", statErr))
	}
	wal, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- walPath appends a fixed filename to the validated store root.
	if err != nil {
		return failOpen(fmt.Errorf("open write-ahead log: %w", err))
	}
	if err = wal.Chmod(0o600); err != nil {
		return failOpen(errors.Join(fmt.Errorf("secure write-ahead log: %w", err), wal.Close()))
	}
	if err = syncDirectory(directory, nil, failureNone); err != nil {
		return failOpen(errors.Join(fmt.Errorf("sync storage directory: %w", err), wal.Close()))
	}
	state, err := replayWAL(wal)
	if err != nil {
		return failOpen(errors.Join(err, wal.Close()))
	}
	state, err = reconcileSnapshot(state, snapshot)
	if err != nil {
		return failOpen(errors.Join(err, wal.Close()))
	}
	if _, err = wal.Seek(0, io.SeekEnd); err != nil {
		return failOpen(errors.Join(fmt.Errorf("seek write-ahead log: %w", err), wal.Close()))
	}
	return &Store{directory: directory, lock: lock, wal: wal, state: state, snapshot: snapshot}, nil
}

// State returns an isolated copy of the recovered durable state.
func (s *Store) State() (raft.PersistentState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.healthError(); err != nil {
		return raft.PersistentState{}, err
	}
	return raft.PersistentState{
		HardState: s.state.hardState,
		Entries:   cloneEntries(s.state.entries),
		Snapshot:  cloneSnapshot(s.snapshot),
	}, nil
}

// Persist atomically appends one metadata/log mutation to the WAL.
func (s *Store) Persist(batch raft.PersistBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthError(); err != nil {
		return err
	}
	payload, err := encodeBatch(batch)
	if err != nil {
		return err
	}
	prepared, err := prepareBatch(s.state, batch)
	if err != nil {
		return err
	}
	record, err := encodeRecord(recordBatch, payload)
	if err != nil {
		return err
	}
	err = s.writeWALRecord(record)
	if err != nil {
		return s.poison(fmt.Errorf("append write-ahead log: %w", err))
	}
	if err = injectFailure(s.inject, failureWALSync); err == nil {
		err = s.wal.Sync()
	}
	if err != nil {
		return s.poison(fmt.Errorf("sync write-ahead log: %w", err))
	}
	s.state = applyPreparedBatch(s.state, prepared)
	return nil
}

// SaveSnapshot durably activates a snapshot, then compacts the WAL to a
// checkpoint. If compaction fails after activation, the store fails closed;
// reopening safely reconciles the new snapshot with the old WAL.
func (s *Store) SaveSnapshot(snapshot raft.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthError(); err != nil {
		return err
	}
	if err := validateSnapshot(s.state, snapshot); err != nil {
		return err
	}
	snapshot = *cloneSnapshot(&snapshot)
	if err := writeSnapshot(filepath.Join(s.directory, snapshotFilename), snapshot, s.inject); err != nil {
		return s.poison(err)
	}

	next := s.state
	next.baseIndex = snapshot.Index
	next.baseTerm = snapshot.Term
	next.entries = retainedEntriesAfter(next.entries, snapshot.Index)
	if err := s.rewriteWAL(next); err != nil {
		return s.poison(err)
	}
	s.state = next
	s.snapshot = cloneSnapshot(&snapshot)
	return nil
}

// Close closes the underlying WAL. It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErr error
	if s.wal != nil {
		closeErr = errors.Join(closeErr, s.wal.Close())
		s.wal = nil
	}
	closeErr = errors.Join(closeErr, releaseStoreLock(s.lock))
	s.lock = nil
	return closeErr
}

func (s *Store) healthError() error {
	if s.closed {
		return ErrClosed
	}
	if s.unhealthy != nil {
		return fmt.Errorf("%w: %w", ErrUnhealthy, s.unhealthy)
	}
	return nil
}

func (s *Store) poison(err error) error {
	s.unhealthy = err
	return fmt.Errorf("%w: %w", ErrUnhealthy, err)
}

func (s *Store) rewriteWAL(state recoveredState) error {
	checkpointHardState := state.hardState
	checkpointHardState.CommitIndex = state.baseIndex
	payload, err := encodeCheckpoint(checkpoint{
		baseIndex: state.baseIndex,
		baseTerm:  state.baseTerm,
		hardState: checkpointHardState,
	})
	if err != nil {
		return err
	}
	record, err := encodeRecord(recordCheckpoint, payload)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.directory, ".raft-wal-*")
	if err != nil {
		return fmt.Errorf("create compacted write-ahead log: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err = temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure compacted write-ahead log: %w", err)
	}
	if err = injectFailure(s.inject, failureWALCompactWrite); err == nil {
		err = writeAll(temporary, record)
	}
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write compacted write-ahead log: %w", err)
	}
	if err = writeEntryRecords(temporary, state.entries); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write retained log entries: %w", err)
	}
	if checkpointHardState != state.hardState {
		if err = writeBatchRecord(temporary, raft.PersistBatch{HardState: &state.hardState}); err != nil {
			_ = temporary.Close()
			return fmt.Errorf("write compacted hard state: %w", err)
		}
	}
	if err = injectFailure(s.inject, failureWALCompactSync); err == nil {
		err = temporary.Sync()
	}
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync compacted write-ahead log: %w", err)
	}
	if err = temporary.Close(); err != nil {
		return fmt.Errorf("close compacted write-ahead log: %w", err)
	}
	if err = s.wal.Close(); err != nil {
		return fmt.Errorf("close active write-ahead log: %w", err)
	}
	s.wal = nil
	walPath := filepath.Join(s.directory, walFilename)
	if err = injectFailure(s.inject, failureWALRename); err == nil {
		err = os.Rename(temporaryName, walPath)
	}
	if err != nil {
		return fmt.Errorf("activate compacted write-ahead log: %w", err)
	}
	if err = syncDirectory(s.directory, s.inject, failureWALDirectorySync); err != nil {
		return fmt.Errorf("sync compacted write-ahead log directory: %w", err)
	}
	if err = injectFailure(s.inject, failureWALReopen); err == nil {
		s.wal, err = os.OpenFile(walPath, os.O_RDWR, 0o600) // #nosec G304 -- walPath appends a fixed filename to the validated store root.
	}
	if err != nil {
		return fmt.Errorf("reopen compacted write-ahead log: %w", err)
	}
	if _, err = s.wal.Seek(0, io.SeekEnd); err != nil {
		_ = s.wal.Close()
		s.wal = nil
		return fmt.Errorf("seek compacted write-ahead log: %w", err)
	}
	return nil
}

func replayWAL(file *os.File) (recoveredState, error) {
	info, err := file.Stat()
	if err != nil {
		return recoveredState{}, fmt.Errorf("stat write-ahead log: %w", err)
	}
	var state recoveredState
	var offset int64
	for offset < info.Size() {
		remaining := info.Size() - offset
		if remaining < recordHeaderSize {
			return recoveredState{}, corrupt(offset, "incomplete record header")
		}
		header := make([]byte, recordHeaderSize)
		if _, err = file.ReadAt(header, offset); err != nil {
			return recoveredState{}, fmt.Errorf("read write-ahead log header: %w", err)
		}
		kind, payloadLength, expectedChecksum, err := decodeRecordHeader(header)
		if err != nil {
			return recoveredState{}, corrupt(offset, err.Error())
		}
		recordEnd := offset + recordHeaderSize + int64(payloadLength)
		if recordEnd > info.Size() {
			if truncateErr := truncateTail(file, offset); truncateErr != nil {
				return recoveredState{}, truncateErr
			}
			break
		}
		payload := make([]byte, payloadLength)
		if _, err = file.ReadAt(payload, offset+recordHeaderSize); err != nil {
			return recoveredState{}, fmt.Errorf("read write-ahead log payload: %w", err)
		}
		if actual := recordChecksum(header[4:12], payload); actual != expectedChecksum {
			return recoveredState{}, corrupt(offset, "record checksum mismatch")
		}
		switch kind {
		case recordCheckpoint:
			if offset != 0 {
				return recoveredState{}, corrupt(offset, "checkpoint is not the first record")
			}
			value, decodeErr := decodeCheckpoint(payload)
			if decodeErr != nil {
				return recoveredState{}, corrupt(offset, "invalid checkpoint payload")
			}
			state = recoveredState(value)
			if validateErr := validateRecoveredState(state); validateErr != nil {
				return recoveredState{}, corrupt(offset, "checkpoint violates storage invariants")
			}
		case recordBatch:
			batch, decodeErr := decodeBatch(payload)
			if decodeErr != nil {
				return recoveredState{}, corrupt(offset, "invalid batch payload")
			}
			state, decodeErr = applyBatch(state, batch)
			if decodeErr != nil {
				return recoveredState{}, corrupt(offset, "batch violates storage invariants")
			}
		default:
			return recoveredState{}, corrupt(offset, "unknown record kind")
		}
		offset = recordEnd
	}
	return state, nil
}

func decodeRecordHeader(header []byte) (recordKind, uint32, uint32, error) {
	if len(header) != recordHeaderSize || !bytes.Equal(header[:4], walMagic[:]) {
		return 0, 0, 0, errors.New("invalid record magic")
	}
	if version := readBigEndianUint16(header[4:6]); version != wireVersion {
		return 0, 0, 0, errors.New("unsupported record version")
	}
	kind := recordKind(readBigEndianUint16(header[6:8]))
	if kind != recordBatch && kind != recordCheckpoint {
		return 0, 0, 0, errors.New("unknown record kind")
	}
	length := readBigEndianUint32(header[8:12])
	if expected := readBigEndianUint32(header[12:16]); expected != crc32Checksum(header[:12]) {
		return 0, 0, 0, errors.New("record header checksum mismatch")
	}
	if length > maxRecordBytes {
		return 0, 0, 0, errors.New("record exceeds maximum size")
	}
	return kind, length, readBigEndianUint32(header[16:20]), nil
}

func applyBatch(current recoveredState, batch raft.PersistBatch) (recoveredState, error) {
	prepared, err := prepareBatch(current, batch)
	if err != nil {
		return recoveredState{}, err
	}
	return applyPreparedBatch(current, prepared), nil
}

func prepareBatch(current recoveredState, batch raft.PersistBatch) (preparedBatch, error) {
	if batch.HardState == nil && batch.TruncateFrom == nil && len(batch.Entries) == 0 {
		return preparedBatch{}, invalid("empty persistence batch")
	}
	prepared := preparedBatch{
		hardState:        current.hardState,
		truncatePosition: len(current.entries),
	}
	if batch.TruncateFrom != nil {
		from := *batch.TruncateFrom
		if from == 0 || from <= current.baseIndex || from <= current.hardState.CommitIndex {
			return preparedBatch{}, invalid("cannot truncate compacted or committed entries")
		}
		tail := lastIndex(current)
		if from > tail && (tail == ^raft.Index(0) || from != tail+1) {
			return preparedBatch{}, invalid("truncation starts beyond the log tail")
		}
		if from <= tail {
			prepared.truncatePosition = sort.Search(len(current.entries), func(position int) bool {
				return current.entries[position].Index >= from
			})
		}
		prepared.hasTruncate = true
	}
	if batch.HardState != nil {
		if err := validateHardStateTransition(current.hardState, *batch.HardState); err != nil {
			return preparedBatch{}, err
		}
		prepared.hardState = *batch.HardState
	}
	tailIndex := current.baseIndex
	tailTerm := current.baseTerm
	if prepared.truncatePosition > 0 {
		tail := current.entries[prepared.truncatePosition-1]
		tailIndex = tail.Index
		tailTerm = tail.Term
	}
	prepared.entries = make([]raft.Entry, 0, len(batch.Entries))
	for _, entry := range batch.Entries {
		if err := validateNextEntry(tailIndex, tailTerm, prepared.hardState.CurrentTerm, entry); err != nil {
			return preparedBatch{}, err
		}
		prepared.entries = append(prepared.entries, cloneEntry(entry))
		tailIndex = entry.Index
		tailTerm = entry.Term
	}
	if prepared.hardState.CommitIndex < current.baseIndex {
		return preparedBatch{}, invalid("commit index precedes checkpoint")
	}
	if prepared.hardState.CommitIndex > tailIndex {
		return preparedBatch{}, invalid("commit index exceeds the durable log")
	}
	return prepared, nil
}

func applyPreparedBatch(current recoveredState, prepared preparedBatch) recoveredState {
	current.hardState = prepared.hardState
	if prepared.hasTruncate {
		clear(current.entries[prepared.truncatePosition:])
		current.entries = current.entries[:prepared.truncatePosition]
	}
	current.entries = append(current.entries, prepared.entries...)
	return current
}

func validateHardStateTransition(current, next raft.HardState) error {
	if next.CurrentTerm < current.CurrentTerm {
		return invalid("current term cannot decrease")
	}
	if next.CommitIndex < current.CommitIndex {
		return invalid("commit index cannot decrease")
	}
	if next.CurrentTerm == current.CurrentTerm && current.VotedFor != "" && next.VotedFor != current.VotedFor {
		return invalid("vote cannot change within a term")
	}
	if next.CurrentTerm == 0 && next.VotedFor != "" {
		return invalid("term zero cannot contain a vote")
	}
	if next.VotedFor != "" {
		if err := contracts.NodeID(next.VotedFor).Validate(); err != nil {
			return invalid("voted-for identifier is invalid")
		}
	}
	return nil
}

func validateNextEntry(tailIndex raft.Index, tailTerm, currentTerm raft.Term, entry raft.Entry) error {
	if entry.Index == 0 || entry.Term == 0 {
		return invalid("log entry index and term must be non-zero")
	}
	if tailIndex == ^raft.Index(0) || entry.Index != tailIndex+1 {
		return invalid("log entries must be contiguous")
	}
	if entry.Term < tailTerm {
		return invalid("log entry term cannot decrease")
	}
	if entry.Term > currentTerm {
		return invalid("log entry term exceeds current term")
	}
	if len(entry.Command) > contracts.MaxCommandBytes {
		return invalid("log entry command exceeds maximum size")
	}
	return nil
}

func validateRecoveredState(state recoveredState) error {
	if state.baseIndex == 0 && state.baseTerm != 0 || state.baseIndex != 0 && state.baseTerm == 0 {
		return invalid("checkpoint index and term must both be zero or non-zero")
	}
	if state.hardState.CommitIndex < state.baseIndex {
		return invalid("commit index precedes checkpoint")
	}
	if state.hardState.CommitIndex > lastIndex(state) {
		return invalid("commit index exceeds the durable log")
	}
	if state.baseTerm > state.hardState.CurrentTerm {
		return invalid("checkpoint term exceeds current term")
	}
	if state.hardState.VotedFor != "" {
		if state.hardState.CurrentTerm == 0 {
			return invalid("term zero cannot contain a vote")
		}
		if err := contracts.NodeID(state.hardState.VotedFor).Validate(); err != nil {
			return invalid("voted-for identifier is invalid")
		}
	}
	if state.baseIndex == ^raft.Index(0) && len(state.entries) != 0 {
		return invalid("durable log index overflows")
	}
	expected := state.baseIndex + 1
	previousTerm := state.baseTerm
	for position, entry := range state.entries {
		if entry.Index != expected || entry.Term == 0 || entry.Term < previousTerm || entry.Term > state.hardState.CurrentTerm {
			return invalid("durable log violates index or term invariants")
		}
		if len(entry.Command) > contracts.MaxCommandBytes {
			return invalid("durable command exceeds maximum size")
		}
		previousTerm = entry.Term
		if expected == ^raft.Index(0) {
			if position != len(state.entries)-1 {
				return invalid("durable log index overflows")
			}
			continue
		}
		expected++
	}
	return nil
}

func validateSnapshot(state recoveredState, snapshot raft.Snapshot) error {
	if snapshot.Index == 0 || snapshot.Term == 0 {
		return invalid("snapshot index and term must be non-zero")
	}
	if len(snapshot.Data) > maxSnapshotBytes {
		return invalid("snapshot exceeds maximum size")
	}
	if snapshot.Index < state.baseIndex || snapshot.Index > state.hardState.CommitIndex {
		return invalid("snapshot must cover a committed index")
	}
	term, found := termAt(state, snapshot.Index)
	if !found || term != snapshot.Term {
		return invalid("snapshot term does not match the durable log")
	}
	return nil
}

func reconcileSnapshot(state recoveredState, snapshot *raft.Snapshot) (recoveredState, error) {
	if snapshot == nil {
		if state.baseIndex != 0 {
			return recoveredState{}, corrupt(0, "checkpoint snapshot is missing")
		}
		return state, nil
	}
	if state.baseIndex != 0 && (state.baseIndex != snapshot.Index || state.baseTerm != snapshot.Term) {
		return recoveredState{}, corrupt(0, "checkpoint does not match snapshot")
	}
	if state.hardState.CommitIndex < snapshot.Index || state.hardState.CurrentTerm < snapshot.Term {
		return recoveredState{}, corrupt(0, "snapshot exceeds durable hard state")
	}
	term, found := termAt(state, snapshot.Index)
	if !found || term != snapshot.Term {
		return recoveredState{}, corrupt(0, "snapshot does not match durable log")
	}
	state.baseIndex = snapshot.Index
	state.baseTerm = snapshot.Term
	state.entries = retainedEntriesAfter(state.entries, snapshot.Index)
	if err := validateRecoveredState(state); err != nil {
		return recoveredState{}, corrupt(0, "snapshot reconciliation violates storage invariants")
	}
	return state, nil
}

func termAt(state recoveredState, index raft.Index) (raft.Term, bool) {
	if index == state.baseIndex {
		return state.baseTerm, index != 0
	}
	if index <= state.baseIndex {
		return 0, false
	}
	if index > lastIndex(state) {
		return 0, false
	}
	offset := index - state.baseIndex - 1
	return state.entries[offset].Term, true
}

func lastIndex(state recoveredState) raft.Index {
	if len(state.entries) == 0 {
		return state.baseIndex
	}
	return state.entries[len(state.entries)-1].Index
}

func retainedEntriesAfter(entries []raft.Entry, index raft.Index) []raft.Entry {
	for position, entry := range entries {
		if entry.Index > index {
			return append([]raft.Entry(nil), entries[position:]...)
		}
	}
	return nil
}

func cloneEntries(entries []raft.Entry) []raft.Entry {
	if entries == nil {
		return nil
	}
	cloned := make([]raft.Entry, len(entries))
	for index, entry := range entries {
		cloned[index] = cloneEntry(entry)
	}
	return cloned
}

func cloneEntry(entry raft.Entry) raft.Entry {
	entry.Command = bytes.Clone(entry.Command)
	return entry
}

func cloneSnapshot(snapshot *raft.Snapshot) *raft.Snapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	cloned.Data = bytes.Clone(snapshot.Data)
	return &cloned
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (s *Store) writeWALRecord(record []byte) error {
	if err := injectFailure(s.inject, failureWALWrite); err != nil {
		return err
	}
	if s.writeWAL != nil {
		return s.writeWAL(s.wal, record)
	}
	return writeAll(s.wal, record)
}

func writeEntryRecords(writer io.Writer, entries []raft.Entry) error {
	const emptyBatchSize = 8
	for len(entries) > 0 {
		encodedSize := emptyBatchSize
		count := 0
		for count < len(entries) {
			entrySize := 20 + len(entries[count].Command)
			if count > 0 && encodedSize+entrySize > maxRecordBytes {
				break
			}
			encodedSize += entrySize
			count++
		}
		if err := writeBatchRecord(writer, raft.PersistBatch{Entries: entries[:count]}); err != nil {
			return err
		}
		entries = entries[count:]
	}
	return nil
}

func writeBatchRecord(writer io.Writer, batch raft.PersistBatch) error {
	payload, err := encodeBatch(batch)
	if err != nil {
		return err
	}
	record, err := encodeRecord(recordBatch, payload)
	if err != nil {
		return err
	}
	return writeAll(writer, record)
}

func truncateTail(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("truncate incomplete write-ahead log tail: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync truncated write-ahead log: %w", err)
	}
	return nil
}

func syncDirectory(directory string, inject failureInjector, point failurePoint) error {
	if err := injectFailure(inject, point); err != nil {
		return err
	}
	file, err := os.Open(directory) // #nosec G304 -- directory is the validated store root itself.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}

func corrupt(offset int64, reason string) error {
	return &CorruptionError{Offset: offset, Reason: reason}
}

func readBigEndianUint16(data []byte) uint16 {
	return uint16(data[0])<<8 | uint16(data[1])
}

func readBigEndianUint32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

func crc32Checksum(data []byte) uint32 {
	return crc32.Checksum(data, checksumTable)
}
