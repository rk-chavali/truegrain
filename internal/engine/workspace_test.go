package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

func workspaceRoot() string { return filepath.Join("..", "..", "testdata", "workspace") }

// teamEngine builds an engine over the two-team fixture, optionally with the
// workspace policy applied.
func teamEngine(t *testing.T, withPolicy bool) *engine.Engine {
	t.Helper()
	ws, err := workspace.Load(workspaceRoot(), workspace.Options{Strict: true})
	if err != nil {
		t.Fatalf("loading workspace:\n%v", err)
	}
	var resolver govern.PolicyResolver = govern.AllowAll{}
	if withPolicy {
		pol, err := govern.LoadFilePolicy(
			filepath.Join("..", "..", "testdata", "workspace-policy.yaml"), ws)
		if err != nil {
			t.Fatalf("loading policy:\n%v", err)
		}
		resolver = pol
	}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{Dialect: "duckdb", Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func compile(t *testing.T, eng *engine.Engine, subject string, req plan.Request) (*engine.Compiled, error) {
	t.Helper()
	return eng.Compile(context.Background(), govern.Identity{Subject: subject}, req)
}

// TestCrossNamespaceJoinThroughImport is the feature working end to end: a
// metric in one namespace aggregating a dataset another namespace exported.
func TestCrossNamespaceJoinThroughImport(t *testing.T) {
	eng := teamEngine(t, false)
	c, err := compile(t, eng, "tester", plan.Request{
		Metrics:    []string{"marketing.attributed_revenue"},
		Dimensions: []string{"marketing.campaigns.channel"},
	})
	if err != nil {
		t.Fatalf("a metric over an imported dataset should compile:\n%v", err)
	}
	if c.Namespace != "marketing" {
		t.Errorf("want namespace marketing, got %q", c.Namespace)
	}
	if !strings.Contains(c.SQL, "main\".\"orders") {
		t.Errorf("the imported dataset should be joined:\n%s", c.SQL)
	}
	if !strings.Contains(c.SQL, "main\".\"campaigns") {
		t.Errorf("the local dataset should be the base:\n%s", c.SQL)
	}
}

// TestMetricsFromTwoNamespacesCombine is the behaviour that replaced a refusal.
//
// Two facts in different namespaces cannot be answered by one flat join, but
// they can be aggregated separately and joined on a dimension both can reach.
// "revenue and marketing spend by month" is the most common real question there
// is, and refusing it was the engine being unhelpful rather than careful.
func TestMetricsFromTwoNamespacesCombine(t *testing.T) {
	eng := teamEngine(t, false)
	c, err := compile(t, eng, "tester", plan.Request{
		Metrics: []string{"sales.order_revenue", "marketing.campaign_spend"},
	})
	if err != nil {
		t.Fatalf("metrics from two namespaces should combine:\n%v", err)
	}
	if c.Parts != 2 {
		t.Errorf("want 2 aggregated parts, got %d", c.Parts)
	}
	if !strings.Contains(c.SQL, "FULL OUTER JOIN") {
		t.Errorf("parts must be joined, not unioned or cross-produced:\n%s", c.SQL)
	}
	for _, want := range []string{"sales", "marketing"} {
		if !strings.Contains(c.Namespace, want) {
			t.Errorf("the compiled result should record both namespaces, missing %q: %q", want, c.Namespace)
		}
	}
}

// TestSharedDimensionAcrossNamespaces checks the two parts group by the same
// column, matched by governed origin rather than by the name each namespace
// happens to address it with.
func TestSharedDimensionAcrossNamespaces(t *testing.T) {
	eng := teamEngine(t, false)
	c, err := compile(t, eng, "tester", plan.Request{
		Metrics:    []string{"sales.order_revenue", "marketing.campaign_spend"},
		Dimensions: []string{"sales.orders.order_date"},
		Grain:      plan.GrainMonth,
	})
	if err != nil {
		t.Fatalf("marketing imports orders, so both parts can group by its date:\n%v", err)
	}
	if c.Parts != 2 {
		t.Fatalf("want 2 parts, got %d", c.Parts)
	}
	// The join must be null safe: a month with orders but no campaigns is a
	// real row, and plain equality would drop it.
	if !strings.Contains(c.SQL, "IS NOT DISTINCT FROM") {
		t.Errorf("the join on the shared dimension must be null safe:\n%s", c.SQL)
	}
	if !strings.Contains(c.SQL, "COALESCE") {
		t.Errorf("the dimension value must be coalesced across parts:\n%s", c.SQL)
	}
}

// TestPartWithoutTheDimensionIsRefused is the case that must still be refused.
// Marketing imports orders but not customers, so it cannot produce a row per
// region, and inventing one would be a wrong answer.
func TestPartWithoutTheDimensionIsRefused(t *testing.T) {
	eng := teamEngine(t, false)
	_, err := compile(t, eng, "tester", plan.Request{
		Metrics:    []string{"sales.order_revenue", "marketing.campaign_spend"},
		Dimensions: []string{"sales.customers.region"},
	})
	if err == nil {
		t.Fatal("a namespace that cannot reach the dimension must refuse, not guess")
	}
	if got := refusalCode(err); got != engine.CodeCrossNamespace {
		t.Fatalf("want %q, got %q from: %v", engine.CodeCrossNamespace, got, err)
	}
	if !strings.Contains(err.Error(), "imports") {
		t.Errorf("the refusal should name the fix:\n%v", err)
	}
}

// TestGovernanceAppliesToEveryPart is the security property of multi-fact
// planning. Checking only the first part would let every later one escape the
// gate, which is the most expensive possible bug in this path.
func TestGovernanceAppliesToEveryPart(t *testing.T) {
	eng := teamEngine(t, true)

	// campaign_spend is ungoverned; attributed_revenue reads a tagged column.
	// The tagged one is deliberately second.
	_, err := compile(t, eng, "growth@acme.com", plan.Request{
		Metrics: []string{"marketing.campaign_spend", "sales.order_revenue"},
	})
	if err == nil {
		t.Fatal("a denied metric in the second part must deny the whole query")
	}
	if got := refusalCode(err); got != govern.CodeDenied {
		t.Fatalf("want %q, got %q from: %v", govern.CodeDenied, got, err)
	}

	// And the grant holder gets both parts.
	if _, err := compile(t, eng, "finance@acme.com", plan.Request{
		Metrics: []string{"marketing.campaign_spend", "sales.order_revenue"},
	}); err != nil {
		t.Errorf("the grant holder should get both parts:\n%v", err)
	}
}

// TestDimensionFromAnotherNamespaceRefused covers the same rule for grouping.
func TestDimensionFromAnotherNamespaceRefused(t *testing.T) {
	eng := teamEngine(t, false)
	_, err := compile(t, eng, "tester", plan.Request{
		Metrics:    []string{"marketing.campaign_spend"},
		Dimensions: []string{"sales.customers.region"},
	})
	if err == nil {
		t.Fatal("grouping by a dimension in a namespace the metric cannot reach must be refused")
	}
	if got := refusalCode(err); got != engine.CodeCrossNamespace {
		t.Fatalf("want %q, got %q from: %v", engine.CodeCrossNamespace, got, err)
	}
	if !strings.Contains(err.Error(), "import") {
		t.Errorf("the refusal should point at the fix, which is an import:\n%v", err)
	}
}

// TestQualifiedAndBareNamesBothResolve checks the accept-bare, return-qualified
// rule: strict qualification everywhere would break every single-team command
// for no safety gain.
func TestQualifiedAndBareNamesBothResolve(t *testing.T) {
	eng := teamEngine(t, false)
	for _, name := range []string{"marketing.campaign_spend", "campaign_spend"} {
		t.Run(name, func(t *testing.T) {
			c, err := compile(t, eng, "tester", plan.Request{Metrics: []string{name}})
			if err != nil {
				t.Fatalf("%q should resolve:\n%v", name, err)
			}
			if c.Namespace != "marketing" {
				t.Errorf("resolved into %q", c.Namespace)
			}
		})
	}
}

// TestGrantFollowsImportedColumn is the security property of composition.
//
// The revenue tag is written by sales against sales.orders.order_total.
// Marketing reads that column through an import. The grant must follow the
// column, otherwise importing would be a way to launder access.
func TestGrantFollowsImportedColumn(t *testing.T) {
	eng := teamEngine(t, true)

	// Marketing's own metric touches no sales column and is unaffected.
	if _, err := compile(t, eng, "growth@acme.com",
		plan.Request{Metrics: []string{"marketing.campaign_spend"}}); err != nil {
		t.Fatalf("a metric over marketing's own data should be allowed:\n%v", err)
	}

	// The same identity reading sales revenue through the import is denied.
	_, err := compile(t, eng, "growth@acme.com",
		plan.Request{Metrics: []string{"marketing.attributed_revenue"}})
	if err == nil {
		t.Fatal("a grant written by sales must follow its column into marketing; " +
			"otherwise importing a dataset launders access to it")
	}
	if got := refusalCode(err); got != govern.CodeDenied {
		t.Fatalf("want %q, got %q from: %v", govern.CodeDenied, got, err)
	}

	// And the holder of the grant may read it from either namespace.
	for _, metric := range []string{"marketing.attributed_revenue", "sales.order_revenue"} {
		if _, err := compile(t, eng, "finance@acme.com",
			plan.Request{Metrics: []string{metric}}); err != nil {
			t.Errorf("the grant holder should read %s:\n%v", metric, err)
		}
	}
}

// TestNamespaceTagCoversItsOwnFieldsOnly checks the namespace-wide tag does not
// reach across an import into the other team's columns.
func TestNamespaceTagCoversItsOwnFieldsOnly(t *testing.T) {
	eng := teamEngine(t, true)
	// email_domain is the only field left under the sales pii tag.
	_, err := compile(t, eng, "growth@acme.com", plan.Request{
		Metrics:    []string{"sales.order_count"},
		Dimensions: []string{"sales.customers.email_domain"},
	})
	if err == nil {
		t.Fatal("a namespace-wide tag should protect the field it covers")
	}
	if got := refusalCode(err); got != govern.CodeDenied {
		t.Fatalf("want %q, got %q", govern.CodeDenied, got)
	}
	if _, err := compile(t, eng, "privacy-cleared@acme.com", plan.Request{
		Metrics:    []string{"sales.order_count"},
		Dimensions: []string{"sales.customers.email_domain"},
	}); err != nil {
		t.Errorf("the pii grant holder should be allowed:\n%v", err)
	}
}

// TestDenialCarriesProvenance is the regression guard for a real bug: denials
// were being recorded without the namespace or the workspace digest, so an
// evidence query could find the successes and not the refusals. An audit record
// that only describes what was allowed is not an audit record.
func TestDenialCarriesProvenance(t *testing.T) {
	ws, err := workspace.Load(workspaceRoot(), workspace.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := govern.LoadFilePolicy(
		filepath.Join("..", "..", "testdata", "workspace-policy.yaml"), ws)
	if err != nil {
		t.Fatal(err)
	}
	audit := &govern.MemoryAudit{}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{
		Dialect: "duckdb", Resolver: pol, Audit: audit,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Compile, not Query: a denial on the compile path must be recorded too.
	_, err = eng.Compile(context.Background(), govern.Identity{Subject: "growth@acme.com"},
		plan.Request{Metrics: []string{"marketing.attributed_revenue"}})
	if err == nil {
		t.Fatal("expected a denial")
	}

	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("want exactly one audit event for a denied compile, got %d", len(events))
	}
	e := events[0]
	if e.Decision != "denied" {
		t.Errorf("want decision denied, got %q", e.Decision)
	}
	if e.Namespace != "marketing" {
		t.Errorf("want the namespace recorded, got %q", e.Namespace)
	}
	if e.ModelVersion != ws.Digest {
		t.Errorf("want the workspace digest %q, got %q", ws.Digest, e.ModelVersion)
	}
	// The denied field is named by its origin, which is where the grant lives.
	if len(e.DeniedFields) == 0 || e.DeniedFields[0] != "sales.orders.order_total" {
		t.Errorf("want the denied field recorded by origin, got %v", e.DeniedFields)
	}
}

// TestHealthReportsEveryNamespace checks an operator can see what loaded and
// what did not without reading logs.
func TestHealthReportsEveryNamespace(t *testing.T) {
	h := teamEngine(t, false).Health()
	if len(h.Namespaces) != 2 {
		t.Fatalf("want 2 namespaces in health, got %d", len(h.Namespaces))
	}
	for _, ns := range h.Namespaces {
		if !ns.Available {
			t.Errorf("namespace %q should be available", ns.Name)
		}
		if len(ns.Owners) == 0 {
			t.Errorf("health should report the owners of %q", ns.Name)
		}
	}
	if h.WorkspaceDigest == "" {
		t.Error("health must carry the workspace digest")
	}
}

// TestListingsReturnQualifiedNames checks the API boundary is unambiguous even
// though bare names are accepted on the way in.
func TestListingsReturnQualifiedNames(t *testing.T) {
	eng := teamEngine(t, false)
	for _, m := range eng.Metrics() {
		if !strings.Contains(m.Qualified, ".") {
			t.Errorf("metric %q should be returned qualified", m.Qualified)
		}
	}
	dims, err := eng.VisibleDimensions(context.Background(),
		govern.Identity{Subject: "tester"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dims) == 0 {
		t.Fatal("expected dimensions")
	}
	for _, d := range dims {
		if strings.Count(d.Qualified, ".") != 2 {
			t.Errorf("dimension %q should be namespace.dataset.field", d.Qualified)
		}
	}
}

// TestAuditRecordsNamespace checks the facet an evidence query filters on first
// is actually recorded.
func TestAuditRecordsNamespace(t *testing.T) {
	ws, err := workspace.Load(workspaceRoot(), workspace.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := govern.LoadFilePolicy(
		filepath.Join("..", "..", "testdata", "workspace-policy.yaml"), ws)
	if err != nil {
		t.Fatal(err)
	}
	audit := &govern.MemoryAudit{}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{
		Dialect: "duckdb", Resolver: pol, Audit: audit, Executor: &spyExecutor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = eng.Query(context.Background(), govern.Identity{Subject: "finance@acme.com"},
		plan.Request{Metrics: []string{"marketing.attributed_revenue"}})

	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(events))
	}
	if events[0].Namespace != "marketing" {
		t.Errorf("want namespace marketing recorded, got %q", events[0].Namespace)
	}
	if events[0].ModelVersion != ws.Digest {
		t.Errorf("the event should carry the workspace digest, got %q", events[0].ModelVersion)
	}
}
