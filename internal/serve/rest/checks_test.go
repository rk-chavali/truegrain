package rest_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Asking the running model about itself.
//
// Written as the attack where there is one to write: these routes report what
// a deployment is serving, and two of them are one design decision away from
// publishing something they should not. Tests exist for the capability being
// absent, for a scope that must not widen itself, and for the one endpoint
// that could have become an access oracle and deliberately did not.

// TestTestsAreNotServedUntilNamed. An engine nobody pointed at a suite has no
// tests, which is not the same as having tests that all pass, and a pipeline
// that believed otherwise would report a clean run over no files.
func TestTestsAreNotServedUntilNamed(t *testing.T) {
	h := server(t, false, govern.Identity{Subject: "analyst@acme.com"}).Handler()
	res := do(t, h, "POST", "/v1/tests", "", token)
	if res.Code != http.StatusNotFound {
		t.Errorf("want 404 when no suite is configured, got %d: %s", res.Code, res.Body)
	}
}

// writeSuite puts one refusal assertion and one row assertion on disk.
func writeSuite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	body := `version: 1
tests:
  - name: the fan-out is still refused
    metrics: [retail.order_revenue]
    dimensions: [retail.order_lines.item_id]
    expect_refusal: fan_out_would_inflate
  - name: revenue is still 885.50
    metrics: [retail.order_revenue]
    expect_rows:
      - ["885.50"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestARefusalAssertionNeedsNoWarehouse is the property that makes this worth
// serving: a credential that cannot read a single row can still prove the
// model declines a fan-out.
func TestARefusalAssertionNeedsNoWarehouse(t *testing.T) {
	h := server(t, false, govern.Identity{
		Subject: "ci@acme.com",
		Scopes:  []string{"read:model"},
	}).WithModelTests(writeSuite(t)).Handler()

	res := do(t, h, "POST", "/v1/tests", "", token)
	if res.Code != http.StatusOK {
		t.Fatalf("want 200 for a read:model caller, got %d: %s", res.Code, res.Body)
	}
	body := decode(t, res.Body.Bytes())

	if body["passed"].(float64) < 1 {
		t.Errorf("the refusal assertion did not run: %s", res.Body)
	}
	// The row assertion must not have run, and must be reported rather than
	// quietly dropped. A suite that checked half of what it claimed and said
	// nothing is the failure this counts against.
	if withheld := body["withheld"].(float64); withheld != 1 {
		t.Errorf("want 1 case withheld from a read:model caller, got %v: %s", withheld, res.Body)
	}
}

// TestAReadModelCredentialCannotRunRowAssertions. The scope decides, not the
// suite: a case that would reach the warehouse must not be run by a credential
// that may not reach the warehouse, however the file is written.
func TestAReadModelCredentialCannotRunRowAssertions(t *testing.T) {
	h := server(t, false, govern.Identity{
		Subject: "ci@acme.com",
		Scopes:  []string{"read:model"},
	}).WithModelTests(writeSuite(t)).Handler()

	res := do(t, h, "POST", "/v1/tests", "", token)
	body := decode(t, res.Body.Bytes())

	// Nothing failed, because nothing that needed a warehouse was attempted.
	if failed := body["failed"].(float64); failed != 0 {
		t.Errorf("a withheld case was run and failed: %s", res.Body)
	}
	if ok := body["ok"].(bool); !ok {
		t.Errorf("want ok for a run with nothing failing: %s", res.Body)
	}
}

// TestPolicyReportsWhatIsNotEnforced. An engine running allow-all has to say
// so, because the alternative is a reader assuming a gate exists on the
// strength of the product having one.
func TestPolicyReportsWhatIsNotEnforced(t *testing.T) {
	h := server(t, false, govern.Identity{Subject: "analyst@acme.com"}).Handler()
	res := do(t, h, "GET", "/v1/policy", "", token)
	if res.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", res.Code, res.Body)
	}

	gov, ok := decode(t, res.Body.Bytes())["governance"].(map[string]any)
	if !ok {
		t.Fatalf("no governance in the response: %s", res.Body)
	}
	if gov["column_level"].(bool) {
		t.Errorf("allow-all claimed column level enforcement: %s", res.Body)
	}
	if gov["resolver"].(string) != "allow-all" {
		t.Errorf("want the resolver named, got %v", gov["resolver"])
	}
}

// TestExplainAnswersForTheCallerAndNobodyElse.
//
// The endpoint takes a metric and never an identity, and this pins that. An
// explain that accepted a subject would let any caller enumerate another
// identity's access, which publishes the policy the engine was configured to
// enforce. The request below asks about somebody else in the only way the
// shape allows, and must be answered about the caller.
func TestExplainAnswersForTheCallerAndNobodyElse(t *testing.T) {
	h := server(t, true, govern.Identity{Subject: "analyst@acme.com"}).Handler()

	res := do(t, h, "POST", "/v1/policy/explain",
		`{"metric":"retail.order_revenue","identity":"ceo@acme.com","subject":"ceo@acme.com"}`,
		token)
	if res.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", res.Code, res.Body)
	}

	body := decode(t, res.Body.Bytes())
	if got := body["identity"].(string); got != "analyst@acme.com" {
		t.Fatalf("explain answered for %q, not the caller; it is an access oracle", got)
	}
}

// TestExplainNamesNoMetricItDoesNotHave. 404 rather than an empty readable
// list: a metric that does not exist and a metric with nothing readable are
// different answers, and only one of them means ask an administrator.
func TestExplainNamesNoMetricItDoesNotHave(t *testing.T) {
	h := server(t, false, govern.Identity{Subject: "analyst@acme.com"}).Handler()
	res := do(t, h, "POST", "/v1/policy/explain", `{"metric":"retail.not_a_metric"}`, token)
	if res.Code != http.StatusNotFound {
		t.Errorf("want 404 for an unknown metric, got %d: %s", res.Code, res.Body)
	}
}

// TestDiffSaysItHasNothingToCompare. Having served one model is not the same
// as nothing having changed, and only one of those is reassuring.
func TestDiffSaysItHasNothingToCompare(t *testing.T) {
	h := server(t, false, govern.Identity{Subject: "analyst@acme.com"}).Handler()
	res := do(t, h, "GET", "/v1/diff", "", token)
	if res.Code != http.StatusNotFound {
		t.Fatalf("want 404 before any reload, got %d: %s", res.Code, res.Body)
	}
	if code := decode(t, res.Body.Bytes())["error"]; code == nil {
		// The engine returns refusals flat rather than wrapped; either way
		// the body must name the reason rather than being empty.
		if decode(t, res.Body.Bytes())["code"] == nil {
			t.Errorf("no code in the refusal: %s", res.Body)
		}
	}
}

// TestDiffComparesTheModelAgainstTheOneBefore.
//
// The point of serving this at all: the comparison nobody can make from
// outside the process. Serving the same model twice must report no change,
// which is the case that would break if the snapshot were not deterministic.
func TestDiffComparesTheModelAgainstTheOneBefore(t *testing.T) {
	srv := server(t, false, govern.Identity{Subject: "analyst@acme.com"})

	// A reload to the same model. The digests match, so nothing moved.
	srv.Serve(engineFor(t))
	h := srv.Handler()

	res := do(t, h, "GET", "/v1/diff", "", token)
	if res.Code != http.StatusOK {
		t.Fatalf("want 200 after a reload, got %d: %s", res.Code, res.Body)
	}
	body := decode(t, res.Body.Bytes())
	if body["changed"].(bool) {
		t.Errorf("reloading the same model reported a change: %s", res.Body)
	}
}

// The scheduled warehouse check.
//
// Concurrency is the reason these exist: a goroutine writing history while a
// request reads it is a data race, and the race detector runs over this
// package in CI.

// TestHistoryIsAbsentUntilScheduled. An engine checking nothing has no
// history, which is not a history in which nothing went wrong.
func TestHistoryIsAbsentUntilScheduled(t *testing.T) {
	h := server(t, false, govern.Identity{Subject: "analyst@acme.com"}).Handler()
	res := do(t, h, "GET", "/v1/doctor/history", "", token)
	if res.Code != http.StatusNotFound {
		t.Errorf("want 404 with no schedule, got %d: %s", res.Code, res.Body)
	}
}

// TestTheFirstCheckRunsImmediately. Waiting a whole interval to discover the
// warehouse connection is wrong is the behaviour nobody asks for twice.
func TestTheFirstCheckRunsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// An interval long enough that only the immediate run can have fired.
	srv := server(t, false, govern.Identity{Subject: "analyst@acme.com"}).
		WithDoctorSchedule(ctx, time.Hour, nil)
	h := srv.Handler()

	deadline := time.Now().Add(5 * time.Second)
	for {
		res := do(t, h, "GET", "/v1/doctor/history", "", token)
		if res.Code != http.StatusOK {
			t.Fatalf("want 200 once scheduled, got %d: %s", res.Code, res.Body)
		}
		runs, _ := decode(t, res.Body.Bytes())["runs"].([]any)
		if len(runs) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the first check never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheScheduleStopsWithItsContext. A goroutine that outlives the server is
// a leak, and one holding a warehouse connection is a leak that costs money.
func TestTheScheduleStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := server(t, false, govern.Identity{Subject: "analyst@acme.com"}).
		WithDoctorSchedule(ctx, 10*time.Millisecond, nil)
	h := srv.Handler()

	time.Sleep(80 * time.Millisecond)
	cancel()
	time.Sleep(80 * time.Millisecond)

	first := len(runsOf(t, h))
	time.Sleep(120 * time.Millisecond)
	if after := len(runsOf(t, h)); after != first {
		t.Errorf("the schedule kept running after its context ended: %d then %d", first, after)
	}
}

func runsOf(t *testing.T, h http.Handler) []any {
	t.Helper()
	res := do(t, h, "GET", "/v1/doctor/history", "", token)
	runs, _ := decode(t, res.Body.Bytes())["runs"].([]any)
	return runs
}
