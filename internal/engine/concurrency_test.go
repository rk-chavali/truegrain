package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Capping concurrent warehouse execution.
//
// The gap this closes: per-caller job slots bound one caller at four running
// jobs, so a hundred callers still put four hundred queries on the warehouse,
// and the synchronous path was bounded by nothing at all.
//
// The cap guards e.exec.Execute, which is the single point every surface
// reaches the warehouse through. These tests drive it through Query, which is
// the same path the REST handler, a job, an MCP tool call and the CLI all use.

// blockingExecutor holds every query until it is released, so a test can put
// a known number of queries in flight at once.
type blockingExecutor struct {
	release chan struct{}
	// inFlight is the high-water mark of simultaneous executions, which is the
	// number the cap is supposed to bound.
	inFlight atomic.Int32
	peak     atomic.Int32
	started  chan struct{}
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{
		release: make(chan struct{}),
		started: make(chan struct{}, 64),
	}
}

func (b *blockingExecutor) Name() string { return "blocking (test)" }
func (b *blockingExecutor) Close() error { return nil }

func (b *blockingExecutor) Execute(ctx context.Context, _ govern.Identity,
	_ string, _ []any) (*engine.Rows, error) {

	now := b.inFlight.Add(1)
	for {
		peak := b.peak.Load()
		if now <= peak || b.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	b.started <- struct{}{}
	defer b.inFlight.Add(-1)

	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &engine.Rows{Columns: []string{"order_revenue"}, Rows: [][]any{{1}}}, nil
}

func cappedEngine(t *testing.T, limit int, ex engine.Executor) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath:            filepath.Join("..", "..", "testdata", "models", "retail.yaml"),
		Dialect:              "duckdb",
		Resolver:             govern.AllowAll{},
		Executor:             ex,
		MaxConcurrentQueries: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func query(eng *engine.Engine) error {
	_, err := eng.Query(context.Background(), govern.Identity{Subject: "tester"},
		plan.Request{Metrics: []string{"order_revenue"}})
	return err
}

// TestTheCapBoundsWhatReachesTheWarehouse.
//
// The property, stated directly: with a cap of two, no more than two queries
// are ever inside the executor at once, however many callers arrive.
func TestTheCapBoundsWhatReachesTheWarehouse(t *testing.T) {
	ex := newBlockingExecutor()
	eng := cappedEngine(t, 2, ex)

	// Refusals are reported as they happen rather than counted at the end.
	// Releasing the slots first would let a straggler acquire a freed one and
	// succeed, which makes the count a race rather than a property.
	refusals := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := query(eng); err != nil {
				refusals <- err
			}
		}()
	}

	// Two win slots and block inside the executor.
	for range 2 {
		select {
		case <-ex.started:
		case <-time.After(5 * time.Second):
			t.Fatal("no query reached the executor")
		}
	}
	// The other six have nowhere to go. Waiting for all six here is the
	// assertion that they were refused promptly rather than queued: if any of
	// them were waiting for a slot, this would time out, because nothing
	// releases one until afterwards.
	for i := range 6 {
		select {
		case err := <-refusals:
			if !isRefusal(err, engine.CodeTooManyQueries) {
				t.Errorf("refusal %d was %v, want %s", i+1, err, engine.CodeTooManyQueries)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 6 excess queries were refused; the rest are queued", i)
		}
	}

	close(ex.release)
	wg.Wait()

	if peak := ex.peak.Load(); peak > 2 {
		t.Errorf("%d queries were in the warehouse at once, cap is 2", peak)
	}
}

