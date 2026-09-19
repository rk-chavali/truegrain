package plan_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

func fixture(t *testing.T) *plan.Planner {
	t.Helper()
	m, err := osi.Load(filepath.Join("..", "..", "testdata", "models", "retail.yaml"))
	if err != nil {
		t.Fatalf("loading fixture model:\n%v", err)
	}
	s, err := resolve.New(m)
	if err != nil {
		t.Fatalf("resolving fixture model:\n%v", err)
	}
	return plan.New(s)
}

// refusalCode extracts the machine-readable code from an error, which is what
// an interface branches on.
func refusalCode(err error) string {
	var r *plan.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// TestFanOutRefused is the test that matters most.
//
// order_revenue sums a column on the order header. Grouping it by a dimension
// that lives on order_lines forces a one-to-many join, which duplicates each
// order once per line. The sum would then be inflated by exactly the line count
// of each order, and it would look entirely plausible. The planner must refuse.
func TestFanOutRefused(t *testing.T) {
	p := fixture(t)
	_, err := p.Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"order_lines.item_id"},
	})
	if err == nil {
		t.Fatal("planner allowed a SUM over orders across a one-to-many join to order_lines; " +
			"this returns a silently inflated total")
	}
	if got := refusalCode(err); got != plan.CodeFanOut {
		t.Fatalf("want refusal code %q, got %q from: %v", plan.CodeFanOut, got, err)
	}
	// The refusal has to name the metric and the relationship, otherwise
	// nobody can act on it.
	for _, want := range []string{"order_revenue", "lines_to_orders", "order_lines"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
}

// TestFanOutSafeAggregatesAllowed pins the other half of the rule. An aggregate
// that is idempotent under row duplication, or that discards duplicates, is
// still correct across the same fan-out join and must not be refused.
func TestFanOutSafeAggregatesAllowed(t *testing.T) {
	p := fixture(t)
	for _, metric := range []string{"order_count", "largest_order"} {
		t.Run(metric, func(t *testing.T) {
			if _, err := p.Plan(plan.Request{
				Metrics:    []string{metric},
				Dimensions: []string{"order_lines.item_id"},
			}); err != nil {
				t.Fatalf("%s survives row duplication and should be allowed across the join:\n%v", metric, err)
			}
		})
	}
}

// TestManyToOneJoinAllowed confirms the planner does not refuse the safe
// direction. Walking from the fact to its parent cannot duplicate fact rows.
func TestManyToOneJoinAllowed(t *testing.T) {
	p := fixture(t)
	pl, err := p.Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.region"},
	})
	if err != nil {
		t.Fatalf("orders to customers is many-to-one and must be allowed:\n%v", err)
	}
	if pl.Base.Name != "orders" {
		t.Errorf("base should be the fact, got %q", pl.Base.Name)
	}
	if len(pl.Joins) != 1 {
		t.Fatalf("want 1 join, got %d", len(pl.Joins))
	}
	if pl.Joins[0].Duplicates() {
		t.Error("orders to customers joins on the customers primary key and cannot duplicate")
	}
}

// TestTwoHopJoin walks order_lines to customers through orders. Both hops are
// many-to-one, so the whole path is safe even though it is two joins long.
func TestTwoHopJoin(t *testing.T) {
	p := fixture(t)
	pl, err := p.Plan(plan.Request{
		Metrics:    []string{"line_revenue"},
		Dimensions: []string{"customers.region"},
	})
	if err != nil {
		t.Fatalf("two many-to-one hops should be allowed:\n%v", err)
	}
	if pl.Base.Name != "order_lines" {
		t.Errorf("base should be order_lines, got %q", pl.Base.Name)
	}
	if len(pl.Joins) != 2 {
		t.Fatalf("want 2 joins, got %d", len(pl.Joins))
	}
	for _, j := range pl.Joins {
		if j.Duplicates() {
			t.Errorf("join %q should not duplicate", j.Rel.Name)
		}
	}
}

