package service

import (
	"context"
	"errors"
	"fmt"

	apiv1 "viper/api/gen/v1"
	"viper/internal/raft"
	"viper/internal/statemachine"
	"viper/pkg/contracts"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ConsensusNode is the small proposal/read port required by the client
// service. The service does not reach into Raft's internal state.
type ConsensusNode interface {
	Propose([]byte) (raft.Index, error)
	ReadIndex(context.Context) (raft.Index, error)
	State() raft.RuntimeState
}

type applyBinder interface {
	SetApply(func(raft.Entry) error) error
}

// KVServer implements the frozen public KV protobuf service.
type KVServer struct {
	apiv1.UnimplementedKVServiceServer
	node    ConsensusNode
	machine *statemachine.Machine
}

var _ apiv1.KVServiceServer = (*KVServer)(nil)

// NewKVServer creates a service and binds ordered Raft application to the
// supplied state machine when the node exposes the standard binder.
func NewKVServer(node ConsensusNode, machine *statemachine.Machine) (*KVServer, error) {
	if node == nil || machine == nil {
		return nil, fmt.Errorf("%w: consensus node and state machine are required", ErrInvalidService)
	}
	server := &KVServer{node: node, machine: machine}
	if binder, ok := node.(applyBinder); ok {
		if err := binder.SetApply(func(entry raft.Entry) error {
			_, applyErr := machine.Apply(uint64(entry.Index), entry.Command)
			return applyErr
		}); err != nil {
			return nil, fmt.Errorf("bind state machine: %w", err)
		}
	}
	return server, nil
}

// Put validates and proposes one retry-safe mutation.
func (s *KVServer) Put(ctx context.Context, request *apiv1.PutRequest) (*apiv1.PutResponse, error) {
	if err := validateContext(ctx); err != nil {
		return nil, statusError(err)
	}
	if request == nil || contracts.ClientID(request.GetClientId()).Validate() != nil ||
		contracts.RequestID(request.GetRequestId()).Validate() != nil ||
		contracts.ValidateKey(request.GetKey()) != nil || contracts.ValidateValue(request.GetValue()) != nil {
		return nil, statusError(invalidServiceError())
	}
	command, err := statemachine.EncodeCommand(statemachine.Command{
		Kind:      statemachine.CommandPut,
		ClientID:  contracts.ClientID(request.GetClientId()),
		RequestID: contracts.RequestID(request.GetRequestId()),
		Key:       append([]byte(nil), request.GetKey()...),
		Value:     append([]byte(nil), request.GetValue()...),
	})
	if err != nil {
		return nil, statusError(invalidServiceError())
	}
	if _, err = s.node.Propose(command); err != nil {
		return nil, statusError(s.consensusError(err))
	}
	result, err := s.resolve(request.GetClientId(), request.GetRequestId())
	if err != nil {
		return nil, statusError(err)
	}
	if result.Status == statemachine.MutationRequestConflict {
		return nil, statusError(conflictServiceError())
	}
	return &apiv1.PutResponse{Revision: result.Revision}, nil
}

// Get performs either a majority-confirmed linearizable read or an explicitly
// stale local read.
func (s *KVServer) Get(ctx context.Context, request *apiv1.GetRequest) (*apiv1.GetResponse, error) {
	if err := validateContext(ctx); err != nil {
		return nil, statusError(err)
	}
	if request == nil || contracts.ValidateKey(request.GetKey()) != nil {
		return nil, statusError(invalidServiceError())
	}
	consistency, err := consistencyFromProto(request.GetConsistency())
	if err != nil {
		return nil, statusError(err)
	}
	if consistency == contracts.ConsistencyLinearizable {
		readIndex, readErr := s.node.ReadIndex(ctx)
		if readErr != nil {
			return nil, statusError(s.consensusError(readErr))
		}
		if s.machine.AppliedIndex() < uint64(readIndex) {
			return nil, statusError(internalServiceError())
		}
	}
	value, err := s.machine.Get(request.GetKey())
	if err != nil {
		return nil, statusError(invalidServiceError())
	}
	return &apiv1.GetResponse{Value: value.Value, Found: value.Found, Revision: value.Revision}, nil
}

// CompareAndSwap validates and proposes one atomic conditional mutation.
func (s *KVServer) CompareAndSwap(ctx context.Context, request *apiv1.CompareAndSwapRequest) (*apiv1.CompareAndSwapResponse, error) {
	if err := validateContext(ctx); err != nil {
		return nil, statusError(err)
	}
	if request == nil || contracts.ClientID(request.GetClientId()).Validate() != nil ||
		contracts.RequestID(request.GetRequestId()).Validate() != nil ||
		contracts.ValidateKey(request.GetKey()) != nil || contracts.ValidateValue(request.GetExpectedValue()) != nil ||
		contracts.ValidateValue(request.GetNewValue()) != nil || !request.GetExpectedPresent() && len(request.GetExpectedValue()) != 0 {
		return nil, statusError(invalidServiceError())
	}
	command, err := statemachine.EncodeCommand(statemachine.Command{
		Kind:            statemachine.CommandCompareAndSwap,
		ClientID:        contracts.ClientID(request.GetClientId()),
		RequestID:       contracts.RequestID(request.GetRequestId()),
		Key:             append([]byte(nil), request.GetKey()...),
		ExpectedValue:   append([]byte(nil), request.GetExpectedValue()...),
		ExpectedPresent: request.GetExpectedPresent(),
		Value:           append([]byte(nil), request.GetNewValue()...),
	})
	if err != nil {
		return nil, statusError(invalidServiceError())
	}
	if _, err = s.node.Propose(command); err != nil {
		return nil, statusError(s.consensusError(err))
	}
	result, err := s.resolve(request.GetClientId(), request.GetRequestId())
	if err != nil {
		return nil, statusError(err)
	}
	if result.Status == statemachine.MutationRequestConflict {
		return nil, statusError(conflictServiceError())
	}
	return &apiv1.CompareAndSwapResponse{
		Swapped: result.Swapped, PreviousValue: result.PreviousValue,
		PreviousPresent: result.PreviousPresent, Revision: result.Revision,
	}, nil
}

func (s *KVServer) resolve(clientID, requestID string) (statemachine.Result, error) {
	result, found, err := s.machine.Resolve(contracts.ClientID(clientID), contracts.RequestID(requestID))
	if err != nil || !found {
		return statemachine.Result{}, internalServiceError()
	}
	return result, nil
}

func (s *KVServer) consensusError(err error) error {
	if errors.Is(err, raft.ErrNotLeader) {
		serviceErr := contracts.NewServiceError(contracts.CodeNotLeader, "request must be sent to the leader").WithRetry(true, contracts.OutcomeRejected)
		if leader := s.node.State().LeaderID; leader != "" {
			if hinted, hintErr := serviceErr.WithLeaderHint(contracts.NodeID(leader)); hintErr == nil {
				serviceErr = hinted
			}
		}
		return serviceErr
	}
	if errors.Is(err, raft.ErrNoQuorum) || errors.Is(err, raft.ErrUnavailable) {
		return contracts.NewServiceError(contracts.CodeUnavailable, "service unavailable").WithRetry(true, contracts.OutcomeUnknown)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return contracts.NewServiceError(contracts.CodeDeadlineExceeded, "request deadline exceeded").WithRetry(true, contracts.OutcomeUnknown)
	}
	if errors.Is(err, context.Canceled) {
		return contracts.NewServiceError(contracts.CodeUnavailable, "request canceled").WithRetry(false, contracts.OutcomeUnknown)
	}
	return internalServiceError()
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return invalidServiceError()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func consistencyFromProto(consistency apiv1.ConsistencyMode) (contracts.ConsistencyMode, error) {
	switch consistency {
	case apiv1.ConsistencyMode_CONSISTENCY_MODE_LINEARIZABLE:
		return contracts.ConsistencyLinearizable, nil
	case apiv1.ConsistencyMode_CONSISTENCY_MODE_STALE:
		return contracts.ConsistencyStale, nil
	default:
		return contracts.ConsistencyUnspecified, invalidServiceError()
	}
}

func invalidServiceError() *contracts.ServiceError {
	return contracts.NewServiceError(contracts.CodeInvalidArgument, "invalid request")
}

func internalServiceError() *contracts.ServiceError {
	return contracts.NewServiceError(contracts.CodeInternal, "request failed")
}

func conflictServiceError() *contracts.ServiceError {
	return contracts.NewServiceError(contracts.CodeConflict, "request conflict")
}

func statusError(err error) error {
	if err == nil {
		return nil
	}
	if serviceErr, ok := err.(*contracts.ServiceError); ok {
		return serviceStatusError(serviceErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return serviceStatusError(contracts.NewServiceError(contracts.CodeDeadlineExceeded, "request deadline exceeded").WithRetry(true, contracts.OutcomeUnknown))
	}
	if errors.Is(err, context.Canceled) {
		return serviceStatusError(contracts.NewServiceError(contracts.CodeUnavailable, "request canceled").WithRetry(false, contracts.OutcomeUnknown))
	}
	return serviceStatusError(internalServiceError())
}

func serviceStatusError(err *contracts.ServiceError) error {
	if err == nil {
		return status.Error(codes.Internal, "request failed")
	}
	detail := &apiv1.ErrorDetail{
		Code:       protoErrorCode(err.Code()),
		Retryable:  err.Retryable(),
		Outcome:    protoOutcome(err.Outcome()),
		LeaderHint: err.LeaderHint(),
	}
	grpcCode := grpcCodeFor(err.Code())
	message := safeMessage(err.Code())
	grpcStatus, detailErr := status.New(grpcCode, message).WithDetails(detail)
	if detailErr != nil {
		return status.Error(grpcCode, message)
	}
	return grpcStatus.Err()
}

func grpcCodeFor(code contracts.ErrorCode) codes.Code {
	switch code {
	case contracts.CodeInvalidArgument:
		return codes.InvalidArgument
	case contracts.CodeNotLeader:
		return codes.FailedPrecondition
	case contracts.CodeUnavailable:
		return codes.Unavailable
	case contracts.CodeDeadlineExceeded:
		return codes.DeadlineExceeded
	case contracts.CodePermissionDenied:
		return codes.PermissionDenied
	case contracts.CodeConflict:
		return codes.Aborted
	case contracts.CodeResourceExhausted:
		return codes.ResourceExhausted
	case contracts.CodeDataCorruption:
		return codes.DataLoss
	default:
		return codes.Internal
	}
}

func protoErrorCode(code contracts.ErrorCode) apiv1.ErrorCode {
	switch code {
	case contracts.CodeInvalidArgument:
		return apiv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
	case contracts.CodeNotLeader:
		return apiv1.ErrorCode_ERROR_CODE_NOT_LEADER
	case contracts.CodeUnavailable:
		return apiv1.ErrorCode_ERROR_CODE_UNAVAILABLE
	case contracts.CodeDeadlineExceeded:
		return apiv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED
	case contracts.CodePermissionDenied:
		return apiv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED
	case contracts.CodeDataCorruption:
		return apiv1.ErrorCode_ERROR_CODE_DATA_CORRUPTION
	case contracts.CodeConflict:
		return apiv1.ErrorCode_ERROR_CODE_CONFLICT
	case contracts.CodeResourceExhausted:
		return apiv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED
	default:
		return apiv1.ErrorCode_ERROR_CODE_INTERNAL
	}
}

func protoOutcome(outcome contracts.OperationOutcome) apiv1.OperationOutcome {
	switch outcome {
	case contracts.OutcomeUnknown:
		return apiv1.OperationOutcome_OPERATION_OUTCOME_UNKNOWN
	case contracts.OutcomeCommitted:
		return apiv1.OperationOutcome_OPERATION_OUTCOME_COMMITTED
	case contracts.OutcomeRejected:
		return apiv1.OperationOutcome_OPERATION_OUTCOME_REJECTED
	default:
		return apiv1.OperationOutcome_OPERATION_OUTCOME_UNSPECIFIED
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
	case contracts.CodeConflict:
		return "request conflict"
	case contracts.CodeResourceExhausted:
		return "service resource limit exceeded"
	case contracts.CodeDataCorruption:
		return "data corruption detected"
	default:
		return "request failed"
	}
}
