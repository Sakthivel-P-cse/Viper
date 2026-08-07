package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"viper/internal/raft"
)

const (
	wireVersion      = uint16(1)
	recordHeaderSize = 20
	maxRecordBytes   = 16 * 1024 * 1024
	maxSnapshotBytes = 64 * 1024 * 1024
)

var (
	walMagic      = [4]byte{'V', 'P', 'W', '1'}
	snapshotMagic = [4]byte{'V', 'P', 'S', '1'}
	checksumTable = crc32.MakeTable(crc32.Castagnoli)
)

type recordKind uint16

const (
	recordBatch recordKind = iota + 1
	recordCheckpoint
)

const (
	batchHasHardState uint8 = 1 << iota
	batchHasTruncate
)

type checkpoint struct {
	entries   []raft.Entry
	hardState raft.HardState
	baseIndex raft.Index
	baseTerm  raft.Term
}

func encodeBatch(batch raft.PersistBatch) ([]byte, error) {
	encodedSize, err := batchEncodedSize(batch)
	if err != nil {
		return nil, err
	}
	buffer := bytes.NewBuffer(make([]byte, 0, encodedSize))
	var flags uint8
	if batch.HardState != nil {
		flags |= batchHasHardState
	}
	if batch.TruncateFrom != nil {
		flags |= batchHasTruncate
	}
	buffer.WriteByte(flags)
	buffer.Write([]byte{0, 0, 0})
	if batch.HardState != nil {
		if err := encodeHardState(buffer, *batch.HardState); err != nil {
			return nil, err
		}
	}
	if batch.TruncateFrom != nil {
		writeUint64(buffer, uint64(*batch.TruncateFrom))
	}
	writeUint32(buffer, uint32(len(batch.Entries))) // #nosec G115 -- entry count is bounded above.
	for _, entry := range batch.Entries {
		if err := encodeEntry(buffer, entry); err != nil {
			return nil, err
		}
	}
	return buffer.Bytes(), nil
}

func decodeBatch(payload []byte) (raft.PersistBatch, error) {
	reader := bytes.NewReader(payload)
	flags, err := reader.ReadByte()
	if err != nil {
		return raft.PersistBatch{}, err
	}
	reserved := make([]byte, 3)
	if _, readErr := io.ReadFull(reader, reserved); readErr != nil {
		return raft.PersistBatch{}, readErr
	}
	if reserved[0] != 0 || reserved[1] != 0 || reserved[2] != 0 {
		return raft.PersistBatch{}, fmt.Errorf("invalid reserved batch bytes")
	}
	if flags & ^(batchHasHardState|batchHasTruncate) != 0 {
		return raft.PersistBatch{}, fmt.Errorf("unknown batch flags")
	}
	batch := raft.PersistBatch{}
	if flags&batchHasHardState != 0 {
		hardState, decodeErr := decodeHardState(reader)
		if decodeErr != nil {
			return raft.PersistBatch{}, decodeErr
		}
		batch.HardState = &hardState
	}
	if flags&batchHasTruncate != 0 {
		value, decodeErr := readUint64(reader)
		if decodeErr != nil {
			return raft.PersistBatch{}, decodeErr
		}
		index := raft.Index(value)
		batch.TruncateFrom = &index
	}
	entries, err := decodeEntries(reader)
	if err != nil {
		return raft.PersistBatch{}, err
	}
	batch.Entries = entries
	if reader.Len() != 0 {
		return raft.PersistBatch{}, fmt.Errorf("trailing batch bytes")
	}
	return batch, nil
}

func encodeCheckpoint(value checkpoint) ([]byte, error) {
	encodedSize, err := checkpointEncodedSize(value)
	if err != nil {
		return nil, err
	}
	buffer := bytes.NewBuffer(make([]byte, 0, encodedSize))
	writeUint64(buffer, uint64(value.baseIndex))
	writeUint64(buffer, uint64(value.baseTerm))
	if err := encodeHardState(buffer, value.hardState); err != nil {
		return nil, err
	}
	writeUint32(buffer, uint32(len(value.entries))) // #nosec G115 -- entry count is bounded above.
	for _, entry := range value.entries {
		if err := encodeEntry(buffer, entry); err != nil {
			return nil, err
		}
	}
	return buffer.Bytes(), nil
}

func batchEncodedSize(batch raft.PersistBatch) (int, error) {
	size := 8 // flags, reserved bytes, and entry count.
	if batch.HardState != nil {
		var err error
		size, err = addHardStateSize(size, *batch.HardState)
		if err != nil {
			return 0, err
		}
	}
	if batch.TruncateFrom != nil {
		size += 8
	}
	return addEntriesSize(size, batch.Entries, "persistence batch")
}

func checkpointEncodedSize(value checkpoint) (int, error) {
	size, err := addHardStateSize(20, value.hardState) // base index, term, and entry count.
	if err != nil {
		return 0, err
	}
	return addEntriesSize(size, value.entries, "checkpoint")
}

func addHardStateSize(size int, state raft.HardState) (int, error) {
	votedForLength := len(state.VotedFor)
	if votedForLength > 65535 {
		return 0, invalid("voted-for identifier is too long")
	}
	return addEncodedSize(size, 18+votedForLength, "hard state")
}

func addEntriesSize(size int, entries []raft.Entry, context string) (int, error) {
	if len(entries) > maxRecordBytes/20 {
		return 0, invalid("too many log entries")
	}
	var err error
	for _, entry := range entries {
		if len(entry.Command) > maxRecordBytes {
			return 0, invalid("entry command is too large")
		}
		size, err = addEncodedSize(size, 20+len(entry.Command), context)
		if err != nil {
			return 0, err
		}
	}
	return size, nil
}

