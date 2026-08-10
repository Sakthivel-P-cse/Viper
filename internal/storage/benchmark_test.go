package storage

import (
	"testing"

	"viper/internal/raft"
)

func BenchmarkStorePersist(b *testing.B) {
	store, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for index := 1; index <= b.N; index++ {
		term := raft.Term(1)
		state := raft.HardState{CurrentTerm: term, VotedFor: "node-1"}
		if err := store.Persist(raft.PersistBatch{
			HardState: &state,
			Entries:   []raft.Entry{{Index: raft.Index(index), Term: term, Command: []byte("benchmark-command")}},
		}); err != nil {
			b.Fatal(err)
		}
	}
}
