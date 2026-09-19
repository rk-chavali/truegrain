package engine_test

import (
	"context"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// The environment is configuration, and the engine does nothing with it
// except say which one it is. That is the whole feature: a number read six
// months later has to be traceable to the deployment that produced it, and
// nothing else in the system records that.
//
// So the tests are about coverage rather than behaviour. Every decision has
// to carry it, including the ones built somewhere other than the happy
// path, because an audit trail where most entries name their environment is
// worse than one where none do: the gaps read as a different environment
// rather than as a missed field.

func environmentEngine(t *testing.T, audit govern.AuditSink) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath:   "../../testdata/workspace",
		Dialect:     "duckdb",
		Environment: "prod",
		Resolver:    govern.AllowAll{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestHealthNamesTheEnvironment(t *testing.T) {
	if got := environmentEngine(t, nil).Health().Environment; got != "prod" {
		t.Errorf("Health().Environment = %q, want prod", got)
	}
}

// TestEveryAuditedDecisionNamesTheEnvironment, including a refusal, which
// is built before the gate runs and on a different path from an answer.
func TestEveryAuditedDecisionNamesTheEnvironment(t *testing.T) {
	audit := &govern.MemoryAudit{}
	eng := environmentEngine(t, audit)
	id := govern.Identity{Subject: "analyst@acme.com"}

	// A refusal: order_revenue is at order grain and item_id is line level,
	// so the join would inflate the total.
	if _, err := eng.Query(context.Background(), id, plan.Request{
		Metrics:    []string{"sales.order_revenue"},
		Dimensions: []string{"sales.order_lines.item_id"},
	}); err == nil {
		t.Fatal("the fan-out should have been refused")
	}
	// A compile, which audits as its own decision.
	if _, err := eng.DryRun(context.Background(), id, plan.Request{
		Metrics: []string{"sales.order_revenue"},
	}); err != nil {
		t.Fatal(err)
	}

	events := audit.Events()
	if len(events) < 2 {
		t.Fatalf("want a refusal and a compile, got %d events", len(events))
	}
	for _, e := range events {
		if e.Environment != "prod" {
			t.Errorf("a %q decision was recorded with environment %q, want prod",
				e.Decision, e.Environment)
		}
	}
}

// TestAnEngineWithNoEnvironmentSaysNothing, because a deployment that has
// not chosen one should leave the field out rather than inventing a
// default that an evidence query would then filter on.
func TestAnEngineWithNoEnvironmentSaysNothing(t *testing.T) {
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
	if got := eng.Health().Environment; got != "" {
		t.Errorf("Health().Environment = %q, want empty", got)
	}
	if _, err := eng.DryRun(context.Background(),
		govern.Identity{Subject: "a@b"}, plan.Request{Metrics: []string{"sales.order_revenue"}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range audit.Events() {
		if e.Environment != "" {
			t.Errorf("environment = %q on a deployment that declared none", e.Environment)
		}
	}
}
