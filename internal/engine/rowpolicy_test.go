package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// Which rows a caller may see, as opposed to which columns.
//
// Column policy answers "may this caller read the email address". This answers
// "may a regional manager read another region's revenue", which until now was
// yes, because nothing asked.
//
// These tests are mostly attempts to get out of a restriction, because a row
// filter is worth exactly as much as the number of ways around it.

const rowPolicy = `
version: 1
tags: []
grants: []
rows:
  - namespace: sales
    principal: "@acme/northeast"
    description: The northeast team sees northeast orders.
    filter:
      dimension: customers.region
      op: eq
      values: ["NE"]
`

func engineWithRows(t *testing.T, policy string) *engine.Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}

	ws, err := workspace.Load("../../testdata/workspace", workspace.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := govern.LoadFilePolicy(path, ws)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{
		Dialect:  "duckdb",
		Resolver: pol,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func compileAs(t *testing.T, eng *engine.Engine, id govern.Identity, req plan.Request) string {
	t.Helper()
	c, err := eng.Compile(context.Background(), id, req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c.SQL
}

// TestARestrictedCallerGetsTheFilter is the base case.
func TestARestrictedCallerGetsTheFilter(t *testing.T) {
	eng := engineWithRows(t, rowPolicy)

	restricted := govern.Identity{Subject: "sam@acme.com", Groups: []string{"@acme/northeast"}}
	sql := compileAs(t, eng, restricted, plan.Request{Metrics: []string{"sales.order_revenue"}})

	if !strings.Contains(sql, "region") {
		t.Errorf("the restriction did not reach the SQL:\n%s", sql)
	}
}

// TestAnUnrestrictedCallerIsUnaffected. A policy restricts the principals it
// names and nobody else, which matches how an untagged column is readable.
func TestAnUnrestrictedCallerIsUnaffected(t *testing.T) {
	eng := engineWithRows(t, rowPolicy)

	anyone := govern.Identity{Subject: "chris@acme.com"}
	sql := compileAs(t, eng, anyone, plan.Request{Metrics: []string{"sales.order_revenue"}})

	if strings.Contains(sql, "region") {
		t.Errorf("a caller nobody restricted was filtered anyway:\n%s", sql)
	}
}

// TestTheCallerCannotFilterTheirWayOut.
//
// The obvious attack: ask for a different region and hope the policy is
// replaced rather than added. Both filters must survive, combined by AND, so
// the result is the intersection and therefore empty.
func TestTheCallerCannotFilterTheirWayOut(t *testing.T) {
	eng := engineWithRows(t, rowPolicy)

	restricted := govern.Identity{Subject: "sam@acme.com", Groups: []string{"@acme/northeast"}}
	sql := compileAs(t, eng, restricted, plan.Request{
		Metrics: []string{"sales.order_revenue"},
		Filters: []plan.Filter{{
			Dimension: "customers.region",
			Op:        plan.OpEq,
			Values:    []any{"MW"},
		}},
	})

	// Two conditions on region, not one. A policy that replaced the caller's
	// filter, or was replaced by it, would leave a single comparison.
	if got := strings.Count(sql, "region"); got < 2 {
		t.Errorf("want the caller's filter and the policy's, got %d mentions:\n%s", got, sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "AND") {
		t.Errorf("the two filters must combine by AND:\n%s", sql)
	}
}

// TestTheRestrictionAppliesToEveryMetricInTheNamespace, not only the one that
// happened to be in the policy author's head.
func TestTheRestrictionAppliesToEveryMetricInTheNamespace(t *testing.T) {
	eng := engineWithRows(t, rowPolicy)
	restricted := govern.Identity{Subject: "sam@acme.com", Groups: []string{"@acme/northeast"}}

	for _, metric := range []string{
		"sales.order_revenue",
		"sales.order_count",
		"sales.largest_order",
	} {
		sql := compileAs(t, eng, restricted, plan.Request{Metrics: []string{metric}})
		if !strings.Contains(sql, "region") {
			t.Errorf("%s escaped the restriction:\n%s", metric, sql)
		}
	}
}

// TestAPolicyForOneNamespaceDoesNotLeakIntoAnother. A restriction written for
// sales must not attach itself to a marketing-only query, where its dimension
// may not even resolve.
func TestAPolicyForOneNamespaceDoesNotLeakIntoAnother(t *testing.T) {
	eng := engineWithRows(t, rowPolicy)
	restricted := govern.Identity{Subject: "sam@acme.com", Groups: []string{"@acme/northeast"}}

	sql := compileAs(t, eng, restricted, plan.Request{Metrics: []string{"marketing.campaign_spend"}})
	if strings.Contains(sql, "region") {
		t.Errorf("a sales restriction reached a marketing query:\n%s", sql)
	}
}

// TestASubjectPolicyWorksToo, since not every restriction is a group.
func TestASubjectPolicyWorksToo(t *testing.T) {
	eng := engineWithRows(t, `
version: 1
tags: []
grants: []
rows:
  - namespace: sales
    principal: "contractor@partner.example"
    filter:
      dimension: customers.region
      op: eq
      values: ["MW"]
`)

	sql := compileAs(t, eng, govern.Identity{Subject: "contractor@partner.example"},
		plan.Request{Metrics: []string{"sales.order_revenue"}})
	if !strings.Contains(sql, "region") {
		t.Errorf("a policy naming a subject did not apply:\n%s", sql)
	}
}

// TestAFilterThatWillNotResolveRefuses.
//
// Silently dropping a restriction whose dimension no longer exists is a data
// leak wearing the costume of a permissive default: the column was renamed,
// the policy stopped matching, and everybody kept believing it was in force.
func TestAFilterThatWillNotResolveRefuses(t *testing.T) {
	eng := engineWithRows(t, `
version: 1
tags: []
grants: []
rows:
  - namespace: sales
    principal: "@acme/northeast"
    filter:
      dimension: customers.no_such_column
      op: eq
      values: ["NE"]
`)

	_, err := eng.Compile(context.Background(),
		govern.Identity{Subject: "sam@acme.com", Groups: []string{"@acme/northeast"}},
		plan.Request{Metrics: []string{"sales.order_revenue"}})

	if err == nil {
		t.Fatal("a restriction that cannot be applied must refuse the query, not be skipped")
	}
}

func TestRowPolicyValidationCatchesTypos(t *testing.T) {
	namespaces := []string{"sales", "marketing"}

	bad := govern.RowPolicies{
		{Namespace: "sails", Principal: "@a", Filter: plan.Filter{Dimension: "x", Op: plan.OpEq}},
	}
	if err := bad.Validate(namespaces); err == nil {
		t.Error("a namespace that does not exist must fail the load")
	}

	noPrincipal := govern.RowPolicies{
		{Namespace: "sales", Filter: plan.Filter{Dimension: "x", Op: plan.OpEq}},
	}
	if err := noPrincipal.Validate(namespaces); err == nil {
		t.Error("a policy restricting nobody must fail the load")
	}

	good := govern.RowPolicies{
		{Namespace: "sales", Principal: "@a", Filter: plan.Filter{Dimension: "x", Op: plan.OpEq}},
	}
	if err := good.Validate(namespaces); err != nil {
		t.Errorf("a valid policy was rejected: %v", err)
	}
}
