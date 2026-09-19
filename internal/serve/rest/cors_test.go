package rest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// Cross-origin access widens who can read a response, so the tests are about
// who cannot rather than who can.

func corsHandler(t *testing.T, origins ...string) http.Handler {
	t.Helper()
	return server(t, false, govern.Identity{Subject: "tester"}).
		WithCORS(rest.NewCORS(origins)).
		Handler()
}

func TestOnlyNamedOriginsAreAnswered(t *testing.T) {
	h := corsHandler(t, "http://localhost:5180")

	allowed := do(t, h, "GET", "/v1/health", "", "")
	allowed.Result()
	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:5180")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5180" {
		t.Errorf("a named origin must be answered, got %q", got)
	}

	// An origin nobody named gets nothing, so the browser withholds the
	// response from the page.
	other := httptest.NewRequest("GET", "/v1/health", nil)
	other.Header.Set("Origin", "https://evil.example")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, other)
	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unnamed origin was allowed: %q", got)
	}
}

// TestNeverAWildcard. On an engine running without authentication, which is the
// normal local configuration, a wildcard would let any website the operator
// visits read their entire model.
func TestNeverAWildcard(t *testing.T) {
	h := corsHandler(t, "http://localhost:5180", "*")

	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" || got == "https://evil.example" {
		t.Errorf("a wildcard in the list must not allow arbitrary origins, got %q", got)
	}
}

// TestOriginIsNeverReflectedUnchecked: echoing whatever the browser sent is a
// wildcard with extra steps, and unlike a wildcard it keeps working when
// credentials are attached.
func TestOriginIsNeverReflectedUnchecked(t *testing.T) {
	h := corsHandler(t, "http://localhost:5180")

	for _, origin := range []string{
		"http://localhost:5180.evil.example",
		"http://localhost:51800",
		"https://localhost:5180",
		"null",
	} {
		req := httptest.NewRequest("GET", "/v1/health", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q must not be allowed, got %q", origin, got)
		}
	}
}

// TestCachesAreToldTheAnswerVaries, or a shared cache will hand one origin's
// permission to another.
func TestCachesAreToldTheAnswerVaries(t *testing.T) {
	h := corsHandler(t, "http://localhost:5180")
	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:5180")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("Vary") != "Origin" {
		t.Errorf("want Vary: Origin, got %q", w.Header().Get("Vary"))
	}
}

func TestPreflightIsAnswered(t *testing.T) {
	h := corsHandler(t, "http://localhost:5180")
	req := httptest.NewRequest("OPTIONS", "/v1/query", nil)
	req.Header.Set("Origin", "http://localhost:5180")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("want 204 from a preflight, got %d", w.Code)
	}
	// The clients send a bearer token and a user agent, and a browser asks
	// permission for both.
	allowed := w.Header().Get("Access-Control-Allow-Headers")
	for _, want := range []string{"Authorization", "Content-Type", "User-Agent"} {
		if !contains(allowed, want) {
			t.Errorf("a preflight must allow %s, got %q", want, allowed)
		}
	}
}

// TestOffUnlessConfigured. An engine nobody is browsing to has no reason to
// answer a cross-origin request at all.
func TestOffUnlessConfigured(t *testing.T) {
	h := server(t, false, govern.Identity{Subject: "tester"}).Handler()
	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:5180")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("cross-origin access must be off by default, got %q", got)
	}
}

// TestHealthEchoesTheCallerOnlyWhenAuthenticated.
//
// A caller cannot interpret a refusal without knowing which identity the
// engine resolved them to. It discloses nothing, because it tells a caller
// only who they already are.
func TestHealthEchoesTheCallerOnlyWhenAuthenticated(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "analyst@acme.com"})

	with := do(t, h, "GET", "/v1/health", "", token)
	if !contains(with.Body.String(), "analyst@acme.com") {
		t.Errorf("an authenticated caller should be told who they are:\n%s", with.Body)
	}

	// The key, not the word: the enforcement notes say "every caller can read
	// every column", which is a different use of it entirely.
	without := do(t, h, "GET", "/v1/health", "", "")
	if contains(without.Body.String(), `"caller"`) {
		t.Errorf("an unauthenticated request must be told nothing:\n%s", without.Body)
	}
}
