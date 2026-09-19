package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// This is the governance suite from docs/06-validation.md, run against the
// file-backed resolver so it needs no cloud account.
//
// The assertion that matters is not that a denied request errors. It is that a
// denied request produces no SQL and reaches no executor. That is the whole
// difference between compile-time governance and a pretty error wrapper around
// a warehouse rejection, and it is the one property a reviewer will check.

const (
	allowed = "sa-allowed@example.iam.gserviceaccount.com"
	denied  = "sa-denied@example.iam.gserviceaccount.com"
)

func modelDir() string { return filepath.Join("..", "..", "testdata", "models") }

// spyExecutor records every statement it is asked to run. In a denial test it
// must record nothing at all.
type spyExecutor struct {
	statements []string
}

func (s *spyExecutor) Name() string { return "spy" }

func (s *spyExecutor) Execute(_ context.Context, _ govern.Identity, sql string, _ []any) (*engine.Rows, error) {
	s.statements = append(s.statements, sql)
	return &engine.Rows{Columns: []string{"order_revenue"}, Rows: [][]any{{100.0}}, JobID: "spy-1"}, nil
}

func (s *spyExecutor) Close() error { return nil }

func policyEngine(t *testing.T, ex engine.Executor, audit govern.AuditSink) *engine.Engine {
	t.Helper()
	ws, err := workspace.Load(filepath.Join(modelDir(), "retail.yaml"), workspace.Options{Strict: true})
	if err != nil {
		t.Fatalf("loading workspace:\n%v", err)
	}
	pol, err := govern.LoadFilePolicy(filepath.Join("..", "..", "testdata", "policy.yaml"), ws)
	if err != nil {
		t.Fatalf("loading policy:\n%v", err)
	}
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join(modelDir(), "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  pol,
		Audit:     audit,
		Executor:  ex,
	})
	if err != nil {
		t.Fatalf("building engine:\n%v", err)
	}
	return eng
}

func refusalCode(err error) string {
	var r *plan.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// TestUngovernedMetricAllowedForEither is the control: a request touching no
// protected column runs for anybody.
func TestUngovernedMetricAllowedForEither(t *testing.T) {
	for _, subject := range []string{allowed, denied} {
		t.Run(subject, func(t *testing.T) {
			eng := policyEngine(t, nil, nil)
			_, err := eng.Compile(context.Background(),
				govern.Identity{Subject: subject},
				plan.Request{Metrics: []string{"order_count"}, Dimensions: []string{"customers.region"}})
			if err != nil {
				t.Fatalf("order_count reads no protected column and must be allowed:\n%v", err)
			}
		})
	}
}

func TestProtectedMetricAllowedForGrantedIdentity(t *testing.T) {
	eng := policyEngine(t, nil, nil)
	c, err := eng.Compile(context.Background(),
		govern.Identity{Subject: allowed},
		plan.Request{Metrics: []string{"order_revenue"}})
	if err != nil {
		t.Fatalf("%s holds the financial tag and must be allowed:\n%v", allowed, err)
	}
	if !strings.Contains(c.SQL, "order_total") {
		t.Errorf("expected the protected column in the compiled SQL:\n%s", c.SQL)
	}
}

// TestDeniedProducesNoSQL is the important one.
func TestDeniedProducesNoSQL(t *testing.T) {
	spy := &spyExecutor{}
	audit := &govern.MemoryAudit{}
	eng := policyEngine(t, spy, audit)

	compiled, err := eng.Compile(context.Background(),
		govern.Identity{Subject: denied},
		plan.Request{Metrics: []string{"order_revenue"}})

	if err == nil {
		t.Fatal("an identity without the financial tag must not be able to compile order_revenue")
	}
	if got := refusalCode(err); got != govern.CodeDenied {
		t.Fatalf("want refusal code %q, got %q from: %v", govern.CodeDenied, got, err)
	}
	if compiled != nil {
		t.Fatalf("a denied request produced SQL, which means the plan left the engine:\n%s", compiled.SQL)
	}
	if len(spy.statements) != 0 {
		t.Fatalf("a denied request reached the executor: %v", spy.statements)
	}
}

// TestDeniedQueryNeverExecutes is the same assertion through Query rather than
// Compile, because that is the path a real caller takes.
func TestDeniedQueryNeverExecutes(t *testing.T) {
	spy := &spyExecutor{}
	eng := policyEngine(t, spy, nil)

	if _, err := eng.Query(context.Background(),
		govern.Identity{Subject: denied},
		plan.Request{Metrics: []string{"order_revenue"}}); err == nil {
		t.Fatal("expected a denial")
	}
	if len(spy.statements) != 0 {
		t.Fatalf("a denied query was sent to the warehouse: %v", spy.statements)
	}
}

// TestDenialIsAudited mirrors the audit row of the validation table: the denial
// is recorded with the identity and the metric.
func TestDenialIsAudited(t *testing.T) {
	audit := &govern.MemoryAudit{}
	eng := policyEngine(t, nil, audit)

	_, _ = eng.Compile(context.Background(),
		govern.Identity{Subject: denied},
		plan.Request{Metrics: []string{"order_revenue"}})

	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("want exactly one audit event, got %d", len(events))
	}
	e := events[0]
	if e.Decision != "denied" {
		t.Errorf("want decision denied, got %q", e.Decision)
	}
	if e.Identity != denied {
		t.Errorf("want identity %q, got %q", denied, e.Identity)
	}
	if len(e.Metrics) != 1 || e.Metrics[0] != "order_revenue" {
		t.Errorf("want the metric recorded, got %v", e.Metrics)
	}
	if len(e.DeniedFields) == 0 {
		t.Error("the audit event should record which fields were denied")
	}
	if e.ModelVersion == "" {
		t.Error("the audit event should record the model version")
	}
}

