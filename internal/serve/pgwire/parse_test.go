package pgwire

import (
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/plan"
)

// The SQL a BI tool sends, and what it must become.
//
// Two halves worth testing separately. What is accepted has to survive the
// spellings real tools emit, because a tool that cannot connect is a tool
// nobody uses. What is refused has to be refused with a reason, because this
// grammar is deliberately small and a caller hitting its edge needs to know
// whether they wrote something wrong or asked for something the product does
// not do.

var retail = namespaceSchema{
	name:    "retail",
	metrics: []string{"retail.order_revenue", "retail.line_revenue"},
	dimensions: []string{
		"retail.customers.region",
		"retail.orders.status",
		"retail.orders.order_date",
	},
}

func bindOrFail(t *testing.T, sql string) *bound {
	t.Helper()
	q, err := parseSelect(sql)
	if err != nil {
		t.Fatalf("parsing %q: %v", sql, err)
	}
	b, err := bindQuery(q, []namespaceSchema{retail})
	if err != nil {
		t.Fatalf("binding %q: %v", sql, err)
	}
	return b
}

func refusal(t *testing.T, sql string) string {
	t.Helper()
	q, err := parseSelect(sql)
	if err != nil {
		return err.Error()
	}
	if _, err := bindQuery(q, []namespaceSchema{retail}); err != nil {
		return err.Error()
	}
	t.Fatalf("%q was accepted and should not have been", sql)
	return ""
}

func TestTheShapeAToolActuallySends(t *testing.T) {
	// Each of these is a spelling some client emits for the same question.
	// They must all reach the same request, because a semantic layer that
	// answers differently depending on how the SQL was phrased is not one.
	same := []string{
		`SELECT "customers.region", order_revenue FROM retail GROUP BY 1`,
		`SELECT customers.region, SUM(order_revenue) FROM retail GROUP BY customers.region`,
		`select CUSTOMERS.REGION, Order_Revenue from RETAIL group by 1`,
		`SELECT "retail"."customers.region", "retail"."order_revenue" FROM "retail" GROUP BY 1`,
		"SELECT customers.region, order_revenue FROM retail GROUP BY 1 -- tagged by the driver",
		"/* Metabase */ SELECT customers.region, order_revenue FROM public.retail GROUP BY 1",
	}
	for _, sql := range same {
		b := bindOrFail(t, sql)
		if got := strings.Join(b.request.Metrics, ","); got != "retail.order_revenue" {
			t.Errorf("%s\n  metrics = %q", sql, got)
		}
		if got := strings.Join(b.request.Dimensions, ","); got != "retail.customers.region" {
			t.Errorf("%s\n  dimensions = %q", sql, got)
		}
	}
}

func TestColumnOrderIsTheCallersNotTheEngines(t *testing.T) {
	// A tool reads the result positionally. If the engine returns dimensions
	// before metrics and the caller asked for the reverse, every value lands
	// under the wrong header and the chart is silently wrong.
	b := bindOrFail(t, `SELECT order_revenue, customers.region FROM retail`)

	if got := strings.Join(b.columns, ","); got != "order_revenue,customers.region" {
		t.Errorf("columns = %q, want the order the caller wrote", got)
	}
	if got := strings.Join(b.sources, ","); got != "order_revenue,region" {
		t.Errorf("sources = %q", got)
	}
}

func TestAnAliasBecomesTheColumnName(t *testing.T) {
	b := bindOrFail(t,
		`SELECT customers.region AS "Region", SUM(order_revenue) AS revenue FROM retail GROUP BY 1`)

	if got := strings.Join(b.columns, ","); got != "Region,revenue" {
		t.Errorf("columns = %q", got)
	}
	// The alias renames the output; it must not change which field is fetched.
	if got := strings.Join(b.sources, ","); got != "region,order_revenue" {
		t.Errorf("sources = %q", got)
	}
}

