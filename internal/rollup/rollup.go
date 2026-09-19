package rollup

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// Pre-aggregated tables, and the rules for when reading one is still
// answering the question that was asked.
//
// A rollup is a second copy of a number. That is the entire risk and it is
// worth naming before any of the mechanics: if the table is stale or was
// built wrong, an engine that reads it returns a confident wrong total,
// which is the exact failure this product exists to refuse. So routing here
// is conservative in three specific ways, and each one costs performance on
// purpose.
//
// A rollup must declare how its freshness is checked, and routing only
// happens when that check passes. A rollup with no freshness block is never
// read. The check needs a warehouse, so an engine compiling without an
// executor never routes, which is correct: compiled output must not depend
// on state nothing verified.
//
// Failing the check falls back to the base fact rather than refusing. A
// rollup is an optimisation, and turning a failed rollup build into an
// outage for queries that would have worked fine against the source tables
// is a worse trade than answering them slowly.
//
// Only distributive metrics route. SUM, COUNT, MIN and MAX can be computed
// from partial aggregates of themselves; AVG, MEDIAN, COUNT(DISTINCT) and
// every ratio cannot. The distinction already exists as osi.Classify, which
// derives it from the aggregate the author wrote. A metric outside that set
// is served from the base fact, always.
//
// The re-aggregation function is not the stored one. A stored SUM
// re-aggregates with SUM, and a stored COUNT re-aggregates with SUM as
// well, because counting the rollup's rows counts groups rather than the
// rows underneath them. Getting that backwards produces a number that is
// plausible, smaller, and wrong, so it is derived here rather than assumed.

// FileName is the sidecar a namespace declares its rollups in.
//
// A sidecar rather than a block in the Ossie model, because the spec has no
// place for one and inventing a proprietary key would mean models had to be
// authored for this engine. A sidecar rather than the project file, because
// a rollup is a fact about the model, not about the deployment: dev and
// prod read the same rollups from the same definitions.
const FileName = "rollups.yaml"

// Rollup is one pre-aggregated table, validated against a model.
type Rollup struct {
	// Name identifies the rollup in logs, in audit events and in the
	// compiled output, so a number can be traced to the table it came from.
	Name string `yaml:"name"`
	// Source is the physical table, in the same `schema.table` form a
	// dataset's source takes.
	Source string `yaml:"source"`
	// Dimensions are the columns this table is grouped by. A request can be
	// served from here only when every dimension it groups or filters on is
	// in this list.
	Dimensions []Dimension `yaml:"dimensions"`
	// Metrics maps a model metric to the column holding its partial
	// aggregate.
	Metrics []Metric `yaml:"metrics"`
	// Freshness is how this table proves it is current. Required: a rollup
	// that cannot be checked is never read.
	Freshness Freshness `yaml:"freshness"`

	// resolved is filled by Validate.
	dimByField map[string]*Dimension
	metByName  map[string]*Metric
}

// Dimension is one grouping column of the rollup.
type Dimension struct {
	// Field is the model dimension this column holds, as `dataset.field`.
	Field string `yaml:"field"`
	// Column is the column name in the rollup table. Empty means the same
	// as the field's own name.
	Column string `yaml:"column"`
	// Grain is the time grain this column is stored at, for a time
	// dimension. A request at this grain or a coarser one can be served;
	// a finer one cannot, because the detail is gone.
	Grain string `yaml:"grain"`

	// Resolved is the model dimension this column holds, filled by
	// Validate.
	Resolved *osi.Field `yaml:"-"`
}

// Metric maps a model metric to its stored partial aggregate.
type Metric struct {
	// Metric is the model metric's name.
	Metric string `yaml:"metric"`
	// Column is the column holding the partial aggregate. Empty means the
	// same as the metric's name.
	Column string `yaml:"column"`

	// Resolved is the model metric, filled by Validate.
	Resolved *osi.Metric `yaml:"-"`
	// Reaggregate is the function that combines partial aggregates, derived
	// rather than declared. See the package comment for why it is not
	// always the stored one.
	Reaggregate string `yaml:"-"`
}

