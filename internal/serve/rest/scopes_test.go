package rest_test

import (
	"net/http"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// What a credential may do, as opposed to what an identity may read.
//
// Groups and policy decide which columns are legible. A scope decides whether
// this token may make the warehouse do work at all. Until scopes existed a
// continuous integration job that only validated a model held a credential
// that could read every row its identity was allowed.

func scopedHandler(t *testing.T, scopes ...string) http.Handler {
	t.Helper()
	return server(t, false, govern.Identity{
		Subject: "ci@acme.com",
		Scopes:  scopes,
	}).Handler()
}

const scopedQuery = `{"metrics":["sales.order_revenue"]}`

// TestAReadOnlyCredentialCannotQuery is the whole point.
func TestAReadOnlyCredentialCannotQuery(t *testing.T) {
	h := scopedHandler(t, rest.ScopeReadModel)

	// Reading the model is what it is for.
	if res := do(t, h, "GET", "/v1/metrics", "", token); res.Code != http.StatusOK {
		t.Errorf("a read:model credential must read the model, got %d: %s", res.Code, res.Body)
	}
	if res := do(t, h, "POST", "/v1/compile", scopedQuery, token); res.Code == http.StatusForbidden {
		t.Errorf("compiling reads the model and does not touch the warehouse: %s", res.Body)
	}

	// Making the warehouse work is not.
	for _, call := range []struct{ method, path string }{
		{"POST", "/v1/query"},
		{"POST", "/v1/jobs"},
	} {
		res := do(t, h, call.method, call.path, scopedQuery, token)
		if res.Code != http.StatusForbidden {
			t.Errorf("%s %s should be refused for a read-only credential, got %d: %s",
				call.method, call.path, res.Code, res.Body)
		}
		if !contains(res.Body.String(), rest.CodeOutOfScope) {
			t.Errorf("the refusal should name the reason:\n%s", res.Body)
		}
	}
}

// TestOutOfScopeIsForbiddenNotUnauthorized.
//
// The credential was accepted and is not permitted, which is different from
// not being recognised. Answering 401 tells a caller to get a new token, and a
// new token of the same kind will fail identically.
func TestOutOfScopeIsForbiddenNotUnauthorized(t *testing.T) {
	h := scopedHandler(t, rest.ScopeReadModel)
	res := do(t, h, "POST", "/v1/query", scopedQuery, token)

	if res.Code == http.StatusUnauthorized {
		t.Error("an accepted credential that may not do this is 403, not 401")
	}
	if res.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", res.Code)
	}
	if res.Header().Get("WWW-Authenticate") != "" {
		t.Error("nothing about re-authenticating helps here")
	}
}

// TestAnUnscopedCredentialKeepsEverything.
//
// Scopes are opt in. Every deployment before them had one token that could do
// anything, and an upgrade that silently revoked access would be worse than
// the problem being solved.
func TestAnUnscopedCredentialKeepsEverything(t *testing.T) {
	h := scopedHandler(t) // no scopes at all

	for _, call := range []struct{ method, path, body string }{
		{"GET", "/v1/metrics", ""},
		{"POST", "/v1/compile", scopedQuery},
		{"POST", "/v1/query", scopedQuery},
	} {
		res := do(t, h, call.method, call.path, call.body, token)
		if res.Code == http.StatusForbidden {
			t.Errorf("%s %s was refused for an unscoped credential: %s", call.method, call.path, res.Body)
		}
	}
}

// TestAQueryCredentialIsNotAlsoAModelReader.
//
// The list is exhaustive once anything is named. A credential granted only
// run:query has not been granted read:model by implication, because "it can
// already run queries so it may as well read metadata" is the reasoning that
// turns a scope system back into one token.
func TestAQueryCredentialIsNotAlsoAModelReader(t *testing.T) {
	h := scopedHandler(t, rest.ScopeRunQuery)

	if res := do(t, h, "GET", "/v1/metrics", "", token); res.Code != http.StatusForbidden {
		t.Errorf("run:query does not imply read:model, got %d", res.Code)
	}
	if res := do(t, h, "POST", "/v1/query", scopedQuery, token); res.Code == http.StatusForbidden {
		t.Errorf("run:query must be able to run a query: %s", res.Body)
	}
}

// TestHealthIsAlwaysReadable. An operator needs to know what a deployment
// enforces before they hold a credential for it, which is why health is
// unauthenticated in the first place.
func TestHealthIsAlwaysReadable(t *testing.T) {
	h := scopedHandler(t, rest.ScopeRunQuery)
	if res := do(t, h, "GET", "/v1/health", "", ""); res.Code != http.StatusOK {
		t.Errorf("health must answer whatever the credential holds, got %d", res.Code)
	}
}

func TestCanIsExhaustiveOnceAnythingIsNamed(t *testing.T) {
	unbounded := govern.Identity{Subject: "a@b.c"}
	if !unbounded.Can(rest.ScopeRunQuery) || !unbounded.Can("anything at all") {
		t.Error("no scopes means unbounded")
	}

	narrow := govern.Identity{Subject: "a@b.c", Scopes: []string{rest.ScopeReadModel}}
	if !narrow.Can(rest.ScopeReadModel) {
		t.Error("a named scope must be permitted")
	}
	if narrow.Can(rest.ScopeRunQuery) {
		t.Error("naming one scope must not grant another")
	}
}

// TestATypoIsReportedRatherThanSilentlyNarrowing.
//
// Dropping an unrecognised scope would produce a credential narrower than
// intended, and the failure would surface far from the mistake looking like a
// permissions problem.
func TestATypoIsReportedRatherThanSilentlyNarrowing(t *testing.T) {
	scopes, unknown := rest.ParseScopes("read:model, run:querry")

	if len(unknown) != 1 || unknown[0] != "run:querry" {
		t.Errorf("the typo should be reported, got %v", unknown)
	}
	if len(scopes) != 1 || scopes[0] != rest.ScopeReadModel {
		t.Errorf("the valid scope should survive, got %v", scopes)
	}

	empty, none := rest.ParseScopes("  ,  ")
	if len(empty) != 0 || len(none) != 0 {
		t.Errorf("an empty list is unbounded, got %v %v", empty, none)
	}
}
