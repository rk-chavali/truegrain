package exec_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	sexec "github.com/rk-chavali/truegrain/internal/exec"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// The correctness parity suite. It runs real SQL against real data, which is
// the only way to catch a planner that produces well-formed SQL with the wrong
// answer in it.
//
// It skips when the duckdb binary is absent rather than failing, so that
// `go test ./...` stays green on a machine with no database. CI installs it and
// the suite runs on every pull request; see the Makefile.

func duckdbOrSkip(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("duckdb")
	if err != nil {
		t.Skip("duckdb binary not on PATH; install it to run the parity suite (make demo)")
	}
	return path
}

// seedDB builds a fresh database from the fixture and returns its path.
func seedDB(t *testing.T) string {
	t.Helper()
	duckdbOrSkip(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "parity.duckdb")

	seed, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("duckdb", db)
	cmd.Stdin = bytesReader(seed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding duckdb: %v\n%s", err, out)
	}
	return db
}

func parityEngine(t *testing.T) *engine.Engine {
	t.Helper()
	db := seedDB(t)
	ex, err := sexec.NewDuckDB(sexec.DuckDBOptions{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })

	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "testdata", "models", "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Executor:  ex,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func query(t *testing.T, eng *engine.Engine, req plan.Request) *engine.Result {
	t.Helper()
	res, err := eng.Query(context.Background(), govern.Identity{Subject: "parity"}, req)
	if err != nil {
		t.Fatalf("query failed:\n%v", err)
	}
	return res
}

// scalar reads a single numeric cell.
func scalar(t *testing.T, res *engine.Result, row, col int) float64 {
	t.Helper()
	if row >= len(res.Rows) || col >= len(res.Rows[row]) {
		t.Fatalf("no cell at row %d column %d in %+v", row, col, res.Rows)
	}
	switch v := res.Rows[row][col].(type) {
	case float64:
		return v
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
			t.Fatalf("cell %q is not numeric: %v", v, err)
		}
		return f
	}
	t.Fatalf("cell %v has unexpected type %T", res.Rows[row][col], res.Rows[row][col])
	return 0
}

// TestTotalsAgreeWithAndWithoutADimension is the test that catches most real
// bugs, straight out of docs/06-validation.md.
//
// Summing a measure grouped by a dimension must give the same total as summing
// it ungrouped. If a join silently fans out, the grouped total is larger and
// nothing else in the system notices.
func TestTotalsAgreeWithAndWithoutADimension(t *testing.T) {
	eng := parityEngine(t)

	ungrouped := query(t, eng, plan.Request{Metrics: []string{"order_revenue"}})
	total := scalar(t, ungrouped, 0, 0)

	for _, dim := range []string{"customers.region", "orders.status", "orders.order_date"} {
		t.Run(dim, func(t *testing.T) {
			grouped := query(t, eng, plan.Request{
				Metrics: []string{"order_revenue"}, Dimensions: []string{dim},
			})
			var sum float64
			for i := range grouped.Rows {
				sum += scalar(t, grouped, i, 1)
			}
			if diff := sum - total; diff > 0.001 || diff < -0.001 {
				t.Errorf("order_revenue grouped by %s sums to %.2f but ungrouped is %.2f; "+
					"a join is duplicating rows\n%s", dim, sum, total, grouped.CompiledSQL)
			}
		})
	}
}

// TestKnownGoodAnswers pins the numbers against values computed by hand from
// testdata/fixtures/seed.sql. These are the oracle, and they are reviewed as
// carefully as the engine.
func TestKnownGoodAnswers(t *testing.T) {
	eng := parityEngine(t)

	cases := []struct {
		name string
		req  plan.Request
		want float64
	}{
		// 100.00 + 250.00 + 75.50 + 400.00 + 60.00
		{"order_revenue", plan.Request{Metrics: []string{"order_revenue"}}, 885.50},
		// The same money counted from the line detail instead of the header.
		{"line_revenue", plan.Request{Metrics: []string{"line_revenue"}}, 885.50},
		{"order_count", plan.Request{Metrics: []string{"order_count"}}, 5},
		{"largest_order", plan.Request{Metrics: []string{"largest_order"}}, 400.00},
		// 885.50 / 5
		{"average_order_value", plan.Request{Metrics: []string{"average_order_value"}}, 177.10},
		// 1+2+1 + 5 + 1+3 + 2+2+2+2 + 1
		{"units_sold", plan.Request{Metrics: []string{"units_sold"}}, 22},
		// Only orders 1 and 3 are shipped: 100.00 + 75.50
		{"shipped_revenue", plan.Request{Metrics: []string{"shipped_revenue"}}, 175.50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scalar(t, query(t, eng, c.req), 0, 0)
			if diff := got - c.want; diff > 0.001 || diff < -0.001 {
				t.Errorf("want %.2f, got %.2f", c.want, got)
			}
		})
	}
}

