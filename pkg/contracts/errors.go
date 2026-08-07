package contracts

import "errors"

// ErrorCode is a stable machine-readable service failure category.
type ErrorCode string

// Stable error codes returned across public service boundaries.
const (
	CodeInvalidArgument   ErrorCode = "invalid_argument"
	CodeNotLeader         ErrorCode = "not_leader"
	CodeUnavailable       ErrorCode = "unavailable"
	CodeDeadlineExceeded  ErrorCode = "deadline_exceeded"
	CodePermissionDenied  ErrorCode = "permission_denied"
	CodeDataCorruption    ErrorCode = "data_corruption"
	CodeConflict          ErrorCode = "conflict"
	CodeResourceExhausted ErrorCode = "resource_exhausted"
	CodeInternal          ErrorCode = "internal"
)

// ServiceError exposes only safe metadata. Internal diagnostic causes belong in
// internal-only wrappers and must not cross the public service boundary.
type ServiceError struct {
	code       ErrorCode
	message    string
	leaderHint string
	outcome    OperationOutcome
	retryable  bool
}

// NewServiceError constructs a safely renderable service error.
func NewServiceError(code ErrorCode, message string) *ServiceError {
	if code == "" {
		code = CodeInternal
	}
	if message == "" {
		message = "request failed"
	}
	return &ServiceError{code: code, message: message}
}

// WithRetry returns a copy carrying retry and operation-outcome metadata.
func (e *ServiceError) WithRetry(retryable bool, outcome OperationOutcome) *ServiceError {
	if e == nil {
		return nil
	}
	clone := *e
	clone.retryable = retryable
	clone.outcome = outcome
	return &clone
}

// WithLeaderHint returns a copy carrying a validated, non-sensitive leader ID.
func (e *ServiceError) WithLeaderHint(leaderHint NodeID) (*ServiceError, error) {
	if e == nil {
		return nil, nil
	}
	if err := leaderHint.Validate(); err != nil {
		return nil, err
	}
	clone := *e
	clone.leaderHint = string(leaderHint)
	return &clone, nil
}

// Code returns the stable machine-readable error category.
func (e *ServiceError) Code() ErrorCode {
	if e == nil {
		return CodeInternal
	}
	return e.code
}

// Retryable reports whether retrying can be safe under the documented outcome.
func (e *ServiceError) Retryable() bool {
	return e != nil && e.retryable
}

// Outcome returns what is known about the mutation's commit state.
func (e *ServiceError) Outcome() OperationOutcome {
	if e == nil {
		return OutcomeUnspecified
	}
	return e.outcome
}

// LeaderHint returns a non-authoritative node ID for client redirection.
func (e *ServiceError) LeaderHint() string {
	if e == nil {
		return ""
	}
	return e.leaderHint
}

func (e *ServiceError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.code) + ": " + e.message
}

// IsCode reports whether err or a wrapped ServiceError has the requested code.
func IsCode(err error, code ErrorCode) bool {
	var serviceError *ServiceError
	return errors.As(err, &serviceError) && serviceError != nil && serviceError.code == code
}
