package mcp_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	mcpserve "github.com/rk-chavali/truegrain/internal/serve/mcp"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// connect wires a real client to a real server over an in-memory transport, so
// these tests exercise the actual protocol rather than the handler functions.
func connect(t *testing.T, id govern.Identity, policy string) *sdk.ClientSession {
	t.Helper()
	modelPath := filepath.Join("..", "..", "..", "testdata", "models", "retail.yaml")

	cfg := engine.Config{
		ModelPath: modelPath,
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
	}
	if policy != "" {
		ws, err := workspace.Load(modelPath, workspace.Options{Strict: true})
		if err != nil {
			t.Fatal(err)
		}
		pol, err := govern.LoadFilePolicy(policy, ws)
		if err != nil {
			t.Fatalf("loading policy:\n%v", err)
		}
		cfg.Resolver = pol
	}

	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatalf("building engine:\n%v", err)
	}

	clientT, serverT := sdk.NewInMemoryTransports()
	srv := mcpserve.New(eng, id).MCPServer()

	ctx := context.Background()
	serverSession, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// TestNoRawSQLTool is the load-bearing conformance test for this package.
//
// The argument for routing an agent through a semantic layer instead of at the
// warehouse is that an ungoverned query is inexpressible. That holds only while
// no tool accepts SQL. If someone adds one for convenience, this fails.
func TestNoRawSQLTool(t *testing.T) {
	cs := connect(t, govern.Identity{Subject: "tester"}, "")

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"list_metrics": false, "describe_metric": false,
		"list_dimensions": false, "query": false,
	}
	for _, tool := range res.Tools {
		if _, expected := want[tool.Name]; !expected {
			t.Errorf("unexpected tool %q: the surface is fixed at four tools, "+
				"and a fifth is how the no-raw-SQL guarantee gets lost", tool.Name)
			continue
		}
		want[tool.Name] = true

		// A tool whose name or schema mentions SQL as an input is the thing
		// this test exists to catch.
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(schema))
		for _, banned := range []string{`"sql"`, `"query_string"`, `"statement"`, `"raw"`} {
			if strings.Contains(lower, banned) {
				t.Errorf("tool %q accepts %s in its input schema, which is a raw SQL escape hatch:\n%s",
					tool.Name, banned, schema)
			}
		}
		if strings.Contains(strings.ToLower(tool.Name), "sql") {
			t.Errorf("tool %q is named after SQL", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q is missing", name)
		}
	}
}

func TestListMetricsDescribesEachMetric(t *testing.T) {
	cs := connect(t, govern.Identity{Subject: "tester"}, "")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "list_metrics", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("list_metrics failed: %s", text(res))
	}
	body := text(res)
	for _, want := range []string{"order_revenue", "order_count", "average_order_value"} {
		if !strings.Contains(body, want) {
			t.Errorf("list_metrics omits %q:\n%s", want, body)
		}
	}
	// Description quality is a product feature: an agent picks the metric from
	// this text, so an entry with a bare name is a defect.
	if !strings.Contains(body, "Total value of all orders") {
		t.Errorf("list_metrics should carry each metric's description:\n%s", body)
	}
}

func TestSearchNarrowsBySynonym(t *testing.T) {
	cs := connect(t, govern.Identity{Subject: "tester"}, "")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		// "top line" is a synonym of order_revenue and appears in no name.
		Name: "list_metrics", Arguments: map[string]any{"search": "top line"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := text(res)
	if !strings.Contains(body, "order_revenue") {
		t.Errorf("search should match synonyms so that business vocabulary resolves:\n%s", body)
	}
	if strings.Contains(body, "units_sold") {
		t.Errorf("search should exclude unrelated metrics:\n%s", body)
	}
}

func TestQueryRefusalIsActionable(t *testing.T) {
	cs := connect(t, govern.Identity{Subject: "tester"}, "")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "query",
		Arguments: map[string]any{
			"metrics":    []string{"order_revenue"},
			"dimensions": []string{"order_lines.item_id"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a fan-out request must come back as an error result, not as rows")
	}
	body := text(res)
	for _, want := range []string{"fan_out_would_inflate", "What to do:", "refusal, not an empty result"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal is missing %q, so an agent cannot act on it:\n%s", want, body)
		}
	}
}

func TestUnknownMetricSuggestsRatherThanFails(t *testing.T) {
	cs := connect(t, govern.Identity{Subject: "tester"}, "")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"metrics": []string{"revenue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := text(res)
	if !strings.Contains(body, "order_revenue") {
		t.Errorf("an unknown metric should suggest the real name:\n%s", body)
	}
}

// TestDeniedDimensionNotAdvertised checks the governance filter reaches the
// tool surface, not just the query path.
func TestDeniedDimensionNotAdvertised(t *testing.T) {
	policy := filepath.Join("..", "..", "..", "testdata", "policy.yaml")
	cs := connect(t, govern.Identity{Subject: "sa-denied@example.iam.gserviceaccount.com"}, policy)

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "list_dimensions", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text(res), "email_domain") {
		t.Errorf("a dimension this identity cannot read was advertised:\n%s", text(res))
	}
}

func TestCompileOnlyEngineReturnsSQLNotSilence(t *testing.T) {
	cs := connect(t, govern.Identity{Subject: "tester"}, "")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "query",
		Arguments: map[string]any{
			"metrics":    []string{"order_revenue"},
			"dimensions": []string{"customers.region"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a valid request should not error: %s", text(res))
	}
	body := text(res)
	if !strings.Contains(body, "compile but not execute") {
		t.Errorf("a compile-only engine must say so rather than implying an empty result:\n%s", body)
	}
	if !strings.Contains(body, "SUM") {
		t.Errorf("the compiled SQL should be returned:\n%s", body)
	}
}

func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
