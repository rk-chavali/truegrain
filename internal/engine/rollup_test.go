package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/duckdbtest"
	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/exec"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Rollup routing, against a real warehouse holding real rows.
//
// These run on DuckDB rather than on a plan comparison, and that is the
// whole point. A rollup is a second copy of a number, so the only test
// worth having is the one that asks both copies the same question and
// requires the same answer. A test that asserted which table was read
// would pass on a rollup that returned a different total.
//
// The fixture's rollup tables hold the same 885.50 and the same 400.00
// largest order as the fact tables. Every number below is one the parity
// suite already proves from the source tables.

// rollupEngine builds an engine over the retail fixture with its rollups,
// executing against a throwaway DuckDB.
//
// The model and its sidecar are copied into a temp directory rather than
// read where they live, because a rollups.yaml sitting next to
// testdata/models/retail.yaml would change what every other test in this
// repository is measuring.
func rollupEngine(t *testing.T, audit govern.AuditSink) *engine.Engine {
	t.Helper()
	db := duckdbtest.Seed(t, filepath.Join("..", ".."))

	dir := t.TempDir()
	for _, f := range []struct{ from, to string }{
		{filepath.Join("..", "..", "testdata", "models", "retail.yaml"), "retail.yaml"},
		{filepath.Join("..", "..", "testdata", "rollups", "rollups.yaml"), "rollups.yaml"},
	} {
		src, err := os.ReadFile(f.from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.to), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ex, err := exec.NewDuckDB(exec.DuckDBOptions{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })

	eng, err := engine.New(engine.Config{
		ModelPath: dir,
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Executor:  ex,
		Audit:     audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func onlyValue(t *testing.T, rows *engine.Result) string {
	t.Helper()
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 1 {
		t.Fatalf("want one value, got %v", rows.Rows)
	}
	return strings.TrimSpace(fmt.Sprint(rows.Rows[0][0]))
}

func compileRollup(t *testing.T, eng *engine.Engine, req plan.Request) *engine.Compiled {
	t.Helper()
	c, err := eng.Compile(context.Background(), govern.Identity{Subject: "analyst"}, req)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestARollupAnswersTheSameNumberAsTheFactTables.
//
// The only assertion that matters. Everything else here is about when to
// route; this is about routing being harmless when it happens.
func TestARollupAnswersTheSameNumberAsTheFactTables(t *testing.T) {
	eng := rollupEngine(t, nil)
	id := govern.Identity{Subject: "analyst"}

	for _, c := range []struct {
		metric string
		want   string
	}{
		{"order_revenue", "885.50"},
		{"largest_order", "400.00"},
	} {
		rows, err := eng.Query(context.Background(), id, plan.Request{Metrics: []string{c.metric}})
		if err != nil {
			t.Fatalf("%s: %v", c.metric, err)
		}
		if got := onlyValue(t, rows); got != c.want {
			t.Errorf("%s = %s, want %s", c.metric, got, c.want)
		}
	}

	// And it really did come from a rollup, or the test above proves only
	// that the fact tables still work.
	c := compileRollup(t, eng, plan.Request{Metrics: []string{"order_revenue"}})
	if len(c.Rollups) != 1 || c.Rollups[0] != "orders_by_region" {
		t.Fatalf("rollups = %v, want the narrowest one; the SQL was:\n%s", c.Rollups, c.SQL)
	}
}

// TestAStoredMaxReaggregatesWithMax.
//
// The re-aggregation is derived from the metric's own aggregate rather than
// declared, because the obvious thing to write in a config file is the
// wrong thing. Combining the two stored maxima with SUM gives 475.50, which
// is a number nothing would flag.
func TestAStoredMaxReaggregatesWithMax(t *testing.T) {
	eng := rollupEngine(t, nil)

	c := compileRollup(t, eng, plan.Request{Metrics: []string{"largest_order"}})
	if len(c.Rollups) == 0 {
		t.Fatalf("this test needs the rollup to be used; SQL:\n%s", c.SQL)
	}
	if !strings.Contains(strings.ToUpper(c.SQL), "MAX(") {
		t.Errorf("the stored maximum is not combined with MAX:\n%s", c.SQL)
	}

	rows, err := eng.Query(context.Background(), govern.Identity{Subject: "analyst"},
		plan.Request{Metrics: []string{"largest_order"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := onlyValue(t, rows); got != "400.00" {
		t.Errorf("largest_order = %s, want 400.00. 475.50 would mean the maxima "+
			"were summed", got)
	}
}

// TestAGrainCoarserThanTheRollupIsServedFromIt, and the monthly totals are
// the ones the fact tables give.
func TestAGrainCoarserThanTheRollupIsServedFromIt(t *testing.T) {
	eng := rollupEngine(t, nil)

	req := plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.order_date"},
		Grain:      plan.GrainMonth,
	}
	c := compileRollup(t, eng, req)
	if len(c.Rollups) != 1 || c.Rollups[0] != "orders_by_region_month" {
		t.Fatalf("rollups = %v, want orders_by_region_month; SQL:\n%s", c.Rollups, c.SQL)
	}

	rows, err := eng.Query(context.Background(), govern.Identity{Subject: "analyst"}, req)
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]string{}
	for _, row := range rows.Rows {
		totals[strings.TrimSpace(fmt.Sprint(row[0]))] = strings.TrimSpace(fmt.Sprint(row[1]))
	}
	for month, want := range map[string]string{
		"2026-01-01": "100.00",
		"2026-02-01": "325.50",
		"2026-03-01": "460.00",
	} {
		if got := totals[month]; got != want {
			t.Errorf("%s = %q, want %s (from %v)", month, got, want, totals)
		}
	}
}

// TestAGrainFinerThanTheRollupFallsBackToTheFacts.
//
// The rollup stores months. The detail below a month is gone, and answering
// with months relabelled as days would be a wrong number under a correct
// column heading, which is the failure mode with no symptom.
func TestAGrainFinerThanTheRollupFallsBackToTheFacts(t *testing.T) {
	eng := rollupEngine(t, nil)

	req := plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.order_date"},
		Grain:      plan.GrainDay,
	}
	c := compileRollup(t, eng, req)
	if len(c.Rollups) != 0 {
		t.Fatalf("a daily question was answered from a monthly table: %v\n%s",
			c.Rollups, c.SQL)
	}

	rows, err := eng.Query(context.Background(), govern.Identity{Subject: "analyst"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 5 {
		t.Errorf("want one row per order date, got %d: %v", len(rows.Rows), rows.Rows)
	}
}

// TestADimensionTheRollupDoesNotHoldFallsBackToTheFacts, because the rollup
// has no column for it and there is nothing to group by.
func TestADimensionTheRollupDoesNotHoldFallsBackToTheFacts(t *testing.T) {
	eng := rollupEngine(t, nil)

	c := compileRollup(t, eng, plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.status"},
	})
	if len(c.Rollups) != 0 {
		t.Fatalf("routed to %v for a dimension it does not store:\n%s", c.Rollups, c.SQL)
	}
}

// TestAStaleRollupIsNotRead.
//
// The property a deployment's correctness rests on. orders_stale covers
// order_revenue by status exactly and would be chosen on structure alone;
// it was built in 2020 and holds numbers nowhere near the real ones, so if
// it were ever read this fails by a wide margin rather than by a rounding
// error.
func TestAStaleRollupIsNotRead(t *testing.T) {
	eng := rollupEngine(t, nil)

	req := plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.status"},
	}
	c := compileRollup(t, eng, req)
	for _, name := range c.Rollups {
		if name == "orders_stale" {
			t.Fatalf("a rollup last built in 2020 answered a question:\n%s", c.SQL)
		}
	}

	rows, err := eng.Query(context.Background(), govern.Identity{Subject: "analyst"}, req)
	if err != nil {
		t.Fatal(err)
	}
	byStatus := map[string]string{}
	for _, row := range rows.Rows {
		byStatus[fmt.Sprint(row[0])] = strings.TrimSpace(fmt.Sprint(row[1]))
	}
	if got := byStatus["shipped"]; got != "175.50" {
		t.Errorf("shipped revenue = %q, want 175.50. 1.00 means the stale table "+
			"was read", got)
	}
}

// TestHealthReportsWhyTheStaleRollupIsNotUsed, because a rollup silently
// never helping is a table somebody is paying to build for nothing, and
// nothing else in the system would ever mention it.
func TestHealthReportsWhyTheStaleRollupIsNotUsed(t *testing.T) {
	eng := rollupEngine(t, nil)

	// Two queries, because each probes only the rollups that cover it: the
	// status one reaches orders_stale and nothing else, and a test that ran
	// only that would find no fresh rollup to contrast it with.
	for _, req := range []plan.Request{
		{Metrics: []string{"order_revenue"}, Dimensions: []string{"orders.status"}},
		{Metrics: []string{"order_revenue"}},
	} {
		if _, err := eng.Query(context.Background(),
			govern.Identity{Subject: "analyst"}, req); err != nil {
			t.Fatal(err)
		}
	}

	var stale, fresh int
	for _, h := range eng.Health().Rollups {
		if !h.Checked {
			continue
		}
		if h.Fresh {
			fresh++
			continue
		}
		stale++
		if h.Name != "orders_stale" {
			t.Errorf("%s is reported stale and should not be", h.Name)
		}
		if h.AgeSeconds < 60*60*24*365 {
			t.Errorf("orders_stale reports an age of %ds, which is not five years",
				h.AgeSeconds)
		}
	}
	if stale != 1 {
		t.Errorf("want exactly the one stale rollup reported, got %d", stale)
	}
	if fresh == 0 {
		t.Error("no rollup was reported fresh, so this test proves nothing about " +
			"telling them apart")
	}
}

// TestTheAuditSaysWhichTableAnsweredIt.
//
// A number somebody does not believe is traced through the audit log, and
// "which table did this come from" is the first question about a number
// that came from a second copy of the data.
func TestTheAuditSaysWhichTableAnsweredIt(t *testing.T) {
	audit := &govern.MemoryAudit{}
	eng := rollupEngine(t, audit)

	if _, err := eng.Query(context.Background(), govern.Identity{Subject: "analyst"},
		plan.Request{Metrics: []string{"order_revenue"}}); err != nil {
		t.Fatal(err)
	}

	var allowed int
	for _, e := range audit.Events() {
		if e.Decision != "allowed" {
			continue
		}
		allowed++
		if len(e.Rollups) != 1 || e.Rollups[0] != "orders_by_region" {
			t.Errorf("the audit event does not name the table that answered: %v", e.Rollups)
		}
	}
	if allowed == 0 {
		t.Fatal("nothing was audited as allowed")
	}
}

// TestGovernanceStillSeesTheModelNotTheRollup.
//
// A rollup carries copies of governed columns. If the gate resolved policy
// against the rollup's own table instead of the model, a grant written on
// customers.region would stop applying the moment a rollup held it, which
// is an access control bypass dressed as an optimisation.
func TestGovernanceStillSeesTheModelNotTheRollup(t *testing.T) {
	db := duckdbtest.Seed(t, filepath.Join("..", ".."))

	dir := t.TempDir()
	for _, f := range []struct{ from, to string }{
		{filepath.Join("..", "..", "testdata", "models", "retail.yaml"), "retail.yaml"},
		{filepath.Join("..", "..", "testdata", "rollups", "rollups.yaml"), "rollups.yaml"},
	} {
		src, err := os.ReadFile(f.from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.to), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex, err := exec.NewDuckDB(exec.DuckDBOptions{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })

	// A resolver that denies the region dimension. If routing bypassed the
	// gate, the rollup's own `region` column would answer anyway.
	eng, err := engine.New(engine.Config{
		ModelPath: dir,
		Dialect:   "duckdb",
		Resolver:  denyRegion{},
		Executor:  ex,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = eng.Query(context.Background(), govern.Identity{Subject: "analyst"},
		plan.Request{
			Metrics:    []string{"order_revenue"},
			Dimensions: []string{"customers.region"},
		})
	if err == nil {
		t.Fatal("a denied column was read out of a rollup")
	}
}

// denyRegion denies the region dimension and allows everything else.
//
// Not embedding govern.AllowAll. An earlier version of this did, and
// spelled the method Resolve rather than CanRead, so the embedded
// allow-everything answered and the test reported that governance had been
// bypassed by rollup routing. It had not been; the fake had simply never
// been called. Declaring both methods explicitly means a signature that
// drifts is a compile error rather than a silently permissive test.
type denyRegion struct{}

func (denyRegion) CanRead(_ context.Context, _ govern.Identity, refs []govern.Ref) (govern.Decision, error) {
	for _, ref := range refs {
		if strings.EqualFold(ref.Field, "region") {
			return govern.Decision{Denied: []govern.Ref{ref}}, nil
		}
	}
	return govern.Decision{Allowed: true}, nil
}

func (denyRegion) Capabilities() govern.Capabilities {
	return govern.Capabilities{Resolver: "test-deny-region", ColumnLevel: true}
}
