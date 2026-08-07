package contracts_test

import (
	"fmt"
	"strings"
	"testing"

	"viper/pkg/contracts"
)

func TestIdentifiersValidateStrictly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		name string
	}{
		{name: "valid client", err: contracts.ClientID("client-01").Validate()},
		{name: "valid request", err: contracts.RequestID("018f:request_01").Validate()},
		{name: "valid node", err: contracts.NodeID("node-01").Validate()},
		{name: "empty client", err: contracts.ClientID("").Validate()},
		{name: "space rejected", err: contracts.RequestID("request 01").Validate()},
		{name: "oversized", err: contracts.RequestID(strings.Repeat("a", contracts.MaxRequestIDBytes+1)).Validate()},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			valid := strings.HasPrefix(test.name, "valid")
			if valid && test.err != nil {
				t.Fatalf("expected valid identifier: %v", test.err)
			}
			if !valid && test.err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestPayloadLimits(t *testing.T) {
	t.Parallel()

	if err := contracts.ValidateKey(nil); err == nil {
		t.Fatal("expected empty key to be rejected")
	}
	if err := contracts.ValidateKey(make([]byte, contracts.MaxKeyBytes)); err != nil {
		t.Fatalf("expected maximum-size key to be accepted: %v", err)
	}
	if err := contracts.ValidateKey(make([]byte, contracts.MaxKeyBytes+1)); err == nil {
		t.Fatal("expected oversized key to be rejected")
	}
	if err := contracts.ValidateValue(make([]byte, contracts.MaxValueBytes)); err != nil {
		t.Fatalf("expected maximum-size value to be accepted: %v", err)
	}
	if err := contracts.ValidateValue(make([]byte, contracts.MaxValueBytes+1)); err == nil {
		t.Fatal("expected oversized value to be rejected")
	}
}

func TestMutationEnvelopeFitsTransportBudget(t *testing.T) {
	t.Parallel()

	// CAS is the largest mutation: expected value, replacement value, key, both
	// identifiers, and conservative serialization metadata.
	const serializationHeadroom = 4 * 1024
	largestMutation := 2*contracts.MaxValueBytes +
		contracts.MaxKeyBytes +
		contracts.MaxClientIDBytes +
		contracts.MaxRequestIDBytes +
		serializationHeadroom

	if contracts.MaxCommandBytes < largestMutation {
		t.Fatalf("command limit %d cannot contain largest mutation %d", contracts.MaxCommandBytes, largestMutation)
	}
	if contracts.MaxCommandBytes >= contracts.MaxRPCMessageBytes {
		t.Fatalf("command limit %d leaves no Raft transport headroom below %d", contracts.MaxCommandBytes, contracts.MaxRPCMessageBytes)
	}
}

func TestServiceErrorSafeMetadata(t *testing.T) {
	t.Parallel()

	err := contracts.NewServiceError(
		contracts.CodeUnavailable,
		"service is temporarily unavailable",
	).WithRetry(true, contracts.OutcomeUnknown)

	wrapped := fmt.Errorf("request context: %w", err)
	if !contracts.IsCode(wrapped, contracts.CodeUnavailable) {
		t.Fatal("service error code was not discoverable")
	}
	if !err.Retryable() || err.Outcome() != contracts.OutcomeUnknown {
		t.Fatal("service error lost retry metadata")
	}
	withHint, hintErr := err.WithLeaderHint(contracts.NodeID("node-02"))
	if hintErr != nil || withHint.LeaderHint() != "node-02" {
		t.Fatalf("expected validated leader hint: error=%v hint=%q", hintErr, withHint.LeaderHint())
	}
	if _, hintErr := err.WithLeaderHint(contracts.NodeID("invalid node")); hintErr == nil {
		t.Fatal("expected invalid leader hint to be rejected")
	}
}

func TestIsCodeHandlesTypedNil(t *testing.T) {
	t.Parallel()

	var serviceError *contracts.ServiceError
	var err error = serviceError
	if contracts.IsCode(err, contracts.CodeInternal) {
		t.Fatal("typed-nil service error unexpectedly matched a code")
	}
}