// TestRefusalDoesNotLeakSchema checks the denial message names the semantic
// object the caller asked for and not the physical column behind it.
func TestRefusalDoesNotLeakSchema(t *testing.T) {
	eng := policyEngine(t, nil, nil)
	_, err := eng.Compile(context.Background(),
		govern.Identity{Subject: denied},
		plan.Request{Metrics: []string{"order_revenue"}})
	if err == nil {
		t.Fatal("expected a denial")
	}
	msg := err.Error()
	if !strings.Contains(msg, "order_revenue") {
		t.Errorf("the refusal should name the metric the caller asked for:\n%s", msg)
	}
	for _, leak := range []string{"main.orders", "order_total", "financial"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the refusal leaks %q, which the caller has no access to:\n%s", leak, msg)
		}
	}
}

// TestDeniedDimensionNotOffered is the last row of the validation table. A
// dimension the caller cannot read is not advertised, so nobody is invited to
// ask for something that will be refused.
func TestDeniedDimensionNotOffered(t *testing.T) {
	eng := policyEngine(t, nil, nil)
	ctx := context.Background()

	visible, err := eng.VisibleDimensions(ctx, govern.Identity{Subject: denied}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range visible {
		if f.Field.QualifiedName() == "customers.email_domain" {
			t.Error("a pii dimension was offered to an identity without the pii tag")
		}
	}

	cleared, err := eng.VisibleDimensions(ctx,
		govern.Identity{Subject: "someone", Groups: []string{"privacy-cleared@example.com"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range cleared {
		if f.Field.QualifiedName() == "customers.email_domain" {
			found = true
		}
	}
	if !found {
		t.Error("a granted identity should be offered the dimension; the grant was resolved through a group")
	}
}

// TestGroupGrantsResolve confirms a grant to a group applies to a member.
func TestGroupGrantsResolve(t *testing.T) {
	eng := policyEngine(t, nil, nil)
	if _, err := eng.Compile(context.Background(),
		govern.Identity{Subject: "analyst@example.com", Groups: []string{"finance@example.com"}},
		plan.Request{Metrics: []string{"order_revenue"}}); err != nil {
		t.Fatalf("membership of finance@example.com grants the financial tag:\n%v", err)
	}
}

// failingResolver stands in for an unreachable policy source.
type failingResolver struct{}

func (failingResolver) CanRead(context.Context, govern.Identity, []govern.Ref) (govern.Decision, error) {
	return govern.Decision{}, errors.New("catalog unreachable")
}

func (failingResolver) Capabilities() govern.Capabilities {
	return govern.Capabilities{Resolver: "failing", ColumnLevel: true}
}

// TestPolicyFailureDeniesRatherThanAllows is the fail-closed guarantee. A
// policy source that cannot be reached is not permission.
func TestPolicyFailureDeniesRatherThanAllows(t *testing.T) {
	spy := &spyExecutor{}
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join(modelDir(), "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  failingResolver{},
		Executor:  spy,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.Query(context.Background(),
		govern.Identity{Subject: allowed},
		plan.Request{Metrics: []string{"order_count"}})
	if err == nil {
		t.Fatal("a policy source failure must deny, not allow")
	}
	if got := refusalCode(err); got != govern.CodePolicyFailure {
		t.Fatalf("want %q, got %q from: %v", govern.CodePolicyFailure, got, err)
	}
	if len(spy.statements) != 0 {
		t.Fatal("a query ran while access could not be resolved")
	}
}

// TestNoResolverDeniesEverything covers a misconfigured deployment. An engine
// built without a policy resolver must refuse, not grant.
func TestNoResolverDeniesEverything(t *testing.T) {
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join(modelDir(), "retail.yaml"),
		Dialect:   "duckdb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Compile(context.Background(), govern.Identity{Subject: allowed},
		plan.Request{Metrics: []string{"order_count"}}); err == nil {
		t.Fatal("an engine with no policy resolver must deny every request")
	}
}

// TestStalePolicyEntryRefusesToLoad covers the failure that would silently
// unprotect a column: a policy naming a field the model no longer has.
func TestStalePolicyEntryRefusesToLoad(t *testing.T) {
	ws, err := workspace.Load(filepath.Join(modelDir(), "retail.yaml"), workspace.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(`version: 1
tags:
  - name: financial
    fields: [orders.order_totla]
grants:
  - principal: someone@example.com
    tags: [financial]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := govern.LoadFilePolicy(path, ws); err == nil {
		t.Fatal("a policy naming a field the model does not define must fail to load; " +
			"ignoring it leaves a column everyone believed was protected readable")
	} else if !strings.Contains(err.Error(), "order_totla") {
		t.Errorf("the error should name the bad entry:\n%v", err)
	}
}

// TestHealthReportsGovernanceHonestly checks the engine does not overstate what
// it enforces, which docs/05-governance.md calls worse than not offering it.
func TestHealthReportsGovernanceHonestly(t *testing.T) {
	eng, err := engine.New(engine.Config{
		ModelPath: filepath.Join(modelDir(), "retail.yaml"),
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := eng.Health()
	if h.Governance.ColumnLevel {
		t.Error("AllowAll must not claim column-level enforcement")
	}
	if h.Dialect.ColumnLevelSecurity || h.Dialect.RowLevelSecurity {
		t.Error("DuckDB enforces neither and must not claim to")
	}
	if len(h.EnforcementNotes) == 0 {
		t.Error("a configuration that enforces nothing must say so in health")
	}
}
