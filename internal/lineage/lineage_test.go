package lineage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rk-chavali/truegrain/internal/osi"
)

// What depends on what.
//
// The whole value of this is that it agrees with the compiler. A lineage
// answer that says a metric is safe when the metric will not compile is worse
// than no lineage, because somebody merged on the strength of it.
//
// So these are written against the shapes a model actually takes: a compound
// primary key, a field that is only a join column, a field nothing references,
// and a name that appears in two datasets.

const model = `
version: "0.2.0.dev0"
semantic_model:
  - name: retail
    datasets:
      - name: orders
        source: main.orders
        primary_key: [order_id]
        description: One row per order.
        fields:
          - name: order_id
            expression: {dialects: [{dialect: ANSI_SQL, expression: order_id}]}
            datatype: Integer
            description: Order identifier.
          - name: customer_id
            expression: {dialects: [{dialect: ANSI_SQL, expression: customer_id}]}
            datatype: Integer
            description: Foreign key to customers.
          - name: status
            expression: {dialects: [{dialect: ANSI_SQL, expression: status}]}
            datatype: String
            dimension: {}
            description: Order status.
          - name: order_total
            expression: {dialects: [{dialect: ANSI_SQL, expression: order_total}]}
            datatype: Decimal
            description: Order value.
      - name: order_lines
        source: main.order_lines
        primary_key: [order_id, line_number]
        description: One row per line.
        fields:
          - name: order_id
            expression: {dialects: [{dialect: ANSI_SQL, expression: order_id}]}
            datatype: Integer
            description: Foreign key to orders.
          - name: line_number
            expression: {dialects: [{dialect: ANSI_SQL, expression: line_number}]}
            datatype: Integer
            description: Line position.
          - name: status
            expression: {dialects: [{dialect: ANSI_SQL, expression: status}]}
            datatype: String
            dimension: {}
            description: A second field with the same name, on purpose.
          - name: quantity
            expression: {dialects: [{dialect: ANSI_SQL, expression: quantity}]}
            datatype: Integer
            description: Units on this line.
    relationships:
      - name: lines_to_orders
        from: order_lines
        to: orders
        from_columns: [order_id]
        to_columns: [order_id]
    metrics:
      - name: order_revenue
        expression:
          dialects: [{dialect: ANSI_SQL, expression: SUM(orders.order_total)}]
        datatype: Decimal
        description: Total value of all orders.
      - name: shipped_revenue
        expression:
          dialects:
            - dialect: ANSI_SQL
              expression: SUM(CASE WHEN orders.status = 'shipped' THEN orders.order_total ELSE 0 END)
        datatype: Decimal
        description: Revenue restricted to shipped orders.
      - name: units_sold
        expression:
          dialects: [{dialect: ANSI_SQL, expression: SUM(order_lines.quantity)}]
        datatype: Integer
        description: Units across all lines.
`

