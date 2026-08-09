package statemachine

import (
	"bytes"
	"errors"
	"testing"

	"viper/pkg/contracts"
)

func commandBytes(t *testing.T, command Command) []byte {
	t.Helper()
	encoded, err := EncodeCommand(command)
	if err != nil {
		t.Fatalf("encode command: %v", err)
	}
	return encoded
}

func putCommand(client, request, key, value string) Command {
	return Command{
		Kind:      CommandPut,
		ClientID:  contracts.ClientID(client),
		RequestID: contracts.RequestID(request),
		Key:       []byte(key),
		Value:     []byte(value),
	}
}

func casCommand(client, request, key, expected, value string, expectedPresent bool) Command {
	return Command{
		Kind:            CommandCompareAndSwap,
		ClientID:        contracts.ClientID(client),
		RequestID:       contracts.RequestID(request),
		Key:             []byte(key),
		ExpectedValue:   []byte(expected),
		ExpectedPresent: expectedPresent,
		Value:           []byte(value),
	}
}

func TestCommandRoundTripAndStrictValidation(t *testing.T) {
	want := casCommand("client-1", "request-1", "key", "old", "new", true)
	encoded := commandBytes(t, want)
	got, err := DecodeCommand(encoded)
	if err != nil {
		t.Fatalf("decode command: %v", err)
	}
	if got.Kind != want.Kind || got.ClientID != want.ClientID || got.RequestID != want.RequestID ||
		!bytes.Equal(got.Key, want.Key) || !bytes.Equal(got.Value, want.Value) ||
		!bytes.Equal(got.ExpectedValue, want.ExpectedValue) || got.ExpectedPresent != want.ExpectedPresent {
		t.Fatalf("decoded command = %#v, want %#v", got, want)
	}
	for _, malformed := range [][]byte{nil, []byte("bad"), append([]byte(nil), encoded[:len(encoded)-1]...)} {
		if _, err := DecodeCommand(malformed); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("DecodeCommand(%x) error = %v, want ErrInvalidCommand", malformed, err)
		}
	}
	if _, err = EncodeCommand(Command{Kind: CommandPut, ClientID: "client-1", RequestID: "request-1", Key: []byte("key"), ExpectedPresent: true}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("invalid Put error = %v, want ErrInvalidCommand", err)
	}
}

func TestMachineAppliesInOrderAndDeduplicatesRetries(t *testing.T) {
	machine := New()
	put := putCommand("client-1", "request-1", "key", "one")
	first, err := machine.Apply(1, commandBytes(t, put))
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if first.Status != MutationApplied || first.Revision != 1 || first.Duplicate {
		t.Fatalf("first result = %#v", first)
	}
	duplicate, err := machine.Apply(2, commandBytes(t, put))
	if err != nil {
		t.Fatalf("duplicate apply: %v", err)
	}
	if !duplicate.Duplicate || duplicate.Revision != first.Revision || duplicate.Status != first.Status {
		t.Fatalf("duplicate result = %#v, want original result marked duplicate", duplicate)
	}
	value, err := machine.Get([]byte("key"))
	if err != nil {
		t.Fatalf("get after duplicate: %v", err)
	}
	if !value.Found || string(value.Value) != "one" || value.Revision != 1 || machine.AppliedIndex() != 2 {
		t.Fatalf("value = %#v, applied index = %d", value, machine.AppliedIndex())
	}
	if _, err = machine.Apply(4, commandBytes(t, putCommand("client-1", "request-2", "other", "value"))); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("skipped index error = %v, want ErrOutOfOrder", err)
	}
}

func TestMachineCASReturnsPreviousStateAndConflict(t *testing.T) {
	machine := New()
	if _, err := machine.Apply(1, commandBytes(t, putCommand("client-1", "put-1", "key", "one"))); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	failed, err := machine.Apply(2, commandBytes(t, casCommand("client-1", "cas-1", "key", "wrong", "two", true)))
	if err != nil {
		t.Fatalf("failed CAS: %v", err)
	}
	if failed.Status != MutationConditionFailed || failed.Swapped || !failed.PreviousPresent || string(failed.PreviousValue) != "one" || failed.Revision != 1 {
		t.Fatalf("failed CAS result = %#v", failed)
	}
	swapped, err := machine.Apply(3, commandBytes(t, casCommand("client-1", "cas-2", "key", "one", "two", true)))
	if err != nil {
		t.Fatalf("successful CAS: %v", err)
	}
	if swapped.Status != MutationApplied || !swapped.Swapped || string(swapped.PreviousValue) != "one" || swapped.Revision != 2 {
		t.Fatalf("successful CAS result = %#v", swapped)
	}
	conflict, err := machine.Apply(4, commandBytes(t, putCommand("client-1", "put-1", "key", "different")))
	if err != nil {
		t.Fatalf("request conflict apply: %v", err)
	}
	if conflict.Status != MutationRequestConflict || conflict.Revision != 2 {
		t.Fatalf("request conflict result = %#v", conflict)
	}
}

func TestMachineSnapshotRestoresKVAndDeduplicationDeterministically(t *testing.T) {
	original := New()
	commands := []Command{
		putCommand("client-1", "request-1", "b-key", "two"),
		putCommand("client-2", "request-1", "a-key", "one"),
		casCommand("client-1", "request-2", "b-key", "two", "three", true),
	}
	for index, command := range commands {
		if _, err := original.Apply(uint64(index+1), commandBytes(t, command)); err != nil {
			t.Fatalf("apply command %d: %v", index+1, err)
		}
	}
	snapshot, err := original.Snapshot()
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	secondSnapshot, err := original.Snapshot()
	if err != nil {
		t.Fatalf("create second snapshot: %v", err)
	}
	if !bytes.Equal(snapshot, secondSnapshot) {
		t.Fatal("identical state produced nondeterministic snapshots")
	}
	restored := New()
	if err = restored.Restore(snapshot); err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	for _, key := range []string{"a-key", "b-key"} {
		want, getErr := original.Get([]byte(key))
		if getErr != nil {
			t.Fatalf("read original %q: %v", key, getErr)
		}
		got, getErr := restored.Get([]byte(key))
		if getErr != nil {
			t.Fatalf("read restored %q: %v", key, getErr)
		}
		if !bytes.Equal(got.Value, want.Value) || got.Found != want.Found || got.Revision != want.Revision {
			t.Fatalf("restored %q = %#v, want %#v", key, got, want)
		}
	}
	resolved, found, err := restored.Resolve("client-1", "request-2")
	if err != nil || !found || !resolved.Swapped || resolved.Revision != 3 {
		t.Fatalf("restored dedup result = %#v, found = %t, error = %v", resolved, found, err)
	}
	if err = restored.Restore(snapshot[:len(snapshot)-1]); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("truncated snapshot error = %v, want ErrInvalidSnapshot", err)
	}
}
