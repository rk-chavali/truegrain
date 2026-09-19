package introspect_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/introspect"
)

// The PostgreSQL metadata reader, against a real server.
//
// A reader cannot be usefully tested against a mock: every bug worth catching
// here is a bug about what the catalogue actually contains. The BigQuery
// reader had one for weeks with a full suite of passing unit tests, because
// the tests agreed with the code about a shape the warehouse did not have.
//
// These need TRUEGRAIN_TEST_POSTGRES_DSN. CI sets it from a service container,
// so they do not skip there, which matters because the build fails on any
// skipped test: a reader suite that quietly stops running is worse than one
// that was never written.

const schema = "truegrain_introspect"

func readOrSkip(t *testing.T) introspect.Schema {
	t.Helper()
	dsn := os.Getenv("TRUEGRAIN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TRUEGRAIN_TEST_POSTGRES_DSN is not set; see .github/workflows/ci.yml for the shape")
	}
	seed(t, dsn)

	got, err := introspect.Postgres(context.Background(), dsn, schema)
	if err != nil {
		t.Fatalf("reading %s: %v", schema, err)
	}
	return got
}

// seed loads the fixture through psql, the same way the executor suite does.
func seed(t *testing.T, dsn string) {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "fixtures", "introspect_postgres.sql")
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skipf("psql is not on PATH, so the fixture cannot be loaded: %v", err)
	}
	cmd := exec.Command(psql, dsn, "-v", "ON_ERROR_STOP=1", "-q", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("loading %s: %v\n%s", path, err, out)
	}
}

func find(t *testing.T, s introspect.Schema, name string) introspect.Table {
	t.Helper()
	for _, table := range s.Tables {
		if table.Name == name {
			return table
		}
	}
	t.Fatalf("table %q is missing; got %v", name, names(s))
	return introspect.Table{}
}

func names(s introspect.Schema) []string {
	var out []string
	for _, t := range s.Tables {
		out = append(out, t.Name)
	}
	return out
}

func relationship(t *testing.T, s introspect.Schema, from, to string) introspect.Relationship {
	t.Helper()
	for _, r := range s.Relationships {
		if r.From == from && r.To == to {
			return r
		}
	}
	t.Fatalf("no relationship %s -> %s; got %+v", from, to, s.Relationships)
	return introspect.Relationship{}
}

// TestACompoundForeignKeyIsPairedByTheConstraintNotByPosition.
//
// The reason this reader exists in the shape it does. shipments declares
// ship_line before ship_order, but the constraint references
// (ship_order, ship_line) -> (order_id, line_number). A reader that pairs the
// sides by column position, or that joins the catalogue on constraint name and
// orders by attnum, emits ship_line -> order_id. That join runs and returns
// wrong rows, which is exactly the bug the BigQuery reader shipped.
func TestACompoundForeignKeyIsPairedByTheConstraintNotByPosition(t *testing.T) {
	got := readOrSkip(t)
	rel := relationship(t, got, "shipments", "order_lines")

	wantFrom := []string{"ship_order", "ship_line"}
	wantTo := []string{"order_id", "line_number"}

	if strings.Join(rel.FromColumns, ",") != strings.Join(wantFrom, ",") {
		t.Errorf("FromColumns = %v, want %v", rel.FromColumns, wantFrom)
	}
	if strings.Join(rel.ToColumns, ",") != strings.Join(wantTo, ",") {
		t.Errorf("ToColumns = %v, want %v", rel.ToColumns, wantTo)
	}

	// The pairing itself, stated as the property rather than as two lists: the
	// nth referencing column matches the nth referenced one.
	if len(rel.FromColumns) != len(rel.ToColumns) {
		t.Fatalf("a compound key came back unpaired: %d from, %d to",
			len(rel.FromColumns), len(rel.ToColumns))
	}
	if len(rel.FromColumns) != 2 {
		t.Fatalf("a two-column key came back as %d columns, which is the fan-out "+
			"inside the fan-out detector: %v", len(rel.FromColumns), rel.FromColumns)
	}
}

