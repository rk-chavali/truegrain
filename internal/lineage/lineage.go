// Package lineage answers what depends on what, from the model alone.
//
// The question is "if I drop this column, what breaks", and it is the one a
// data engineer asks before every schema change. Until now the honest answer
// here was to grep the YAML, which finds the string and not the meaning: a
// field named `status` appears in four datasets and only one of them is the
// one being dropped.
//
// Nothing in here touches a warehouse or needs a credential. A model already
// declares everything the answer needs. Datasets declare fields, metrics
// reference fields by `dataset.field`, relationships join datasets on named
// columns, and a primary key is a list of field names. The graph is a reverse
// index over facts already parsed, which is why this can run in a pull request
// on a machine with no access to anything.
//
// It deliberately reports structure rather than guessing at consequence. A
// metric that references a dropped field will not compile, and that is a fact.
// Whether the dashboard built on that metric matters to anybody is not
// something a model can know, so this names what depends on what and stops.

package lineage

import (
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
)

// Graph is the reverse index: from a thing, everything that refers to it.
type Graph struct {
	// namespace this was built from, prefixed onto every name it reports so
	// two namespaces with a dataset called orders stay distinguishable.
	namespace string

	fieldMetrics map[string][]string
	fieldJoins   map[string][]string
	fieldKeyOf   map[string][]string
	fieldIsDim   map[string]bool
	datasetHas   map[string][]string
	metricFields map[string][]string
}

// Impact is everything that refers to one field or dataset.
//
// Every list is sorted and qualified. Empty lists are empty rather than nil so
// a caller rendering JSON gets `[]` and not `null`, which is the difference
// between "nothing depends on this" and "this was never computed".
type Impact struct {
	// Subject is what was asked about, qualified.
	Subject string `json:"subject"`
	// Kind is "field" or "dataset".
	Kind string `json:"kind"`
	// Found reports whether the subject exists in the model at all. A typo
	// and a genuinely unused field both produce an empty impact, and only one
	// of them should be reassuring.
	Found bool `json:"found"`
	// Metrics whose expression references this. These stop compiling.
	Metrics []string `json:"metrics"`
	// Relationships that join on this. Removing one of these disconnects
	// datasets, which usually turns safe questions into refused ones rather
	// than into errors.
	Relationships []string `json:"relationships"`
	// PrimaryKeyOf names datasets whose declared grain includes this. This is
	// the worst one: a dataset with no grain has every join into it treated
	// as a fan-out, so sums across it are refused rather than wrong.
	PrimaryKeyOf []string `json:"primary_key_of"`
	// Dimension reports that callers can group by this.
	Dimension bool `json:"dimension"`
	// Fields is what a dataset holds, on a dataset impact.
	Fields []string `json:"fields,omitempty"`
}

// Breaks reports whether the model itself stops working without the subject.
//
// Deliberately excludes Dimension. Nearly every field is groupable, so
// counting that as breakage made the answer yes for almost any field, and a
// gate that fails on everything is one people route around. What is left is
// derivable from the model and unambiguous: a metric stops compiling, a join
// disconnects, a grain is lost.
//
// Whether a caller is grouping by the field today is a real risk and is not
// knowable from the model, so it is reported beside this rather than folded
// into it. Usage data answers that question; the model cannot.
func (i Impact) Breaks() bool {
	return len(i.Metrics) > 0 || len(i.Relationships) > 0 || len(i.PrimaryKeyOf) > 0
}

// Build indexes one namespace's model.
//
// Resolution follows exactly what internal/resolve does when it validates a
// metric: an expression reference is `dataset.field` and anything else is not
// a reference to a field. Matching that matters more than being lenient here,
// because a lineage answer that disagrees with the compiler is worse than no
// lineage at all.
func Build(namespace string, m *osi.Model) *Graph {
	g := &Graph{
		namespace:    namespace,
		fieldMetrics: map[string][]string{},
		fieldJoins:   map[string][]string{},
		fieldKeyOf:   map[string][]string{},
		fieldIsDim:   map[string]bool{},
		datasetHas:   map[string][]string{},
		metricFields: map[string][]string{},
	}
	if m == nil {
		return g
	}

	for _, d := range m.Datasets {
		for _, f := range d.Fields {
			key := fieldKey(d.Name, f.Name)
			g.datasetHas[norm(d.Name)] = append(g.datasetHas[norm(d.Name)], d.Name+"."+f.Name)
			if f.IsDimension {
				g.fieldIsDim[key] = true
			}
		}
		for _, column := range d.PrimaryKey {
			g.fieldKeyOf[fieldKey(d.Name, column)] = append(
				g.fieldKeyOf[fieldKey(d.Name, column)], d.Name)
		}
		// A unique key declares a grain too, and dropping a column out of one
		// has the same consequence as dropping it from the primary key.
		for _, set := range d.UniqueKeys {
			for _, column := range set {
				g.fieldKeyOf[fieldKey(d.Name, column)] = append(
					g.fieldKeyOf[fieldKey(d.Name, column)], d.Name)
			}
		}
	}

	for _, m := range m.Metrics {
		for _, id := range osi.Refs(m.Expression.AST) {
			// Exactly the resolver's rule. An unqualified or over-qualified
			// reference is not a field reference, and the resolver has
			// already refused the model if one exists.
			if len(id.Norm) != 2 {
				continue
			}
			key := id.Norm[0] + "." + id.Norm[1]
			g.fieldMetrics[key] = append(g.fieldMetrics[key], m.Name)
			g.metricFields[norm(m.Name)] = append(g.metricFields[norm(m.Name)],
				id.Parts[0]+"."+id.Parts[1])
		}
	}

	for _, r := range m.Relationships {
		for _, column := range r.FromColumns {
			key := fieldKey(r.From, column)
			g.fieldJoins[key] = append(g.fieldJoins[key], r.Name)
		}
		for _, column := range r.ToColumns {
			key := fieldKey(r.To, column)
			g.fieldJoins[key] = append(g.fieldJoins[key], r.Name)
		}
	}

	for _, index := range []map[string][]string{
		g.fieldMetrics, g.fieldJoins, g.fieldKeyOf, g.datasetHas, g.metricFields,
	} {
		for k := range index {
			index[k] = unique(index[k])
		}
	}
	return g
}

