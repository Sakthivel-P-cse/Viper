package security

import (
	"crypto/x509"
	"net/url"
	"testing"
	"time"
)

func TestPolicyUsesExplicitLeastPrivilege(t *testing.T) {
	policy, err := NewPolicy(map[Principal][]Operation{
		{ID: "client-1", Kind: PrincipalClient}: {OperationRead, OperationMutate},
		{ID: "node-1", Kind: PrincipalPeer}:     {OperationPeerRPC},
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if err = policy.Authorize(Principal{ID: "client-1", Kind: PrincipalClient}, OperationRead); err != nil {
		t.Fatalf("authorized read rejected: %v", err)
	}
	if err = policy.Authorize(Principal{ID: "client-1", Kind: PrincipalClient}, OperationAdmin); err != ErrPermissionDenied {
		t.Fatalf("unauthorized admin = %v, want ErrPermissionDenied", err)
	}
	if err = policy.Authorize(Principal{ID: "unknown", Kind: PrincipalClient}, OperationRead); err != ErrPermissionDenied {
		t.Fatalf("unknown client = %v, want ErrPermissionDenied", err)
	}
}

func TestPeerURIParsingIsBounded(t *testing.T) {
	identity, err := ParsePeerURI("urn:viper:node:node-1")
	if err != nil || identity != "node-1" {
		t.Fatalf("parsed identity = %q, error = %v", identity, err)
	}
	if _, err = ParsePeerURI("https://example.invalid/node-1"); err != ErrPeerIdentity {
		t.Fatalf("invalid peer URI = %v, want ErrPeerIdentity", err)
	}
}

func TestCertificatePrincipalRequiresScopedURI(t *testing.T) {
	identity, err := url.Parse("urn:viper:client:client-1")
	if err != nil {
		t.Fatalf("parse client identity: %v", err)
	}
	certificate := &x509.Certificate{
		NotBefore: time.Now().Add(-time.Minute),
		NotAfter:  time.Now().Add(time.Minute),
		URIs:      []*url.URL{identity},
	}
	principal, err := PrincipalFromCertificate(certificate)
	if err != nil || principal != (Principal{ID: "client-1", Kind: PrincipalClient}) {
		t.Fatalf("principal = %#v, error = %v", principal, err)
	}
	certificate.URIs = nil
	certificate.DNSNames = []string{"client-1"}
	if _, err = PrincipalFromCertificate(certificate); err != ErrPeerIdentity {
		t.Fatalf("unscoped DNS identity = %v, want ErrPeerIdentity", err)
	}
}