func TestFiltersBecomeBoundValues(t *testing.T) {
	b := bindOrFail(t, `
		SELECT order_revenue FROM retail
		WHERE orders.status = 'shipped'
		  AND customers.region IN ('NE', 'MW')
		  AND orders.order_date >= DATE '2026-01-01'
		  AND orders.status IS NOT NULL`)

	if len(b.request.Filters) != 4 {
		t.Fatalf("got %d filters, want 4: %+v", len(b.request.Filters), b.request.Filters)
	}
	want := []struct {
		dim string
		op  plan.Op
		n   int
	}{
		{"retail.orders.status", plan.OpEq, 1},
		{"retail.customers.region", plan.OpIn, 2},
		{"retail.orders.order_date", plan.OpGte, 1},
		{"retail.orders.status", plan.OpIsNotNil, 0},
	}
	for i, w := range want {
		got := b.request.Filters[i]
		if got.Dimension != w.dim || got.Op != w.op || len(got.Values) != w.n {
			t.Errorf("filter %d = %+v, want %s %s with %d values", i, got, w.dim, w.op, w.n)
		}
	}
	if b.request.Filters[0].Values[0] != "shipped" {
		t.Errorf("the value did not survive as a literal: %#v", b.request.Filters[0].Values[0])
	}
}

func TestAQuoteInsideAValueSurvives(t *testing.T) {
	// '' is an escaped quote. Getting this wrong truncates the value at the
	// apostrophe, which silently changes which rows the answer covers.
	b := bindOrFail(t, `SELECT order_revenue FROM retail WHERE customers.region = 'O''Hare'`)

	if got := b.request.Filters[0].Values[0]; got != "O'Hare" {
		t.Errorf("value = %#v, want O'Hare", got)
	}
}

func TestOrderAndLimitCarryThrough(t *testing.T) {
	b := bindOrFail(t, `
		SELECT customers.region, order_revenue FROM retail
		GROUP BY 1 ORDER BY SUM(order_revenue) DESC NULLS LAST LIMIT 10`)

	if len(b.request.OrderBy) != 1 || b.request.OrderBy[0].Field != "order_revenue" ||
		!b.request.OrderBy[0].Desc {
		t.Errorf("order = %+v", b.request.OrderBy)
	}
	if b.request.Limit != 10 {
		t.Errorf("limit = %d, want 10", b.request.Limit)
	}
}

func TestOrderByAnAliasResolvesToTheField(t *testing.T) {
	b := bindOrFail(t,
		`SELECT SUM(order_revenue) AS revenue FROM retail ORDER BY revenue DESC`)

	if len(b.request.OrderBy) != 1 || b.request.OrderBy[0].Field != "order_revenue" {
		t.Errorf("order = %+v, want the underlying field", b.request.OrderBy)
	}
}

// TestWhatIsRefusedAndWhy.
//
// The grammar is small on purpose. Every refusal here has to say what the
// caller can do instead, because otherwise the only way to learn the shape of
// this surface is to guess.
func TestWhatIsRefusedAndWhy(t *testing.T) {
	cases := []struct {
		sql  string
		says string
	}{
		{`SELECT * FROM retail`, "Name what you want"},
		{`SELECT customers.region FROM retail`, "selects no metric"},
		{`SELECT order_revenue FROM retail JOIN customers ON 1=1`, "declared"},
		{`SELECT order_revenue FROM retail, customers`, "join cannot be written"},
		{`SELECT order_revenue FROM retail WHERE orders.status = 'a' OR orders.status = 'b'`, "OR is not supported"},
		{`SELECT order_revenue FROM retail HAVING SUM(order_revenue) > 1`, "HAVING is not supported"},
		{`SELECT DISTINCT customers.region FROM retail`, "already distinct"},
		{`SELECT COUNT(*) FROM retail`, "count is a metric the model defines"},
		{`SELECT order_revenue FROM retail WHERE customers.region LIKE 'N%'`, "LIKE is not supported"},
		{`SELECT order_revenue FROM retail LIMIT 5 OFFSET 20`, "OFFSET is not supported"},
		{`UPDATE retail SET x = 1`, "only SELECT is supported"},
		{`DROP TABLE retail`, "only SELECT is supported"},
		{`SELECT order_revenue FROM nowhere`, "not a namespace"},
		{`SELECT no_such_metric FROM retail`, "not a metric or dimension"},
		{`SELECT order_revenue FROM retail WHERE order_revenue > 5`, "is a metric"},
		{`SELECT customers.region, order_revenue FROM retail GROUP BY orders.status`,
			"has to appear in GROUP BY"},
		{`SELECT order_revenue FROM retail WHERE customers.region = NULL`, "IS NULL"},
	}

	for _, c := range cases {
		got := refusal(t, c.sql)
		if !strings.Contains(got, c.says) {
			t.Errorf("%s\n  got:  %s\n  want it to mention: %s", c.sql, got, c.says)
		}
	}
}

