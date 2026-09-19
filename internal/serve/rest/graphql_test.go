package rest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// The GraphQL surface.
//
// It exists for a front end that already speaks GraphQL. The property
// worth asserting most is that it is the same product underneath: the same
// vocabulary, the same planner, and above all the same refusal, carrying
// the same code and hint. A surface where a fan-out came back as a generic
// error would be a second, worse answer to the same question.

func graphQL(t *testing.T, h http.Handler, query string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the response is not JSON: %v\n%s", err, rec.Body)
	}
	return out
}

func firstError(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	errs, ok := resp["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("expected an errors array, got %v", resp)
	}
	e, _ := errs[0].(map[string]any)
	return e
}

func TestGraphQLListsMetrics(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	resp := graphQL(t, h, `{ metrics { name description } }`)

	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data: %v", resp)
	}
	metrics, ok := data["metrics"].([]any)
	if !ok || len(metrics) == 0 {
		t.Fatalf("no metrics: %v", data)
	}
}

// TestGraphQLRefusesAFanOutWithItsHint.
//
// The whole point of it being the same product underneath. A client that
// gave up here without the hint would be giving up on a question the
// engine just told it how to ask.
func TestGraphQLRefusesAFanOutWithItsHint(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	resp := graphQL(t, h,
		`{ query(metrics: ["order_revenue"], dimensions: ["order_lines.item_id"]) { rows } }`)

	e := firstError(t, resp)
	ext, _ := e["extensions"].(map[string]any)
	if ext == nil {
		t.Fatalf("no extensions on the error: %v", e)
	}
	if ext["code"] != "fan_out_would_inflate" {
		t.Errorf("code = %v, want fan_out_would_inflate", ext["code"])
	}
	if hint, _ := ext["hint"].(string); !strings.Contains(hint, "line_revenue") {
		t.Errorf("the hint does not name the metric that answers: %v", ext["hint"])
	}
	if ext["retry"] != "modify" {
		t.Errorf("retry = %v, want modify", ext["retry"])
	}
}

// TestAGraphQLErrorIsA200WithAnErrorsArray.
//
// The specification says so, and every GraphQL client reads the body and
// ignores the status. A refusal returned as 4xx would be invisible to all
// of them.
func TestAGraphQLErrorIsA200WithAnErrorsArray(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	body := `{"query": "{ query(metrics: [\"nope\"]) { rows } }"}`

	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with an errors array", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["errors"]; !ok {
		t.Errorf("no errors array: %v", resp)
	}
}

// TestGraphQLTakesVariables, because a client with a persisted query sends
// its values that way rather than interpolating them into the document.
func TestGraphQLTakesVariables(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	body := `{
		"query": "query Q($m: [String!]) { query(metrics: $m) { rowCount } }",
		"variables": {"m": ["order_revenue"]}
	}`
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// This engine has no executor, so the query cannot run; what matters is
	// that the variable resolved rather than the request being rejected as
	// naming no metric.
	if errs, ok := resp["errors"].([]any); ok && len(errs) > 0 {
		e, _ := errs[0].(map[string]any)
		if msg, _ := e["message"].(string); strings.Contains(msg, "at least one metric") {
			t.Errorf("the variable was not resolved: %v", msg)
		}
	}
}

// TestThereAreNoMutations.
//
// There is no write path, and a surface that accepted a mutation and then
// failed somewhere deeper would suggest one existed.
func TestThereAreNoMutations(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	resp := graphQL(t, h, `mutation { deleteEverything }`)

	msg, _ := firstError(t, resp)["message"].(string)
	if !strings.Contains(msg, "no write path") {
		t.Errorf("the refusal does not say why: %q", msg)
	}
}

// TestASubscriptionPointsAtTheWebhook, because wanting to be told when a
// question is refused is a reasonable thing to want and there is an answer
// for it.
func TestASubscriptionPointsAtTheWebhook(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	resp := graphQL(t, h, `subscription { refusals { code } }`)

	msg, _ := firstError(t, resp)["message"].(string)
	if !strings.Contains(msg, "webhook") {
		t.Errorf("the refusal does not point anywhere useful: %q", msg)
	}
}

// TestIntrospectionAnswersEnoughToValidate, so a client that insists on
// introspecting before it will send anything can still connect.
func TestIntrospectionAnswersEnoughToValidate(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	resp := graphQL(t, h, `{ __schema { queryType { name } } }`)

	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data: %v", resp)
	}
	schema, ok := data["__schema"].(map[string]any)
	if !ok {
		t.Fatalf("no __schema: %v", data)
	}
	if schema["mutationType"] != nil {
		t.Error("introspection advertises a mutation type that does not exist")
	}
	qt, _ := schema["queryType"].(map[string]any)
	if qt["name"] != "Query" {
		t.Errorf("queryType = %v", qt)
	}
}

// TestGraphQLIsNotInTheVersionedContract.
//
// It is a whole other protocol with its own envelope. Documenting it in
// api/openapi.yaml would make all three typed SDKs grow a graphql()
// method that posts a string, which is worse than useless.
func TestGraphQLIsNotInTheVersionedContract(t *testing.T) {
	for _, p := range server(t, false, govern.Identity{Subject: "tester"}).Patterns() {
		if strings.Contains(strings.ToLower(p), "graphql") {
			t.Errorf("Patterns() returned %q, which would put GraphQL in the "+
				"typed client contract", p)
		}
	}
}

// TestGraphQLStillNeedsACredential.
//
// A second surface is a second chance to forget authentication, and this
// one reaches the same governed data as every other.
func TestGraphQLStillNeedsACredential(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})
	body := `{"query": "{ metrics { name } }"}`

	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(body))
	// No Authorization header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("an unauthenticated caller reached the GraphQL surface: %s", rec.Body)
	}
}
