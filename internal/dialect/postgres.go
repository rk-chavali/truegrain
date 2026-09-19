package dialect

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// postgres emits PostgreSQL.
//
// Worth stating because it is the dialect most likely to be pointed at a
// database the caller can also reach directly: Postgres has real row-level
// security and real column privileges, so unlike DuckDB the engine's gate is
// not necessarily the only control. Whether it is depends on how the deployment
// grants the connecting role, which is why Capabilities says both are available
// rather than claiming they are in force.
type postgres struct{}

func (postgres) Name() string { return "postgres" }

func (postgres) QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (p postgres) QuoteSource(source string) string { return quoteDottedSource(p, source) }

// Placeholder uses the numbered form. Postgres has no positional `?`, and the
// numbering is what lets the same value be bound once and referenced twice.
func (postgres) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

// DateTrunc uses the standard two-argument form.
//
// date_trunc returns a timestamp even when given a date, which is correct for
// grouping: two rows in the same month must land on the same value, and the
// engine compares the truncated expression rather than reading its type.
func (postgres) DateTrunc(g plan.Grain, _ osi.Datatype, expr string) (string, error) {
	// Every grain the engine offers is a Postgres field name, including the
	// sub-day ones, so there is no mapping table to drift.
	return fmt.Sprintf("date_trunc('%s', %s)", g, expr), nil
}

// NullSafeEquals uses the ANSI spelling, which Postgres has supported since 8.4.
func (postgres) NullSafeEquals(a, b string) string {
	return a + " IS NOT DISTINCT FROM " + b
}

func (postgres) Capabilities() Capabilities {
	return Capabilities{
		Dialect: "postgres",
		// Both are available in Postgres, but neither is switched on by the
		// engine. Reporting true here would claim the deployment had configured
		// something this engine has no way to observe.
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "Postgres supports column privileges and row-level security, but " +
			"this engine neither configures nor verifies them. Whether they are " +
			"in force depends on how the connecting role was granted. If the role " +
			"can read everything, the engine's compile-time gate is the only " +
			"control, and it binds only queries that go through the engine.",
	}
}
