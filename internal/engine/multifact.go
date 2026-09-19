package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/dialect"
	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// Multi-fact planning.
//
// A query whose metrics aggregate facts at different grains cannot be answered
// by one flat join: joining the order header to its lines repeats the header
// once per line, so a sum over it inflates. The fix `docs/03-semantic-model.md`
// prescribes is to aggregate each fact separately at the shared dimension grain
// and join the grouped results, which is what this file arranges.
//
// The same arrangement answers a query whose metrics live in different
// namespaces, because each part carries its own schema and planner. Two parts
// share a dimension when their fields have the same governed name, which is
// exactly the identity an imported dataset preserves.

// factGroup is a set of metrics that can be aggregated in one pass: same
// namespace, same set of underlying datasets.
type factGroup struct {
	ns *workspace.Namespace
	// key is what makes two metrics groupable.
	key string
	// metrics are bare names, as the namespace's own planner expects.
	metrics []string
	// labels are the qualified names, for diagnostics.
	labels []string
}

// groupMetrics resolves every requested metric and buckets them into facts.
//
// Grouping is by namespace and by the exact set of datasets a metric
// aggregates. Two metrics over the same datasets always belong together;
// metrics over different datasets get their own part. That can produce one more
// part than strictly necessary when one metric's datasets are a subset of
// another's, which costs a redundant subquery and never a wrong answer.
func (e *Engine) groupMetrics(names []string) ([]*factGroup, error) {
	var order []*factGroup
	byKey := map[string]*factGroup{}
	seen := map[string]bool{}

	for _, name := range names {
		ref, candidates, ok := e.ws.FindMetric(name)
		if !ok {
			if len(candidates) > 0 {
				return nil, &plan.Refusal{Code: plan.CodeUnknownMetric, Subject: name,
					Reason: fmt.Sprintf("%q exists in more than one namespace", name),
					Hint:   "qualify it as one of: " + strings.Join(candidates, ", ")}
			}
			return nil, &plan.Refusal{Code: plan.CodeUnknownMetric, Subject: name,
				Reason: fmt.Sprintf("no metric named %q", name),
				Hint:   suggest(name, e.ws.MetricNames())}
		}
		if seen[ref.Qualified] {
			continue // asking twice is harmless; answer once
		}
		seen[ref.Qualified] = true

		key := ref.Namespace.Name + "|" + datasetSignature(ref.Namespace, ref.Metric)
		g, ok := byKey[key]
		if !ok {
			g = &factGroup{ns: ref.Namespace, key: key}
			byKey[key] = g
			order = append(order, g)
		}
		g.metrics = append(g.metrics, ref.Metric.Name)
		g.labels = append(g.labels, ref.Qualified)
	}
	return order, nil
}

