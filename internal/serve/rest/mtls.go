package rest

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Authenticating a workload by the certificate it presents.
//
// The case this exists for is a service mesh, or a deployment where the
// callers are services rather than people and the platform already issues
// each one a certificate. There, a bearer token is a second credential to
// mint, rotate and leak on top of one that already exists and is already
// rotated automatically.
//
// # Why this is a different authenticator rather than a flag
//
// A bearer token is presented per request and can be revoked by a deny
// list. A client certificate is presented per connection and is revoked by
// the issuer, which this engine cannot observe. Pretending they are the
// same thing behind one switch would make the second look like the first,
// and an operator would assume the revocation list covered both.
//
// It does not. The deny list still applies, because the identity a
// certificate resolves to is checked against it like any other, but a
// certificate revoked at the CA and not in the deny list keeps working
// until it expires. Short-lived certificates are the answer, which is what
// a mesh issues anyway.

// ClientCertAuth resolves a verified client certificate to an identity.
//
// Verification itself is done by crypto/tls before a request reaches here:
// this reads the identity out of a certificate the handshake already
// accepted. That split matters, because doing the verification here would
// mean a handler deciding whether to trust a chain, which is exactly the
// code nobody should hand-write.
type ClientCertAuth struct {
	// SubjectFrom decides which part of the certificate is the identity.
	// One of "cn", "dns" or "uri".
	SubjectFrom string
	// Groups, when set, reads group membership from the certificate's DNS
	// names beyond the first. A mesh that encodes roles that way can use
	// it; most will leave it off.
	GroupsFromSAN bool
}

// Subject sources.
const (
	SubjectFromCN  = "cn"
	SubjectFromDNS = "dns"
	SubjectFromURI = "uri"
)

// Authenticate reads the identity from the verified peer certificate.
func (c ClientCertAuth) Authenticate(r *http.Request) (govern.Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		// Not "no certificate was valid": no certificate was presented. The
		// caller's message stays vague either way, because distinguishing
		// them tells a prober how the server is configured.
		return govern.Identity{}, errors.New("no client certificate was presented")
	}
	// The leaf. VerifiedChains has already been checked by the handshake;
	// PeerCertificates[0] is the peer's own certificate.
	leaf := r.TLS.PeerCertificates[0]

	subject, err := subjectOf(leaf, c.SubjectFrom)
	if err != nil {
		return govern.Identity{}, err
	}
	id := govern.Identity{Subject: subject}
	if c.GroupsFromSAN && len(leaf.DNSNames) > 1 {
		id.Groups = append(id.Groups, leaf.DNSNames[1:]...)
	}
	// The serial is stable per certificate, so it is what a deny list
	// entry can name to revoke one workload's credential without revoking
	// the workload.
	id.TokenID = leaf.SerialNumber.String()
	return id, nil
}

// Name describes the configuration for health.
func (c ClientCertAuth) Name() string {
	return "mutual TLS (identity from the client certificate's " + c.SubjectFrom + ")"
}

func subjectOf(cert *x509.Certificate, from string) (string, error) {
	switch strings.ToLower(from) {
	case SubjectFromDNS, "":
		if len(cert.DNSNames) > 0 {
			return cert.DNSNames[0], nil
		}
		// Falling back to the common name rather than failing, because a
		// certificate with no SAN is old but not forged, and the handshake
		// already accepted it.
		if cert.Subject.CommonName != "" {
			return cert.Subject.CommonName, nil
		}
	case SubjectFromCN:
		if cert.Subject.CommonName != "" {
			return cert.Subject.CommonName, nil
		}
	case SubjectFromURI:
		// A SPIFFE ID lands here, which is what a mesh issues.
		if len(cert.URIs) > 0 {
			return cert.URIs[0].String(), nil
		}
	default:
		return "", fmt.Errorf("unknown certificate subject source %q", from)
	}
	return "", errors.New("the client certificate carries no usable identity")
}

// MutualTLSConfig builds a TLS config that requires and verifies a client
// certificate.
//
// RequireAndVerifyClientCert, not VerifyClientCertIfGiven. The weaker
// setting accepts a connection with no certificate at all and leaves the
// handler to notice, which is one forgotten check away from an
// unauthenticated caller reaching a governed query.
func MutualTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the server certificate: %w", err)
	}

	pem, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading the client CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf(
			"%s contains no PEM certificates. This file is the set of "+
				"authorities whose client certificates are accepted; an empty one "+
				"would accept nobody, and a wrong one would accept the wrong "+
				"callers", clientCAFile)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
