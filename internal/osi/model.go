// Package osi holds the in-memory representation of an Apache Ossie semantic
// model and the parser that produces it.
//
// The IR deliberately mirrors the spec's own shape (core-spec/spec.yaml,
// version 0.2.0.dev0) rather than inventing a friendlier one. Where the engine
// needs a concept the spec does not carry, it is derived here and the
// derivation is named, never silently defaulted. See COMPLIANCE.md.
package osi

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// DialectANSI is the dialect every model is required to express, and the one
// the planner analyses. Other dialects are emitted verbatim when the target
// matches, but they are never the basis for a planning decision.
const DialectANSI = "ANSI_SQL"

// Pos locates a construct in the source files for diagnostics. Every error the
// parser and resolver raise carries one.
type Pos struct {
	File string
	Line int
}

func (p Pos) String() string {
	if p.File == "" {
		return fmt.Sprintf("line %d", p.Line)
	}
	return fmt.Sprintf("%s:%d", p.File, p.Line)
}

// AIContext is the spec's grounding text. It accepts either a bare string or a
// structured mapping, so both forms in the wild round-trip.
type AIContext struct {
	Instructions string   `yaml:"instructions"`
	Synonyms     []string `yaml:"synonyms"`
	Examples     []string `yaml:"examples"`
}

func (a *AIContext) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		a.Instructions = n.Value
		return nil
	}
	type plain AIContext
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*a = AIContext(p)
	return nil
}

// Expression is the spec's per-dialect expression block. The ANSI_SQL variant
// is parsed into an AST; the raw text of every dialect is retained so an
// emitter can prefer an author-supplied variant for its own target.
type Expression struct {
	Pos       Pos
	ByDialect map[string]string
	AST       Expr
}

// For returns the author's expression text for a dialect, falling back to ANSI.
func (e *Expression) For(dialect string) (string, bool) {
	if s, ok := e.ByDialect[dialect]; ok {
		return s, true
	}
	s, ok := e.ByDialect[DialectANSI]
	return s, ok
}

// ANSI returns the ANSI_SQL source text, which is what the AST was built from.
func (e *Expression) ANSI() string { return e.ByDialect[DialectANSI] }

