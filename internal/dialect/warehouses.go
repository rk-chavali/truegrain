package dialect

import (
	"fmt"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Four more warehouses, each differing from an existing dialect in a small
// number of specific ways.
//
// Grouped in one file on purpose. Each is sixty lines of genuine difference
// and four hundred lines of the same quoting and placeholder handling, and
// four near-identical files would drift: somebody fixing a date-truncation
// bug would fix it in one and not the others. Embedding the dialect each
// one is closest to means only the real differences are written down, and a
// change to the shared behaviour reaches all of them.
//
// None of these has a verified executor. They compile and their output is
// golden tested, which is exactly what was true of Postgres before somebody
// ran it and found that a whole dialect had never executed a statement. So
// each Capabilities says so rather than leaving it to be assumed.

// ---------- Databricks ----------

// databricks emits Databricks SQL.
//
// Closest to Postgres: backtick-free, standard date_trunc, ANSI null-safe
// equality. The differences are the identifier quote and the fact that
// Unity Catalog names are three-part.
type databricks struct{ postgres }

func (databricks) Name() string { return "databricks" }

// QuoteIdent uses backticks.
//
// Databricks accepts double quotes only when ANSI_MODE is on, which is a
// session setting this engine does not control and cannot observe.
// Backticks work either way, which is the property worth having: a
// statement that depends on a session flag is a statement that works until
// somebody changes a default.
func (databricks) QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func (d databricks) QuoteSource(source string) string { return quoteDottedSource(d, source) }

// Placeholder is positional.
func (databricks) Placeholder(int) string { return "?" }

func (databricks) Capabilities() Capabilities {
	return Capabilities{
		Dialect: "databricks",
		// Unity Catalog has column masks and row filters, which are real
		// warehouse-enforced security. This engine neither configures nor
		// reads them, so claiming them would describe Databricks rather
		// than this deployment.
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "Unity Catalog supports column masks and row filters, but this " +
			"engine neither configures nor verifies them. No executor has been " +
			"verified against a real workspace: this dialect compiles and is " +
			"golden tested, and running it is unproven.",
	}
}

// ---------- ClickHouse ----------

// clickhouse emits ClickHouse SQL.
//
// The one that differs most, and the differences are not cosmetic.
type clickhouse struct{ postgres }

func (clickhouse) Name() string { return "clickhouse" }

func (clickhouse) QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "\\`") + "`"
}

func (c clickhouse) QuoteSource(source string) string { return quoteDottedSource(c, source) }

func (clickhouse) Placeholder(int) string { return "?" }

// DateTrunc uses the toStartOf family.
//
// ClickHouse has date_trunc as an alias, but the toStartOf functions are
// the native spelling and the one that works on every version anybody is
// still running. An unknown grain is refused rather than passed through:
// a function ClickHouse does not have fails at the warehouse with an error
// that does not mention the grain the caller asked for.
func (clickhouse) DateTrunc(g plan.Grain, _ osi.Datatype, expr string) (string, error) {
	fn, ok := map[plan.Grain]string{
		plan.GrainSecond:  "toStartOfSecond",
		plan.GrainMinute:  "toStartOfMinute",
		plan.GrainHour:    "toStartOfHour",
		plan.GrainDay:     "toStartOfDay",
		plan.GrainWeek:    "toStartOfWeek",
		plan.GrainMonth:   "toStartOfMonth",
		plan.GrainQuarter: "toStartOfQuarter",
		plan.GrainYear:    "toStartOfYear",
	}[g]
	if !ok {
		return "", fmt.Errorf("clickhouse: unsupported grain %q", g)
	}
	return fmt.Sprintf("%s(%s)", fn, expr), nil
}

// NullSafeEquals uses the null-safe comparison operator.
//
// ClickHouse has no IS NOT DISTINCT FROM. It does have `<=>`, which is
// exactly this, and getting it wrong does not fail loudly: plain equality
// drops rows where the joined dimension is null, which understates a
// multi-fact total rather than erroring.
func (clickhouse) NullSafeEquals(a, b string) string {
	return fmt.Sprintf("(%s <=> %s)", a, b)
}

func (clickhouse) Capabilities() Capabilities {
	return Capabilities{
		Dialect:             "clickhouse",
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "ClickHouse supports row policies and column grants, but this " +
			"engine neither configures nor verifies them. No executor has been " +
			"verified against a real server: this dialect compiles and is " +
			"golden tested, and running it is unproven.",
	}
}

// ---------- Athena ----------

// athena emits Amazon Athena SQL, which is Trino's.
//
// Separate from trino because the capability note differs: Athena is a
// managed Trino with Lake Formation attached, and Lake Formation is a real
// column and row security layer that a reader could one day consult.
type athena struct{ trino }

func (athena) Name() string { return "athena" }

func (athena) Capabilities() Capabilities {
	return Capabilities{
		Dialect: "athena",
		// Lake Formation enforces both when configured. This engine does
		// not read it, so an operator cannot learn from here whether it is
		// on.
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "Athena with Lake Formation enforces column and row security, but " +
			"this engine neither configures nor reads it. No executor has been " +
			"verified: this dialect compiles and is golden tested, and running " +
			"it is unproven.",
	}
}

// ---------- Trino ----------

// trino emits Trino SQL.
//
// Closest to Postgres, with two differences that matter.
type trino struct{ postgres }

func (trino) Name() string { return "trino" }

// Placeholder is positional. Trino's JDBC and Go drivers both bind by
// position, and `$1` is a syntax error there rather than a portable
// spelling.
func (trino) Placeholder(int) string { return "?" }

// DateTrunc takes the unit as a string literal, like Postgres, but Trino
// spells the sub-day units the same and rejects anything else. Refusing an
// unknown grain here keeps the error about the grain rather than about a
// function signature.
func (trino) DateTrunc(g plan.Grain, _ osi.Datatype, expr string) (string, error) {
	switch g {
	case plan.GrainSecond, plan.GrainMinute, plan.GrainHour,
		plan.GrainDay, plan.GrainWeek, plan.GrainMonth,
		plan.GrainQuarter, plan.GrainYear:
		return fmt.Sprintf("date_trunc('%s', %s)", g, expr), nil
	default:
		return "", fmt.Errorf("trino: unsupported grain %q", g)
	}
}

// NullSafeEquals uses IS NOT DISTINCT FROM, which Trino does support.
// Inherited from postgres, and named here so the next person does not have
// to check whether it was an oversight.
func (t trino) NullSafeEquals(a, b string) string {
	return t.postgres.NullSafeEquals(a, b)
}

func (trino) Capabilities() Capabilities {
	return Capabilities{
		Dialect:             "trino",
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "Trino's access control is a connector-level concern this engine " +
			"neither configures nor reads. No executor has been verified: this " +
			"dialect compiles and is golden tested, and running it is unproven.",
	}
}
