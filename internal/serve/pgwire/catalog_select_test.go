package pgwire

import (
	"strings"
	"testing"
)

// The synthesised catalogue, and the line between answering a statement and
// declining it.
//
// Every test here is about one property: an answer is either the answer to
// the question that was asked, or there is no answer. The failure this
// replaces was the third option, an answer to a nearby question, which is
// the only one a client cannot detect.

func testSchemas() []namespaceSchema {
	return []namespaceSchema{
		{
			name:       "retail",
			dimensions: []string{"retail.customers.region", "retail.orders.status"},
			metrics:    []string{"retail.order_revenue", "retail.order_count"},
		},
		{
			name:       "finance",
			dimensions: []string{"finance.ledger.account"},
			metrics:    []string{"finance.balance"},
		},
	}
}

func answer(t *testing.T, sql string) *result {
	t.Helper()
	r, ok := catalogAnswer(sql, testSchemas(), "16.0")
	if !ok {
		t.Fatalf("declined: %s", sql)
	}
	return r
}

func declined(t *testing.T, sql string) {
	t.Helper()
	if r, ok := catalogAnswer(sql, testSchemas(), "16.0"); ok {
		t.Errorf("answered a statement it does not understand: %s\ngave %v", sql, r.rows)
	}
}

// TestAWhereClauseIsApplied.
//
// This is the whole reason the evaluator exists. The version before it
// matched on a statement mentioning information_schema.columns and returned
// the columns of every namespace regardless of the filter, so a BI tool
// asking for one table's fields drew a picker holding two tables' fields and
// had no way to know.
func TestAWhereClauseIsApplied(t *testing.T) {
	r := answer(t, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_name = 'finance'`)

	if len(r.rows) != 2 {
		t.Fatalf("want the two finance columns, got %d rows: %v", len(r.rows), r.rows)
	}
	for _, row := range r.rows {
		name, _ := row[0].(string)
		if name != "ledger.account" && name != "balance" {
			t.Errorf("a row from another namespace survived the filter: %v", row)
		}
	}
}

// TestAFilterOnEveryPredicateFormIsApplied, because a predicate this
// understands partially is the same bug as one it ignores.
func TestAFilterOnEveryPredicateFormIsApplied(t *testing.T) {
	for _, c := range []struct {
		name string
		sql  string
		want int
	}{
		{"equality", `SELECT relname FROM pg_class WHERE relname = 'retail'`, 1},
		{"inequality", `SELECT relname FROM pg_class WHERE relname <> 'retail'`, 1},
		{"in", `SELECT relname FROM pg_class WHERE relname IN ('retail', 'finance')`, 2},
		{"in misses", `SELECT relname FROM pg_class WHERE relname IN ('nope')`, 0},
		{"like", `SELECT relname FROM pg_class WHERE relname LIKE 'ret%'`, 1},
		{"is null", `SELECT tablename FROM pg_tables WHERE tablespace IS NULL`, 2},
		{"is not null", `SELECT tablename FROM pg_tables WHERE tablespace IS NOT NULL`, 0},
		{"numeric", `SELECT attname FROM pg_attribute WHERE attrelid = 16385`, 2},
		{"conjunction", `SELECT attname FROM pg_attribute WHERE attrelid = 16385 AND attnum = 1`, 1},
		{"no match", `SELECT relname FROM pg_class WHERE relname = 'absent'`, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := len(answer(t, c.sql).rows); got != c.want {
				t.Errorf("want %d rows, got %d", c.want, got)
			}
		})
	}
}

// TestALikePatternIsNotARegexp.
//
// A pattern holding a regexp metacharacter has to match it literally. A
// client filtering on a name that contains a dot is ordinary, and treating
// the dot as "any character" would return neighbouring tables.
func TestALikePatternIsNotARegexp(t *testing.T) {
	r := answer(t, `SELECT relname FROM pg_class WHERE relname LIKE 'r.tail'`)
	if len(r.rows) != 0 {
		t.Errorf("the dot was treated as a wildcard: %v", r.rows)
	}
}

// TestTheShapeIsTheOneAsked, because a client binds destinations by position
// before it reads anything. Handing back more columns than were selected is
// what produced psql's "column number 8 is out of range".
func TestTheShapeIsTheOneAsked(t *testing.T) {
	r := answer(t, `SELECT table_name, column_name FROM information_schema.columns`)
	if len(r.columns) != 2 || r.columns[0] != "table_name" || r.columns[1] != "column_name" {
		t.Fatalf("columns = %v", r.columns)
	}
	for _, row := range r.rows {
		if len(row) != 2 {
			t.Fatalf("a row has %d values for 2 columns: %v", len(row), row)
		}
	}
}

// TestSelectStarExpandsToTheRelation, and an alias renames the header
// without changing which column is read.
func TestSelectStarExpandsToTheRelation(t *testing.T) {
	star := answer(t, `SELECT * FROM pg_namespace`)
	if len(star.columns) != 3 || star.columns[1] != "nspname" {
		t.Fatalf("columns = %v", star.columns)
	}

	aliased := answer(t, `SELECT nspname AS name FROM pg_namespace`)
	if len(aliased.columns) != 1 || aliased.columns[0] != "name" {
		t.Fatalf("the alias did not reach the header: %v", aliased.columns)
	}
	if got, _ := aliased.rows[0][0].(string); got != "public" {
		t.Errorf("the alias changed which column was read: %v", aliased.rows[0])
	}
}

// TestAMeasureIsNumericAndADimensionIsText.
//
// The single most consequential thing this catalogue says. A BI tool reading
// a measure as text files it under categories, and the person using it never
// finds the metric they came for.
func TestAMeasureIsNumericAndADimensionIsText(t *testing.T) {
	r := answer(t, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_name = 'retail'`)

	types := map[string]string{}
	for _, row := range r.rows {
		name, _ := row[0].(string)
		kind, _ := row[1].(string)
		types[name] = kind
	}
	for name, want := range map[string]string{
		"customers.region": "text",
		"orders.status":    "text",
		"order_revenue":    "numeric",
		"order_count":      "numeric",
	} {
		if types[name] != want {
			t.Errorf("%s is reported as %q, want %q", name, types[name], want)
		}
	}
}

