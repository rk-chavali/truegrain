package cube_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/importer/cube"
	"github.com/rk-chavali/truegrain/internal/introspect"
)

// Importing a Cube data model.
//
// Cube states join cardinality, so that arrives rather than being inferred.
// What it does not state reliably is the grain: primary_key is optional,
// and a cube without one loses fan-out detection entirely. Most of what is
// asserted here is that both of those are handled honestly.

func fixture(t *testing.T) (introspect.Schema, cube.Report) {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "testdata", "fixtures", "cube")
	s, r, err := cube.Import(dir)
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

func mentioning(lines []string, needle string) string {
	for _, s := range lines {
		if strings.Contains(s, needle) {
			return s
		}
	}
	return ""
}

// TestAOneToManyJoinPointsFromTheManySide, the same property the LookML
// importer has to get right and for the same reason: backwards, the
// planner believes the many side is unique and lets a fan-out through.
func TestAOneToManyJoinPointsFromTheManySide(t *testing.T) {
	s, _ := fixture(t)

	var found bool
	for _, r := range s.Relationships {
		if r.From == "order_lines" && r.To == "orders" {
			found = true
		}
		if r.From == "orders" && r.To == "order_lines" {
			t.Error("a one_to_many join was imported pointing the wrong way")
		}
	}
	if !found {
		t.Errorf("order_lines -> orders is missing; got %+v", s.Relationships)
	}
}

func TestPrimaryKeyBecomesTheGrain(t *testing.T) {
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

// TestACubeWithNoPrimaryKeyIsReportedProminently.
//
// Cube treats primary_key as optional, so this is common rather than
// exceptional, and it is the single edit that turns a cautious imported
// model into a usable one. Mentioning it once at the bottom would not be
// enough.
func TestACubeWithNoPrimaryKeyIsReportedProminently(t *testing.T) {
	s, r := fixture(t)

	if key := table(t, s, "web_events").PrimaryKey; len(key) != 0 {
		t.Errorf("a grain was invented for a cube that declares none: %v", key)
	}
	note := mentioning(r.Notes, "web_events")
	if note == "" {
		t.Fatalf("the keyless cube was not named in the notes: %v", r.Notes)
	}
	for _, want := range []string{"primary_key", "fan out", "refused"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not explain the cost (%q): %s", want, note)
		}
	}
}

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

// TestAFilteredMeasureIsNotImportedUnfiltered, because it would be the
// same number as the unfiltered one under a different name.
func TestAFilteredMeasureIsNotImportedUnfiltered(t *testing.T) {
	s, r := fixture(t)
	for _, m := range s.Metrics {
		if m.Name == "shipped_revenue" {
			t.Errorf("a filtered measure was imported unfiltered as %q", m.Expression)
		}
	}
	if mentioning(r.Skipped, "shipped_revenue") == "" {
		t.Errorf("it was dropped without being reported: %v", r.Skipped)
	}
}

func TestWhatIsNotImportedIsNamed(t *testing.T) {
	_, r := fixture(t)
	for name, because := range map[string]string{
		"revenue_7d":        "window function",
		"revenue_per_order": "expression",
		"sessions":          "query rather than",
		"legacy.js":         "program rather than",
	} {
		line := mentioning(r.Skipped, name)
		if line == "" {
			t.Errorf("%s was dropped without being reported", name)
			continue
		}
		if !strings.Contains(line, because) {
			t.Errorf("%s reported without the reason %q: %s", name, because, line)
		}
	}
}

// TestAJavaScriptModelIsNamedNotParsed.
//
// A .js model builds definitions with loops and conditionals. Reading it
// would mean running it, and half-parsing it would produce a model missing
// whatever the code generated.
func TestAJavaScriptModelIsNamedNotParsed(t *testing.T) {
	s, r := fixture(t)
	for _, tbl := range s.Tables {
		if strings.EqualFold(tbl.Name, "Legacy") {
			t.Error("a JavaScript model was parsed")
		}
	}
	if mentioning(r.Skipped, "legacy.js") == "" {
		t.Errorf("it was skipped silently: %v", r.Skipped)
	}
}

// TestImportingTwiceProducesTheSameFile.
func TestImportingTwiceProducesTheSameFile(t *testing.T) {
	first, _ := fixture(t)
	for i := range 4 {
		again, _ := fixture(t)
		if first.Datasets("cube") != again.Datasets("cube") {
			t.Fatalf("run %d produced different datasets", i+2)
		}
		if first.MetricsFile("cube") != again.MetricsFile("cube") {
			t.Fatalf("run %d produced different metrics", i+2)
		}
	}
}

// TestAnEmptyDirectorySaysWhereToPoint.
func TestAnEmptyDirectorySaysWhereToPoint(t *testing.T) {
	_, _, err := cube.Import(t.TempDir())
	if err == nil {
		t.Fatal("an empty directory imported")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("the error does not say where to point: %v", err)
	}
}

// TestAJoinToAnUnknownCubeIsReported rather than emitted against a dataset
// that does not exist, which fails validation later with a confusing
// message.
func TestAJoinToAnUnknownCubeIsReported(t *testing.T) {
	dir := t.TempDir()
	body := `cubes:
  - name: a
    sql_table: s.a
    dimensions:
      - name: id
        sql: id
        primary_key: true
    joins:
      - name: nowhere
        relationship: many_to_one
        sql: "${a.other_id} = ${nowhere.id}"
`
	if err := os.WriteFile(filepath.Join(dir, "m.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, r, err := cube.Import(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Relationships) != 0 {
		t.Errorf("a join to a cube that does not exist was emitted: %+v", s.Relationships)
	}
	if mentioning(r.Skipped, "nowhere") == "" {
		t.Errorf("it was dropped without being reported: %v", r.Skipped)
	}
}