func addEncodedSize(current, addition int, context string) (int, error) {
	if current > maxRecordBytes || addition > maxRecordBytes-current {
		return 0, invalid(context + " exceeds maximum encoded size")
	}
	return current + addition, nil
}

func decodeCheckpoint(payload []byte) (checkpoint, error) {
	reader := bytes.NewReader(payload)
	baseIndex, err := readUint64(reader)
	if err != nil {
		return checkpoint{}, err
	}
	baseTerm, err := readUint64(reader)
	if err != nil {
		return checkpoint{}, err
	}
	hardState, err := decodeHardState(reader)
	if err != nil {
		return checkpoint{}, err
	}
	entries, err := decodeEntries(reader)
	if err != nil {
		return checkpoint{}, err
	}
	if reader.Len() != 0 {
		return checkpoint{}, fmt.Errorf("trailing checkpoint bytes")
	}
	return checkpoint{
		baseIndex: raft.Index(baseIndex),
		baseTerm:  raft.Term(baseTerm),
		hardState: hardState,
		entries:   entries,
	}, nil
}

func encodeHardState(writer *bytes.Buffer, state raft.HardState) error {
	writeUint64(writer, uint64(state.CurrentTerm))
	writeUint64(writer, uint64(state.CommitIndex))
	votedFor := []byte(state.VotedFor)
	if len(votedFor) > 65535 {
		return invalid("voted-for identifier is too long")
	}
	writeUint16(writer, uint16(len(votedFor))) // #nosec G115 -- votedFor is bounded to uint16.
	_, err := writer.Write(votedFor)
	return err
}

func decodeHardState(reader io.Reader) (raft.HardState, error) {
	currentTerm, err := readUint64(reader)
	if err != nil {
		return raft.HardState{}, err
	}
	commitIndex, err := readUint64(reader)
	if err != nil {
		return raft.HardState{}, err
	}
	votedForLength, err := readUint16(reader)
	if err != nil {
		return raft.HardState{}, err
	}
	votedFor := make([]byte, votedForLength)
	if _, err := io.ReadFull(reader, votedFor); err != nil {
		return raft.HardState{}, err
	}
	return raft.HardState{
		CurrentTerm: raft.Term(currentTerm),
		VotedFor:    raft.NodeID(votedFor),
		CommitIndex: raft.Index(commitIndex),
	}, nil
}

func encodeEntry(writer *bytes.Buffer, entry raft.Entry) error {
	writeUint64(writer, uint64(entry.Index))
	writeUint64(writer, uint64(entry.Term))
	if len(entry.Command) > maxRecordBytes {
		return invalid("entry command is too large")
	}
	writeUint32(writer, uint32(len(entry.Command))) // #nosec G115 -- command length is bounded above.
	_, err := writer.Write(entry.Command)
	return err
}

func decodeEntries(reader *bytes.Reader) ([]raft.Entry, error) {
	count, err := readUint32(reader)
	if err != nil {
		return nil, err
	}
	if count > maxRecordBytes/20 {
		return nil, fmt.Errorf("entry count exceeds payload")
	}
	entries := make([]raft.Entry, 0, count)
	for range count {
		index, decodeErr := readUint64(reader)
		if decodeErr != nil {
			return nil, decodeErr
		}
		term, decodeErr := readUint64(reader)
		if decodeErr != nil {
			return nil, decodeErr
		}
		commandLength, decodeErr := readUint32(reader)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if commandLength > maxRecordBytes {
			return nil, fmt.Errorf("entry command exceeds maximum size")
		}
		commandSize := int(commandLength)
		if commandSize > reader.Len() {
			return nil, io.ErrUnexpectedEOF
		}
		command := make([]byte, commandSize)
		if _, decodeErr := io.ReadFull(reader, command); decodeErr != nil {
			return nil, decodeErr
		}
		entries = append(entries, raft.Entry{Index: raft.Index(index), Term: raft.Term(term), Command: command})
	}
	return entries, nil
}

func encodeRecord(kind recordKind, payload []byte) ([]byte, error) {
	if len(payload) > maxRecordBytes {
		return nil, invalid("record exceeds maximum size")
	}
	record := make([]byte, recordHeaderSize+len(payload))
	copy(record[:4], walMagic[:])
	binary.BigEndian.PutUint16(record[4:6], wireVersion)
	binary.BigEndian.PutUint16(record[6:8], uint16(kind))
	binary.BigEndian.PutUint32(record[8:12], uint32(len(payload))) // #nosec G115 -- payload is bounded above.
	copy(record[recordHeaderSize:], payload)
	binary.BigEndian.PutUint32(record[12:16], crc32.Checksum(record[:12], checksumTable))
	binary.BigEndian.PutUint32(record[16:20], recordChecksum(record[4:12], payload))
	return record, nil
}

func recordChecksum(metadata, payload []byte) uint32 {
	hash := crc32.New(checksumTable)
	_, _ = hash.Write(metadata)
	_, _ = hash.Write(payload)
	return hash.Sum32()
}

func writeUint16(writer *bytes.Buffer, value uint16) {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeUint32(writer *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeUint64(writer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func readUint16(reader io.Reader) (uint16, error) {
	var encoded [2]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(encoded[:]), nil
}

func readUint32(reader io.Reader) (uint32, error) {
	var encoded [4]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(encoded[:]), nil
}

func readUint64(reader io.Reader) (uint64, error) {
	var encoded [8]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(encoded[:]), nil
}
