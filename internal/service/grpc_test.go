package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"
	"time"

	"viper/internal/security"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestAuthorizedUnaryInterceptorRequiresScopedTLSPrincipal(t *testing.T) {
	policy, err := security.NewPolicy(map[security.Principal][]security.Operation{
		{ID: "client-1", Kind: security.PrincipalClient}: {security.OperationRead},
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	interceptor := authorizedUnaryInterceptor(policy)
	handler := func(context.Context, any) (any, error) { return "ok", nil }
	if _, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/viper.api.v1.KVService/Get"}, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated request = %v, want Unauthenticated", err)
	}
	identity, err := url.Parse("urn:viper:client:client-1")
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{{NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute), URIs: []*url.URL{identity}}}}}})
	if response, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/viper.api.v1.KVService/Get"}, handler); err != nil || response != "ok" {
		t.Fatalf("authorized request = %#v, error = %v", response, err)
	}
	if _, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/viper.api.v1.KVService/Put"}, handler); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthorized mutation = %v, want PermissionDenied", err)
	}
}

func TestRequestRateLimiterBoundsBurst(t *testing.T) {
	limiter := newRequestRateLimiter(1)
	if !limiter.allow() {
		t.Fatal("first request was unexpectedly rate limited")
	}
	if limiter.allow() {
		t.Fatal("burst exceeded the configured request rate")
	}
}