// TestANearMissNamesTheRealThing, because the commonest mistake is writing a
// dimension without its dataset, and the full list is noise when one name is
// obviously meant.
func TestANearMissNamesTheRealThing(t *testing.T) {
	got := refusal(t, `SELECT order_revenue, region FROM retail GROUP BY 1`)
	if !strings.Contains(got, "customers.region") {
		t.Errorf("a near miss did not name the real dimension: %s", got)
	}
}

// TestNothingFromTheStatementBecomesSQL.
//
// The security property, asserted rather than described. Whatever a caller
// writes, what comes out is a list of names that were found in the model and
// values carried separately. If an attacker-controlled string could reach the
// warehouse, it would have to appear in one of these fields.
func TestNothingFromTheStatementBecomesSQL(t *testing.T) {
	hostile := []string{
		`SELECT order_revenue FROM retail WHERE orders.status = 'x''; DROP TABLE orders; --'`,
		`SELECT order_revenue FROM retail WHERE orders.status = 'UNION SELECT * FROM pg_shadow'`,
	}
	for _, sql := range hostile {
		b := bindOrFail(t, sql)

		for _, m := range b.request.Metrics {
			if !known(m) {
				t.Errorf("%s\n  metric %q is not a name from the model", sql, m)
			}
		}
		for _, d := range b.request.Dimensions {
			if !known(d) {
				t.Errorf("%s\n  dimension %q is not a name from the model", sql, d)
			}
		}
		for _, f := range b.request.Filters {
			if !known(f.Dimension) {
				t.Errorf("%s\n  filter names %q, which is not from the model", sql, f.Dimension)
			}
			// The payload survives intact as a value, which is correct: it is
			// data. It is carried in Values and bound as a parameter, never
			// rendered, which is what the executor tests assert separately.
			for _, v := range f.Values {
				if _, ok := v.(string); !ok {
					t.Errorf("%s\n  value %#v is not a plain literal", sql, v)
				}
			}
		}
	}
}

// TestAHostileNameIsRefusedRatherThanCarried.
//
// The other half of containment, and the half the value-payload test above
// does not reach: a payload written where a field name goes. Those queries
// all used legal names, so deleting the resolution check left them passing.
// This is the case that fails when an unresolved string is trusted.
func TestAHostileNameIsRefusedRatherThanCarried(t *testing.T) {
	hostile := []string{
		`SELECT order_revenue, "region FROM pg_shadow --" FROM retail`,
		`SELECT order_revenue FROM retail WHERE "x'; DROP TABLE orders; --" = 1`,
		`SELECT "order_revenue); DROP TABLE orders; --" FROM retail`,
		`SELECT order_revenue FROM retail GROUP BY "nonexistent"`,
	}
	for _, sql := range hostile {
		q, err := parseSelect(sql)
		if err != nil {
			continue // refused at the grammar, which is also containment
		}
		b, err := bindQuery(q, []namespaceSchema{retail})
		if err != nil {
			continue // refused at resolution, which is the intended path
		}
		// It was accepted, so every name it carries has to be one the model
		// declared. Anything else is the caller's string travelling onward.
		for _, n := range append(append([]string{}, b.request.Metrics...), b.request.Dimensions...) {
			if !known(n) {
				t.Errorf("%s carried %q, which the model never declared", sql, n)
			}
		}
		for _, f := range b.request.Filters {
			if !known(f.Dimension) {
				t.Errorf("%s filter carried %q, which the model never declared",
					sql, f.Dimension)
			}
		}
	}
}

func known(name string) bool {
	for _, n := range append(append([]string{}, retail.metrics...), retail.dimensions...) {
		if n == name {
			return true
		}
	}
	return false
}

// TestAnUnterminatedLiteralIsAnError rather than a silently truncated value.
func TestAnUnterminatedLiteralIsAnError(t *testing.T) {
	for _, sql := range []string{
		`SELECT order_revenue FROM retail WHERE orders.status = 'open`,
		`SELECT "unclosed FROM retail`,
		`SELECT order_revenue /* unclosed FROM retail`,
	} {
		if _, err := parseSelect(sql); err == nil {
			t.Errorf("%q parsed without error", sql)
		}
	}
}

