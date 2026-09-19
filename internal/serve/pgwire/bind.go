package pgwire

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Resolving a parsed statement against the model it claims to query.
//
// This is where a name written by a BI tool becomes a name the engine knows,
// and where a name the engine does not know becomes a refusal naming the
// alternatives. Nothing reaches the planner until every identifier in the
// statement has been matched to something the loaded model declares, which is
// the property that makes the parser safe: an unrecognised string cannot
// travel any further than here.

// namespaceSchema is what one namespace looks like as a SQL table.
type namespaceSchema struct {
	name       string
	metrics    []string
	dimensions []string
}

// schemaOf describes every namespace the caller may see as a virtual table.
//
// The dimension listing is the caller's own: one they may not read is absent
// rather than present-and-refused, because a catalogue is itself a
// disclosure, and because offering a name that will be denied invites a
// question nobody can answer. This is the same rule /v1/dimensions follows.
func schemaOf(ctx context.Context, eng *engine.Engine, id govern.Identity) ([]namespaceSchema, error) {
	byNamespace := map[string]*namespaceSchema{}
	get := func(name string) *namespaceSchema {
		if s, ok := byNamespace[name]; ok {
			return s
		}
		s := &namespaceSchema{name: name}
		byNamespace[name] = s
		return s
	}
	for _, ns := range eng.Workspace().Available() {
		get(ns.Name)
	}

	for _, m := range eng.Metrics() {
		s := get(m.Namespace.Name)
		s.metrics = append(s.metrics, m.Qualified)
	}

	dims, err := eng.VisibleDimensions(ctx, id, nil)
	if err != nil {
		return nil, err
	}
	for _, d := range dims {
		s := get(d.Namespace.Name)
		s.dimensions = append(s.dimensions, d.Qualified)
	}

	out := make([]namespaceSchema, 0, len(byNamespace))
	for _, s := range byNamespace {
		sort.Strings(s.metrics)
		sort.Strings(s.dimensions)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// bound is a statement resolved against the model.
type bound struct {
	request plan.Request
	// columns is the output column order the caller asked for, which is not
	// the order the engine returns. A BI tool reading positionally would show
	// the wrong values under the wrong headers, so the rows are reordered
	// before they go out.
	columns []string
	// sources maps each output column to the engine field it comes from.
	sources []string
	// measure records which output columns are metrics.
	//
	// The type a client is told has to come from the model, not from the value
	// that arrived. Executors disagree: the DuckDB CLI hands back every number
	// as a string, pgx hands back a typed value, and inferring from the value
	// meant the same metric was numeric on Postgres and text on DuckDB. A
	// client told text renders a measure as a label and will not plot it, so
	// that difference was the same dashboard working on one warehouse and
	// silently producing a category axis on another.
	measure []bool
}

// bindQuery resolves names and produces the request to run.
func bindQuery(q *query, schemas []namespaceSchema) (*bound, error) {
	ns, err := findNamespace(q.from, schemas)
	if err != nil {
		return nil, err
	}

	b := &bound{}
	expanded, err := expandStars(q.fields, ns)
	if err != nil {
		return nil, err
	}

	for _, f := range expanded {
		kind, canonical := classify(f.name, ns)
		switch kind {
		case isMetric:
			b.request.Metrics = append(b.request.Metrics, canonical)
		case isDimension:
			b.request.Dimensions = append(b.request.Dimensions, canonical)
		default:
			return nil, unknownName(f.name, ns)
		}
		b.columns = append(b.columns, f.alias)
		b.sources = append(b.sources, aliasOf(f.name))
		b.measure = append(b.measure, kind == isMetric)
	}

	if len(b.request.Metrics) == 0 {
		return nil, fmt.Errorf(
			"this query selects no metric. A semantic layer answers questions "+
				"about measures; selecting only dimensions would be a list of rows, "+
				"which is what the warehouse is for. Metrics in %s: %s",
			ns.name, preview(ns.metrics))
	}

	for _, f := range q.filters {
		kind, canonical := classify(f.Dimension, ns)
		if kind == isMetric {
			return nil, fmt.Errorf(
				"%q is a metric, and a metric cannot be filtered on here: it is "+
					"computed after the rows are chosen. Filter on a dimension instead",
				f.Dimension)
		}
		if kind != isDimension {
			return nil, unknownName(f.Dimension, ns)
		}
		f.Dimension = canonical
		b.request.Filters = append(b.request.Filters, f)
	}

	if err := checkGroupBy(q, expanded, ns); err != nil {
		return nil, err
	}

	for _, o := range q.orderBy {
		field, err := resolveOrder(o.Field, expanded, ns)
		if err != nil {
			return nil, err
		}
		b.request.OrderBy = append(b.request.OrderBy, plan.Order{Field: field, Desc: o.Desc})
	}

	b.request.Limit = q.limit
	return b, nil
}

type nameKind int

const (
	isUnknown nameKind = iota
	isMetric
	isDimension
)

// classify matches a written name to something the model declares.
//
// Matching is case-insensitive because SQL is, and a BI tool will lowercase
// an unquoted identifier before sending it. A qualified dimension may also be
// written with only its trailing parts, so customers.region matches
// retail.customers.region, which is how a tool that treats the namespace as
// the table name spells it.
func classify(name string, ns namespaceSchema) (nameKind, string) {
	for _, m := range ns.metrics {
		if matches(name, m, ns.name) {
			return isMetric, m
		}
	}
	for _, d := range ns.dimensions {
		if matches(name, d, ns.name) {
			return isDimension, d
		}
	}
	return isUnknown, ""
}

func matches(written, declared, namespace string) bool {
	if strings.EqualFold(written, declared) {
		return true
	}
	// Declared as namespace.rest: accept rest on its own.
	if rest, ok := cutPrefixFold(declared, namespace+"."); ok &&
		strings.EqualFold(written, rest) {
		return true
	}
	// Written as namespace.rest against a bare declaration.
	if rest, ok := cutPrefixFold(written, namespace+"."); ok &&
		strings.EqualFold(rest, declared) {
		return true
	}
	return false
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// aliasOf is the output column name the engine uses for a field, which is the
// last dotted part. The engine names result columns this way, so this is how
// a requested field is found in the result.
func aliasOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

func findNamespace(name string, schemas []namespaceSchema) (namespaceSchema, error) {
	// A tool may qualify the table as schema.table. Both halves are accepted
	// when they name the same namespace, and public.x is accepted because
	// that is what a client with no search_path sends.
	bare := name
	if i := strings.LastIndex(name, "."); i >= 0 {
		prefix, rest := name[:i], name[i+1:]
		if strings.EqualFold(prefix, "public") || strings.EqualFold(prefix, rest) {
			bare = rest
		}
	}
	for _, s := range schemas {
		if strings.EqualFold(s.name, bare) || strings.EqualFold(s.name, name) {
			return s, nil
		}
	}
	names := make([]string, len(schemas))
	for i, s := range schemas {
		names[i] = s.name
	}
	if len(names) == 0 {
		return namespaceSchema{}, fmt.Errorf(
			"no namespace is available to this caller")
	}
	return namespaceSchema{}, fmt.Errorf(
		"%q is not a namespace in this workspace. Available: %s",
		name, strings.Join(names, ", "))
}

func expandStars(fields []field, ns namespaceSchema) ([]field, error) {
	var out []field
	for _, f := range fields {
		if !f.star {
			out = append(out, f)
			continue
		}
		// SELECT * on a semantic model is every metric at once, which is
		// almost always a fan-out refusal and never what the caller meant.
		// Refusing with the list is more useful than returning it.
		return nil, fmt.Errorf(
			"SELECT * is not supported: a namespace is not a table, and every "+
				"metric in it at once is usually a question with no correct answer. "+
				"Name what you want.\n  metrics: %s\n  dimensions: %s",
			preview(ns.metrics), preview(ns.dimensions))
	}
	return out, nil
}

// checkGroupBy reports a GROUP BY that disagrees with the select list.
//
// The grouping is implied: a semantic result is grouped by exactly the
// dimensions selected. A caller who wrote something else has misunderstood
// what they will get back, and quietly ignoring it would hand them a number
// grouped differently from the one they asked for.
func checkGroupBy(q *query, fields []field, ns namespaceSchema) error {
	if len(q.groupBy) == 0 {
		return nil
	}

	wanted := map[string]bool{}
	for _, f := range fields {
		if kind, canonical := classify(f.name, ns); kind == isDimension {
			wanted[strings.ToLower(canonical)] = true
		}
	}

	grouped := map[string]bool{}
	for _, g := range q.groupBy {
		// Positional: GROUP BY 1 refers to the first select item.
		if n, err := strconv.Atoi(g); err == nil {
			if n < 1 || n > len(fields) {
				return fmt.Errorf("GROUP BY %d refers to a column that is not selected", n)
			}
			g = fields[n-1].name
		}
		kind, canonical := classify(g, ns)
		if kind == isMetric {
			return fmt.Errorf(
				"%q is a metric and cannot be grouped by: it is the thing being "+
					"aggregated", g)
		}
		if kind != isDimension {
			return unknownName(g, ns)
		}
		grouped[strings.ToLower(canonical)] = true
	}

	var missing []string
	for d := range wanted {
		if !grouped[d] {
			missing = append(missing, d)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf(
			"every selected dimension has to appear in GROUP BY, and %s does not. "+
				"The result is grouped by the dimensions you select, so dropping "+
				"GROUP BY entirely also works", strings.Join(missing, ", "))
	}
	return nil
}

func resolveOrder(written string, fields []field, ns namespaceSchema) (string, error) {
	if n, err := strconv.Atoi(written); err == nil {
		if n < 1 || n > len(fields) {
			return "", fmt.Errorf("ORDER BY %d refers to a column that is not selected", n)
		}
		return aliasOf(fields[n-1].name), nil
	}
	// An alias from the select list takes precedence, which is what SQL does.
	for _, f := range fields {
		if strings.EqualFold(f.alias, written) {
			return aliasOf(f.name), nil
		}
	}
	if kind, canonical := classify(written, ns); kind != isUnknown {
		return aliasOf(canonical), nil
	}
	return "", fmt.Errorf(
		"cannot sort by %q: it is neither a selected column nor a field of %s",
		written, ns.name)
}

func unknownName(name string, ns namespaceSchema) error {
	if near := closest(name, append(append([]string{}, ns.metrics...), ns.dimensions...)); near != "" {
		return fmt.Errorf("%q is not in %s. Did you mean %q?", name, ns.name, near)
	}
	return fmt.Errorf(
		"%q is not a metric or dimension of %s.\n  metrics: %s\n  dimensions: %s",
		name, ns.name, preview(ns.metrics), preview(ns.dimensions))
}

// closest finds a name differing only by qualification or case, which covers
// the mistakes people actually make. It is not a spelling corrector: a guess
// at what somebody meant is worse than the full list when it is wrong.
func closest(name string, candidates []string) string {
	bare := strings.ToLower(aliasOf(name))
	for _, c := range candidates {
		if strings.ToLower(aliasOf(c)) == bare {
			return c
		}
	}
	return ""
}

func preview(names []string) string {
	const max = 12
	if len(names) == 0 {
		return "none"
	}
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") +
		fmt.Sprintf(", and %d more", len(names)-max)
}