func (e *Expression) UnmarshalYAML(n *yaml.Node) error {
	var raw struct {
		Dialects []struct {
			Dialect    string `yaml:"dialect"`
			Expression string `yaml:"expression"`
		} `yaml:"dialects"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	e.Pos = Pos{Line: n.Line}
	e.ByDialect = make(map[string]string, len(raw.Dialects))
	for _, d := range raw.Dialects {
		name := d.Dialect
		if name == "" {
			name = DialectANSI // spec: dialect defaults to ANSI_SQL
		}
		e.ByDialect[name] = d.Expression
	}
	return nil
}

// Field is a row-level attribute: the spec's `fields` entry. A field is usable
// as a dimension when it carries a `dimension:` block.
type Field struct {
	Pos         Pos
	Name        string
	Expression  Expression
	Label       string
	Description string
	Datatype    string
	AIContext   AIContext

	// IsDimension records whether a `dimension:` block was present. Absent
	// means the field exists for use inside metric expressions but is not
	// offered for grouping.
	IsDimension bool
	// IsTime is the resolved temporal role, after applying the spec's default.
	IsTime bool

	// Dataset is set by the resolver.
	Dataset *Dataset `yaml:"-"`
}

// QualifiedName is how the field is addressed across an interface: dataset.field.
func (f *Field) QualifiedName() string {
	if f.Dataset == nil {
		return f.Name
	}
	return f.Dataset.Name + "." + f.Name
}

// GovernedName is the name a policy is resolved against. It uses the dataset's
// origin rather than the name it happens to be addressed by, so an imported
// dataset stays bound to the grant written where it was defined.
func (f *Field) GovernedName() string {
	if f.Dataset == nil {
		return f.Name
	}
	return f.Dataset.Origin() + "." + f.Name
}

func (f *Field) UnmarshalYAML(n *yaml.Node) error {
	type plain struct {
		Name        string      `yaml:"name"`
		Expression  Expression  `yaml:"expression"`
		Label       string      `yaml:"label"`
		Description string      `yaml:"description"`
		Datatype    string      `yaml:"datatype"`
		AIContext   AIContext   `yaml:"ai_context"`
		Dimension   *dimensionB `yaml:"dimension"`
	}
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	f.Pos = Pos{Line: n.Line}
	f.Name, f.Expression = p.Name, p.Expression
	f.Label, f.Description, f.Datatype, f.AIContext = p.Label, p.Description, p.Datatype, p.AIContext
	f.IsDimension = p.Dimension != nil
	f.IsTime = resolveIsTime(p.Dimension, p.Datatype)
	return nil
}

type dimensionB struct {
	IsTime *bool `yaml:"is_time"`
}

// Datatype is the spec's portable logical type vocabulary, from the
// `datatypes` enum in core-spec/spec.yaml. A field may also omit its datatype,
// which the engine treats as unknown rather than as a default.
type Datatype string

const (
	TypeString     Datatype = "String"
	TypeInteger    Datatype = "Integer"
	TypeDecimal    Datatype = "Decimal"
	TypeFloat      Datatype = "Float"
	TypeBoolean    Datatype = "Boolean"
	TypeDate       Datatype = "Date"
	TypeTime       Datatype = "Time"
	TypeDateTime   Datatype = "DateTime"
	TypeDateTimeTz Datatype = "DateTimeTz"
	TypeOpaque     Datatype = "Opaque"
)

// KnownDatatypes is the full portable vocabulary, used to validate a model.
var KnownDatatypes = []Datatype{
	TypeString, TypeInteger, TypeDecimal, TypeFloat, TypeBoolean,
	TypeDate, TypeTime, TypeDateTime, TypeDateTimeTz, TypeOpaque,
}

// IsTemporal reports whether a datatype carries a time component, which is what
// makes `dimension.is_time` default to true.
func (d Datatype) IsTemporal() bool {
	switch d {
	case TypeDate, TypeTime, TypeDateTime, TypeDateTimeTz:
		return true
	}
	return false
}

// resolveIsTime applies the spec's default: is_time is true for temporal
// datatypes unless explicitly set false, and false otherwise unless explicitly
// set true. A field with no `dimension:` block is never a time dimension
// because it is not a dimension at all.
func resolveIsTime(d *dimensionB, datatype string) bool {
	if d == nil {
		return false
	}
	if d.IsTime != nil {
		return *d.IsTime
	}
	return Datatype(datatype).IsTemporal()
}

// Dataset is the spec's logical dataset: one physical table or query, plus the
// fields defined over it.
type Dataset struct {
	Pos         Pos        `yaml:"-"`
	Name        string     `yaml:"name"`
	Source      string     `yaml:"source"`
	PrimaryKey  []string   `yaml:"primary_key"`
	UniqueKeys  [][]string `yaml:"unique_keys"`
	Description string     `yaml:"description"`
	AIContext   AIContext  `yaml:"ai_context"`
	Fields      []*Field   `yaml:"fields"`

	// Namespace is the workspace namespace that owns this dataset. Empty when
	// the model was loaded outside a workspace.
	Namespace string `yaml:"-"`
	// ImportedFrom records the `namespace.dataset` a workspace cloned this from,
	// and is empty for a dataset defined in its own namespace.
	//
	// Governance resolves policy against this origin rather than against the
	// local name. Without that, aliasing an import would silently detach a
	// column from the grant written against it, which would make `as:` an
	// access control bypass.
	ImportedFrom string `yaml:"-"`
}

// Origin returns the `namespace.dataset` this dataset is governed as: the place
// it was defined, not the name it is addressed by here.
func (d *Dataset) Origin() string {
	if d.ImportedFrom != "" {
		return d.ImportedFrom
	}
	if d.Namespace != "" {
		return d.Namespace + "." + d.Name
	}
	return d.Name
}

// Field looks up a field by name.
func (d *Dataset) Field(name string) (*Field, bool) {
	for _, f := range d.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return nil, false
}

// Grain returns the columns one row of this dataset is unique by, which the
// spec expresses as primary_key. The engine treats primary_key as the grain
// declaration because the spec carries no dedicated grain field.
func (d *Dataset) Grain() []string { return d.PrimaryKey }

// keySets returns every declared uniqueness constraint: the primary key plus
// each unique key. Used to decide whether a join can fan out.
func (d *Dataset) keySets() [][]string {
	var out [][]string
	if len(d.PrimaryKey) > 0 {
		out = append(out, d.PrimaryKey)
	}
	out = append(out, d.UniqueKeys...)
	return out
}

// HasUniquenessOn reports whether cols exactly match a declared primary or
// unique key, as a set. This is the spec's only evidence that a join to this
// dataset is many-to-one rather than many-to-many.
func (d *Dataset) HasUniquenessOn(cols []string) bool {
	for _, k := range d.keySets() {
		if sameSet(k, cols) {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

func (d *Dataset) UnmarshalYAML(n *yaml.Node) error {
	type plain Dataset
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*d = Dataset(p)
	d.Pos = Pos{Line: n.Line}
	for _, f := range d.Fields {
		f.Dataset = d
	}
	return nil
}

// Relationship is the spec's foreign-key edge. The spec defines `from` as the
// many side and `to` as the one side, so direction is meaningful and the engine
// does not infer it.
type Relationship struct {
	Pos         Pos       `yaml:"-"`
	Name        string    `yaml:"name"`
	From        string    `yaml:"from"`
	To          string    `yaml:"to"`
	FromColumns []string  `yaml:"from_columns"`
	ToColumns   []string  `yaml:"to_columns"`
	AIContext   AIContext `yaml:"ai_context"`
}

func (r *Relationship) UnmarshalYAML(n *yaml.Node) error {
	type plain Relationship
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*r = Relationship(p)
	r.Pos = Pos{Line: n.Line}
	return nil
}

// Metric is the spec's model-level metric. Its expression may reference fields
// across several datasets, which is why the planner derives the required join
// set from the parsed expression rather than from a declaration.
type Metric struct {
	Pos         Pos        `yaml:"-"`
	Name        string     `yaml:"name"`
	Expression  Expression `yaml:"expression"`
	Description string     `yaml:"description"`
	Datatype    string     `yaml:"datatype"`
	AIContext   AIContext  `yaml:"ai_context"`
}

func (m *Metric) UnmarshalYAML(n *yaml.Node) error {
	type plain Metric
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*m = Metric(p)
	m.Pos = Pos{Line: n.Line}
	return nil
}

// Model is one entry under the file's `semantic_model:` list.
type Model struct {
	Pos           Pos             `yaml:"-"`
	Name          string          `yaml:"name"`
	Description   string          `yaml:"description"`
	AIContext     AIContext       `yaml:"ai_context"`
	Datasets      []*Dataset      `yaml:"datasets"`
	Relationships []*Relationship `yaml:"relationships"`
	Metrics       []*Metric       `yaml:"metrics"`

	// SpecVersion is the file's top-level `version:` value.
	SpecVersion string `yaml:"-"`
	// Version is a content hash of the source files. Every query response
	// carries it so a disputed number can be traced to an exact model.
	Version string `yaml:"-"`
}

func (m *Model) UnmarshalYAML(n *yaml.Node) error {
	type plain Model
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*m = Model(p)
	m.Pos = Pos{Line: n.Line}
	return nil
}

// Dataset looks up a dataset by name.
func (m *Model) Dataset(name string) (*Dataset, bool) {
	for _, d := range m.Datasets {
		if d.Name == name {
			return d, true
		}
	}
	return nil, false
}

// Metric looks up a metric by name.
func (m *Model) Metric(name string) (*Metric, bool) {
	for _, x := range m.Metrics {
		if x.Name == name {
			return x, true
		}
	}
	return nil, false
}
