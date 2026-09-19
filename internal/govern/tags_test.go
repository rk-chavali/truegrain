package govern_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// The decision logic is tested against fakes rather than a GCP project. What
// needs real Data Catalog is the lookup; what decides whether a caller reads a
// column is here, and it must be right without anybody holding credentials.

const (
	piiTag     = "projects/p/locations/eu/taxonomies/1/policyTags/pii"
	revenueTag = "projects/p/locations/eu/taxonomies/1/policyTags/revenue"
)

type fakeTags struct {
	byTable map[string]map[string][]string
	err     error
	calls   int
}

func (f *fakeTags) TagsFor(_ context.Context, source string) (map[string][]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byTable[source], nil
}

type fakeAccess struct {
	allow map[string]bool // policy tag -> may read
	err   error
	calls int
}

func (f *fakeAccess) CanReadTag(_ context.Context, _ govern.Identity, tag string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.allow[tag], nil
}

func resolver(tags *fakeTags, access *fakeAccess) *govern.TagResolver {
	return govern.NewTagResolver(tags, access)
}

var analyst = govern.Identity{Subject: "analyst@example.com"}

// TestUntaggedColumnsAreReadable is BigQuery's own semantics, not a default we
// invented. Getting this wrong the other way would deny every column in every
// table that has no taxonomy, which is most of them.
func TestUntaggedColumnsAreReadable(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.orders": {"email": {piiTag}},
	}}
	access := &fakeAccess{allow: map[string]bool{}}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.orders", Field: "order_id", Source: "analytics.orders", Column: "order_id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Fatalf("an untagged column must be readable, denied: %v", d.Denied)
	}
	if access.calls != 0 {
		t.Errorf("an untagged column must not cost an IAM check, made %d", access.calls)
	}
}

func TestTaggedColumnIsDeniedWithoutTheGrant(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.customers": {"email": {piiTag}},
	}}
	access := &fakeAccess{allow: map[string]bool{piiTag: false}}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.customers", Field: "email", Source: "analytics.customers", Column: "email"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("a tagged column must be denied when IAM says no")
	}
	if len(d.Denied) != 1 || d.Denied[0].Field != "email" {
		t.Errorf("the denial must name the field, got %v", d.Denied)
	}
}

func TestTaggedColumnIsAllowedWithTheGrant(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.customers": {"email": {piiTag}},
	}}
	access := &fakeAccess{allow: map[string]bool{piiTag: true}}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.customers", Field: "email", Source: "analytics.customers", Column: "email"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Fatalf("a tagged column must be readable when IAM allows it, denied: %v", d.Denied)
	}
}

// TestEveryTagMustAllow: a column can carry more than one tag, and access to
// one is not access to the column.
func TestEveryTagMustAllow(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.orders": {"amount": {revenueTag, piiTag}},
	}}
	access := &fakeAccess{allow: map[string]bool{revenueTag: true, piiTag: false}}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.orders", Field: "amount", Source: "analytics.orders", Column: "amount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("holding one of two required tags must not grant the column")
	}
}

// TestComputedFieldRequiresEveryTagInTheTable.
//
// A field whose expression does not reduce to one column could read any of
// them. Requiring every tag in the table can deny a field that would have been
// allowed; it can never allow one that should have been denied, which is the
// direction to be wrong in.
func TestComputedFieldRequiresEveryTagInTheTable(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.orders": {"amount": {revenueTag}, "email": {piiTag}},
	}}
	access := &fakeAccess{allow: map[string]bool{revenueTag: true, piiTag: false}}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		// No Column: the engine could not reduce the expression to one column.
		{Origin: "sales.orders", Field: "margin", Source: "analytics.orders"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("a computed field must not be readable when any tag in its table is denied")
	}
}

func TestComputedFieldIsAllowedWhenTheTableHasNoTags(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.orders": {},
	}}
	access := &fakeAccess{allow: map[string]bool{}}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.orders", Field: "margin", Source: "analytics.orders"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Fatalf("an untagged table must not deny a computed field, denied: %v", d.Denied)
	}
}

// TestSchemaLookupFailureIsAnError. The gate turns an error into a refusal that
// retries later. Returning Allowed because Data Catalog was unreachable is the
// one outcome that must never happen.
func TestSchemaLookupFailureIsAnError(t *testing.T) {
	tags := &fakeTags{err: errors.New("data catalog unreachable")}
	access := &fakeAccess{allow: map[string]bool{}}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.customers", Field: "email", Source: "analytics.customers", Column: "email"},
	})
	if err == nil {
		t.Fatal("a policy source outage must be an error, never a decision")
	}
	if d.Allowed {
		t.Fatal("a failed lookup reported Allowed")
	}
}

func TestIAMFailureIsAnError(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.customers": {"email": {piiTag}},
	}}
	access := &fakeAccess{err: errors.New("iam unreachable")}

	d, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.customers", Field: "email", Source: "analytics.customers", Column: "email"},
	})
	if err == nil {
		t.Fatal("an IAM outage must be an error, never a decision")
	}
	if d.Allowed {
		t.Fatal("a failed IAM check reported Allowed")
	}
}

// TestRefWithNoSourceIsRefused: without a table there is nothing to look up,
// and no way to prove the data is unrestricted.
func TestRefWithNoSourceIsRefused(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{}}
	access := &fakeAccess{allow: map[string]bool{}}

	if _, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.orders", Field: "order_id"},
	}); err == nil {
		t.Fatal("a ref with no source table must not be silently allowed")
	}
}

// TestOneTagIsCheckedOncePerQuery. A query over eight columns behind one tag
// should cost one IAM round trip, not eight.
func TestOneTagIsCheckedOncePerQuery(t *testing.T) {
	tags := &fakeTags{byTable: map[string]map[string][]string{
		"analytics.customers": {
			"email": {piiTag}, "phone": {piiTag}, "address": {piiTag},
		},
	}}
	access := &fakeAccess{allow: map[string]bool{piiTag: true}}

	_, err := resolver(tags, access).CanRead(context.Background(), analyst, []govern.Ref{
		{Origin: "sales.customers", Field: "email", Source: "analytics.customers", Column: "email"},
		{Origin: "sales.customers", Field: "phone", Source: "analytics.customers", Column: "phone"},
		{Origin: "sales.customers", Field: "address", Source: "analytics.customers", Column: "address"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if access.calls != 1 {
		t.Errorf("want 1 IAM check for 3 columns behind one tag, got %d", access.calls)
	}
	if tags.calls != 1 {
		t.Errorf("want 1 schema read for 3 columns in one table, got %d", tags.calls)
	}
}

// TestCapabilitiesAdmitWhatIsEnforced. health prints this, and a resolver that
// makes real column decisions must say so, just as allow-all says it does not.
func TestCapabilitiesAdmitWhatIsEnforced(t *testing.T) {
	c := resolver(&fakeTags{}, &fakeAccess{}).Capabilities()
	if !c.ColumnLevel {
		t.Error("the policy tag resolver makes real column level decisions")
	}
	if c.Resolver == "" || c.Note == "" {
		t.Error("capabilities must name the resolver and explain it")
	}
}

// TestTagResolverSatisfiesPolicyResolver keeps it usable by the gate.
func TestTagResolverSatisfiesPolicyResolver(t *testing.T) {
	var _ govern.PolicyResolver = govern.NewTagResolver(&fakeTags{}, &fakeAccess{})
}