// TestACompoundPrimaryKeyKeepsItsDeclaredOrder.
//
// Grain is derived from this. Out of order it still detects a fan-out, but it
// produces different bytes on every run, so regenerating after a schema change
// stops being a readable diff.
func TestACompoundPrimaryKeyKeepsItsDeclaredOrder(t *testing.T) {
	lines := find(t, readOrSkip(t), "order_lines")
	if got := strings.Join(lines.PrimaryKey, ","); got != "order_id,line_number" {
		t.Errorf("PrimaryKey = %q, want \"order_id,line_number\"", got)
	}
}

// TestATableWithNoKeyIsReportedRatherThanGuessed.
//
// A guessed key is worse than a missing one: the engine would believe it knows
// the grain, pass the fan-out check, and inflate a sum confidently.
func TestATableWithNoKeyIsReportedRatherThanGuessed(t *testing.T) {
	got := readOrSkip(t)

	if key := find(t, got, "events").PrimaryKey; len(key) != 0 {
		t.Errorf("events came back with a key it does not declare: %v", key)
	}

	var reported bool
	for _, name := range got.TablesWithoutKeys() {
		if name == "events" {
			reported = true
		}
	}
	if !reported {
		t.Errorf("events is keyless but TablesWithoutKeys did not name it: %v",
			got.TablesWithoutKeys())
	}

	var noted bool
	for _, note := range got.Notes {
		if strings.Contains(note, "events") && strings.Contains(note, "fan-out") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("nothing in the notes warns that events has no derivable grain: %v", got.Notes)
	}
}

// TestAForeignKeyOutOfTheSchemaIsDropped.
//
// The generated model has no dataset for the other schema, so a relationship
// naming it would fail validation later with a confusing message.
func TestAForeignKeyOutOfTheSchemaIsDropped(t *testing.T) {
	for _, r := range readOrSkip(t).Relationships {
		if r.To == "warehouses" {
			t.Errorf("a foreign key into another schema was emitted: %+v", r)
		}
	}
}

// TestStructuredColumnsAreNamedAndLeftOut.
//
// An array or a composite emitted as a String produces a model that looks
// complete, groups by a value no filter can match, and compares badly in
// doctor. Naming it is the honest shape.
func TestStructuredColumnsAreNamedAndLeftOut(t *testing.T) {
	got := readOrSkip(t)
	customers := find(t, got, "customers")

	for _, c := range customers.Columns {
		if c.Name == "tags" || c.Name == "address" {
			t.Errorf("%s was emitted as a dimension with type %q", c.Name, c.Type)
		}
	}

	joined := strings.Join(got.Notes, " ")
	for _, want := range []string{"customers.tags", "customers.address"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s was dropped without being named in the notes: %v", want, got.Notes)
		}
	}
}

// TestViewsAreReadAndArriveWithoutAKey.
//
// Excluding views would silently drop half of most warehouses.
func TestViewsAreReadAndArriveWithoutAKey(t *testing.T) {
	view := find(t, readOrSkip(t), "recent_orders")
	if len(view.Columns) != 3 {
		t.Errorf("recent_orders has %d columns, want 3", len(view.Columns))
	}
	if len(view.PrimaryKey) != 0 {
		t.Errorf("a view came back with a primary key: %v", view.PrimaryKey)
	}
}

