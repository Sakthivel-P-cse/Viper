package statemachine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"

	"viper/pkg/contracts"
)

const (
	snapshotHeaderBytes   = 32
	snapshotValueHeader   = 16
	snapshotRequestHeader = 52
	snapshotChecksumBytes = 4
	snapshotVersion       = 1
	maxSnapshotBytes      = 64 * 1024 * 1024
	maxSnapshotRecords    = 1 << 20
	snapshotSwappedBit    = 1 << 0
	snapshotPreviousBit   = 1 << 1
)

var (
	snapshotMagic = [4]byte{'V', 'K', 'S', '1'}
	snapshotCRC   = crc32.MakeTable(crc32.Castagnoli)
)

type snapshotValue struct {
	key    string
	record valueRecord
}

type snapshotRequest struct {
	key    requestKey
	record requestRecord
}

// Snapshot encodes all KV data and retry records in a stable sorted format
// protected by a trailing CRC32C checksum.
func (m *Machine) Snapshot() ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: machine is nil", ErrInvalidSnapshot)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.values) > maxSnapshotRecords || len(m.requests) > maxSnapshotRecords {
		return nil, ErrSnapshotTooLarge
	}

	values := make([]snapshotValue, 0, len(m.values))
	for key, record := range m.values {
		values = append(values, snapshotValue{key: key, record: record})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].key < values[j].key })
	requests := make([]snapshotRequest, 0, len(m.requests))
	for key, record := range m.requests {
		requests = append(requests, snapshotRequest{key: key, record: record})
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].key.client == requests[j].key.client {
			return requests[i].key.request < requests[j].key.request
		}
		return requests[i].key.client < requests[j].key.client
	})

	total := uint64(snapshotHeaderBytes + snapshotChecksumBytes)
	for _, item := range values {
		total += snapshotValueHeader + uint64(len(item.key)) + uint64(len(item.record.value))
	}
	for _, item := range requests {
		total += snapshotRequestHeader + uint64(len(item.key.client)) + uint64(len(item.key.request)) + uint64(len(item.record.result.PreviousValue))
	}
	if total > maxSnapshotBytes {
		return nil, ErrSnapshotTooLarge
	}

	buffer := bytes.NewBuffer(make([]byte, 0, int(total)))
	header := make([]byte, snapshotHeaderBytes)
	copy(header[0:4], snapshotMagic[:])
	binary.BigEndian.PutUint16(header[4:6], snapshotVersion)
	binary.BigEndian.PutUint64(header[8:16], m.appliedIndex)
	binary.BigEndian.PutUint64(header[16:24], m.revision)
	binary.BigEndian.PutUint32(header[24:28], uint32(len(values)))
	binary.BigEndian.PutUint32(header[28:32], uint32(len(requests)))
	buffer.Write(header)

	for _, item := range values {
		recordHeader := make([]byte, snapshotValueHeader)
		binary.BigEndian.PutUint32(recordHeader[0:4], uint32(len(item.key)))
		binary.BigEndian.PutUint32(recordHeader[4:8], uint32(len(item.record.value)))
		binary.BigEndian.PutUint64(recordHeader[8:16], item.record.revision)
		buffer.Write(recordHeader)
		buffer.WriteString(item.key)
		buffer.Write(item.record.value)
	}
	for _, item := range requests {
		recordHeader := make([]byte, snapshotRequestHeader)
		binary.BigEndian.PutUint16(recordHeader[0:2], uint16(len(item.key.client)))
		binary.BigEndian.PutUint16(recordHeader[2:4], uint16(len(item.key.request)))
		recordHeader[4] = byte(item.record.result.Kind)
		recordHeader[5] = byte(item.record.result.Status)
		if item.record.result.Swapped {
			recordHeader[6] |= snapshotSwappedBit
		}
		if item.record.result.PreviousPresent {
			recordHeader[6] |= snapshotPreviousBit
		}
		binary.BigEndian.PutUint64(recordHeader[8:16], item.record.result.Revision)
		binary.BigEndian.PutUint32(recordHeader[16:20], uint32(len(item.record.result.PreviousValue)))
		copy(recordHeader[20:52], item.record.digest[:])
		buffer.Write(recordHeader)
		buffer.WriteString(string(item.key.client))
		buffer.WriteString(string(item.key.request))
		buffer.Write(item.record.result.PreviousValue)
	}
	checksum := crc32.Checksum(buffer.Bytes(), snapshotCRC)
	var trailer [snapshotChecksumBytes]byte
	binary.BigEndian.PutUint32(trailer[:], checksum)
	buffer.Write(trailer[:])
	return buffer.Bytes(), nil
}

