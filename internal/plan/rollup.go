package plan

import (
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/rollup"
)

// Choosing a rollup, which is choosing whether to answer from a second copy
// of the data.
//
// Every rule here is a reason not to route. That asymmetry is deliberate:
// the base fact is always correct and always available, so a rollup has to
// earn the read, and anything this cannot prove is a fallback rather than a
// refusal. A caller never sees a rollup decision go wrong, they see a
// slower query.
//
// The freshness half is not here. It needs a warehouse and the planner has
// none, so the engine checks it and passes down which rollups are allowed.
// Keeping the structural rules pure means they are testable without a
// database, which is the half most likely to be subtly wrong.

// RollupUse records that a plan reads a pre-aggregated table.
//
// Carried on the plan so that the emitter knows what to write and, just as
// importantly, so the name reaches the audit event and the compiled output.
// A number that came from a rollup and cannot be traced back to it is the
// one thing worse than not having rollups.
type RollupUse struct {
	Name   string
	Source string
	// Reaggregated is true when the request groups by fewer dimensions than
	// the rollup stores, so partial aggregates are being combined. False
	// means each rollup row is one output row and the stored values are
	// read as they are.
	Reaggregated bool
}

// rollupPlan is a chosen rollup with the column mapping for this request.
type rollupPlan struct {
	use     RollupUse
	metrics map[*osi.Metric]*rollup.Metric
	dims    map[*osi.Field]*rollup.Dimension
}

