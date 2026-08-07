package contracts

import "fmt"

const (
	// MaxClientIDBytes bounds identifiers before allocation-heavy processing.
	MaxClientIDBytes = 128
	// MaxRequestIDBytes bounds idempotency identifiers.
	MaxRequestIDBytes = 128
	// MaxNodeIDBytes bounds peer identities and leader hints.
	MaxNodeIDBytes = 128
	// MaxKeyBytes is the maximum encoded key size accepted by the baseline API.
	MaxKeyBytes = 4 * 1024
	// MaxValueBytes is the maximum encoded value size accepted by the baseline API.
	MaxValueBytes = 1024 * 1024
	// MaxCommandBytes bounds one encoded replicated state-machine command while
	// leaving transport headroom for the Raft envelope.
	MaxCommandBytes = 3 * 1024 * 1024
	// MaxSnapshotChunkBytes bounds allocation for one snapshot transfer chunk.
	MaxSnapshotChunkBytes = 1024 * 1024
	// MaxLogEntriesPerAppend bounds one replication batch.
	MaxLogEntriesPerAppend = 256
	// MaxStatusReasonBytes bounds redacted operational status text.
	MaxStatusReasonBytes = 256
	// MaxRPCMessageBytes is the transport-level inbound and outbound message cap.
	MaxRPCMessageBytes = 4 * 1024 * 1024
)

// ClientID identifies a logical client across requests and retries.
type ClientID string

// Validate verifies that the client ID is safe and bounded.
func (id ClientID) Validate() error {
	return validateIdentifier("client_id", string(id), MaxClientIDBytes)
}

// RequestID identifies one idempotent mutation across retries and failover.
type RequestID string

// Validate verifies that the request ID is safe and bounded.
func (id RequestID) Validate() error {
	return validateIdentifier("request_id", string(id), MaxRequestIDBytes)
}

// NodeID identifies a configured Raft peer.
type NodeID string

// Validate verifies that the node ID is safe and bounded.
func (id NodeID) Validate() error {
	return validateIdentifier("node_id", string(id), MaxNodeIDBytes)
}

// ValidationError is a safe, field-specific external input error.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

// ValidateKey verifies a key before it enters the proposal pipeline.
func ValidateKey(key []byte) error {
	switch {
	case len(key) == 0:
		return &ValidationError{Field: "key", Reason: "must not be empty"}
	case len(key) > MaxKeyBytes:
		return &ValidationError{Field: "key", Reason: "exceeds maximum size"}
	default:
		return nil
	}
}

// ValidateValue verifies a value before it enters the proposal pipeline.
func ValidateValue(value []byte) error {
	if len(value) > MaxValueBytes {
		return &ValidationError{Field: "value", Reason: "exceeds maximum size"}
	}
	return nil
}

func validateIdentifier(field, value string, limit int) error {
	if value == "" {
		return &ValidationError{Field: field, Reason: "must not be empty"}
	}
	if len(value) > limit {
		return &ValidationError{Field: field, Reason: "exceeds maximum size"}
	}
	for _, character := range value {
		if !isIdentifierCharacter(character) {
			return &ValidationError{Field: field, Reason: "contains unsupported characters"}
		}
	}
	return nil
}

func isIdentifierCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		character == '-' || character == '_' || character == '.' || character == ':'
}

// ConsistencyMode names the read guarantee requested by a client.
type ConsistencyMode uint8

// Supported consistency modes for read operations.
const (
	ConsistencyUnspecified ConsistencyMode = iota
	ConsistencyLinearizable
	ConsistencyStale
)

// NodeRole is the externally observable consensus role of a node.
type NodeRole uint8

// Observable consensus roles for node status responses.
const (
	NodeRoleUnspecified NodeRole = iota
	NodeRoleFollower
	NodeRoleCandidate
	NodeRoleLeader
)

// HealthState describes whether a process can safely serve its intended role.
type HealthState uint8

// Stable health states exposed to operators and orchestrators.
const (
	HealthUnspecified HealthState = iota
	HealthServing
	HealthDegraded
	HealthNotServing
)

// OperationOutcome records whether a mutation is known to have committed.
type OperationOutcome uint8

// Known outcomes for mutations that return an error or timeout.
const (
	OutcomeUnspecified OperationOutcome = iota
	OutcomeUnknown
	OutcomeCommitted
	OutcomeRejected
)