// Field answers what depends on one `dataset.field`.
func (g *Graph) Field(qualified string) Impact {
	parts := strings.Split(qualified, ".")
	if len(parts) != 2 {
		return Impact{Subject: qualified, Kind: "field"}
	}
	key := fieldKey(parts[0], parts[1])
	_, known := g.datasetHas[norm(parts[0])]

	return Impact{
		Subject:       g.qualify(qualified),
		Kind:          "field",
		Found:         known && g.declares(key),
		Metrics:       g.qualifyAll(g.fieldMetrics[key]),
		Relationships: nonNil(g.fieldJoins[key]),
		PrimaryKeyOf:  g.qualifyAll(g.fieldKeyOf[key]),
		Dimension:     g.fieldIsDim[key],
	}
}

// Dataset answers what depends on a whole dataset: the union over its fields,
// which is what "we are dropping this table" actually means.
func (g *Graph) Dataset(name string) Impact {
	fields, known := g.datasetHas[norm(name)]
	out := Impact{
		Subject: g.qualify(name),
		Kind:    "dataset",
		Found:   known,
		Fields:  nonNil(fields),
	}
	var metrics, joins, keys []string
	for _, qualified := range fields {
		each := g.Field(qualified)
		metrics = append(metrics, each.Metrics...)
		joins = append(joins, each.Relationships...)
		keys = append(keys, each.PrimaryKeyOf...)
		out.Dimension = out.Dimension || each.Dimension
	}
	out.Metrics = unique(metrics)
	out.Relationships = unique(joins)
	out.PrimaryKeyOf = unique(keys)
	return out
}

// Metric answers the other direction: what this metric is built from. The
// question before changing a field is what breaks; the question before
// trusting a number is what it reads.
func (g *Graph) Metric(name string) Impact {
	fields, known := g.metricFields[norm(name)]
	return Impact{
		Subject: g.qualify(name),
		Kind:    "metric",
		Found:   known,
		Fields:  g.qualifyAll(fields),
		Metrics: []string{},
		// A metric depends on the joins its fields are keyed by, which is what
		// makes a relationship change able to move a number.
		Relationships: g.joinsFor(fields),
		PrimaryKeyOf:  []string{},
	}
}

func (g *Graph) joinsFor(fields []string) []string {
	var out []string
	for _, qualified := range fields {
		parts := strings.Split(qualified, ".")
		if len(parts) != 2 {
			continue
		}
		out = append(out, g.fieldJoins[fieldKey(parts[0], parts[1])]...)
	}
	return unique(out)
}

// declares reports whether the model actually has this field, so a typo is
// distinguishable from something genuinely unreferenced.
func (g *Graph) declares(key string) bool {
	dataset := strings.SplitN(key, ".", 2)[0]
	for _, qualified := range g.datasetHas[dataset] {
		if fieldKey(strings.SplitN(qualified, ".", 2)[0],
			strings.SplitN(qualified, ".", 2)[1]) == key {
			return true
		}
	}
	return false
}

func (g *Graph) qualify(name string) string {
	if g.namespace == "" || strings.HasPrefix(name, g.namespace+".") {
		return name
	}
	return g.namespace + "." + name
}

func (g *Graph) qualifyAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, g.qualify(n))
	}
	sort.Strings(out)
	return out
}

func fieldKey(dataset, field string) string { return norm(dataset) + "." + norm(field) }

// norm matches the spec's identifier normalization for unquoted identifiers,
// which is what the resolver indexes by.
func norm(s string) string { return osi.NormalizeIdent(s, false) }

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