// CandidateRollups returns every rollup that structurally covers a request,
// best first, ignoring freshness.
//
// The engine calls this, checks freshness on the candidates in order, and
// plans with the first that passes. Splitting it this way means a stale
// rollup falls through to the next one rather than straight to the base
// fact, which matters where a daily and an hourly rollup both cover a
// question and only one has finished building.
func (p *Planner) CandidateRollups(req Request) []*rollup.Rollup {
	if len(p.rollups) == 0 {
		return nil
	}
	metrics, err := p.bindMetrics(req.Metrics)
	if err != nil {
		return nil
	}
	dims, err := p.bindDimensions(req.Dimensions, req.Grain)
	if err != nil {
		return nil
	}
	filters, err := p.bindFilters(req.Filters)
	if err != nil {
		return nil
	}

	var out []*rollup.Rollup
	for _, r := range p.rollups {
		if _, ok := coverage(r, metrics, dims, filters); ok {
			out = append(out, r)
		}
	}

	// Narrowest first: a rollup storing fewer dimensions holds fewer rows,
	// so it is the cheaper read and the one to try first. Ties break on name
	// so the choice is the same on every replica, which matters because two
	// replicas answering the same question from different tables is a
	// support call nobody can reproduce.
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].Dimensions) != len(out[j].Dimensions) {
			return len(out[i].Dimensions) < len(out[j].Dimensions)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// coverage reports whether a rollup can answer this request, and with which
// columns.
//
// Five conditions, and failing any one of them is a fallback:
//
//   - every requested metric is stored here
//   - every requested dimension is stored here
//   - a time dimension is requested at the stored grain or coarser
//   - every filtered field is stored here, because a filter on a column the
//     rollup does not carry cannot be applied and dropping it would return
//     rows the caller excluded
//   - every requested metric is distributive, which Validate already
//     enforced at load, so this is the assertion that it stayed true
func coverage(r *rollup.Rollup, metrics []MetricSelect, dims []DimSelect, filters []BoundFilter) (*rollupPlan, bool) {
	rp := &rollupPlan{
		use:     RollupUse{Name: r.Name, Source: r.Source},
		metrics: map[*osi.Metric]*rollup.Metric{},
		dims:    map[*osi.Field]*rollup.Dimension{},
	}

	for _, m := range metrics {
		stored, ok := r.Metric(m.Metric.Name)
		if !ok || stored.Reaggregate == "" {
			return nil, false
		}
		rp.metrics[m.Metric] = stored
	}

	grouped := map[*osi.Field]bool{}
	for _, d := range dims {
		stored, ok := r.Dimension(d.Field)
		if !ok {
			return nil, false
		}
		if d.Field.IsTime && !grainAtLeastAsCoarse(d.Grain, stored.Grain) {
			// The rollup stores days and the caller asked for hours. The
			// detail is gone, and answering with days relabelled as hours
			// would be a wrong number with the right column heading.
			return nil, false
		}
		rp.dims[d.Field] = stored
		grouped[d.Field] = true
	}

	for _, f := range filters {
		stored, ok := r.Dimension(f.Field)
		if !ok {
			return nil, false
		}
		if f.Field.IsTime && !filterFitsGrain(f, stored.Grain) {
			return nil, false
		}
		rp.dims[f.Field] = stored
	}

	// Anything the rollup groups by that this request does not is being
	// summed away, which is the case that needs the metrics to be
	// distributive. Validate refuses a non-distributive metric at load, so
	// reaching here with one is a bug rather than a configuration mistake,
	// and the check above already caught it.
	rp.use.Reaggregated = len(rp.dims) < len(r.Dimensions)
	return rp, true
}

// grainAtLeastAsCoarse reports whether a request's grain can be produced
// from a stored one.
//
// Day rolls up to month. Month does not roll down to day. An empty request
// grain means the caller did not ask for a time bucket at all, which is the
// coarsest thing there is and is always producible.
func grainAtLeastAsCoarse(requested Grain, stored string) bool {
	if requested == "" {
		return true
	}
	have := Grain(strings.ToLower(stored))

	// Week is not on the same ladder as the rest and is handled first,
	// because putting it on the ladder is the mistake that looks right. A
	// week is producible from days, hours and seconds. Nothing is
	// producible from weeks except weeks: a week straddles a month
	// boundary, so summing weeks into months double counts one and drops
	// another, and the total looks plausible either way.
	switch {
	case requested == GrainWeek:
		return grainRank[have] > 0 && grainRank[have] <= grainRank[GrainDay]
	case have == GrainWeek:
		return false
	}

	want, ok := grainRank[requested]
	if !ok {
		return false
	}
	from, ok := grainRank[have]
	if !ok {
		// A stored grain this engine does not know. Nothing can be
		// concluded about it, so nothing is.
		return false
	}
	return want >= from
}

// filterFitsGrain reports whether a filter on a time dimension can be
// applied to a column stored at a coarser grain.
//
// The rule is that the stored grain must be no coarser than the precision
// of the values being compared. A filter for one day against a table
// stored by month cannot be evaluated: the month bucket contains days
// outside the filter and days inside it, and keeping or dropping the whole
// bucket is wrong in opposite directions. Falling back is the only correct
// answer, and it is cheap.
func filterFitsGrain(f BoundFilter, stored string) bool {
	have, known := grainRank[Grain(strings.ToLower(stored))]
	if !known {
		return false
	}
	for _, v := range f.Values {
		var precision int
		switch v.(type) {
		case Date:
			precision = grainRank[GrainDay]
		case nil:
			// IS NULL and IS NOT NULL carry no precision and survive any
			// grain: a bucket is null or it is not.
			continue
		default:
			// A timestamp, or anything else this does not recognise.
			// Second precision is the conservative reading and it means
			// only a rollup stored at second grain can take the filter.
			precision = grainRank[GrainSecond]
		}
		if have > precision {
			return false
		}
	}
	return true
}

// grainRank orders the grains from finest to coarsest, so a comparison is
// one integer rather than a table of cases. Week is absent on purpose and
// is handled above.
var grainRank = map[Grain]int{
	GrainSecond:  1,
	GrainMinute:  2,
	GrainHour:    3,
	GrainDay:     4,
	GrainMonth:   6,
	GrainQuarter: 7,
	GrainYear:    8,
}

// applyRollup rewrites a finished plan to read a pre-aggregated table, when
// one of the allowed rollups covers it.
//
// After the plan is built rather than instead of building it. Everything
// that makes an answer correct has already run by here: the fan-out check
// refused the questions that cannot be answered, the join graph established
// which datasets are involved, and the fields are the model's own, which is
// what the governance gate will resolve policy against. A rollup can make a
// correct answer cheaper. It cannot make a refused question answerable, and
// this ordering is why.
func applyRollup(pl *Plan, allowed []*rollup.Rollup) {
	if len(allowed) == 0 {
		return
	}
	for _, r := range allowed {
		rp, ok := coverage(r, pl.Metrics, pl.Dimensions, pl.Filters)
		if !ok {
			continue
		}

		// Resolved in full before anything is written. A half-rewritten
		// plan, some selects pointing at the rollup and some at the model,
		// would be a mixture no emitter is prepared for, and the way to
		// have none of those is to leave the plan alone until every column
		// is known.
		metricColumns := make([]*rollup.Metric, len(pl.Metrics))
		for i, m := range pl.Metrics {
			if metricColumns[i] = rp.metrics[m.Metric]; metricColumns[i] == nil {
				return
			}
		}
		dimColumns := make([]*rollup.Dimension, len(pl.Dimensions))
		for i, d := range pl.Dimensions {
			if dimColumns[i] = rp.dims[d.Field]; dimColumns[i] == nil {
				return
			}
		}
		filterColumns := make([]*rollup.Dimension, len(pl.Filters))
		for i, f := range pl.Filters {
			// A filtered field with no column would mean the predicate is
			// dropped, which returns rows the caller excluded. coverage
			// already established every one of them is here, so this is the
			// assertion rather than the handling.
			if filterColumns[i] = rp.dims[f.Field]; filterColumns[i] == nil {
				return
			}
		}

		for i := range pl.Metrics {
			pl.Metrics[i].RollupColumn = metricColumns[i].Column
			pl.Metrics[i].RollupAggregate = metricColumns[i].Reaggregate
		}
		for i := range pl.Dimensions {
			pl.Dimensions[i].RollupColumn = dimColumns[i].Column
			pl.Dimensions[i].RollupGrain = dimColumns[i].Grain
		}
		for i := range pl.Filters {
			pl.Filters[i].RollupColumn = filterColumns[i].Column
		}
		use := rp.use
		pl.Rollup = &use
		return
	}
}
