package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Caching a result.
//
// Two things are worth asserting and one of them is a security property.
//
// It has to actually save a warehouse round trip, or it is doing nothing.
//
// It must never serve a result to a caller who was not authorized to see
// it. A cache in front of a governed query is one line away from being an
// access control bypass with a latency benefit, so most of what is below
// is about that line.

// countingExecutor records how many times the warehouse was reached.
type countingExecutor struct {
	calls atomic.Int32
	rows  [][]any
}

func (c *countingExecutor) Name() string { return "counting (test)" }
func (c *countingExecutor) Close() error { return nil }

func (c *countingExecutor) Execute(context.Context, govern.Identity, string, []any) (*engine.Rows, error) {
	c.calls.Add(1)
	rows := c.rows
	if rows == nil {
		rows = [][]any{{885.50}}
	}
	return &engine.Rows{Columns: []string{"order_revenue"}, Rows: rows}, nil
}

func cachingEngine(t *testing.T, ex engine.Executor, ttl time.Duration) *engine.Engine {
	t.Helper()
	var cache *engine.Cache
	if ttl > 0 {
		var err error
		cache, err = engine.NewCache(engine.CacheOptions{TTL: ttl})
		if err != nil {
			t.Fatal(err)
		}
	}
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "testdata", "models", "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Executor:  ex,
		Cache:     cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func askAs(eng *engine.Engine, subject string) (*engine.Result, error) {
	return eng.Query(context.Background(), govern.Identity{Subject: subject},
		plan.Request{Metrics: []string{"order_revenue"}})
}

// TestARepeatedQuestionDoesNotReachTheWarehouse, which is the entire point.
func TestARepeatedQuestionDoesNotReachTheWarehouse(t *testing.T) {
	ex := &countingExecutor{}
	eng := cachingEngine(t, ex, time.Minute)

	for range 5 {
		if _, err := askAs(eng, "analyst@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if got := ex.calls.Load(); got != 1 {
		t.Errorf("the warehouse was reached %d times for the same question, want 1", got)
	}
}

// TestTheCachedAnswerIsTheSameAnswer, because a cache that returns
// something subtly different is worse than none.
func TestTheCachedAnswerIsTheSameAnswer(t *testing.T) {
	eng := cachingEngine(t, &countingExecutor{}, time.Minute)

	first, err := askAs(eng, "analyst@example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := askAs(eng, "analyst@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if second.Rows[0][0] != first.Rows[0][0] {
		t.Errorf("cached %v, computed %v", second.Rows[0][0], first.Rows[0][0])
	}
	// The provenance has to survive too: a disputed number is settled with
	// the compiled SQL and the model version that produced it.
	if second.CompiledSQL != first.CompiledSQL || second.ModelVersion != first.ModelVersion {
		t.Error("a cached result lost its provenance")
	}
}

// TestOneCallerCannotReadAnothersCachedResult.
//
// The security property. Row policy is applied before compilation, so two
// callers with different access already produce different SQL and would
// not collide anyway; the subject is in the key regardless, so that a bug
// in row-policy application cannot become a cross-caller data leak. This
// asserts the belt as well as the braces.
func TestOneCallerCannotReadAnothersCachedResult(t *testing.T) {
	ex := &countingExecutor{}
	eng := cachingEngine(t, ex, time.Minute)

	if _, err := askAs(eng, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := ex.calls.Load(); got != 1 {
		t.Fatalf("setup: %d calls", got)
	}

	if _, err := askAs(eng, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := ex.calls.Load(); got != 2 {
		t.Errorf("a second caller was served from the first caller's cache "+
			"entry (%d warehouse calls, want 2)", got)
	}
}

// TestAnExpiredEntryIsNotServed.
//
// A cache with no expiry serves yesterday's number forever, and the whole
// product is about a number being right.
func TestAnExpiredEntryIsNotServed(t *testing.T) {
	ex := &countingExecutor{}
	eng := cachingEngine(t, ex, 50*time.Millisecond)

	if _, err := askAs(eng, "analyst@example.com"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if _, err := askAs(eng, "analyst@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := ex.calls.Load(); got != 2 {
		t.Errorf("%d warehouse calls; an expired entry was served", got)
	}
}

// TestADifferentQuestionIsNotAHit, which sounds obvious and is the way a
// cache key goes wrong.
func TestADifferentQuestionIsNotAHit(t *testing.T) {
	ex := &countingExecutor{}
	eng := cachingEngine(t, ex, time.Minute)
	ctx := context.Background()
	id := govern.Identity{Subject: "analyst@example.com"}

	if _, err := eng.Query(ctx, id, plan.Request{Metrics: []string{"order_revenue"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Query(ctx, id, plan.Request{
		Metrics: []string{"order_revenue"}, Dimensions: []string{"customers.region"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Query(ctx, id, plan.Request{
		Metrics: []string{"order_revenue"},
		Filters: []plan.Filter{{
			Dimension: "orders.status", Op: plan.OpEq, Values: []any{"shipped"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if got := ex.calls.Load(); got != 3 {
		t.Errorf("%d warehouse calls for 3 different questions, want 3", got)
	}
}

// TestADifferentFilterValueIsNotAHit.
//
// The parameters are bound, so two queries differing only in a value have
// identical SQL. A key built from the statement alone would serve the
// wrong region's revenue, which is the worst kind of cache bug because
// both numbers look right.
func TestADifferentFilterValueIsNotAHit(t *testing.T) {
	ex := &countingExecutor{}
	eng := cachingEngine(t, ex, time.Minute)
	ctx := context.Background()
	id := govern.Identity{Subject: "analyst@example.com"}

	for _, region := range []string{"NE", "MW"} {
		_, err := eng.Query(ctx, id, plan.Request{
			Metrics: []string{"order_revenue"},
			Filters: []plan.Filter{{
				Dimension: "customers.region", Op: plan.OpEq, Values: []any{region},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := ex.calls.Load(); got != 2 {
		t.Errorf("%d warehouse calls; two different filter values shared a cache "+
			"entry, so one region's revenue was reported as the other's", got)
	}
}

// TestNoCacheMeansNoCache, so enabling it stays a deliberate choice.
func TestNoCacheMeansNoCache(t *testing.T) {
	ex := &countingExecutor{}
	eng := cachingEngine(t, ex, 0)

	for range 3 {
		if _, err := askAs(eng, "analyst@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if got := ex.calls.Load(); got != 3 {
		t.Errorf("%d warehouse calls with no cache configured, want 3", got)
	}
}

// TestACacheWithNoTTLIsRefused.
//
// Not a default, not infinity. A cache with no expiry serves an answer
// from an arbitrarily old warehouse state and nothing would ever say so.
func TestACacheWithNoTTLIsRefused(t *testing.T) {
	if _, err := engine.NewCache(engine.CacheOptions{}); err == nil {
		t.Fatal("a cache with no TTL was accepted")
	}
}

// TestHealthReportsTheCache.
//
// A cached answer is a correct answer from a moment ago, which is a
// different thing, and somebody reconciling a dashboard against the
// warehouse needs to know the difference exists.
func TestHealthReportsTheCache(t *testing.T) {
	eng := cachingEngine(t, &countingExecutor{}, 90*time.Second)

	h := eng.Health()
	if h.CacheTTLSeconds != 90 {
		t.Errorf("CacheTTLSeconds = %d, want 90", h.CacheTTLSeconds)
	}
	var noted bool
	for _, n := range h.EnforcementNotes {
		if strings.Contains(n, "cached") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("health does not mention the cache: %v", h.EnforcementNotes)
	}

	// And an engine without one must not claim staleness it does not have.
	plainHealth := cachingEngine(t, &countingExecutor{}, 0).Health()
	if plainHealth.CacheTTLSeconds != 0 {
		t.Errorf("an uncached engine reports a TTL of %d", plainHealth.CacheTTLSeconds)
	}
	for _, n := range plainHealth.EnforcementNotes {
		if strings.Contains(n, "cached") {
			t.Errorf("an uncached engine warns about caching: %q", n)
		}
	}
}

// TestACachedAnswerIsStillAudited.
//
// An operator reconciling a number against warehouse billing needs the
// record to cover every answer, including the ones that never reached the
// warehouse. A cache that quietly stopped auditing would make the audit
// trail undercount exactly as usage grew.
func TestACachedAnswerIsStillAudited(t *testing.T) {
	recent := govern.NewRecentAudit(16)
	cache, err := engine.NewCache(engine.CacheOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "testdata", "models", "retail.yaml"),
		Dialect:   "duckdb", Resolver: govern.AllowAll{},
		Executor: &countingExecutor{}, Cache: cache, Audit: recent,
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, err := askAs(eng, "analyst@example.com"); err != nil {
			t.Fatal(err)
		}
	}

	allowed := 0
	for _, e := range recent.Recent(0) {
		if e.Decision == "allowed" {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("%d allowed decisions recorded for 3 answers; cached answers "+
			"are missing from the audit record", allowed)
	}
}
