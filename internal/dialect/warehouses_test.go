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

// The four dialects in warehouses.go are written as small differences from
// an existing one, which is what keeps them short and is also what can go
// silently wrong: an override that is never reached leaves the parent's
// behaviour in place, and the parent's behaviour is valid SQL for a
// different warehouse. Nothing here has a verified executor, so these
// guards are the only thing standing between a wrong emitter and a
// customer's first query.
//
// Each test below names a construct that is a syntax error or, worse, a
// silently wrong answer on the warehouse in question.

// body returns a golden file with its trailing parameter listing removed.
//
// That listing is a comment block holding the bound values, and those values
// are quoted strings. Scanning a whole golden for a quoting mistake would
// match them and report every parameterised query as broken.
func body(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sql, _, _ := strings.Cut(string(src), "-- parameters:")
	return sql
}

// goldens returns every golden emitted for a dialect, failing if there are
// none: a dialect registered without goldens generated passes every content
// check in this file vacuously.
func goldens(t *testing.T, name string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "*."+name+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no %s golden files; run `go test ./internal/dialect/ ./internal/engine/ -update`", name)
	}
	return matches
}

// TestClickHouseNeverEmitsIsNotDistinctFrom.
//
// The same shape as the Redshift guard and for a worse reason. ClickHouse
// has no IS NOT DISTINCT FROM, so inheriting the Postgres spelling would be
// a syntax error in exactly one place: the full outer join that combines two
// separately aggregated facts on a shared dimension. If somebody "fixed"
// that by reaching for plain equality instead of `<=>`, it would stop being
// an error and start being a wrong number, because a null dimension value is
// a real group and equality drops it.
func TestClickHouseNeverEmitsIsNotDistinctFrom(t *testing.T) {
	ch, err := dialect.Get("clickhouse")
	if err != nil {
		t.Fatal(err)
	}

	got := ch.NullSafeEquals("`a`.`x`", "`b`.`x`")
	if strings.Contains(got, "IS NOT DISTINCT FROM") {
		t.Errorf("ClickHouse cannot parse this: %s", got)
	}
	if !strings.Contains(got, "<=>") {
		t.Errorf("the comparison is not null-safe, so a null group would be dropped: %s", got)
	}

	for _, path := range goldens(t, "clickhouse") {
		if strings.Contains(body(t, path), "IS NOT DISTINCT FROM") {
			t.Errorf("%s emits a construct ClickHouse cannot parse", filepath.Base(path))
		}
	}
}

// TestClickHouseTruncatesWithTheToStartOfFamily, and refuses a grain it has
// no function for rather than emitting a name that does not exist. A missing
// function fails at the warehouse with an error naming the function, which
// tells the caller nothing about the grain they asked for.
func TestClickHouseTruncatesWithTheToStartOfFamily(t *testing.T) {
	ch, err := dialect.Get("clickhouse")
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range []plan.Grain{
		plan.GrainSecond, plan.GrainMinute, plan.GrainHour, plan.GrainDay,
		plan.GrainWeek, plan.GrainMonth, plan.GrainQuarter, plan.GrainYear,
	} {
		got, err := ch.DateTrunc(g, osi.TypeDate, "`t`.`d`")
		if err != nil {
			t.Errorf("%s: %v", g, err)
			continue
		}
		if !strings.HasPrefix(got, "toStartOf") {
			t.Errorf("%s emits %q, which is not the native spelling", g, got)
		}
	}

	if got, err := ch.DateTrunc(plan.Grain("fortnight"), osi.TypeDate, "`t`.`d`"); err == nil {
		t.Errorf("an unsupported grain compiled to %q instead of being refused", got)
	}
}

