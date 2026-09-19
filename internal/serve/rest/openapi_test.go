package rest_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// The OpenAPI document is the contract SDKs are generated from. A contract that
// drifts from the running server is worse than no contract, because a generated
// client fails at runtime instead of at build time. These tests make drift a
// test failure.

type openAPIDoc struct {
	OpenAPI string                            `yaml:"openapi"`
	Paths   map[string]map[string]openAPIOper `yaml:"paths"`
}

type openAPIOper struct {
	OperationID string `yaml:"operationId"`
	Summary     string `yaml:"summary"`
}

func loadSpec(t *testing.T) openAPIDoc {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("parsing the spec: %v", err)
	}
	return doc
}

// specPatterns renders the document's paths in the same "METHOD /path" shape
// the server registers, so the two sets can be compared directly.
func specPatterns(doc openAPIDoc) []string {
	var out []string
	for path, ops := range doc.Paths {
		// OpenAPI writes a path parameter as {name}; so does Go's ServeMux.
		for method := range ops {
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}

// TestOpenAPIMatchesRoutes is the guarantee that makes the spec worth
// generating clients from.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	doc := loadSpec(t)
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Fatalf("unexpected OpenAPI version %q", doc.OpenAPI)
	}

	served := server(t, false, govern.Identity{Subject: "tester"}).Patterns()
	documented := specPatterns(doc)

	inSpec := map[string]bool{}
	for _, p := range documented {
		inSpec[p] = true
	}
	for _, p := range served {
		if !inSpec[p] {
			t.Errorf("the server serves %q but api/openapi.yaml does not document it; "+
				"a generated client would not know it exists", p)
		}
	}

	isServed := map[string]bool{}
	for _, p := range served {
		isServed[p] = true
	}
	for _, p := range documented {
		if !isServed[p] {
			t.Errorf("api/openapi.yaml documents %q but the server does not serve it; "+
				"a generated client would call it and get a 404", p)
		}
	}
}

// TestEveryOperationHasAnID checks the spec generates usable client method
// names. Without operationId, generators invent names from the path and they
// change whenever the path does.
func TestEveryOperationHasAnID(t *testing.T) {
	doc := loadSpec(t)
	seen := map[string]string{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			where := strings.ToUpper(method) + " " + path
			if op.OperationID == "" {
				t.Errorf("%s has no operationId, so generated clients will name it arbitrarily", where)
				continue
			}
			if prev, dup := seen[op.OperationID]; dup {
				t.Errorf("operationId %q is used by both %s and %s", op.OperationID, prev, where)
			}
			seen[op.OperationID] = where
			if op.Summary == "" {
				t.Errorf("%s has no summary, which becomes the docstring of every generated client", where)
			}
		}
	}
}

// TestRefusalsCarryRetry is the field an agent branches on. A refusal without
// it leaves a caller to either give up on a typo or loop forever on a denial.
func TestRefusalsCarryRetry(t *testing.T) {
	h := handler(t, true, govern.Identity{Subject: "sa-denied@example.iam.gserviceaccount.com"})

	cases := []struct {
		name, body string
		want       plan.Retry
	}{
		{"unknown metric", `{"metrics":["nope"]}`, plan.RetryModify},
		{"fan out", `{"metrics":["order_revenue"],"dimensions":["order_lines.item_id"]}`, plan.RetryModify},
		{"denied", `{"metrics":["order_revenue"]}`, plan.RetryNever},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, h, "POST", "/v1/query", c.body, token)
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			got, _ := body["retry"].(string)
			if plan.Retry(got) != c.want {
				t.Errorf("want retry %q, got %q for code %v", c.want, got, body["code"])
			}
		})
	}
}

// TestCompileReturnsSQLWithoutRunning covers the dry run an agent uses to
// inspect a query before committing to it.
func TestCompileReturnsSQLWithoutRunning(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	w := do(t, h, "POST", "/v1/compile",
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
		t.Errorf("compile must return the SQL:\n%s", w.Body)
	}
	if _, hasRows := body["rows"]; hasRows {
		t.Error("compile must not return rows; it is a dry run")
	}
}

// TestCompileIsGoverned is the important one. If a dry run skipped the gate it
// would be a way to read the schema, table names and join structure for metrics
// the caller may not query.
func TestCompileIsGoverned(t *testing.T) {
	h := handler(t, true, govern.Identity{Subject: "sa-denied@example.iam.gserviceaccount.com"})
	w := do(t, h, "POST", "/v1/compile", `{"metrics":["order_revenue"]}`, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 from a denied compile, got %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "SELECT") {
		t.Errorf("a denied compile leaked SQL:\n%s", w.Body)
	}
}

// TestNamespacesAreExposed checks the composition model is visible over HTTP,
// not only to the CLI.
func TestNamespacesAreExposed(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	w := do(t, h, "GET", "/v1/namespaces", "", token)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var body struct {
		WorkspaceDigest string `json:"workspace_digest"`
		Namespaces      []struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.WorkspaceDigest == "" {
		t.Error("the namespace listing must carry the workspace digest")
	}
	if len(body.Namespaces) == 0 {
		t.Fatal("expected at least one namespace")
	}
	for _, ns := range body.Namespaces {
		if ns.Name == "" {
			t.Error("a namespace was returned with no name")
		}
	}
}

func TestNamespacesRequireAuth(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	if got := do(t, h, "GET", "/v1/namespaces", "", "").Code; got != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", got)
	}
}

// TestCompileRejectsSQLInBody is the raw-SQL guarantee applied to the new
// endpoint, since a dry run is the most tempting place to add an escape hatch.
func TestCompileRejectsSQLInBody(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	for _, body := range []string{
		`{"sql":"SELECT 1"}`,
		`{"metrics":["order_revenue"],"sql":"SELECT 1"}`,
	} {
		if got := do(t, h, "POST", "/v1/compile", body, token).Code; got != http.StatusBadRequest {
			t.Errorf("a compile body carrying SQL must be rejected, got %d for %s", got, body)
		}
	}
}

// TestEveryRefResolves catches the failure mode that makes a contract useless:
// a $ref pointing at a schema that does not exist. Every OpenAPI generator
// fails on it, so an unresolvable reference breaks SDK builds for everyone
// downstream while the engine itself keeps passing its own tests.
func TestEveryRefResolves(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	var doc any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("parsing the spec: %v", err)
	}

	for _, ref := range collectRefs(doc) {
		target, ok := strings.CutPrefix(ref, "#/")
		if !ok {
			t.Errorf("$ref %q is not a local reference; SDK generation should not reach the network", ref)
			continue
		}
		if resolve(doc, strings.Split(target, "/")) == nil {
			t.Errorf("$ref %q does not resolve; every generated client fails to build on this", ref)
		}
	}
}

// collectRefs walks the decoded document and returns every $ref value.
func collectRefs(node any) []string {
	var out []string
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "$ref" {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
				continue
			}
			out = append(out, collectRefs(v)...)
		}
	case []any:
		for _, v := range n {
			out = append(out, collectRefs(v)...)
		}
	}
	return out
}

// resolve follows a JSON pointer path, returning nil if any segment is absent.
func resolve(node any, path []string) any {
	for _, seg := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		if node, ok = m[seg]; !ok {
			return nil
		}
	}
	return node
}