// TestTypesNullabilityAndCommentsSurvive.
//
// The generated file is meant to be read and edited, and a warehouse comment
// is often the only documentation that exists.
func TestTypesNullabilityAndCommentsSurvive(t *testing.T) {
	got := readOrSkip(t)
	customers := find(t, got, "customers")

	if !strings.Contains(customers.Description, "canonical") {
		t.Errorf("the table comment was lost: %q", customers.Description)
	}

	byName := map[string]introspect.Column{}
	for _, c := range customers.Columns {
		byName[c.Name] = c
	}

	if got := byName["customer_id"].Type; got != "integer" {
		t.Errorf("customer_id type = %q, want integer", got)
	}
	if byName["customer_id"].Nullable {
		t.Error("customer_id is the primary key and came back nullable")
	}
	if byName["email"].Nullable {
		t.Error("email is NOT NULL and came back nullable")
	}
	if !byName["region"].Nullable {
		t.Error("region has no NOT NULL and came back non-nullable")
	}
	if !strings.Contains(byName["region"].Description, "CRM") {
		t.Errorf("the column comment was lost: %q", byName["region"].Description)
	}
	// NUMERIC(12,2) rather than "numeric": the warehouse's own spelling is
	// kept so a generated model can be checked back against the warehouse.
	if got := find(t, got, "order_lines"); !strings.Contains(columnType(got, "line_total"), "12,2") {
		t.Errorf("the precision was dropped from line_total: %q", columnType(got, "line_total"))
	}
}

// TestASingleColumnForeignKeyStillWorks, because the compound path is the
// interesting one and is not the only one.
func TestASingleColumnForeignKeyStillWorks(t *testing.T) {
	rel := relationship(t, readOrSkip(t), "orders", "customers")
	if len(rel.FromColumns) != 1 || rel.FromColumns[0] != "customer_id" {
		t.Errorf("FromColumns = %v, want [customer_id]", rel.FromColumns)
	}
	if len(rel.ToColumns) != 1 || rel.ToColumns[0] != "customer_id" {
		t.Errorf("ToColumns = %v, want [customer_id]", rel.ToColumns)
	}
	if rel.Name != "orders_customer" {
		t.Errorf("Name = %q, want the declared constraint name", rel.Name)
	}
}

// TestReadingTwiceProducesTheSameFile.
//
// Regenerating after a schema change should be a readable diff. Map iteration
// order in Go is deliberately random, so this fails immediately if any part of
// the reader depends on it.
func TestReadingTwiceProducesTheSameFile(t *testing.T) {
	dsn := os.Getenv("TRUEGRAIN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TRUEGRAIN_TEST_POSTGRES_DSN is not set; see .github/workflows/ci.yml for the shape")
	}
	seed(t, dsn)

	first, err := introspect.Postgres(context.Background(), dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		again, err := introspect.Postgres(context.Background(), dsn, schema)
		if err != nil {
			t.Fatal(err)
		}
		if first.Datasets("retail") != again.Datasets("retail") {
			t.Fatalf("run %d produced a different file", i+2)
		}
	}
}

// TestAMissingSchemaSaysSoRatherThanReturningNothing.
func TestAMissingSchemaSaysSoRatherThanReturningNothing(t *testing.T) {
	dsn := os.Getenv("TRUEGRAIN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TRUEGRAIN_TEST_POSTGRES_DSN is not set; see .github/workflows/ci.yml for the shape")
	}
	_, err := introspect.Postgres(context.Background(), dsn, "no_such_schema_here")
	if err == nil {
		t.Fatal("reading a schema that does not exist returned no error")
	}
	if !strings.Contains(err.Error(), "no_such_schema_here") {
		t.Errorf("the error does not name the schema: %v", err)
	}
}

// TestASchemaNameIsValidatedAtTheEdge.
//
// Needs no database, which is the point: the rule is asserted directly rather
// than only through a server that might be absent.
func TestASchemaNameIsValidatedAtTheEdge(t *testing.T) {
	for _, bad := range []string{"", "   ", "with\x00null", "new\nline", strings.Repeat("a", 64)} {
		if err := introspect.ValidateSchemaName(bad); err == nil {
			t.Errorf("%q was accepted as a schema name", bad)
		}
	}
	// What people actually call schemas has to keep working.
	for _, ok := range []string{"public", "truegrain_introspect", "Sales", "raw-data", "über"} {
		if err := introspect.ValidateSchemaName(ok); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}
}

func columnType(t introspect.Table, name string) string {
	for _, c := range t.Columns {
		if c.Name == name {
			return c.Type
		}
	}
	return ""
}
