package transport

import (
	"context"
	"errors"
	"hash/crc32"
	"sync"
	"testing"
	"time"

	"viper/internal/raft"
)

type recordingPeerHandler struct {
	mu       sync.Mutex
	started  chan struct{}
	release  chan struct{}
	commands [][]byte
}

func (h *recordingPeerHandler) HandleRequestVote(_ context.Context, request raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	return raft.RequestVoteResponse{Term: request.Term, VoteGranted: true}, nil
}

func (h *recordingPeerHandler) HandleAppendEntries(ctx context.Context, request raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	if h.started != nil {
		select {
		case h.started <- struct{}{}:
		default:
		}
	}
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return raft.AppendEntriesResponse{}, ctx.Err()
		}
	}
	h.mu.Lock()
	for _, entry := range request.Entries {
		h.commands = append(h.commands, append([]byte(nil), entry.Command...))
	}
	h.mu.Unlock()
	return raft.AppendEntriesResponse{
		Term:       request.Term,
		Success:    true,
		MatchIndex: request.PrevLogIndex + raft.Index(len(request.Entries)),
	}, nil
}

func (h *recordingPeerHandler) HandleInstallSnapshot(_ context.Context, request raft.InstallSnapshotRequest) (raft.InstallSnapshotResponse, error) {
	return raft.InstallSnapshotResponse{Term: request.Term, NextOffset: request.Offset + uint64(len(request.Data))}, nil
}

func testCluster(t *testing.T, localID raft.NodeID) *StaticCluster {
	t.Helper()
	cluster, err := NewStaticCluster(localID, []Peer{
		{ID: "node-1", Address: "127.0.0.1:7001"},
		{ID: "node-2", Address: "127.0.0.1:7002"},
		{ID: "node-3", Address: "127.0.0.1:7003"},
	})
	if err != nil {
		t.Fatalf("create test cluster: %v", err)
	}
	return cluster
}

func TestStaticClusterFreezesMembershipAndQuorum(t *testing.T) {
	cluster := testCluster(t, "node-1")
	if cluster.Quorum() != 2 {
		t.Fatalf("quorum = %d, want 2", cluster.Quorum())
	}
	members := cluster.Members()
	members[0].Address = "changed"
	peer, ok := cluster.Peer("node-1")
	if !ok || peer.Address == "changed" {
		t.Fatal("membership was not isolated from returned peer data")
	}
	if len(cluster.RemotePeers()) != 2 || cluster.Contains("unknown") {
		t.Fatal("remote membership view is incorrect")
	}
}

func TestStaticClusterRejectsUnsafeMembership(t *testing.T) {
	base := []Peer{
		{ID: "node-1", Address: "127.0.0.1:7001"},
		{ID: "node-2", Address: "127.0.0.1:7002"},
		{ID: "node-3", Address: "127.0.0.1:7003"},
	}
	cases := []struct {
		name    string
		localID raft.NodeID
		peers   []Peer
	}{
		{name: "wrong size", localID: "node-1", peers: base[:2]},
		{name: "missing local", localID: "node-4", peers: base},
		{name: "duplicate id", localID: "node-1", peers: append(append([]Peer(nil), base...), Peer{ID: "node-1", Address: "127.0.0.1:7004", ServerName: "extra"})},
		{name: "duplicate address", localID: "node-1", peers: []Peer{{ID: "node-1", Address: "127.0.0.1:7001"}, {ID: "node-2", Address: "127.0.0.1:7001"}, {ID: "node-3", Address: "127.0.0.1:7003"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewStaticCluster(testCase.localID, testCase.peers); !errors.Is(err, ErrInvalidCluster) {
				t.Fatalf("NewStaticCluster error = %v, want ErrInvalidCluster", err)
			}
		})
	}
}

func TestInMemoryTransportCopiesRequestsAndRejectsInvalidPeers(t *testing.T) {
	network, err := NewInMemoryNetwork(testCluster(t, "node-1"), 2)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = network.Close() })
	handler := &recordingPeerHandler{}
	remoteCluster := testCluster(t, "node-2")
	if _, err = network.Bind(remoteCluster, handler); err != nil {
		t.Fatalf("bind peer: %v", err)
	}
	transport, err := network.Bind(testCluster(t, "node-1"), &recordingPeerHandler{})
	if err != nil {
		t.Fatalf("bind local peer: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })

	command := []byte("set:a=1")
	request := raft.AppendEntriesRequest{
		Term:         1,
		LeaderID:     "node-1",
		PrevLogIndex: 0,
		Entries:      []raft.Entry{{Index: 1, Term: 1, Command: command}},
	}
	response, err := transport.AppendEntries(context.Background(), "node-2", request)
	if err != nil || !response.Success {
		t.Fatalf("AppendEntries response = %#v, error = %v", response, err)
	}
	command[0] = 'X'
	handler.mu.Lock()
	gotCommand := string(handler.commands[0])
	handler.mu.Unlock()
	if gotCommand != "set:a=1" {
		t.Fatalf("handler observed aliased command %q", gotCommand)
	}
	if _, err = transport.AppendEntries(context.Background(), "unknown", request); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("unknown peer error = %v, want ErrUnknownPeer", err)
	}
	request.LeaderID = "node-2"
	if _, err = transport.AppendEntries(context.Background(), "node-2", request); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("spoofed sender error = %v, want ErrInvalidMessage", err)
	}
}

func TestInMemoryTransportBoundsConcurrentPeerWork(t *testing.T) {
	network, err := NewInMemoryNetwork(testCluster(t, "node-1"), 1)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = network.Close() })
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	if _, err = network.Bind(testCluster(t, "node-2"), &recordingPeerHandler{started: started, release: release}); err != nil {
		t.Fatalf("bind peer: %v", err)
	}
	transport, err := network.Bind(testCluster(t, "node-1"), &recordingPeerHandler{})
	if err != nil {
		t.Fatalf("bind local peer: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	request := raft.AppendEntriesRequest{Term: 1, LeaderID: "node-1"}
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := transport.AppendEntries(context.Background(), "node-2", request)
		firstDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first peer call did not reach the handler")
	}
	if _, err = transport.AppendEntries(context.Background(), "node-2", request); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("second concurrent call error = %v, want ErrOverloaded", err)
	}
	close(release)
	if err = <-firstDone; err != nil {
		t.Fatalf("first peer call error = %v", err)
	}
}

func TestSnapshotChunkChecksumValidation(t *testing.T) {
	data := []byte("snapshot chunk")
	request := raft.InstallSnapshotRequest{
		Term:              2,
		LeaderID:          "node-1",
		LastIncludedIndex: 1,
		LastIncludedTerm:  1,
		Data:              data,
		Checksum:          crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)),
	}
	if err := validateInstallSnapshot(testCluster(t, "node-2"), request); err != nil {
		t.Fatalf("valid snapshot chunk rejected: %v", err)
	}
	request.Data[0] ^= 1
	if err := validateInstallSnapshot(testCluster(t, "node-2"), request); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("corrupt snapshot chunk error = %v, want ErrInvalidMessage", err)
	}
}
