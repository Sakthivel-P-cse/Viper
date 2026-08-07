package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"viper/internal/raft"
)

const snapshotHeaderSize = 32

func writeSnapshot(path string, snapshot raft.Snapshot, inject failureInjector) error {
	encoded, err := encodeSnapshot(snapshot)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".snapshot-*")
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err = temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure snapshot: %w", err)
	}
	if err = injectFailure(inject, failureSnapshotWrite); err == nil {
		err = writeAll(temporary, encoded)
	}
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err = injectFailure(inject, failureSnapshotSync); err == nil {
		err = temporary.Sync()
	}
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if err = temporary.Close(); err != nil {
		return fmt.Errorf("close snapshot: %w", err)
	}
	if err = injectFailure(inject, failureSnapshotRename); err == nil {
		err = os.Rename(temporaryName, path)
	}
	if err != nil {
		return fmt.Errorf("activate snapshot: %w", err)
	}
	if err = syncDirectory(directory, inject, failureSnapshotDirectorySync); err != nil {
		return fmt.Errorf("sync snapshot directory: %w", err)
	}
	return nil
}

func encodeSnapshot(snapshot raft.Snapshot) ([]byte, error) {
	if len(snapshot.Data) > maxSnapshotBytes {
		return nil, invalid("snapshot exceeds maximum size")
	}
	encoded := make([]byte, snapshotHeaderSize+len(snapshot.Data))
	copy(encoded[:4], snapshotMagic[:])
	binary.BigEndian.PutUint16(encoded[4:6], wireVersion)
	binary.BigEndian.PutUint64(encoded[8:16], uint64(snapshot.Index))
	binary.BigEndian.PutUint64(encoded[16:24], uint64(snapshot.Term))
	binary.BigEndian.PutUint32(encoded[24:28], uint32(len(snapshot.Data))) // #nosec G115 -- snapshot is bounded above.
	copy(encoded[snapshotHeaderSize:], snapshot.Data)
	checksum := crc32.New(checksumTable)
	_, _ = checksum.Write(encoded[4:28])
	_, _ = checksum.Write(snapshot.Data)
	binary.BigEndian.PutUint32(encoded[28:32], checksum.Sum32())
	return encoded, nil
}

func loadSnapshot(path string) (*raft.Snapshot, error) {
	pathInfo, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil, nil
	}
	if statErr != nil {
		return nil, fmt.Errorf("stat snapshot path: %w", statErr)
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, corrupt(0, "snapshot is not a regular file")
	}
	file, err := os.Open(path) // #nosec G304 -- path is a fixed filename under the validated store root.
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat snapshot: %w", err)
	}
	if info.Size() < snapshotHeaderSize || info.Size() > snapshotHeaderSize+maxSnapshotBytes {
		return nil, corrupt(0, "invalid snapshot size")
	}
	header := make([]byte, snapshotHeaderSize)
	if _, err = io.ReadFull(file, header); err != nil {
		return nil, fmt.Errorf("read snapshot header: %w", err)
	}
	if !bytes.Equal(header[:4], snapshotMagic[:]) {
		return nil, corrupt(0, "invalid snapshot magic")
	}
	if binary.BigEndian.Uint16(header[4:6]) != wireVersion {
		return nil, corrupt(0, "unsupported snapshot version")
	}
	if header[6] != 0 || header[7] != 0 {
		return nil, corrupt(0, "invalid snapshot reserved bytes")
	}
	length := binary.BigEndian.Uint32(header[24:28])
	if length > maxSnapshotBytes || int64(snapshotHeaderSize)+int64(length) != info.Size() {
		return nil, corrupt(0, "snapshot length mismatch")
	}
	data := make([]byte, length)
	if _, err = io.ReadFull(file, data); err != nil {
		return nil, fmt.Errorf("read snapshot data: %w", err)
	}
	checksum := crc32.New(checksumTable)
	_, _ = checksum.Write(header[4:28])
	_, _ = checksum.Write(data)
	if checksum.Sum32() != binary.BigEndian.Uint32(header[28:32]) {
		return nil, corrupt(0, "snapshot checksum mismatch")
	}
	index := raft.Index(binary.BigEndian.Uint64(header[8:16]))
	term := raft.Term(binary.BigEndian.Uint64(header[16:24]))
	if index == 0 || term == 0 {
		return nil, corrupt(0, "invalid snapshot index or term")
	}
	return &raft.Snapshot{Index: index, Term: term, Data: data}, nil
}
