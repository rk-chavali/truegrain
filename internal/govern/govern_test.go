package govern_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// This package decides who may read what, so it is tested directly rather than
// only through the engine. Every test here corresponds to a way the gate could
// be wrong in a direction that grants access.

func testdata(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, append([]string{"testdata"}, parts...)...)...)
}

func fixture(t *testing.T) (*resolve.Schema, *plan.Plan) {
	t.Helper()
	model, err := osi.Load(testdata("models", "retail.yaml"))
	if err != nil {
		t.Fatalf("loading fixture:\n%v", err)
	}
	for _, d := range model.Datasets {
		d.Namespace = model.Name
	}
	schema, err := resolve.New(model)
	if err != nil {
		t.Fatalf("resolving fixture:\n%v", err)
	}
	p, err := plan.New(schema).Plan(plan.Request{
		Metrics:    []string{"order_revenue"},
		Dimensions: []string{"customers.region"},
	})
	if err != nil {
		t.Fatalf("planning:\n%v", err)
	}
	p.Namespace = model.Name
	return schema, p
}

func workspaceFor(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.Load(testdata("models", "retail.yaml"), workspace.Options{Strict: true})
	if err != nil {
		t.Fatalf("loading workspace:\n%v", err)
	}
	return ws
}

func refusalCode(err error) string {
	var r *plan.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// ---------- fail closed ----------

// countingResolver records how often it was consulted and can be made to fail.
type countingResolver struct {
	calls   atomic.Int64
	fail    atomic.Bool
	denyAll atomic.Bool
}

func (c *countingResolver) CanRead(_ context.Context, _ govern.Identity, refs []govern.Ref) (govern.Decision, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return govern.Decision{}, errors.New("catalog unreachable")
	}
	if c.denyAll.Load() {
		return govern.Decision{Allowed: false, Denied: refs}, nil
	}
	return govern.Decision{Allowed: true}, nil
}

func (c *countingResolver) Capabilities() govern.Capabilities {
	return govern.Capabilities{Resolver: "counting", ColumnLevel: true}
}

// TestResolverErrorDenies is the fail-closed guarantee. Treating an unreachable
// policy source as permission turns the whole control into a no-op during an
// outage, which is exactly when it matters.
func TestResolverErrorDenies(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	resolver.fail.Store(true)

	gate := govern.NewGate(resolver, nil, govern.Options{})
	err := gate.Check(context.Background(), govern.Identity{Subject: "someone"}, schema, p)

	if err == nil {
		t.Fatal("a policy source that cannot be reached must deny")
	}
	if got := refusalCode(err); got != govern.CodePolicyFailure {
		t.Fatalf("want %q, got %q", govern.CodePolicyFailure, got)
	}
}

// TestNilResolverDeniesEverything covers a misconfigured deployment. A gate
// built by mistake must refuse rather than grant.
func TestNilResolverDeniesEverything(t *testing.T) {
	schema, p := fixture(t)
	gate := govern.NewGate(nil, nil, govern.Options{})

	if err := gate.Check(context.Background(), govern.Identity{Subject: "someone"}, schema, p); err == nil {
		t.Fatal("a gate with no resolver must deny")
	}
	if caps := gate.Capabilities(); !strings.Contains(strings.ToLower(caps.Note), "denied") {
		t.Errorf("capabilities should say every request is denied, got %q", caps.Note)
	}
}

// TestErrorIsAuditedNotSwallowed: an outage that produces no record is an
// outage nobody can reconstruct afterwards.
func TestErrorIsAuditedNotSwallowed(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	resolver.fail.Store(true)
	audit := &govern.MemoryAudit{}

	_ = govern.NewGate(resolver, audit, govern.Options{}).
		Check(context.Background(), govern.Identity{Subject: "someone"}, schema, p)

	events := audit.Events()
	if len(events) != 1 || events[0].Decision != "error" {
		t.Fatalf("want one error event, got %+v", events)
	}
	if events[0].Error == "" {
		t.Error("the event should carry why the policy source failed")
	}
}

// ---------- the decision cache ----------