// psqlDescribeTable is what psql actually sends for \d, abbreviated but
// keeping the parts that matter: a regex operator and a function call the
// grammar does not implement.
const psqlDescribeTable = `SELECT c.oid, n.nspname, c.relname
FROM pg_catalog.pg_class c
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname OPERATOR(pg_catalog.~) '^(retail)$' COLLATE pg_catalog.default
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 2, 3;`

// psqlListTables is the query behind \dt.
const psqlListTables = `SELECT n.nspname as "Schema", c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' ELSE 'other' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner"
FROM pg_catalog.pg_class c
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','') AND n.nspname !~ '^pg_toast'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2;`

// TestAnUnimplementedOperatorLexes.
//
// The statement is going to be refused either way. Refusing it for what it
// asks, rather than for a tilde in it, is the difference between a message
// somebody can act on and one that sends them looking for a typo they did
// not make.
func TestAnUnimplementedOperatorLexes(t *testing.T) {
	for _, sql := range []string{psqlDescribeTable, psqlListTables} {
		if _, err := lex(sql); err != nil {
			if strings.Contains(err.Error(), "unexpected character") {
				t.Errorf("the lexer choked on an operator rather than the parser "+
					"refusing the statement: %v", err)
			}
		}
	}
}

// TestPsqlListTablesIsAnswered.
//
// The first thing a person types after connecting. An error here reads as
// the whole thing being broken, so it is matched by fingerprint and
// answered with exactly the four columns psql binds.
func TestPsqlListTablesIsAnswered(t *testing.T) {
	r, ok := catalogAnswer(psqlListTables, []namespaceSchema{retail}, "15.0")
	if !ok {
		t.Fatal("the psql table listing was not answered")
	}
	want := []string{"Schema", "Name", "Type", "Owner"}
	if strings.Join(r.columns, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want exactly what psql binds: %v", r.columns, want)
	}
	if len(r.rows) != 1 || r.rows[0][1] != "retail" {
		t.Errorf("rows = %v, want one naming the namespace", r.rows)
	}
}

// TestAnUnshapeableCatalogQueryIsNotClaimed.
//
// The bug this replaced: a query that merely mentioned pg_class got the
// generic eight-column table listing, and psql reported "column number 8 is
// out of range" because it had bound its own different set. A shape nobody
// asked for is a protocol error, so the answer is not claimed at all and
// the caller gets a refusal naming what does work.
func TestAnUnshapeableCatalogQueryIsNotClaimed(t *testing.T) {
	if _, ok := catalogAnswer(psqlDescribeTable, []namespaceSchema{retail}, "15.0"); ok {
		t.Error("a catalogue query that cannot be shaped was answered anyway")
	}
	if !isCatalogProbe(psqlDescribeTable) {
		t.Error("it is not recognised as a catalogue probe, so the refusal will " +
			"complain about grammar instead of naming what does work")
	}
}

// TestAnExactAnswerIsNotNarrowed.
//
// SELECT version() is a function call and cannot be read as a column list,
// but its single column is exactly right. Narrowing it would refuse the
// first statement psql sends and nothing would ever connect.
func TestAnExactAnswerIsNotNarrowed(t *testing.T) {
	for _, sql := range []string{"SELECT version()", "SHOW server_version", "select current_schema()"} {
		r, ok := catalogAnswer(sql, []namespaceSchema{retail}, "15.0")
		if !ok {
			t.Errorf("%q was not answered", sql)
			continue
		}
		if len(r.columns) != 1 {
			t.Errorf("%q returned %d columns, want 1", sql, len(r.columns))
		}
	}
}

// TestASimpleCatalogQueryIsStillNarrowed, because that is the path every
// JDBC driver takes and it binds exactly the columns it named.
func TestASimpleCatalogQueryIsStillNarrowed(t *testing.T) {
	r, ok := catalogAnswer(
		"SELECT table_name, column_name FROM information_schema.columns",
		[]namespaceSchema{retail}, "15.0")
	if !ok {
		t.Fatal("a simple column listing was not answered")
	}
	if len(r.columns) != 2 {
		t.Errorf("columns = %v, want exactly the two asked for", r.columns)
	}
	for _, row := range r.rows {
		if len(row) != 2 {
			t.Fatalf("a row has %d values for 2 columns", len(row))
		}
	}
}
