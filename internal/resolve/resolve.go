// Package resolve turns a parsed Ossie model into a validated schema with a
// join graph, and answers the questions the planner asks of it: which datasets
// a metric needs, how to get from one dataset to another, and whether that
// traversal duplicates rows.
package resolve

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
)

// MaxJoinHops caps the length of a join path. The v1 subset in
// docs/03-semantic-model.md excludes entity chains beyond two joins, and the
// cap is what makes exhaustive path enumeration cheap enough to do eagerly.
const MaxJoinHops = 2

// Edge is one relationship in the join graph, usable in either direction.
type Edge struct {
	Rel  *osi.Relationship
	From *osi.Dataset // the spec's many side
	To   *osi.Dataset // the spec's one side
}

// Other returns the dataset at the far end of the edge from d.
func (e *Edge) Other(d *osi.Dataset) *osi.Dataset {
	if d == e.From {
		return e.To
	}
	return e.From
}

// Duplicates reports whether traversing this edge away from base multiplies the
// rows accumulated so far.
//
// This is the whole fan-out question, and the spec answers it without a
// cardinality field. Traversal into a dataset duplicates rows unless the join
// columns on the far side are covered by a declared primary or unique key.
// Walking many-to-one (the spec's from side to its to side) is safe when
// to_columns are a declared key; walking the same edge backwards is one-to-many
// and fans out unless from_columns happen to be unique too.
func (e *Edge) Duplicates(base *osi.Dataset) bool {
	if base == e.From {
		return !e.To.HasUniquenessOn(e.Rel.ToColumns)
	}
	return !e.From.HasUniquenessOn(e.Rel.FromColumns)
}

// ColumnsFrom returns the join columns on the near and far side of a traversal
// that starts at base, in corresponding order.
func (e *Edge) ColumnsFrom(base *osi.Dataset) (near, far []string) {
	if base == e.From {
		return e.Rel.FromColumns, e.Rel.ToColumns
	}
	return e.Rel.ToColumns, e.Rel.FromColumns
}

// Path is an ordered join traversal starting at some base dataset.
type Path []*Edge

// Datasets returns the datasets visited by the path, base first.
func (p Path) Datasets(base *osi.Dataset) []*osi.Dataset {
	out := []*osi.Dataset{base}
	cur := base
	for _, e := range p {
		cur = e.Other(cur)
		out = append(out, cur)
	}
	return out
}

// Duplicates reports whether any hop in the path fans out, and names the first
// relationship that does.
func (p Path) Duplicates(base *osi.Dataset) (bool, *osi.Relationship) {
	cur := base
	for _, e := range p {
		if e.Duplicates(cur) {
			return true, e.Rel
		}
		cur = e.Other(cur)
	}
	return false, nil
}

func (p Path) String() string {
	names := make([]string, len(p))
	for i, e := range p {
		names[i] = e.Rel.Name
	}
	return strings.Join(names, " then ")
}

// Schema is a validated model plus its join graph.
type Schema struct {
	Model *osi.Model

	datasets map[string]*osi.Dataset // normalized dataset name
	fields   map[string]*osi.Field   // normalized "dataset.field"
	metrics  map[string]*osi.Metric  // normalized metric name
	edges    []*Edge
	adj      map[*osi.Dataset][]*Edge

	// metricDatasets is the set of datasets each metric's expression touches,
	// derived from the identifiers in the parsed expression.
	metricDatasets map[*osi.Metric][]*osi.Dataset
}

// New validates a model and builds its join graph. All problems are reported
// together rather than one per run.
func New(m *osi.Model) (*Schema, error) {
	s := &Schema{
		Model:          m,
		datasets:       map[string]*osi.Dataset{},
		fields:         map[string]*osi.Field{},
		metrics:        map[string]*osi.Metric{},
		adj:            map[*osi.Dataset][]*Edge{},
		metricDatasets: map[*osi.Metric][]*osi.Dataset{},
	}
	var ds osi.Diags

	ds = append(ds, s.indexDatasets()...)
	ds = append(ds, s.indexMetrics()...)
	ds = append(ds, s.buildEdges()...)
	ds = append(ds, s.resolveFieldExpressions()...)
	ds = append(ds, s.resolveMetricExpressions()...)
	ds = append(ds, s.checkAmbiguity()...)

	if err := ds.Sorted().ErrOrNil(); err != nil {
		return nil, err
	}
	return s, nil
}

