package rest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// Token rotation, from the outside.
//
// govern's own tests prove the file reloads. These prove the REST surface
// is wired to it: that a rotated token reaches a real request, that the
// identity the file names is the identity policy sees, and that health
// tells an operator where the rotation stands without telling anybody what
// the tokens are.

func credentialsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// rotatingHandler is the fixture model behind a credential file rather
// than behind a fixed token.
func rotatingHandler(t *testing.T, path string) http.Handler {
	t.Helper()
	creds, err := govern.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "..", "testdata", "models", "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rest.New(eng, rest.RotatingTokens{Credentials: creds}).Handler()
}

// TestARotatedTokenReachesARealRequest.
//
// The end-to-end version of the overlap property: both tokens answer a
// governed route while both are in the file, which is the window a client
// rolls over in.
func TestARotatedTokenReachesARealRequest(t *testing.T) {
	// Both entries are in the file from the start, which is the state a
	// rotation passes through. That the file reloads into that state
	// without a restart is govern's own test; this one is about the two
	// tokens both reaching a governed route.
	path := credentialsFile(t, `
version: 1
tokens:
  - id: old
    token: old-secret
    subject: analyst@example.com
  - id: new
    token: new-secret
    subject: analyst@example.com
`)
	h := rotatingHandler(t, path)

	for _, token := range []string{"old-secret", "new-secret"} {
		if code := do(t, h, "GET", "/v1/metrics", "", token).Code; code != http.StatusOK {
			t.Errorf("token %q got %d, want 200 while both are in the file", token, code)
		}
	}
	if code := do(t, h, "GET", "/v1/metrics", "", "never-issued").Code; code == http.StatusOK {
		t.Error("an unknown token was accepted")
	}
}

// TestTheIdentityComesFromTheFileNotTheFlag.
//
// A rotating credential is only useful if the identity travels with it. Two
// tokens in one file naming two subjects have to produce two subjects, or
// the audit log cannot tell a rotated service from a second one.
func TestTheIdentityComesFromTheFileNotTheFlag(t *testing.T) {
	path := credentialsFile(t, `
version: 1
tokens:
  - id: bi
    token: bi-secret
    subject: bi@example.com
    groups: [analysts]
  - id: ci
    token: ci-secret
    subject: ci@example.com
`)
	h := rotatingHandler(t, path)

	for token, want := range map[string]string{
		"bi-secret": "bi@example.com",
		"ci-secret": "ci@example.com",
	} {
		rec := do(t, h, "GET", "/v1/health", "", token)
		var body struct {
			Caller struct {
				Subject string `json:"subject"`
			} `json:"caller"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Caller.Subject != want {
			t.Errorf("token %q authenticated as %q, want %q", token, body.Caller.Subject, want)
		}
	}
}

// TestHealthReportsTheRotationWithoutReportingTheTokens.
//
// An operator halfway through a rotation wants to watch the live count go
// back to one. Reading the secret mount to check is the thing this saves
// them, so the endpoint has to say enough to be useful and nothing more.
func TestHealthReportsTheRotationWithoutReportingTheTokens(t *testing.T) {
	path := credentialsFile(t, `
version: 1
tokens:
  - id: old
    token: old-secret
    subject: a@example.com
  - id: new
    token: new-secret
    subject: a@example.com
  - id: retired
    token: retired-secret
    subject: a@example.com
    not_after: 2020-01-01T00:00:00Z
`)
	h := rotatingHandler(t, path)

	rec := do(t, h, "GET", "/v1/health", "", "new-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, secret := range []string{"old-secret", "new-secret", "retired-secret"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("health disclosed a bearer token: %s", rec.Body)
		}
	}

	var body struct {
		Credentials struct {
			Source    string `json:"source"`
			ActiveNow int    `json:"active_now"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Credentials.ActiveNow != 2 {
		t.Errorf("active_now = %d, want 2; the expired entry should not be counted",
			body.Credentials.ActiveNow)
	}
	if body.Credentials.Source != path {
		t.Errorf("source = %q, want the file path", body.Credentials.Source)
	}
}

// TestAnUnauthenticatedCallerLearnsNothingAboutTheCredentials.
//
// /v1/health answers without a credential on purpose, so a console can show
// what a deployment enforces. That makes anything added to it public by
// default, and the credentials block is a path on the server's filesystem
// and a count of live tokens. This test caught exactly that: the block was
// outside the authenticated branch when it was written.
func TestAnUnauthenticatedCallerLearnsNothingAboutTheCredentials(t *testing.T) {
	path := credentialsFile(t, `
version: 1
tokens:
  - id: only
    token: only-secret
    subject: a@example.com
`)
	h := rotatingHandler(t, path)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health stopped answering an anonymous caller (%d), which is a "+
			"deliberate property of this route: %s", rec.Code, rec.Body)
	}
	for _, secret := range []string{path, "only-secret", "credentials", "active_now"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("an unauthenticated caller learned %q from health: %s", secret, rec.Body)
		}
	}
}
