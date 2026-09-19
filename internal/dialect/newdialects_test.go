package dialect_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/dialect"
	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Postgres and Snowflake each differ from the others in one way that fails
// quietly rather than loudly. These pin those two places.

// TestPostgresNumbersItsPlaceholders.
//
// Postgres has no positional `?`. Emitting one produces a syntax error at the
// warehouse rather than in any test here, so the numbering is worth asserting
// directly rather than only through a golden file that somebody could
// regenerate without reading.
func TestPostgresNumbersItsPlaceholders(t *testing.T) {
	pg, err := dialect.Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]string{1: "$1", 2: "$2", 17: "$17"} {
		if got := pg.Placeholder(n); got != want {
			t.Errorf("Placeholder(%d) = %q, want %q", n, got, want)
		}
	}
	if strings.Contains(pg.Placeholder(1), "?") {
		t.Error("postgres does not accept a positional ?")
	}
}

// TestSnowflakeUsesEqualNull is the one that would fail silently.
//
// Snowflake does not accept the ANSI `IS NOT DISTINCT FROM` spelling, so a
// multi-fact join written that way is a syntax error. Worse, falling back to
// plain `=` would parse and run, and would drop every row where the joined
// dimension is null: a quietly understated result rather than an error.
func TestSnowflakeUsesEqualNull(t *testing.T) {
	sf, err := dialect.Get("snowflake")
	if err != nil {
		t.Fatal(err)
	}
	got := sf.NullSafeEquals(`"a"."d"`, `"b"."d"`)

	if !strings.Contains(got, "EQUAL_NULL") {
		t.Errorf("snowflake needs EQUAL_NULL, got %q", got)
	}
	if strings.Contains(got, "IS NOT DISTINCT FROM") {
		t.Errorf("snowflake does not accept the ANSI spelling, got %q", got)
	}
	// Plain equality would compile and silently drop null groups, which is the
	// failure mode this whole engine exists to prevent.
	if got == `"a"."d" = "b"."d"` {
		t.Error("plain equality drops rows where the dimension is null")
	}
}

// TestSnowflakeUppercasesDateParts. Snowflake accepts either case, but the
// engine emits one consistently so a golden diff shows a real change rather
// than a formatting churn.
func TestSnowflakeUppercasesDateParts(t *testing.T) {
	sf, _ := dialect.Get("snowflake")
	got, err := sf.DateTrunc(plan.GrainMonth, osi.TypeDate, `"o"."d"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "'MONTH'") {
		t.Errorf("want an uppercase date part, got %q", got)
	}
}

// TestSnowflakeRefusesAnUnknownGrain rather than passing it through. A date
// part Snowflake does not recognise fails at the warehouse with a message that
// does not mention the grain the caller asked for.
func TestSnowflakeRefusesAnUnknownGrain(t *testing.T) {
	sf, _ := dialect.Get("snowflake")
	if _, err := sf.DateTrunc(plan.Grain("fortnight"), osi.TypeDate, `"o"."d"`); err == nil {
		t.Fatal("an unknown grain must be refused at compile time")
	}
}

// TestEveryGrainTheEngineOffersCompilesEverywhere.
//
// The engine advertises its grains through health and the OpenAPI document. A
// dialect that cannot render one of them turns an advertised capability into a
// runtime failure, so every dialect is checked against the full list rather
// than against whichever grains the fixtures happen to use.
func TestEveryGrainTheEngineOffersCompilesEverywhere(t *testing.T) {
	for _, name := range dialect.Names() {
		d, err := dialect.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range plan.Grains() {
			// A Date column is the narrowest temporal type, so if a grain
			// renders for it the wider types follow.
			if _, err := d.DateTrunc(plan.Grain(g), osi.TypeDateTime, `"o"."d"`); err != nil {
				t.Errorf("%s cannot render grain %q, which the engine advertises: %v", name, g, err)
			}
		}
	}
}

// TestNewDialectsDoNotOverstateTheirSecurity.
//
// Postgres has row-level security and Snowflake has masking policies, and both
// are real. Neither is configured or verified by this engine, so reporting
// either as enforced would claim something the engine cannot observe, which is
// the one direction health must never be wrong in.
func TestNewDialectsDoNotOverstateTheirSecurity(t *testing.T) {
	for _, name := range []string{"postgres", "snowflake"} {
		d, _ := dialect.Get(name)
		caps := d.Capabilities()

		if caps.ColumnLevelSecurity || caps.RowLevelSecurity {
			t.Errorf("%s claims to enforce security this engine neither configures nor reads", name)
		}
		if caps.Note == "" {
			t.Errorf("%s must explain what it does and does not enforce", name)
		}
		// The note has to say the warehouse is capable of it, otherwise a
		// reader concludes the warehouse cannot do it at all.
		if !strings.Contains(strings.ToLower(caps.Note), "policies") &&
			!strings.Contains(strings.ToLower(caps.Note), "privileges") {
			t.Errorf("%s should say the warehouse supports these controls even "+
				"though the engine does not read them: %q", name, caps.Note)
		}
	}
}

// TestEveryDialectQuotesIdentifiers is the injection guarantee at the emitter
// boundary. A dialect that does not quote lets a column named `from` or one
// carrying a quote character change the shape of the statement.
func TestEveryDialectQuotesIdentifiers(t *testing.T) {
	for _, name := range dialect.Names() {
		d, _ := dialect.Get(name)
		got := d.QuoteIdent(`we"ird`)

		if strings.Count(got, `"`) < 2 && !strings.Contains(got, "`") {
			t.Errorf("%s does not appear to quote identifiers: %q", name, got)
		}
		// The embedded quote must be escaped rather than passed through, or it
		// terminates the identifier early.
		if strings.HasSuffix(got, `we"ird"`) {
			t.Errorf("%s did not escape the embedded quote: %q", name, got)
		}
	}
}

// TestRedshiftNeverEmitsIsNotDistinctFrom.
//
// Redshift forked from PostgreSQL 8.0.2 and has no IS NOT DISTINCT FROM, so
// the spelling the Postgres emitter uses is a syntax error there. It appears
// in exactly one place, joining two separately aggregated facts on a shared
// dimension, which makes it both easy to miss and the worst place to be
// wrong: a null dimension value is a real group, and plain equality would
// drop it and answer a smaller number without failing.
func TestRedshiftNeverEmitsIsNotDistinctFrom(t *testing.T) {
	red, err := dialect.Get("redshift")
	if err != nil {
		t.Fatal(err)
	}

	got := red.NullSafeEquals(`"a"."x"`, `"b"."x"`)
	if strings.Contains(got, "IS NOT DISTINCT FROM") {
		t.Errorf("Redshift cannot parse this: %s", got)
	}
	// It still has to be null-safe, or the fix is worse than the bug.
	for _, want := range []string{"IS NULL AND", " OR "} {
		if !strings.Contains(got, want) {
			t.Errorf("the expansion is not null-safe, missing %q: %s", want, got)
		}
	}

	// And the whole emitted corpus, because the construct could arrive from
	// somewhere other than NullSafeEquals.
	matches, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "*.redshift.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no Redshift golden files; run `go test ./internal/dialect/ -update`")
	}
	for _, path := range matches {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "IS NOT DISTINCT FROM") {
			t.Errorf("%s emits a construct Redshift cannot parse", filepath.Base(path))
		}
	}
}
