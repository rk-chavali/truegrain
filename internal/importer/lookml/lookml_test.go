package lookml_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/importer/lookml"
	"github.com/rk-chavali/truegrain/internal/introspect"
)

// Importing a Looker project.
//
// The join cardinality is what makes this worth doing: Looker states it,
// so nothing has to be inferred. Most of what is asserted here is that the
// stated cardinality reaches the model the right way round, because a join
// oriented backwards passes the fan-out check it should fail, which is the
// one failure that produces a confident wrong number.

func fixture(t *testing.T) (introspect.Schema, lookml.Report) {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "testdata", "fixtures", "looker")
	s, r, err := lookml.Import(dir)
	if err != nil {
		t.Fatalf("importing %s: %v", dir, err)
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

func skippedMentioning(r lookml.Report, needle string) string {
	for _, s := range r.Skipped {
		if strings.Contains(s, needle) {
			return s
		}
	}
	return ""
}

// TestAOneToManyJoinPointsFromTheManySide.
//
// The property everything depends on. Looker says orders one_to_many
// order_lines, which means order_lines is the many side. Ossie
// relationships point from many to one, so this has to come out as
// order_lines -> orders. Backwards, the planner would believe orders was
// unique per line and let a fan-out through.
func TestAOneToManyJoinPointsFromTheManySide(t *testing.T) {
	s, _ := fixture(t)

	var found bool
	for _, r := range s.Relationships {
		if r.From == "order_lines" && r.To == "orders" {
			found = true
			if r.FromColumns[0] != "order_id" || r.ToColumns[0] != "order_id" {
				t.Errorf("columns = %v -> %v", r.FromColumns, r.ToColumns)
			}
		}
		if r.From == "orders" && r.To == "order_lines" {
			t.Error("a one_to_many join was imported pointing the wrong way; the " +
				"planner would believe orders was unique per line and let a " +
				"fan-out through")
		}
	}
	if !found {
		t.Errorf("order_lines -> orders is missing; got %+v", s.Relationships)
	}
}

// TestAManyToOneJoinKeepsItsDirection.
func TestAManyToOneJoinKeepsItsDirection(t *testing.T) {
	s, _ := fixture(t)
	for _, r := range s.Relationships {
		if r.From == "orders" && r.To == "customers" {
			return
		}
	}
	t.Errorf("orders -> customers is missing; got %+v", s.Relationships)
}

// TestTheSameJoinInTwoExploresIsImportedOnce.
//
// The fixture declares orders/order_lines from both sides. Two
// relationships for one join would give the planner a contradiction, and
// which one wins would depend on file order.
func TestTheSameJoinInTwoExploresIsImportedOnce(t *testing.T) {
	s, _ := fixture(t)
	seen := map[string]int{}
	for _, r := range s.Relationships {
		seen[r.From+"->"+r.To]++
	}
	for pair, n := range seen {
		if n > 1 {
			t.Errorf("%s was imported %d times", pair, n)
		}
	}
}

// TestAManyToManyJoinIsRefusedRatherThanGuessed.
//
// Neither side is unique, so there is no key to declare and the planner
// could check nothing. A relationship it cannot check is worse than none,
// because it passes the fan-out check it should fail.
func TestAManyToManyJoinIsRefusedRatherThanGuessed(t *testing.T) {
	s, r := fixture(t)
	for _, rel := range s.Relationships {
		if (rel.From == "customers" && rel.To == "order_lines") ||
			(rel.From == "order_lines" && rel.To == "customers") {
			t.Errorf("a many_to_many join was imported: %+v", rel)
		}
	}
	if skippedMentioning(r, "many_to_many") == "" {
		t.Errorf("it was dropped without being reported: %v", r.Skipped)
	}
}

// TestPrimaryKeyYesBecomesTheGrain, which is what fan-out detection reads.
func TestPrimaryKeyYesBecomesTheGrain(t *testing.T) {
	s, _ := fixture(t)
	for name, want := range map[string]string{
		"orders":      "order_id",
		"order_lines": "line_id",
		"customers":   "customer_id",
	} {
		if got := strings.Join(table(t, s, name).PrimaryKey, ","); got != want {
			t.Errorf("%s grain = %q, want %q", name, got, want)
		}
	}
}

// TestMeasuresBecomeAggregates, in ANSI so the model runs anywhere rather
// than only on the warehouse it came from.
func TestMeasuresBecomeAggregates(t *testing.T) {
	s, _ := fixture(t)
	got := map[string]string{}
	for _, m := range s.Metrics {
		got[m.Name] = m.Expression
	}
	for name, want := range map[string]string{
		"total_revenue":  "SUM(orders.amount)",
		"line_revenue":   "SUM(order_lines.line_amount)",
		"customer_count": "COUNT(DISTINCT customers.customer_id)",
		"orders_count":   "COUNT(*)",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

// TestAFilteredMeasureIsNotImportedUnfiltered.
//
// The bug this caught. LookML writes filters three ways and one of them is
// a list; missing it imported shipped_revenue as SUM(amount), identical to
// total_revenue. A different number under the same name, silently, which
// is the exact failure this whole project exists to prevent.
func TestAFilteredMeasureIsNotImportedUnfiltered(t *testing.T) {
	s, r := fixture(t)
	for _, m := range s.Metrics {
		if m.Name == "shipped_revenue" {
			t.Errorf("a filtered measure was imported without its filter as %q, "+
				"which makes it equal to the unfiltered one", m.Expression)
		}
	}
	if skippedMentioning(r, "shipped_revenue") == "" {
		t.Errorf("it was dropped without being reported: %v", r.Skipped)
	}
}

// TestWhatIsNotImportedIsNamed.
func TestWhatIsNotImportedIsNamed(t *testing.T) {
	_, r := fixture(t)
	for name, because := range map[string]string{
		"revenue_running":   "window function",
		"revenue_per_order": "expression",
		"median_spend":      "portable",
		"is_large":          "expression",
		"sessions":          "derived table",
	} {
		line := skippedMentioning(r, name)
		if line == "" {
			t.Errorf("%s was dropped without being reported", name)
			continue
		}
		if !strings.Contains(line, because) {
			t.Errorf("%s reported without the reason %q: %s", name, because, line)
		}
	}
}

// TestSqlIsReadToTheDoubleSemicolon.
//
// LookML terminates embedded SQL with ;; and a single semicolon is ordinary
// punctuation inside it. Stopping at the first one truncates somebody's
// expression, which is a silently different definition.
func TestSqlIsReadToTheDoubleSemicolon(t *testing.T) {
	dir := t.TempDir()
	body := `view: t {
  sql_table_name: s.t ;;
  dimension: id {
    primary_key: yes
    sql: ${TABLE}.id ;;
  }
  measure: total {
    type: sum
    sql: ${TABLE}.amount ;;
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "t.view.lkml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := lookml.Import(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := table(t, s, "t").PrimaryKey; len(got) != 1 || got[0] != "id" {
		t.Errorf("PrimaryKey = %v", got)
	}
	if len(s.Metrics) != 1 || s.Metrics[0].Expression != "SUM(t.amount)" {
		t.Errorf("metrics = %+v", s.Metrics)
	}
}

// TestTemplatingIsRefused.
//
// A definition that changes per user is the thing a governed semantic layer
// replaces. Importing whichever branch came first would produce a model
// that answers differently from the tool it was imported from.
func TestTemplatingIsRefused(t *testing.T) {
	dir := t.TempDir()
	body := `view: t {
  sql_table_name: {% if x %} a.b {% else %} c.d {% endif %} ;;
  dimension: id { primary_key: yes }
}`
	if err := os.WriteFile(filepath.Join(dir, "t.view.lkml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, r, err := lookml.Import(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tables) != 0 {
		t.Errorf("a templated view was imported: %+v", s.Tables)
	}
	if skippedMentioning(r, "templating") == "" {
		t.Errorf("it was dropped without being reported: %v", r.Skipped)
	}
}

// TestImportingTwiceProducesTheSameFile, because Go randomises map
// iteration and a Looker project is read from many files.
func TestImportingTwiceProducesTheSameFile(t *testing.T) {
	first, _ := fixture(t)
	for i := range 4 {
		again, _ := fixture(t)
		if first.Datasets("looker") != again.Datasets("looker") {
			t.Fatalf("run %d produced different datasets", i+2)
		}
		if first.MetricsFile("looker") != again.MetricsFile("looker") {
			t.Fatalf("run %d produced different metrics", i+2)
		}
	}
}

// TestAnEmptyDirectorySaysWhatToPointAt.
func TestAnEmptyDirectorySaysWhatToPointAt(t *testing.T) {
	_, _, err := lookml.Import(t.TempDir())
	if err == nil {
		t.Fatal("an empty directory imported")
	}
	if !strings.Contains(err.Error(), ".view.lkml") {
		t.Errorf("the error does not say what to point at: %v", err)
	}
}

// TestAMetricWithNoDescriptionGetsAMarkedPlaceholder.
//
// validate requires a description, so without one an imported model fails
// on its first run. The placeholder says it is a placeholder rather than
// inventing a meaning, because a description that reads as considered and
// says nothing is worse than one that admits it.
func TestAMetricWithNoDescriptionGetsAMarkedPlaceholder(t *testing.T) {
	s, _ := fixture(t)
	if s.MetricsNeedingDescription() == 0 {
		t.Fatal("the fixture no longer covers a measure without a description")
	}
	rendered := s.MetricsFile("looker")
	if !strings.Contains(rendered, "TODO") {
		t.Error("an undescribed metric was written without a marked placeholder")
	}
	if strings.Contains(rendered, "description: \"\"") {
		t.Error("an empty description was written, which fails validate")
	}
}
