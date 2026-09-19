package govern

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Policy tags are how BigQuery actually does column level security.
//
// The file resolver governs queries that go through this engine and places no
// control on the warehouse: anyone with a console and the same credentials
// reads everything. It also asks a customer to maintain a second copy of their
// access rules in our format, which will drift from the real one.
//
// This resolver reads the tags already attached to columns and asks the
// warehouse's own IAM whether the caller may read under them. The engine then
// enforces the same rule BigQuery enforces, before the query is sent, with a
// refusal that names what to ask for. Nothing has to be restated in our format.
//
// The lookups are split behind two small interfaces so the decision logic here
// is testable without a GCP project. The concrete implementations live in
// internal/gcp.

// ColumnTags reports the policy tags attached to a table's columns.
//
// The key of the returned map is the physical column name; a column absent from
// the map carries no tag. An implementation reads this from the table schema.
type ColumnTags interface {
	// TagsFor returns column name to policy tag resource names, for one table
	// addressed the way the model wrote it, for example `analytics.orders`.
	TagsFor(ctx context.Context, source string) (map[string][]string, error)
}

// TagAccess reports whether an identity may read data classified under a tag.
//
// An implementation must answer for the identity passed in, not for the
// credentials this process holds. Evaluating the tag's IAM policy locally is
// the wrong way to do it: that misses bindings inherited from the project,
// folder and organization, conditional bindings, and group expansion, so it
// reports access the warehouse would refuse and refuses access it would allow.
type TagAccess interface {
	CanReadTag(ctx context.Context, id Identity, policyTag string) (bool, error)
}

// DefaultSchemaTTL bounds how long a table's tag layout is reused. Schemas
// change rarely, and a stale entry is corrected within this window.
const DefaultSchemaTTL = 10 * time.Minute

// TagResolver resolves access from BigQuery policy tags.
type TagResolver struct {
	tags   ColumnTags
	access TagAccess

	// Access decisions are not cached here. Gate already caches them per
	// identity with its own TTL, and a second cache with a different lifetime
	// would make a revoked grant take the longer of the two to take effect.
	mu     sync.Mutex
	schema map[string]schemaEntry
	ttl    time.Duration
	now    func() time.Time
}

type schemaEntry struct {
	tags    map[string][]string
	fetched time.Time
}

// NewTagResolver builds a resolver over the two lookups.
func NewTagResolver(tags ColumnTags, access TagAccess) *TagResolver {
	return &TagResolver{
		tags:   tags,
		access: access,
		schema: map[string]schemaEntry{},
		ttl:    DefaultSchemaTTL,
		now:    time.Now,
	}
}

// Capabilities states what this resolver enforces.
func (t *TagResolver) Capabilities() Capabilities {
	return Capabilities{
		Resolver:    "bigquery-policy-tags",
		ColumnLevel: true,
		Note: "Column access comes from the BigQuery policy tags already on your " +
			"columns, checked against the caller's own IAM. A column with no policy " +
			"tag is readable, which is what BigQuery itself does.",
	}
}

// CanRead resolves every ref, denying any the identity may not read.
//
// An error from either lookup is returned rather than swallowed, so the gate
// fails closed. Reporting "allowed" because the policy source was unreachable
// is the one outcome that must never happen.
func (t *TagResolver) CanRead(ctx context.Context, id Identity, refs []Ref) (Decision, error) {
	var denied []Ref

	// Decided caches the answer per tag for the span of this one call, so a
	// query touching eight columns under one tag makes one IAM check.
	decided := map[string]bool{}

	for _, ref := range refs {
		required, err := t.tagsRequiredBy(ctx, ref)
		if err != nil {
			return Decision{}, err
		}
		if len(required) == 0 {
			// No policy tag means no column level restriction. This is
			// BigQuery's own semantics, not a default we chose.
			continue
		}

		allowed := true
		for _, tag := range required {
			ok, seen := decided[tag]
			if !seen {
				if ok, err = t.access.CanReadTag(ctx, id, tag); err != nil {
					return Decision{}, fmt.Errorf("checking access to policy tag: %w", err)
				}
				decided[tag] = ok
			}
			if !ok {
				allowed = false
				break
			}
		}
		if !allowed {
			denied = append(denied, ref)
		}
	}

	return Decision{Allowed: len(denied) == 0, Denied: denied}, nil
}

// tagsRequiredBy returns every policy tag a ref's data sits behind.
//
// A ref whose Column is empty is a computed field: the engine could not reduce
// its expression to one column, so there is no way to know which columns it
// reads. It is treated as reading every tagged column in the table, which can
// deny a field that would have been allowed and will never allow one that
// should have been denied.
func (t *TagResolver) tagsRequiredBy(ctx context.Context, ref Ref) ([]string, error) {
	if ref.Source == "" {
		// Nothing to look up and no way to prove the data is unrestricted.
		return nil, fmt.Errorf("field %s has no source table, so its policy tags cannot be resolved", ref.Name())
	}
	columns, err := t.tagsForSource(ctx, ref.Source)
	if err != nil {
		return nil, err
	}

	if ref.Column != "" {
		return columns[ref.Column], nil
	}

	seen := map[string]bool{}
	var all []string
	for _, tags := range columns {
		for _, tag := range tags {
			if !seen[tag] {
				seen[tag] = true
				all = append(all, tag)
			}
		}
	}
	// Deterministic, so a refusal reads the same way twice.
	sort.Strings(all)
	return all, nil
}

// tagsForSource reads a table's tag layout, from cache when it is fresh.
func (t *TagResolver) tagsForSource(ctx context.Context, source string) (map[string][]string, error) {
	t.mu.Lock()
	entry, ok := t.schema[source]
	fresh := ok && t.now().Sub(entry.fetched) < t.ttl
	t.mu.Unlock()

	if fresh {
		return entry.tags, nil
	}

	// Fetched outside the lock: a slow API call must not block every other
	// table's lookup. Two concurrent misses on the same table both fetch,
	// which costs one extra request and avoids holding a lock across the network.
	tags, err := t.tags.TagsFor(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("reading policy tags for %s: %w", source, err)
	}

	t.mu.Lock()
	t.schema[source] = schemaEntry{tags: tags, fetched: t.now()}
	t.mu.Unlock()
	return tags, nil
}
