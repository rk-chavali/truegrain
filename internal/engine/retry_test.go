package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Retrying a warehouse that faltered, and not retrying one that refused.
//
// A flaky warehouse used to surface as a failed query, which an agent reads
// as "this question cannot be answered" and gives up on. The risk in fixing
// that is the opposite mistake: retrying a permission denial three times, or
// re-running a query that already cost money. Most of what is asserted here
// is that the second thing does not happen.

var errTransient = errors.New("connection reset by peer")
var errFinal = errors.New("permission denied on table orders")

// flaky fails a set number of times, then succeeds, and counts attempts.
type flaky struct {
	failures atomic.Int32
	attempts atomic.Int32
	err      error
	// classify is nil for an executor that cannot tell transient from final,
	// which is what an executor that does not implement the interface is.
	classify func(error) bool
}

func (f *flaky) Name() string { return "flaky (test)" }
func (f *flaky) Close() error { return nil }

func (f *flaky) Execute(context.Context, govern.Identity, string, []any) (*engine.Rows, error) {
	f.attempts.Add(1)
	if f.failures.Load() > 0 {
		f.failures.Add(-1)
		return nil, f.err
	}
	return &engine.Rows{Columns: []string{"order_revenue"}, Rows: [][]any{{885.50}}}, nil
}

// classifying implements TransientClassifier over a flaky executor.
type classifying struct{ *flaky }

func (c classifying) Transient(err error) bool { return c.flaky.classify(err) }

func engineOver(t *testing.T, ex engine.Executor) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join("..", "..", "testdata", "models", "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Executor:  ex,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func ask(eng *engine.Engine) (*engine.Result, error) {
	return eng.Query(context.Background(), govern.Identity{Subject: "tester"},
		plan.Request{Metrics: []string{"order_revenue"}})
}

// TestATransientFailureIsRetried.
//
// One dropped connection should not look like a refusal.
func TestATransientFailureIsRetried(t *testing.T) {
	f := &flaky{err: errTransient, classify: func(e error) bool { return errors.Is(e, errTransient) }}
	f.failures.Store(2)

	res, err := ask(engineOver(t, classifying{f}))
	if err != nil {
		t.Fatalf("a transient failure was not retried through: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("got %d rows", len(res.Rows))
	}
	if got := f.attempts.Load(); got != 3 {
		t.Errorf("%d attempts, want 3", got)
	}
}

// TestAFinalFailureIsNotRetried.
//
// The expensive mistake. A permission denial retried three times is three
// audit entries, three warehouse round trips and the same answer; on a
// metered warehouse it is also three bills.
func TestAFinalFailureIsNotRetried(t *testing.T) {
	f := &flaky{err: errFinal, classify: func(e error) bool { return errors.Is(e, errTransient) }}
	f.failures.Store(5)

	if _, err := ask(engineOver(t, classifying{f})); err == nil {
		t.Fatal("a final failure succeeded")
	}
	if got := f.attempts.Load(); got != 1 {
		t.Errorf("%d attempts on a final failure, want exactly 1", got)
	}
}

// TestAnExecutorThatCannotClassifyIsNotRetried.
//
// The conservative default. Guessing that an unclassifiable error was
// transient would retry every real failure, which is the behaviour this is
// careful to avoid.
func TestAnExecutorThatCannotClassifyIsNotRetried(t *testing.T) {
	f := &flaky{err: errTransient}
	f.failures.Store(5)

	if _, err := ask(engineOver(t, f)); err == nil {
		t.Fatal("the query succeeded")
	}
	if got := f.attempts.Load(); got != 1 {
		t.Errorf("%d attempts without a classifier, want exactly 1", got)
	}
}

// TestRetriesAreBounded, so a warehouse having an outage does not turn one
// request into an indefinite wait.
func TestRetriesAreBounded(t *testing.T) {
	f := &flaky{err: errTransient, classify: func(error) bool { return true }}
	f.failures.Store(100)

	if _, err := ask(engineOver(t, classifying{f})); err == nil {
		t.Fatal("the query succeeded against an executor that always fails")
	}
	if got := f.attempts.Load(); got != 3 {
		t.Errorf("%d attempts, want 3: retries must be bounded", got)
	}
}

// TestTheWarehousesOwnErrorSurvives.
//
// After the last attempt the caller sees the warehouse's message, not a
// wrapper saying it was retried. The original is the most informative thing
// anybody will read.
func TestTheWarehousesOwnErrorSurvives(t *testing.T) {
	f := &flaky{err: errTransient, classify: func(error) bool { return true }}
	f.failures.Store(100)

	_, err := ask(engineOver(t, classifying{f}))
	if err == nil {
		t.Fatal("the query succeeded")
	}
	if !errors.Is(err, errTransient) {
		t.Errorf("the warehouse's error was lost: %v", err)
	}
}

// TestACancelledCallerStopsTheRetries.
//
// The caller's deadline wins. Sleeping through a cancellation would turn a
// transient failure into a timeout and report a slow warehouse instead of a
// flaky one.
func TestACancelledCallerStopsTheRetries(t *testing.T) {
	f := &flaky{err: errTransient, classify: func(error) bool { return true }}
	f.failures.Store(100)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := engineOver(t, classifying{f}).Query(ctx,
		govern.Identity{Subject: "tester"},
		plan.Request{Metrics: []string{"order_revenue"}})
	if err == nil {
		t.Fatal("a cancelled query succeeded")
	}
	if got := f.attempts.Load(); got > 1 {
		t.Errorf("%d attempts after cancellation, want 1", got)
	}
}
