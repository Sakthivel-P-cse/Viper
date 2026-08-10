package service

import (
	"context"
	"errors"
	"testing"

	apiv1 "viper/api/gen/v1"
	"viper/internal/observability"
	"viper/internal/raft"
	"viper/internal/statemachine"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeConsensus struct {
	machine    *statemachine.Machine
	index      uint64
	state      raft.RuntimeState
	proposeErr error
}

func (f *fakeConsensus) Propose(command []byte) (raft.Index, error) {
	if f.proposeErr != nil {
		return 0, f.proposeErr
	}
	f.index++
	if _, err := f.machine.Apply(f.index, command); err != nil {
		return 0, err
	}
	f.state.CommitIndex = raft.Index(f.index)
	f.state.LastApplied = raft.Index(f.index)
	return raft.Index(f.index), nil
}

func (f *fakeConsensus) ReadIndex(context.Context) (raft.Index, error) {
	return raft.Index(f.index), nil
}

func (f *fakeConsensus) State() raft.RuntimeState { return f.state }

func newTestServer(t *testing.T) (*KVServer, *fakeConsensus) {
	t.Helper()
	machine := statemachine.New()
	node := &fakeConsensus{machine: machine, state: raft.RuntimeState{ID: "node-1", Role: raft.RoleLeader, Healthy: true}}
	server, err := NewKVServer(node, machine)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	return server, node
}

func TestKVServerPutCASAndConsistencyAwareGet(t *testing.T) {
	server, node := newTestServer(t)
	put, err := server.Put(context.Background(), &apiv1.PutRequest{ClientId: "client-1", RequestId: "put-1", Key: []byte("key"), Value: []byte("one")})
	if err != nil || put.GetRevision() != 1 {
		t.Fatalf("Put response = %#v, error = %v", put, err)
	}
	get, err := server.Get(context.Background(), &apiv1.GetRequest{Key: []byte("key"), Consistency: apiv1.ConsistencyMode_CONSISTENCY_MODE_LINEARIZABLE})
	if err != nil || !get.GetFound() || string(get.GetValue()) != "one" || get.GetRevision() != 1 {
		t.Fatalf("linearizable Get response = %#v, error = %v", get, err)
	}
	cas, err := server.CompareAndSwap(context.Background(), &apiv1.CompareAndSwapRequest{
		ClientId: "client-1", RequestId: "cas-1", Key: []byte("key"), ExpectedValue: []byte("one"), ExpectedPresent: true, NewValue: []byte("two"),
	})
	if err != nil || !cas.GetSwapped() || string(cas.GetPreviousValue()) != "one" || cas.GetRevision() != 2 {
		t.Fatalf("CAS response = %#v, error = %v", cas, err)
	}
	stale, err := server.Get(context.Background(), &apiv1.GetRequest{Key: []byte("key"), Consistency: apiv1.ConsistencyMode_CONSISTENCY_MODE_STALE})
	if err != nil || string(stale.GetValue()) != "two" || node.index != 2 {
		t.Fatalf("stale Get response = %#v, error = %v", stale, err)
	}
}

func TestKVServerRejectsInvalidAndUnspecifiedReads(t *testing.T) {
	server, _ := newTestServer(t)
	if _, err := server.Put(context.Background(), &apiv1.PutRequest{ClientId: "", RequestId: "request", Key: []byte("key")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid Put status = %v, want InvalidArgument", err)
	}
	if _, err := server.Get(context.Background(), &apiv1.GetRequest{Key: []byte("key")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unspecified Get status = %v, want InvalidArgument", err)
	}
	if _, err := server.Get(context.Background(), &apiv1.GetRequest{Key: []byte("key"), Consistency: apiv1.ConsistencyMode_CONSISTENCY_MODE_LINEARIZABLE}); err != nil {
		t.Fatalf("valid Get status = %v", err)
	}
}

func TestKVServerMapsNotLeaderWithSafeDetails(t *testing.T) {
	machine := statemachine.New()
	node := &fakeConsensus{machine: machine, state: raft.RuntimeState{ID: "node-2", Role: raft.RoleFollower, LeaderID: "node-1", Healthy: true}, proposeErr: raft.ErrNotLeader}
	server, err := NewKVServer(node, machine)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	_, err = server.Put(context.Background(), &apiv1.PutRequest{ClientId: "client-1", RequestId: "request-1", Key: []byte("key"), Value: []byte("value")})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NotLeader status = %v, want FailedPrecondition", err)
	}
	grpcStatus, ok := status.FromError(err)
	if !ok {
		t.Fatalf("NotLeader error is not a gRPC status: %v", err)
	}
	details := grpcStatus.Details()
	if len(details) != 1 {
		t.Fatalf("NotLeader details = %#v", details)
	}
	detail, ok := details[0].(*apiv1.ErrorDetail)
	if !ok || detail.GetCode() != apiv1.ErrorCode_ERROR_CODE_NOT_LEADER || detail.GetLeaderHint() != "node-1" {
		t.Fatalf("NotLeader detail = %#v", details[0])
	}
}

func TestKVServerPropagatesOnlySafeServiceErrors(t *testing.T) {
	server, node := newTestServer(t)
	node.proposeErr = errors.New("private storage path and token")
	_, err := server.Put(context.Background(), &apiv1.PutRequest{ClientId: "client-1", RequestId: "request-1", Key: []byte("key"), Value: []byte("value")})
	if status.Code(err) != codes.Internal || err.Error() == "private storage path and token" {
		t.Fatalf("unsafe error exposure: %v", err)
	}
}

func TestNodeStatusServerReturnsRedactedState(t *testing.T) {
	_, node := newTestServer(t)
	health := observability.NewHealthRegistry("node-1")
	health.Update(observability.HealthSnapshot{NodeID: "node-1", Health: observability.HealthServing, Reason: "ready"})
	server, err := NewNodeStatusServer(node, health)
	if err != nil {
		t.Fatalf("create status server: %v", err)
	}
	response, err := server.GetNodeStatus(context.Background(), &apiv1.GetNodeStatusRequest{Scope: apiv1.HealthScope_HEALTH_SCOPE_LIVENESS})
	if err != nil || response.GetNodeId() != "node-1" || response.GetHealth() != apiv1.HealthState_HEALTH_STATE_SERVING {
		t.Fatalf("status response = %#v, error = %v", response, err)
	}
	if _, err = server.GetNodeStatus(context.Background(), &apiv1.GetNodeStatusRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unspecified status scope = %v, want InvalidArgument", err)
	}
}
