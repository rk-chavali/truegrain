package exec_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rk-chavali/truegrain/internal/engine"
	sexec "github.com/rk-chavali/truegrain/internal/exec"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Postgres, executed.
//
// The dialect emitted golden-tested Postgres from v0.1.0 and nothing ever ran
// it, so "four warehouses" described the emitter rather than the product. These
// tests run the same fixture and the same metrics that the DuckDB parity suite
// runs, against a real server, and compare the answers.
//
// The live tests need TRUEGRAIN_TEST_POSTGRES_DSN. CI sets it from a service
// container, so they do not skip there, which matters because the build fails
// on any skipped test: a parity suite that quietly stops running is worse than
// one that was never written.

// TestARoleNameIsValidatedBeforeItReachesSQL.
//
// `SET LOCAL ROLE` takes an identifier and cannot be parameterized, so the
// caller's identity reaches SQL as text. That identity arrives from a JWT,
// which makes it the one attacker-controlled input in the executor, and being
// wrong here is an authorization bypass rather than a broken query.
//
// No database needed, which is the point: the rule is asserted directly rather
// than only through a server that might be absent.
func TestARoleNameIsValidatedBeforeItReachesSQL(t *testing.T) {
	refused := []string{
		"",
		`postgres"; DROP TABLE orders; --`,
		`admin" OR "1"="1`,
		"role with spaces",
		"role\nnewline",
		`back\slash`,
		"semi;colon",
		"quote'single",
		"tab\there",
		strings.Repeat("a", 64), // PostgreSQL truncates at 63 and truncation is a different role
	}
	for _, name := range refused {
		if err := sexec.ValidateRoleName(name); err == nil {
			t.Errorf("%q was accepted as a role name", name)
		}
	}

	// What an identity provider actually puts in a subject has to still work,
	// or the feature is unusable and somebody turns it off.
	for _, name := range []string{
		"analyst",
		"svc_reporting",
		"alice@acme.com",
		"team-northeast",
		"svc.reporting.prod",
		strings.Repeat("a", 63),
	} {
		if err := sexec.ValidateRoleName(name); err != nil {
			t.Errorf("%q is a legitimate role name and was refused: %v", name, err)
		}
	}
}

// postgresOrSkip returns an executor against the test server.
func postgresOrSkip(t *testing.T) *sexec.Postgres {
	t.Helper()
	dsn := os.Getenv("TRUEGRAIN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TRUEGRAIN_TEST_POSTGRES_DSN is not set; see .github/workflows/ci.yml for the shape")
	}
	pg, err := sexec.NewPostgres(context.Background(), sexec.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("connecting to the test server: %v", err)
	}
	t.Cleanup(func() { pg.Close() })

	seedPostgres(t, dsn)
	return pg
}

// seedPostgres loads the same fixture the DuckDB suite uses.
//
// The same file, unmodified. If it ever needs a Postgres-specific variant then
// the two suites are no longer comparing the same data, and a parity result
// between them stops meaning anything.
func seedPostgres(t *testing.T, dsn string) {
	t.Helper()
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql not on PATH; needed to load the fixture")
	}
	seed := filepath.Join("..", "..", "testdata", "fixtures", "seed.sql")
	cmd := exec.Command(psql, dsn, "-v", "ON_ERROR_STOP=1", "-q", "-f", seed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding postgres: %v\n%s", err, out)
	}
}

// onlyCell renders the single value of a single-row result as text.
//
// Text rather than float64, unlike the DuckDB suite's `scalar`: the scale is
// the thing under test here. Parsing "75.50" into a float would pass whether
// the executor returned 75.50 or 75.5, and losing a trailing zero on money is
// exactly the class of bug this is watching for.
func onlyCell(t *testing.T, rows [][]any) string {
	t.Helper()
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("want one value, got %d row(s)", len(rows))
	}
	return strings.TrimSpace(fmt.Sprint(rows[0][0]))
}

// postgresEngine wires the Postgres executor to the same model the DuckDB
// parity suite uses, so a disagreement between them is about the warehouse
// rather than about the model.
func postgresEngine(t *testing.T, pg *sexec.Postgres) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "testdata", "models", "retail.yaml"),
		Dialect:   "postgres",
		Resolver:  govern.AllowAll{},
		Executor:  pg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// TestPostgresAnswersTheKnownGoodNumbers.
//
// The fixture's answers are fixed by construction and already asserted against
// DuckDB and BigQuery. A third warehouse agreeing on them is what makes
// "dialect-correct" a measurement.
func TestPostgresAnswersTheKnownGoodNumbers(t *testing.T) {
	pg := postgresOrSkip(t)
	eng := postgresEngine(t, pg)

	for _, c := range []struct {
		metric string
		want   string
	}{
		{"order_revenue", "885.50"},
		{"line_revenue", "885.50"},
		{"order_count", "5"},
		{"largest_order", "400.00"},
		{"units_sold", "22"},
		{"shipped_revenue", "175.50"},
	} {
		rows, err := eng.Query(context.Background(), govern.Identity{Subject: "parity"},
			plan.Request{Metrics: []string{c.metric}})
		if err != nil {
			t.Errorf("%s: %v", c.metric, err)
			continue
		}
		if got := onlyCell(t, rows.Rows); got != c.want {
			t.Errorf("%s: want %s, got %s", c.metric, c.want, got)
		}
	}
}

