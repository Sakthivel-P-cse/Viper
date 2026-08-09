// Package client provides a small typed client adapter for the frozen Viper
// KV protobuf service. Leadership, proposal, and consistency semantics remain
// server responsibilities.
package client

import (
	"context"
	"errors"

	apiv1 "viper/api/gen/v1"
	"viper/pkg/contracts"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrInvalidClient indicates that the adapter was constructed without a
// usable generated service client.
var ErrInvalidClient = errors.New("invalid Viper client")

// PutResult is the committed revision returned by a Put operation.
type PutResult struct {
	Revision uint64
}

// GetResult is an isolated value and its observed state revision.
type GetResult struct {
	Value    []byte
	Found    bool
	Revision uint64
}

// CompareAndSwapResult is the atomic CAS outcome and prior value.
type CompareAndSwapResult struct {
	Swapped         bool
	PreviousValue   []byte
	PreviousPresent bool
	Revision        uint64
}

// Client is a typed adapter around the generated KV service client.
type Client struct {
	service apiv1.KVServiceClient
}

// New constructs a client from a gRPC connection. The caller owns the
// connection and must configure its credentials and message limits.
func New(connection grpc.ClientConnInterface) (*Client, error) {
	if connection == nil {
		return nil, ErrInvalidClient
	}
	return NewWithService(apiv1.NewKVServiceClient(connection))
}

// NewWithService constructs a client around an already configured service.
func NewWithService(service apiv1.KVServiceClient) (*Client, error) {
	if service == nil {
		return nil, ErrInvalidClient
	}
	return &Client{service: service}, nil
}

// Put submits one validated idempotent mutation request.
func (c *Client) Put(ctx context.Context, clientID contracts.ClientID, requestID contracts.RequestID, key, value []byte) (PutResult, error) {
	if err := validateMutationContext(ctx); err != nil {
		return PutResult{}, err
	}
	if err := validateMutation(clientID, requestID, key, value); err != nil {
		return PutResult{}, err
	}
	response, err := c.service.Put(ctx, &apiv1.PutRequest{
		ClientId:  string(clientID),
		RequestId: string(requestID),
		Key:       append([]byte(nil), key...),
		Value:     append([]byte(nil), value...),
	})
	if err != nil {
		return PutResult{}, safeServiceError(err)
	}
	if response == nil {
		return PutResult{}, internalError()
	}
	return PutResult{Revision: response.GetRevision()}, nil
}

// Get performs a read with an explicit linearizable or stale consistency mode.
func (c *Client) Get(ctx context.Context, key []byte, consistency contracts.ConsistencyMode) (GetResult, error) {
	if err := validateMutationContext(ctx); err != nil {
		return GetResult{}, err
	}
	if err := contracts.ValidateKey(key); err != nil {
		return GetResult{}, invalidArgument()
	}
	protoConsistency, err := consistencyToProto(consistency)
	if err != nil {
		return GetResult{}, err
	}
	response, err := c.service.Get(ctx, &apiv1.GetRequest{
		Key:         append([]byte(nil), key...),
		Consistency: protoConsistency,
	})
	if err != nil {
		return GetResult{}, safeServiceError(err)
	}
	if response == nil {
		return GetResult{}, internalError()
	}
	return GetResult{
		Value:    append([]byte(nil), response.GetValue()...),
		Found:    response.GetFound(),
		Revision: response.GetRevision(),
	}, nil
}

// CompareAndSwap atomically replaces a value when the expected state matches.
func (c *Client) CompareAndSwap(ctx context.Context, clientID contracts.ClientID, requestID contracts.RequestID, key, expectedValue, newValue []byte, expectedPresent bool) (CompareAndSwapResult, error) {
	if err := validateMutationContext(ctx); err != nil {
		return CompareAndSwapResult{}, err
	}
	if err := validateMutation(clientID, requestID, key, newValue); err != nil {
		return CompareAndSwapResult{}, err
	}
	if err := contracts.ValidateValue(expectedValue); err != nil {
		return CompareAndSwapResult{}, invalidArgument()
	}
	if !expectedPresent && len(expectedValue) != 0 {
		return CompareAndSwapResult{}, invalidArgument()
	}
	response, err := c.service.CompareAndSwap(ctx, &apiv1.CompareAndSwapRequest{
		ClientId:        string(clientID),
		RequestId:       string(requestID),
		Key:             append([]byte(nil), key...),
		ExpectedValue:   append([]byte(nil), expectedValue...),
		ExpectedPresent: expectedPresent,
		NewValue:        append([]byte(nil), newValue...),
	})
	if err != nil {
		return CompareAndSwapResult{}, safeServiceError(err)
	}
	if response == nil {
		return CompareAndSwapResult{}, internalError()
	}
	return CompareAndSwapResult{
		Swapped:         response.GetSwapped(),
		PreviousValue:   append([]byte(nil), response.GetPreviousValue()...),
		PreviousPresent: response.GetPreviousPresent(),
		Revision:        response.GetRevision(),
	}, nil
}

func validateMutationContext(ctx context.Context) error {
	if ctx == nil {
		return invalidArgument()
	}
	if err := ctx.Err(); err != nil {
		return safeServiceError(err)
	}
	return nil
}

func validateMutation(clientID contracts.ClientID, requestID contracts.RequestID, key, value []byte) error {
	if clientID.Validate() != nil || requestID.Validate() != nil || contracts.ValidateKey(key) != nil || contracts.ValidateValue(value) != nil {
		return invalidArgument()
	}
	return nil
}

func consistencyToProto(consistency contracts.ConsistencyMode) (apiv1.ConsistencyMode, error) {
	switch consistency {
	case contracts.ConsistencyLinearizable:
		return apiv1.ConsistencyMode_CONSISTENCY_MODE_LINEARIZABLE, nil
	case contracts.ConsistencyStale:
		return apiv1.ConsistencyMode_CONSISTENCY_MODE_STALE, nil
	default:
		return apiv1.ConsistencyMode_CONSISTENCY_MODE_UNSPECIFIED, invalidArgument()
	}
}

func invalidArgument() error {
	return contracts.NewServiceError(contracts.CodeInvalidArgument, "invalid request")
}

func internalError() error {
	return contracts.NewServiceError(contracts.CodeInternal, "service returned an invalid response")
}

func safeServiceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return contracts.NewServiceError(contracts.CodeDeadlineExceeded, "request deadline exceeded").WithRetry(true, contracts.OutcomeUnknown)
	}
	if errors.Is(err, context.Canceled) {
		return contracts.NewServiceError(contracts.CodeUnavailable, "request canceled").WithRetry(false, contracts.OutcomeUnknown)
	}
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return contracts.NewServiceError(contracts.CodeInternal, "request failed")
	}
	code := codeFromGRPC(grpcStatus.Code())
	retryable := code == contracts.CodeUnavailable || code == contracts.CodeDeadlineExceeded
	outcome := contracts.OutcomeUnspecified
	leaderHint := ""
	for _, detail := range grpcStatus.Details() {
		errorDetail, ok := detail.(*apiv1.ErrorDetail)
		if !ok {
			continue
		}
		if detailCode := codeFromProto(errorDetail.GetCode()); detailCode != "" {
			code = detailCode
		}
		retryable = errorDetail.GetRetryable()
		outcome = outcomeFromProto(errorDetail.GetOutcome())
		leaderHint = errorDetail.GetLeaderHint()
		break
	}
	serviceError := contracts.NewServiceError(code, safeMessage(code)).WithRetry(retryable, outcome)
	if leaderHint != "" {
		if hinted, hintErr := serviceError.WithLeaderHint(contracts.NodeID(leaderHint)); hintErr == nil {
			serviceError = hinted
		}
	}
	return serviceError
}

