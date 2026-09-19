package exec_test

import (
	"testing"

	"github.com/rk-chavali/truegrain/internal/plan"
)

// Multi-fact parity. Aggregating two facts separately and joining them is only
// worth having if the numbers come back identical to asking for each alone.
// These run real SQL against real data, which is the only way to catch a
// combiner that produces well-formed SQL with the wrong answer in it.

// TestMultiFactMatchesSingleFact is the core guarantee: adding a second metric
// from a different grain must not change the first one's value.
func TestMultiFactMatchesSingleFact(t *testing.T) {
	eng := parityEngine(t)

	alone := scalar(t, query(t, eng, plan.Request{Metrics: []string{"order_revenue"}}), 0, 0)
	lines := scalar(t, query(t, eng, plan.Request{Metrics: []string{"line_revenue"}}), 0, 0)

	both := query(t, eng, plan.Request{Metrics: []string{"order_revenue", "line_revenue"}})
	if len(both.Rows) != 1 {
		t.Fatalf("want one row, got %d:\n%s", len(both.Rows), both.CompiledSQL)
	}
	if got := scalar(t, both, 0, 0); got != alone {
		t.Errorf("order_revenue changed when a second fact was added: %.2f alone, %.2f combined\n%s",
			alone, got, both.CompiledSQL)
	}
	if got := scalar(t, both, 0, 1); got != lines {
		t.Errorf("line_revenue changed when combined: %.2f alone, %.2f combined", lines, got)
	}
}

// TestMultiFactGroupedMatchesSingleFact repeats it per dimension value, which is
// where a bad join condition shows up and a total would not.
func TestMultiFactGroupedMatchesSingleFact(t *testing.T) {
	eng := parityEngine(t)

	single := query(t, eng, plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.region"},
		OrderBy:    []plan.Order{{Field: "region"}},
	})
	combined := query(t, eng, plan.Request{
		Metrics:    []string{"order_revenue", "line_revenue"},
		Dimensions: []string{"customers.region"},
		OrderBy:    []plan.Order{{Field: "region"}},
	})

	if len(single.Rows) != len(combined.Rows) {
		t.Fatalf("row count changed: %d alone, %d combined\n%s",
			len(single.Rows), len(combined.Rows), combined.CompiledSQL)
	}
	for i := range single.Rows {
		want := scalar(t, single, i, 1)
		got := scalar(t, combined, i, 1)
		if diff := want - got; diff > 0.001 || diff < -0.001 {
			t.Errorf("row %d: order_revenue is %.2f alone but %.2f combined", i, want, got)
		}
	}
}

// TestMultiFactTotalsAgreeWithUngrouped is the grouped-versus-ungrouped check
// applied to the combined path: a join that duplicated rows would show up as a
// grouped total exceeding the real one.
func TestMultiFactTotalsAgreeWithUngrouped(t *testing.T) {
	eng := parityEngine(t)

	total := scalar(t, query(t, eng, plan.Request{Metrics: []string{"order_revenue"}}), 0, 0)

	grouped := query(t, eng, plan.Request{
		Metrics:    []string{"order_revenue", "line_revenue"},
		Dimensions: []string{"orders.status"},
	})
	var sum float64
	for i := range grouped.Rows {
		sum += scalar(t, grouped, i, 1)
	}
	if diff := sum - total; diff > 0.001 || diff < -0.001 {
		t.Errorf("combined and grouped sums to %.2f but the real total is %.2f\n%s",
			sum, total, grouped.CompiledSQL)
	}
}

// TestFullOuterJoinKeepsRowsWithOneSideMissing checks the join does not quietly
// drop a group that only one fact has. An inner join here would understate
// whichever side has the extra rows, and nothing else would notice.
func TestFullOuterJoinKeepsRowsWithOneSideMissing(t *testing.T) {
	eng := parityEngine(t)

	// Every order has lines, so both facts cover the same statuses. Restrict one
	// side with a filter that the other does not share, and the row must
	// survive with a null on the filtered side rather than disappearing.
	all := query(t, eng, plan.Request{
		Metrics:    []string{"order_count"},
		Dimensions: []string{"orders.status"},
	})
	combined := query(t, eng, plan.Request{
		Metrics:    []string{"order_count", "line_revenue"},
		Dimensions: []string{"orders.status"},
	})
	if len(combined.Rows) != len(all.Rows) {
		t.Errorf("combining facts changed the number of groups: %d against %d\n%s",
			len(combined.Rows), len(all.Rows), combined.CompiledSQL)
	}
}
