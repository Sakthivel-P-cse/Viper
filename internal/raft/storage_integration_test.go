package raft_test

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"viper/internal/raft"
	"viper/internal/storage"
	"viper/internal/transport"
)

func TestThreeNodeClusterPersistsCommitAndReplaysAfterRestart(t *testing.T) {
	ids := []raft.NodeID{"node-1", "node-2", "node-3"}
	clock := raft.NewManualClock(time.Unix(0, 0))
	network, nodes, stores, directories, applied := openCluster(t, ids, clock)
	defer network.Close()

	clock.Advance(100 * time.Millisecond)
	if err := nodes[ids[0]].Tick(); err != nil {
		t.Fatalf("elect leader: %v", err)
	}
	if state := nodes[ids[0]].State(); state.Role != raft.RoleLeader || state.Term != 1 {
		t.Fatalf("leader state = %#v", state)
	}
	index, err := nodes[ids[0]].Propose([]byte("durable-command"))
	if err != nil || index != 1 {
		t.Fatalf("proposal index = %d, error = %v", index, err)
	}
	for _, id := range ids {
		if state := nodes[id].State(); state.CommitIndex != 1 || state.LastApplied != 1 {
			t.Fatalf("%s state = %#v", id, state)
		}
		if got := applied[id]; !reflect.DeepEqual(got, [][]byte{[]byte("durable-command")}) {
			t.Fatalf("%s applied commands = %q", id, got)
		}
	}

	for _, id := range ids {
		if err := nodes[id].Close(); err != nil {
			t.Fatalf("close node %s: %v", id, err)
		}
		if err := stores[id].Close(); err != nil {
			t.Fatalf("close store %s: %v", id, err)
		}
	}

	reopenedStore, err := storage.Open(directories[ids[0]])
	if err != nil {
		t.Fatalf("reopen node-1 store: %v", err)
	}
	var replayed [][]byte
	reopened, err := raft.New(raft.Config{
		ID: ids[0], Peers: ids, Clock: raft.NewManualClock(time.Unix(0, 0)), Store: reopenedStore,
		Apply: func(entry raft.Entry) error {
			replayed = append(replayed, append([]byte(nil), entry.Command...))
			return nil
		},
	})
	if err != nil {
		_ = reopenedStore.Close()
		t.Fatalf("reopen node-1: %v", err)
	}
	if state := reopened.State(); state.CommitIndex != 1 || state.LastApplied != 1 {
		t.Fatalf("reopened node state = %#v", state)
	}
	if !reflect.DeepEqual(replayed, [][]byte{[]byte("durable-command")}) {
		t.Fatalf("replayed commands = %q", replayed)
	}
	if err := reopened.SaveSnapshot(raft.Snapshot{Index: 1, Term: 1, Data: []byte("state-at-one")}); err != nil {
		t.Fatalf("compact reopened node: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened node: %v", err)
	}
	if err := reopenedStore.Close(); err != nil {
		t.Fatalf("close reopened store: %v", err)
	}

	verifiedStore, err := storage.Open(directories[ids[0]])
	if err != nil {
		t.Fatalf("reopen compacted store: %v", err)
	}
	var restored raft.Snapshot
	verified, err := raft.New(raft.Config{
		ID: ids[0], Peers: ids, Clock: raft.NewManualClock(time.Unix(0, 0)), Store: verifiedStore,
		Restore: func(snapshot raft.Snapshot) error { restored = snapshot; return nil },
	})
	if err != nil {
		_ = verifiedStore.Close()
		t.Fatalf("reopen compacted node: %v", err)
	}
	t.Cleanup(func() { _ = verified.Close(); _ = verifiedStore.Close() })
	if state := verified.State(); state.CommitIndex != 1 || state.LastApplied != 1 || state.LastLogIndex != 1 {
		t.Fatalf("compacted node state = %#v", state)
	}
	if restored.Index != 1 || string(restored.Data) != "state-at-one" {
		t.Fatalf("restored state = %#v", restored)
	}
}

func openCluster(t *testing.T, ids []raft.NodeID, clock *raft.ManualClock) (*transport.InMemoryNetwork, map[raft.NodeID]*raft.Node, map[raft.NodeID]*storage.Store, map[raft.NodeID]string, map[raft.NodeID][][]byte) {
	t.Helper()
	clusters := make(map[raft.NodeID]*transport.StaticCluster, len(ids))
	peers := make([]transport.Peer, len(ids))
	for i, id := range ids {
		peers[i] = transport.Peer{ID: id, Address: "memory://" + string(id)}
	}
	for _, id := range ids {
		cluster, err := transport.NewStaticCluster(id, peers)
		if err != nil {
			t.Fatalf("create cluster %s: %v", id, err)
		}
		clusters[id] = cluster
	}
	network, err := transport.NewInMemoryNetwork(clusters[ids[0]], 16)
	if err != nil {
		t.Fatalf("create memory network: %v", err)
	}
	nodes := make(map[raft.NodeID]*raft.Node, len(ids))
	stores := make(map[raft.NodeID]*storage.Store, len(ids))
	directories := make(map[raft.NodeID]string, len(ids))
	applied := make(map[raft.NodeID][][]byte, len(ids))
	var appliedMu sync.Mutex
	for _, id := range ids {
		directory := t.TempDir()
		directories[id] = directory
		store, err := storage.Open(directory)
		if err != nil {
			t.Fatalf("open store %s: %v", id, err)
		}
		stores[id] = store
		localID := id
		node, err := raft.New(raft.Config{
			ID: id, Peers: ids, Clock: clock, Store: store,
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 100 * time.Millisecond,
			HeartbeatInterval:  10 * time.Millisecond,
			RPCTimeout:         100 * time.Millisecond,
			Apply: func(entry raft.Entry) error {
				appliedMu.Lock()
				applied[localID] = append(applied[localID], append([]byte(nil), entry.Command...))
				appliedMu.Unlock()
				return nil
			},
		})
		if err != nil {
			t.Fatalf("create node %s: %v", id, err)
		}
		nodes[id] = node
	}
	for _, id := range ids {
		peerTransport, err := network.Bind(clusters[id], nodes[id])
		if err != nil {
			t.Fatalf("bind node %s: %v", id, err)
		}
		if err := nodes[id].AttachTransport(peerTransport); err != nil {
			t.Fatalf("attach transport %s: %v", id, err)
		}
	}
	return network, nodes, stores, directories, applied
}