func codeFromGRPC(code codes.Code) contracts.ErrorCode {
	switch code {
	case codes.InvalidArgument:
		return contracts.CodeInvalidArgument
	case codes.Aborted, codes.FailedPrecondition:
		return contracts.CodeConflict
	case codes.PermissionDenied, codes.Unauthenticated:
		return contracts.CodePermissionDenied
	case codes.ResourceExhausted:
		return contracts.CodeResourceExhausted
	case codes.DeadlineExceeded:
		return contracts.CodeDeadlineExceeded
	case codes.Unavailable, codes.Canceled:
		return contracts.CodeUnavailable
	default:
		return contracts.CodeInternal
	}
}

func codeFromProto(code apiv1.ErrorCode) contracts.ErrorCode {
	switch code {
	case apiv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return contracts.CodeInvalidArgument
	case apiv1.ErrorCode_ERROR_CODE_NOT_LEADER:
		return contracts.CodeNotLeader
	case apiv1.ErrorCode_ERROR_CODE_UNAVAILABLE:
		return contracts.CodeUnavailable
	case apiv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED:
		return contracts.CodeDeadlineExceeded
	case apiv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return contracts.CodePermissionDenied
	case apiv1.ErrorCode_ERROR_CODE_DATA_CORRUPTION:
		return contracts.CodeDataCorruption
	case apiv1.ErrorCode_ERROR_CODE_CONFLICT:
		return contracts.CodeConflict
	case apiv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED:
		return contracts.CodeResourceExhausted
	case apiv1.ErrorCode_ERROR_CODE_INTERNAL:
		return contracts.CodeInternal
	default:
		return ""
	}
}

func outcomeFromProto(outcome apiv1.OperationOutcome) contracts.OperationOutcome {
	switch outcome {
	case apiv1.OperationOutcome_OPERATION_OUTCOME_UNKNOWN:
		return contracts.OutcomeUnknown
	case apiv1.OperationOutcome_OPERATION_OUTCOME_COMMITTED:
		return contracts.OutcomeCommitted
	case apiv1.OperationOutcome_OPERATION_OUTCOME_REJECTED:
		return contracts.OutcomeRejected
	default:
		return contracts.OutcomeUnspecified
	}
}

func safeMessage(code contracts.ErrorCode) string {
	switch code {
	case contracts.CodeInvalidArgument:
		return "invalid request"
	case contracts.CodeNotLeader:
		return "request must be sent to the leader"
	case contracts.CodeUnavailable:
		return "service unavailable"
	case contracts.CodeDeadlineExceeded:
		return "request deadline exceeded"
	case contracts.CodePermissionDenied:
		return "permission denied"
	case contracts.CodeDataCorruption:
		return "data corruption detected"
	case contracts.CodeConflict:
		return "request conflict"
	case contracts.CodeResourceExhausted:
		return "service resource limit exceeded"
	default:
		return "request failed"
	}
}
