// Package dbt imports a dbt semantic layer into an Ossie model.
//
// The cold start `truegrain init` does not answer. init helps somebody who
// has a warehouse and no semantic layer; this helps somebody who already has
// one, which is the larger and harder group, because they have already had
// the arguments about what revenue means and will not have them again to
// try a new tool.
//
// # What dbt gives us, and why it is enough
//
// MetricFlow semantic models declare `entities` typed primary, foreign or
// unique. That is exactly the grain and the join graph, which is exactly
// what fan-out detection needs and exactly what an importer usually cannot
// get. A dbt project therefore arrives with everything required for the
// engine to start refusing questions it could not answer correctly, which
// dbt itself does not do.
//
// That is the interesting outcome and worth stating plainly: importing a
// dbt project can reveal that a number somebody has been reading for a year
// is inflated. The engine will refuse it rather than reproduce it.
//
// # Read from manifest.json
//
// Not from the YAML. The manifest is compiled, so refs are resolved,
// inheritance is applied and the physical relation name is present; parsing
// the YAML would mean reimplementing dbt's resolver and getting a different
// answer from dbt in some corner. Every project produces one with
// `dbt parse` or `dbt compile`, and it needs no warehouse credentials.
//
// # What is not imported, and why that is said out loud
//
// A cumulative or conversion metric is a window function, which this engine
// refuses by name rather than approximating. A derived metric can reference
// metrics at different grains, which is the exact thing the fan-out check
// exists to catch, so importing one silently would smuggle past the check
// the import was supposed to add. Both are reported by name with the reason
// instead of being dropped, because an importer that quietly drops a third
// of a catalogue is worse than one that refuses to run.
package dbt

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/introspect"
)

// manifest is the subset of dbt's artifact this reads.
//
// Deliberately partial. The manifest is large and mostly about compilation;
// decoding all of it would couple this to dbt's internals and break on a
// schema bump that changed something unrelated.
type manifest struct {
	Metadata struct {
		ProjectName string `json:"project_name"`
		Version     string `json:"dbt_schema_version"`
		AdapterType string `json:"adapter_type"`
	} `json:"metadata"`
	SemanticModels map[string]semanticModel `json:"semantic_models"`
	Metrics        map[string]dbtMetric     `json:"metrics"`
}

type semanticModel struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	NodeRelation struct {
		Alias        string `json:"alias"`
		SchemaName   string `json:"schema_name"`
		Database     string `json:"database"`
		RelationName string `json:"relation_name"`
	} `json:"node_relation"`
	Entities   []entity    `json:"entities"`
	Measures   []measure   `json:"measures"`
	Dimensions []dimension `json:"dimensions"`
	// PrimaryEntity names the grain when no entity is typed primary, which
	// MetricFlow allows for a model keyed by something it does not expose.
	PrimaryEntity string `json:"primary_entity"`
}

type entity struct {
	Name string `json:"name"`
	// Type is primary, foreign, unique or natural.
	Type string `json:"type"`
	// Expr is the column, when it differs from the entity name. An entity
	// called `customer` backed by `customer_id` is the common case, and
	// reading the name instead of the expression would emit a join on a
	// column that does not exist.
	Expr string `json:"expr"`
}

// column returns the physical column this entity is.
func (e entity) column() string {
	if e.Expr != "" {
		return e.Expr
	}
	return e.Name
}

type measure struct {
	Name        string `json:"name"`
	Agg         string `json:"agg"`
	Expr        string `json:"expr"`
	Description string `json:"description"`
	// AggTimeDimension is MetricFlow's marker for a semi-additive measure.
	// Not imported, and named in the report: a measure that sums over one
	// dimension and not another is the thing this engine refuses rather than
	// guesses.
	NonAdditiveDimension *struct {
		Name string `json:"name"`
	} `json:"non_additive_dimension"`
}

func (m measure) column() string {
	if m.Expr != "" {
		return m.Expr
	}
	return m.Name
}

type dimension struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Expr        string `json:"expr"`
	Description string `json:"description"`
}

func (d dimension) column() string {
	if d.Expr != "" {
		return d.Expr
	}
	return d.Name
}

type dbtMetric struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Label       string `json:"label"`
	// Type is simple, ratio, derived, cumulative or conversion.
	Type       string `json:"type"`
	TypeParams struct {
		Measure     *measureRef `json:"measure"`
		Numerator   *measureRef `json:"numerator"`
		Denominator *measureRef `json:"denominator"`
	} `json:"type_params"`
}

