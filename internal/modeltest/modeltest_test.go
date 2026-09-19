package modeltest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/duckdbtest"
	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/exec"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/modeltest"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// A test framework that cannot fail is worse than none: it reports green over
// work it never checked, and everybody stops looking. Most of what is
// asserted here is that the framework fails when it should.

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func models(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "models")
}

// compileOnly is an engine with no executor, which is what a pull request
// has: it can plan and govern, and it cannot run anything.
func compileOnly(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath: models(t), Dialect: "duckdb", Resolver: govern.AllowAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func executing(t *testing.T) *engine.Engine {
	t.Helper()
	ex, err := exec.NewDuckDB(exec.DuckDBOptions{
		Database: duckdbtest.Seed(t, filepath.Join("..", "..")),
	})
	if err != nil {
		t.Fatalf("building a DuckDB executor: %v", err)
	}
	t.Cleanup(func() { _ = ex.Close() })

	eng, err := engine.New(engine.Config{
		ModelPath: models(t), Dialect: "duckdb",
		Resolver: govern.AllowAll{}, Executor: ex,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func runBody(t *testing.T, eng *engine.Engine, body string) []modeltest.Result {
	t.Helper()
	suites, err := modeltest.Load(write(t, body))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	return modeltest.Run(context.Background(), eng, suites,
		govern.Identity{Subject: "tester"})
}

// TestARefusalAssertionNeedsNoWarehouse.
//
// The property that makes this usable in CI. A pull request has no
// credentials, and the thing most worth asserting about a semantic model is
// what it declines.
func TestARefusalAssertionNeedsNoWarehouse(t *testing.T) {
	results := runBody(t, compileOnly(t), `
version: 1
tests:
  - name: the fan-out is refused
    metrics: [order_revenue]
    dimensions: [order_lines.item_id]
    expect_refusal: fan_out_would_inflate
`)

	if len(results) != 1 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].Skipped {
		t.Fatal("a refusal case was skipped for want of a warehouse; it needs none")
	}
	if !results[0].Passed {
		t.Errorf("the refusal was not asserted: %s", results[0].Reason)
	}
}

// TestARowAssertionSkipsWithoutAWarehouseRatherThanPassing.
//
// Silently passing would be the worst behaviour available: a green CI run
// over assertions that never executed.
func TestARowAssertionSkipsWithoutAWarehouseRatherThanPassing(t *testing.T) {
	results := runBody(t, compileOnly(t), `
version: 1
tests:
  - name: a row assertion
    metrics: [order_revenue]
    expect_rows:
      - [885.50]
`)

	if results[0].Passed {
		t.Fatal("a row assertion passed without ever running")
	}
	if !results[0].Skipped {
		t.Fatalf("expected a skip, got a failure: %s", results[0].Reason)
	}
	if !strings.Contains(results[0].Reason, "warehouse") {
		t.Errorf("the skip does not say why: %q", results[0].Reason)
	}
}

// TestAWrongNumberFails, which is the entire point.
func TestAWrongNumberFails(t *testing.T) {
	results := runBody(t, executing(t), `
version: 1
tests:
  - name: the inflated answer
    metrics: [order_revenue]
    expect_rows:
      - [2361.00]
`)

	if results[0].Passed {
		t.Fatal("2361.00 was accepted as the total; the framework asserts nothing")
	}
	// The real value has to be in the report, or somebody has to go and run
	// the query by hand to find out what they got.
	if !strings.Contains(results[0].Reason, "885.50") {
		t.Errorf("the failure does not show what came back: %s", results[0].Reason)
	}
}

// TestARefusalThatStoppedHappeningFails.
//
// The regression this catches is the dangerous one: a model change that
// makes the engine answer a question it used to decline. Nothing else in the
// toolchain notices that.
func TestARefusalThatStoppedHappeningFails(t *testing.T) {
	results := runBody(t, compileOnly(t), `
version: 1
tests:
  - name: a question that is not actually refused
    metrics: [order_revenue]
    dimensions: [customers.region]
    expect_refusal: fan_out_would_inflate
`)

	if results[0].Passed {
		t.Fatal("a refusal that never happened was reported as asserted")
	}
	if !strings.Contains(results[0].Reason, "compiled") {
		t.Errorf("the failure is unclear: %s", results[0].Reason)
	}
}

// TestTheWrongRefusalCodeFails, because "it was refused" is not the
// assertion; "it was refused for this reason" is. A question declined for
// lack of access instead of for a fan-out is a different bug.
func TestTheWrongRefusalCodeFails(t *testing.T) {
	results := runBody(t, compileOnly(t), `
version: 1
tests:
  - name: refused, but not for the reason claimed
    metrics: [order_revenue]
    dimensions: [order_lines.item_id]
    expect_refusal: access_denied
`)

	if results[0].Passed {
		t.Fatal("any refusal satisfied a test naming a specific one")
	}
	if !strings.Contains(results[0].Reason, "fan_out_would_inflate") {
		t.Errorf("the failure does not name the refusal that did happen: %s", results[0].Reason)
	}
}

// TestRowOrderIsIgnoredUnlessAskedFor.
//
// A warehouse may return rows in any order without ORDER BY. Comparing
// positionally regardless would make a test pass on DuckDB and fail on
// BigQuery for a reason nobody can act on.
func TestRowOrderIsIgnoredUnlessAskedFor(t *testing.T) {
	// Written in the opposite order to what the warehouse returns.
	results := runBody(t, executing(t), `
version: 1
tests:
  - name: unordered comparison
    metrics: [order_revenue]
    dimensions: [customers.region]
    expect_rows:
      - [MW, 135.50]
      - [NE, 750.00]
`)

	if !results[0].Passed {
		t.Errorf("an unordered result was compared positionally: %s", results[0].Reason)
	}
}

// TestANumberMatchesWhateverShapeItArrivesIn.
//
// Executors disagree: the DuckDB CLI returns "750.00" as a string, pgx
// returns a typed value. A test that failed on the spelling would be a test
// about the driver rather than about the model.
func TestANumberMatchesWhateverShapeItArrivesIn(t *testing.T) {
	for _, written := range []string{"885.50", "885.5", "885.500"} {
		results := runBody(t, executing(t), `
version: 1
tests:
  - name: spelling
    metrics: [order_revenue]
    expect_rows:
      - [`+written+`]
`)
		if !results[0].Passed {
			t.Errorf("%s did not match: %s", written, results[0].Reason)
		}
	}
}

// TestToleranceAppliesOnlyWhenAskedFor.
//
// Sums and counts should be exact. Division is the case that differs across
// warehouses, and the tolerance exists so that a test can say so instead of
// somebody rounding in the model to make one warehouse agree with another.
func TestToleranceAppliesOnlyWhenAskedFor(t *testing.T) {
	strict := runBody(t, executing(t), `
version: 1
tests:
  - name: no tolerance
    metrics: [order_revenue]
    expect_rows:
      - [885.51]
`)
	if strict[0].Passed {
		t.Error("a penny of difference passed without a tolerance")
	}

	loose := runBody(t, executing(t), `
version: 1
tests:
  - name: with tolerance
    metrics: [order_revenue]
    tolerance: 0.01
    expect_rows:
      - [885.51]
`)
	if !loose[0].Passed {
		t.Errorf("a penny of difference failed within a tolerance of 0.01: %s", loose[0].Reason)
	}
}

// TestATestThatAssertsNothingIsRejected.
//
// A case with no assertion runs a query and passes whatever happens, which
// is a test that reports green forever. Refusing it at load is the only
// point at which anybody will notice.
func TestATestThatAssertsNothingIsRejected(t *testing.T) {
	_, err := modeltest.Load(write(t, `
version: 1
tests:
  - name: asserts nothing
    metrics: [order_revenue]
`))
	if err == nil {
		t.Fatal("a case with no assertion was accepted")
	}
	if !strings.Contains(err.Error(), "asserts nothing") {
		t.Errorf("the error is unclear: %v", err)
	}
}

// TestAMisspelledFieldIsRejected.
//
// expect_row where expect_rows was meant would assert nothing and pass. A
// silently ignored key in a test file is the quietest way to have no tests.
func TestAMisspelledFieldIsRejected(t *testing.T) {
	_, err := modeltest.Load(write(t, `
version: 1
tests:
  - name: typo
    metrics: [order_revenue]
    expect_row:
      - [885.50]
`))
	if err == nil {
		t.Fatal("a misspelled key was ignored rather than reported")
	}
}

// TestARefusalCannotBeCombinedWithARowAssertion, because a refused question
// returns no rows and the author has contradicted themselves.
func TestARefusalCannotBeCombinedWithARowAssertion(t *testing.T) {
	_, err := modeltest.Load(write(t, `
version: 1
tests:
  - name: both
    metrics: [order_revenue]
    expect_refusal: fan_out_would_inflate
    expect_rows:
      - [885.50]
`))
	if err == nil {
		t.Fatal("contradictory assertions were accepted")
	}
}

// TestAnUnexpectedRefusalNamesTheCodeToAssert.
//
// Somebody writing a row assertion against a question the model declines
// needs to be told which code to write, not left to find it.
func TestAnUnexpectedRefusalNamesTheCodeToAssert(t *testing.T) {
	results := runBody(t, executing(t), `
version: 1
tests:
  - name: a row assertion on a refused question
    metrics: [order_revenue]
    dimensions: [order_lines.item_id]
    expect_rows:
      - [2361.00]
`)

	if results[0].Passed {
		t.Fatal("a refused question satisfied a row assertion")
	}
	if !strings.Contains(results[0].Reason, "expect_refusal: fan_out_would_inflate") {
		t.Errorf("the failure does not say how to assert the refusal: %s", results[0].Reason)
	}
}

// TestTheShippedSuitePasses, so the worked example in testdata cannot rot.
func TestTheShippedSuitePasses(t *testing.T) {
	suites, err := modeltest.Load(filepath.Join("..", "..", "testdata", "tests"))
	if err != nil {
		t.Fatal(err)
	}
	results := modeltest.Run(context.Background(), executing(t), suites,
		govern.Identity{Subject: "tester"})

	s := modeltest.Summarize(results)
	if s.Failed > 0 || s.Skipped > 0 {
		for _, r := range results {
			if !r.Passed {
				t.Errorf("%s: %s", r.Case.Name, r.Reason)
			}
		}
	}
	if s.Passed == 0 {
		t.Fatal("the shipped suite asserted nothing")
	}
}

// TestRowPolicyCanBeAsserted.
//
// The governance rule most in need of a test, because a row policy that
// stops applying is invisible: the query still succeeds and the number is
// just bigger than it should be. Nothing else in the toolchain notices,
// and the caller it leaks to is the last person who will report it.
//
// Kept in its own directory because it needs a policy file, and a suite
// that silently passes without one would be worse than no suite.
func TestRowPolicyCanBeAsserted(t *testing.T) {
	ex, err := exec.NewDuckDB(exec.DuckDBOptions{
		Database: duckdbtest.Seed(t, filepath.Join("..", "..")),
	})
	if err != nil {
		t.Fatalf("building a DuckDB executor: %v", err)
	}
	t.Cleanup(func() { _ = ex.Close() })

	ws, err := workspace.Load(models(t), workspace.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := govern.LoadFilePolicy(
		filepath.Join("..", "..", "testdata", "policy.yaml"), ws)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{
		Dialect: "duckdb", Resolver: policy, Executor: ex,
	})
	if err != nil {
		t.Fatal(err)
	}

	suites, err := modeltest.Load(filepath.Join("..", "..", "testdata", "tests-policy"))
	if err != nil {
		t.Fatal(err)
	}
	results := modeltest.Run(context.Background(), eng, suites,
		govern.Identity{Subject: "analyst@example.com"})

	s := modeltest.Summarize(results)
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", r.Case.Name, r.Reason)
		}
	}
	if s.Passed == 0 {
		t.Fatal("the row-policy suite asserted nothing")
	}

	// And the suite has to be capable of failing: without the policy every
	// caller would see the whole total, so a suite that passed either way
	// would be testing nothing.
	open, err := engine.NewFromWorkspace(ws, engine.Config{
		Dialect: "duckdb", Resolver: govern.AllowAll{}, Executor: ex,
	})
	if err != nil {
		t.Fatal(err)
	}
	ungoverned := modeltest.Summarize(modeltest.Run(context.Background(), open, suites,
		govern.Identity{Subject: "analyst@example.com"}))
	if ungoverned.Failed == 0 {
		t.Error("the row-policy suite passes with no policy applied, so it is " +
			"asserting nothing about row policy")
	}
}