// datasetSignature is the sorted set of datasets a metric's expression
// aggregates, which is what decides whether two metrics can share a pass.
func datasetSignature(ns *workspace.Namespace, m *osi.Metric) string {
	seen := map[string]bool{}
	for _, id := range osi.Refs(m.Expression.AST) {
		if len(id.Norm) != 2 {
			continue
		}
		if f, ok := ns.Schema.Field(id.Parts[0] + "." + id.Parts[1]); ok {
			seen[f.Dataset.Name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// sharedDimension is one requested dimension, resolved to the governed name
// that identifies it in every namespace.
type sharedDimension struct {
	requested string
	governed  string
}

// resolveShared turns requested dimension names into governed ones, refusing a
// name that is unknown or ambiguous.
func (e *Engine) resolveShared(names []string, what string) ([]sharedDimension, error) {
	out := make([]sharedDimension, 0, len(names))
	for _, name := range names {
		governed, candidates, ok := e.ws.ResolveDimension(name)
		if !ok {
			if len(candidates) > 0 {
				return nil, &plan.Refusal{Code: plan.CodeUnknownDimension, Subject: name,
					Reason: fmt.Sprintf("%q exists in more than one namespace", name),
					Hint:   "qualify it as one of: " + strings.Join(candidates, ", ")}
			}
			return nil, &plan.Refusal{Code: plan.CodeUnknownDimension, Subject: name,
				Reason: fmt.Sprintf("no %s named %q", what, name),
				Hint:   suggest(name, e.ws.DimensionNames())}
		}
		out = append(out, sharedDimension{requested: name, governed: governed})
	}
	return out, nil
}

// localise rewrites governed dimension names into one namespace's vocabulary,
// refusing when that namespace cannot reach the column at all.
func localise(ns *workspace.Namespace, dims []sharedDimension) ([]string, error) {
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		f, ok := ns.FieldByGoverned(d.governed)
		if !ok {
			owner, _, _ := strings.Cut(d.governed, ".")
			return nil, &plan.Refusal{Code: CodeCrossNamespace, Subject: d.requested,
				Reason: fmt.Sprintf(
					"namespace %q cannot group by %q, which belongs to %q",
					ns.Name, d.requested, owner),
				Hint: fmt.Sprintf(
					"add the dataset to `imports` in %s's namespace.yaml, and to `exports` in %s's, or drop this dimension",
					ns.Name, owner)}
		}
		if !f.IsDimension {
			return nil, &plan.Refusal{Code: plan.CodeUnknownDimension, Subject: d.requested,
				Reason: fmt.Sprintf("%q is not usable for grouping", d.requested),
				Hint:   "the field has no `dimension:` block in the model"}
		}
		out = append(out, f.QualifiedName())
	}
	return out, nil
}

// buildParts plans one aggregation per fact group.
func (e *Engine) buildParts(ctx context.Context, groups []*factGroup, dims []sharedDimension,
	filters []plan.Filter, grain plan.Grain) ([]dialect.Part, error) {

	// Filters are resolved per namespace the same way dimensions are, so a
	// filter on a shared column restricts every part rather than only the one
	// that happens to own it.
	filterNames := make([]string, len(filters))
	for i, f := range filters {
		filterNames[i] = f.Dimension
	}
	sharedFilters, err := e.resolveShared(filterNames, "dimension")
	if err != nil {
		return nil, err
	}

	parts := make([]dialect.Part, 0, len(groups))
	for _, g := range groups {
		localDims, err := localise(g.ns, dims)
		if err != nil {
			return nil, err
		}
		localFilterDims, err := localise(g.ns, sharedFilters)
		if err != nil {
			return nil, err
		}
		localFilters := make([]plan.Filter, len(filters))
		for i, f := range filters {
			f.Dimension = localFilterDims[i]
			localFilters[i] = f
		}

		req := plan.Request{
			Metrics:    g.metrics,
			Dimensions: localDims,
			Filters:    localFilters,
			Grain:      grain,
		}
		planner := e.planners[strings.ToUpper(g.ns.Name)]

		// Which rollups could answer this is a question about the model;
		// which of those may be read is a question about the warehouse.
		// The planner answers the first and knows nothing about the
		// second, so the freshness check happens here, between them.
		// usableRollups returns nothing when there is no executor, so a
		// compile-only engine plans against the base fact exactly as it
		// always has.
		fresh := e.usableRollups(ctx, planner.CandidateRollups(req))

		p, err := planner.PlanWith(req, fresh)
		if err != nil {
			return nil, annotatePart(err, g, len(groups))
		}
		p.Namespace = g.ns.Name
		p.ModelVersion = e.ws.Digest
		parts = append(parts, dialect.Part{Schema: g.ns.Schema, Plan: p, Namespace: g.ns.Name})
	}

	if err := checkAliasAgreement(parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// annotatePart makes a per-part refusal readable when there are several parts,
// so the caller learns which metrics caused it.
func annotatePart(err error, g *factGroup, total int) error {
	if total == 1 {
		return err
	}
	r, ok := err.(*plan.Refusal)
	if !ok {
		return err
	}
	clone := *r
	clone.Reason = fmt.Sprintf("%s (while planning %s)", r.Reason, strings.Join(g.labels, ", "))
	return &clone
}

// checkAliasAgreement verifies every part groups by the same output columns in
// the same order, which is what the join between them relies on.
//
// It should be true by construction. It is checked anyway because if it ever
// stops being true the join would silently match the wrong columns, and a
// silently wrong number is the failure this engine exists to prevent.
func checkAliasAgreement(parts []dialect.Part) error {
	if len(parts) < 2 {
		return nil
	}
	want := aliases(parts[0].Plan.Dimensions)
	for _, p := range parts[1:] {
		if got := aliases(p.Plan.Dimensions); !equalStrings(want, got) {
			return &plan.Refusal{Code: CodeCrossNamespace,
				Reason: fmt.Sprintf(
					"namespaces %q and %q name the shared dimensions differently (%v against %v)",
					parts[0].Namespace, p.Namespace, want, got),
				Hint: "this is an engine limitation rather than a model error; qualify the dimensions explicitly, or report it"}
		}
	}
	return nil
}

func aliases(dims []plan.DimSelect) []string {
	out := make([]string, len(dims))
	for i, d := range dims {
		out[i] = d.Alias
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