func norm(s string) string { return strings.ToUpper(s) }

func fieldKey(dataset, field string) string { return norm(dataset) + "." + norm(field) }

func (s *Schema) indexDatasets() osi.Diags {
	var ds osi.Diags
	if len(s.Model.Datasets) == 0 {
		ds = append(ds, osi.Diag{Pos: s.Model.Pos, Msg: "model declares no datasets",
			Hint: "a semantic model needs at least one `datasets:` entry"})
	}
	for _, d := range s.Model.Datasets {
		subj := "dataset " + d.Name
		if d.Name == "" {
			ds = append(ds, osi.Diag{Pos: d.Pos, Msg: "dataset has no name"})
			continue
		}
		if prev, dup := s.datasets[norm(d.Name)]; dup {
			ds = append(ds, osi.Diag{Pos: d.Pos, Subject: subj,
				Msg:  fmt.Sprintf("duplicate dataset name, already defined at %s", prev.Pos),
				Hint: "dataset names are case-insensitive; rename one of them"})
			continue
		}
		s.datasets[norm(d.Name)] = d

		if strings.TrimSpace(d.Source) == "" {
			ds = append(ds, osi.Diag{Pos: d.Pos, Subject: subj, Msg: "no `source`",
				Hint: "set source to database.schema.table or an inline query"})
		}
		if len(d.PrimaryKey) == 0 && len(d.UniqueKeys) == 0 {
			// Not fatal on its own, but every join into this dataset will be
			// treated as fanning out, so say why now rather than at query time.
			ds = append(ds, osi.Diag{Pos: d.Pos, Subject: subj,
				Msg:  "no `primary_key` and no `unique_keys`, so its grain is undeclared",
				Hint: "the engine reads primary_key as the grain; without it every join into this dataset is assumed to fan out and sums across it will be refused"})
		}
		seen := map[string]*osi.Field{}
		for _, f := range d.Fields {
			if f.Name == "" {
				ds = append(ds, osi.Diag{Pos: f.Pos, Subject: subj, Msg: "field has no name"})
				continue
			}
			if prev, dup := seen[norm(f.Name)]; dup {
				ds = append(ds, osi.Diag{Pos: f.Pos, Subject: subj + "." + f.Name,
					Msg: fmt.Sprintf("duplicate field name, already defined at %s", prev.Pos)})
				continue
			}
			seen[norm(f.Name)] = f
			s.fields[fieldKey(d.Name, f.Name)] = f
		}
	}
	return ds
}

func (s *Schema) indexMetrics() osi.Diags {
	var ds osi.Diags
	for _, m := range s.Model.Metrics {
		if m.Name == "" {
			ds = append(ds, osi.Diag{Pos: m.Pos, Msg: "metric has no name"})
			continue
		}
		if prev, dup := s.metrics[norm(m.Name)]; dup {
			ds = append(ds, osi.Diag{Pos: m.Pos, Subject: "metric " + m.Name,
				Msg:  fmt.Sprintf("duplicate metric name, already defined at %s", prev.Pos),
				Hint: "a metric name is a contract; define it once"})
			continue
		}
		s.metrics[norm(m.Name)] = m
		if strings.TrimSpace(m.Description) == "" {
			ds = append(ds, osi.Diag{Pos: m.Pos, Subject: "metric " + m.Name,
				Msg:  "no description",
				Hint: "an agent reads the description to decide whether this metric answers the question; an undescribed metric gets picked wrongly or not at all"})
		}
	}
	return ds
}

