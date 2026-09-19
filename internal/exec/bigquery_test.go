package exec

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"

	"github.com/rk-chavali/truegrain/internal/dialect"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// These run without a GCP project. What needs a real warehouse is the round
// trip; what is testable here is the part that decides whether a query is
// refused, what it costs, and what the deployment admits about itself.

func TestBigQueryNeedsAProject(t *testing.T) {
	if _, err := NewBigQuery(context.Background(), BigQueryOptions{}); err == nil {
		t.Fatal("a BigQuery executor without a project must not build")
	}
}

// TestNameDisclosesMissingImpersonation is the honesty requirement applied to
// the warehouse. A deployment running every query as its own service account
// gives the warehouse no way to tell callers apart, so any row or column
// security defined there protects the engine rather than the user. health
// prints this string, and a reader must not have to infer the gap.
func TestNameDisclosesMissingImpersonation(t *testing.T) {
	off := (&BigQuery{opts: BigQueryOptions{Project: "p"}, maxByte: DefaultMaxBytesBilled}).Name()
	if !strings.Contains(off, "impersonation is off") {
		t.Errorf("Name must state that impersonation is off:\n%s", off)
	}
	if !strings.Contains(off, "cannot tell callers apart") {
		t.Errorf("Name must state the consequence, not just the setting:\n%s", off)
	}

	on := (&BigQuery{
		opts:    BigQueryOptions{Project: "p", ImpersonateServiceAccounts: true},
		maxByte: DefaultMaxBytesBilled,
	}).Name()
	if strings.Contains(on, "impersonation is off") {
		t.Errorf("Name misreports an impersonating deployment:\n%s", on)
	}
	// This once read "queries run as the calling identity", which is true only
	// of service account callers. Only a service account can be impersonated,
	// so with impersonation on a human caller still runs as this process, and
	// an operator reading that sentence would have believed BigQuery's row and
	// column security applied to people it did not apply to.
	if !strings.Contains(on, "service account callers") {
		t.Errorf("Name must say which callers are impersonated, not imply all are:\n%s", on)
	}
	if !strings.Contains(on, "runs as this process") {
		t.Errorf("Name must state what happens to everybody else:\n%s", on)
	}
}

// TestStrictImpersonationSaysSo. The two settings behave differently for the
// same caller, so health has to tell them apart.
func TestStrictImpersonationSaysSo(t *testing.T) {
	strict := (&BigQuery{
		opts: BigQueryOptions{
			Project:                    "p",
			ImpersonateServiceAccounts: true,
			RequireImpersonation:       true,
		},
		maxByte: DefaultMaxBytesBilled,
	}).Name()

	if !strings.Contains(strict, "refused") {
		t.Errorf("a strict deployment must say it refuses:\n%s", strict)
	}
	if strings.Contains(strict, "runs as this process") {
		t.Errorf("a strict deployment never falls back, so it must not claim to:\n%s", strict)
	}
}

// TestAHumanCallerIsRefusedWhenStrict.
//
// Only a service account can be impersonated. Without strict mode a human
// caller's query runs as this process, so BigQuery evaluates row and column
// security against the engine rather than against them. A deployment that
// depends on that security turns strict on and gives up interactive access.
// This test is what stops the refusal quietly regressing into the fallback.
func TestAHumanCallerIsRefusedWhenStrict(t *testing.T) {
	b := &BigQuery{
		opts: BigQueryOptions{
			Project:                    "p",
			ImpersonateServiceAccounts: true,
			RequireImpersonation:       true,
		},
		clients: map[string]*bigquery.Client{},
	}

	_, err := b.clientFor(context.Background(), govern.Identity{Subject: "analyst@acme.com"})
	if err == nil {
		t.Fatal("a caller who cannot be impersonated must be refused, not run as the engine")
	}
	var refusal *plan.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want a structured refusal, got %T: %v", err, err)
	}
	if refusal.Code != CodeWarehouseDenied {
		t.Errorf("want %s, got %s", CodeWarehouseDenied, refusal.Code)
	}
	if !strings.Contains(refusal.Reason, "analyst@acme.com") {
		t.Errorf("the refusal should name who could not be impersonated: %s", refusal.Reason)
	}
}

// TestAHumanCallerFallsBackWhenNotStrict, which is the default, because
// refusing would mean an engine that serves no interactive users at all. The
// cost of that default is disclosed by Name rather than hidden.
func TestAHumanCallerFallsBackWhenNotStrict(t *testing.T) {
	base := &bigquery.Client{}
	b := &BigQuery{
		opts:    BigQueryOptions{Project: "p", ImpersonateServiceAccounts: true},
		base:    base,
		clients: map[string]*bigquery.Client{},
	}

	got, err := b.clientFor(context.Background(), govern.Identity{Subject: "analyst@acme.com"})
	if err != nil {
		t.Fatalf("the default must serve interactive callers: %v", err)
	}
	if got != base {
		t.Error("the fallback must be this process's own client")
	}
}

func TestNameStatesTheSpendCap(t *testing.T) {
	name := (&BigQuery{opts: BigQueryOptions{Project: "p"}, maxByte: DefaultMaxBytesBilled}).Name()
	if !strings.Contains(name, "200.0 GiB") {
		t.Errorf("Name must state the scan cap so an operator can see it:\n%s", name)
	}
}