// TestChasmRefused covers the second join trap. Asking for a metric on the
// order header and a metric on the lines together forces the same one-to-many
// join, so the header sum inflates. This is the chasm case and it falls out of
// the same rule rather than needing its own.
func TestChasmRefused(t *testing.T) {
	p := fixture(t)
	_, err := p.Plan(plan.Request{Metrics: []string{"order_revenue", "line_revenue"}})
	if err == nil {
		t.Fatal("summing a header measure and a line measure in one query inflates the header sum")
	}
	if got := refusalCode(err); got != plan.CodeFanOut {
		t.Fatalf("want %q, got %q from: %v", plan.CodeFanOut, got, err)
	}
}

// TestRatioMetricSpansOneDataset checks a ratio that divides two aggregates.
// It must be computed after aggregation, which it is by construction: the whole
// expression sits in the SELECT list, so the division happens on the grouped
// result, never row by row.
func TestRatioMetric(t *testing.T) {
	p := fixture(t)
	pl, err := p.Plan(plan.Request{
		Metrics:    []string{"average_order_value"},
		Dimensions: []string{"customers.region"},
	})
	if err != nil {
		t.Fatalf("ratio metric should plan:\n%v", err)
	}
	if len(pl.Metrics) != 1 || pl.Metrics[0].Alias != "average_order_value" {
		t.Fatalf("unexpected metric selection: %+v", pl.Metrics)
	}
}

func TestUnknownMetricSuggests(t *testing.T) {
	p := fixture(t)
	_, err := p.Plan(plan.Request{Metrics: []string{"order_revenu"}})
	if err == nil {
		t.Fatal("expected a refusal for a misspelled metric")
	}
	if got := refusalCode(err); got != plan.CodeUnknownMetric {
		t.Fatalf("want %q, got %q", plan.CodeUnknownMetric, got)
	}
	if !strings.Contains(err.Error(), "order_revenue") {
		t.Errorf("refusal should suggest the real metric name:\n%v", err)
	}
}

func TestNoMetricsRefused(t *testing.T) {
	p := fixture(t)
	if _, err := p.Plan(plan.Request{Dimensions: []string{"customers.region"}}); err == nil {
		t.Fatal("a request with no metrics must be refused")
	} else if got := refusalCode(err); got != plan.CodeNoMetrics {
		t.Fatalf("want %q, got %q", plan.CodeNoMetrics, got)
	}
}

func TestGrainRequiresTimeDimension(t *testing.T) {
	p := fixture(t)
	_, err := p.Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.region"},
		Grain:      plan.GrainMonth,
	})
	if err == nil {
		t.Fatal("a grain with no time dimension selected must be refused, not ignored")
	}
	if got := refusalCode(err); got != plan.CodeBadGrain {
		t.Fatalf("want %q, got %q", plan.CodeBadGrain, got)
	}
}

func TestGrainAppliesToTimeDimensionOnly(t *testing.T) {
	p := fixture(t)
	pl, err := p.Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.order_date", "orders.status"},
		Grain:      plan.GrainMonth,
	})
	if err != nil {
		t.Fatalf("unexpected refusal:\n%v", err)
	}
	for _, d := range pl.Dimensions {
		switch d.Field.Name {
		case "order_date":
			if d.Grain != plan.GrainMonth {
				t.Errorf("time dimension should carry the grain, got %q", d.Grain)
			}
		case "status":
			if d.Grain != "" {
				t.Errorf("a categorical dimension must not be truncated, got grain %q", d.Grain)
			}
		}
	}
}

func TestDefaultLimitApplied(t *testing.T) {
	p := fixture(t)
	pl, err := p.Plan(plan.Request{Metrics: []string{"order_revenue"}})
	if err != nil {
		t.Fatal(err)
	}
	if pl.Limit != plan.DefaultLimit {
		t.Errorf("want the default limit %d applied, got %d", plan.DefaultLimit, pl.Limit)
	}
}

func TestLimitBounds(t *testing.T) {
	p := fixture(t)
	for name, limit := range map[string]int{"negative": -1, "above maximum": plan.MaxLimit + 1} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Plan(plan.Request{Metrics: []string{"order_revenue"}, Limit: limit})
			if got := refusalCode(err); got != plan.CodeBadLimit {
				t.Fatalf("want %q, got %q (err: %v)", plan.CodeBadLimit, got, err)
			}
		})
	}
}

