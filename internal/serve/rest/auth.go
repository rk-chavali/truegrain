package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"google.golang.org/api/idtoken"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Real authenticators.
//
// Everything this engine promises about governance rests on the identity being
// the caller's. A static token map is honest for a single tenant and useless
// for anything where two callers must be told apart, which is every deployment
// where the policy file matters.
//
// Verification is delegated to go-oidc and to Google's own verifier rather than
// hand written. Signature checking, discovery, key rotation and claim
// validation are where JWT authentication goes wrong, and a bespoke parser is
// the single most common way to accept a forged token.
//
// What these do not defend against: a stolen token is usable until it expires.
// A bearer token is a bearer token. Serve over TLS, keep lifetimes short, and
// read the audit log, which records the subject on every decision.

// allowedSigningAlgorithms pins verification to asymmetric signatures.
//
// This is the defence against algorithm confusion. Left open, a caller can
// present a token signed with HMAC using the provider's public key as the
// shared secret, and a verifier that trusts the header's alg will accept it.
// `none` is rejected for the obvious reason.
var allowedSigningAlgorithms = []string{
	oidc.RS256, oidc.RS384, oidc.RS512,
	oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512,
}

// OIDCOptions configures token verification.
type OIDCOptions struct {
	// Issuer is the provider's issuer URL, for example
	// https://accounts.google.com or https://acme.okta.com/oauth2/default.
	// Discovery reads the signing keys from it.
	Issuer string
	// Audience is the value this deployment requires in the token's aud claim.
	//
	// Required. Without it any token the issuer ever minted, for any
	// application, authenticates here. That is the most common and most
	// expensive OIDC misconfiguration, so an empty audience fails to start
	// rather than defaulting to accepting everything.
	Audience string
	// SubjectClaim names the claim that becomes govern.Identity.Subject.
	// Defaults to email, falling back to sub. Email is the default because it
	// is the form a warehouse grant and a policy file are written against.
	SubjectClaim string
	// GroupsClaim names the claim carrying group membership. Defaults to
	// "groups". A provider that does not emit it yields no groups, which
	// denies more than it allows and is the safe direction.
	GroupsClaim string
}

// OIDC authenticates bearer tokens against an OpenID Connect provider.
type OIDC struct {
	verifier     *oidc.IDTokenVerifier
	subjectClaim string
	groupsClaim  string
	issuer       string
}

// NewOIDC builds a verifier by discovering the issuer's configuration.
//
// Discovery happens once at startup so a misconfigured issuer fails loudly
// rather than on the first request. Key rotation is handled afterwards by the
// caching key set, which refetches when it sees an unknown key id.
func NewOIDC(ctx context.Context, opts OIDCOptions) (*OIDC, error) {
	if opts.Issuer == "" {
		return nil, errors.New("an OIDC issuer is required")
	}
	if opts.Audience == "" {
		return nil, errors.New(
			"an OIDC audience is required: without one, every token this issuer " +
				"has minted for any application would authenticate here")
	}
	if !strings.HasPrefix(opts.Issuer, "https://") {
		// Keys fetched over plaintext can be substituted in transit, which
		// makes the whole verification decorative.
		return nil, fmt.Errorf("the OIDC issuer must be https, got %q", opts.Issuer)
	}

	provider, err := oidc.NewProvider(ctx, opts.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering the OIDC provider at %s: %w", opts.Issuer, err)
	}
	verifier := provider.Verifier(&oidc.Config{
		ClientID:             opts.Audience,
		SupportedSigningAlgs: allowedSigningAlgorithms,
	})

	subject := opts.SubjectClaim
	if subject == "" {
		subject = "email"
	}
	groups := opts.GroupsClaim
	if groups == "" {
		groups = "groups"
	}
	return &OIDC{verifier: verifier, subjectClaim: subject, groupsClaim: groups, issuer: opts.Issuer}, nil
}

// Authenticate verifies the bearer token and returns the identity it carries.
func (o *OIDC) Authenticate(r *http.Request) (govern.Identity, error) {
	raw, err := bearerToken(r)
	if err != nil {
		return govern.Identity{}, err
	}

	// Verify checks the signature against the issuer's keys, and the issuer,
	// audience and expiry claims. Nothing reads a claim before this returns.
	token, err := o.verifier.Verify(r.Context(), raw)
	if err != nil {
		// The provider's message can name the audience it saw, which tells a
		// caller probing the endpoint how this deployment is configured.
		return govern.Identity{}, errors.New("the token was not accepted")
	}

	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return govern.Identity{}, errors.New("the token carried no readable claims")
	}
	return identityFromClaims(claims, token.Subject, o.subjectClaim, o.groupsClaim)
}

