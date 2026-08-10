package security

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// PrincipalKind identifies the authentication domain of a request.
type PrincipalKind uint8

const (
	PrincipalUnknown PrincipalKind = iota
	PrincipalPeer
	PrincipalClient
	PrincipalAdmin
)

// Operation names the bounded authorization surface.
type Operation string

const (
	OperationRead      Operation = "read"
	OperationMutate    Operation = "mutate"
	OperationPeerRPC   Operation = "peer_rpc"
	OperationLiveness  Operation = "liveness"
	OperationReadiness Operation = "readiness"
	OperationAdmin     Operation = "admin"
)

// Principal is the authenticated identity presented to an authorization
// policy. IDs are safe opaque identifiers and never contain request data.
type Principal struct {
	ID   string
	Kind PrincipalKind
}

// Policy is an immutable least-privilege authorization policy.
type Policy struct {
	permissions map[Principal]map[Operation]struct{}
}

// NewPolicy validates and freezes explicit identity permissions.
func NewPolicy(permissions map[Principal][]Operation) (*Policy, error) {
	policy := &Policy{permissions: make(map[Principal]map[Operation]struct{}, len(permissions))}
	for principal, operations := range permissions {
		if !validIdentity(principal.ID) || principal.Kind == PrincipalUnknown {
			return nil, fmt.Errorf("invalid principal")
		}
		allowed := make(map[Operation]struct{}, len(operations))
		for _, operation := range operations {
			if !validOperation(operation) {
				return nil, fmt.Errorf("invalid operation")
			}
			allowed[operation] = struct{}{}
		}
		policy.permissions[principal] = allowed
	}
	return policy, nil
}

// Authorize checks one operation without exposing policy internals.
func (p *Policy) Authorize(principal Principal, operation Operation) error {
	if p == nil || !validIdentity(principal.ID) || principal.Kind == PrincipalUnknown || !validOperation(operation) {
		return ErrPermissionDenied
	}
	allowed, exists := p.permissions[principal]
	if !exists {
		return ErrPermissionDenied
	}
	if _, exists = allowed[operation]; !exists {
		return ErrPermissionDenied
	}
	return nil
}

// ErrPermissionDenied is intentionally generic to avoid identity disclosure.
var ErrPermissionDenied = errors.New("permission denied")

func validOperation(operation Operation) bool {
	switch operation {
	case OperationRead, OperationMutate, OperationPeerRPC, OperationLiveness, OperationReadiness, OperationAdmin:
		return true
	default:
		return false
	}
}

func validIdentity(identity string) bool {
	if identity == "" || len(identity) > 128 {
		return false
	}
	for _, character := range identity {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character)) {
			return false
		}
	}
	return true
}

// PeerCertificateIdentity extracts the node ID from a peer certificate. The
// certificate must contain exactly one URI SAN of the form urn:viper:node:<id>
// or an exact DNS SAN matching the expected node ID.
func PeerCertificateIdentity(certificate *x509.Certificate, expectedNodeID string) error {
	if certificate == nil || !validIdentity(expectedNodeID) {
		return ErrPeerIdentity
	}
	if !certificate.NotBefore.Before(time.Now().Add(time.Second)) || certificate.NotAfter.Before(time.Now().Add(-time.Second)) {
		return ErrPeerIdentity
	}
	for _, name := range certificate.DNSNames {
		if name == expectedNodeID {
			return nil
		}
	}
	wantURI := "urn:viper:node:" + expectedNodeID
	for _, identity := range certificate.URIs {
		if identity != nil && identity.String() == wantURI {
			return nil
		}
	}
	return ErrPeerIdentity
}

// PrincipalFromCertificate maps an explicitly scoped Viper URI SAN to a
// least-privilege principal. Public client and admin identities must use a
// URI; an arbitrary DNS name is never treated as an authorization identity.
func PrincipalFromCertificate(certificate *x509.Certificate) (Principal, error) {
	if certificate == nil || !certificateValid(certificate) {
		return Principal{}, ErrPeerIdentity
	}
	var principal Principal
	for _, identity := range certificate.URIs {
		if identity == nil {
			continue
		}
		kind, prefix := PrincipalUnknown, ""
		switch {
		case strings.HasPrefix(identity.Opaque, "viper:node:"):
			kind, prefix = PrincipalPeer, "viper:node:"
		case strings.HasPrefix(identity.Opaque, "viper:client:"):
			kind, prefix = PrincipalClient, "viper:client:"
		case strings.HasPrefix(identity.Opaque, "viper:admin:"):
			kind, prefix = PrincipalAdmin, "viper:admin:"
		default:
			continue
		}
		id := strings.TrimPrefix(identity.Opaque, prefix)
		if !validIdentity(id) {
			return Principal{}, ErrPeerIdentity
		}
		candidate := Principal{ID: id, Kind: kind}
		if principal != (Principal{}) && principal != candidate {
			return Principal{}, ErrPeerIdentity
		}
		principal = candidate
	}
	if principal == (Principal{}) {
		return Principal{}, ErrPeerIdentity
	}
	return principal, nil
}

