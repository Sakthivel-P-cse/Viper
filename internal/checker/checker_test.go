package checker

import (
	"testing"
)

func TestCheckerAcceptsSequentialPutAndGet(t *testing.T) {
	history := History{Version: HistoryVersion, Operations: []Operation{
		{ID: "op-1", ClientID: "client-1", RequestID: "request-1", Kind: OperationPut, Key: []byte("key"), NewValue: []byte("one"), Succeeded: true, ResultRevision: 1, Invocation: 1, Completion: 2},
		{ID: "op-2", Kind: OperationGet, Consistency: ConsistencyLinearizable, Key: []byte("key"), ResultValue: []byte("one"), ResultFound: true, ResultRevision: 1, Succeeded: true, Invocation: 3, Completion: 4},
	}}
	result := Check(history)
	if !result.Linearizable || result.ExploredStates == 0 {
		t.Fatalf("checker result = %#v", result)
	}
	encoded, err := Encode(history)
	if err != nil {
		t.Fatalf("encode history: %v", err)
	}
	if decodedResult := CheckJSON(encoded); !decodedResult.Linearizable {
		t.Fatalf("encoded history result = %#v", decodedResult)
	}
}

func TestCheckerUsesPerKeyReadRevision(t *testing.T) {
	history := History{Version: HistoryVersion, Operations: []Operation{
		{ID: "op-1", ClientID: "client-1", RequestID: "request-1", Kind: OperationPut, Key: []byte("a"), NewValue: []byte("one"), Succeeded: true, ResultRevision: 1, Invocation: 1, Completion: 2},
		{ID: "op-2", ClientID: "client-1", RequestID: "request-2", Kind: OperationPut, Key: []byte("b"), NewValue: []byte("two"), Succeeded: true, ResultRevision: 2, Invocation: 3, Completion: 4},
		{ID: "op-3", Kind: OperationGet, Consistency: ConsistencyLinearizable, Key: []byte("a"), ResultValue: []byte("one"), ResultFound: true, ResultRevision: 1, Succeeded: true, Invocation: 5, Completion: 6},
	}}
	if result := Check(history); !result.Linearizable {
		t.Fatalf("per-key revision history result = %#v", result)
	}
}

func TestCheckerRejectsReadOlderThanCompletedWrite(t *testing.T) {
	history := History{Version: HistoryVersion, Operations: []Operation{
		{ID: "op-1", ClientID: "client-1", RequestID: "request-1", Kind: OperationPut, Key: []byte("key"), NewValue: []byte("one"), Succeeded: true, ResultRevision: 1, Invocation: 1, Completion: 2},
		{ID: "op-2", Kind: OperationGet, Consistency: ConsistencyLinearizable, Key: []byte("key"), ResultValue: nil, ResultFound: false, ResultRevision: 1, Succeeded: true, Invocation: 3, Completion: 4},
	}}
	result := Check(history)
	if result.Linearizable || result.Reason == "" || len(result.Counterexample) == 0 {
		t.Fatalf("invalid history result = %#v", result)
	}
}

func TestCheckerAcceptsRetryWithoutApplyingTwice(t *testing.T) {
	history := History{Version: HistoryVersion, Operations: []Operation{
		{ID: "op-1", ClientID: "client-1", RequestID: "request-1", Kind: OperationPut, Key: []byte("key"), NewValue: []byte("one"), Succeeded: true, ResultRevision: 1, Invocation: 1, Completion: 2},
		{ID: "op-2", ClientID: "client-1", RequestID: "request-1", Kind: OperationPut, Key: []byte("key"), NewValue: []byte("one"), Succeeded: true, ResultRevision: 1, Invocation: 1, Completion: 5},
	}}
	if result := Check(history); !result.Linearizable {
		t.Fatalf("retry history result = %#v", result)
	}
}

func TestCheckerAcceptsPutReplacingExistingKeyWithoutPrivatePreviousValue(t *testing.T) {
	history := History{Version: HistoryVersion, Operations: []Operation{
		{ID: "op-1", ClientID: "client-1", RequestID: "request-1", Kind: OperationPut, Key: []byte("key"), NewValue: []byte("one"), Succeeded: true, ResultRevision: 1, Invocation: 1, Completion: 2},
		{ID: "op-2", ClientID: "client-1", RequestID: "request-2", Kind: OperationPut, Key: []byte("key"), NewValue: []byte("two"), Succeeded: true, ResultRevision: 2, Invocation: 3, Completion: 4},
	}}
	if result := Check(history); !result.Linearizable {
		t.Fatalf("replacement Put history result = %#v", result)
	}
}

func TestHistoryRejectsUnknownFieldsAndOversizedOperations(t *testing.T) {
	if _, err := Decode([]byte(`{"version":1,"operations":[{"id":"op-1","kind":"get","key":"a","consistency":"linearizable","unknown":true}]}`)); err == nil {
		t.Fatal("unknown history field was accepted")
	}
	history := History{Version: HistoryVersion, Operations: make([]Operation, MaxHistoryOperations+1)}
	if err := Validate(history); err == nil {
		t.Fatal("oversized history was accepted")
	}
}
