package workspace

import (
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
)

// This file implements name lookup across the workspace: turning the names a
// caller or a policy file writes into the one object they mean, or refusing
// when they mean more than one.
//
// The rule throughout: a qualified name always resolves, a bare name resolves
// only when it is unambiguous, and ambiguity is an error that names the
// candidates rather than a guess.

// GovernedField canonicalises a field name written in a policy file.
//
// It accepts `namespace.dataset.field` always, and `dataset.field` when that
// resolves to exactly one governed field across the workspace. A dataset
// imported into several namespaces appears under one governed name, so an
// import never makes a policy entry ambiguous.
func (ws *Workspace) GovernedField(name string) (string, []string, bool) {
	parts := strings.Split(name, ".")
	switch len(parts) {
	case 3:
		ns, ok := ws.Namespace(parts[0])
		if !ok || !ns.Available() {
			return "", nil, false
		}
		f, ok := ns.Schema.Field(parts[1] + "." + parts[2])
		if !ok {
			return "", nil, false
		}
		return f.GovernedName(), nil, true

	case 2:
		seen := map[string]bool{}
		for _, ns := range ws.Available() {
			if f, ok := ns.Schema.Field(name); ok {
				seen[f.GovernedName()] = true
			}
		}
		governed := keys(seen)
		if len(governed) == 1 {
			return governed[0], nil, true
		}
		return "", governed, false
	}
	return "", nil, false
}

// GovernedFieldsIn returns every governed field a namespace defines, which is
// how a policy tag protects a whole namespace.
//
// Fields grafted in from another namespace are excluded: they belong to the
// namespace that defined them and are governed there. Including them would let
// a tag on one namespace silently reach into another team's columns.
func (ws *Workspace) GovernedFieldsIn(namespace string) ([]string, bool) {
	ns, ok := ws.Namespace(namespace)
	if !ok {
		return nil, false
	}
	if !ns.Available() {
		return nil, true
	}
	var out []string
	for _, d := range ns.Model.Datasets {
		if d.ImportedFrom != "" {
			continue
		}
		for _, f := range d.Fields {
			out = append(out, f.GovernedName())
		}
	}
	sort.Strings(out)
	return out, true
}

// MetricRef is a metric located in the workspace.
type MetricRef struct {
	Namespace *Namespace
	Metric    *osi.Metric
	// Qualified is the name every interface reports: namespace.metric.
	Qualified string
}

// FindMetric resolves a metric name to exactly one metric.
//
// `namespace.metric` always resolves. A bare `metric` resolves when only one
// namespace defines it. Ambiguity returns the qualified candidates so the
// refusal can tell the caller which to pick.
func (ws *Workspace) FindMetric(name string) (MetricRef, []string, bool) {
	if ns, bare, ok := ws.splitQualified(name); ok {
		m, found := ns.Schema.Metric(bare)
		if !found {
			return MetricRef{}, nil, false
		}
		return MetricRef{Namespace: ns, Metric: m, Qualified: Qualify(ns.Name, m.Name)}, nil, true
	}

	var hits []MetricRef
	for _, ns := range ws.Available() {
		if m, ok := ns.Schema.Metric(name); ok {
			hits = append(hits, MetricRef{
				Namespace: ns, Metric: m, Qualified: Qualify(ns.Name, m.Name),
			})
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil, true
	case 0:
		return MetricRef{}, nil, false
	}
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.Qualified
	}
	sort.Strings(names)
	return MetricRef{}, names, false
}

// FindDimension resolves a dimension name, which is `dataset.field` inside a
// namespace and `namespace.dataset.field` across the workspace.
func (ws *Workspace) FindDimension(name string) (ns *Namespace, local string, candidates []string, ok bool) {
	parts := strings.Split(name, ".")
	if len(parts) == 3 {
		n, found := ws.Namespace(parts[0])
		if !found || !n.Available() {
			return nil, "", nil, false
		}
		local = parts[1] + "." + parts[2]
		f, found := n.Schema.Field(local)
		if !found || !f.IsDimension {
			return nil, "", nil, false
		}
		return n, local, nil, true
	}
	if len(parts) != 2 {
		return nil, "", nil, false
	}

	var hits []*Namespace
	for _, n := range ws.Available() {
		if f, found := n.Schema.Field(name); found && f.IsDimension {
			hits = append(hits, n)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], name, nil, true
	case 0:
		return nil, "", nil, false
	}
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = Qualify(h.Name, name)
	}
	sort.Strings(names)
	return nil, "", names, false
}

// splitQualified separates a leading namespace when the name carries one.
func (ws *Workspace) splitQualified(name string) (*Namespace, string, bool) {
	prefix, rest, found := strings.Cut(name, ".")
	if !found {
		return nil, "", false
	}
	ns, ok := ws.Namespace(prefix)
	if !ok || !ns.Available() {
		return nil, "", false
	}
	return ns, rest, true
}

// MetricNames returns every metric in the workspace, qualified, for suggestions
// and listings.
func (ws *Workspace) MetricNames() []string {
	var out []string
	for _, ns := range ws.Available() {
		for _, m := range ns.Model.Metrics {
			out = append(out, Qualify(ns.Name, m.Name))
		}
	}
	return out
}

// DimensionNames returns every dimension in the workspace, qualified.
func (ws *Workspace) DimensionNames() []string {
	var out []string
	for _, ns := range ws.Available() {
		for _, f := range ns.Schema.Dimensions() {
			out = append(out, Qualify(ns.Name, f.QualifiedName()))
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResolveDimension turns a requested dimension name into the governed name that
// identifies it across the whole workspace.
//
// Two namespaces addressing the same imported column produce the same governed
// name, which is what lets a multi-fact query know they are the same grain.
func (ws *Workspace) ResolveDimension(name string) (string, []string, bool) {
	ns, local, candidates, ok := ws.FindDimension(name)
	if !ok {
		return "", candidates, false
	}
	f, found := ns.Schema.Field(local)
	if !found {
		return "", nil, false
	}
	return f.GovernedName(), nil, true
}

// FieldByGoverned finds the field a namespace addresses a governed name by, or
// reports that this namespace cannot reach it at all.
func (n *Namespace) FieldByGoverned(governed string) (*osi.Field, bool) {
	if !n.Available() {
		return nil, false
	}
	for _, d := range n.Model.Datasets {
		for _, f := range d.Fields {
			if f.GovernedName() == governed {
				return f, true
			}
		}
	}
	return nil, false
}