// Name describes the configuration for health.
func (o *OIDC) Name() string {
	return "oidc (" + o.issuer + ")"
}

// GoogleIDToken verifies Google-signed identity tokens.
//
// This is the authenticator for a deployment on Cloud Run, GKE or Compute
// Engine, where a calling workload gets a token from the metadata server and
// no OIDC client registration exists. It is also what
// `gcloud auth print-identity-token` produces, which makes it the quickest
// real authentication to stand up.
type GoogleIDToken struct {
	audience    string
	groupsClaim string
}

// NewGoogleIDToken requires the audience the calling workload requested.
func NewGoogleIDToken(audience, groupsClaim string) (*GoogleIDToken, error) {
	if audience == "" {
		return nil, errors.New(
			"a Google ID token audience is required: without one, any token Google " +
				"has ever signed would authenticate here")
	}
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	return &GoogleIDToken{audience: audience, groupsClaim: groupsClaim}, nil
}

// Authenticate validates the token against Google's public keys.
func (g *GoogleIDToken) Authenticate(r *http.Request) (govern.Identity, error) {
	raw, err := bearerToken(r)
	if err != nil {
		return govern.Identity{}, err
	}
	// idtoken.Validate checks the signature, issuer, audience and expiry
	// against Google's published keys, which it caches and rotates.
	payload, err := idtoken.Validate(r.Context(), raw, g.audience)
	if err != nil {
		return govern.Identity{}, errors.New("the token was not accepted")
	}

	claims := payload.Claims
	if claims == nil {
		claims = map[string]any{}
	}
	return identityFromClaims(claims, payload.Subject, "email", g.groupsClaim)
}

// Name describes the configuration for health.
func (g *GoogleIDToken) Name() string { return "google id token" }

// identityFromClaims maps verified claims onto the identity the gate resolves
// policy against.
//
// The subject is the string a policy file and a warehouse grant are written
// against, so it has to be the durable one. An email that the provider has not
// marked verified is refused: an unverified email claim is caller-supplied, and
// treating it as an identity would let anyone claim to be anyone.
func identityFromClaims(claims map[string]any, fallbackSubject, subjectClaim, groupsClaim string) (govern.Identity, error) {
	subject := fallbackSubject
	if v, ok := claims[subjectClaim].(string); ok && v != "" {
		if subjectClaim == "email" && !emailIsVerified(claims) {
			return govern.Identity{}, errors.New("the token was not accepted")
		}
		subject = v
	}
	if subject == "" {
		return govern.Identity{}, errors.New("the token identified no subject")
	}

	id := govern.Identity{
		Subject: subject,
		Groups:  stringsFrom(claims[groupsClaim]),
	}
	// jti, when the issuer mints one. Carried so a single leaked credential
	// can be revoked without taking down every token the workload holds.
	if v, ok := claims["jti"].(string); ok {
		id.TokenID = v
	}
	return id, nil
}

// emailIsVerified reads email_verified, treating an absent claim as verified.
//
// Google service account tokens carry email_verified true. Some providers omit
// it entirely for machine identities, and refusing those would make the
// authenticator unusable; what must never pass is an explicit false.
func emailIsVerified(claims map[string]any) bool {
	switch v := claims["email_verified"].(type) {
	case nil:
		return true
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// stringsFrom reads a claim that may be a list of strings or a single one.
func stringsFrom(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// maxTokenBytes bounds the Authorization header. A JWT carrying a large groups
// claim is a few kilobytes; anything past this is not a token.
const maxTokenBytes = 8 << 10

// bearerToken extracts the credential without logging or echoing it.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if len(header) > maxTokenBytes {
		return "", errors.New("the credential is too large to be a token")
	}
	// Scheme matching is case insensitive per RFC 7235, and clients differ.
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return "", errors.New("missing bearer token")
	}
	token := strings.TrimSpace(header[7:])
	if token == "" {
		return "", errors.New("missing bearer token")
	}
	return token, nil
}
