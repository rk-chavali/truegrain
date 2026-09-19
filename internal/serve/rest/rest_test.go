package rest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

const token = "test-token-not-a-real-secret"

// server builds the REST server over the fixture model.
func server(t *testing.T, withPolicy bool, id govern.Identity) *rest.Server {
	t.Helper()
	modelPath := filepath.Join("..", "..", "..", "testdata", "models", "retail.yaml")

	var resolver govern.PolicyResolver = govern.AllowAll{}
	if withPolicy {
		ws, err := workspace.Load(modelPath, workspace.Options{Strict: true})
		if err != nil {
			t.Fatal(err)
		}
		pol, err := govern.LoadFilePolicy(filepath.Join("..", "..", "..", "testdata", "policy.yaml"), ws)
		if err != nil {
			t.Fatal(err)
		}
		resolver = pol
	}

	eng, err := engine.New(engine.Config{
		ModelPath: modelPath, Dialect: "duckdb", Resolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rest.New(eng, rest.StaticTokens{Tokens: map[string]govern.Identity{token: id}})
}

// engineFor builds another engine over the same fixture, for a test that
// needs to reload the server with one.
func engineFor(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "..", "testdata", "models", "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// handler is the routed server, which is what most tests exercise.
func handler(t *testing.T, withPolicy bool, id govern.Identity) http.Handler {
	t.Helper()
	return server(t, withPolicy, id).Handler()
}

func do(t *testing.T, h http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	for _, path := range []string{"/v1/metrics", "/v1/dimensions", "/v1/metrics/order_revenue"} {
		t.Run(path, func(t *testing.T) {
			if got := do(t, h, "GET", path, "", "").Code; got != http.StatusUnauthorized {
				t.Errorf("want 401, got %d", got)
			}
		})
	}
	if got := do(t, h, "POST", "/v1/query", `{"metrics":["order_count"]}`, "").Code; got != http.StatusUnauthorized {
		t.Errorf("query: want 401, got %d", got)
	}
}

func TestWrongTokenIsRejectedWithoutDetail(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	w := do(t, h, "GET", "/v1/metrics", "", "wrong-token")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	// The message must not tell a prober whether the token was known.
	if strings.Contains(strings.ToLower(w.Body.String()), "unknown token") {
		t.Errorf("the error distinguishes an unknown token from a malformed one:\n%s", w.Body)
	}
}

// TestHealthIsUnauthenticated is deliberate: an operator must be able to read
// what the deployment enforces before holding a credential.
func TestHealthIsUnauthenticated(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	w := do(t, h, "GET", "/v1/health", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["enforcement_notes"]; !ok {
		t.Error("health must report what this configuration does not enforce")
	}
}

func TestQueryRoundTrip(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	w := do(t, h, "POST", "/v1/query",
		`{"metrics":["order_revenue"],"dimensions":["customers.region"]}`, token)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	sql, _ := body["compiled_sql"].(string)
	if !strings.Contains(sql, "SUM") {
		t.Errorf("every response carries the compiled SQL:\n%s", w.Body)
	}
	if body["model_version"] == "" {
		t.Error("every response carries the model version")
	}
}

func TestUnknownFieldsInRequestAreRejected(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	// `dimension` is a plausible misspelling of `dimensions`. Ignoring it would
	// silently return an ungrouped total that looks like a real answer.
	w := do(t, h, "POST", "/v1/query",
		`{"metrics":["order_revenue"],"dimension":["customers.region"]}`, token)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown field, got %d: %s", w.Code, w.Body)
	}
}

func TestRefusalStatusCodes(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	cases := []struct {
		name, body string
		want       int
	}{
		{"unknown metric", `{"metrics":["nope"]}`, http.StatusNotFound},
		{"no metrics", `{"dimensions":["customers.region"]}`, http.StatusBadRequest},
		{"fan out", `{"metrics":["order_revenue"],"dimensions":["order_lines.item_id"]}`,
			http.StatusUnprocessableEntity},
		{"bad filter", `{"metrics":["order_revenue"],"filters":[{"dimension":"orders.order_date","op":"eq","values":["not a date"]}]}`,
			http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, h, "POST", "/v1/query", c.body, token)
			if w.Code != c.want {
				t.Errorf("want %d, got %d: %s", c.want, w.Code, w.Body)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] == "" || body["reason"] == "" {
				t.Errorf("a refusal must carry a code and a reason:\n%s", w.Body)
			}
		})
	}
}

// TestGovernanceDenialIsForbiddenNotEmpty checks the denial surfaces as 403
// with a reason rather than as 200 with no rows.
func TestGovernanceDenialIsForbiddenNotEmpty(t *testing.T) {
	h := handler(t, true, govern.Identity{Subject: "sa-denied@example.iam.gserviceaccount.com"})
	w := do(t, h, "POST", "/v1/query", `{"metrics":["order_revenue"]}`, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != govern.CodeDenied {
		t.Errorf("want code %q, got %q", govern.CodeDenied, body["code"])
	}
}

// TestNoEndpointAcceptsSQL is the REST counterpart of the MCP conformance test.
func TestNoEndpointAcceptsSQL(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	for _, body := range []string{
		`{"sql":"SELECT 1"}`,
		`{"metrics":["order_revenue"],"sql":"SELECT 1"}`,
		`{"metrics":["order_revenue"],"where":"1=1"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if got := do(t, h, "POST", "/v1/query", body, token).Code; got != http.StatusBadRequest {
				t.Errorf("a body carrying SQL must be rejected, got %d", got)
			}
		})
	}
}
