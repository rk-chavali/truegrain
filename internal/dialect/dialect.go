// Package dialect turns a plan into SQL for one warehouse.
//
// There is one shared emitter and one small Syntax implementation per
// warehouse. The warehouses differ in quoting, parameter placeholders and date
// truncation, and almost nowhere else, so duplicating the emitter per dialect
// would duplicate the bugs too.
package dialect

import (
	"fmt"
	"sort"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// Capabilities declares what a warehouse can and cannot enforce. Interfaces
// report this verbatim. Overstating a governance guarantee is worse than not
// offering one, so a dialect that enforces nothing must say so here.
type Capabilities struct {
	Dialect string `json:"dialect"`
	// ColumnLevelSecurity reports whether the warehouse itself can deny a
	// column to an identity, independent of this engine's compile-time gate.
	ColumnLevelSecurity bool `json:"column_level_security"`
	// RowLevelSecurity reports whether the warehouse applies row filters at
	// execution time.
	RowLevelSecurity bool `json:"row_level_security"`
	// Note explains the honest limits of the two flags above.
	Note string `json:"note"`
}

// Syntax is the per-warehouse surface the shared emitter needs.
type Syntax interface {
	// Name is the dialect identifier used on the CLI and in the Ossie
	// `dialects` enum where one exists.
	Name() string
	// QuoteIdent quotes one identifier part.
	QuoteIdent(s string) string
	// QuoteSource renders a dataset `source` value, which is a dotted physical
	// path such as project.dataset.table.
	QuoteSource(source string) string
	// Placeholder renders the bind marker for the nth parameter, 1-based.
	Placeholder(n int) string
	// DateTrunc truncates an already-rendered temporal expression to a grain.
	// The dimension's declared datatype is passed because GoogleSQL has a
	// separate truncation function per temporal type; a dialect that does not
	// need it ignores it.
	DateTrunc(g plan.Grain, dt osi.Datatype, expr string) (string, error)
	// NullSafeEquals compares two already-rendered expressions so that two
	// nulls match. Joining aggregated facts on a dimension needs it: a null
	// dimension value is a real group, and plain equality would drop it.
	NullSafeEquals(a, b string) string
	// Capabilities declares what this warehouse enforces.
	Capabilities() Capabilities
}

// Dialect emits SQL for a plan.
type Dialect interface {
	Syntax
	// Emit returns parameterized SQL and its bind values, in order. Values are
	// never interpolated into the text.
	Emit(s *resolve.Schema, p *plan.Plan) (string, []any, error)
	// EmitCombined renders one or more facts aggregated separately and joined
	// on their shared dimensions.
	EmitCombined(c *Combined) (string, []any, error)
}

// dialect pairs a Syntax with the shared emitter.
type dialect struct{ Syntax }

func (d dialect) Emit(s *resolve.Schema, p *plan.Plan) (string, []any, error) {
	return emit(d.Syntax, s, p)
}

func (d dialect) EmitCombined(c *Combined) (string, []any, error) {
	return emitCombined(d.Syntax, c)
}

// registry holds every built dialect by name.
var registry = map[string]Dialect{}

func register(s Syntax) { registry[s.Name()] = dialect{s} }

func init() {
	register(duckDB{})
	register(bigQuery{})
	register(postgres{})
	register(snowflake{})
	register(redshift{})
	// Four more, each embedding the dialect it is closest to. See
	// warehouses.go for what differs and for why none claims an executor.
	register(databricks{})
	register(clickhouse{})
	register(trino{})
	register(athena{})
}

// Get returns the dialect by name.
func Get(name string) (Dialect, error) {
	d, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown dialect %q, available: %v", name, Names())
	}
	return d, nil
}

// Names lists every available dialect, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
