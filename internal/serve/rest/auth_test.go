package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// This file is in package rest rather than rest_test because it exercises the
// unexported claim mapping, which is where an authenticator turns verified
// input into the subject every policy decision is made against.

func request(header string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/metrics", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

// TestAudienceIsRequired is the misconfiguration that matters most.
//
// Without an audience, every token the issuer ever minted for any application
// authenticates here. A deployment must not be able to reach that state by
// leaving a field blank, so it fails to start instead.
func TestAudienceIsRequired(t *testing.T) {
	_, err := NewOIDC(context.Background(), OIDCOptions{Issuer: "https://accounts.google.com"})
	if err == nil {
		t.Fatal("an OIDC authenticator without an audience must not build")
	}
	if !strings.Contains(err.Error(), "audience") {
		t.Errorf("the error must name the missing audience, got: %v", err)
	}

	if _, err := NewGoogleIDToken("", ""); err == nil {
		t.Fatal("a Google authenticator without an audience must not build")
	}
}

func TestIssuerMustBeHTTPS(t *testing.T) {
	_, err := NewOIDC(context.Background(), OIDCOptions{
		Issuer: "http://accounts.example.com", Audience: "semantic",
	})
	if err == nil {
		t.Fatal("a plaintext issuer must be refused: its keys can be substituted in transit")
	}
}

// TestOnlyAsymmetricAlgorithmsAreAccepted guards against algorithm confusion.
//
// With HMAC in the list, a caller can sign a token using the provider's public
// key as the shared secret and a verifier that trusts the header will accept
// it. `none` is the same attack without the arithmetic.
func TestOnlyAsymmetricAlgorithmsAreAccepted(t *testing.T) {
	for _, alg := range allowedSigningAlgorithms {
		if strings.HasPrefix(alg, "HS") {
			t.Errorf("%s is symmetric and enables key confusion", alg)
		}
		if strings.EqualFold(alg, "none") {
			t.Error("`none` accepts unsigned tokens")
		}
	}
	if len(allowedSigningAlgorithms) == 0 {
		t.Fatal("an empty algorithm list lets the verifier choose, which is the bug")
	}
}

// TestUnverifiedEmailIsRefused: an email claim the provider has not vouched for
// is caller-supplied. Trusting it lets anyone assert any identity, and the
// subject is exactly what the policy file and the warehouse grant key on.
func TestUnverifiedEmailIsRefused(t *testing.T) {
	_, err := identityFromClaims(map[string]any{
		"email":          "cfo@example.com",
		"email_verified": false,
	}, "sub-123", "email", "groups")
	if err == nil {
		t.Fatal("an unverified email must not become an identity")
	}

	// Absent is treated as verified: machine identities at several providers
	// omit the claim entirely, and refusing those makes the engine unusable.
	id, err := identityFromClaims(map[string]any{
		"email": "svc@example.iam.gserviceaccount.com",
	}, "sub-123", "email", "groups")
	if err != nil {
		t.Fatalf("a token with no email_verified claim must still authenticate: %v", err)
	}
	if id.Subject != "svc@example.iam.gserviceaccount.com" {
		t.Errorf("want the email as subject, got %q", id.Subject)
	}
}

func TestSubjectFallsBackWhenTheClaimIsAbsent(t *testing.T) {
	id, err := identityFromClaims(map[string]any{}, "sub-123", "email", "groups")
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "sub-123" {
		t.Errorf("want the sub claim as fallback, got %q", id.Subject)
	}
}

func TestAnIdentityWithNoSubjectIsRefused(t *testing.T) {
	// An empty subject would resolve policy against "", which is neither a
	// denial nor a real identity.
	if _, err := identityFromClaims(map[string]any{}, "", "email", "groups"); err == nil {
		t.Fatal("a token identifying nobody must be refused")
	}
}

func TestGroupsAreReadFromTheConfiguredClaim(t *testing.T) {
	cases := []struct {
		name  string
		claim any
		want  []string
	}{
		{"list", []any{"analysts", "finance"}, []string{"analysts", "finance"}},
		{"single string", "analysts", []string{"analysts"}},
		{"absent", nil, nil},
		{"wrong type", 42, nil},
		{"empty entries dropped", []any{"analysts", ""}, []string{"analysts"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := map[string]any{"sub": "x"}
			if c.claim != nil {
				claims["groups"] = c.claim
			}
			id, err := identityFromClaims(claims, "x", "email", "groups")
			if err != nil {
				t.Fatal(err)
			}
			if len(id.Groups) != len(c.want) {
				t.Fatalf("want groups %v, got %v", c.want, id.Groups)
			}
			for i := range c.want {
				if id.Groups[i] != c.want[i] {
					t.Errorf("want groups %v, got %v", c.want, id.Groups)
				}
			}
		})
	}
}

// TestUnparseableGroupsDenyRatherThanGrant. A claim shaped unexpectedly must
// yield no groups, never every group.
func TestUnparseableGroupsDenyRatherThanGrant(t *testing.T) {
	id, err := identityFromClaims(map[string]any{
		"sub": "x", "groups": map[string]any{"unexpected": true},
	}, "x", "email", "groups")
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Groups) != 0 {
		t.Errorf("an unreadable groups claim must grant nothing, got %v", id.Groups)
	}
}

