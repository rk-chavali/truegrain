package dialect_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/dialect"
	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// update rewrites the golden files instead of comparing against them.
//
//	go test ./internal/dialect/ -update
var update = flag.Bool("update", false, "rewrite golden SQL files")

// Golden SQL is committed so that any change to the emitter shows up as a
// readable diff in a pull request. It is how a reviewer catches a planner
// change that still happens to return the right answer on the fixture data.

func planner(t *testing.T) *plan.Planner {
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

var cases = []struct {
	name string
	req  plan.Request
}{
	{"single_metric", plan.Request{
		Metrics: []string{"order_revenue"},
	}},
	{"metric_by_dimension", plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.region"},
		OrderBy:    []plan.Order{{Field: "order_revenue", Desc: true}},
		Limit:      10,
	}},
	{"time_grain_month", plan.Request{
		Metrics:    []string{"order_revenue", "order_count"},
		Dimensions: []string{"orders.order_date"},
		Grain:      plan.GrainMonth,
	}},
	{"two_hop_join", plan.Request{
		Metrics:    []string{"line_revenue", "units_sold"},
		Dimensions: []string{"customers.region", "order_lines.item_id"},
		Limit:      50,
	}},
	{"ratio_metric", plan.Request{
		Metrics:    []string{"average_order_value"},
		Dimensions: []string{"customers.region"},
	}},
	{"case_expression_metric", plan.Request{
		Metrics:    []string{"shipped_revenue"},
		Dimensions: []string{"orders.status"},
	}},
	{"computed_dimension", plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.email_domain"},
	}},
	{"all_filter_operators", plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.region"},
		Filters: []plan.Filter{
			{Dimension: "orders.status", Op: plan.OpIn, Values: []any{"shipped", "delivered"}},
			{Dimension: "customers.region", Op: plan.OpNe, Values: []any{"XX"}},
			{Dimension: "orders.order_date", Op: plan.OpBetween,
				Values: []any{"2026-01-01", "2026-12-31"}},
			{Dimension: "customers.signup_date", Op: plan.OpIsNotNil},
		},
	}},
}

func TestGoldenSQL(t *testing.T) {
	p := planner(t)
	for _, dialectName := range dialect.Names() {
		d, err := dialect.Get(dialectName)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			t.Run(dialectName+"/"+c.name, func(t *testing.T) {
				pl, err := p.Plan(c.req)
				if err != nil {
					t.Fatalf("planning %s:\n%v", c.name, err)
				}
				sql, params, err := d.Emit(p.Schema(), pl)
				if err != nil {
					t.Fatalf("emitting %s:\n%v", c.name, err)
				}
				got := render(sql, params)

				path := filepath.Join("..", "..", "testdata", "golden",
					fmt.Sprintf("%s.%s.sql", c.name, dialectName))
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("no golden file; run `go test ./internal/dialect/ -update`:\n%v", err)
				}
				if normalize(string(want)) != normalize(got) {
					t.Errorf("compiled SQL changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
				}
			})
		}
	}
}

// render writes the SQL plus its bind parameters. The parameters are part of
// the snapshot because a change that moves a value from a placeholder into the
// SQL text would otherwise pass silently, and that is exactly the change that
// must never happen.
func render(sql string, params []any) string {
	var b strings.Builder
	b.WriteString(sql)
	b.WriteString("\n")
	if len(params) > 0 {
		b.WriteString("\n-- parameters:\n")
		for i, p := range params {
			fmt.Fprintf(&b, "--   $%d = %#v\n", i+1, p)
		}
	}
	return b.String()
}

func normalize(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
}

// TestNoLiteralsInSQL is a standing guarantee rather than a snapshot. Filter
// values must reach the warehouse as bind parameters. If one ever appears in
// the SQL text, this fails regardless of what the golden files say.
func TestNoLiteralsInSQL(t *testing.T) {
	p := planner(t)
	const marker = "' OR 1=1 --"

	pl, err := p.Plan(plan.Request{
		Metrics: []string{"order_revenue"},
		Filters: []plan.Filter{
			{Dimension: "orders.status", Op: plan.OpEq, Values: []any{marker}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range dialect.Names() {
		d, _ := dialect.Get(name)
		sql, params, err := d.Emit(p.Schema(), pl)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sql, "OR 1=1") {
			t.Errorf("%s: filter value reached the SQL text:\n%s", name, sql)
		}
		if len(params) != 1 || params[0] != marker {
			t.Errorf("%s: want the value bound as a single parameter, got %#v", name, params)
		}
	}
}

// TestDialectsDifferWhereExpected guards the thing the parity suite cares
// about: two warehouses must produce different SQL from the same plan, and the
// differences must be confined to quoting, placeholders and date truncation.
func TestDialectsDifferWhereExpected(t *testing.T) {
	p := planner(t)
	pl, err := p.Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.order_date"},
		Grain:      plan.GrainMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	duck, _ := dialect.Get("duckdb")
	bq, _ := dialect.Get("bigquery")

	duckSQL, _, err := duck.Emit(p.Schema(), pl)
	if err != nil {
		t.Fatal(err)
	}
	bqSQL, _, err := bq.Emit(p.Schema(), pl)
	if err != nil {
		t.Fatal(err)
	}
	if duckSQL == bqSQL {
		t.Fatal("the two dialects emitted identical SQL, which means the emitter is not dialect aware")
	}
	if !strings.Contains(duckSQL, "date_trunc('month'") {
		t.Errorf("duckdb should use date_trunc:\n%s", duckSQL)
	}
	// order_date is declared Date, so BigQuery must use DATE_TRUNC rather than
	// TIMESTAMP_TRUNC, which would be a type error in the warehouse.
	if !strings.Contains(bqSQL, "DATE_TRUNC(") {
		t.Errorf("bigquery should use DATE_TRUNC for a Date column:\n%s", bqSQL)
	}
}

// TestBigQueryRefusesUndeclaredTemporalType covers the case where GoogleSQL
// cannot be emitted safely. Four truncation functions exist and picking wrongly
// is a runtime failure in the warehouse, so the engine refuses at compile time
// with a message naming the model fix.
func TestBigQueryRefusesUndeclaredTemporalType(t *testing.T) {
	src := strings.ReplaceAll(mustRead(t,
		filepath.Join("..", "..", "testdata", "models", "retail.yaml")),
		"            datatype: Date\n            dimension:\n              is_time: true\n            description: Date the order was placed.",
		"            dimension:\n              is_time: true\n            description: Date the order was placed.")

	dir := t.TempDir()
	path := filepath.Join(dir, "retail.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := osi.Load(path)
	if err != nil {
		t.Fatalf("loading edited fixture:\n%v", err)
	}
	s, err := resolve.New(m)
	if err != nil {
		t.Fatalf("resolving edited fixture:\n%v", err)
	}
	pl, err := plan.New(s).Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"orders.order_date"},
		Grain:      plan.GrainMonth,
	})
	if err != nil {
		t.Fatalf("planning should succeed; the problem is dialect specific:\n%v", err)
	}
	bq, _ := dialect.Get("bigquery")
	if _, _, err := bq.Emit(s, pl); err == nil {
		t.Fatal("BigQuery cannot choose a truncation function without a declared datatype and must refuse")
	} else if !strings.Contains(err.Error(), "datatype") {
		t.Errorf("refusal should name the missing datatype:\n%v", err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
