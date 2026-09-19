package engine_test

import (
	"context"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// A refusal is a decision this engine made about somebody's question, and
// until now it left no trace at all.
//
// The gate audits denials, which are about access. A refusal happens in
// Compile, before the gate ever runs, so nothing recorded the most distinctive
// thing this engine does: declining to answer a question it cannot answer
// correctly. An operator asked "why did that agent give up" and the audit file
// had nothing to say.

func TestARefusalIsRecorded(t *testing.T) {
	audit := &govern.MemoryAudit{}
	eng, err := engine.New(engine.Config{
		ModelPath: "../../testdata/workspace",
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Audit:     audit,
	})
	if err != nil {
		t.Fatal(err)
	}

	// order_revenue is at order grain; item_id is a line-level dimension, so
	// joining them repeats every order and inflates the total.
	_, err = eng.Query(context.Background(), govern.Identity{Subject: "analyst@acme.com"},
		plan.Request{
			Metrics:    []string{"sales.order_revenue"},
			Dimensions: []string{"sales.order_lines.item_id"},
		})
	if err == nil {
		t.Fatal("the fan-out should have been refused")
	}

	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("want exactly one recorded decision, got %d: %+v", len(events), events)
	}
	e := events[0]

	if e.Decision != "refused" {
		t.Errorf("want decision refused, got %q", e.Decision)
	}
	// refused and denied must stay distinct. A governance report that counts
	// every fan-out as an access incident is a report nobody trusts.
	if e.Decision == "denied" {
		t.Error("a correctness refusal was recorded as an access denial")
	}
	if e.Identity != "analyst@acme.com" {
		t.Errorf("the record must name who asked, got %q", e.Identity)
	}
	if e.RefusalCode == "" {
		t.Error("the record must carry the refusal code")
	}
	if e.Retry != string(plan.RetryModify) {
		t.Errorf("a fan-out is answerable a different way, so retry should be modify, got %q", e.Retry)
	}
	if e.Hint == "" {
		t.Error("the hint names what would answer, and is the actionable half")
	}
	if len(e.Metrics) != 1 || e.Metrics[0] != "sales.order_revenue" {
		t.Errorf("the record must name what was asked for, got %v", e.Metrics)
	}
}

// TestARefusalRecordsNoRequestValues. The audit file deliberately carries no
// filter values, and a refusal is recorded from the same place, so the rule has
// to hold here too.
func TestARefusalRecordsNoRequestValues(t *testing.T) {
	audit := &govern.MemoryAudit{}
	eng, err := engine.New(engine.Config{
		ModelPath: "../../testdata/workspace",
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Audit:     audit,
	})
	if err != nil {
		t.Fatal(err)
	}

	const customer = "anna@customer.example"
	_, _ = eng.Query(context.Background(), govern.Identity{Subject: "analyst@acme.com"},
		plan.Request{
			Metrics:    []string{"sales.order_revenue"},
			Dimensions: []string{"sales.order_lines.item_id"},
			Filters: []plan.Filter{{
				Dimension: "sales.customers.email_domain",
				Op:        plan.OpEq,
				Values:    []any{customer},
			}},
		})

	for _, e := range audit.Events() {
		for _, field := range []string{e.Reason, e.Hint, e.Error} {
			if field != "" && contains(field, customer) {
				t.Errorf("a filter value reached the audit record: %q", field)
			}
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// TestADenialIsRecordedOnceAsADenial.
//
// A denial is a refusal, so anything auditing refusals generically records it
// twice: once by the gate as `denied`, once again as `refused`. The second
// copy is worse than a duplicate, because it reports an access denial as a
// correctness problem. Three existing tests caught this the first time.
func TestADenialIsRecordedOnceAsADenial(t *testing.T) {
	for _, code := range []string{
		govern.CodeDenied,
		govern.CodePolicyFailure,
		govern.CodePolicyIdentity,
	} {
		if !govern.IsGateCode(code) {
			t.Errorf("%s comes from the gate and must not be audited twice", code)
		}
	}
	// A correctness refusal is nobody else's to record.
	for _, code := range []string{plan.CodeFanOut, plan.CodeNoMetrics, plan.CodeUnknownMetric} {
		if govern.IsGateCode(code) {
			t.Errorf("%s is a correctness refusal and would go unrecorded", code)
		}
	}
}
