package osi

import (
	"strings"
	"testing"
)

// The expression parser is the piece the Ossie spec forces on this engine: a
// metric is a SQL string, so the datasets it touches and the aggregate it uses
// only exist inside that text. These tests cover the constructs
// core-spec/expression_language.md marks REQUIRED, plus the precedence traps.

func TestParsesRequiredConstructs(t *testing.T) {
	// Every one of these appears in the spec's REQUIRED tables.
	exprs := []string{
		"order_total",
		"orders.order_total",
		`"Quoted Column"`,
		"SUM(orders.amount)",
		"COUNT(*)",
		"COUNT(DISTINCT customers.id)",
		"AVG(orders.amount)",
		"MIN(a) + MAX(b)",
		"STDDEV_POP(x)",
		"APPROX_COUNT_DISTINCT(customers.id)",
		"PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY orders.amount)",
		"SUM(a) / COUNT(DISTINCT b)",
		"SUM(CASE WHEN status = 'shipped' THEN amount ELSE 0 END)",
		"CASE status WHEN 'a' THEN 1 WHEN 'b' THEN 2 ELSE 0 END",
		"CAST(x AS DECIMAL(18, 2))",
		"EXTRACT(YEAR FROM order_date)",
		"YEAR(order_date)",
		"DATE_TRUNC('month', order_date)",
		"POSITION('@' IN email)",
		"CHARINDEX('@', email)",
		"LOWER(SUBSTR(email, POSITION('@' IN email) + 1))",
		"first_name || ' ' || last_name",
		"a IS NULL",
		"a IS NOT NULL",
		"status IN ('a', 'b', 'c')",
		"status NOT IN ('a')",
		"amount BETWEEN 1 AND 10",
		"amount NOT BETWEEN 1 AND 10",
		"name LIKE 'A%'",
		"name NOT LIKE 'A%'",
		"NOT (a AND b)",
		"a > 1 AND b < 2 OR c = 3",
		"-amount",
		"amount * 1.5 + 2",
		"x % 3",
		"TRUE",
		"COALESCE(a, b, 0)",
		"order_date + INTERVAL '1' DAY",
		"it''s",
	}
	for _, src := range exprs {
		t.Run(src, func(t *testing.T) {
			if _, err := ParseExpr(src); err != nil {
				// The last case is a bare string literal body, not valid alone.
				if src == "it''s" {
					t.Skip("not an expression on its own")
				}
				t.Fatalf("failed to parse a construct the spec marks REQUIRED:\n%v", err)
			}
		})
	}
}

func TestRejectsMalformedExpressions(t *testing.T) {
	for _, src := range []string{
		"",
		"SUM(",
		"SUM(a))",
		"CASE WHEN a THEN 1",   // no END
		"CASE END",             // no WHEN
		"CAST(x AS)",           // no type
		"EXTRACT(YEAR x)",      // no FROM
		"a IN",                 // no list
		"a BETWEEN 1",          // no upper bound
		"'unterminated",        // open string
		"SELECT * FROM orders", // a statement, not an expression
		"a @ b",                // unknown operator
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := ParseExpr(src); err == nil {
				t.Fatalf("parsed %q, which is not a valid expression", src)
			}
		})
	}
}

// TestBetweenDoesNotSwallowBooleanAnd is a precedence trap. The AND inside
// BETWEEN binds tighter than a boolean AND; getting it wrong silently changes
// which rows match.
func TestBetweenDoesNotSwallowBooleanAnd(t *testing.T) {
	e, err := ParseExpr("amount BETWEEN 1 AND 10 AND status = 'x'")
	if err != nil {
		t.Fatal(err)
	}
	top, ok := e.(*Binary)
	if !ok || top.Op != "AND" {
		t.Fatalf("the outermost node should be the boolean AND, got %T", e)
	}
	if _, ok := top.L.(*Between); !ok {
		t.Errorf("the left side should be the complete BETWEEN, got %T", top.L)
	}
}

// TestPositionKeywordIsNotMembership covers the other keyword collision: the IN
// inside POSITION is part of the call, not the set-membership operator.
func TestPositionKeywordIsNotMembership(t *testing.T) {
	e, err := ParseExpr("POSITION('@' IN email)")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := e.(*Call)
	if !ok || c.Name != "POSITION" {
		t.Fatalf("want a POSITION call, got %T", e)
	}
	if len(c.Args) != 2 {
		t.Fatalf("want 2 arguments, got %d", len(c.Args))
	}
	if _, isIn := e.(*In); isIn {
		t.Error("POSITION was parsed as set membership")
	}
}

func TestOperatorPrecedence(t *testing.T) {
	// 1 + 2 * 3 must group as 1 + (2 * 3).
	e, err := ParseExpr("1 + 2 * 3")
	if err != nil {
		t.Fatal(err)
	}
	top := e.(*Binary)
	if top.Op != "+" {
		t.Fatalf("addition should be outermost, got %q", top.Op)
	}
	if right, ok := top.R.(*Binary); !ok || right.Op != "*" {
		t.Errorf("multiplication should bind tighter, got %T", top.R)
	}
}

