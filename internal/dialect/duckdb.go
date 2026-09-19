package dialect

import (
	"fmt"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// duckDB emits DuckDB SQL. DuckDB is the zero-credential dialect: the quickstart
// and the parity suite both run against it on every pull request, which is what
// keeps the planner honest without anyone holding a cloud account.
type duckDB struct{}

func (duckDB) Name() string { return "duckdb" }

func (duckDB) QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (d duckDB) QuoteSource(source string) string { return quoteDottedSource(d, source) }

func (duckDB) Placeholder(int) string { return "?" }

func (duckDB) DateTrunc(g plan.Grain, _ osi.Datatype, expr string) (string, error) {
	// DuckDB's date_trunc accepts every grain the engine offers and works
	// across DATE, TIMESTAMP and TIMESTAMPTZ, so the declared datatype is not
	// needed to pick a function the way it is on BigQuery.
	return fmt.Sprintf("date_trunc('%s', %s)", g, expr), nil
}

// NullSafeEquals uses the ANSI spelling, which DuckDB supports and which keeps
// the compiled SQL readable.
func (duckDB) NullSafeEquals(a, b string) string {
	return a + " IS NOT DISTINCT FROM " + b
}

func (duckDB) Capabilities() Capabilities {
	return Capabilities{
		Dialect:             "duckdb",
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "DuckDB enforces neither column-level nor row-level security. " +
			"The engine's compile-time gate is the only access control in this " +
			"configuration, and it is advisory unless the engine runs in server " +
			"mode where the caller cannot reach the database directly.",
	}
}

// quoteDottedSource renders a dataset `source`. The spec allows either a dotted
// physical path or an inline query, so the two are distinguished before
// quoting: quoting a SELECT statement as an identifier would produce SQL that
// fails in a confusing place.
func quoteDottedSource(syn Syntax, source string) string {
	s := strings.TrimSpace(source)
	if isInlineQuery(s) {
		return "(" + strings.TrimSuffix(strings.TrimSpace(strings.Trim(s, "()")), ";") + ")"
	}
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = syn.QuoteIdent(p)
	}
	return strings.Join(parts, ".")
}

// isInlineQuery reports whether a source is a query rather than a table path.
func isInlineQuery(s string) bool {
	if strings.HasPrefix(s, "(") {
		return true
	}
	upper := strings.ToUpper(strings.TrimLeft(s, "( \t\n"))
	return strings.HasPrefix(upper, "SELECT ") || strings.HasPrefix(upper, "WITH ")
}