// TestCacheDoesNotLeakAcrossIdentities is the sharpest way this cache could be
// wrong: one identity's allow being reused for another is a total bypass.
func TestCacheDoesNotLeakAcrossIdentities(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	gate := govern.NewGate(resolver, nil, govern.Options{CacheTTL: time.Minute})
	ctx := context.Background()

	if err := gate.Check(ctx, govern.Identity{Subject: "alice"}, schema, p); err != nil {
		t.Fatal(err)
	}
	before := resolver.calls.Load()

	// A different identity must not be answered from alice's entry.
	if err := gate.Check(ctx, govern.Identity{Subject: "bob"}, schema, p); err != nil {
		t.Fatal(err)
	}
	if resolver.calls.Load() == before {
		t.Fatal("a second identity was answered from the first identity's cached decision")
	}
}

// TestCacheDoesNotLeakAcrossGroups checks membership is part of the key too. An
// identity that gains or loses a group is a different principal.
func TestCacheDoesNotLeakAcrossGroups(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	gate := govern.NewGate(resolver, nil, govern.Options{CacheTTL: time.Minute})
	ctx := context.Background()

	plain := govern.Identity{Subject: "sam"}
	elevated := govern.Identity{Subject: "sam", Groups: []string{"finance"}}

	if err := gate.Check(ctx, plain, schema, p); err != nil {
		t.Fatal(err)
	}
	before := resolver.calls.Load()
	if err := gate.Check(ctx, elevated, schema, p); err != nil {
		t.Fatal(err)
	}
	if resolver.calls.Load() == before {
		t.Fatal("adding a group reused the decision made without it")
	}
}

