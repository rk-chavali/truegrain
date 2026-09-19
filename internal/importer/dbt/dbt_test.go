package dbt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/importer/dbt"
	"github.com/rk-chavali/truegrain/internal/introspect"
)

// Importing a dbt semantic layer.
//
// Two things are worth asserting and they pull in opposite directions.
//
// What comes across has to be faithful, because somebody is going to compare
// a number here against the same number in dbt and any difference will be
// read as this tool being wrong.
//
// What does not come across has to be named, because an importer that
// quietly drops a third of a catalogue is worse than one that refuses to
// run: the comparison above then happens against a model missing the metric
// nobody noticed was gone.

func importFixture(t *testing.T) (introspect.Schema, dbt.Report) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "fixtures", "dbt_manifest.json")
	s, r, err := dbt.Import(path)
	if err != nil {
		t.Fatalf("importing %s: %v", path, err)
	}
	return s, r
}

func table(t *testing.T, s introspect.Schema, name string) introspect.Table {
	t.Helper()
	for _, tbl := range s.Tables {
		if tbl.Name == name {
			return tbl
		}
	}
	t.Fatalf("dataset %q is missing", name)
	return introspect.Table{}
}

func metric(t *testing.T, s introspect.Schema, name string) introspect.Metric {
	t.Helper()
	for _, m := range s.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("metric %q is missing", name)
	return introspect.Metric{}
}

func skippedMentioning(r dbt.Report, needle string) string {
	for _, s := range r.Skipped {
		if strings.Contains(s, needle) {
			return s
		}
	}
	return ""
}

// TestEntitiesBecomeTheGrain.
//
// The reason a dbt import is worth doing at all. MetricFlow entities are the
// grain and the join graph, which is exactly what fan-out detection needs
// and exactly what an importer usually cannot recover.
func TestEntitiesBecomeTheGrain(t *testing.T) {
	s, _ := importFixture(t)

	for name, want := range map[string]string{
		"orders":      "order_id",
		"order_lines": "line_id",
		"customers":   "customer_id",
	} {
		got := strings.Join(table(t, s, name).PrimaryKey, ",")
		if got != want {
			t.Errorf("%s grain = %q, want %q", name, got, want)
		}
	}
}

// TestAnEntityIsReadByItsExpressionNotItsName.
//
// An entity called `customer` backed by `customer_id` is the common case.
// Reading the name would emit a join on a column the warehouse does not
// have, which fails loudly, and a primary key on one that does not exist,
// which does not.
func TestAnEntityIsReadByItsExpressionNotItsName(t *testing.T) {
	s, _ := importFixture(t)

	if got := table(t, s, "customers").PrimaryKey; len(got) != 1 || got[0] != "customer_id" {
		t.Errorf("PrimaryKey = %v, want [customer_id]: the entity is named `customer`", got)
	}

	var found bool
	for _, r := range s.Relationships {
		if r.From == "orders" && r.To == "customers" {
			found = true
			if r.FromColumns[0] != "customer_id" || r.ToColumns[0] != "customer_id" {
				t.Errorf("join columns = %v -> %v, want the expressions",
					r.FromColumns, r.ToColumns)
			}
		}
	}
	if !found {
		t.Error("orders -> customers was not imported")
	}
}

// TestAForeignEntityJoinsToWhicheverModelDeclaresItPrimary.
//
// That is how MetricFlow resolves a join. Resolving it any other way
// produces a join dbt would not have made, and a join nobody wrote is the
// worst kind: it runs and returns rows.
func TestAForeignEntityJoinsToWhicheverModelDeclaresItPrimary(t *testing.T) {
	s, r := importFixture(t)

	want := map[string]string{
		"order_lines": "orders",
		"orders":      "customers",
		"web_events":  "customers",
	}
	got := map[string]string{}
	for _, rel := range s.Relationships {
		got[rel.From] = rel.To
	}
	for from, to := range want {
		if got[from] != to {
			t.Errorf("%s joins to %q, want %q", from, got[from], to)
		}
	}
	if r.Relationships != len(want) {
		t.Errorf("reported %d relationships, want %d", r.Relationships, len(want))
	}
}

// TestAForeignEntityWithNoOwnerIsReportedNotInvented.
//
// web_events references a `campaign` entity no model declares. Inventing a
// dataset for it, or silently dropping it, both leave somebody wondering
// where their campaign breakdown went.
func TestAForeignEntityWithNoOwnerIsReportedNotInvented(t *testing.T) {
	s, r := importFixture(t)

	for _, rel := range s.Relationships {
		if rel.To == "campaign" || rel.To == "campaigns" {
			t.Errorf("a dataset was invented for an undeclared entity: %+v", rel)
		}
	}
	if skippedMentioning(r, "campaign") == "" {
		t.Errorf("the unresolvable join was not reported: %v", r.Skipped)
	}
}

