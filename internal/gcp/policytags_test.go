package gcp

import (
	"context"
	"testing"

	"cloud.google.com/go/bigquery"

	"github.com/rk-chavali/truegrain/internal/govern"
)

func TestSplitSource(t *testing.T) {
	cases := []struct {
		source                    string
		wantProj, wantDS, wantTbl string
		wantErr                   bool
	}{
		// A two part source takes the engine's configured project, which is
		// what a model written for one project means by it.
		{source: "analytics.orders", wantProj: "default-proj", wantDS: "analytics", wantTbl: "orders"},
		{source: "other-proj.analytics.orders", wantProj: "other-proj", wantDS: "analytics", wantTbl: "orders"},
		// BigQuery writes backticks around a qualified name; a model copied
		// from a console query carries them.
		{source: "`other-proj.analytics.orders`", wantProj: "other-proj", wantDS: "analytics", wantTbl: "orders"},
		{source: "orders", wantErr: true},
		{source: "a.b.c.d", wantErr: true},
		{source: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			p, d, tbl, err := splitSource(c.source, "default-proj")
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q, got %s.%s.%s", c.source, p, d, tbl)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p != c.wantProj || d != c.wantDS || tbl != c.wantTbl {
				t.Errorf("got %s.%s.%s, want %s.%s.%s", p, d, tbl, c.wantProj, c.wantDS, c.wantTbl)
			}
		})
	}
}

// TestCollectTagsOmitsUntaggedColumns. The resolver reads an absent column as
// unrestricted, so an untagged column must not appear with an empty slice.
func TestCollectTagsOmitsUntaggedColumns(t *testing.T) {
	schema := bigquery.Schema{
		{Name: "order_id", Type: bigquery.StringFieldType},
		{Name: "email", Type: bigquery.StringFieldType,
			PolicyTags: &bigquery.PolicyTagList{Names: []string{"projects/p/locations/eu/taxonomies/1/policyTags/pii"}}},
		{Name: "no_tags", Type: bigquery.StringFieldType, PolicyTags: &bigquery.PolicyTagList{}},
	}

	got := map[string][]string{}
	collectTags(schema, "", got)

	if _, present := got["order_id"]; present {
		t.Error("an untagged column must be absent, not present and empty")
	}
	if _, present := got["no_tags"]; present {
		t.Error("a column with an empty tag list must be absent")
	}
	if len(got["email"]) != 1 {
		t.Errorf("want one tag on email, got %v", got["email"])
	}
}

// TestCollectTagsWalksNestedRecords. A tag on a field inside a RECORD restricts
// it just as one at the top level does; missing them would silently allow a
// column BigQuery protects.
func TestCollectTagsWalksNestedRecords(t *testing.T) {
	const tag = "projects/p/locations/eu/taxonomies/1/policyTags/pii"
	schema := bigquery.Schema{
		{
			Name: "customer", Type: bigquery.RecordFieldType,
			Schema: bigquery.Schema{
				{Name: "id", Type: bigquery.StringFieldType},
				{Name: "email", Type: bigquery.StringFieldType,
					PolicyTags: &bigquery.PolicyTagList{Names: []string{tag}}},
			},
		},
	}

	got := map[string][]string{}
	collectTags(schema, "", got)

	if len(got["customer.email"]) != 1 || got["customer.email"][0] != tag {
		t.Errorf("a tag on a nested field must be found under its dotted path, got %v", got)
	}
	if _, present := got["email"]; present {
		t.Error("a nested field must not be recorded under its bare name")
	}
}

// TestAnonymousReadsNothingTagged. Returning true for an empty subject would
// turn every unauthenticated request into a full access request.
func TestAnonymousReadsNothingTagged(t *testing.T) {
	ok, err := NewCatalogAccess().CanReadTag(context.Background(), govern.Identity{},
		"projects/p/locations/eu/taxonomies/1/policyTags/pii")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an unauthenticated caller must not read a tagged column")
	}
}

// TestMalformedTagIsAnError rather than a quiet false: a tag name we cannot
// parse means the schema lookup returned something unexpected, and treating
// that as "denied" would hide the misconfiguration behind a plausible refusal.
func TestMalformedTagIsAnError(t *testing.T) {
	_, err := NewCatalogAccess().CanReadTag(context.Background(),
		govern.Identity{Subject: "a@b.iam.gserviceaccount.com"}, "pii")
	if err == nil {
		t.Fatal("a value that is not a policy tag resource name must be an error")
	}
}

// TestSchemaTagsNeedsAProject keeps a misconfigured deployment from starting.
func TestSchemaTagsNeedsAProject(t *testing.T) {
	if _, err := NewSchemaTags(context.Background(), ""); err == nil {
		t.Fatal("reading schemas without a project must not be possible")
	}
}

// TestImplementsTheResolverInterfaces is what lets these be wired to the gate.
func TestImplementsTheResolverInterfaces(t *testing.T) {
	var _ govern.TagAccess = NewCatalogAccess()
	var _ govern.ColumnTags = (*SchemaTags)(nil)
}