func build(t *testing.T) *Graph {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retail.yaml")
	if err := os.WriteFile(path, []byte(model), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := osi.Load(path)
	if err != nil {
		t.Fatalf("the fixture model does not parse: %v", err)
	}
	return Build("retail", m)
}

func TestAFieldNamesEveryMetricThatReadsIt(t *testing.T) {
	got := build(t).Field("orders.order_total")

	if !got.Found {
		t.Fatal("orders.order_total is declared and was reported as not found")
	}
	// Two metrics read it, and one of them reads it inside a CASE. A reverse
	// index built on anything less than the parsed expression misses that one.
	want := []string{"retail.order_revenue", "retail.shipped_revenue"}
	if !equal(got.Metrics, want) {
		t.Errorf("metrics = %v, want %v", got.Metrics, want)
	}
	if !got.Breaks() {
		t.Error("a field two metrics depend on was reported as breaking nothing")
	}
}

// TestTheSameFieldNameOnTwoDatasetsStaysApart.
//
// The reason this exists rather than a grep. `status` is declared on both
// datasets and only one of them is read by a metric. A text search answers
// this question wrongly and confidently.
func TestTheSameFieldNameOnTwoDatasetsStaysApart(t *testing.T) {
	g := build(t)

	if got := g.Field("orders.status").Metrics; !equal(got, []string{"retail.shipped_revenue"}) {
		t.Errorf("orders.status metrics = %v, want [retail.shipped_revenue]", got)
	}
	if got := g.Field("order_lines.status").Metrics; len(got) != 0 {
		t.Errorf("order_lines.status is read by no metric, got %v", got)
	}
}

// TestAJoinColumnIsReportedEvenWhenNoMetricReadsIt.
//
// orders.customer_id and order_lines.order_id are referenced by no metric at
// all. Dropping one does not break a metric, it disconnects the graph, and a
// disconnected graph turns questions that worked into refusals. That is a
// quieter failure and worth naming louder.
func TestAJoinColumnIsReportedEvenWhenNoMetricReadsIt(t *testing.T) {
	got := build(t).Field("order_lines.order_id")

	if len(got.Metrics) != 0 {
		t.Errorf("no metric reads this, got %v", got.Metrics)
	}
	if !equal(got.Relationships, []string{"lines_to_orders"}) {
		t.Errorf("relationships = %v, want [lines_to_orders]", got.Relationships)
	}
	if !equal(got.PrimaryKeyOf, []string{"retail.order_lines"}) {
		t.Errorf("primary key of = %v, want [retail.order_lines]", got.PrimaryKeyOf)
	}
	if !got.Breaks() {
		t.Error("a compound key column that carries a join was reported as safe to drop")
	}
}

// TestBothHalvesOfACompoundKeyAreKeyColumns, because dropping either one
// destroys the grain just as completely as dropping both.
func TestBothHalvesOfACompoundKeyAreKeyColumns(t *testing.T) {
	g := build(t)
	for _, f := range []string{"order_lines.order_id", "order_lines.line_number"} {
		if got := g.Field(f).PrimaryKeyOf; len(got) == 0 {
			t.Errorf("%s is half of the primary key and was not reported as one", f)
		}
	}
}

// TestAFieldNothingUsesIsDistinguishableFromATypo.
//
// Both produce an empty impact. Only one of them means the change is safe, so
// Found is the field that keeps a typo from reading as a green light.
func TestAFieldNothingUsesIsDistinguishableFromATypo(t *testing.T) {
	g := build(t)

	// Declared, read by nothing, not a key, not a dimension.
	unused := g.Field("order_lines.status")
	if !unused.Found {
		t.Error("a declared field was reported as not found")
	}

	typo := g.Field("orders.order_totl")
	if typo.Found {
		t.Error("a misspelled field was reported as found, which reads as safe to drop")
	}
	if typo.Breaks() {
		t.Error("a field that does not exist cannot break anything")
	}
}

func TestDroppingAWholeDatasetUnionsItsFields(t *testing.T) {
	got := build(t).Dataset("orders")

	if !got.Found {
		t.Fatal("orders is declared and was reported as not found")
	}
	want := []string{"retail.order_revenue", "retail.shipped_revenue"}
	if !equal(got.Metrics, want) {
		t.Errorf("metrics = %v, want %v", got.Metrics, want)
	}
	if !equal(got.Relationships, []string{"lines_to_orders"}) {
		t.Errorf("relationships = %v, want [lines_to_orders]", got.Relationships)
	}
	if len(got.Fields) != 4 {
		t.Errorf("orders has four fields, got %v", got.Fields)
	}
}

// TestAMetricReportsWhatItReads, which is the question asked of a number
// rather than of a column.
func TestAMetricReportsWhatItReads(t *testing.T) {
	got := build(t).Metric("shipped_revenue")

	if !got.Found {
		t.Fatal("shipped_revenue is declared and was reported as not found")
	}
	want := []string{"retail.orders.order_total", "retail.orders.status"}
	if !equal(got.Fields, want) {
		t.Errorf("fields = %v, want %v", got.Fields, want)
	}
}

// TestNamesAreQualified, so two namespaces each with an orders dataset do not
// report each other's metrics.
func TestNamesAreQualified(t *testing.T) {
	got := build(t).Field("orders.order_total")
	for _, m := range got.Metrics {
		if len(m) < 7 || m[:7] != "retail." {
			t.Errorf("%q is not namespace qualified", m)
		}
	}
}

// TestAGroupableFieldAloneIsNotBreakage.
//
// order_lines.status is a dimension and nothing else: no metric reads it, no
// join keys on it, it is not part of the grain. Counting "is groupable" as
// breakage made Breaks true for nearly every field in any model, which turns
// -strict into a check that always fails and therefore gets ignored.
//
// The risk is real and is reported separately: somebody may be grouping by it
// today, and the model cannot know.
func TestAGroupableFieldAloneIsNotBreakage(t *testing.T) {
	got := build(t).Field("order_lines.status")

	if !got.Dimension {
		t.Fatal("order_lines.status declares a dimension block and was not reported as groupable")
	}
	if got.Breaks() {
		t.Error("a field that is only groupable was counted as model breakage, " +
			"which makes -strict fail on almost every field")
	}
}

func TestAnEmptyModelAnswersWithoutPanicking(t *testing.T) {
	g := Build("", nil)
	if g.Field("a.b").Found || g.Dataset("a").Found || g.Metric("m").Found {
		t.Error("an empty graph found something")
	}
	// Empty rather than nil, so JSON renders [] and a reader can tell the
	// difference between nothing depends on this and nothing was computed.
	if g.Field("a.b").Metrics == nil {
		t.Error("Metrics is nil, which renders as null")
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