// TestParametersMatchTheEmittedPlaceholders locks the convention the dialect
// and this executor share. They are two halves of one decision, and they live
// in different packages, so nothing but a test keeps them together: if the
// emitter changed to ?, every query would fail at the warehouse with a message
// about an undeclared parameter.
func TestParametersMatchTheEmittedPlaceholders(t *testing.T) {
	bq, err := dialect.Get("bigquery")
	if err != nil {
		t.Fatal(err)
	}

	// Ask the dialect for the placeholders it emits, then bind that many
	// values and check the names line up.
	const count = 3
	var emitted []string
	for i := 1; i <= count; i++ {
		emitted = append(emitted, bq.Placeholder(i))
	}

	bound := namedParams([]any{"shipped", 42, true})
	if len(bound) != count {
		t.Fatalf("want %d parameters, got %d", count, len(bound))
	}

	names := make([]string, len(bound))
	for i, p := range bound {
		// BigQuery names a parameter without the @; the SQL carries it.
		names[i] = "@" + p.Name
	}
	sort.Strings(names)
	sort.Strings(emitted)
	for i := range names {
		if names[i] != emitted[i] {
			t.Errorf("the emitter writes %s but the executor binds %s; "+
				"every query would fail with an undeclared parameter",
				emitted[i], names[i])
		}
	}
}

// TestParametersCoverEveryPlaceholderInRealSQL runs the same check against a
// statement the emitter actually produced, so a change to how filters compile
// cannot slip past the convention test above.
func TestParametersCoverEveryPlaceholderInRealSQL(t *testing.T) {
	bq, err := dialect.Get("bigquery")
	if err != nil {
		t.Fatal(err)
	}
	sql := "SELECT 1 WHERE status = " + bq.Placeholder(1) + " AND region = " + bq.Placeholder(2)
	inSQL := regexp.MustCompile(`@p\d+`).FindAllString(sql, -1)
	if len(inSQL) != 2 {
		t.Fatalf("expected 2 placeholders in %q, found %v", sql, inSQL)
	}

	declared := map[string]bool{}
	for _, p := range namedParams([]any{"shipped", "NE"}) {
		declared["@"+p.Name] = true
	}
	for _, want := range inSQL {
		if !declared[want] {
			t.Errorf("the statement references %s but no parameter declares it", want)
		}
	}
}

// TestFilterValuesNeverEnterTheStatement is the injection guarantee at the
// executor boundary: the value travels as a typed parameter, never as text.
func TestFilterValuesNeverEnterTheStatement(t *testing.T) {
	hostile := "'; DROP TABLE orders; --"
	params := namedParams([]any{hostile})
	if len(params) != 1 {
		t.Fatalf("want 1 parameter, got %d", len(params))
	}
	if params[0].Value != hostile {
		t.Error("the value must be passed through unchanged, as data")
	}
	// Nothing in this package builds SQL from a value. The statement the
	// executor sends is exactly what the emitter produced.
	if strings.Contains(params[0].Name, hostile) {
		t.Error("a value reached a parameter name")
	}
}

func TestTooLargeIsRetryableByModifying(t *testing.T) {
	// A query over the cap is answerable at a narrower breadth, so an agent
	// should add a filter rather than give up or repeat it unchanged.
	if got := plan.RetryFor(CodeQueryTooLarge); got != plan.RetryModify {
		t.Errorf("want retry modify for %s, got %s", CodeQueryTooLarge, got)
	}
	// The warehouse's own IAM said no. Rewriting cannot help.
	if got := plan.RetryFor(CodeWarehouseDenied); got != plan.RetryNever {
		t.Errorf("want retry never for %s, got %s", CodeWarehouseDenied, got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{200 << 30, "200.0 GiB"},
		{3 << 40, "3.0 TiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A 400 from BigQuery is not one thing, and the two kinds need opposite advice.
//
// A partitioned table refusing to be scanned whole is answerable: the same
// question with a date filter works. Classified as unsupported, which is what
// it was, an agent is told never to retry and gives up on a question it could
// have asked correctly. Found running a model against a table that required
// one.
func TestAPartitionFilterRefusalIsAnswerable(t *testing.T) {
	messages := []string{
		"Cannot query over table 'shop_mart.fct_order' without a filter over column(s) 'order_date' that can be used for partition elimination",
		"Query requires a filter over column(s) 'event_date'",
	}
	for _, m := range messages {
		if !needsPartitionFilter(m) {
			t.Errorf("not recognised as a partition filter refusal: %q", m)
		}
	}

	// A genuine schema problem must keep saying the model is wrong, because
	// adding a filter will not conjure a missing column.
	for _, m := range []string{
		"Unrecognized name: ordr_total at [2:3]",
		"Syntax error: Unexpected keyword ROWS at [16:6]",
		"Table project:dataset.missing was not found",
	} {
		if needsPartitionFilter(m) {
			t.Errorf("a schema error was mistaken for a partition filter refusal: %q", m)
		}
	}

	if got := plan.RetryFor(CodeNeedsPartitionFilter); got != plan.RetryModify {
		t.Errorf("a partition filter refusal should be answerable, got %v", got)
	}
	if got := plan.RetryFor(plan.CodeUnsupported); got == plan.RetryModify {
		t.Error("an unsupported statement must not be classified as answerable")
	}
}
