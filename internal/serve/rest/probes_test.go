package rest_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// The probes answer an orchestrator, so the properties worth asserting are the
// ones a Kubernetes deployment depends on: that they need no credentials, that
// they say nothing about the model, and that neither of them fails because a
// warehouse is unreachable.

func TestLivenessNeedsNoCredentials(t *testing.T) {
	// A kubelet carries no bearer token. A probe that needed one is how a
	// deployment ends up with no probes configured at all.
	rec := httptest.NewRecorder()
	handler(t, false, govern.Identity{Subject: "tester"}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz without a token = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestReadinessNeedsNoCredentials(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(t, false, govern.Identity{Subject: "tester"}).ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz without a token = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// TestAProbeDisclosesNothingAboutTheModel.
//
// They are unauthenticated, so anything they say is public. Readiness may say
// whether a model is loaded; it may not name it, version it, or list what is
// in it.
func TestAProbeDisclosesNothingAboutTheModel(t *testing.T) {
	// Terms drawn from the fixture model this server actually serves.
	secrets := []string{"retail", "order_revenue", "customers", "sha256:", "duckdb", "email"}

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		handler(t, false, govern.Identity{Subject: "tester"}).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		body := strings.ToLower(rec.Body.String())
		for _, secret := range secrets {
			if strings.Contains(body, strings.ToLower(secret)) {
				t.Errorf("%s leaks %q to an unauthenticated caller: %s", path, secret, rec.Body)
			}
		}
	}
}

// TestLivenessDoesNotDependOnTheWarehouse.
//
// The engine under test is compile-only: it has no executor at all, which is
// the most complete version of "the warehouse is unreachable". Liveness must
// still pass, or an orchestrator restarts every replica during somebody
// else's outage, and a restart cannot fix a warehouse.
func TestLivenessDoesNotDependOnTheWarehouse(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(t, false, govern.Identity{Subject: "tester"}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("liveness failed with no executor configured: %d %s", rec.Code, rec.Body)
	}
}

// TestReadinessDoesNotDependOnTheWarehouse.
//
// Same server, and the same reasoning one step further out: with the warehouse
// gone this engine still answers every metadata route and still refuses a
// fan-out. Reporting not-ready would withdraw the half that still works, and
// would do it on every replica simultaneously.
func TestReadinessDoesNotDependOnTheWarehouse(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(t, false, govern.Identity{Subject: "tester"}).ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("readiness failed with no executor configured: %d %s", rec.Code, rec.Body)
	}

	// The claim above, asserted rather than described: metadata still answers.
	metrics := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler(t, false, govern.Identity{Subject: "tester"}).ServeHTTP(metrics, req)
	if metrics.Code != http.StatusOK {
		t.Fatalf("the premise is wrong: metadata does not answer without an executor (%d)",
			metrics.Code)
	}
}

// TestProbesAreNotInTheClientContract.
//
// The versioned surface is published as api/openapi.yaml and checked against
// the mux in both directions. The probes are deliberately outside it, so the
// separation has to be asserted or the next person merges them and every SDK
// grows a live() method nobody wants.
func TestUnversionedRoutesAreNotInTheContract(t *testing.T) {
	served := server(t, false, govern.Identity{Subject: "tester"}).Patterns()
	for _, p := range served {
		if !strings.Contains(p, "/v1/") {
			t.Errorf("Patterns() returned %q, which is not part of the versioned "+
				"client contract; probes belong in probes()", p)
		}
	}

	src, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []string{"/healthz", "/readyz", "/graphql"} {
		if _, found := doc.Paths[probe]; found {
			t.Errorf("api/openapi.yaml documents %s; a probe is operational plumbing "+
				"and documenting it makes every SDK's contract test demand a method "+
				"for it", probe)
		}
	}
}

// TestBothProbesAreActuallyRouted guards the other direction: a probe that is
// defined and never registered is worse than none, because the deployment
// looks configured and the endpoint 404s.
func TestBothProbesAreActuallyRouted(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	var missing []string
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == http.StatusNotFound {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("declared but not routed: %v", missing)
	}
}