func TestFilterValidation(t *testing.T) {
	p := fixture(t)
	cases := []struct {
		name   string
		filter plan.Filter
		want   string
	}{
		{"unknown dimension",
			plan.Filter{Dimension: "customers.regio", Op: plan.OpEq, Values: []any{"NE"}},
			plan.CodeUnknownDimension},
		{"unknown operator",
			plan.Filter{Dimension: "customers.region", Op: "matches", Values: []any{"NE"}},
			plan.CodeBadFilter},
		{"wrong value count",
			plan.Filter{Dimension: "customers.region", Op: plan.OpEq, Values: []any{"NE", "MW"}},
			plan.CodeBadFilter},
		{"between needs two",
			plan.Filter{Dimension: "orders.order_date", Op: plan.OpBetween, Values: []any{"2026-01-01"}},
			plan.CodeBadFilter},
		{"non-date for a date dimension",
			plan.Filter{Dimension: "orders.order_date", Op: plan.OpEq, Values: []any{"last tuesday"}},
			plan.CodeBadFilter},
		{"null value",
			plan.Filter{Dimension: "customers.region", Op: plan.OpEq, Values: []any{nil}},
			plan.CodeBadFilter},
		{"reversed between",
			plan.Filter{Dimension: "orders.order_date", Op: plan.OpBetween,
				Values: []any{"2026-12-31", "2026-01-01"}},
			plan.CodeBadFilter},
		{"is_null takes no values",
			plan.Filter{Dimension: "customers.region", Op: plan.OpIsNull, Values: []any{"NE"}},
			plan.CodeBadFilter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.Plan(plan.Request{
				Metrics: []string{"order_revenue"},
				Filters: []plan.Filter{c.filter},
			})
			if got := refusalCode(err); got != c.want {
				t.Fatalf("want %q, got %q (err: %v)", c.want, got, err)
			}
		})
	}
}

func TestFilterValuesNormalized(t *testing.T) {
	p := fixture(t)
	pl, err := p.Plan(plan.Request{
		Metrics: []string{"order_revenue"},
		Filters: []plan.Filter{
			// A date arrives as a string and must come out as a time value, so
			// every dialect binds it as a real date rather than as text.
			{Dimension: "orders.order_date", Op: plan.OpGte, Values: []any{"2026-01-01"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Filters) != 1 {
		t.Fatalf("want 1 filter, got %d", len(pl.Filters))
	}
	// A calendar date, not a timestamp at midnight. The declared type has to
	// survive this far: BigQuery refuses to compare a DATE column against a
	// TIMESTAMP parameter, so flattening the two here produces a query the
	// warehouse rejects outright.
	got, ok := pl.Filters[0].Values[0].(plan.Date)
	if !ok {
		t.Fatalf("a Date dimension should normalise to plan.Date, got %T", pl.Filters[0].Values[0])
	}
	if got.String() != "2026-01-01" {
		t.Errorf("got %s", got)
	}
}

func TestOrderByMustNameAnOutputColumn(t *testing.T) {
	p := fixture(t)
	_, err := p.Plan(plan.Request{
		Metrics: []string{"order_revenue"},
		OrderBy: []plan.Order{{Field: "customers.region"}},
	})
	if got := refusalCode(err); got != plan.CodeBadOrder {
		t.Fatalf("want %q, got %q (err: %v)", plan.CodeBadOrder, got, err)
	}
}

func TestPlanFieldsCoversEverythingRead(t *testing.T) {
	p := fixture(t)
	pl, err := p.Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.region"},
		Filters:    []plan.Filter{{Dimension: "orders.status", Op: plan.OpEq, Values: []any{"shipped"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The governance gate resolves access against exactly this set, so a field
	// missing here is a field whose policy tag never gets checked.
	want := map[string]bool{
		"customers.region":   true,
		"orders.status":      true,
		"orders.order_total": true,
	}
	got := map[string]bool{}
	for _, f := range pl.Fields(p.Schema()) {
		got[f.QualifiedName()] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("plan reads %s but Fields() omits it, so its access would never be checked", w)
		}
	}
}
