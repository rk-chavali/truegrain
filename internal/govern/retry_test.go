package govern_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// Telling an agent to wait is only useful when waiting can help.
//
// The gate used to flatten every resolver failure into policy_unavailable,
// which is classified RetryLater. Found against a real project: with policy
// tags on and a human caller, the resolver cannot impersonate them and never
// will, so every query against a tagged column returned "retry later" and an
// agent obeying the classification would have retried until someone stopped it.

// permanentResolver fails the way a misconfigured deployment does.
type permanentResolver struct{}

func (permanentResolver) CanRead(context.Context, govern.Identity, []govern.Ref) (govern.Decision, error) {
	return govern.Decision{}, &plan.Refusal{
		Code:   govern.CodePolicyIdentity,
		Reason: "this engine cannot act as analyst@acme.com",
	}
}

func (permanentResolver) Capabilities() govern.Capabilities {
	return govern.Capabilities{Resolver: "permanent-failure", ColumnLevel: true}
}

// transientResolver fails the way an outage does.
type transientResolver struct{}

func (transientResolver) CanRead(context.Context, govern.Identity, []govern.Ref) (govern.Decision, error) {
	return govern.Decision{}, errors.New("data catalog is unreachable")
}

func (transientResolver) Capabilities() govern.Capabilities {
	return govern.Capabilities{Resolver: "transient-failure", ColumnLevel: true}
}

func check(t *testing.T, r govern.PolicyResolver) *plan.Refusal {
	t.Helper()
	g := govern.NewGate(r, nil, govern.Options{})
	err := g.Check(context.Background(), govern.Identity{Subject: "analyst@acme.com"},
		&resolve.Schema{}, &plan.Plan{})
	if err == nil {
		t.Fatal("a resolver that failed must not produce permission")
	}
	var refusal *plan.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want a structured refusal, got %T: %v", err, err)
	}
	return refusal
}

func TestAPermanentPolicyFailureIsNotRetryable(t *testing.T) {
	refusal := check(t, permanentResolver{})

	if refusal.Code != govern.CodePolicyIdentity {
		t.Errorf("the resolver's own code was relabelled: got %s", refusal.Code)
	}
	if got := refusal.Retry(); got != plan.RetryNever {
		t.Errorf("an engine that can never act as this caller must not be retried: got %v", got)
	}
}

func TestATransientPolicyFailureStaysRetryable(t *testing.T) {
	refusal := check(t, transientResolver{})

	if refusal.Code != govern.CodePolicyFailure {
		t.Errorf("want %s, got %s", govern.CodePolicyFailure, refusal.Code)
	}
	if got := refusal.Retry(); got != plan.RetryLater {
		t.Errorf("a policy source that is down may come back: got %v", got)
	}
}

// TestNeitherFailureGrantsAccess. Both paths above are error paths, and the
// rule that matters more than either classification is that an error is never
// permission.
func TestNeitherFailureGrantsAccess(t *testing.T) {
	for name, r := range map[string]govern.PolicyResolver{
		"permanent": permanentResolver{},
		"transient": transientResolver{},
	} {
		g := govern.NewGate(r, nil, govern.Options{})
		if err := g.Check(context.Background(), govern.Identity{Subject: "a@b.c"},
			&resolve.Schema{}, &plan.Plan{}); err == nil {
			t.Errorf("%s: a failed resolution allowed the query", name)
		}
	}
}
