package dialect

import (
	"fmt"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// snowflake emits Snowflake SQL.
//
// Two things about Snowflake shape this file.
//
// Unquoted identifiers fold to upper case rather than lower, which is the
// opposite of Postgres and DuckDB. Everything the engine emits is quoted, so
// the fold never happens and a model naming `order_total` reads that column
// rather than `ORDER_TOTAL`. The cost is that a model written against a table
// created without quotes must spell its columns in upper case, which is what
// the warehouse actually stored.
//
// Snowflake has masking policies and row access policies, which are real
// column and row level security enforced by the warehouse itself. Reading them
// is the equivalent of the BigQuery policy tag resolver and is not built, so
// Capabilities does not claim they are in force.
type snowflake struct{}

func (snowflake) Name() string { return "snowflake" }

func (snowflake) QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (s snowflake) QuoteSource(source string) string { return quoteDottedSource(s, source) }

func (snowflake) Placeholder(int) string { return "?" }

// DateTrunc maps the engine's grains onto Snowflake's date parts.
//
// The names line up with what Snowflake accepts, including the sub-day ones,
// and an unknown grain is refused rather than passed through: a date part
// Snowflake does not recognise fails at the warehouse with an error that does
// not mention the grain the caller asked for.
func (snowflake) DateTrunc(g plan.Grain, _ osi.Datatype, expr string) (string, error) {
	switch g {
	case plan.GrainSecond, plan.GrainMinute, plan.GrainHour,
		plan.GrainDay, plan.GrainWeek, plan.GrainMonth,
		plan.GrainQuarter, plan.GrainYear:
		return fmt.Sprintf("DATE_TRUNC('%s', %s)", strings.ToUpper(string(g)), expr), nil
	default:
		return "", fmt.Errorf("snowflake: unsupported grain %q", g)
	}
}

// NullSafeEquals uses EQUAL_NULL.
//
// Snowflake does not accept the ANSI `IS NOT DISTINCT FROM` spelling, and
// getting this wrong would not fail loudly: plain equality drops rows where the
// joined dimension is null, which silently understates a multi-fact result
// rather than erroring.
func (snowflake) NullSafeEquals(a, b string) string {
	return fmt.Sprintf("EQUAL_NULL(%s, %s)", a, b)
}

func (snowflake) Capabilities() Capabilities {
	return Capabilities{
		Dialect: "snowflake",
		// Snowflake enforces both when masking and row access policies are
		// attached, but this engine neither reads nor verifies them, so
		// claiming true here would overstate what is known.
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "Snowflake supports masking policies and row access policies, but " +
			"this engine does not read them, so it cannot report whether any are " +
			"attached. Column access here comes from the engine's own resolver, " +
			"which binds only queries that go through the engine.",
	}
}