// Freshness is how a rollup proves it is current enough to read.
type Freshness struct {
	// Column is a timestamp column in the rollup, typically the build time
	// or the maximum event time it covers.
	Column string `yaml:"column"`
	// MaxAge is how old that timestamp may be before the rollup stops being
	// used, written the way Go writes a duration: 26h, 90m. Required and
	// positive: a rollup with no bound is one nobody will notice has
	// stopped building.
	//
	// A string in the file and a duration after parsing, because yaml.v3
	// decodes a duration as a bare integer count of nanoseconds, and
	// `max_age: 26` silently meaning twenty-six nanoseconds is a rollup
	// that is never fresh and nobody can see why.
	MaxAge string `yaml:"max_age"`

	// Age is MaxAge parsed, filled by Validate.
	Age time.Duration `yaml:"-"`
}

type file struct {
	Version int       `yaml:"version"`
	Rollups []*Rollup `yaml:"rollups"`
}

// Load reads a namespace's rollup sidecar and validates every entry against
// the model.
//
// A missing file is not an error and returns nothing: rollups are an
// addition, and every namespace that existed before this had none.
func Load(path string, s *resolve.Schema) ([]*Rollup, error) {
	src, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var f file
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d, expected 1", path, f.Version)
	}

	seen := map[string]bool{}
	for _, r := range f.Rollups {
		if seen[strings.ToLower(r.Name)] {
			return nil, fmt.Errorf("%s: two rollups are named %q", path, r.Name)
		}
		seen[strings.ToLower(r.Name)] = true
		if err := r.Validate(s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return f.Rollups, nil
}

// Validate resolves every name in the rollup against the model and derives
// the re-aggregation for each metric.
//
// Strict on purpose, and the strictness is the feature. A rollup naming a
// metric that no longer exists, or a column for a metric that cannot be
// re-aggregated, is a wrong answer waiting for the first caller. Failing at
// load turns it into a startup error in front of whoever changed the model.
func (r *Rollup) Validate(s *resolve.Schema) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("a rollup has no name; the name is what an audit event " +
			"records when a number came from it")
	}
	if err := checkSource(r.Source); err != nil {
		return fmt.Errorf("rollup %q source: %w", r.Name, err)
	}
	if len(r.Metrics) == 0 {
		return fmt.Errorf("rollup %q holds no metrics", r.Name)
	}
	if r.Freshness.Column == "" || r.Freshness.MaxAge == "" {
		return fmt.Errorf(
			"rollup %q has no freshness check. Give it a timestamp column and a "+
				"max_age: a pre-aggregated table is a second copy of a number, and "+
				"one nobody checks is one nobody notices has stopped building",
			r.Name)
	}
	age, err := time.ParseDuration(r.Freshness.MaxAge)
	if err != nil {
		return fmt.Errorf("rollup %q: max_age %q is not a duration like 26h or 90m",
			r.Name, r.Freshness.MaxAge)
	}
	if age <= 0 {
		return fmt.Errorf("rollup %q: max_age %q is not positive, so the rollup "+
			"would never be considered fresh", r.Name, r.Freshness.MaxAge)
	}
	r.Freshness.Age = age
	if err := checkIdent(r.Freshness.Column); err != nil {
		return fmt.Errorf("rollup %q freshness column: %w", r.Name, err)
	}

	r.dimByField = map[string]*Dimension{}
	for i := range r.Dimensions {
		d := &r.Dimensions[i]
		field, ok := s.Field(d.Field)
		if !ok {
			return fmt.Errorf("rollup %q: no dimension %q in this model", r.Name, d.Field)
		}
		if !field.IsDimension {
			return fmt.Errorf(
				"rollup %q: %q is a field but not a dimension, so nothing can group by it",
				r.Name, d.Field)
		}
		if d.Column == "" {
			d.Column = field.Name
		}
		if err := checkIdent(d.Column); err != nil {
			return fmt.Errorf("rollup %q dimension %q: %w", r.Name, d.Field, err)
		}
		if d.Grain != "" && !field.IsTime {
			return fmt.Errorf(
				"rollup %q: %q has a grain but is not a time dimension",
				r.Name, d.Field)
		}
		if field.IsTime && d.Grain == "" {
			return fmt.Errorf(
				"rollup %q: time dimension %q needs a grain. Without one nothing "+
					"can tell whether a request for monthly totals is answerable "+
					"from this table or is asking for detail it does not hold",
				r.Name, d.Field)
		}
		d.Resolved = field
		key := strings.ToUpper(field.QualifiedName())
		if _, duplicate := r.dimByField[key]; duplicate {
			return fmt.Errorf("rollup %q: dimension %q is listed twice", r.Name, d.Field)
		}
		r.dimByField[key] = d
	}

	r.metByName = map[string]*Metric{}
	for i := range r.Metrics {
		m := &r.Metrics[i]
		metric, ok := s.Metric(m.Metric)
		if !ok {
			return fmt.Errorf("rollup %q: no metric %q in this model", r.Name, m.Metric)
		}
		if m.Column == "" {
			m.Column = metric.Name
		}
		if err := checkIdent(m.Column); err != nil {
			return fmt.Errorf("rollup %q metric %q: %w", r.Name, m.Metric, err)
		}
		reagg, err := reaggregationFor(metric)
		if err != nil {
			return fmt.Errorf("rollup %q: %w", r.Name, err)
		}
		m.Resolved, m.Reaggregate = metric, reagg

		key := strings.ToUpper(metric.Name)
		if _, duplicate := r.metByName[key]; duplicate {
			return fmt.Errorf("rollup %q: metric %q is listed twice", r.Name, m.Metric)
		}
		r.metByName[key] = m
	}
	return nil
}