// ErrPeerIdentity indicates that a valid certificate is not authorized for a
// configured node identity.
var ErrPeerIdentity = errors.New("peer certificate identity is not authorized")

// PeerTLSConfig creates a mutually authenticated TLS 1.3 configuration. The
// caller supplies the certificate and trust roots; node identity is checked by
// VerifyPeerCertificate against the expected peer ID.
func PeerTLSConfig(certificate tls.Certificate, roots *x509.CertPool, expectedPeerID string, server bool) (*tls.Config, error) {
	if len(certificate.Certificate) == 0 || roots == nil || !validIdentity(expectedPeerID) {
		return nil, fmt.Errorf("invalid peer TLS configuration")
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      roots,
		ClientCAs:    roots,
		ServerName:   "",
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	config.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrPeerIdentity
		}
		certificate, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return ErrPeerIdentity
		}
		if !server {
			if _, err = verifyPeerChain(certificate, rawCerts[1:], roots); err != nil {
				return ErrPeerIdentity
			}
		} else if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
			return ErrPeerIdentity
		}
		if err := PeerCertificateIdentity(certificate, expectedPeerID); err != nil {
			return err
		}
		return nil
	}
	if !server {
		// URI SANs carry the node identity and are intentionally not used as a
		// DNS hostname. The custom callback above performs full chain
		// verification and exact identity authorization.
		config.InsecureSkipVerify = true // #nosec G402 -- VerifyPeerCertificate verifies roots and identity.
	}
	return config, nil
}

// PeerServerTLSConfig creates a server-side peer configuration that accepts
// only valid peer certificates whose scoped node identity is a member of the
// supplied static cluster.
func PeerServerTLSConfig(certificate tls.Certificate, roots *x509.CertPool, memberIDs []string) (*tls.Config, error) {
	if len(certificate.Certificate) == 0 || roots == nil || len(memberIDs) == 0 {
		return nil, fmt.Errorf("invalid peer server TLS configuration")
	}
	allowed := make(map[string]struct{}, len(memberIDs))
	for _, memberID := range memberIDs {
		if !validIdentity(memberID) {
			return nil, fmt.Errorf("invalid peer member identity")
		}
		allowed[memberID] = struct{}{}
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    roots,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	config.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(rawCerts) == 0 || len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
			return ErrPeerIdentity
		}
		certificate, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return ErrPeerIdentity
		}
		principal, err := PrincipalFromCertificate(certificate)
		if err != nil || principal.Kind != PrincipalPeer {
			return ErrPeerIdentity
		}
		if _, ok := allowed[principal.ID]; !ok {
			return ErrPeerIdentity
		}
		return nil
	}
	return config, nil
}

func certificateValid(certificate *x509.Certificate) bool {
	now := time.Now()
	return !now.Before(certificate.NotBefore.Add(-time.Second)) && !now.After(certificate.NotAfter.Add(time.Second))
}

func verifyPeerChain(leaf *x509.Certificate, rawIntermediates [][]byte, roots *x509.CertPool) ([][]*x509.Certificate, error) {
	intermediates := x509.NewCertPool()
	for _, raw := range rawIntermediates {
		certificate, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, err
		}
		intermediates.AddCert(certificate)
	}
	return leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
}

// ParsePeerURI parses the explicit Viper node URI used in certificate SANs.
func ParsePeerURI(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "urn" || parsed.Opaque == "" || !strings.HasPrefix(parsed.Opaque, "viper:node:") {
		return "", ErrPeerIdentity
	}
	identity := strings.TrimPrefix(parsed.Opaque, "viper:node:")
	if !validIdentity(identity) {
		return "", ErrPeerIdentity
	}
	return identity, nil
}