// TestSaturationRefusesRatherThanQueues.
//
// Queuing would turn saturation into every caller timing out at once, and an
// agent cannot tell a slow answer from a stuck one. The refusal has to arrive
// promptly and carry RetryLater, which is what tells the agent to come back
// rather than to rewrite the question or give up.
func TestSaturationRefusesRatherThanQueues(t *testing.T) {
	ex := newBlockingExecutor()
	eng := cappedEngine(t, 1, ex)

	go func() { _ = query(eng) }()
	select {
	case <-ex.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first query never reached the executor")
	}

	start := time.Now()
	err := query(eng)
	elapsed := time.Since(start)
	close(ex.release)

	var refusal *plan.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("got %v, want a *plan.Refusal", err)
	}
	if refusal.Code != engine.CodeTooManyQueries {
		t.Errorf("Code = %q, want %q", refusal.Code, engine.CodeTooManyQueries)
	}
	if refusal.Retry() != plan.RetryLater {
		t.Errorf("Retry = %q, want later: backpressure is not a verdict on the request",
			refusal.Retry())
	}
	if elapsed > 2*time.Second {
		t.Errorf("the refusal took %v; it should not have waited for a slot", elapsed)
	}
	if refusal.Hint == "" {
		t.Error("no hint: a caller told only that it was refused cannot act")
	}
}

// TestASlotIsReturnedAfterEveryQuery.
//
// A leaked slot is the worst failure this could have: the engine would refuse
// everything forever, and only a restart would clear it. Both the success and
// the failure path have to give the slot back.
func TestASlotIsReturnedAfterEveryQuery(t *testing.T) {
	ex := newBlockingExecutor()
	close(ex.release) // never block: every query completes immediately
	eng := cappedEngine(t, 1, ex)

	for i := range 20 {
		if err := query(eng); err != nil {
			t.Fatalf("query %d was refused, so a slot was not returned: %v", i+1, err)
		}
	}
}

// TestAFailedQueryAlsoReturnsItsSlot, because the executor erroring is the
// path most likely to skip the release.
func TestAFailedQueryAlsoReturnsItsSlot(t *testing.T) {
	eng := cappedEngine(t, 1, failingExecutor{})

	for i := range 5 {
		err := query(eng)
		if err == nil {
			t.Fatalf("query %d succeeded against a failing executor", i+1)
		}
		if isRefusal(err, engine.CodeTooManyQueries) {
			t.Fatalf("query %d was refused for saturation, so the slot from query %d "+
				"leaked when the executor failed", i+1, i)
		}
		if !errors.Is(err, errExecutorDown) {
			t.Fatalf("query %d failed with %v, want the executor's own error", i+1, err)
		}
	}
}

// TestNoCapMeansNoCap keeps the default honest: an engine configured without a
// limit must not acquire anything, or upgrading would silently throttle a
// deployment that was fine.
func TestNoCapMeansNoCap(t *testing.T) {
	ex := newBlockingExecutor()
	eng := cappedEngine(t, 0, ex)

	if eng.QueryConcurrency() != 0 {
		t.Errorf("QueryConcurrency = %d, want 0 for an unconfigured cap",
			eng.QueryConcurrency())
	}

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = query(eng) }()
	}
	for range 6 {
		select {
		case <-ex.started:
		case <-time.After(5 * time.Second):
			t.Fatal("an uncapped engine refused or blocked a query")
		}
	}
	close(ex.release)
	wg.Wait()

	if peak := ex.peak.Load(); peak != 6 {
		t.Errorf("peak concurrency %d, want 6: nothing should have been held back", peak)
	}
}

// TestTheCapIsReported, so an operator is not left assuming one exists.
func TestTheCapIsReported(t *testing.T) {
	if got := cappedEngine(t, 12, newBlockingExecutor()).QueryConcurrency(); got != 12 {
		t.Errorf("QueryConcurrency = %d, want 12", got)
	}
}

var errExecutorDown = errors.New("the warehouse is down")

type failingExecutor struct{}

func (failingExecutor) Name() string { return "failing (test)" }
func (failingExecutor) Close() error { return nil }
func (failingExecutor) Execute(context.Context, govern.Identity, string, []any) (*engine.Rows, error) {
	return nil, errExecutorDown
}

func isRefusal(err error, code string) bool {
	var r *plan.Refusal
	return errors.As(err, &r) && r.Code == code
}