// Restore atomically replaces state from a validated snapshot. A snapshot
// older than the current applied index is rejected.
func (m *Machine) Restore(data []byte) error {
	if m == nil {
		return fmt.Errorf("%w: machine is nil", ErrInvalidSnapshot)
	}
	state, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if state.appliedIndex < m.appliedIndex {
		return ErrSnapshotRegression
	}
	m.values = state.values
	m.requests = state.requests
	m.appliedIndex = state.appliedIndex
	m.revision = state.revision
	return nil
}

type decodedSnapshot struct {
	values       map[string]valueRecord
	requests     map[requestKey]requestRecord
	appliedIndex uint64
	revision     uint64
}

type snapshotReader struct {
	data     []byte
	position int
}

func decodeSnapshot(data []byte) (decodedSnapshot, error) {
	if len(data) > maxSnapshotBytes {
		return decodedSnapshot{}, ErrSnapshotTooLarge
	}
	if len(data) < snapshotHeaderBytes+snapshotChecksumBytes {
		return decodedSnapshot{}, fmt.Errorf("%w: truncated header", ErrInvalidSnapshot)
	}
	payload := data[:len(data)-snapshotChecksumBytes]
	wantChecksum := binary.BigEndian.Uint32(data[len(payload):])
	if crc32.Checksum(payload, snapshotCRC) != wantChecksum {
		return decodedSnapshot{}, fmt.Errorf("%w: checksum mismatch", ErrInvalidSnapshot)
	}
	if string(payload[0:4]) != string(snapshotMagic[:]) || binary.BigEndian.Uint16(payload[4:6]) != snapshotVersion {
		return decodedSnapshot{}, fmt.Errorf("%w: unsupported format", ErrInvalidSnapshot)
	}
	if binary.BigEndian.Uint16(payload[6:8]) != 0 {
		return decodedSnapshot{}, fmt.Errorf("%w: unsupported flags", ErrInvalidSnapshot)
	}
	state := decodedSnapshot{
		appliedIndex: binary.BigEndian.Uint64(payload[8:16]),
		revision:     binary.BigEndian.Uint64(payload[16:24]),
		values:       make(map[string]valueRecord),
		requests:     make(map[requestKey]requestRecord),
	}
	valueCount := binary.BigEndian.Uint32(payload[24:28])
	requestCount := binary.BigEndian.Uint32(payload[28:32])
	if valueCount > maxSnapshotRecords || requestCount > maxSnapshotRecords {
		return decodedSnapshot{}, ErrSnapshotTooLarge
	}
	if uint64(valueCount) > state.revision || uint64(requestCount) > state.appliedIndex {
		return decodedSnapshot{}, fmt.Errorf("%w: record counts exceed state progress", ErrInvalidSnapshot)
	}
	reader := snapshotReader{data: payload, position: snapshotHeaderBytes}
	var maxValueRevision uint64
	for i := uint32(0); i < valueCount; i++ {
		header, ok := reader.take(snapshotValueHeader)
		if !ok {
			return decodedSnapshot{}, fmt.Errorf("%w: truncated value record", ErrInvalidSnapshot)
		}
		keyLength := binary.BigEndian.Uint32(header[0:4])
		valueLength := binary.BigEndian.Uint32(header[4:8])
		revision := binary.BigEndian.Uint64(header[8:16])
		if keyLength == 0 || keyLength > contracts.MaxKeyBytes || valueLength > contracts.MaxValueBytes || revision == 0 || revision > state.revision {
			return decodedSnapshot{}, fmt.Errorf("%w: invalid value record", ErrInvalidSnapshot)
		}
		key, ok := reader.take(int(keyLength))
		if !ok {
			return decodedSnapshot{}, fmt.Errorf("%w: truncated key", ErrInvalidSnapshot)
		}
		value, ok := reader.take(int(valueLength))
		if !ok {
			return decodedSnapshot{}, fmt.Errorf("%w: truncated value", ErrInvalidSnapshot)
		}
		if _, exists := state.values[string(key)]; exists {
			return decodedSnapshot{}, fmt.Errorf("%w: duplicate key", ErrInvalidSnapshot)
		}
		state.values[string(key)] = valueRecord{value: append([]byte(nil), value...), revision: revision}
		if revision > maxValueRevision {
			maxValueRevision = revision
		}
	}
	for i := uint32(0); i < requestCount; i++ {
		header, ok := reader.take(snapshotRequestHeader)
		if !ok {
			return decodedSnapshot{}, fmt.Errorf("%w: truncated request record", ErrInvalidSnapshot)
		}
		clientLength := binary.BigEndian.Uint16(header[0:2])
		requestLength := binary.BigEndian.Uint16(header[2:4])
		kind := CommandKind(header[4])
		mutationStatus := MutationStatus(header[5])
		flags := header[6]
		resultRevision := binary.BigEndian.Uint64(header[8:16])
		previousLength := binary.BigEndian.Uint32(header[16:20])
		if header[7] != 0 || flags&^byte(snapshotSwappedBit|snapshotPreviousBit) != 0 ||
			clientLength == 0 || clientLength > contracts.MaxClientIDBytes ||
			requestLength == 0 || requestLength > contracts.MaxRequestIDBytes ||
			previousLength > contracts.MaxValueBytes || resultRevision > state.revision ||
			(kind != CommandPut && kind != CommandCompareAndSwap) ||
			(mutationStatus != MutationApplied && mutationStatus != MutationConditionFailed) {
			return decodedSnapshot{}, fmt.Errorf("%w: invalid request record", ErrInvalidSnapshot)
		}
		client, ok := reader.take(int(clientLength))
		if !ok {
			return decodedSnapshot{}, fmt.Errorf("%w: truncated client ID", ErrInvalidSnapshot)
		}
		request, ok := reader.take(int(requestLength))
		if !ok {
			return decodedSnapshot{}, fmt.Errorf("%w: truncated request ID", ErrInvalidSnapshot)
		}
		previous, ok := reader.take(int(previousLength))
		if !ok {
			return decodedSnapshot{}, fmt.Errorf("%w: truncated previous value", ErrInvalidSnapshot)
		}
		key := requestKey{client: contracts.ClientID(client), request: contracts.RequestID(request)}
		if err := key.client.Validate(); err != nil {
			return decodedSnapshot{}, fmt.Errorf("%w: invalid client ID", ErrInvalidSnapshot)
		}
		if err := key.request.Validate(); err != nil {
			return decodedSnapshot{}, fmt.Errorf("%w: invalid request ID", ErrInvalidSnapshot)
		}
		if _, exists := state.requests[key]; exists {
			return decodedSnapshot{}, fmt.Errorf("%w: duplicate request ID", ErrInvalidSnapshot)
		}
		result := Result{
			Kind:            kind,
			Status:          mutationStatus,
			Revision:        resultRevision,
			Swapped:         flags&snapshotSwappedBit != 0,
			PreviousPresent: flags&snapshotPreviousBit != 0,
			PreviousValue:   append([]byte(nil), previous...),
		}
		if (kind == CommandPut && (mutationStatus != MutationApplied || result.Swapped)) ||
			(kind == CommandCompareAndSwap && mutationStatus == MutationApplied && !result.Swapped) ||
			(mutationStatus == MutationConditionFailed && result.Swapped) ||
			(mutationStatus == MutationApplied && result.Revision == 0) ||
			(!result.PreviousPresent && len(result.PreviousValue) != 0) {
			return decodedSnapshot{}, fmt.Errorf("%w: inconsistent request result", ErrInvalidSnapshot)
		}
		var digest [32]byte
		copy(digest[:], header[20:52])
		state.requests[key] = requestRecord{digest: digest, result: result}
	}
	if reader.position != len(payload) {
		return decodedSnapshot{}, fmt.Errorf("%w: trailing payload bytes", ErrInvalidSnapshot)
	}
	if state.revision > state.appliedIndex {
		return decodedSnapshot{}, fmt.Errorf("%w: revision exceeds applied index", ErrInvalidSnapshot)
	}
	if maxValueRevision != state.revision {
		return decodedSnapshot{}, fmt.Errorf("%w: latest revision is absent from the KV image", ErrInvalidSnapshot)
	}
	return state, nil
}

func (r *snapshotReader) take(length int) ([]byte, bool) {
	if length < 0 || r.position > len(r.data)-length {
		return nil, false
	}
	value := r.data[r.position : r.position+length]
	r.position += length
	return value, true
}
