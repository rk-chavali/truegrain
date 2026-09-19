package pgwire_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rk-chavali/truegrain/internal/duckdbtest"
	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/exec"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/pgwire"
)

// The wire protocol, exercised by a real PostgreSQL driver.
//
// pgx is the client here rather than a hand-written message stream, and that
// is the point: this surface exists so that software written to talk to
// PostgreSQL can talk to a semantic model instead, and the only way to know
// that works is to let such software try. pgx uses the extended query
// protocol by default, which is what every BI tool uses and what a
// simple-query-only server would fail.
//
// These tests need DuckDB on PATH to execute, the same as the rest of the
// correctness suite, so CI runs them and the build fails on a skip.

const testToken = "wire-test-token-not-a-real-secret"

// start brings up a server on an ephemeral port and returns its address.
func start(t *testing.T, auth pgwire.Authenticator) string {
	t.Helper()

	ex, err := exec.NewDuckDB(exec.DuckDBOptions{
		Database: duckdbtest.Seed(t, filepath.Join("..", "..", "..")),
	})
	if err != nil {
		t.Fatalf("building a DuckDB executor: %v", err)
	}
	t.Cleanup(func() { _ = ex.Close() })

	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "..", "testdata", "models"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Executor:  ex,
	})
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := pgwire.New(eng, auth)
	go func() { _ = srv.ListenAndServe(ctx, addr) }()

	// Wait for the listener rather than sleeping a fixed time.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the server never started on %s", addr)
	return ""
}

