// Package plan turns a semantic request into a dialect-free plan, or refuses.
//
// Nothing in this package emits SQL. That separation is deliberate: correctness
// lives here and is unit testable without a database, and an emitter bug can
// never be mistaken for a planning bug.
package plan

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/resolve"
	"github.com/rk-chavali/truegrain/internal/rollup"
)

// Op is a structured filter operator. The set is closed: a filter is never a
// SQL fragment, which is what keeps an ungoverned predicate inexpressible.
type Op string

const (
	OpEq       Op = "eq"
	OpNe       Op = "ne"
	OpIn       Op = "in"
	OpNotIn    Op = "not_in"
	OpGt       Op = "gt"
	OpGte      Op = "gte"
	OpLt       Op = "lt"
	OpLte      Op = "lte"
	OpBetween  Op = "between"
	OpIsNull   Op = "is_null"
	OpIsNotNil Op = "is_not_null"
)

// arity is the number of values each operator requires. -1 means one or more.
var arity = map[Op]int{
	OpEq: 1, OpNe: 1, OpGt: 1, OpGte: 1, OpLt: 1, OpLte: 1,
	OpIn: -1, OpNotIn: -1,
	OpBetween: 2,
	OpIsNull:  0, OpIsNotNil: 0,
}

// Ops returns every supported operator, for interface discovery and errors.
func Ops() []Op {
	out := make([]Op, 0, len(arity))
	for op := range arity {
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Grain is a time truncation level.
type Grain string

const (
	GrainSecond  Grain = "second"
	GrainMinute  Grain = "minute"
	GrainHour    Grain = "hour"
	GrainDay     Grain = "day"
	GrainWeek    Grain = "week"
	GrainMonth   Grain = "month"
	GrainQuarter Grain = "quarter"
	GrainYear    Grain = "year"
)

var grains = []Grain{GrainSecond, GrainMinute, GrainHour, GrainDay, GrainWeek, GrainMonth, GrainQuarter, GrainYear}

// Grains returns every supported time grain.
func Grains() []Grain { return append([]Grain(nil), grains...) }

func validGrain(g Grain) bool {
	for _, x := range grains {
		if x == g {
			return true
		}
	}
	return false
}

// Filter is a structured predicate on a dimension.
type Filter struct {
	Dimension string `json:"dimension"`
	Op        Op     `json:"op"`
	Values    []any  `json:"values,omitempty"`
}

// Order is a sort instruction naming a requested metric or dimension by its
// output alias.
type Order struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// Request is the one vocabulary every interface speaks. There is no field in
// which a caller can put SQL.
type Request struct {
	Metrics    []string `json:"metrics"`
	Dimensions []string `json:"dimensions,omitempty"`
	Filters    []Filter `json:"filters,omitempty"`
	Grain      Grain    `json:"grain,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	OrderBy    []Order  `json:"order_by,omitempty"`
}

// DefaultLimit is applied when a request does not set one, so an agent cannot
// pull an unbounded result set by omission.
const DefaultLimit = 1000

// MaxLimit caps what a caller may ask for.
const MaxLimit = 100000

// JoinStep is one join in the plan, already oriented: Left is the side already
// in the query, Right is the side being added.
type JoinStep struct {
	Rel          *osi.Relationship
	Left, Right  *osi.Dataset
	LeftColumns  []string
	RightColumns []string

	// RightUnique reports whether the join columns are a declared key on the
	// right side. When they are not, each left row matches many right rows and
	// the join multiplies every row accumulated so far.
	RightUnique bool
	// LeftUnique reports whether the join columns are a declared key on the
	// left side. When they are not, many left rows point at the same right row,
	// so the right dataset's own rows repeat in the result even though the
	// result row count does not grow.
	LeftUnique bool
}

// Duplicates reports whether this step multiplies the rows accumulated so far.
func (j JoinStep) Duplicates() bool { return !j.RightUnique }

// DimSelect is one grouping column in the plan.
type DimSelect struct {
	Alias string
	Field *osi.Field
	// Grain is set only for a time dimension under an explicit grain request.
	Grain Grain
	// RollupColumn is the column to read when the plan reads a rollup, and
	// empty otherwise. The field is still here and still what governance
	// resolves against; this is only where the value lives.
	RollupColumn string
	// RollupGrain is the grain the rollup column is already stored at, so
	// an emitter knows whether it still has to truncate. Truncating a
	// column that is already truncated is harmless on every warehouse here
	// but reads as though the grain were not trusted.
	RollupGrain string
}

// MetricSelect is one aggregate in the plan.
type MetricSelect struct {
	Alias  string
	Metric *osi.Metric
	// RollupColumn is the stored partial aggregate, empty when the plan
	// reads the base fact.
	RollupColumn string
	// RollupAggregate combines the stored partials, and is not always the
	// metric's own function: a stored COUNT combines with SUM, because
	// counting the rollup's rows counts groups. See internal/rollup.
	RollupAggregate string
}

// BoundFilter is a filter resolved against a real field, with its values
// validated against that field's declared type.
type BoundFilter struct {
	Field  *osi.Field
	Op     Op
	Values []any
	// RollupColumn is where this field lives in the chosen rollup, empty
	// when the plan reads the base fact. The model field stays here and
	// stays what governance resolves against: a grant written on
	// customers.region must not stop applying because a rollup carries a
	// copy of the column.
	RollupColumn string
}

// BoundOrder is a sort resolved to an output alias present in the plan.
type BoundOrder struct {
	Alias string
	Desc  bool
}

// Plan is a dialect-free query plan. An emitter turns it into SQL; a governance
// gate inspects it before an emitter ever runs.
type Plan struct {
	ModelName    string
	ModelVersion string
	// Namespace is the workspace namespace this plan belongs to. The planner
	// does not know composition exists, so the engine fills it in before the
	// governance gate runs; it is the first facet an evidence query filters on.
	Namespace  string
	Base       *osi.Dataset
	Joins      []JoinStep
	Dimensions []DimSelect
	Metrics    []MetricSelect
	Filters    []BoundFilter
	OrderBy    []BoundOrder
	Limit      int

	// Rollup is set when this plan reads a pre-aggregated table instead of
	// the base fact. Nil is the ordinary case and the one every emitter
	// handled before rollups existed.
	//
	// When it is set, Base and Joins are still filled in with what the plan
	// would have been. The governance gate inspects a plan's fields to
	// decide column access, and a rollup is a copy of those same columns:
	// resolving policy against the rollup's own table instead would mean a
	// grant written on customers.region stopped applying the moment a
	// rollup carried it. So the gate keeps seeing the model, and only the
	// emitter sees the rollup.
	Rollup *RollupUse
}

// Columns returns the output column aliases in order: dimensions then metrics.
func (p *Plan) Columns() []string {
	out := make([]string, 0, len(p.Dimensions)+len(p.Metrics))
	for _, d := range p.Dimensions {
		out = append(out, d.Alias)
	}
	for _, m := range p.Metrics {
		out = append(out, m.Alias)
	}
	return out
}

// Datasets returns every dataset the plan touches, base first then join order.
func (p *Plan) Datasets() []*osi.Dataset {
	out := []*osi.Dataset{p.Base}
	for _, j := range p.Joins {
		out = append(out, j.Right)
	}
	return out
}

// Fields returns every field the plan reads: the dimensions it groups by, the
// fields its metrics aggregate, and the fields its filters restrict. This is
// the set the governance gate resolves access against, so it must be complete.
func (p *Plan) Fields(s *resolve.Schema) []*osi.Field {
	seen := map[*osi.Field]bool{}
	var out []*osi.Field
	add := func(f *osi.Field) {
		if f != nil && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, d := range p.Dimensions {
		add(d.Field)
	}
	for _, f := range p.Filters {
		add(f.Field)
	}
	for _, m := range p.Metrics {
		for _, id := range osi.Refs(m.Metric.Expression.AST) {
			if len(id.Norm) == 2 {
				if f, ok := s.Field(id.Parts[0] + "." + id.Parts[1]); ok {
					add(f)
				}
			}
		}
	}
	return out
}

// Retry tells a caller what to do next. It exists because an agent deciding
// whether to try again is the most common consumer of a refusal, and the three
// cases need entirely different behaviour: change the request, wait and repeat
// it unchanged, or stop. Without this an agent either gives up on a typo or
// loops forever on a denial.
type Retry string

const (
	// RetryModify means the request was understood and cannot be answered as
	// written. Changing the arguments may work; repeating it will not.
	RetryModify Retry = "modify"
	// RetryLater means the same request may succeed later. Something outside
	// the request failed, such as the policy source being unreachable.
	RetryLater Retry = "later"
	// RetryNever means no version of this request from this caller will
	// succeed. A denial, or something the model itself has to change.
	RetryNever Retry = "never"
)

// Refusal is a structured denial. Every interface renders it as a refusal with
// a reason rather than as an empty result, because a silent empty answer trains
// a caller, human or agent, to report a wrong number confidently.
type Refusal struct {
	Code    string
	Subject string
	Reason  string
	Hint    string
}

// Retry says what a caller should do about this refusal. It is derived from the
// code rather than stored, so no construction site can forget to set it and no
// two refusals with the same code can disagree.
func (r *Refusal) Retry() Retry { return RetryFor(r.Code) }

// retryByCode maps every refusal code to what a caller should do about it.
//
// Anything absent defaults to RetryNever, which is the safe direction: an
// unclassified failure that an agent retries forever is worse than one it
// gives up on.
var retryByCode = map[string]Retry{
	// The request is answerable, just not as written.
	CodeUnknownMetric:    RetryModify,
	CodeUnknownDimension: RetryModify,
	CodeNoMetrics:        RetryModify,
	CodeBadFilter:        RetryModify,
	CodeBadGrain:         RetryModify,
	CodeBadOrder:         RetryModify,
	CodeBadLimit:         RetryModify,
	// The model cannot answer this shape, but a different metric can, and the
	// hint names which.
	CodeFanOut: RetryModify,

	// The model itself has to change. Retrying with different arguments will
	// not help, and an agent should say so rather than keep guessing.
	CodeNoJoinPath:    RetryNever,
	CodeAmbiguousJoin: RetryNever,
	CodeUnsupported:   RetryNever,
}

// RetryFor classifies a refusal code. Packages that define their own codes
// register them with RegisterRetry.
func RetryFor(code string) Retry {
	if r, ok := retryByCode[code]; ok {
		return r
	}
	return RetryNever
}

// RegisterRetry lets a package that defines its own refusal codes classify
// them, so the table stays next to the code that produces it.
func RegisterRetry(code string, r Retry) { retryByCode[code] = r }

// CodeOf returns the refusal code err carries, or empty if it is not a
// refusal. It exists so a caller can classify a failure without repeating the
// errors.As dance at every site that wants to count one.
func CodeOf(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// IsRegisteredCode reports whether code is one this build knows about.
//
// Telemetry uses it to decide whether a code is safe to attach to a metric.
// The registry is closed at init, so a registered code is drawn from a fixed
// set, while anything else could be attacker chosen and would let a caller
// grow the label space of a metrics backend without bound.
func IsRegisteredCode(code string) bool {
	_, ok := retryByCode[code]
	return ok
}

func (r *Refusal) Error() string {
	var b strings.Builder
	b.WriteString(r.Code)
	if r.Subject != "" {
		b.WriteString(" [" + r.Subject + "]")
	}
	b.WriteString(": ")
	b.WriteString(r.Reason)
	if r.Hint != "" {
		b.WriteString("\n  hint: " + r.Hint)
	}
	return b.String()
}

// Refusal codes. These are part of the interface contract: a caller, and
// especially an agent deciding whether to retry, branches on the code.
const (
	CodeUnknownMetric    = "unknown_metric"
	CodeUnknownDimension = "unknown_dimension"
	CodeNoMetrics        = "no_metrics"
	CodeBadFilter        = "invalid_filter"
	CodeBadGrain         = "invalid_grain"
	CodeBadOrder         = "invalid_order"
	CodeBadLimit         = "invalid_limit"
	CodeNoJoinPath       = "no_join_path"
	CodeAmbiguousJoin    = "ambiguous_join_path"
	CodeFanOut           = "fan_out_would_inflate"
	CodeUnsupported      = "unsupported"
)

// Planner builds plans against one resolved schema.
type Planner struct {
	schema *resolve.Schema
	// rollups are the pre-aggregated tables this namespace declares, already
	// validated against the schema. Empty for a namespace with none, which
	// is every namespace that existed before rollups did.
	rollups []*rollup.Rollup
}

// New returns a planner over a resolved schema.
func New(s *resolve.Schema) *Planner { return &Planner{schema: s} }

// NewWithRollups builds a planner that may read pre-aggregated tables.
//
// Separate from New rather than a field set afterwards, because a planner
// that acquired rollups after someone already held it would answer the same
// question two different ways depending on when they asked.
func NewWithRollups(s *resolve.Schema, rs []*rollup.Rollup) *Planner {
	return &Planner{schema: s, rollups: rs}
}

// Rollups are the pre-aggregated tables this planner may read, for health
// to report.
func (p *Planner) Rollups() []*rollup.Rollup { return p.rollups }

// Schema exposes the underlying schema for interfaces that list metadata.
func (p *Planner) Schema() *resolve.Schema { return p.schema }

// Plan validates a request and produces a plan, or refuses with a reason.
func (p *Planner) Plan(req Request) (*Plan, error) { return p.PlanWith(req, nil) }

// PlanWith plans a request, optionally reading a pre-aggregated table.
//
// allowed names the rollups the caller has verified are fresh enough to
// read. Nil means none, which is what Plan passes and what every path
// without a warehouse gets: a rollup nothing has checked is never read.
//
// The plan is built against the model either way, including the base
// dataset and the join graph, and only then rewritten to read the rollup.
// That ordering is the safety property: the fan-out check, the grain check
// and the governance gate all run on the real model, so a rollup cannot
// make a question answerable that was not, and cannot make a column legible
// that was not.
func (p *Planner) PlanWith(req Request, allowed []*rollup.Rollup) (*Plan, error) {
	metrics, err := p.bindMetrics(req.Metrics)
	if err != nil {
		return nil, err
	}
	dims, err := p.bindDimensions(req.Dimensions, req.Grain)
	if err != nil {
		return nil, err
	}
	filters, err := p.bindFilters(req.Filters)
	if err != nil {
		return nil, err
	}
	limit, err := boundLimit(req.Limit)
	if err != nil {
		return nil, err
	}

	needed := p.requiredDatasets(metrics, dims, filters)
	bases, err := p.candidateBases(metrics, needed)
	if err != nil {
		return nil, err
	}

	// Which dataset the FROM clause starts from changes which datasets end up
	// with repeated rows, so a query that is unplannable from one base can be
	// correct from another. Try each valid base and keep the first that every
	// requested metric survives; refuse with the best-placed base's reason only
	// when none does.
	var base *osi.Dataset
	var joins []JoinStep
	var refusal error
	for _, candidate := range bases {
		js, err := p.buildJoins(candidate, needed)
		if err != nil {
			if refusal == nil {
				refusal = err
			}
			continue
		}
		if err := p.checkFanOut(metrics, candidate, js); err != nil {
			if refusal == nil {
				refusal = err
			}
			continue
		}
		base, joins = candidate, js
		break
	}
	if base == nil {
		return nil, refusal
	}

	pl := &Plan{
		ModelName:    p.schema.Model.Name,
		ModelVersion: p.schema.Model.Version,
		Base:         base,
		Joins:        joins,
		Dimensions:   dims,
		Metrics:      metrics,
		Filters:      filters,
		Limit:        limit,
	}
	if pl.OrderBy, err = bindOrder(req.OrderBy, pl.Columns()); err != nil {
		return nil, err
	}
	applyRollup(pl, allowed)
	return pl, nil
}

func (p *Planner) bindMetrics(names []string) ([]MetricSelect, error) {
	if len(names) == 0 {
		return nil, &Refusal{Code: CodeNoMetrics,
			Reason: "a request must name at least one metric",
			Hint:   "call list_metrics to see what is available"}
	}
	seen := map[string]bool{}
	out := make([]MetricSelect, 0, len(names))
	for _, n := range names {
		m, ok := p.schema.Metric(n)
		if !ok {
			return nil, &Refusal{Code: CodeUnknownMetric, Subject: n,
				Reason: fmt.Sprintf("no metric named %q", n),
				Hint:   suggest(n, p.metricNames())}
		}
		if seen[m.Name] {
			continue // asking twice is harmless; return it once
		}
		seen[m.Name] = true
		out = append(out, MetricSelect{Alias: m.Name, Metric: m})
	}
	return out, nil
}

func (p *Planner) bindDimensions(names []string, grain Grain) ([]DimSelect, error) {
	if grain != "" && !validGrain(grain) {
		return nil, &Refusal{Code: CodeBadGrain, Subject: string(grain),
			Reason: fmt.Sprintf("%q is not a supported grain", grain),
			Hint:   "supported grains: " + joinGrains()}
	}
	seen := map[string]bool{}
	out := make([]DimSelect, 0, len(names))
	for _, n := range names {
		f, ok := p.schema.Field(n)
		if !ok || !f.IsDimension {
			return nil, &Refusal{Code: CodeUnknownDimension, Subject: n,
				Reason: fmt.Sprintf("no dimension named %q", n),
				Hint:   suggest(n, p.dimensionNames())}
		}
		alias := f.Name
		if seen[alias] {
			alias = f.Dataset.Name + "_" + f.Name
		}
		seen[alias] = true

		d := DimSelect{Alias: alias, Field: f}
		if f.IsTime && grain != "" {
			d.Grain = grain
		}
		out = append(out, d)
	}
	if grain != "" && !hasTimeDim(out) {
		return nil, &Refusal{Code: CodeBadGrain, Subject: string(grain),
			Reason: fmt.Sprintf("grain %q was requested but no time dimension was selected", grain),
			Hint:   "add a time dimension to `dimensions`, or drop `grain`"}
	}
	return out, nil
}

func hasTimeDim(dims []DimSelect) bool {
	for _, d := range dims {
		if d.Field.IsTime {
			return true
		}
	}
	return false
}

func (p *Planner) bindFilters(fs []Filter) ([]BoundFilter, error) {
	out := make([]BoundFilter, 0, len(fs))
	for _, f := range fs {
		field, ok := p.schema.Field(f.Dimension)
		if !ok || !field.IsDimension {
			return nil, &Refusal{Code: CodeUnknownDimension, Subject: f.Dimension,
				Reason: fmt.Sprintf("filter names no known dimension %q", f.Dimension),
				Hint:   suggest(f.Dimension, p.dimensionNames())}
		}
		want, known := arity[f.Op]
		if !known {
			return nil, &Refusal{Code: CodeBadFilter, Subject: f.Dimension,
				Reason: fmt.Sprintf("unsupported operator %q", f.Op),
				Hint:   "supported operators: " + joinOps()}
		}
		switch {
		case want >= 0 && len(f.Values) != want:
			return nil, &Refusal{Code: CodeBadFilter, Subject: f.Dimension,
				Reason: fmt.Sprintf("operator %q takes %d value(s), got %d", f.Op, want, len(f.Values))}
		case want == -1 && len(f.Values) == 0:
			return nil, &Refusal{Code: CodeBadFilter, Subject: f.Dimension,
				Reason: fmt.Sprintf("operator %q needs at least one value", f.Op)}
		}
		values, err := normalizeValues(field, f.Op, f.Values)
		if err != nil {
			return nil, err
		}
		out = append(out, BoundFilter{Field: field, Op: f.Op, Values: values})
	}
	return out, nil
}

func boundLimit(n int) (int, error) {
	switch {
	case n == 0:
		return DefaultLimit, nil
	case n < 0:
		return 0, &Refusal{Code: CodeBadLimit,
			Reason: fmt.Sprintf("limit must be positive, got %d", n)}
	case n > MaxLimit:
		return 0, &Refusal{Code: CodeBadLimit,
			Reason: fmt.Sprintf("limit %d exceeds the maximum of %d", n, MaxLimit),
			Hint:   "narrow the request with filters or a coarser grain"}
	}
	return n, nil
}

// BindOrder validates sort instructions against a column list. The engine uses
// it for a multi-fact query, where the columns come from every part combined
// and no single part knows them all.
func BindOrder(orders []Order, columns []string) ([]BoundOrder, error) {
	return bindOrder(orders, columns)
}

func bindOrder(orders []Order, columns []string) ([]BoundOrder, error) {
	valid := map[string]bool{}
	for _, c := range columns {
		valid[c] = true
	}
	out := make([]BoundOrder, 0, len(orders))
	for _, o := range orders {
		if !valid[o.Field] {
			return nil, &Refusal{Code: CodeBadOrder, Subject: o.Field,
				Reason: fmt.Sprintf("cannot order by %q because it is not in the result", o.Field),
				Hint:   "order_by names an output column: " + strings.Join(columns, ", ")}
		}
		out = append(out, BoundOrder{Alias: o.Field, Desc: o.Desc})
	}
	return out, nil
}

// requiredDatasets is every dataset the query must reach.
func (p *Planner) requiredDatasets(ms []MetricSelect, dims []DimSelect, fs []BoundFilter) map[*osi.Dataset]bool {
	need := map[*osi.Dataset]bool{}
	for _, m := range ms {
		for _, d := range p.schema.MetricDatasets(m.Metric) {
			need[d] = true
		}
	}
	for _, d := range dims {
		need[d.Field.Dataset] = true
	}
	for _, f := range fs {
		need[f.Field.Dataset] = true
	}
	return need
}

// candidateBases returns every dataset the FROM clause could start from, most
// promising first.
//
// A base is valid only if every other required dataset is reachable from it by
// exactly one join path. Datasets a metric aggregates over come first, because
// starting at the fact is what keeps its rows unique in the result.
func (p *Planner) candidateBases(ms []MetricSelect, need map[*osi.Dataset]bool) ([]*osi.Dataset, error) {
	metricDatasets := map[*osi.Dataset]bool{}
	for _, m := range ms {
		for _, d := range p.schema.MetricDatasets(m.Metric) {
			metricDatasets[d] = true
		}
	}
	candidates := sortedDatasets(need)
	if len(candidates) == 0 {
		return nil, &Refusal{Code: CodeNoMetrics,
			Reason: "the request resolves to no datasets",
			Hint:   "the selected metrics reference no fields, which means the model is malformed"}
	}

	var valid []*osi.Dataset
	var unreachable []string
	for _, c := range candidates {
		ok := true
		for _, other := range candidates {
			if other == c {
				continue
			}
			switch len(p.schema.Paths(c, other)) {
			case 1:
				// reachable, unambiguously
			case 0:
				ok = false
				unreachable = append(unreachable, fmt.Sprintf("%s to %s", c.Name, other.Name))
			default:
				return nil, &Refusal{Code: CodeAmbiguousJoin,
					Subject: c.Name + " to " + other.Name,
					Reason:  "more than one join path connects these datasets",
					Hint:    "the engine never guesses a join path; change the model so only one path connects them"}
			}
			if !ok {
				break
			}
		}
		if ok {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		sort.Strings(unreachable)
		return nil, &Refusal{Code: CodeNoJoinPath,
			Reason: "the requested metrics and dimensions span datasets with no join path between them (" +
				strings.Join(dedupe(unreachable), ", ") + ")",
			Hint: "add a relationship to the model, or drop the dimension that reaches the unconnected dataset"}
	}
	sort.SliceStable(valid, func(i, j int) bool {
		return metricDatasets[valid[i]] && !metricDatasets[valid[j]]
	})
	return valid, nil
}

// buildJoins expands the unique path from base to every other required dataset
// into an ordered, deduplicated list of join steps.
func (p *Planner) buildJoins(base *osi.Dataset, need map[*osi.Dataset]bool) ([]JoinStep, error) {
	joined := map[*osi.Dataset]bool{base: true}
	var steps []JoinStep

	for _, target := range sortedDatasets(need) {
		if target == base {
			continue
		}
		paths := p.schema.Paths(base, target)
		if len(paths) != 1 {
			// chooseBase already proved this is 1; defend anyway rather than
			// index into an empty slice if that ever stops being true.
			return nil, &Refusal{Code: CodeNoJoinPath,
				Subject: base.Name + " to " + target.Name,
				Reason:  fmt.Sprintf("expected exactly one join path, found %d", len(paths))}
		}
		cur := base
		for _, e := range paths[0] {
			next := e.Other(cur)
			if !joined[next] {
				near, far := e.ColumnsFrom(cur)
				steps = append(steps, JoinStep{
					Rel:          e.Rel,
					Left:         cur,
					Right:        next,
					LeftColumns:  near,
					RightColumns: far,
					RightUnique:  next.HasUniquenessOn(far),
					LeftUnique:   cur.HasUniquenessOn(near),
				})
				joined[next] = true
			}
			cur = next
		}
	}
	return steps, nil
}

// repetition explains why one dataset's rows appear more than once in a result.
type repetition struct {
	rel *osi.Relationship
	why string
	// finer is the dataset whose grain caused the repetition. A metric defined
	// over it can answer the question the caller was really asking, so the
	// refusal names those metrics instead of only saying no.
	finer *osi.Dataset
}

// repeatedDatasets works out, for each dataset in the plan, whether its own rows
// appear more than once in the joined result, and why.
//
// Two different things cause it, and an engine that models only the first gets
// the chasm case wrong:
//
//  1. A one-to-many join multiplies every row accumulated so far. Joining
//     orders to its order_lines child repeats each order once per line.
//  2. A many-to-one join repeats the parent's rows whenever several child rows
//     point at the same parent. Starting from order_lines and joining up to
//     orders repeats each order once per line as well, even though the result
//     row count never grows.
//
// Both produce the same wrong number from a SUM over orders. The second is the
// one that hides, because no join in the plan looks like it fans out.
func repeatedDatasets(base *osi.Dataset, joins []JoinStep) map[*osi.Dataset]repetition {
	repeated := map[*osi.Dataset]repetition{}
	placed := []*osi.Dataset{base}

	for _, j := range joins {
		if !j.RightUnique {
			// Case 1: everything already in the query gets multiplied.
			for _, d := range placed {
				if _, already := repeated[d]; !already {
					repeated[d] = repetition{rel: j.Rel, finer: j.Right, why: fmt.Sprintf(
						"%s is not unique on %s, so joining it repeats every %s row once per matching %s row",
						j.Right.Name, strings.Join(j.RightColumns, ", "), d.Name, j.Right.Name)}
				}
			}
		}
		switch {
		case !j.LeftUnique:
			// Case 2: many left rows share one right row.
			repeated[j.Right] = repetition{rel: j.Rel, finer: j.Left, why: fmt.Sprintf(
				"%s is not unique on %s, so several %s rows match the same %s row and it is repeated",
				j.Left.Name, strings.Join(j.LeftColumns, ", "), j.Left.Name, j.Right.Name)}
		default:
			// The right side inherits whatever repetition the left side has.
			if r, ok := repeated[j.Left]; ok {
				repeated[j.Right] = r
			}
		}
		placed = append(placed, j.Right)
	}
	return repeated
}

// checkFanOut is the correctness gate. An aggregate may only read a dataset
// whose rows are unique in the result, unless the aggregate is one that
// survives row duplication. Anything else returns a number that is silently too
// large, which is the single failure mode that kills trust in a semantic layer.
func (p *Planner) checkFanOut(ms []MetricSelect, base *osi.Dataset, joins []JoinStep) error {
	repeated := repeatedDatasets(base, joins)
	if len(repeated) == 0 {
		return nil
	}
	for _, m := range ms {
		for _, agg := range osi.Aggregates(m.Metric.Expression.AST) {
			if osi.SurvivesFanOut(agg) {
				continue
			}
			for _, d := range p.aggregateDatasets(agg) {
				r, isRepeated := repeated[d]
				if !isRepeated {
					continue
				}
				// ponytail: v1 refuses. Aggregating the fact to its own grain
				// in a subquery before joining would answer this correctly and
				// is the v2 upgrade path. Refusing is the version that cannot
				// be silently wrong.
				return &Refusal{Code: CodeFanOut, Subject: m.Metric.Name,
					Reason: fmt.Sprintf(
						"metric %q aggregates %s(...) over %s, but this query repeats %s rows: %s",
						m.Metric.Name, agg.Name, d.Name, d.Name, r.why),
					Hint: p.fanOutHint(r),
				}
			}
		}
	}
	return nil
}

// fanOutHint tells the caller what to do instead.
//
// The useful answer is usually not "drop a dimension" but "ask a metric defined\n// at the finer grain", so when such a metric exists it is named. Revenue by
// product category is undefined on the order header and well defined on the
// order line, and pointing at the line metric is the difference between a dead
// end and an answer.
func (p *Planner) fanOutHint(r repetition) string {
	var b strings.Builder
	b.WriteString("the total would be inflated, so the engine will not run it.")

	if r.finer != nil {
		if alts := p.metricsOver(r.finer); len(alts) > 0 {
			fmt.Fprintf(&b, " Metrics defined at the %s grain answer this correctly: %s.",
				r.finer.Name, strings.Join(alts, ", "))
		}
	}
	fmt.Fprintf(&b,
		" Otherwise drop the dimensions or metrics that pull in relationship %q,"+
			" or use an aggregate that survives duplication such as COUNT(DISTINCT ...), MIN or MAX.",
		r.rel.Name)
	return b.String()
}

// metricsOver lists the metrics whose aggregates read a dataset, which is how
// the engine offers a metric at the grain the caller actually needs.
func (p *Planner) metricsOver(d *osi.Dataset) []string {
	var out []string
	for _, m := range p.schema.Model.Metrics {
		for _, agg := range osi.Aggregates(m.Expression.AST) {
			if containsDataset(p.aggregateDatasets(agg), d) {
				out = append(out, m.Name)
				break
			}
		}
	}
	sort.Strings(out)
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

func containsDataset(ds []*osi.Dataset, want *osi.Dataset) bool {
	for _, d := range ds {
		if d == want {
			return true
		}
	}
	return false
}

// aggregateDatasets returns the datasets an aggregate call reads.
func (p *Planner) aggregateDatasets(agg *osi.Call) []*osi.Dataset {
	seen := map[*osi.Dataset]bool{}
	for _, id := range osi.Refs(agg) {
		if len(id.Norm) != 2 {
			continue
		}
		if f, ok := p.schema.Field(id.Parts[0] + "." + id.Parts[1]); ok {
			seen[f.Dataset] = true
		}
	}
	return sortedDatasets(seen)
}

func sortedDatasets(set map[*osi.Dataset]bool) []*osi.Dataset {
	out := make([]*osi.Dataset, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (p *Planner) metricNames() []string {
	out := make([]string, 0, len(p.schema.Model.Metrics))
	for _, m := range p.schema.Model.Metrics {
		out = append(out, m.Name)
	}
	return out
}

func (p *Planner) dimensionNames() []string {
	dims := p.schema.Dimensions()
	out := make([]string, 0, len(dims))
	for _, f := range dims {
		out = append(out, f.QualifiedName())
	}
	return out
}

func joinOps() string {
	ops := Ops()
	out := make([]string, len(ops))
	for i, o := range ops {
		out[i] = string(o)
	}
	return strings.Join(out, ", ")
}

func joinGrains() string {
	out := make([]string, len(grains))
	for i, g := range grains {
		out[i] = string(g)
	}
	return strings.Join(out, ", ")
}

// suggest offers the closest known names. An agent reads the hint and retries,
// so a hint that names real alternatives saves a round trip.
func suggest(given string, known []string) string {
	near := closest(given, known, 3)
	if len(near) == 0 {
		if len(known) == 0 {
			return "the model declares none"
		}
		return "available: " + strings.Join(known, ", ")
	}
	return "did you mean " + strings.Join(near, ", ") + "?"
}

// closest ranks candidates by edit distance, keeping only plausible matches.
func closest(given string, known []string, n int) []string {
	type scored struct {
		name string
		d    int
	}
	g := strings.ToLower(given)
	var all []scored
	for _, k := range known {
		d := levenshtein(g, strings.ToLower(k))
		// A candidate is plausible if it is within a third of its length, or
		// if one name contains the other.
		if d <= 1+len(k)/3 || strings.Contains(strings.ToLower(k), g) {
			all = append(all, scored{k, d})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].d != all[j].d {
			return all[i].d < all[j].d
		}
		return all[i].name < all[j].name
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]string, len(all))
	for i, s := range all {
		out[i] = s.name
	}
	return out
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
