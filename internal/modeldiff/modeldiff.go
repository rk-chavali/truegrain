// Package modeldiff answers one question: does this change alter a number?
//
// docs/08-contributing.md calls an undocumented change to a compiled result a
// silent correctness incident, which is exactly right: the model still
// validates, the tests still pass, and every dashboard quietly moves.
// Reviewing a YAML diff does not catch it, because the YAML change is usually
// small and the consequence is not.
//
// So the comparison is made on compiled SQL rather than on model text. Two
// models are each compiled across the same deterministic matrix of requests
// and the results compared. Renaming a description is invisible here; changing
// a join, a grain or an expression is not.
//
// This lives in its own package rather than in the diff command because two
// callers now ask the question and they must not be able to disagree: the
// command compares a working tree against a branch, and the server compares
// the model it is serving against the one it served before a reload. Two
// definitions of "a number moved" would be worse than one imperfect one.
package modeldiff

import (
	"context"
	"errors"
	"sort"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// DimensionsPerMetric bounds the matrix. Every metric is compiled alone and
// then grouped by a few of its dimensions, which is enough to catch a changed
// join path without compiling a combinatorial explosion on a large model.
const DimensionsPerMetric = 3

// Snapshot maps a request label to what that request compiles to, which is
// either SQL or the refusal it earns.
type Snapshot map[string]string

// diffIdentity compiles the matrix.
//
// It is granted everything, deliberately. The question is whether the model
// changed, and running the matrix as a restricted caller would report a column
// policy change as a compiled-output change, which is a different question and
// a misleading answer to this one.
var diffIdentity = govern.Identity{Subject: "truegrain-diff"}

// Take compiles the matrix against one engine.
//
// The engine is passed in rather than built here so that a server can take a
// snapshot of a model it is already serving without loading it a second time.
func Take(ctx context.Context, eng *engine.Engine) (Snapshot, error) {
	out := Snapshot{}

	for _, vm := range eng.Metrics() {
		requests := map[string]plan.Request{
			vm.Qualified: {Metrics: []string{vm.Qualified}},
		}

		dims, err := eng.VisibleDimensions(ctx, diffIdentity, &vm)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(dims))
		for _, d := range dims {
			names = append(names, d.Qualified)
		}
		// Sorted and truncated, so the matrix is the same on both sides for
		// the same model rather than depending on declaration order.
		sort.Strings(names)
		if len(names) > DimensionsPerMetric {
			names = names[:DimensionsPerMetric]
		}
		for _, dim := range names {
			requests[vm.Qualified+" by "+dim] = plan.Request{
				Metrics: []string{vm.Qualified}, Dimensions: []string{dim},
			}
		}

		for label, req := range requests {
			compiled, err := eng.Compile(ctx, diffIdentity, req)
			if err != nil {
				// A refusal is part of what the model means. A request that
				// used to be refused and now compiles is a real change, and
				// so is the reverse.
				out[label] = "REFUSED: " + refusalCode(err)
				continue
			}
			out[label] = compiled.SQL
		}
	}
	return out, nil
}

func refusalCode(err error) string {
	var r *plan.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return "error: " + err.Error()
}

// Report is what changed between two snapshots.
type Report struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	// Altered maps a request label to what it compiled to before and after.
	Altered map[string]Change `json:"altered"`
}

// Change is one request's before and after.
type Change struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// Compare reports how current differs from baseline.
func Compare(baseline, current Snapshot) Report {
	out := Report{Added: []string{}, Removed: []string{}, Altered: map[string]Change{}}

	for label, sql := range current {
		before, existed := baseline[label]
		switch {
		case !existed:
			out.Added = append(out.Added, label)
		case before != sql:
			out.Altered[label] = Change{Before: before, After: sql}
		}
	}
	for label := range baseline {
		if _, still := current[label]; !still {
			out.Removed = append(out.Removed, label)
		}
	}
	sort.Strings(out.Added)
	sort.Strings(out.Removed)
	return out
}

// Changed reports whether anything moved.
func (r Report) Changed() bool {
	return len(r.Added) > 0 || len(r.Removed) > 0 || len(r.Altered) > 0
}