func (s *Schema) buildEdges() osi.Diags {
	var ds osi.Diags
	seenNames := map[string]*osi.Relationship{}
	for _, r := range s.Model.Relationships {
		subj := "relationship " + r.Name
		if r.Name == "" {
			ds = append(ds, osi.Diag{Pos: r.Pos, Msg: "relationship has no name"})
			continue
		}
		if prev, dup := seenNames[norm(r.Name)]; dup {
			ds = append(ds, osi.Diag{Pos: r.Pos, Subject: subj,
				Msg: fmt.Sprintf("duplicate relationship name, already defined at %s", prev.Pos)})
			continue
		}
		seenNames[norm(r.Name)] = r

		from, okFrom := s.datasets[norm(r.From)]
		if !okFrom {
			ds = append(ds, osi.Diag{Pos: r.Pos, Subject: subj,
				Msg: fmt.Sprintf("`from` names an unknown dataset %q", r.From)})
		}
		to, okTo := s.datasets[norm(r.To)]
		if !okTo {
			ds = append(ds, osi.Diag{Pos: r.Pos, Subject: subj,
				Msg: fmt.Sprintf("`to` names an unknown dataset %q", r.To)})
		}
		if !okFrom || !okTo {
			continue
		}
		if len(r.FromColumns) == 0 || len(r.ToColumns) == 0 {
			ds = append(ds, osi.Diag{Pos: r.Pos, Subject: subj,
				Msg: "from_columns and to_columns are both required"})
			continue
		}
		if len(r.FromColumns) != len(r.ToColumns) {
			ds = append(ds, osi.Diag{Pos: r.Pos, Subject: subj,
				Msg: fmt.Sprintf("from_columns has %d columns but to_columns has %d",
					len(r.FromColumns), len(r.ToColumns)),
				Hint: "the two lists are positional and must be the same length"})
			continue
		}
		if from == to {
			ds = append(ds, osi.Diag{Pos: r.Pos, Subject: subj,
				Msg:  "self-join: `from` and `to` name the same dataset",
				Hint: "v1 does not plan self-joins"})
			continue
		}

		e := &Edge{Rel: r, From: from, To: to}
		s.edges = append(s.edges, e)
		s.adj[from] = append(s.adj[from], e)
		s.adj[to] = append(s.adj[to], e)
	}
	return ds
}

// resolveFieldExpressions checks that a field expression only references
// columns of its own dataset. Field expressions address physical columns of the
// dataset's source, which the engine cannot verify without warehouse metadata,
// so the check is confined to qualification: a field may not reach into another
// dataset.
func (s *Schema) resolveFieldExpressions() osi.Diags {
	var ds osi.Diags
	for _, d := range s.Model.Datasets {
		for _, f := range d.Fields {
			if f.Expression.AST == nil {
				continue
			}
			subj := fmt.Sprintf("field %s.%s", d.Name, f.Name)
			for _, id := range osi.Refs(f.Expression.AST) {
				switch len(id.Norm) {
				case 1:
					// A bare column of this dataset's source table.
				case 2:
					if id.Norm[0] != norm(d.Name) {
						ds = append(ds, osi.Diag{Pos: f.Pos, Subject: subj,
							Msg:  fmt.Sprintf("expression references %q, which belongs to another dataset", id.String()),
							Hint: "a field expression may only reference columns of its own dataset; move a cross-dataset calculation into a metric"})
					}
				default:
					ds = append(ds, osi.Diag{Pos: f.Pos, Subject: subj,
						Msg:  fmt.Sprintf("reference %q has %d parts", id.String(), len(id.Norm)),
						Hint: "field references are `column` or `dataset.column`"})
				}
			}
			if w := osi.WindowCalls(f.Expression.AST); len(w) > 0 {
				ds = append(ds, osi.Diag{Pos: f.Pos, Subject: subj,
					Msg:  fmt.Sprintf("window function %s(...) OVER is not supported in v1", w[0].Name),
					Hint: "see internal/osi/COMPLIANCE.md; window functions are planned for v2"})
			}
			if aggs := osi.Aggregates(f.Expression.AST); len(aggs) > 0 {
				ds = append(ds, osi.Diag{Pos: f.Pos, Subject: subj,
					Msg:  fmt.Sprintf("field expression contains the aggregate %s(...)", aggs[0].Name),
					Hint: "fields are row-level; define the aggregate as a metric instead"})
			}
		}
	}
	return ds
}

