// Package modeltest lets a model author assert what their metrics answer.
//
// The product's whole claim is that a number is correct. Until now nothing
// let the person who wrote the model say what correct is: `validate` checks
// the model holds together, `diff` reports that a number changed, and
// neither says whether it was ever right. This is the missing half, and it
// is the first thing an adopter asks for.
//
// Two kinds of assertion, and the second is the one no other semantic layer
// has.
//
//	expect_rows      running this question returns exactly these rows
//	expect_refusal   this question is declined, for this reason
//
// A refusal assertion needs no warehouse. The planner declines a fan-out
// before any SQL exists, so the check runs on a laptop with no credentials
// and in CI with none either, which is the same property that makes
// `validate` and `diff` usable in a pull request. That matters more than it
// sounds: the thing most worth testing about a semantic model is what it
// refuses, and testing it costs nothing.
package modeltest

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Suite is one test file.
type Suite struct {
	Version int    `yaml:"version"`
	Tests   []Case `yaml:"tests"`

	// Path is where it was read from, for diagnostics.
	Path string `yaml:"-"`
}

// Case is one assertion about the model.
type Case struct {
	Name string `yaml:"name"`

	// The question, in the same vocabulary a query uses. Deliberately the
	// request shape rather than SQL: a test written in SQL would be testing
	// the emitter, and would have to be rewritten for every dialect.
	Metrics    []string      `yaml:"metrics"`
	Dimensions []string      `yaml:"dimensions,omitempty"`
	Filters    []plan.Filter `yaml:"filters,omitempty"`
	Grain      plan.Grain    `yaml:"grain,omitempty"`
	OrderBy    []plan.Order  `yaml:"order_by,omitempty"`
	Limit      int           `yaml:"limit,omitempty"`

	// Identity runs the case as a named caller, so a governance rule can be
	// tested rather than assumed. Empty runs as the suite's default caller.
	Identity string   `yaml:"identity,omitempty"`
	Groups   []string `yaml:"groups,omitempty"`

	// ExpectRows is the whole result, in order when OrderBy is set and in any
	// order when it is not.
	ExpectRows [][]any `yaml:"expect_rows,omitempty"`
	// ExpectRowCount asserts only how many rows came back, for a case where
	// the values are large or volatile but the shape is the point.
	ExpectRowCount *int `yaml:"expect_row_count,omitempty"`
	// ExpectRefusal is the refusal code this question must be declined with.
	//
	// The code rather than the message: a message is prose and will be
	// improved, and a test that breaks when a sentence is reworded teaches
	// people to stop writing tests.
	ExpectRefusal string `yaml:"expect_refusal,omitempty"`

	// Tolerance allows a numeric comparison to differ by this much.
	//
	// Zero means exact, which is what sums and counts should be. It exists
	// because division does not agree across warehouses: Postgres and
	// BigQuery divide NUMERIC into NUMERIC and keep scale, DuckDB divides
	// into float64. A test on an average that must run on all three needs
	// somewhere to say so, and the alternative is people rounding in the
	// model to make tests pass.
	Tolerance float64 `yaml:"tolerance,omitempty"`
}

// needsWarehouse reports whether running this case requires an executor.
// NeedsWarehouse reports whether running this case executes a query.
//
// Exported because a caller holding only read:model may run the refusal
// assertions and must not run the others, and deciding that outside this
// package would mean a second opinion about which is which.
func (c Case) NeedsWarehouse() bool {
	return c.ExpectRows != nil || c.ExpectRowCount != nil
}

func (c Case) request() plan.Request {
	return plan.Request{
		Metrics:    c.Metrics,
		Dimensions: c.Dimensions,
		Filters:    c.Filters,
		Grain:      c.Grain,
		OrderBy:    c.OrderBy,
		Limit:      c.Limit,
	}
}

// Result is what happened to one case.
type Result struct {
	Case     Case
	Passed   bool
	Skipped  bool
	Duration time.Duration
	// Reason explains a failure or a skip, in one or more lines.
	Reason string
}

// Load reads a suite, or every suite in a directory.
func Load(path string) ([]Suite, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if !info.IsDir() {
		s, err := loadFile(path)
		if err != nil {
			return nil, err
		}
		return []Suite{s}, nil
	}

	var out []Suite
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		s, err := loadFile(filepath.Join(path, name))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no test files in %s; they are .yaml files with a `tests:` list", path)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func loadFile(path string) (Suite, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var s Suite
	// Unknown fields are an error rather than a shrug. A test file with
	// `expect_row` where `expect_rows` was meant would otherwise assert
	// nothing and pass, which is the worst outcome a test framework can have.
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	s.Path = path
	if s.Version != 1 {
		return Suite{}, fmt.Errorf("%s: unsupported version %d, expected 1", path, s.Version)
	}
	for i, c := range s.Tests {
		if err := c.validate(); err != nil {
			return Suite{}, fmt.Errorf("%s: test %d (%s): %w", path, i+1, c.Name, err)
		}
	}
	return s, nil
}

