package statemachine

import (
	"fmt"
	"testing"

	"viper/pkg/contracts"
)

func BenchmarkMachineApplyPut(b *testing.B) {
	machine := New()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 1; index <= b.N; index++ {
		command, err := EncodeCommand(Command{
			Kind:      CommandPut,
			ClientID:  contracts.ClientID("benchmark-client"),
			RequestID: contracts.RequestID(fmt.Sprintf("request-%d", index)),
			Key:       []byte("benchmark-key"),
			Value:     []byte("benchmark-value"),
		})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := machine.Apply(uint64(index), command); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMachineGet(b *testing.B) {
	machine := New()
	command, err := EncodeCommand(Command{
		Kind:      CommandPut,
		ClientID:  contracts.ClientID("benchmark-client"),
		RequestID: contracts.RequestID("seed-request"),
		Key:       []byte("benchmark-key"),
		Value:     []byte("benchmark-value"),
	})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := machine.Apply(1, command); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := machine.Get([]byte("benchmark-key")); err != nil {
			b.Fatal(err)
		}
	}
}
