package checker

import (
	"bytes"
	"encoding/json"
	"fmt"

	"viper/pkg/contracts"
)

const (
	HistoryVersion       = 1
	MaxHistoryBytes      = 16 * 1024 * 1024
	MaxHistoryOperations = 64
)

// OperationKind identifies one operation in a recorded client history.
type OperationKind string

const (
	OperationPut OperationKind = "put"
	OperationGet OperationKind = "get"
	OperationCAS OperationKind = "compare_and_swap"
)

// Consistency names the read guarantee captured in an operation.
type Consistency string

const (
	ConsistencyLinearizable Consistency = "linearizable"
	ConsistencyStale        Consistency = "stale"
)

// Operation is the bounded, versioned JSON history record. Byte fields are
// encoded by encoding/json as base64, keeping the format binary-safe.
type Operation struct {
	ID                    string        `json:"id"`
	ClientID              string        `json:"client_id,omitempty"`
	RequestID             string        `json:"request_id,omitempty"`
	Kind                  OperationKind `json:"kind"`
	Consistency           Consistency   `json:"consistency,omitempty"`
	Key                   []byte        `json:"key,omitempty"`
	ExpectedValue         []byte        `json:"expected_value,omitempty"`
	ExpectedPresent       bool          `json:"expected_present,omitempty"`
	NewValue              []byte        `json:"new_value,omitempty"`
	ResultValue           []byte        `json:"result_value,omitempty"`
	ResultFound           bool          `json:"result_found,omitempty"`
	ResultSwapped         bool          `json:"result_swapped,omitempty"`
	ResultPreviousValue   []byte        `json:"result_previous_value,omitempty"`
	ResultPreviousPresent bool          `json:"result_previous_present,omitempty"`
	ResultRevision        uint64        `json:"result_revision,omitempty"`
	Succeeded             bool          `json:"succeeded"`
	ErrorCode             string        `json:"error_code,omitempty"`
	Invocation            uint64        `json:"invocation"`
	Completion            uint64        `json:"completion"`
}

// History is the complete bounded operation history consumed by the checker.
type History struct {
	Version    int         `json:"version"`
	Operations []Operation `json:"operations"`
}

// Encode validates and serializes a history deterministically.
func Encode(history History) ([]byte, error) {
	if err := Validate(history); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		return nil, fmt.Errorf("encode history: %w", err)
	}
	if len(encoded) > MaxHistoryBytes {
		return nil, ErrHistoryTooLarge
	}
	return encoded, nil
}

// Decode validates a JSON history before allocating model state.
func Decode(data []byte) (History, error) {
	if len(data) > MaxHistoryBytes {
		return History{}, ErrHistoryTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var history History
	if err := decoder.Decode(&history); err != nil {
		return History{}, fmt.Errorf("%w: invalid JSON", ErrInvalidHistory)
	}
	if err := Validate(history); err != nil {
		return History{}, err
	}
	return history, nil
}

// Validate checks all bounded fields and operation shape.
func Validate(history History) error {
	if history.Version != HistoryVersion {
		return fmt.Errorf("%w: unsupported history version", ErrInvalidHistory)
	}
	if len(history.Operations) == 0 || len(history.Operations) > MaxHistoryOperations {
		return fmt.Errorf("%w: invalid operation count", ErrInvalidHistory)
	}
	seenIDs := make(map[string]struct{}, len(history.Operations))
	for _, operation := range history.Operations {
		if !validHistoryID(operation.ID) {
			return fmt.Errorf("%w: invalid operation ID", ErrInvalidHistory)
		}
		if _, exists := seenIDs[operation.ID]; exists {
			return fmt.Errorf("%w: duplicate operation ID", ErrInvalidHistory)
		}
		seenIDs[operation.ID] = struct{}{}
		if operation.Completion < operation.Invocation {
			return fmt.Errorf("%w: operation interval is inverted", ErrInvalidHistory)
		}
		if err := validateOperation(operation); err != nil {
			return err
		}
	}
	return nil
}

func validateOperation(operation Operation) error {
	switch operation.Kind {
	case OperationGet, OperationPut, OperationCAS:
	default:
		return fmt.Errorf("%w: invalid operation kind or key", ErrInvalidHistory)
	}
	if contracts.ValidateKey(operation.Key) != nil {
		return fmt.Errorf("%w: invalid operation key", ErrInvalidHistory)
	}
	if operation.Kind == OperationGet {
		if operation.Consistency != ConsistencyLinearizable && operation.Consistency != ConsistencyStale {
			return fmt.Errorf("%w: invalid get consistency", ErrInvalidHistory)
		}
	} else {
		if contracts.ClientID(operation.ClientID).Validate() != nil || contracts.RequestID(operation.RequestID).Validate() != nil || contracts.ValidateKey(operation.Key) != nil || contracts.ValidateValue(operation.NewValue) != nil || contracts.ValidateValue(operation.ExpectedValue) != nil {
			return fmt.Errorf("%w: invalid mutation input", ErrInvalidHistory)
		}
		if operation.Kind == OperationPut && (operation.ExpectedPresent || len(operation.ExpectedValue) != 0) {
			return fmt.Errorf("%w: invalid put input", ErrInvalidHistory)
		}
		if operation.Kind == OperationCAS && !operation.ExpectedPresent && len(operation.ExpectedValue) != 0 {
			return fmt.Errorf("%w: invalid cas input", ErrInvalidHistory)
		}
	}
	if operation.Consistency != "" && operation.Consistency != ConsistencyLinearizable && operation.Consistency != ConsistencyStale {
		return fmt.Errorf("%w: invalid consistency", ErrInvalidHistory)
	}
	if len(operation.ResultValue) > contracts.MaxValueBytes || len(operation.ResultPreviousValue) > contracts.MaxValueBytes {
		return fmt.Errorf("%w: result exceeds size limit", ErrInvalidHistory)
	}
	if len(operation.ErrorCode) > 64 {
		return fmt.Errorf("%w: error code is too long", ErrInvalidHistory)
	}
	return nil
}

func validHistoryID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' || character == ':') {
			return false
		}
	}
	return true
}