func connect(t *testing.T, addr, password string) *pgx.Conn {
	t.Helper()
	dsn := fmt.Sprintf("host=%s port=%s user=analyst dbname=truegrain sslmode=disable",
		host(addr), port(addr))
	if password != "" {
		dsn += " password=" + password
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func host(addr string) string { h, _, _ := net.SplitHostPort(addr); return h }
func port(addr string) string { _, p, _ := net.SplitHostPort(addr); return p }

// queryErr runs a statement and returns the error however pgx chose to
// deliver it.
//
// pgx.Query reports a server error on the first read rather than from the
// call, so checking only the call's return silently passes on a refusal that
// was delivered perfectly well. Draining is what actually asks.
func queryErr(t *testing.T, conn *pgx.Conn, sql string) error {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	return rows.Err()
}

// TestADriverAnswersAGovernedQuery.
//
// The whole point of the surface, end to end: software that speaks
// PostgreSQL gets a number out of a semantic model, and the number is the
// right one. 885.50 is the true total, which is the figure the fan-out
// refusal exists to protect.
func TestADriverAnswersAGovernedQuery(t *testing.T) {
	conn := connect(t, start(t, nil), "")

	rows, err := conn.Query(context.Background(),
		`SELECT "customers.region", order_revenue FROM retail GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	total := 0.0
	seen := 0
	for rows.Next() {
		var region string
		var revenue float64
		if err := rows.Scan(&region, &revenue); err != nil {
			t.Fatal(err)
		}
		total += revenue
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if seen == 0 {
		t.Fatal("no rows came back")
	}
	if total != 885.50 {
		t.Errorf("total = %.2f, want 885.50", total)
	}
}

// TestAMeasureArrivesAsANumberNotAString.
//
// A client told a column is text renders a measure as a label and will not
// plot it, so every chart built on this would come out as a category axis.
// Scanning into a float is the assertion: it only succeeds if the declared
// OID is numeric.
func TestAMeasureArrivesAsANumberNotAString(t *testing.T) {
	conn := connect(t, start(t, nil), "")

	var revenue float64
	err := conn.QueryRow(context.Background(),
		`SELECT order_revenue FROM retail`).Scan(&revenue)
	if err != nil {
		t.Fatalf("a measure did not scan as a number: %v", err)
	}
	if revenue != 885.50 {
		t.Errorf("order_revenue = %.2f, want 885.50", revenue)
	}
}

// TestTheFanOutRefusalSurvivesTheProtocol.
//
// The product's whole claim, over the surface a business actually uses. It
// has to arrive as an error a client will show rather than as an empty
// result, and the hint has to survive, because the hint is what turns a
// rejection into a next step.
func TestTheFanOutRefusalSurvivesTheProtocol(t *testing.T) {
	conn := connect(t, start(t, nil), "")

	err := queryErr(t, conn,
		`SELECT "order_lines.item_id", order_revenue FROM retail GROUP BY 1`)
	if err == nil {
		t.Fatal("the fan-out was not refused")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("got %T (%v), want a *pgconn.PgError so a client can branch on it", err, err)
	}

	if !strings.Contains(pg.Message, "repeats orders rows") {
		t.Errorf("Message = %q", pg.Message)
	}
	if !strings.Contains(pg.Detail, "fan_out_would_inflate") {
		t.Errorf("Detail = %q; the refusal code must travel where a client can read it", pg.Detail)
	}
	if !strings.Contains(pg.Hint, "line_revenue") {
		t.Errorf("Hint = %q; it must name the metric that answers correctly", pg.Hint)
	}
	if !strings.Contains(pg.Detail, "modify") {
		t.Errorf("Detail = %q; the retry class must travel too", pg.Detail)
	}
}

// TestTheConnectionSurvivesARefusal.
//
// A dashboard sends many queries on one connection. If a refusal left the
// connection unusable, one bad panel would break every other panel on the
// page, and the failure would look like a network problem.
func TestTheConnectionSurvivesARefusal(t *testing.T) {
	conn := connect(t, start(t, nil), "")
	ctx := context.Background()

	if err := queryErr(t, conn,
		`SELECT "order_lines.item_id", order_revenue FROM retail GROUP BY 1`); err == nil {
		t.Fatal("the fan-out was not refused")
	}

	var revenue float64
	if err := conn.QueryRow(ctx, `SELECT order_revenue FROM retail`).Scan(&revenue); err != nil {
		t.Fatalf("the connection was unusable after a refusal: %v", err)
	}
	if revenue != 885.50 {
		t.Errorf("order_revenue = %.2f after a refusal", revenue)
	}
}

// TestAWriteIsRefused.
//
// There is no write path, and a client has to be told so rather than have
// its statement silently succeed or silently do nothing.
func TestAWriteIsRefused(t *testing.T) {
	conn := connect(t, start(t, nil), "")

	for _, sql := range []string{
		`INSERT INTO retail VALUES (1)`,
		`UPDATE retail SET order_revenue = 0`,
		`DELETE FROM retail`,
		`DROP TABLE retail`,
		`CREATE TABLE x (a int)`,
	} {
		if _, err := conn.Exec(context.Background(), sql); err == nil {
			t.Errorf("%q was not refused", sql)
		}
	}
}

// TestNoStatementReachesTheWarehouseAsText.
//
// The security property. A payload in a filter value is data and survives as
// data; what it must never do is change the statement the warehouse runs. If
// it could, this query would error from the warehouse rather than return the
// filtered total, or would affect the table.
func TestNoStatementReachesTheWarehouseAsText(t *testing.T) {
	conn := connect(t, start(t, nil), "")
	ctx := context.Background()

	// Nullable, because a SUM over no matching rows is NULL rather than zero,
	// and no status equals this payload.
	var revenue *float64
	err := conn.QueryRow(ctx,
		`SELECT order_revenue FROM retail WHERE "orders.status" = 'shipped''; DROP TABLE orders; --'`).
		Scan(&revenue)
	if err != nil {
		t.Fatalf("the payload was not treated as a value: %v", err)
	}
	if revenue != nil && *revenue != 0 {
		t.Errorf("a status no row has returned %.2f", *revenue)
	}

	// The model is still there, which it would not be if the payload had run.
	var after float64
	if err := conn.QueryRow(ctx, `SELECT order_revenue FROM retail`).Scan(&after); err != nil {
		t.Fatalf("the warehouse is broken after the payload: %v", err)
	}
	if after != 885.50 {
		t.Errorf("order_revenue = %.2f after the payload, want 885.50", after)
	}
}

// TestAuthenticationIsRequiredWhenConfigured.
func TestAuthenticationIsRequiredWhenConfigured(t *testing.T) {
	auth := pgwire.TokenAuth{
		Tokens: map[string]govern.Identity{testToken: {Subject: "analyst@example.com"}},
	}
	addr := start(t, auth)

	dsn := fmt.Sprintf("host=%s port=%s user=analyst dbname=truegrain sslmode=disable",
		host(addr), port(addr))

	if _, err := pgx.Connect(context.Background(), dsn+" password=wrong-token"); err == nil {
		t.Error("a wrong token was accepted")
	}
	if _, err := pgx.Connect(context.Background(), dsn); err == nil {
		t.Error("a connection with no token was accepted")
	}

	conn, err := pgx.Connect(context.Background(), dsn+" password="+testToken)
	if err != nil {
		t.Fatalf("the correct token was refused: %v", err)
	}
	defer conn.Close(context.Background())

	var revenue float64
	if err := conn.QueryRow(context.Background(),
		`SELECT order_revenue FROM retail`).Scan(&revenue); err != nil {
		t.Fatal(err)
	}
}

// TestAFailedLoginDoesNotSayWhichHalfWasWrong, because a message that
// distinguishes an unknown user from a wrong token is how tokens get
// enumerated.
func TestAFailedLoginDoesNotSayWhichHalfWasWrong(t *testing.T) {
	auth := pgwire.TokenAuth{
		Tokens: map[string]govern.Identity{testToken: {Subject: "analyst@example.com"}},
	}
	addr := start(t, auth)

	_, err := pgx.Connect(context.Background(), fmt.Sprintf(
		"host=%s port=%s user=nobody dbname=truegrain sslmode=disable password=wrong",
		host(addr), port(addr)))
	if err == nil {
		t.Fatal("a bad login was accepted")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("the error echoed the expected token")
	}
	for _, leak := range []string{"unknown user", "no such user", "wrong password", "not recognised"} {
		if strings.Contains(strings.ToLower(err.Error()), leak) {
			t.Errorf("the error says which half was wrong: %v", err)
		}
	}
}

// TestASettingsRoundTripDoesNotBreakTheConnection.
//
// Drivers push SET and read SHOW at connect. Failing any of them stops a
// client before its first real query, which looks like the server is down.
func TestASettingsRoundTripDoesNotBreakTheConnection(t *testing.T) {
	conn := connect(t, start(t, nil), "")
	ctx := context.Background()

	for _, sql := range []string{
		`SET application_name = 'metabase'`,
		`SET extra_float_digits = 3`,
		`BEGIN`,
		`COMMIT`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Errorf("%q failed: %v", sql, err)
		}
	}

	var revenue float64
	if err := conn.QueryRow(ctx, `SELECT order_revenue FROM retail`).Scan(&revenue); err != nil {
		t.Fatalf("the connection was unusable after settings: %v", err)
	}
}

// TestACatalogueListingNamesTheNamespacesAndFields.
//
// A BI tool lists tables and columns before it will draw anything, so this is
// load bearing for the tool ever getting as far as a query.
func TestACatalogueListingNamesTheNamespacesAndFields(t *testing.T) {
	conn := connect(t, start(t, nil), "")
	ctx := context.Background()

	var table string
	if err := conn.QueryRow(ctx,
		`SELECT tablename FROM pg_catalog.pg_tables`).Scan(&table); err != nil {
		t.Fatalf("listing tables failed: %v", err)
	}
	if table != "retail" {
		t.Errorf("tablename = %q, want retail", table)
	}

	rows, err := conn.Query(ctx,
		`SELECT column_name, data_type FROM information_schema.columns`)
	if err != nil {
		t.Fatalf("listing columns failed: %v", err)
	}
	defer rows.Close()

	types := map[string]string{}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			t.Fatal(err)
		}
		types[name] = kind
	}

	// A metric has to be numeric and a dimension text, or a tool cannot tell
	// what it may plot against what.
	if types["order_revenue"] != "numeric" {
		t.Errorf("order_revenue is %q, want numeric", types["order_revenue"])
	}
	if types["customers.region"] != "text" {
		t.Errorf("customers.region is %q, want text", types["customers.region"])
	}
}

// TestTheQueryIsAudited, because a wire-protocol query that escaped the audit
// record would be a hole in the evidence half of the product.
func TestTheQueryIsAudited(t *testing.T) {
	ex, err := exec.NewDuckDB(exec.DuckDBOptions{
		Database: duckdbtest.Seed(t, filepath.Join("..", "..", "..")),
	})
	if err != nil {
		t.Fatalf("building a DuckDB executor: %v", err)
	}
	defer func() { _ = ex.Close() }()

	recent := govern.NewRecentAudit(16)
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "..", "testdata", "models"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Executor:  ex,
		Audit:     recent,
	})
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pgwire.New(eng, nil).ListenAndServe(ctx, addr) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn, err := pgx.Connect(ctx, fmt.Sprintf(
		"host=%s port=%s user=analyst dbname=truegrain sslmode=disable",
		host(addr), port(addr)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// One allowed and one refused, so both halves of the record are covered.
	var revenue float64
	if err := conn.QueryRow(ctx, `SELECT order_revenue FROM retail`).Scan(&revenue); err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Query(ctx, `SELECT "order_lines.item_id", order_revenue FROM retail GROUP BY 1`)

	events := recent.Recent(0)
	var allowed, refused int
	for _, e := range events {
		switch e.Decision {
		case "allowed":
			allowed++
		case "refused":
			refused++
		}
	}
	if allowed == 0 {
		t.Error("the answered query was not recorded")
	}
	if refused == 0 {
		t.Error("the refusal was not recorded, so nothing can report it")
	}
}