// TestOrderAndLimitAreApplied, because a client that asked for the first ten
// alphabetically and got an arbitrary ten would show the wrong ten.
func TestOrderAndLimitAreApplied(t *testing.T) {
	r := answer(t, `SELECT relname FROM pg_class ORDER BY relname`)
	if len(r.rows) != 2 {
		t.Fatalf("rows = %v", r.rows)
	}
	if first, _ := r.rows[0][0].(string); first != "finance" {
		t.Errorf("ORDER BY was not applied: %v", r.rows)
	}

	desc := answer(t, `SELECT relname FROM pg_class ORDER BY relname DESC LIMIT 1`)
	if len(desc.rows) != 1 {
		t.Fatalf("LIMIT was not applied: %v", desc.rows)
	}
	if only, _ := desc.rows[0][0].(string); only != "retail" {
		t.Errorf("DESC was not applied: %v", desc.rows)
	}
}

// TestARelationWithNothingInItAnswersEmpty.
//
// A namespace is not a view and has no indexes. Zero rows is the true
// answer, and a refusal here would read to a client as a server that is
// broken rather than as a database with no views in it.
func TestARelationWithNothingInItAnswersEmpty(t *testing.T) {
	for _, sql := range []string{
		`SELECT viewname FROM pg_views`,
		`SELECT indexname FROM pg_indexes`,
		`SELECT table_name FROM information_schema.views`,
	} {
		r := answer(t, sql)
		if len(r.rows) != 0 {
			t.Errorf("%s returned rows: %v", sql, r.rows)
		}
		if len(r.columns) != 1 {
			t.Errorf("%s did not shape its answer: %v", sql, r.columns)
		}
	}
}

