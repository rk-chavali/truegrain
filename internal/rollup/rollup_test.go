package rollup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/resolve"
	"github.com/rk-chavali/truegrain/internal/rollup"
)

// What a rollup is allowed to declare.
//
// Everything refused here is refused at load, in front of whoever changed
// the model, rather than at query time in front of a caller who would have
// been handed a number. A rollup is a second copy of the data: the moment
// one is wrong, every question it covers is answered wrongly and nothing
// in the protocol says so.

func schema(t *testing.T) *resolve.Schema {
	t.Helper()
	model, err := osi.Load(filepath.Join("..", "..", "testdata", "models", "retail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := resolve.New(model)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func load(t *testing.T, body string) ([]*rollup.Rollup, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), rollup.FileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return rollup.Load(path, schema(t))
}

const valid = `
version: 1
rollups:
  - name: orders_by_region
    source: main.rollup_orders_by_region
    dimensions:
      - field: customers.region
        column: region
    metrics:
      - metric: order_revenue
        column: order_revenue
    freshness:
      column: built_at
      max_age: 26h
`

// TestASumReaggregatesWithSumAndAMaxWithMax, which is the easy half.
func TestASumReaggregatesWithSumAndAMaxWithMax(t *testing.T) {
	rs, err := load(t, `
version: 1
rollups:
  - name: r
    source: main.r
    dimensions: []
    metrics:
      - metric: order_revenue
      - metric: largest_order
    freshness: {column: built_at, max_age: 1h}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"order_revenue": "SUM", "largest_order": "MAX"}
	for _, m := range rs[0].Metrics {
		if m.Reaggregate != want[m.Metric] {
			t.Errorf("%s re-aggregates with %s, want %s",
				m.Metric, m.Reaggregate, want[m.Metric])
		}
	}
	// An omitted column defaults to the metric's own name, which is what
	// almost every rollup build produces.
	if rs[0].Metrics[0].Column != "order_revenue" {
		t.Errorf("column = %q, want the metric name", rs[0].Metrics[0].Column)
	}
}

// TestAStoredCountReaggregatesWithSum.
//
// The single most consequential line in this package. Counting the
// rollup's rows counts groups rather than the rows underneath them, so a
// stored COUNT recombined with COUNT returns the number of buckets: a
// smaller number, in the right units, that nothing would flag.
func TestAStoredCountReaggregatesWithSum(t *testing.T) {
	// A synthetic model, because the fixture's only count metric is a
	// COUNT(DISTINCT), which is holistic and cannot be rolled up at all.
	model, err := osi.LoadFiles([]string{writeModel(t, `
version: "0.2.0.dev0"
semantic_model:
  - name: tiny
    version: "1"
    datasets:
      - name: orders
        source: main.orders
        primary_key: [order_id]
        fields:
          - name: order_id
            expression:
              dialects: [{dialect: ANSI_SQL, expression: order_id}]
            datatype: Integer
    metrics:
      - name: order_count
        expression:
          dialects: [{dialect: ANSI_SQL, expression: COUNT(orders.order_id)}]
        datatype: Integer
        description: Number of order rows.
`)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := resolve.New(model)
	if err != nil {
		t.Fatal(err)
	}

	r := &rollup.Rollup{
		Name: "r", Source: "main.r",
		Metrics:   []rollup.Metric{{Metric: "order_count"}},
		Freshness: rollup.Freshness{Column: "built_at", MaxAge: "1h"},
	}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	if got := r.Metrics[0].Reaggregate; got != "SUM" {
		t.Errorf("a stored COUNT re-aggregates with %s. COUNT would return the "+
			"number of rollup rows, which is the number of groups", got)
	}
}

func writeModel(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAMetricThatCannotBeRecomputedIsRefused.
//
// Not warned about, not skipped: the whole file fails to load. A rollup
// listing a metric that cannot be recombined is one that will return a
// wrong number for it, and the author is the only person who can fix that.
func TestAMetricThatCannotBeRecomputedIsRefused(t *testing.T) {
	for _, c := range []struct{ metric, because string }{
		// COUNT(DISTINCT ...) is holistic: two buckets' distinct counts
		// cannot be combined without the values behind them.
		{"order_count", "holistic"},
		// A ratio is computed after aggregation and has no partial form.
		{"average_order_value", "not a single aggregate"},
	} {
		_, err := load(t, `
version: 1
rollups:
  - name: r
    source: main.r
    dimensions: []
    metrics:
      - metric: `+c.metric+`
    freshness: {column: built_at, max_age: 1h}
`)
		if err == nil {
			t.Errorf("%s was accepted into a rollup", c.metric)
			continue
		}
		if !strings.Contains(err.Error(), c.because) {
			t.Errorf("%s: the error does not say why: %v", c.metric, err)
		}
	}
}

// TestAFreshnessCheckIsRequired.
//
// The decision this whole feature rests on. A rollup nobody checks is a
// rollup nobody notices has stopped building, and the difference between
// that and no rollups at all is that one of them returns wrong numbers.
func TestAFreshnessCheckIsRequired(t *testing.T) {
	for _, c := range []struct{ name, freshness, want string }{
		{"absent", "freshness: {}", "no freshness check"},
		{"no column", "freshness: {max_age: 1h}", "no freshness check"},
		{"no age", "freshness: {column: built_at}", "no freshness check"},
		{"not a duration", "freshness: {column: built_at, max_age: 26}", "not a duration"},
		{"zero", "freshness: {column: built_at, max_age: 0s}", "not positive"},
		{"negative", "freshness: {column: built_at, max_age: -1h}", "not positive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, `
version: 1
rollups:
  - name: r
    source: main.r
    dimensions: []
    metrics: [{metric: order_revenue}]
    `+c.freshness+`
`)
			if err == nil {
				t.Fatal("accepted a rollup whose freshness cannot be checked")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not say what is wrong: %v", err)
			}
		})
	}
}

// TestATimeDimensionNeedsAGrain, because without one nothing can tell
// whether a request for monthly totals is answerable from the table or is
// asking for detail it does not hold.
func TestATimeDimensionNeedsAGrain(t *testing.T) {
	_, err := load(t, `
version: 1
rollups:
  - name: r
    source: main.r
    dimensions:
      - field: orders.order_date
    metrics: [{metric: order_revenue}]
    freshness: {column: built_at, max_age: 1h}
`)
	if err == nil {
		t.Fatal("a time dimension with no grain was accepted")
	}
	if !strings.Contains(err.Error(), "needs a grain") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// TestANameThatIsNotInTheModelIsRefused, so a rollup left behind by a model
// change fails at startup rather than routing a query to a table whose
// columns no longer mean what the file says.
func TestANameThatIsNotInTheModelIsRefused(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{
			"metric",
			"metrics: [{metric: no_such_metric}]\n    dimensions: []",
			"no metric",
		},
		{
			"dimension",
			"metrics: [{metric: order_revenue}]\n    dimensions: [{field: customers.nope}]",
			"no dimension",
		},
		{
			"not a dimension",
			"metrics: [{metric: order_revenue}]\n    dimensions: [{field: orders.order_total}]",
			"not a dimension",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, `
version: 1
rollups:
  - name: r
    source: main.r
    `+c.body+`
    freshness: {column: built_at, max_age: 1h}
`)
			if err == nil {
				t.Fatal("accepted a rollup naming something the model does not have")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not say what is wrong: %v", err)
			}
		})
	}
}

// TestAColumnNameIsAPlainIdentifier.
//
// The freshness probe formats the source and the freshness column into
// `SELECT MAX(col) FROM source`. Both come from a file an operator wrote
// and both are quoted on the way out, so this is a third layer rather than
// the only one. It is here because these are the only two values in the
// whole package that become part of a statement.
func TestAColumnNameIsAPlainIdentifier(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{
			"freshness column",
			"freshness: {column: \"built_at) FROM secrets --\", max_age: 1h}\n    dimensions: []\n    metrics: [{metric: order_revenue}]",
		},
		{
			"source",
			"source2: x\n    freshness: {column: built_at, max_age: 1h}\n    dimensions: []\n    metrics: [{metric: order_revenue}]",
		},
		{
			"metric column",
			"freshness: {column: built_at, max_age: 1h}\n    dimensions: []\n    metrics: [{metric: order_revenue, column: \"x\\\"; DROP TABLE orders; --\"}]",
		},
		{
			"dimension column",
			"freshness: {column: built_at, max_age: 1h}\n    metrics: [{metric: order_revenue}]\n    dimensions: [{field: customers.region, column: \"r-1\"}]",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := load(t, "version: 1\nrollups:\n  - name: r\n    source: main.r\n    "+c.body+"\n"); err == nil {
				t.Fatal("accepted a name that is not a plain identifier")
			}
		})
	}
}

// TestTheFileIsStrictAboutItsOwnShape, because a typo in a sidecar that
// loaded anyway would be a rollup silently not there.
func TestTheFileIsStrictAboutItsOwnShape(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"version", strings.Replace(valid, "version: 1", "version: 2", 1), "unsupported version"},
		{"unknown field", strings.Replace(valid, "    source:", "    sources:", 1), "field sources"},
		{"no name", strings.Replace(valid, "name: orders_by_region", "source2: x", 1), ""},
		{"duplicate name", valid + strings.SplitN(valid, "rollups:", 2)[1], "named"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := load(t, c.body); err == nil {
				t.Fatalf("accepted:\n%s", c.body)
			} else if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not say what is wrong: %v", err)
			}
		})
	}
}

// TestAMissingSidecarIsNotAnError, because rollups are an addition and
// every namespace that existed before them has none.
func TestAMissingSidecarIsNotAnError(t *testing.T) {
	rs, err := rollup.Load(filepath.Join(t.TempDir(), rollup.FileName), schema(t))
	if err != nil {
		t.Fatalf("a namespace with no rollups failed to load: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("got %d rollups from nothing", len(rs))
	}
}

// TestADirectoryLoadSkipsTheSidecars.
//
// internal/osi walks a model directory and parses every YAML in it, which
// is right for a model split across files and wrong for the files this
// engine puts beside one. The skip list lives in osi because config and
// rollup both depend on it and cannot be depended on back, so it is a
// second copy of these names and this is what keeps the two in step.
func TestADirectoryLoadSkipsTheSidecars(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "models", "retail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "retail.yaml"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sidecar := range []string{rollup.FileName, "truegrain.yaml", "namespace.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, sidecar), []byte("version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := osi.Load(dir); err != nil {
		t.Fatalf("a sidecar was parsed as a model: %v", err)
	}
}