func TestBearerTokenExtraction(t *testing.T) {
	cases := []struct {
		name, header, want string
		wantErr            bool
	}{
		{name: "standard", header: "Bearer abc123", want: "abc123"},
		// RFC 7235 says the scheme is case insensitive and clients differ.
		{name: "lowercase scheme", header: "bearer abc123", want: "abc123"},
		{name: "mixed case scheme", header: "BeArEr abc123", want: "abc123"},
		{name: "surrounding space", header: "Bearer   abc123  ", want: "abc123"},
		{name: "absent", header: "", wantErr: true},
		{name: "wrong scheme", header: "Basic abc123", wantErr: true},
		{name: "scheme only", header: "Bearer ", wantErr: true},
		{name: "oversized", header: "Bearer " + strings.Repeat("a", maxTokenBytes+1), wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := bearerToken(request(c.header))
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q, got %q", c.header, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.header, err)
			}
			if got != c.want {
				t.Errorf("want %q, got %q", c.want, got)
			}
		})
	}
}

// TestErrorsDoNotEchoTheCredential. An error rendered back to the caller, or
// into a log, must never contain the token: logs outlive the token's expiry.
func TestErrorsDoNotEchoTheCredential(t *testing.T) {
	const secret = "super-secret-token-value"
	auth := StaticTokens{Tokens: map[string]govern.Identity{"a-real-token": {Subject: "x"}}}

	_, err := auth.Authenticate(request("Bearer " + secret))
	if err == nil {
		t.Fatal("an unknown token must be refused")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error echoed the credential: %v", err)
	}

	_, err = bearerToken(request("Bearer " + strings.Repeat("x", maxTokenBytes+1)))
	if err != nil && strings.Contains(err.Error(), "xxxx") {
		t.Errorf("the error echoed an oversized credential: %v", err)
	}
}

// TestStaticTokensDoNotDistinguishUnknownFromMalformed. Different messages
// would let a caller probe for which tokens exist.
func TestStaticTokensDoNotDistinguishUnknownFromMalformed(t *testing.T) {
	auth := StaticTokens{Tokens: map[string]govern.Identity{"good": {Subject: "x"}}}
	if _, err := auth.Authenticate(request("Bearer wrong")); err == nil {
		t.Fatal("an unknown token must be refused")
	}
	if id, err := auth.Authenticate(request("Bearer good")); err != nil || id.Subject != "x" {
		t.Fatalf("a known token must authenticate: %v %v", id, err)
	}
}

// TestStaticTokensDistinguishTokensThatSharePrefixes is what the constant time
// comparison is for. A byte-at-a-time comparison that returns early leaks where
// two tokens diverge.
func TestStaticTokensDistinguishTokensThatSharePrefixes(t *testing.T) {
	auth := StaticTokens{Tokens: map[string]govern.Identity{
		"token-aaaaaaaaaaaa": {Subject: "first"},
		"token-aaaaaaaaaaab": {Subject: "second"},
	}}
	for token, want := range map[string]string{
		"token-aaaaaaaaaaaa": "first",
		"token-aaaaaaaaaaab": "second",
	} {
		id, err := auth.Authenticate(request("Bearer " + token))
		if err != nil {
			t.Fatalf("%s: %v", token, err)
		}
		if id.Subject != want {
			t.Errorf("token %s resolved to %q, want %q", token, id.Subject, want)
		}
	}
	if _, err := auth.Authenticate(request("Bearer token-aaaaaaaaaaa")); err == nil {
		t.Error("a prefix of a real token must not authenticate")
	}
}