// TestHeaderAndLineTotalsMatch is the parity check between two ways of counting
// the same money. They agree in the fixture by construction, so a disagreement
// means the engine changed one of them.
func TestHeaderAndLineTotalsMatch(t *testing.T) {
	eng := parityEngine(t)
	header := scalar(t, query(t, eng, plan.Request{Metrics: []string{"order_revenue"}}), 0, 0)
	lines := scalar(t, query(t, eng, plan.Request{Metrics: []string{"line_revenue"}}), 0, 0)
	if diff := header - lines; diff > 0.001 || diff < -0.001 {
		t.Errorf("order_revenue is %.2f but line_revenue is %.2f; the same money counted two ways must agree",
			header, lines)
	}
}

// TestFanOutWouldHaveBeenWrong demonstrates the size of the error the planner
// prevents. It runs the inflated SQL directly, proving the refusal is not
// conservatism: the naive answer really is wrong.
func TestFanOutWouldHaveBeenWrong(t *testing.T) {
	db := seedDB(t)
	out, err := exec.Command("duckdb", db, "-noheader", "-list", "-c",
		`SELECT SUM(o.order_total) FROM main.orders o
		 LEFT JOIN main.order_lines l ON o.order_id = l.order_id`).CombinedOutput()
	if err != nil {
		t.Fatalf("running the naive join: %v\n%s", err, out)
	}
	var inflated float64
	if _, err := fmt.Sscanf(string(out), "%g", &inflated); err != nil {
		t.Fatalf("parsing %q: %v", out, err)
	}
	const correct = 885.50
	if inflated <= correct {
		t.Fatalf("the fixture no longer demonstrates fan-out: naive join gives %.2f, correct is %.2f",
			inflated, correct)
	}
	t.Logf("the join the planner refuses would have answered %.2f instead of %.2f, "+
		"an overstatement of %.0f%%", inflated, correct, (inflated/correct-1)*100)

	// And the engine refuses to produce it.
	eng := parityEngine(t)
	if _, err := eng.Query(context.Background(), govern.Identity{Subject: "parity"},
		plan.Request{Metrics: []string{"order_revenue"}, Dimensions: []string{"order_lines.item_id"}}); err == nil {
		t.Error("the engine ran the query that produces the inflated number")
	}
}

// TestFiltersBindCorrectly checks that a validated filter value survives the
// round trip into the warehouse with its type intact.
func TestFiltersBindCorrectly(t *testing.T) {
	eng := parityEngine(t)

	// Orders 1 and 3 are shipped: 100.00 + 75.50.
	res := query(t, eng, plan.Request{
		Metrics: []string{"order_revenue"},
		Filters: []plan.Filter{{Dimension: "orders.status", Op: plan.OpEq, Values: []any{"shipped"}}},
	})
	if got := scalar(t, res, 0, 0); got != 175.50 {
		t.Errorf("string filter: want 175.50, got %.2f\n%s", got, res.CompiledSQL)
	}

	// Orders on or after 2026-03-01: 400.00 + 60.00.
	res = query(t, eng, plan.Request{
		Metrics: []string{"order_revenue"},
		Filters: []plan.Filter{{Dimension: "orders.order_date", Op: plan.OpGte, Values: []any{"2026-03-01"}}},
	})
	if got := scalar(t, res, 0, 0); got != 460.00 {
		t.Errorf("date filter: want 460.00, got %.2f\n%s", got, res.CompiledSQL)
	}
}

// TestMonthGrainBuckets checks the time truncation actually groups.
func TestMonthGrainBuckets(t *testing.T) {
	eng := parityEngine(t)
	res := query(t, eng, plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.order_date"},
		Grain:      plan.GrainMonth,
		OrderBy:    []plan.Order{{Field: "order_date"}},
	})
	// January, February, March 2026.
	if len(res.Rows) != 3 {
		t.Fatalf("want 3 monthly buckets, got %d:\n%+v\n%s", len(res.Rows), res.Rows, res.CompiledSQL)
	}
	want := []float64{100.00, 325.50, 460.00}
	for i, w := range want {
		if got := scalar(t, res, i, 1); got != w {
			t.Errorf("bucket %d: want %.2f, got %.2f", i, w, got)
		}
	}
}

func bytesReader(b []byte) *os.File {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	go func() {
		defer w.Close()
		w.Write(b)
	}()
	return r
}