// TestBacktickDialectsNeverEmitADoubleQuotedIdentifier.
//
// Databricks accepts double quotes only with ANSI_MODE on, a session setting
// this engine neither sets nor observes, and ClickHouse reads a double-quoted
// token as an identifier only in some positions. A single inherited Postgres
// quoting path would produce SQL that works on one cluster and fails on the
// next, which is the hardest kind of bug to be handed.
func TestBacktickDialectsNeverEmitADoubleQuotedIdentifier(t *testing.T) {
	for _, name := range []string{"databricks", "clickhouse"} {
		for _, path := range goldens(t, name) {
			sql := body(t, path)
			if strings.Contains(sql, `"`) {
				t.Errorf("%s emits a double-quoted token; %s quotes with backticks",
					filepath.Base(path), name)
			}
			if !strings.Contains(sql, "`") {
				t.Errorf("%s quotes nothing at all, so the override is not being reached",
					filepath.Base(path))
			}
		}
	}
}

// TestPositionalDialectsNeverEmitANumberedPlaceholder.
//
// Databricks, ClickHouse and Trino all bind by position. `$1` is not a
// placeholder there, it is a syntax error, and it is what the embedded
// Postgres emitter produces if Placeholder is not overridden. This catches
// the override being dropped, which is otherwise invisible until the first
// filtered query reaches a real cluster.
func TestPositionalDialectsNeverEmitANumberedPlaceholder(t *testing.T) {
	for _, name := range []string{"databricks", "clickhouse", "trino", "athena"} {
		for _, path := range goldens(t, name) {
			if strings.Contains(body(t, path), "$1") {
				t.Errorf("%s emits $1; %s binds by position with ?", filepath.Base(path), name)
			}
		}
	}
	// The premise: at least one golden is parameterised, or the loop above
	// proves nothing.
	var parameterised bool
	for _, path := range goldens(t, "trino") {
		if strings.Contains(body(t, path), "?") {
			parameterised = true
		}
	}
	if !parameterised {
		t.Error("no Trino golden binds a parameter, so the placeholder check is vacuous")
	}
}

// TestAthenaAndTrinoEmitTheSameSQL.
//
// Athena is managed Trino and the difference between them here is the
// capability note, nothing about the SQL. Asserting byte equality means a
// change to Trino's emitter that Athena does not inherit shows up as a
// failure rather than as two dialects quietly drifting apart, and it is also
// how somebody learns that athena embeds trino rather than postgres.
func TestAthenaAndTrinoEmitTheSameSQL(t *testing.T) {
	for _, path := range goldens(t, "trino") {
		athena := strings.TrimSuffix(path, ".trino.sql") + ".athena.sql"
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(athena)
		if err != nil {
			t.Errorf("%s has no Athena counterpart: %v", filepath.Base(path), err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from its Trino original; Athena is managed Trino and "+
				"the two emitters are not meant to diverge", filepath.Base(athena))
		}
	}
}

// TestTheUnverifiedDialectsSayTheyAreUnverified.
//
// Every one of these compiles and is golden tested, and not one has ever run
// a statement against the warehouse it targets. Postgres was in exactly that
// position until somebody ran it. An operator reading Capabilities is
// entitled to know which of those two states a dialect is in, and the only
// way that stays true is if it is asserted.
func TestTheUnverifiedDialectsSayTheyAreUnverified(t *testing.T) {
	for _, name := range []string{"databricks", "clickhouse", "trino", "athena"} {
		d, err := dialect.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		caps := d.Capabilities()
		if caps.Dialect != name {
			t.Errorf("%s reports itself as %q; an embedded parent's Capabilities is "+
				"being returned", name, caps.Dialect)
		}
		if !strings.Contains(caps.Note, "unproven") {
			t.Errorf("%s does not disclose that no executor has been verified: %q",
				name, caps.Note)
		}
		if caps.ColumnLevelSecurity || caps.RowLevelSecurity {
			t.Errorf("%s claims warehouse-enforced security this engine neither "+
				"configures nor reads", name)
		}
	}
}

// TestEveryRegisteredDialectHasGoldens, because registering one is two lines
// and generating its goldens is a separate step. A dialect present in
// Names() and absent from testdata/golden is one that has never been
// compiled even once, and it would be advertised over /v1/capabilities all
// the same.
func TestEveryRegisteredDialectHasGoldens(t *testing.T) {
	for _, name := range dialect.Names() {
		goldens(t, name)
	}
}
