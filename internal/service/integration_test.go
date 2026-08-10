package service_test

import (
	"context"
	"testing"
	"time"

	apiv1 "viper/api/gen/v1"
	"viper/internal/raft"
	"viper/internal/service"
	"viper/internal/statemachine"
	"viper/internal/transport"
)

func TestReplicatedKVClusterServicePath(t *testing.T) {
	ids := []raft.NodeID{"node-1", "node-2", "node-3"}
	clock := raft.NewManualClock(time.Unix(0, 0))
	peers := []transport.Peer{{ID: ids[0], Address: "memory://node-1"}, {ID: ids[1], Address: "memory://node-2"}, {ID: ids[2], Address: "memory://node-3"}}
	clusters := make(map[raft.NodeID]*transport.StaticCluster, len(ids))
	for _, id := range ids {
		cluster, err := transport.NewStaticCluster(id, peers)
		if err != nil {
			t.Fatalf("create cluster %s: %v", id, err)
		}
		clusters[id] = cluster
	}
	network, err := transport.NewInMemoryNetwork(clusters[ids[0]], 16)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	defer network.Close()
	nodes := make(map[raft.NodeID]*raft.Node, len(ids))
	servers := make(map[raft.NodeID]*service.KVServer, len(ids))
	for _, id := range ids {
		node, nodeErr := raft.New(raft.Config{
			ID: id, Peers: ids, Clock: clock,
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 100 * time.Millisecond,
			HeartbeatInterval:  10 * time.Millisecond,
			RPCTimeout:         100 * time.Millisecond,
		})
		if nodeErr != nil {
			t.Fatalf("create node %s: %v", id, nodeErr)
		}
		nodes[id] = node
		server, serverErr := service.NewKVServer(node, statemachine.New())
		if serverErr != nil {
			t.Fatalf("create service %s: %v", id, serverErr)
		}
		servers[id] = server
	}
	for _, id := range ids {
		peerTransport, bindErr := network.Bind(clusters[id], nodes[id])
		if bindErr != nil {
			t.Fatalf("bind node %s: %v", id, bindErr)
		}
		if attachErr := nodes[id].AttachTransport(peerTransport); attachErr != nil {
			t.Fatalf("attach node %s: %v", id, attachErr)
		}
	}
	for _, node := range nodes {
		t.Cleanup(func() { _ = node.Close() })
	}
	clock.Advance(100 * time.Millisecond)
	if err := nodes[ids[0]].Tick(); err != nil {
		t.Fatalf("elect leader: %v", err)
	}
	if nodes[ids[0]].State().Role != raft.RoleLeader {
		t.Fatalf("node-1 did not become leader: %#v", nodes[ids[0]].State())
	}

	putRequest := &apiv1.PutRequest{ClientId: "client-1", RequestId: "request-1", Key: []byte("key"), Value: []byte("one")}
	put, err := servers[ids[0]].Put(context.Background(), putRequest)
	if err != nil || put.GetRevision() != 1 {
		t.Fatalf("Put = %#v, error = %v", put, err)
	}
	retry, err := servers[ids[0]].Put(context.Background(), putRequest)
	if err != nil || retry.GetRevision() != 1 {
		t.Fatalf("retry Put = %#v, error = %v", retry, err)
	}
	cas, err := servers[ids[0]].CompareAndSwap(context.Background(), &apiv1.CompareAndSwapRequest{
		ClientId: "client-1", RequestId: "request-2", Key: []byte("key"), ExpectedValue: []byte("one"), ExpectedPresent: true, NewValue: []byte("two"),
	})
	if err != nil || !cas.GetSwapped() || cas.GetRevision() != 2 {
		t.Fatalf("CAS = %#v, error = %v", cas, err)
	}
	stale, err := servers[ids[1]].Get(context.Background(), &apiv1.GetRequest{Key: []byte("key"), Consistency: apiv1.ConsistencyMode_CONSISTENCY_MODE_STALE})
	if err != nil || !stale.GetFound() || string(stale.GetValue()) != "two" || stale.GetRevision() != 2 {
		t.Fatalf("follower stale Get = %#v, error = %v", stale, err)
	}
	linearizable, err := servers[ids[0]].Get(context.Background(), &apiv1.GetRequest{Key: []byte("key"), Consistency: apiv1.ConsistencyMode_CONSISTENCY_MODE_LINEARIZABLE})
	if err != nil || !linearizable.GetFound() || string(linearizable.GetValue()) != "two" || linearizable.GetRevision() != 2 {
		t.Fatalf("leader linearizable Get = %#v, error = %v", linearizable, err)
	}
}
