package dialect

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// Multi-fact emission: aggregate each fact at the shared grain, then join the
// results on it.
//
// This is the fix `docs/03-semantic-model.md` prescribes for the chasm trap.
// Asking for revenue from the order header and revenue from the order lines in
// one query cannot be answered by one flat join, because joining the two facts
// repeats the header once per line. Answering it by aggregating each fact
// separately and joining the two grouped results is correct, and it is what
// makes "revenue and marketing spend by month" expressible at all.
//
// The same machinery answers a query whose metrics live in different
// namespaces, because a part carries its own schema.

// Part is one fact aggregation: a plan plus the schema it was planned against.
// Different parts may come from different namespaces.
type Part struct {
	Schema *resolve.Schema
	Plan   *plan.Plan
	// Namespace labels the part for diagnostics.
	Namespace string
}

// Combined is one or more parts sharing a dimension grain.
//
// Every part groups by the same dimensions in the same order, which is what
// makes joining them on those columns meaningful. Ordering and the row limit
// apply to the joined result, never to a part.
type Combined struct {
	Parts   []Part
	OrderBy []plan.BoundOrder
	Limit   int
}

// Dimensions returns the shared grain, taken from the first part.
func (c *Combined) Dimensions() []plan.DimSelect {
	if len(c.Parts) == 0 {
		return nil
	}
	return c.Parts[0].Plan.Dimensions
}

// Columns returns the output columns: the shared dimensions, then every part's
// metrics in part order.
func (c *Combined) Columns() []string {
	var out []string
	for _, d := range c.Dimensions() {
		out = append(out, d.Alias)
	}
	for _, p := range c.Parts {
		for _, m := range p.Plan.Metrics {
			out = append(out, m.Alias)
		}
	}
	return out
}

// emitCombined renders a multi-part query. A single part is emitted as a plain
// statement, because wrapping one aggregation in a CTE would make the compiled
// SQL harder to read for no benefit, and readable compiled SQL is a feature.
func emitCombined(syn Syntax, c *Combined) (string, []any, error) {
	switch len(c.Parts) {
	case 0:
		return "", nil, fmt.Errorf("a query must have at least one fact part")
	case 1:
		p := c.Parts[0]
		// Ordering and the limit live on the combined query; push them onto the
		// single plan so the emitted statement carries them.
		single := *p.Plan
		single.OrderBy = c.OrderBy
		single.Limit = c.Limit
		return emit(syn, p.Schema, &single)
	}

	dims := c.Dimensions()
	var b strings.Builder
	var params []any

	b.WriteString("WITH ")
	for i, p := range c.Parts {
		if i > 0 {
			b.WriteString(",\n")
		}
		// A part never carries the outer ordering or limit: limiting a part
		// before the join would drop rows the other parts still need.
		inner := *p.Plan
		inner.OrderBy = nil
		inner.Limit = 0

		sql, partParams, err := emit(syn, p.Schema, &inner)
		if err != nil {
			return "", nil, fmt.Errorf("part %d (%s): %w", i+1, p.Namespace, err)
		}
		params = append(params, partParams...)

		fmt.Fprintf(&b, "%s AS (\n%s\n)", syn.QuoteIdent(partAlias(i)), indent(sql))
	}

	b.WriteString("\nSELECT\n")
	var selects []string
	for _, d := range dims {
		// The dimension value can be null on any one side when that fact has no
		// rows for it, so it is coalesced across every part rather than read
		// from the first.
		var refs []string
		for i := range c.Parts {
			refs = append(refs, syn.QuoteIdent(partAlias(i))+"."+syn.QuoteIdent(d.Alias))
		}
		selects = append(selects,
			fmt.Sprintf("COALESCE(%s) AS %s", strings.Join(refs, ", "), syn.QuoteIdent(d.Alias)))
	}
	for i, p := range c.Parts {
		for _, m := range p.Plan.Metrics {
			selects = append(selects, fmt.Sprintf("%s.%s AS %s",
				syn.QuoteIdent(partAlias(i)), syn.QuoteIdent(m.Alias), syn.QuoteIdent(m.Alias)))
		}
	}
	b.WriteString("  " + strings.Join(selects, ",\n  "))

	b.WriteString("\nFROM ")
	b.WriteString(syn.QuoteIdent(partAlias(0)))
	for i := 1; i < len(c.Parts); i++ {
		// FULL OUTER JOIN, because a month with orders but no campaigns is a
		// real row and dropping it would understate the side that does have
		// data. An inner join here would silently narrow the answer.
		b.WriteString("\n  FULL OUTER JOIN ")
		b.WriteString(syn.QuoteIdent(partAlias(i)))
		if len(dims) == 0 {
			// Each part is a single row, so there is nothing to match on.
			b.WriteString(" ON TRUE")
			continue
		}
		var conds []string
		for _, d := range dims {
			// Null-safe, because a null dimension value is a real group and
			// plain equality would drop it from the joined result.
			conds = append(conds, syn.NullSafeEquals(
				syn.QuoteIdent(partAlias(0))+"."+syn.QuoteIdent(d.Alias),
				syn.QuoteIdent(partAlias(i))+"."+syn.QuoteIdent(d.Alias)))
		}
		b.WriteString(" ON " + strings.Join(conds, "\n    AND "))
	}

	if len(c.OrderBy) > 0 {
		parts := make([]string, len(c.OrderBy))
		for i, o := range c.OrderBy {
			parts[i] = syn.QuoteIdent(o.Alias)
			if o.Desc {
				parts[i] += " DESC"
			}
		}
		b.WriteString("\nORDER BY " + strings.Join(parts, ", "))
	}
	if c.Limit > 0 {
		fmt.Fprintf(&b, "\nLIMIT %d", c.Limit)
	}
	return b.String(), params, nil
}

// partAlias names the CTE for one fact.
func partAlias(i int) string { return "fact_" + strconv.Itoa(i+1) }

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}
