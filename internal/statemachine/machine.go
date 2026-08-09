package statemachine

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sync"

	"viper/pkg/contracts"
)

// MutationStatus is the deterministic outcome recorded for a request ID.
type MutationStatus uint8

const (
	MutationUnspecified MutationStatus = iota
	MutationApplied
	MutationConditionFailed
	MutationRequestConflict
)

// Result is the deterministic result of one applied mutation.
type Result struct {
	Kind            CommandKind
	Status          MutationStatus
	Revision        uint64
	Swapped         bool
	PreviousPresent bool
	Duplicate       bool
	PreviousValue   []byte
}

// ValueResult is an isolated point-in-time read from the machine.
type ValueResult struct {
	Value    []byte
	Found    bool
	Revision uint64
}

type valueRecord struct {
	value    []byte
	revision uint64
}

type requestKey struct {
	client  contracts.ClientID
	request contracts.RequestID
}

type requestRecord struct {
	digest [sha256.Size]byte
	result Result
}

// Machine applies committed KV mutations in exact log-index order.
type Machine struct {
	mu           sync.RWMutex
	values       map[string]valueRecord
	requests     map[requestKey]requestRecord
	appliedIndex uint64
	revision     uint64
}

// New returns an empty deterministic KV state machine.
func New() *Machine {
	return &Machine{
		values:   make(map[string]valueRecord),
		requests: make(map[requestKey]requestRecord),
	}
}

// Apply executes a committed command at exactly the next log index.
func (m *Machine) Apply(index uint64, encoded []byte) (Result, error) {
	if m == nil {
		return Result{}, fmt.Errorf("%w: machine is nil", ErrOutOfOrder)
	}
	command, err := DecodeCommand(encoded)
	if err != nil {
		return Result{}, err
	}
	digest := sha256.Sum256(encoded)

	m.mu.Lock()
	defer m.mu.Unlock()
	if index == 0 || index != m.appliedIndex+1 {
		return Result{}, fmt.Errorf("%w: got %d after %d", ErrOutOfOrder, index, m.appliedIndex)
	}
	key := requestKey{client: command.ClientID, request: command.RequestID}
	if previous, exists := m.requests[key]; exists {
		m.appliedIndex = index
		if previous.digest != digest {
			return Result{
				Kind:     command.Kind,
				Status:   MutationRequestConflict,
				Revision: m.revision,
			}, nil
		}
		result := cloneResult(previous.result)
		result.Duplicate = true
		return result, nil
	}

	current, present := m.values[string(command.Key)]
	result := Result{
		Kind:            command.Kind,
		PreviousPresent: present,
		PreviousValue:   append([]byte(nil), current.value...),
	}
	switch command.Kind {
	case CommandPut:
		m.revision++
		m.values[string(command.Key)] = valueRecord{value: append([]byte(nil), command.Value...), revision: m.revision}
		result.Status = MutationApplied
		result.Revision = m.revision
	case CommandCompareAndSwap:
		matches := present == command.ExpectedPresent
		if matches && present {
			matches = bytes.Equal(current.value, command.ExpectedValue)
		}
		if matches {
			m.revision++
			m.values[string(command.Key)] = valueRecord{value: append([]byte(nil), command.Value...), revision: m.revision}
			result.Status = MutationApplied
			result.Swapped = true
			result.Revision = m.revision
		} else {
			result.Status = MutationConditionFailed
			result.Revision = m.revision
		}
	}
	m.requests[key] = requestRecord{digest: digest, result: cloneResult(result)}
	m.appliedIndex = index
	return cloneResult(result), nil
}

// Get returns a copy of the current value for key.
func (m *Machine) Get(key []byte) (ValueResult, error) {
	if err := contracts.ValidateKey(key); err != nil {
		return ValueResult{}, err
	}
	if m == nil {
		return ValueResult{}, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, found := m.values[string(key)]
	return ValueResult{Value: append([]byte(nil), record.value...), Found: found, Revision: record.revision}, nil
}

// Resolve returns a previously recorded request outcome, if present.
func (m *Machine) Resolve(clientID contracts.ClientID, requestID contracts.RequestID) (Result, bool, error) {
	if err := clientID.Validate(); err != nil {
		return Result{}, false, err
	}
	if err := requestID.Validate(); err != nil {
		return Result{}, false, err
	}
	if m == nil {
		return Result{}, false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, found := m.requests[requestKey{client: clientID, request: requestID}]
	return cloneResult(record.result), found, nil
}

// AppliedIndex returns the latest committed index incorporated by the machine.
func (m *Machine) AppliedIndex() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.appliedIndex
}

// Revision returns the latest successful mutation revision.
func (m *Machine) Revision() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revision
}

// Len returns the number of current keys.
func (m *Machine) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.values)
}

// DedupCount returns the number of retained retry records.
func (m *Machine) DedupCount() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.requests)
}

func cloneResult(result Result) Result {
	result.PreviousValue = append([]byte(nil), result.PreviousValue...)
	return result
}
