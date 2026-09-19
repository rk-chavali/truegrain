package rest

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"github.com/rk-chavali/truegrain/internal/modeldiff"
	"github.com/rk-chavali/truegrain/internal/modeltest"
)

// Checking a model that is already running.
//
// The command line has answered these questions since the beginning, and a
// pipeline is the right place to ask most of them. What it could not do is
// answer them about the model a particular deployment is actually serving,
// which is the version anybody debugging a number cares about. `truegrain
// doctor` run on a laptop checks the laptop's warehouse credentials against
// the laptop's checkout; it says nothing about production.
//
// So these routes ask the running engine about itself. None of them take a
// model as input, and that is the point: the answer is about this deployment
// or it is not worth having.

// WithModelTests serves the assertions at this path.
//
// Off until a path is given, and 404 until then, the same as the audit log:
// an engine nobody pointed at a test suite has no tests, as opposed to having
// tests that all pass. Reporting a clean run over no files is the failure mode
// worth designing against, because it is the one a pipeline believes.
func (s *Server) WithModelTests(path string) *Server {
	s.testPath = path
	return s
}

// doctor asks the warehouse whether the model is still true.
//
// read:model rather than run:query, deliberately. It reads no rows: it lists
// schemas and compares them to what the model declares, and the extra thing it
// discloses over /v1/metrics is whether the tables a caller can already see
// named actually exist. A continuous integration credential that validates a
// model is exactly who should be able to run this, and that credential should
// not also be able to read the warehouse.
func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireScope(w, r, ScopeReadModel); !ok {
		return
	}
	report, err := s.engine().Doctor(r.Context())
	if err != nil {
		writeRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             report.OK(),
		"tables_checked": report.Checked,
		"findings":       report.Findings,
		"skipped":        report.Skipped,
	})
}

// runTests asserts what this model answers.
//
// Refusal cases need no warehouse, which is the property that makes this worth
// exposing: a caller holding only read:model can still prove the model still
// declines a fan-out. Cases that need rows are reported as skipped for such a
// caller rather than run or silently dropped, because a test run that quietly
// checked half of what it claimed is worse than one that failed.
func (s *Server) runTests(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeReadModel)
	if !ok {
		return
	}
	if s.testPath == "" {
		writeError(w, http.StatusNotFound, "tests_not_served",
			"this engine was not given a test suite",
			"start it with -tests naming a file or a directory of them")
		return
	}

	suites, err := modeltest.Load(s.testPath)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "tests_unreadable",
			"the test suite could not be loaded: "+err.Error(),
			"check the path and the file's shape")
		return
	}

	/*
		A caller without run:query may not cause warehouse execution, so
		the cases that would are removed before the run rather than being
		allowed to fail. They are counted as withheld and reported, which
		is the difference between a suite that passed and a suite that
		checked half of what it claimed.
	*/
	withheld := 0
	if !id.Can(ScopeRunQuery) {
		for i := range suites {
			kept := suites[i].Tests[:0]
			for _, c := range suites[i].Tests {
				if c.NeedsWarehouse() {
					withheld++
					continue
				}
				kept = append(kept, c)
			}
			suites[i].Tests = kept
		}
	}

	type caseJSON struct {
		Name     string `json:"name"`
		Passed   bool   `json:"passed"`
		Skipped  bool   `json:"skipped"`
		Reason   string `json:"reason,omitempty"`
		Duration int64  `json:"duration_ms"`
	}

	results := modeltest.Run(r.Context(), s.engine(), suites, id)
	out := make([]caseJSON, 0, len(results))
	passed, failed, skipped := 0, 0, 0
	for _, res := range results {
		switch {
		case res.Skipped:
			skipped++
		case res.Passed:
			passed++
		default:
			failed++
		}
		out = append(out, caseJSON{
			Name:     res.Case.Name,
			Passed:   res.Passed,
			Skipped:  res.Skipped,
			Reason:   res.Reason,
			Duration: res.Duration.Milliseconds(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       failed == 0,
		"passed":   passed,
		"failed":   failed,
		"skipped":  skipped,
		"withheld": withheld,
		"results":  out,
	})
}

// policy reports what this engine enforces, without naming who it enforces on.
//
// Capabilities already travel on /v1/health, which is unauthenticated; they
// are repeated here so a console has one place to ask about governance rather
// than reading an operator endpoint for a product screen.
func (s *Server) policy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireScope(w, r, ScopeReadModel); !ok {
		return
	}
	h := s.engine().Health()
	writeJSON(w, http.StatusOK, map[string]any{
		"governance":        h.Governance,
		"enforcement_notes": h.EnforcementNotes,
	})
}

// explainPolicy says what the caller may read, and only the caller.
//
// Deliberately not an oracle. It would be useful for an administrator to ask
// what somebody else can see, and that is a different endpoint with its own
// authorization: an engine where any caller can enumerate another identity's
// access has published the policy it was configured to enforce. This answers
// "why can I not see this column", which is the question people actually
// arrive with, and it needs no permission beyond being the person asking.
func (s *Server) explainPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeReadModel)
	if !ok {
		return
	}

	var body struct {
		Metric string `json:"metric"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Metric == "" {
		writeError(w, http.StatusBadRequest, "metric_required",
			"name the metric to explain", `for example {"metric": "retail.order_revenue"}`)
		return
	}

	eng := s.engine()
	vm, _, found := eng.FindMetric(body.Metric)
	if !found {
		writeError(w, http.StatusNotFound, "no_such_metric",
			"this model declares no metric by that name",
			"list them with GET /v1/metrics")
		return
	}

	visible, err := eng.VisibleDimensions(r.Context(), id, &vm)
	if err != nil {
		writeRefusal(w, err)
		return
	}
	readable := make([]string, 0, len(visible))
	for _, d := range visible {
		readable = append(readable, d.Qualified)
	}
	sort.Strings(readable)

	writeJSON(w, http.StatusOK, map[string]any{
		"metric":     vm.Qualified,
		"identity":   id.Subject,
		"readable":   readable,
		"governance": eng.Health().Governance,
	})
}

// diff reports whether the last model reload moved a number.
//
// The command compares two checkouts; this compares the model being served
// against the one served before it, which is the comparison nobody can make
// from outside the process. It is empty until a reload has happened, and says
// so rather than reporting that nothing changed, because those are different
// answers and only one of them is reassuring.
func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireScope(w, r, ScopeReadModel); !ok {
		return
	}
	previous := s.previous.Load()
	if previous == nil {
		writeError(w, http.StatusNotFound, "no_previous_model",
			"this engine has served only one model, so there is nothing to compare it to",
			"a comparison appears after the model reloads")
		return
	}

	current := s.engine()
	before, err := modeldiff.Take(r.Context(), previous)
	if err != nil {
		writeRefusal(w, err)
		return
	}
	after, err := modeldiff.Take(r.Context(), current)
	if err != nil {
		writeRefusal(w, err)
		return
	}

	report := modeldiff.Compare(before, after)
	writeJSON(w, http.StatusOK, map[string]any{
		"changed":       report.Changed(),
		"from":          previous.ModelVersion(),
		"to":            current.ModelVersion(),
		"added":         report.Added,
		"removed":       report.Removed,
		"altered":       report.Altered,
		"model_version": current.ModelVersion(),
	})
}

// decodeBody reads a small JSON body, which plan requests do not use.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "unreadable_body",
			"the request body is not the JSON this route expects", err.Error())
		return false
	}
	return true
}