func (c Case) validate() error {
	if c.Name == "" {
		return errors.New("every test needs a name, which is what a failure report names")
	}
	if len(c.Metrics) == 0 {
		return errors.New("a test asks a question, so it needs at least one metric")
	}
	asserts := 0
	if c.ExpectRows != nil {
		asserts++
	}
	if c.ExpectRowCount != nil {
		asserts++
	}
	if c.ExpectRefusal != "" {
		asserts++
	}
	switch {
	case asserts == 0:
		return errors.New(
			"this test asserts nothing. Give it expect_rows, expect_row_count " +
				"or expect_refusal; a case that only runs a query passes whatever happens")
	case asserts > 1 && c.ExpectRefusal != "":
		return errors.New(
			"a refused question returns no rows, so expect_refusal cannot be " +
				"combined with a row assertion")
	}
	if c.Tolerance < 0 {
		return errors.New("tolerance cannot be negative")
	}
	return nil
}

// Run executes every case in order and returns one result each.
//
// Cases run sequentially rather than in parallel. They hit a warehouse, the
// engine's concurrency cap may be set, and a test suite that saturates the
// warehouse it is testing against is a bad neighbour. Suites are small.
func Run(ctx context.Context, eng *engine.Engine, suites []Suite, caller govern.Identity) []Result {
	var out []Result
	for _, s := range suites {
		for _, c := range s.Tests {
			out = append(out, run(ctx, eng, c, caller))
		}
	}
	return out
}

func run(ctx context.Context, eng *engine.Engine, c Case, caller govern.Identity) Result {
	started := time.Now()
	r := Result{Case: c}

	id := caller
	if c.Identity != "" {
		id = govern.Identity{Subject: c.Identity, Groups: c.Groups}
	} else if len(c.Groups) > 0 {
		id.Groups = c.Groups
	}

	// A refusal assertion never executes, so it needs no warehouse. Compile
	// is where the planner and the governance gate both decide, which is
	// exactly what the assertion is about.
	if c.ExpectRefusal != "" {
		_, err := eng.Compile(ctx, id, c.request())
		r.Duration = time.Since(started)
		return checkRefusal(r, c, err)
	}

	if !eng.CanExecute() {
		r.Duration = time.Since(started)
		r.Skipped = true
		r.Reason = "needs a warehouse: this engine can compile but not execute"
		return r
	}

	res, err := eng.Query(ctx, id, c.request())
	r.Duration = time.Since(started)
	if err != nil {
		var refusal *plan.Refusal
		if errors.As(err, &refusal) {
			r.Reason = fmt.Sprintf(
				"the question was refused (%s): %s\n    If that is correct, assert it "+
					"with expect_refusal: %s", refusal.Code, refusal.Reason, refusal.Code)
			return r
		}
		r.Reason = "the query failed: " + err.Error()
		return r
	}

	if c.ExpectRowCount != nil && len(res.Rows) != *c.ExpectRowCount {
		r.Reason = fmt.Sprintf("expected %d row(s), got %d", *c.ExpectRowCount, len(res.Rows))
		return r
	}
	if c.ExpectRows != nil {
		if reason := compareRows(c, res); reason != "" {
			r.Reason = reason
			return r
		}
	}
	r.Passed = true
	return r
}

func checkRefusal(r Result, c Case, err error) Result {
	var refusal *plan.Refusal
	switch {
	case err == nil:
		r.Reason = fmt.Sprintf(
			"expected a refusal (%s) but the question compiled. A model that stops "+
				"refusing something is the change this test exists to catch",
			c.ExpectRefusal)
	case !errors.As(err, &refusal):
		r.Reason = fmt.Sprintf("expected a refusal (%s) but got an error: %v",
			c.ExpectRefusal, err)
	case refusal.Code != c.ExpectRefusal:
		r.Reason = fmt.Sprintf("expected refusal %s, got %s: %s",
			c.ExpectRefusal, refusal.Code, refusal.Reason)
	default:
		r.Passed = true
	}
	return r
}