// TestPostgresRefusesTheFanOut, and the naive join really does inflate.
//
// The same assertion the DuckDB suite makes, because the point is not that one
// warehouse refuses: it is that the refusal is a property of the planner and
// therefore holds wherever the model is served.
func TestPostgresRefusesTheFanOut(t *testing.T) {
	pg := postgresOrSkip(t)

	// First prove the fixture still demonstrates the trap on this server.
	naive, err := pg.Execute(context.Background(), govern.Identity{Subject: "parity"},
		`SELECT SUM(o.order_total) FROM main.orders o
		 LEFT JOIN main.order_lines l ON o.order_id = l.order_id`, nil)
	if err != nil {
		t.Fatalf("running the naive join: %v", err)
	}
	inflated := onlyCell(t, naive.Rows)
	if inflated == "885.50" {
		t.Fatalf("the fixture no longer demonstrates fan-out: the naive join gives %s", inflated)
	}
	t.Logf("the join the planner refuses would have answered %s instead of 885.50", inflated)

	eng := postgresEngine(t, pg)
	_, err = eng.Query(context.Background(), govern.Identity{Subject: "parity"},
		plan.Request{Metrics: []string{"order_revenue"}, Dimensions: []string{"order_lines.item_id"}})
	if err == nil {
		t.Error("the engine ran the query that produces the inflated number")
	}
}

// TestPostgresRendersDecimalsAsDecimals.
//
// NUMERIC is where this went wrong on BigQuery: every monetary column came back
// as `271/2` instead of `135.50`, through the CLI and through every SDK,
// because the underlying type renders a fraction. pgtype.Numeric has the same
// hazard in a different shape.
func TestPostgresRendersDecimalsAsDecimals(t *testing.T) {
	pg := postgresOrSkip(t)

	rows, err := pg.Execute(context.Background(), govern.Identity{Subject: "parity"},
		"SELECT order_total FROM main.orders WHERE order_id = 3", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := onlyCell(t, rows.Rows)
	if got != "75.50" {
		t.Errorf("want 75.50 with its scale intact, got %q", got)
	}
	if strings.Contains(got, "/") {
		t.Errorf("a decimal was rendered as a fraction: %q", got)
	}
}

// TestPostgresWillNotWrite.
//
// Every query runs in a read-only transaction. The planner only emits SELECT,
// so this changes nothing today; it means a future bug in the emitter cannot
// write, rather than being trusted not to. Enforced by the server, not by us.
func TestPostgresWillNotWrite(t *testing.T) {
	pg := postgresOrSkip(t)

	_, err := pg.Execute(context.Background(), govern.Identity{Subject: "parity"},
		"CREATE TABLE main.should_not_exist (x int)", nil)
	if err == nil {
		t.Fatal("a write succeeded through a read-only executor")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("the refusal should come from the read-only transaction: %v", err)
	}
}

// TestAConnectionStringNeverReachesAnError.
//
// An error goes to a log, to a terminal and over the API. A password in any of
// those is a credential leaking through a diagnostic, which is how credentials
// usually leak.
//
// pgx already provides this: a parse failure renders `password=xxxxx` and a
// connection failure names only the user and database. So this passes today
// without any help from us, and that is the point. It fails the moment somebody
// adds `fmt.Errorf("connecting to %s", dsn)` to this package, which is the
// realistic way the property gets lost.
func TestAConnectionStringNeverReachesAnError(t *testing.T) {
	const secret = "sup3rs3cret"

	for _, dsn := range []string{
		// Refused connection: reaches the server-dial path.
		"postgres://someone:" + secret + "@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1",
		"host=127.0.0.1 port=1 user=someone password=" + secret + " dbname=nope connect_timeout=1",
		// Unparseable: reaches the config path, which echoes more of the input.
		"not a dsn at all password=" + secret,
	} {
		_, err := sexec.NewPostgres(context.Background(), sexec.PostgresOptions{DSN: dsn})
		if err == nil {
			t.Fatalf("expected a failure for %q", dsn)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the password reached the error text:\n%v", err)
		}
	}
}

// TestThePostgresClassifierDoesNotOverReach.
//
// The dangerous direction is calling a real failure transient: a permission
// denial retried is three audit entries and the same answer, and a statement
// cancelled for running too long retried runs too long again.
//
// No database needed, which is the point: the rule is asserted directly
// rather than only through a server that might be absent.
func TestThePostgresClassifierDoesNotOverReach(t *testing.T) {
	pg := &sexec.Postgres{}

	final := map[string]string{
		"42501": "insufficient_privilege, a permission denial",
		"42601": "syntax_error",
		"42P01": "undefined_table",
		"42703": "undefined_column",
		"57014": "query_canceled, which is usually the statement timeout",
		"22012": "division_by_zero",
		"23505": "unique_violation",
	}
	for code, what := range final {
		if pg.Transient(&pgconn.PgError{Code: code, Message: what}) {
			t.Errorf("%s (%s) was classified as transient; retrying it costs and cannot help", code, what)
		}
	}

	transient := map[string]string{
		"53300": "too_many_connections",
		"57P01": "admin_shutdown",
		"57P03": "cannot_connect_now, a server still starting",
		"58000": "system_error",
		"08006": "connection_failure",
		"40001": "serialization_failure",
		"40P01": "deadlock_detected",
	}
	for code, what := range transient {
		if !pg.Transient(&pgconn.PgError{Code: code, Message: what}) {
			t.Errorf("%s (%s) was classified as final; a caller sees a flaky warehouse as a refusal", code, what)
		}
	}

	// A caller giving up is not the warehouse faltering.
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if pg.Transient(err) {
			t.Errorf("%v was classified as transient, which ignores the caller", err)
		}
	}
	if pg.Transient(nil) {
		t.Error("a nil error was classified as transient")
	}
	if pg.Transient(errors.New("something nobody classified")) {
		t.Error("an unrecognised error was classified as transient; the default must be final")
	}
}
