package osi

// Aggregate classification, derived from the tables in
// core-spec/expression_language.md.
//
// This is the engine's additivity model. The Ossie spec carries no additivity
// field on a metric, so rather than requiring a proprietary extension (which
// would mean models had to be authored for this engine specifically), the
// engine derives additivity from the aggregate function the author already
// wrote. A stock Ossie model works unmodified.
//
// Semi-additive measures (balances, inventory on hand) are the one concept this
// derivation cannot recover, because nothing in the expression distinguishes
// them. They are refused rather than guessed. See COMPLIANCE.md.

// Decomposability is the spec's own classification of how an aggregate behaves
// under multi-stage aggregation.
type Decomposability int

const (
	// NotAggregate is the zero value: the call is a scalar function.
	NotAggregate Decomposability = iota
	// Distributive aggregates can be computed from partial aggregates of the
	// same function: SUM, COUNT, MIN, MAX.
	Distributive
	// Algebraic aggregates can be computed from a bounded set of partial
	// aggregates: AVG, STDDEV, VARIANCE.
	Algebraic
	// Holistic aggregates need the whole input: MEDIAN, PERCENTILE,
	// COUNT(DISTINCT).
	Holistic
	// SketchBased aggregates are mergeable but approximate.
	SketchBased
)

func (d Decomposability) String() string {
	switch d {
	case Distributive:
		return "distributive"
	case Algebraic:
		return "algebraic"
	case Holistic:
		return "holistic"
	case SketchBased:
		return "sketch-based"
	}
	return "not an aggregate"
}

// baseDecomposability maps an aggregate name to its classification, ignoring
// the DISTINCT modifier which is applied separately.
var baseDecomposability = map[string]Decomposability{
	"SUM":   Distributive,
	"COUNT": Distributive,
	"MIN":   Distributive,
	"MAX":   Distributive,

	"AVG":         Algebraic,
	"STDDEV":      Algebraic,
	"STDDEV_POP":  Algebraic,
	"STDDEV_SAMP": Algebraic,
	"VARIANCE":    Algebraic,
	"VAR_POP":     Algebraic,
	"VAR_SAMP":    Algebraic,

	"MEDIAN":          Holistic,
	"PERCENTILE_CONT": Holistic,
	"PERCENTILE_DISC": Holistic,

	"APPROX_COUNT_DISTINCT": SketchBased,
	"APPROX_PERCENTILE":     SketchBased,
	"APPROX_QUANTILES":      SketchBased,
}

// IsAggregate reports whether name is one of the spec's aggregate functions.
func IsAggregate(name string) bool {
	_, ok := baseDecomposability[name]
	return ok
}

// Classify returns the decomposability of a call. DISTINCT promotes an
// otherwise distributive count to holistic, which is exactly the row the spec
// calls out: COUNT is Distributive, COUNT(DISTINCT expr) is Holistic.
func Classify(c *Call) Decomposability {
	d, ok := baseDecomposability[c.Name]
	if !ok {
		return NotAggregate
	}
	if c.Distinct && d == Distributive {
		return Holistic
	}
	return d
}

// SurvivesFanOut reports whether an aggregate still returns the right answer
// when the join graph duplicates its input rows.
//
// This is a different question from decomposability and it is the one the
// planner actually asks. An aggregate survives row duplication only when it is
// idempotent under repetition (MIN, MAX) or when it explicitly discards
// duplicates (any DISTINCT modifier, and the distinct-count sketches).
// Everything else, SUM above all, silently inflates.
func SurvivesFanOut(c *Call) bool {
	if !IsAggregate(c.Name) {
		return false
	}
	if c.Distinct {
		return true
	}
	switch c.Name {
	case "MIN", "MAX", "APPROX_COUNT_DISTINCT":
		return true
	}
	return false
}