type measureRef struct {
	Name string `json:"name"`
	// Filter, when set, means the reference is not a plain measure and the
	// engine would need a filtered aggregate to reproduce it.
	Filter any `json:"filter"`
}

// Report says what happened, including what did not.
type Report struct {
	Project string
	// Imported names what came across.
	SemanticModels int
	Metrics        int
	Relationships  int
	// Skipped names what did not, one line each, with the reason. This is
	// the half that matters: an importer that quietly drops a third of a
	// catalogue leaves somebody comparing two tools on numbers that are not
	// the same numbers.
	Skipped []string
	// Notes are things to check rather than things that failed.
	Notes []string
}

// Import reads a dbt manifest and produces a model plus a report.
func Import(path string) (introspect.Schema, Report, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return introspect.Schema{}, Report{}, fmt.Errorf(
			"reading the dbt manifest %s: %w\n"+
				"Produce one with `dbt parse`, which needs no warehouse connection; "+
				"it lands in target/manifest.json", path, err)
	}

	var m manifest
	if err := json.Unmarshal(src, &m); err != nil {
		return introspect.Schema{}, Report{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	report := Report{Project: m.Metadata.ProjectName}
	if len(m.SemanticModels) == 0 {
		return introspect.Schema{}, report, fmt.Errorf(
			"%s declares no semantic models.\n"+
				"This imports the dbt Semantic Layer, which is the `semantic_models:` "+
				"and `metrics:` blocks introduced in dbt 1.6. A project with only "+
				"`models:` has no metrics or entities to carry across; for that, point "+
				"`truegrain init` at the warehouse the project builds into", path)
	}

	schema := introspect.Schema{Name: schemaNameOf(m)}

	// Ordered, so importing twice produces the same file and a re-import
	// after a dbt change is a readable diff.
	names := make([]string, 0, len(m.SemanticModels))
	for k := range m.SemanticModels {
		names = append(names, k)
	}
	sort.Strings(names)

	// primaryOwner maps an entity name to the dataset that has it as its
	// primary key. A foreign entity joins to whichever model declares the
	// same entity name as primary: that is how MetricFlow resolves a join,
	// and reading it any other way produces a join dbt would not have made.
	primaryOwner := map[string]primary{}
	for _, key := range names {
		sm := m.SemanticModels[key]
		for _, e := range sm.Entities {
			if e.Type == "primary" || e.Type == "unique" {
				// A model may declare several unique entities; the primary
				// one wins, and otherwise first by sorted model name so the
				// choice is stable.
				existing, seen := primaryOwner[e.Name]
				if !seen || (e.Type == "primary" && existing.kind != "primary") {
					primaryOwner[e.Name] = primary{
						dataset: sm.Name, column: e.column(), kind: e.Type,
					}
				}
			}
		}
	}

	measures := map[string]locatedMeasure{}

	for _, key := range names {
		sm := m.SemanticModels[key]
		table, skipped := datasetFrom(sm)
		schema.Tables = append(schema.Tables, table)
		report.Skipped = append(report.Skipped, skipped...)
		report.SemanticModels++

		for _, ms := range sm.Measures {
			measures[ms.Name] = locatedMeasure{measure: ms, dataset: sm.Name}
		}

		for _, e := range sm.Entities {
			if e.Type != "foreign" {
				continue
			}
			owner, ok := primaryOwner[e.Name]
			if !ok {
				report.Skipped = append(report.Skipped, fmt.Sprintf(
					"join %s.%s: no semantic model declares %q as a primary entity, "+
						"so there is nothing to join to. dbt would not have made this "+
						"join either", sm.Name, e.column(), e.Name))
				continue
			}
			if owner.dataset == sm.Name {
				continue // a model joining to itself is not a join
			}
			schema.Relationships = append(schema.Relationships, introspect.Relationship{
				Name:        sm.Name + "_to_" + owner.dataset,
				From:        sm.Name,
				FromColumns: []string{e.column()},
				To:          owner.dataset,
				ToColumns:   []string{owner.column},
			})
			report.Relationships++
		}
	}

	schema.Metrics, report.Skipped = importMetrics(m, measures, report.Skipped)
	report.Metrics = len(schema.Metrics)

	sort.Strings(report.Skipped)
	report.Notes = notesFor(schema, m)
	// The same pass every warehouse reader goes through, so an imported
	// model's missing grain is reported in the same words as a read one.
	return introspect.Finish(schema), report, nil
}

type primary struct {
	dataset string
	column  string
	kind    string
}

type locatedMeasure struct {
	measure measure
	dataset string
}

// datasetFrom turns one semantic model into a dataset.
func datasetFrom(sm semanticModel) (introspect.Table, []string) {
	t := introspect.Table{Name: sm.Name, Description: sm.Description}
	var skipped []string

	// The grain. Without it every join into this dataset is assumed to fan
	// out and sums across it are refused, which is the safe direction but
	// makes the model much less useful, so it is worth reporting.
	for _, e := range sm.Entities {
		if e.Type == "primary" {
			t.PrimaryKey = append(t.PrimaryKey, e.column())
		}
	}
	if len(t.PrimaryKey) == 0 && sm.PrimaryEntity != "" {
		t.PrimaryKey = append(t.PrimaryKey, sm.PrimaryEntity)
	}

	seen := map[string]bool{}
	add := func(name, kind, description string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		t.Columns = append(t.Columns, introspect.Column{
			Name: name, Type: kind, Nullable: true, Description: description,
		})
	}

	// Entities are columns too: a foreign key is a legitimate thing to group
	// by, and leaving it out would make the join column invisible in the
	// model that declares it.
	for _, e := range sm.Entities {
		add(e.column(), "string", "")
	}
	for _, d := range sm.Dimensions {
		kind := "string"
		if d.Type == "time" {
			kind = "timestamp"
		}
		// An expression dimension is a computed column, not a physical one.
		// Emitting it as a plain field would produce a model that names a
		// column the warehouse does not have.
		if d.Expr != "" && !isPlainColumn(d.Expr) {
			skipped = append(skipped, fmt.Sprintf(
				"dimension %s.%s: it is an expression (%s) rather than a column, "+
					"so it is left out. Add it by hand as a field with that "+
					"expression if you want it", sm.Name, d.Name, d.Expr))
			continue
		}
		add(d.column(), kind, d.Description)
	}
	for _, ms := range sm.Measures {
		if ms.Expr != "" && !isPlainColumn(ms.Expr) {
			continue // the metric carries the expression; the column is not one
		}
		add(ms.column(), "numeric", "")
	}

	return t, skipped
}

// isPlainColumn reports whether an expr is just a column name, rather than
// SQL. dbt allows either in the same field.
func isPlainColumn(expr string) bool {
	if expr == "" {
		return false
	}
	for _, r := range expr {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// importMetrics carries across the metric types that map cleanly, and names
// the ones that do not.
func importMetrics(m manifest, measures map[string]locatedMeasure, skipped []string) ([]introspect.Metric, []string) {
	keys := make([]string, 0, len(m.Metrics))
	for k := range m.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []introspect.Metric
	for _, key := range keys {
		dm := m.Metrics[key]
		switch dm.Type {
		case "simple":
			ref := dm.TypeParams.Measure
			if ref == nil {
				skipped = append(skipped, fmt.Sprintf(
					"metric %s: simple, but names no measure", dm.Name))
				continue
			}
			if ref.Filter != nil {
				skipped = append(skipped, fmt.Sprintf(
					"metric %s: its measure carries a filter, which would need a "+
						"filtered aggregate to reproduce exactly. Write it by hand "+
						"as SUM(CASE WHEN ... THEN ... END)", dm.Name))
				continue
			}
			expr, err := aggregate(ref.Name, measures)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("metric %s: %v", dm.Name, err))
				continue
			}
			out = append(out, introspect.Metric{
				Name: dm.Name, Description: describe(dm), Expression: expr,
				Datatype: "Number",
			})

		case "ratio":
			num, errN := aggregate(refName(dm.TypeParams.Numerator), measures)
			den, errD := aggregate(refName(dm.TypeParams.Denominator), measures)
			if errN != nil || errD != nil {
				skipped = append(skipped, fmt.Sprintf(
					"metric %s: ratio, but a side could not be resolved (%v %v)",
					dm.Name, errN, errD))
				continue
			}
			// A ratio of two aggregates computed at the query grain is
			// exactly what dbt does, so this is a faithful import rather
			// than an approximation. It is also non-additive, which the
			// engine already handles by refusing to sum it.
			out = append(out, introspect.Metric{
				Name: dm.Name, Description: describe(dm),
				Expression: fmt.Sprintf("(%s) / NULLIF(%s, 0)", num, den),
				Datatype:   "Decimal",
			})

		case "cumulative", "conversion":
			skipped = append(skipped, fmt.Sprintf(
				"metric %s: a %s metric is a window function over an ordered "+
					"frame, which this engine refuses by name rather than "+
					"approximating. It is not imported", dm.Name, dm.Type))

		case "derived":
			skipped = append(skipped, fmt.Sprintf(
				"metric %s: a derived metric composes other metrics, which may "+
					"sit at different grains. Importing it silently would carry "+
					"past the fan-out check this import exists to add. Write it "+
					"by hand once you have checked the grains agree", dm.Name))

		default:
			skipped = append(skipped, fmt.Sprintf(
				"metric %s: unrecognised type %q", dm.Name, dm.Type))
		}
	}
	return out, skipped
}

func refName(r *measureRef) string {
	if r == nil {
		return ""
	}
	return r.Name
}

func describe(dm dbtMetric) string {
	if dm.Description != "" {
		return dm.Description
	}
	return dm.Label
}

// aggregate renders one dbt measure as ANSI SQL.
//
// ANSI rather than the source warehouse's spelling, because the dialect
// emitters rewrite from ANSI and a model carrying Snowflake syntax would
// only run on Snowflake, which defeats importing it.
func aggregate(name string, measures map[string]locatedMeasure) (string, error) {
	if name == "" {
		return "", fmt.Errorf("no measure named")
	}
	lm, ok := measures[name]
	if !ok {
		return "", fmt.Errorf("measure %q is not declared by any semantic model", name)
	}
	if lm.measure.NonAdditiveDimension != nil {
		return "", fmt.Errorf(
			"measure %q is semi-additive (non_additive_dimension: %s), which is "+
				"not expressible here and is refused rather than guessed",
			name, lm.measure.NonAdditiveDimension.Name)
	}

	col := lm.dataset + "." + lm.measure.column()
	switch lm.measure.Agg {
	case "sum":
		return "SUM(" + col + ")", nil
	case "min":
		return "MIN(" + col + ")", nil
	case "max":
		return "MAX(" + col + ")", nil
	case "count":
		return "COUNT(" + col + ")", nil
	case "count_distinct":
		return "COUNT(DISTINCT " + col + ")", nil
	case "average", "avg":
		return "AVG(" + col + ")", nil
	case "sum_boolean":
		// A boolean summed is a count of true. Written explicitly because
		// summing a boolean directly is not portable.
		return "SUM(CASE WHEN " + col + " THEN 1 ELSE 0 END)", nil
	case "median", "percentile":
		return "", fmt.Errorf(
			"aggregation %q has no portable ANSI form and differs by warehouse, "+
				"so it is not imported", lm.measure.Agg)
	}
	return "", fmt.Errorf("unrecognised aggregation %q", lm.measure.Agg)
}

// schemaNameOf works out the physical schema the datasets live in.
//
// dbt models may span schemas. When they do, the source prefix cannot be one
// value and the caller is told to check, rather than having one silently
// chosen for them.
func schemaNameOf(m manifest) string {
	seen := map[string]bool{}
	var name string
	for _, sm := range m.SemanticModels {
		s := sm.NodeRelation.SchemaName
		if s == "" {
			continue
		}
		seen[s] = true
		if name == "" {
			name = s
		}
	}
	if name == "" {
		return m.Metadata.ProjectName
	}
	return name
}

func notesFor(s introspect.Schema, m manifest) []string {
	var notes []string

	schemas := map[string]bool{}
	for _, sm := range m.SemanticModels {
		if sm.NodeRelation.SchemaName != "" {
			schemas[sm.NodeRelation.SchemaName] = true
		}
	}
	if len(schemas) > 1 {
		names := make([]string, 0, len(schemas))
		for k := range schemas {
			names = append(names, k)
		}
		sort.Strings(names)
		notes = append(notes, fmt.Sprintf(
			"the project spans %d schemas (%s) and a generated model has one "+
				"source prefix. Check the `source:` lines and correct the ones that "+
				"are not in %s", len(schemas), strings.Join(names, ", "), s.Name))
	}
	if m.Metadata.AdapterType != "" {
		notes = append(notes, fmt.Sprintf(
			"the project targets %s. Set warehouse.dialect to match, or the "+
				"compiled SQL will be correct for a different warehouse",
			m.Metadata.AdapterType))
	}
	return notes
}