// compareRows checks the result against what the author wrote.
//
// Order matters only when the test asked for one. A warehouse is free to
// return rows in any order without ORDER BY, so comparing positionally
// without it produces a test that passes on DuckDB and fails on BigQuery for
// no reason anybody can act on.
func compareRows(c Case, res *engine.Result) string {
	if len(res.Rows) != len(c.ExpectRows) {
		return fmt.Sprintf("expected %d row(s), got %d\n%s",
			len(c.ExpectRows), len(res.Rows), render(res))
	}

	got := res.Rows
	want := c.ExpectRows
	if len(c.OrderBy) == 0 {
		got = sortedCopy(got)
		want = sortedCopy(want)
	}

	for i := range want {
		if len(want[i]) != len(got[i]) {
			return fmt.Sprintf("row %d has %d value(s), expected %d\n%s",
				i+1, len(got[i]), len(want[i]), render(res))
		}
		for j := range want[i] {
			if !equal(want[i][j], got[i][j], c.Tolerance) {
				return fmt.Sprintf(
					"row %d, column %s: expected %v, got %v\n%s",
					i+1, columnName(res, j), want[i][j], got[i][j], render(res))
			}
		}
	}
	return ""
}

func columnName(res *engine.Result, i int) string {
	if i < len(res.Columns) {
		return res.Columns[i]
	}
	return fmt.Sprintf("%d", i+1)
}

// equal compares one value, numerically when both sides are numbers.
//
// Numeric comparison matters because executors disagree about the Go type
// and the rendering: 750, "750", "750.00" and 750.0 are the same number, and
// a test that failed on the spelling would be a test about the driver.
func equal(want, got any, tolerance float64) bool {
	wantNum, wantOK := asRat(want)
	gotNum, gotOK := asRat(got)

	if wantOK && gotOK {
		if tolerance == 0 {
			return wantNum.Cmp(gotNum) == 0
		}
		diff := new(big.Rat).Sub(wantNum, gotNum)
		diff.Abs(diff)
		limit := new(big.Rat).SetFloat64(tolerance)
		if limit == nil {
			return false
		}
		return diff.Cmp(limit) <= 0
	}
	if want == nil || got == nil {
		return want == nil && got == nil
	}
	return fmt.Sprint(want) == fmt.Sprint(got)
}

// asRat reads a value as an exact rational, whatever shape it arrived in.
//
// big.Rat rather than float64 so that comparing money is exact: 0.1+0.2 as
// floats is not 0.3, and a test framework for a product about correct
// numbers cannot be the place that introduces a rounding error.
func asRat(v any) (*big.Rat, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case int:
		return new(big.Rat).SetInt64(int64(t)), true
	case int32:
		return new(big.Rat).SetInt64(int64(t)), true
	case int64:
		return new(big.Rat).SetInt64(t), true
	case float32:
		return new(big.Rat).SetFloat64(float64(t)), true
	case float64:
		r := new(big.Rat).SetFloat64(t)
		return r, r != nil
	case *big.Rat:
		return t, true
	case string:
		r, ok := new(big.Rat).SetString(strings.TrimSpace(t))
		return r, ok
	}
	return nil, false
}

// sortedCopy orders rows by their rendered form, so an unordered comparison
// is stable without requiring the author to write ORDER BY.
func sortedCopy(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool {
		return renderRow(out[i]) < renderRow(out[j])
	})
	return out
}

func renderRow(row []any) string {
	parts := make([]string, len(row))
	for i, v := range row {
		if v == nil {
			parts[i] = "<null>"
			continue
		}
		// Numbers render canonically so that 750 and "750.00" sort together,
		// which is what makes an unordered comparison match them up.
		if r, ok := asRat(v); ok {
			parts[i] = r.FloatString(6)
			continue
		}
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, "\x00")
}

// render shows what actually came back, because a failure that does not show
// the result sends somebody to run the query by hand.
func render(res *engine.Result) string {
	var b strings.Builder
	b.WriteString("    got:\n      ")
	b.WriteString(strings.Join(res.Columns, " | "))
	const maxRows = 20
	for i, row := range res.Rows {
		if i == maxRows {
			fmt.Fprintf(&b, "\n      ... and %d more", len(res.Rows)-maxRows)
			break
		}
		b.WriteString("\n      ")
		parts := make([]string, len(row))
		for j, v := range row {
			if v == nil {
				parts[j] = "<null>"
				continue
			}
			parts[j] = fmt.Sprint(v)
		}
		b.WriteString(strings.Join(parts, " | "))
	}
	return b.String()
}

// Summary counts a run.
type Summary struct {
	Passed  int
	Failed  int
	Skipped int
}

// Summarize counts results.
func Summarize(results []Result) Summary {
	var s Summary
	for _, r := range results {
		switch {
		case r.Skipped:
			s.Skipped++
		case r.Passed:
			s.Passed++
		default:
			s.Failed++
		}
	}
	return s
}

// NeedsWarehouse counts the cases in these suites that cannot run without an
// executor, so a run with none can say how much it did not check rather than
// reporting a clean pass.
func NeedsWarehouse(suites []Suite) int {
	n := 0
	for _, s := range suites {
		for _, c := range s.Tests {
			if c.NeedsWarehouse() {
				n++
			}
		}
	}
	return n
}