func TestCacheAvoidsRepeatedResolutionForTheSameCaller(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	gate := govern.NewGate(resolver, nil, govern.Options{CacheTTL: time.Minute})
	ctx := context.Background()
	id := govern.Identity{Subject: "alice"}

	for range 3 {
		if err := gate.Check(ctx, id, schema, p); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolver.calls.Load(); got != 1 {
		t.Errorf("want the resolver consulted once, got %d", got)
	}
}

// TestCacheExpires is the other half: a revoked grant has to take effect. A
// positive decision held past its TTL is an access control bypass with a timer
// on it.
func TestCacheExpires(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	gate := govern.NewGate(resolver, nil, govern.Options{CacheTTL: 20 * time.Millisecond})
	ctx := context.Background()
	id := govern.Identity{Subject: "alice"}

	if err := gate.Check(ctx, id, schema, p); err != nil {
		t.Fatal(err)
	}
	// The grant is revoked behind the gate's back.
	resolver.denyAll.Store(true)

	// Still cached, so still allowed. This is the documented window.
	if err := gate.Check(ctx, id, schema, p); err != nil {
		t.Fatalf("within the TTL the cached allow should stand:\n%v", err)
	}

	time.Sleep(40 * time.Millisecond)
	if err := gate.Check(ctx, id, schema, p); err == nil {
		t.Fatal("after the TTL the revoked grant must take effect")
	}
}

// TestReadingDoesNotExtendTheTTL: if a read refreshed the entry, a busy caller
// would hold a revoked grant indefinitely, which is the worst case for this
// cache.
func TestReadingDoesNotExtendTheTTL(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	gate := govern.NewGate(resolver, nil, govern.Options{CacheTTL: 60 * time.Millisecond})
	ctx := context.Background()
	id := govern.Identity{Subject: "alice"}

	if err := gate.Check(ctx, id, schema, p); err != nil {
		t.Fatal(err)
	}
	resolver.denyAll.Store(true)

	// Read repeatedly across the whole TTL. If reads refreshed it, the entry
	// would never expire and the last check would still be allowed.
	deadline := time.Now().Add(80 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = gate.Check(ctx, id, schema, p)
		time.Sleep(10 * time.Millisecond)
	}
	if err := gate.Check(ctx, id, schema, p); err == nil {
		t.Fatal("constant reads kept a revoked decision alive past its TTL")
	}
}

// ---------- what a denial discloses ----------

// TestDenialNamesSemanticObjectsOnly. A refusal is shown to someone who by
// definition lacks access, so it must not teach them the schema.
func TestDenialNamesSemanticObjectsOnly(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	resolver.denyAll.Store(true)

	err := govern.NewGate(resolver, nil, govern.Options{}).
		Check(context.Background(), govern.Identity{Subject: "nobody"}, schema, p)
	if err == nil {
		t.Fatal("expected a denial")
	}

	message := err.Error()
	for _, leak := range []string{"main.orders", "main.customers", "order_total", "SELECT"} {
		if strings.Contains(message, leak) {
			t.Errorf("the refusal discloses %q to a caller without access:\n%s", leak, message)
		}
	}
	if !strings.Contains(message, "order_revenue") {
		t.Errorf("the refusal should name what the caller asked for:\n%s", message)
	}
}

// TestDeniedFieldsAreAuditedButNotReturned. The audit needs the detail; the
// caller must not have it.
func TestDeniedFieldsAreAuditedButNotReturned(t *testing.T) {
	schema, p := fixture(t)
	resolver := &countingResolver{}
	resolver.denyAll.Store(true)
	audit := &govern.MemoryAudit{}

	err := govern.NewGate(resolver, audit, govern.Options{}).
		Check(context.Background(), govern.Identity{Subject: "nobody"}, schema, p)

	events := audit.Events()
	if len(events) != 1 || len(events[0].DeniedFields) == 0 {
		t.Fatalf("the audit must record which fields were denied, got %+v", events)
	}
	for _, field := range events[0].DeniedFields {
		if strings.Contains(err.Error(), field) {
			t.Errorf("denied field %q appears in the message shown to the caller", field)
		}
	}
}

// ---------- the file resolver ----------

func filePolicy(t *testing.T, name string) *govern.FilePolicy {
	t.Helper()
	pol, err := govern.LoadFilePolicy(testdata(name), workspaceFor(t))
	if err != nil {
		t.Fatalf("loading policy:\n%v", err)
	}
	return pol
}

func refsFor(t *testing.T, names ...string) []govern.Ref {
	t.Helper()
	refs := make([]govern.Ref, 0, len(names))
	for _, n := range names {
		parts := strings.Split(n, ".")
		if len(parts) != 3 {
			t.Fatalf("test ref %q must be namespace.dataset.field", n)
		}
		refs = append(refs, govern.Ref{Origin: parts[0] + "." + parts[1], Field: parts[2]})
	}
	return refs
}

func TestFilePolicyDeniesWithoutTheTag(t *testing.T) {
	pol := filePolicy(t, "policy.yaml")
	decision, err := pol.CanRead(context.Background(),
		govern.Identity{Subject: "sa-denied@example.iam.gserviceaccount.com"},
		refsFor(t, "retail.orders.order_total"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed {
		t.Fatal("a protected field must be denied to an identity without the tag")
	}
}

func TestFilePolicyAllowsTheGrantHolder(t *testing.T) {
	pol := filePolicy(t, "policy.yaml")
	decision, err := pol.CanRead(context.Background(),
		govern.Identity{Subject: "sa-allowed@example.iam.gserviceaccount.com"},
		refsFor(t, "retail.orders.order_total"))
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Allowed {
		t.Fatalf("the grant holder should be allowed, denied: %v", decision.Denied)
	}
}

// TestGrantsResolveThroughGroups: grants go to groups, so membership has to be
// what satisfies them.
func TestGrantsResolveThroughGroups(t *testing.T) {
	pol := filePolicy(t, "policy.yaml")
	decision, err := pol.CanRead(context.Background(),
		govern.Identity{Subject: "nobody-in-particular", Groups: []string{"finance@example.com"}},
		refsFor(t, "retail.orders.order_total"))
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Allowed {
		t.Fatal("membership of a granted group should satisfy the tag")
	}
}

func TestUngovernedFieldNeedsNoGrant(t *testing.T) {
	pol := filePolicy(t, "policy.yaml")
	decision, err := pol.CanRead(context.Background(),
		govern.Identity{Subject: "anyone"}, refsFor(t, "retail.orders.status"))
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Allowed {
		t.Fatal("a field no tag protects should need no grant")
	}
}

// TestStalePolicyEntryFailsToLoad. A policy naming a field that no longer
// exists leaves a column everyone believed was protected readable, so it is
// refused rather than ignored.
func TestStalePolicyEntryFailsToLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, `version: 1
tags:
  - name: financial
    fields: [orders.order_totla]
grants:
  - principal: someone@example.com
    tags: [financial]
`)
	_, err := govern.LoadFilePolicy(path, workspaceFor(t))
	if err == nil {
		t.Fatal("a policy naming a field the model does not define must fail to load")
	}
	if !strings.Contains(err.Error(), "order_totla") {
		t.Errorf("the error should name the bad entry:\n%v", err)
	}
}

func TestUnknownTagInGrantFailsToLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, `version: 1
tags:
  - name: financial
    fields: [orders.order_total]
grants:
  - principal: someone@example.com
    tags: [financail]
`)
	if _, err := govern.LoadFilePolicy(path, workspaceFor(t)); err == nil {
		t.Fatal("a grant naming a tag that does not exist must fail to load")
	}
}

