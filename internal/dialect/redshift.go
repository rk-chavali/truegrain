package dialect

import (
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// redshift emits Amazon Redshift SQL.
//
// Redshift forked from PostgreSQL 8.0.2 and has diverged, so it is a dialect
// rather than an alias. Aliasing it to postgres would emit one construct
// Redshift cannot parse, in the one place it is hardest to notice.
//
// It embeds postgres rather than copying it, so quoting, placeholders and
// date_trunc stay identical by construction and only the genuine differences
// are written down. A copy would drift the first time one of them changed.
type redshift struct{ postgres }

func (redshift) Name() string { return "redshift" }

// NullSafeEquals is the difference that matters.
//
// Redshift has no IS NOT DISTINCT FROM, so the ANSI spelling the Postgres
// emitter uses is a syntax error there. This matters more than a missing
// convenience: null-safe equality is how two separately aggregated facts are
// joined on a shared dimension, and a null dimension value is a real group.
// Plain equality would drop those rows, which does not fail, it just quietly
// answers a smaller number.
//
// The expanded form is what Redshift's own documentation recommends. Both
// sides are rendered expressions rather than column references, so each is
// evaluated twice; that is the cost of the construct not existing, and it is
// paid only on a multi-fact join.
func (redshift) NullSafeEquals(a, b string) string {
	var sb strings.Builder
	sb.WriteString("(")
	sb.WriteString(a)
	sb.WriteString(" = ")
	sb.WriteString(b)
	sb.WriteString(" OR (")
	sb.WriteString(a)
	sb.WriteString(" IS NULL AND ")
	sb.WriteString(b)
	sb.WriteString(" IS NULL))")
	return sb.String()
}

// DateTrunc is PostgreSQL's, and is inherited, but the grains are worth a
// note: Redshift's date_trunc accepts the same field names, including the
// sub-day ones, so there is no mapping table here either.
func (r redshift) DateTrunc(g plan.Grain, dt osi.Datatype, expr string) (string, error) {
	return r.postgres.DateTrunc(g, dt, expr)
}

func (redshift) Capabilities() Capabilities {
	return Capabilities{
		Dialect: "redshift",
		// Redshift has GRANT on columns and, since RA3, row-level security
		// policies. Neither is configured or observed by this engine, so
		// claiming them would describe the warehouse rather than the
		// deployment.
		ColumnLevelSecurity: false,
		RowLevelSecurity:    false,
		Note: "Redshift supports column grants and row-level security, but this " +
			"engine neither configures nor verifies them, so whether they are in " +
			"force depends on how the connecting user was granted. No executor " +
			"has been verified against a real Redshift cluster: this dialect " +
			"compiles and is golden tested, and running it is unproven.",
	}
}