// TestSimpleMetricsBecomeAggregates, in ANSI rather than the source
// warehouse's spelling, or the imported model would only run where it came
// from.
func TestSimpleMetricsBecomeAggregates(t *testing.T) {
	s, _ := importFixture(t)

	for name, want := range map[string]string{
		"revenue":        "SUM(orders.amount)",
		"line_revenue":   "SUM(order_lines.line_amount)",
		"orders_placed":  "COUNT(orders.order_id)",
		"customers_seen": "COUNT(DISTINCT customers.customer_id)",
	} {
		if got := metric(t, s, name).Expression; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestARatioBecomesADivisionThatCannotDivideByZero.
//
// dbt computes a ratio as two aggregates divided at the query grain, so
// this is a faithful import rather than an approximation. NULLIF is the
// difference between an empty group returning null and the query failing.
func TestARatioBecomesADivisionThatCannotDivideByZero(t *testing.T) {
	s, _ := importFixture(t)

	got := metric(t, s, "average_order_value").Expression
	if !strings.Contains(got, "NULLIF") {
		t.Errorf("a ratio without NULLIF divides by zero on an empty group: %s", got)
	}
	if !strings.Contains(got, "SUM(orders.amount)") || !strings.Contains(got, "COUNT(orders.order_id)") {
		t.Errorf("the ratio did not resolve both sides: %s", got)
	}
}

// TestWhatIsNotImportedIsNamed.
//
// The half that matters. Each of these is a metric somebody has in dbt and
// will not have here, and each has a different reason they need to know.
func TestWhatIsNotImportedIsNamed(t *testing.T) {
	s, r := importFixture(t)

	cases := map[string]string{
		"revenue_mtd":          "cumulative",
		"signup_to_order":      "conversion",
		"revenue_per_customer": "derived",
		"shipped_revenue":      "filter",
		"closing_balance":      "semi-additive",
		"median_order":         "not declared",
	}
	for name, because := range cases {
		line := skippedMentioning(r, name)
		if line == "" {
			t.Errorf("%s was dropped without being reported", name)
			continue
		}
		if !strings.Contains(line, because) {
			t.Errorf("%s reported without the reason %q: %s", name, because, line)
		}
		// And it really is absent, rather than reported and imported anyway.
		for _, m := range s.Metrics {
			if m.Name == name {
				t.Errorf("%s was reported as skipped and imported anyway", name)
			}
		}
	}
}

// TestASemiAdditiveMeasureIsRefusedRatherThanSummed.
//
// A balance that sums across customers and not across time is the classic
// way to get a plausible wrong number. dbt marks it; summing it anyway
// would be the engine discarding the one piece of information that said not
// to.
func TestASemiAdditiveMeasureIsRefusedRatherThanSummed(t *testing.T) {
	s, _ := importFixture(t)
	for _, m := range s.Metrics {
		if strings.Contains(m.Expression, "balance") {
			t.Errorf("a semi-additive measure was imported as %s", m.Expression)
		}
	}
}

// TestAnExpressionDimensionIsNotEmittedAsAColumn.
//
// `is_large: amount > 100` is computed, not physical. Emitting it as a
// field would name a column the warehouse does not have, and the model
// would fail at query time rather than at import.
func TestAnExpressionDimensionIsNotEmittedAsAColumn(t *testing.T) {
	s, r := importFixture(t)

	for _, c := range table(t, s, "orders").Columns {
		if c.Name == "is_large" || strings.Contains(c.Name, ">") {
			t.Errorf("an expression dimension was emitted as a column: %q", c.Name)
		}
	}
	if skippedMentioning(r, "is_large") == "" {
		t.Errorf("the expression dimension was dropped without being reported: %v", r.Skipped)
	}
}

// TestAKeylessModelIsReportedInTheUsualWords.
//
// web_events has no primary entity. It must be reported with the same
// warning a warehouse reader would give, or somebody learns the rule only
// from whichever source happened to produce it.
func TestAKeylessModelIsReportedInTheUsualWords(t *testing.T) {
	s, _ := importFixture(t)

	if key := table(t, s, "web_events").PrimaryKey; len(key) != 0 {
		t.Errorf("a grain was invented for a model that declares none: %v", key)
	}
	var noted bool
	for _, n := range s.Notes {
		if strings.Contains(n, "web_events") && strings.Contains(n, "fan-out") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("no note warns that web_events has no derivable grain: %v", s.Notes)
	}
}

// TestImportingTwiceProducesTheSameFile.
//
// Re-importing after a dbt change should be a readable diff. Go randomises
// map iteration, so this fails immediately if any part of the importer
// depends on it, and a manifest is all maps.
func TestImportingTwiceProducesTheSameFile(t *testing.T) {
	first, _ := importFixture(t)
	for i := range 4 {
		again, _ := importFixture(t)
		if first.Datasets("jaffle") != again.Datasets("jaffle") {
			t.Fatalf("run %d produced different datasets", i+2)
		}
		if first.MetricsFile("jaffle") != again.MetricsFile("jaffle") {
			t.Fatalf("run %d produced different metrics", i+2)
		}
	}
}

// TestAProjectWithNoSemanticModelsSaysWhatToDoInstead.
//
// A dbt project without the Semantic Layer blocks is the commonest thing
// somebody will point this at, and "no semantic models" on its own tells
// them nothing.
func TestAProjectWithNoSemanticModelsSaysWhatToDoInstead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	body := `{"metadata":{"project_name":"plain"},"nodes":{},"metrics":{}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := dbt.Import(path)
	if err == nil {
		t.Fatal("a project with no semantic models imported")
	}
	if !strings.Contains(err.Error(), "truegrain init") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

// TestAMissingManifestSaysHowToMakeOne.
func TestAMissingManifestSaysHowToMakeOne(t *testing.T) {
	_, _, err := dbt.Import(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("a missing manifest imported")
	}
	if !strings.Contains(err.Error(), "dbt parse") {
		t.Errorf("the error does not say how to produce one: %v", err)
	}
}

// TestTheAdapterIsReported, because a model compiled for the wrong dialect
// is correct SQL for a warehouse nobody is querying.
func TestTheAdapterIsReported(t *testing.T) {
	_, r := importFixture(t)
	var found bool
	for _, n := range r.Notes {
		if strings.Contains(n, "postgres") {
			found = true
		}
	}
	if !found {
		t.Errorf("the project's adapter was not reported: %v", r.Notes)
	}
}