// TestIdentifierNormalization pins the spec's case rules: a regular identifier
// is case insensitive, a quoted one is exact. They must not collide.
func TestIdentifierNormalization(t *testing.T) {
	cases := []struct {
		src      string
		wantNorm string
	}{
		{"id", "ID"},
		{"Id", "ID"},
		{`"ID"`, "ID"},
		{`"id"`, "id"},
		{"orders.Amount", "ORDERS.AMOUNT"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e, err := ParseExpr(c.src)
			if err != nil {
				t.Fatal(err)
			}
			id := e.(*Ident)
			if got := id.NormKey(); got != c.wantNorm {
				t.Errorf("want normalized %q, got %q", c.wantNorm, got)
			}
		})
	}
}

func TestRefsFindsEveryDatasetReference(t *testing.T) {
	e, err := ParseExpr("SUM(orders.total) / COUNT(DISTINCT customers.id) + MAX(orders.fee)")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, id := range Refs(e) {
		got = append(got, id.String())
	}
	want := "orders.total customers.id orders.fee"
	if strings.Join(got, " ") != want {
		t.Errorf("want refs %q, got %q; the planner uses these to decide which datasets to join",
			want, strings.Join(got, " "))
	}
}

// TestDecomposabilityMatchesSpec pins the classification table from
// core-spec/expression_language.md. This is the engine's additivity model, so a
// wrong row here becomes a wrong number.
func TestDecomposabilityMatchesSpec(t *testing.T) {
	cases := []struct {
		expr string
		want Decomposability
	}{
		{"SUM(a)", Distributive},
		{"COUNT(a)", Distributive},
		{"COUNT(*)", Distributive},
		{"MIN(a)", Distributive},
		{"MAX(a)", Distributive},
		// The spec calls this out explicitly: COUNT is Distributive but
		// COUNT(DISTINCT expr) is Holistic.
		{"COUNT(DISTINCT a)", Holistic},
		{"AVG(a)", Algebraic},
		{"STDDEV(a)", Algebraic},
		{"VARIANCE(a)", Algebraic},
		{"MEDIAN(a)", Holistic},
		{"PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY a)", Holistic},
		{"APPROX_COUNT_DISTINCT(a)", SketchBased},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			e, err := ParseExpr(c.expr)
			if err != nil {
				t.Fatal(err)
			}
			aggs := Aggregates(e)
			if len(aggs) != 1 {
				t.Fatalf("want 1 aggregate, found %d", len(aggs))
			}
			if got := Classify(aggs[0]); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}

// TestSurvivesFanOut pins the rule the planner actually branches on. This is a
// different question from decomposability: it asks whether the aggregate still
// returns the right answer when its input rows are duplicated by a join.
func TestSurvivesFanOut(t *testing.T) {
	cases := map[string]bool{
		"SUM(a)":                    false, // inflates, the classic failure
		"COUNT(a)":                  false,
		"COUNT(*)":                  false,
		"AVG(a)":                    false, // duplication reweights the mean
		"STDDEV(a)":                 false,
		"MEDIAN(a)":                 false, // duplication reshapes the distribution
		"MIN(a)":                    true,  // idempotent under repetition
		"MAX(a)":                    true,
		"COUNT(DISTINCT a)":         true, // discards duplicates by definition
		"SUM(DISTINCT a)":           true,
		"APPROX_COUNT_DISTINCT(a)":  true,
		"APPROX_PERCENTILE(a, 0.5)": false,
	}
	for expr, want := range cases {
		t.Run(strings.TrimSpace(expr), func(t *testing.T) {
			e, err := ParseExpr(expr)
			if err != nil {
				t.Fatal(err)
			}
			aggs := Aggregates(e)
			if len(aggs) != 1 {
				t.Fatalf("want 1 aggregate, found %d", len(aggs))
			}
			if got := SurvivesFanOut(aggs[0]); got != want {
				t.Errorf("SurvivesFanOut(%s) = %v, want %v", expr, got, want)
			}
		})
	}
}

func TestWindowFunctionsAreDetectedNotDropped(t *testing.T) {
	e, err := ParseExpr("SUM(amount) OVER (PARTITION BY region ORDER BY d)")
	if err != nil {
		t.Fatalf("a window function should parse so the refusal can name it:\n%v", err)
	}
	w := WindowCalls(e)
	if len(w) != 1 {
		t.Fatalf("want 1 window call, got %d", len(w))
	}
	if len(Aggregates(e)) != 0 {
		t.Error("a windowed SUM is not a plain aggregate and must not be classified as one")
	}
}

func TestNestedAggregateDetected(t *testing.T) {
	e, err := ParseExpr("SUM(COUNT(a))")
	if err != nil {
		t.Fatal(err)
	}
	outer := Aggregates(e)[0]
	if !HasAggregateInside(outer) {
		t.Error("a nested aggregate is invalid SQL and must be reported at validate time")
	}
}

func TestStringLiteralEscaping(t *testing.T) {
	e, err := ParseExpr("'it''s'")
	if err != nil {
		t.Fatal(err)
	}
	lit := e.(*Lit)
	if lit.Kind != LitString || lit.Text != "it's" {
		t.Errorf("want the doubled quote unescaped to \"it's\", got %q", lit.Text)
	}
}
