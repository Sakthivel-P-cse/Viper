package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"viper/internal/raft"
)

func TestPeerRPCBindsCertificateToClaimedNode(t *testing.T) {
	identity, _ := url.Parse("urn:viper:node:node-1")
	cert := &x509.Certificate{NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute), URIs: []*url.URL{identity}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	server := &peerRPCServer{cluster: testCluster(t, "node-3"), handler: &recordingPeerHandler{}}
	if _, err := server.RequestVote(ctx, requestVoteToProto(raft.RequestVoteRequest{Term: 1, CandidateID: "node-2"})); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("impersonated vote error = %v", err)
	}
	if _, err := server.RequestVote(ctx, requestVoteToProto(raft.RequestVoteRequest{Term: 1, CandidateID: "node-1"})); err != nil {
		t.Fatalf("own vote: %v", err)
	}
	if _, err := server.AppendEntries(ctx, appendEntriesToProto(raft.AppendEntriesRequest{Term: 1, LeaderID: "node-2"})); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("impersonated append error = %v", err)
	}
	if _, err := server.InstallSnapshot(ctx, installSnapshotToProto(raft.InstallSnapshotRequest{Term: 1, LeaderID: "node-2", LastIncludedIndex: 1, LastIncludedTerm: 1})); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("impersonated snapshot error = %v", err)
	}
	if _, err := server.RequestVote(context.Background(), requestVoteToProto(raft.RequestVoteRequest{Term: 1, CandidateID: "node-1"})); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing certificate error = %v", err)
	}
}
