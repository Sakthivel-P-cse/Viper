package statemachine

import "testing"

func FuzzDecodeCommand(f *testing.F) {
	seed, err := EncodeCommand(Command{Kind: CommandPut, ClientID: "client-1", RequestID: "request-1", Key: []byte("key"), Value: []byte("value")})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeCommand(data)
	})
}

func FuzzDecodeSnapshot(f *testing.F) {
	data, err := New().Snapshot()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Fuzz(func(t *testing.T, bytes []byte) {
		_, _ = decodeSnapshot(bytes)
	})
}