// resolveMetricExpressions checks every reference in a metric resolves to a
// declared field, and records which datasets the metric needs.
func (s *Schema) resolveMetricExpressions() osi.Diags {
	var ds osi.Diags
	for _, m := range s.Model.Metrics {
		if m.Expression.AST == nil {
			continue
		}
		subj := "metric " + m.Name
		need := map[*osi.Dataset]bool{}

		for _, id := range osi.Refs(m.Expression.AST) {
			if len(id.Norm) != 2 {
				ds = append(ds, osi.Diag{Pos: m.Pos, Subject: subj,
					Msg:  fmt.Sprintf("reference %q is not a `dataset.field` reference", id.String()),
					Hint: s.unqualifiedHint(id)})
				continue
			}
			d, ok := s.datasets[id.Norm[0]]
			if !ok {
				ds = append(ds, osi.Diag{Pos: m.Pos, Subject: subj,
					Msg: fmt.Sprintf("reference %q names an unknown dataset %q", id.String(), id.Parts[0])})
				continue
			}
			if _, ok := s.fields[fieldKey(id.Norm[0], id.Norm[1])]; !ok {
				ds = append(ds, osi.Diag{Pos: m.Pos, Subject: subj,
					Msg:  fmt.Sprintf("dataset %s has no field %q", d.Name, id.Parts[1]),
					Hint: "metric expressions address declared fields, not physical columns"})
				continue
			}
			need[d] = true
		}

		aggs := osi.Aggregates(m.Expression.AST)
		if len(aggs) == 0 {
			ds = append(ds, osi.Diag{Pos: m.Pos, Subject: subj,
				Msg:  "expression contains no aggregate function",
				Hint: "a metric aggregates; if this is a row-level calculation, declare it as a field"})
		}
		for _, a := range aggs {
			if osi.HasAggregateInside(a) {
				ds = append(ds, osi.Diag{Pos: m.Pos, Subject: subj,
					Msg:  fmt.Sprintf("%s(...) contains another aggregate", a.Name),
					Hint: "nested aggregates are not valid SQL; compute the inner aggregate as its own metric"})
			}
		}
		if w := osi.WindowCalls(m.Expression.AST); len(w) > 0 {
			ds = append(ds, osi.Diag{Pos: m.Pos, Subject: subj,
				Msg:  fmt.Sprintf("window function %s(...) OVER is not supported in v1", w[0].Name),
				Hint: "cumulative and conversion metrics are v2; see internal/osi/COMPLIANCE.md"})
		}

		s.metricDatasets[m] = sortDatasets(need)
	}
	return ds
}

// unqualifiedHint produces the most useful next step for a reference that is
// not `dataset.field`.
func (s *Schema) unqualifiedHint(id *osi.Ident) string {
	if len(id.Norm) == 1 {
		if _, isMetric := s.metrics[id.Norm[0]]; isMetric {
			return "referencing another metric is not supported in v1; see internal/osi/COMPLIANCE.md"
		}
		if owners := s.datasetsWithField(id.Norm[0]); len(owners) > 0 {
			names := make([]string, len(owners))
			for i, d := range owners {
				names[i] = d.Name + "." + id.String()
			}
			return "did you mean " + strings.Join(names, " or ") + "?"
		}
	}
	return "metric references must be qualified as `dataset.field`"
}

func (s *Schema) datasetsWithField(normField string) []*osi.Dataset {
	var out []*osi.Dataset
	for _, d := range s.Model.Datasets {
		if _, ok := s.fields[fieldKey(d.Name, normField)]; ok {
			out = append(out, d)
		}
	}
	return out
}