// reaggregationFor derives the function that combines this metric's stored
// partial aggregates.
//
// Not the stored function. A stored COUNT re-aggregates with SUM: counting
// the rollup's rows counts groups, not the rows underneath them, and the
// result is smaller, plausible and wrong. That single substitution is the
// reason this is derived from the expression rather than declared in the
// file, where somebody would eventually write COUNT.
func reaggregationFor(m *osi.Metric) (string, error) {
	call, ok := topLevelCall(m)
	if !ok {
		return "", fmt.Errorf(
			"metric %q is not a single aggregate, so it has no partial aggregate to "+
				"store. A ratio or an expression over two metrics is computed from "+
				"metrics that can themselves be rolled up; roll those up instead",
			m.Name)
	}
	if osi.Classify(call) != osi.Distributive {
		return "", fmt.Errorf(
			"metric %q is %s, so it cannot be recomputed from partial aggregates. "+
				"Only SUM, COUNT, MIN and MAX can; this one is served from the base "+
				"fact and does not belong in a rollup",
			m.Name, osi.Classify(call))
	}
	switch call.Name {
	case "SUM", "COUNT":
		// COUNT deliberately becomes SUM. See the function comment.
		return "SUM", nil
	case "MIN":
		return "MIN", nil
	case "MAX":
		return "MAX", nil
	}
	return "", fmt.Errorf("metric %q aggregates with %s, which this does not roll up",
		m.Name, call.Name)
}

// topLevelCall returns the metric's aggregate when its expression is exactly
// one, which is the only shape that has a partial aggregate to store.
func topLevelCall(m *osi.Metric) (*osi.Call, bool) {
	call, ok := m.Expression.AST.(*osi.Call)
	if !ok || !osi.IsAggregate(call.Name) {
		return nil, false
	}
	return call, true
}

// Metric returns the stored column for a metric, when this rollup holds one.
func (r *Rollup) Metric(name string) (*Metric, bool) {
	m, ok := r.metByName[strings.ToUpper(name)]
	return m, ok
}

// Dimension returns the stored column for a field, when this rollup has one.
func (r *Rollup) Dimension(field *osi.Field) (*Dimension, bool) {
	d, ok := r.dimByField[strings.ToUpper(field.QualifiedName())]
	return d, ok
}

// checkIdent refuses anything that is not a plain unquoted identifier.
//
// The freshness probe builds `SELECT MAX(col) FROM source` by formatting
// these two values. They come from a file an operator wrote, not from a
// caller, and both go through the dialect's quoting on the way out, so this
// is the third layer rather than the only one. It is here because a column
// name is the one place in this file where a value becomes part of a
// statement, and a rule that says "letters, digits and underscore" is
// cheaper to keep true than a quoting argument is to keep winning.
func checkIdent(s string) error {
	if s == "" {
		return fmt.Errorf("is empty")
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf(
				"%q is not a plain identifier. A rollup column name is formatted "+
					"into the freshness probe, so it is restricted to letters, "+
					"digits and underscore rather than escaped", s)
		}
	}
	return nil
}

// checkSource applies the same rule to each dotted part of a table path.
func checkSource(source string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("is empty; name the physical table this rollup lives in")
	}
	for _, part := range strings.Split(source, ".") {
		if err := checkIdent(part); err != nil {
			return err
		}
	}
	return nil
}