func TestPolicyVersionIsChecked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, "version: 2\ntags: []\n")
	if _, err := govern.LoadFilePolicy(path, workspaceFor(t)); err == nil {
		t.Fatal("an unsupported policy version must fail to load")
	}
}

// TestTagProtectingNothingFailsToLoad catches a tag that looks like protection
// and is not, which is worse than no tag at all.
func TestTagProtectingNothingFailsToLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, "version: 1\ntags:\n  - name: empty\n")
	if _, err := govern.LoadFilePolicy(path, workspaceFor(t)); err == nil {
		t.Fatal("a tag protecting no fields and no namespaces must fail to load")
	}
}

// TestAllRequiredTagsMustBeHeld. A column carrying two classifications is
// protected by both, and satisfying one of them is not permission to read it.
func TestAllRequiredTagsMustBeHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, `version: 1
tags:
  - name: financial
    fields: [orders.order_total]
  - name: restricted
    fields: [orders.order_total]
grants:
  - principal: half@example.com
    tags: [financial]
  - principal: full@example.com
    tags: [financial, restricted]
`)
	pol, err := govern.LoadFilePolicy(path, workspaceFor(t))
	if err != nil {
		t.Fatal(err)
	}
	refs := refsFor(t, "retail.orders.order_total")

	half, err := pol.CanRead(context.Background(), govern.Identity{Subject: "half@example.com"}, refs)
	if err != nil {
		t.Fatal(err)
	}
	if half.Allowed {
		t.Error("holding one of two required tags must not grant access")
	}

	full, err := pol.CanRead(context.Background(), govern.Identity{Subject: "full@example.com"}, refs)
	if err != nil {
		t.Fatal(err)
	}
	if !full.Allowed {
		t.Error("holding both required tags should grant access")
	}
}

// ---------- honesty about capabilities ----------

func TestAllowAllSaysItEnforcesNothing(t *testing.T) {
	caps := govern.AllowAll{}.Capabilities()
	if caps.ColumnLevel {
		t.Error("AllowAll must not claim column-level enforcement")
	}
	if !strings.Contains(strings.ToLower(caps.Note), "no access control") {
		t.Errorf("the note must say plainly that nothing is enforced, got %q", caps.Note)
	}
}

func TestFilePolicySaysItDoesNotGovernTheWarehouse(t *testing.T) {
	caps := filePolicy(t, "policy.yaml").Capabilities()
	if !caps.ColumnLevel {
		t.Error("the file resolver does make column-level decisions")
	}
	// Overstating a governance guarantee is worse than not offering one.
	if !strings.Contains(caps.Note, "warehouse") {
		t.Errorf("the note must say it places no control on the warehouse, got %q", caps.Note)
	}
}

func TestRefNameUsesOrigin(t *testing.T) {
	ref := govern.Ref{Origin: "sales.orders", Field: "order_total", Local: "imported.order_total"}
	if got := ref.Name(); got != "sales.orders.order_total" {
		t.Errorf("policy must key on the origin, got %q", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
