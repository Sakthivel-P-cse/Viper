package service

import (
	"context"
	"fmt"

	apiv1 "viper/api/gen/v1"
	"viper/internal/observability"
	"viper/internal/raft"
	"viper/pkg/contracts"
)

// NodeStatusServer exposes redacted consensus and readiness state. It never
// returns keys, values, storage paths, or underlying error text.
type NodeStatusServer struct {
	apiv1.UnimplementedNodeStatusServiceServer
	node   ConsensusNode
	health *observability.HealthRegistry
}

var _ apiv1.NodeStatusServiceServer = (*NodeStatusServer)(nil)

// NewNodeStatusServer creates the operational status adapter.
func NewNodeStatusServer(node ConsensusNode, health *observability.HealthRegistry) (*NodeStatusServer, error) {
	if node == nil || health == nil {
		return nil, fmt.Errorf("%w: status dependencies are required", ErrInvalidService)
	}
	return &NodeStatusServer{node: node, health: health}, nil
}

// GetNodeStatus returns a bounded health snapshot for an explicitly selected
// liveness or readiness scope.
func (s *NodeStatusServer) GetNodeStatus(ctx context.Context, request *apiv1.GetNodeStatusRequest) (*apiv1.GetNodeStatusResponse, error) {
	if err := validateContext(ctx); err != nil {
		return nil, statusError(err)
	}
	if request == nil || (request.GetScope() != apiv1.HealthScope_HEALTH_SCOPE_LIVENESS && request.GetScope() != apiv1.HealthScope_HEALTH_SCOPE_READINESS) {
		return nil, statusError(invalidServiceError())
	}
	state := s.node.State()
	health := s.health.Snapshot()
	if health.NodeID == "" {
		health.NodeID = string(state.ID)
	}
	if !state.Healthy {
		health.Health = observability.HealthNotServing
		if health.Reason == "" {
			health.Reason = "consensus node is unhealthy"
		}
	}
	if health.Health == observability.HealthUnknown {
		health.Health = observability.HealthServing
	}
	if request.GetScope() == apiv1.HealthScope_HEALTH_SCOPE_READINESS && health.Health == observability.HealthNotServing {
		return nil, statusError(contractsUnavailableError())
	}
	return &apiv1.GetNodeStatusResponse{
		NodeId:       health.NodeID,
		Health:       statusHealth(health.Health),
		Role:         statusRole(state.Role),
		Term:         uint64(state.Term),
		CommitIndex:  uint64(state.CommitIndex),
		AppliedIndex: uint64(state.LastApplied),
		Reason:       boundedReason(health.Reason),
	}, nil
}

func statusRole(role raft.Role) apiv1.NodeRole {
	switch role {
	case raft.RoleFollower:
		return apiv1.NodeRole_NODE_ROLE_FOLLOWER
	case raft.RoleCandidate:
		return apiv1.NodeRole_NODE_ROLE_CANDIDATE
	case raft.RoleLeader:
		return apiv1.NodeRole_NODE_ROLE_LEADER
	default:
		return apiv1.NodeRole_NODE_ROLE_UNSPECIFIED
	}
}

func statusHealth(health observability.HealthState) apiv1.HealthState {
	switch health {
	case observability.HealthServing:
		return apiv1.HealthState_HEALTH_STATE_SERVING
	case observability.HealthDegraded:
		return apiv1.HealthState_HEALTH_STATE_DEGRADED
	case observability.HealthNotServing:
		return apiv1.HealthState_HEALTH_STATE_NOT_SERVING
	default:
		return apiv1.HealthState_HEALTH_STATE_NOT_SERVING
	}
}

func boundedReason(reason string) string {
	if len(reason) > 256 {
		return reason[:256]
	}
	return reason
}

func contractsUnavailableError() *contracts.ServiceError {
	return contracts.NewServiceError(contracts.CodeUnavailable, "service unavailable").WithRetry(true, contracts.OutcomeUnknown)
}