// TestWhatItCannotUnderstandItDeclines.
//
// Each of these is a statement real clients send. Declining means the caller
// gets a refusal naming what does work, which is recoverable. Answering any
// of them approximately would mean returning rows the caller did not ask
// for, under headers that look right.
func TestWhatItCannotUnderstandItDeclines(t *testing.T) {
	for _, sql := range []string{
		// A join is where a catalogue becomes a database.
		`SELECT c.relname, n.nspname FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid`,
		// OR is not a conjunction, and treating it as one narrows the answer.
		`SELECT relname FROM pg_class WHERE relname = 'retail' OR relname = 'finance'`,
		// A predicate on a column this does not synthesise. Dropping it
		// would return rows the caller excluded.
		`SELECT relname FROM pg_class WHERE relacl IS NULL`,
		// A cast, a function call and an array predicate, all of which psql
		// and JDBC drivers send.
		`SELECT relname::text FROM pg_class`,
		`SELECT pg_get_userbyid(relowner) FROM pg_class`,
		`SELECT typname FROM pg_type WHERE oid = ANY('{25,1700}')`,
		// A relation this does not synthesise at all.
		`SELECT conname FROM pg_constraint`,
		// A subquery.
		`SELECT relname FROM pg_class WHERE oid IN (SELECT oid FROM pg_class)`,
		// OFFSET, which this does not implement, so the page would be wrong.
		`SELECT relname FROM pg_class LIMIT 1 OFFSET 1`,
		// Aggregation.
		`SELECT count(*) FROM pg_class`,
	} {
		declined(t, sql)
	}
}

// TestADeclinedStatementIsRefusedRatherThanAnsweredByThePlanner.
//
// The two surfaces are separate on purpose. A catalogue statement that falls
// through must not be read as a semantic query: pg_class is not a namespace
// and never will be, and a grammar error about it would send somebody to
// look at their model.
func TestADeclinedStatementIsRefusedRatherThanAnsweredByThePlanner(t *testing.T) {
	sql := `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid`
	if _, ok := catalogAnswer(sql, testSchemas(), "16.0"); ok {
		t.Fatal("the catalogue claimed a join")
	}
	if !isCatalogProbe(sql) {
		t.Error("the refusal would complain about grammar instead of naming the catalogue")
	}
}

// TestACallerSeesOnlyTheirOwnCatalogue.
//
// schemaOf already filters by identity, so the rows here are the caller's.
// Asserting it at this level too is the cheap half of a guard that matters:
// a catalogue is itself a disclosure, and a listing built from anything
// other than the caller's own schema would name fields they cannot read.
func TestACallerSeesOnlyTheirOwnCatalogue(t *testing.T) {
	restricted := []namespaceSchema{{
		name:       "retail",
		dimensions: []string{"retail.customers.region"},
	}}

	r, ok := catalogAnswer(`SELECT table_name, column_name FROM information_schema.columns`,
		restricted, "16.0")
	if !ok {
		t.Fatal("declined")
	}
	for _, row := range r.rows {
		if name, _ := row[0].(string); name != "retail" {
			t.Errorf("a namespace outside the caller's schema was listed: %v", row)
		}
		if col, _ := row[1].(string); col != "customers.region" {
			t.Errorf("a field outside the caller's schema was listed: %v", row)
		}
	}
	if len(r.rows) != 1 {
		t.Errorf("want one row, got %d: %v", len(r.rows), r.rows)
	}
}

// TestSessionChatterStillAnswers, because the evaluator sits behind the
// fixed replies and a regression there stops every client at connect.
func TestSessionChatterStillAnswers(t *testing.T) {
	for _, c := range []struct{ sql, tag string }{
		{"SET extra_float_digits = 3", "SET"},
		{"BEGIN", "BEGIN"},
		{"COMMIT", "COMMIT"},
		{"SHOW server_version", "SHOW"},
		{"SELECT version()", "SELECT"},
	} {
		r := answer(t, c.sql)
		if r.tag != c.tag {
			t.Errorf("%s: tag = %q, want %q", c.sql, r.tag, c.tag)
		}
	}
	if v, _ := answer(t, "SELECT version()").rows[0][0].(string); !strings.Contains(v, "truegrain") {
		t.Errorf("the version string does not name this engine: %q", v)
	}
}
