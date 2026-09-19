package dialect

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// bigQuery emits GoogleSQL.
type bigQuery struct{}

func (bigQuery) Name() string { return "bigquery" }

// QuoteIdent wraps an identifier in backticks. GoogleSQL has no escape for a
// backtick inside an identifier, so one is stripped rather than smuggled
// through: a name containing a backtick cannot be addressed in BigQuery at all.
func (bigQuery) QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "") + "`"
}

// QuoteSource renders project.dataset.table inside a single pair of backticks,
// which is BigQuery's convention, rather than quoting each part separately.
func (b bigQuery) QuoteSource(source string) string {
	s := strings.TrimSpace(source)
	if isInlineQuery(s) {
		return "(" + strings.TrimSuffix(strings.TrimSpace(strings.Trim(s, "()")), ";") + ")"
	}
	return b.QuoteIdent(s)
}

func (bigQuery) Placeholder(n int) string { return "@p" + strconv.Itoa(n) }

// bqGrain maps an engine grain to a GoogleSQL date part.
//
// WEEK is written as WEEK(MONDAY) so that a week bucket matches DuckDB's
// ISO-8601 week. BigQuery's bare WEEK starts on Sunday, and leaving that
// difference in place would make the same model return different weekly totals
// on two warehouses, which is exactly the failure the parity suite exists to
// catch.
var bqGrain = map[plan.Grain]string{
	plan.GrainSecond:  "SECOND",
	plan.GrainMinute:  "MINUTE",
	plan.GrainHour:    "HOUR",
	plan.GrainDay:     "DAY",
	plan.GrainWeek:    "WEEK(MONDAY)",
	plan.GrainMonth:   "MONTH",
	plan.GrainQuarter: "QUARTER",
	plan.GrainYear:    "YEAR",
}

// subDayGrains are finer than a day and are therefore invalid on a DATE column.
var subDayGrains = map[plan.Grain]bool{
	plan.GrainSecond: true, plan.GrainMinute: true, plan.GrainHour: true,
}

func (bigQuery) DateTrunc(g plan.Grain, dt osi.Datatype, expr string) (string, error) {
	part, ok := bqGrain[g]
	if !ok {
		return "", fmt.Errorf("grain %q has no BigQuery date part", g)
	}
	switch dt {
	case osi.TypeDate:
		if subDayGrains[g] {
			return "", fmt.Errorf("grain %q is finer than a day but the dimension is declared Date", g)
		}
		return fmt.Sprintf("DATE_TRUNC(%s, %s)", expr, part), nil
	case osi.TypeDateTime:
		return fmt.Sprintf("DATETIME_TRUNC(%s, %s)", expr, part), nil
	case osi.TypeDateTimeTz:
		return fmt.Sprintf("TIMESTAMP_TRUNC(%s, %s)", expr, part), nil
	case osi.TypeTime:
		if !subDayGrains[g] {
			return "", fmt.Errorf("grain %q is coarser than a day but the dimension is declared Time", g)
		}
		return fmt.Sprintf("TIME_TRUNC(%s, %s)", expr, part), nil
	}
	// GoogleSQL has four separate truncation functions and picking the wrong
	// one is a runtime error in the warehouse, so refuse here instead, where
	// the message can name the model fix.
	return "", fmt.Errorf(
		"cannot truncate to %q because the dimension declares no datatype; "+
			"BigQuery needs to know whether this is a Date, DateTime, DateTimeTz or Time column", g)
}

// NullSafeEquals is written out rather than using IS NOT DISTINCT FROM, which
// GoogleSQL has only gained recently. The explicit form is correct on every
// version and the verbosity is confined to a join condition.
func (bigQuery) NullSafeEquals(a, b string) string {
	return fmt.Sprintf("(%s = %s OR (%s IS NULL AND %s IS NULL))", a, b, a, b)
}

func (bigQuery) Capabilities() Capabilities {
	return Capabilities{
		Dialect:             "bigquery",
		ColumnLevelSecurity: true,
		RowLevelSecurity:    true,
		Note: "BigQuery enforces column-level security through Data Catalog policy tags " +
			"and row-level security through row access policies. The engine resolves " +
			"column access before emitting SQL; row access policies are applied by " +
			"BigQuery at execution, so results may be narrower than the plan implies.",
	}
}
