package client

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	apiv1 "viper/api/gen/v1"
	"viper/pkg/contracts"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type fakeKVService struct {
	putRequest *apiv1.PutRequest
	getRequest *apiv1.GetRequest
	casRequest *apiv1.CompareAndSwapRequest
	putError   error
}

func (f *fakeKVService) Put(_ context.Context, request *apiv1.PutRequest, _ ...grpc.CallOption) (*apiv1.PutResponse, error) {
	f.putRequest = request
	if f.putError != nil {
		return nil, f.putError
	}
	return &apiv1.PutResponse{Revision: 6}, nil
}

func (f *fakeKVService) Get(_ context.Context, request *apiv1.GetRequest, _ ...grpc.CallOption) (*apiv1.GetResponse, error) {
	f.getRequest = request
	return &apiv1.GetResponse{Value: []byte("value"), Found: true, Revision: 7}, nil
}

func (f *fakeKVService) CompareAndSwap(_ context.Context, request *apiv1.CompareAndSwapRequest, _ ...grpc.CallOption) (*apiv1.CompareAndSwapResponse, error) {
	f.casRequest = request
	return &apiv1.CompareAndSwapResponse{Swapped: true, PreviousValue: []byte("old"), PreviousPresent: true, Revision: 8}, nil
}

func TestClientValidatesRequestsAndCopiesPayloads(t *testing.T) {
	fake := &fakeKVService{}
	client, err := NewWithService(fake)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	key := []byte("key")
	value := []byte("value")
	result, err := client.Put(context.Background(), "client-1", "request-1", key, value)
	if err != nil || result.Revision != 6 {
		t.Fatalf("Put result = %#v, error = %v", result, err)
	}
	key[0] = 'X'
	value[0] = 'X'
	if string(fake.putRequest.GetKey()) != "key" || string(fake.putRequest.GetValue()) != "value" {
		t.Fatal("Put request retained caller-owned byte slices")
	}
	if _, err = client.Put(context.Background(), "", "request-2", []byte("key"), nil); !contracts.IsCode(err, contracts.CodeInvalidArgument) {
		t.Fatalf("invalid Put error = %v, want invalid argument", err)
	}
	getResult, err := client.Get(context.Background(), []byte("key"), contracts.ConsistencyLinearizable)
	if err != nil || !getResult.Found || string(getResult.Value) != "value" || getResult.Revision != 7 {
		t.Fatalf("Get result = %#v, error = %v", getResult, err)
	}
	if fake.getRequest.GetConsistency() != apiv1.ConsistencyMode_CONSISTENCY_MODE_LINEARIZABLE {
		t.Fatalf("Get consistency = %v", fake.getRequest.GetConsistency())
	}
	casResult, err := client.CompareAndSwap(context.Background(), "client-1", "request-2", []byte("key"), []byte("value"), []byte("new"), true)
	if err != nil || !casResult.Swapped || string(casResult.PreviousValue) != "old" || casResult.Revision != 8 {
		t.Fatalf("CAS result = %#v, error = %v", casResult, err)
	}
}

func TestClientMapsSafeErrorDetails(t *testing.T) {
	grpcStatus, err := status.New(codes.FailedPrecondition, "sensitive server detail").WithDetails(&apiv1.ErrorDetail{
		Code:       apiv1.ErrorCode_ERROR_CODE_NOT_LEADER,
		Retryable:  true,
		Outcome:    apiv1.OperationOutcome_OPERATION_OUTCOME_UNKNOWN,
		LeaderHint: "node-2",
	})
	if err != nil {
		t.Fatalf("create gRPC detail: %v", err)
	}
	grpcError := grpcStatus.Err()
	client, err := NewWithService(&fakeKVService{putError: grpcError})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	_, err = client.Put(context.Background(), "client-1", "request-1", []byte("key"), []byte("value"))
	if !contracts.IsCode(err, contracts.CodeNotLeader) {
		t.Fatalf("mapped error = %v, want not leader", err)
	}
	serviceError := err.(*contracts.ServiceError)
	if !serviceError.Retryable() || serviceError.Outcome() != contracts.OutcomeUnknown || serviceError.LeaderHint() != "node-2" {
		t.Fatalf("mapped metadata = retryable:%t outcome:%v leader:%q", serviceError.Retryable(), serviceError.Outcome(), serviceError.LeaderHint())
	}
	if serviceError.Error() == "not leader: sensitive server detail" {
		t.Fatal("client exposed the server's raw error message")
	}
}

func TestClientRejectsMissingExplicitConsistency(t *testing.T) {
	client, err := NewWithService(&fakeKVService{})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	_, err = client.Get(context.Background(), []byte("key"), contracts.ConsistencyUnspecified)
	if !contracts.IsCode(err, contracts.CodeInvalidArgument) {
		t.Fatalf("unspecified consistency error = %v, want invalid argument", err)
	}
}

func TestClientRejectsNilConstruction(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("New(nil) error = %v, want ErrInvalidClient", err)
	}
	if _, err := NewWithService(nil); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("NewWithService(nil) error = %v, want ErrInvalidClient", err)
	}
}

func TestDialRequiresTLSAndAppliesClientLimits(t *testing.T) {
	if _, _, err := Dial(context.Background(), "127.0.0.1:1", DialConfig{}); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("Dial without TLS error = %v, want ErrInvalidClient", err)
	}
	client, connection, err := Dial(context.Background(), "127.0.0.1:1", DialConfig{Credentials: credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13})})
	if err != nil || client == nil || connection == nil {
		t.Fatalf("Dial = client %#v, connection %#v, error %v", client, connection, err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close client connection: %v", err)
	}
}