func sortDatasets(set map[*osi.Dataset]bool) []*osi.Dataset {
	out := make([]*osi.Dataset, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// checkAmbiguity rejects a model in which two datasets are reachable by more
// than one distinct join path. The engine never guesses a join path, and the
// spec has nowhere to declare a preferred one, so the model has to be changed.
func (s *Schema) checkAmbiguity() osi.Diags {
	var ds osi.Diags
	all := s.Model.Datasets
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			paths := s.Paths(all[i], all[j])
			if len(paths) < 2 {
				continue
			}
			desc := make([]string, len(paths))
			for k, p := range paths {
				desc[k] = "[" + p.String() + "]"
			}
			sort.Strings(desc)
			ds = append(ds, osi.Diag{Pos: s.Model.Pos,
				Subject: fmt.Sprintf("join path %s to %s", all[i].Name, all[j].Name),
				Msg:     fmt.Sprintf("%d join paths exist: %s", len(paths), strings.Join(desc, ", ")),
				Hint:    "the engine will not guess; remove a relationship or split the model so only one path connects these datasets"})
		}
	}
	return ds
}

// Paths enumerates every simple join path between two datasets, up to
// MaxJoinHops edges.
func (s *Schema) Paths(from, to *osi.Dataset) []Path {
	if from == to {
		return []Path{{}}
	}
	var out []Path
	visited := map[*osi.Dataset]bool{from: true}
	var walk func(cur *osi.Dataset, acc Path)
	walk = func(cur *osi.Dataset, acc Path) {
		if len(acc) >= MaxJoinHops {
			return
		}
		for _, e := range s.adj[cur] {
			nxt := e.Other(cur)
			if visited[nxt] {
				continue
			}
			step := append(append(Path{}, acc...), e)
			if nxt == to {
				out = append(out, step)
				continue
			}
			visited[nxt] = true
			walk(nxt, step)
			delete(visited, nxt)
		}
	}
	walk(from, nil)
	return out
}

// Dataset looks up a dataset by name, case-insensitively.
func (s *Schema) Dataset(name string) (*osi.Dataset, bool) {
	d, ok := s.datasets[norm(name)]
	return d, ok
}

// Metric looks up a metric by name, case-insensitively.
func (s *Schema) Metric(name string) (*osi.Metric, bool) {
	m, ok := s.metrics[norm(name)]
	return m, ok
}

// Field looks up a field by its `dataset.field` name.
func (s *Schema) Field(qualified string) (*osi.Field, bool) {
	parts := strings.SplitN(qualified, ".", 2)
	if len(parts) != 2 {
		return nil, false
	}
	f, ok := s.fields[fieldKey(parts[0], parts[1])]
	return f, ok
}

// MetricDatasets returns the datasets a metric's expression touches.
func (s *Schema) MetricDatasets(m *osi.Metric) []*osi.Dataset { return s.metricDatasets[m] }

// Dimensions returns every field usable for grouping, in dataset then
// declaration order.
func (s *Schema) Dimensions() []*osi.Field {
	var out []*osi.Field
	for _, d := range s.Model.Datasets {
		for _, f := range d.Fields {
			if f.IsDimension {
				out = append(out, f)
			}
		}
	}
	return out
}

// DimensionsFor returns the dimensions reachable from the datasets a metric
// needs, which is what an interface offers a caller for that metric.
func (s *Schema) DimensionsFor(m *osi.Metric) []*osi.Field {
	reachable := map[*osi.Dataset]bool{}
	for _, d := range s.metricDatasets[m] {
		reachable[d] = true
		for _, other := range s.Model.Datasets {
			if other != d && len(s.Paths(d, other)) == 1 {
				reachable[other] = true
			}
		}
	}
	var out []*osi.Field
	for _, d := range s.Model.Datasets {
		if !reachable[d] {
			continue
		}
		for _, f := range d.Fields {
			if f.IsDimension {
				out = append(out, f)
			}
		}
	}
	return out
}
