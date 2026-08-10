package checker

import "testing"

func FuzzDecodeHistory(f *testing.F) {
	seed, err := Encode(History{Version: HistoryVersion, Operations: []Operation{{
		ID: "op-1", Kind: OperationGet, Consistency: ConsistencyLinearizable, Key: []byte("key"), Invocation: 1, Completion: 2,
	}}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Decode(data)
	})
}
