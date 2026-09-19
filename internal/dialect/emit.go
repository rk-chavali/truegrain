package dialect

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// emit renders a plan as parameterized SQL.
//
// Output is formatted for a human to read, because every response carries its
// compiled SQL and that text is how a disputed number gets settled.
func emit(syn Syntax, s *resolve.Schema, p *plan.Plan) (string, []any, error) {
	r := &renderer{syn: syn, schema: s, aliases: map[*osi.Dataset]string{}}
	r.assignAliases(p)

	selects, err := r.selectList(p)
	if err != nil {
		return "", nil, err
	}
	from := r.fromClause(p)
	where, err := r.whereClause(p)
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	b.WriteString("SELECT\n  ")
	b.WriteString(strings.Join(selects, ",\n  "))
	b.WriteString("\nFROM ")
	b.WriteString(from)
	if where != "" {
		b.WriteString("\nWHERE ")
		b.WriteString(where)
	}
	if n := len(p.Dimensions); n > 0 {
		// Group by ordinal. Every target warehouse accepts it, and it keeps a
		// long dimension expression from appearing twice in the statement.
		ords := make([]string, n)
		for i := range ords {
			ords[i] = strconv.Itoa(i + 1)
		}
		b.WriteString("\nGROUP BY ")
		b.WriteString(strings.Join(ords, ", "))
	}
	if len(p.OrderBy) > 0 {
		parts := make([]string, len(p.OrderBy))
		for i, o := range p.OrderBy {
			parts[i] = syn.QuoteIdent(o.Alias)
			if o.Desc {
				parts[i] += " DESC"
			}
		}
		b.WriteString("\nORDER BY ")
		b.WriteString(strings.Join(parts, ", "))
	}
	if p.Limit > 0 {
		fmt.Fprintf(&b, "\nLIMIT %d", p.Limit)
	}
	return b.String(), r.params, nil
}

type renderer struct {
	syn     Syntax
	schema  *resolve.Schema
	aliases map[*osi.Dataset]string
	params  []any
}

// assignAliases gives each dataset a stable, collision-free table alias.
func (r *renderer) assignAliases(p *plan.Plan) {
	used := map[string]bool{}
	assign := func(d *osi.Dataset) {
		base := strings.ToLower(d.Name)
		alias := base
		for i := 2; used[alias]; i++ {
			alias = base + strconv.Itoa(i)
		}
		used[alias] = true
		r.aliases[d] = alias
	}
	assign(p.Base)
	for _, j := range p.Joins {
		if _, ok := r.aliases[j.Right]; !ok {
			assign(j.Right)
		}
	}
}

func (r *renderer) selectList(p *plan.Plan) ([]string, error) {
	out := make([]string, 0, len(p.Dimensions)+len(p.Metrics))
	for _, d := range p.Dimensions {
		expr, err := r.dimensionExpr(p, d)
		if err != nil {
			return nil, err
		}
		out = append(out, expr+" AS "+r.syn.QuoteIdent(d.Alias))
	}
	for _, m := range p.Metrics {
		expr, err := r.metricSelectExpr(p, m)
		if err != nil {
			return nil, err
		}
		out = append(out, expr+" AS "+r.syn.QuoteIdent(m.Alias))
	}
	return out, nil
}

// dimensionExpr renders one grouping expression, from the model or from the
// rollup column standing in for it.
func (r *renderer) dimensionExpr(p *plan.Plan, d plan.DimSelect) (string, error) {
	if p.Rollup != nil {
		expr := r.rollupColumn(d.RollupColumn)
		// Already truncated to its stored grain by whatever built the
		// table. Truncating again is only needed when the caller asked for
		// something coarser, which is the case the grain check allowed.
		if d.Grain != "" && !strings.EqualFold(string(d.Grain), d.RollupGrain) {
			out, err := r.syn.DateTrunc(d.Grain, osi.Datatype(d.Field.Datatype), expr)
			if err != nil {
				return "", fmt.Errorf("dimension %s: %w", d.Field.QualifiedName(), err)
			}
			return out, nil
		}
		return expr, nil
	}

	expr, err := r.fieldExpr(d.Field)
	if err != nil {
		return "", err
	}
	if d.Grain != "" {
		expr, err = r.syn.DateTrunc(d.Grain, osi.Datatype(d.Field.Datatype), expr)
		if err != nil {
			return "", fmt.Errorf("dimension %s: %w", d.Field.QualifiedName(), err)
		}
	}
	return expr, nil
}

// metricSelectExpr renders one aggregate, from the model or by combining
// the rollup's stored partials.
//
// The aggregate applied to a rollup column is the plan's, not the metric's.
// A stored COUNT combines with SUM, because counting the rollup's rows
// counts groups rather than the rows underneath them. Rendering the
// metric's own expression here instead would produce that smaller,
// plausible, wrong number, so the metric's AST is deliberately not
// consulted on this branch.
func (r *renderer) metricSelectExpr(p *plan.Plan, m plan.MetricSelect) (string, error) {
	if p.Rollup != nil {
		if m.RollupAggregate == "" {
			return "", fmt.Errorf(
				"metric %s is being read from rollup %s with no re-aggregation; "+
					"the planner should not have routed it", m.Metric.Name, p.Rollup.Name)
		}
		return m.RollupAggregate + "(" + r.rollupColumn(m.RollupColumn) + ")", nil
	}
	return r.metricExpr(m.Metric)
}

// rollupColumn qualifies a column with the rollup's table alias.
func (r *renderer) rollupColumn(column string) string {
	return r.syn.QuoteIdent(rollupAlias) + "." + r.syn.QuoteIdent(column)
}

// rollupAlias is the table alias a rollup gets in the FROM clause.
//
// Fixed rather than derived from the rollup's name, and that is the point:
// a reader looking at a disputed statement sees the word rollup in it and
// knows immediately that the number did not come from the fact tables.
const rollupAlias = "rollup"

func (r *renderer) fromClause(p *plan.Plan) string {
	if p.Rollup != nil {
		// One table and no joins. Everything the joins would have reached
		// is already a column here, which is the whole reason the table
		// exists.
		return r.syn.QuoteSource(p.Rollup.Source) + " AS " + r.syn.QuoteIdent(rollupAlias)
	}

	var b strings.Builder
	b.WriteString(r.syn.QuoteSource(p.Base.Source))
	b.WriteString(" AS ")
	b.WriteString(r.syn.QuoteIdent(r.aliases[p.Base]))

	for _, j := range p.Joins {
		// LEFT JOIN, always. Every join in a plan runs from the fact outwards
		// to a dimension, and an INNER JOIN would silently drop fact rows whose
		// foreign key does not match, producing a total that is quietly too
		// small. A missing dimension row must show as a null, not as absence.
		b.WriteString("\n  LEFT JOIN ")
		b.WriteString(r.syn.QuoteSource(j.Right.Source))
		b.WriteString(" AS ")
		b.WriteString(r.syn.QuoteIdent(r.aliases[j.Right]))
		b.WriteString(" ON ")

		conds := make([]string, len(j.LeftColumns))
		for i := range j.LeftColumns {
			conds[i] = fmt.Sprintf("%s.%s = %s.%s",
				r.syn.QuoteIdent(r.aliases[j.Left]), r.syn.QuoteIdent(j.LeftColumns[i]),
				r.syn.QuoteIdent(r.aliases[j.Right]), r.syn.QuoteIdent(j.RightColumns[i]))
		}
		b.WriteString(strings.Join(conds, " AND "))
	}
	return b.String()
}

func (r *renderer) whereClause(p *plan.Plan) (string, error) {
	if len(p.Filters) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(p.Filters))
	for _, f := range p.Filters {
		var col string
		if p.Rollup != nil {
			col = r.rollupColumn(f.RollupColumn)
		} else {
			var err error
			if col, err = r.fieldExpr(f.Field); err != nil {
				return "", err
			}
		}
		pred, err := r.predicate(col, f)
		if err != nil {
			return "", err
		}
		parts = append(parts, pred)
	}
	return strings.Join(parts, "\n  AND "), nil
}

// predicate renders one structured filter. Values become bind parameters; none
// of them is ever written into the SQL text.
func (r *renderer) predicate(col string, f plan.BoundFilter) (string, error) {
	bind := func(v any) string {
		r.params = append(r.params, v)
		return r.syn.Placeholder(len(r.params))
	}
	switch f.Op {
	case plan.OpIsNull:
		return col + " IS NULL", nil
	case plan.OpIsNotNil:
		return col + " IS NOT NULL", nil
	case plan.OpEq:
		return col + " = " + bind(f.Values[0]), nil
	case plan.OpNe:
		return col + " <> " + bind(f.Values[0]), nil
	case plan.OpGt:
		return col + " > " + bind(f.Values[0]), nil
	case plan.OpGte:
		return col + " >= " + bind(f.Values[0]), nil
	case plan.OpLt:
		return col + " < " + bind(f.Values[0]), nil
	case plan.OpLte:
		return col + " <= " + bind(f.Values[0]), nil
	case plan.OpBetween:
		return col + " BETWEEN " + bind(f.Values[0]) + " AND " + bind(f.Values[1]), nil
	case plan.OpIn, plan.OpNotIn:
		marks := make([]string, len(f.Values))
		for i, v := range f.Values {
			marks[i] = bind(v)
		}
		op := " IN ("
		if f.Op == plan.OpNotIn {
			op = " NOT IN ("
		}
		return col + op + strings.Join(marks, ", ") + ")", nil
	}
	return "", fmt.Errorf("filter on %s: unsupported operator %q", f.Field.QualifiedName(), f.Op)
}

// metricExpr renders a metric. Identifiers inside a metric are `dataset.field`
// references that resolve to declared fields.
func (r *renderer) metricExpr(m *osi.Metric) (string, error) {
	// Prefer an expression the author wrote for this exact dialect. The spec
	// supports per-dialect variants precisely so a model can carry warehouse
	// specific SQL, and second-guessing it would defeat the point.
	if src, ok := m.Expression.ByDialect[r.syn.Name()]; ok && r.syn.Name() != osi.DialectANSI {
		ast, err := osi.ParseExpr(src)
		if err != nil {
			return "", fmt.Errorf("metric %s: %s expression: %w", m.Name, r.syn.Name(), err)
		}
		return r.render(ast, nil)
	}
	if m.Expression.AST == nil {
		return "", fmt.Errorf("metric %s has no parsed expression", m.Name)
	}
	return r.render(m.Expression.AST, nil)
}

// fieldExpr renders a field in the context of its own dataset. Identifiers
// inside a field expression are physical columns of that dataset's source.
func (r *renderer) fieldExpr(f *osi.Field) (string, error) {
	if src, ok := f.Expression.ByDialect[r.syn.Name()]; ok && r.syn.Name() != osi.DialectANSI {
		ast, err := osi.ParseExpr(src)
		if err != nil {
			return "", fmt.Errorf("field %s: %s expression: %w", f.QualifiedName(), r.syn.Name(), err)
		}
		return r.render(ast, f.Dataset)
	}
	if f.Expression.AST == nil {
		return "", fmt.Errorf("field %s has no parsed expression", f.QualifiedName())
	}
	return r.render(f.Expression.AST, f.Dataset)
}

// render walks the AST. ctx is the dataset whose physical columns bare
// identifiers refer to; nil means the expression is a metric, where every
// identifier must be a qualified `dataset.field` reference.
//
// A metric resolves its references into field expressions, and a field
// expression always renders with ctx set, so the two cannot recurse into each
// other indefinitely.
func (r *renderer) render(e osi.Expr, ctx *osi.Dataset) (string, error) {
	switch n := e.(type) {
	case *osi.Ident:
		return r.ident(n, ctx)

	case *osi.Star:
		return "*", nil

	case *osi.Lit:
		return r.literal(n)

	case *osi.Paren:
		inner, err := r.render(n.X, ctx)
		if err != nil {
			return "", err
		}
		return "(" + inner + ")", nil

	case *osi.Unary:
		x, err := r.render(n.X, ctx)
		if err != nil {
			return "", err
		}
		if n.Op == "NOT" {
			return "NOT " + x, nil
		}
		return n.Op + x, nil

	case *osi.Binary:
		l, err := r.render(n.L, ctx)
		if err != nil {
			return "", err
		}
		rr, err := r.render(n.R, ctx)
		if err != nil {
			return "", err
		}
		return l + " " + n.Op + " " + rr, nil

	case *osi.Call:
		return r.call(n, ctx)

	case *osi.Cast:
		x, err := r.render(n.X, ctx)
		if err != nil {
			return "", err
		}
		return "CAST(" + x + " AS " + n.Type + ")", nil

	case *osi.Extract:
		x, err := r.render(n.X, ctx)
		if err != nil {
			return "", err
		}
		return "EXTRACT(" + n.Part + " FROM " + x + ")", nil

	case *osi.IsNull:
		x, err := r.render(n.X, ctx)
		if err != nil {
			return "", err
		}
		if n.Not {
			return x + " IS NOT NULL", nil
		}
		return x + " IS NULL", nil

	case *osi.In:
		x, err := r.render(n.X, ctx)
		if err != nil {
			return "", err
		}
		list, err := r.renderAll(n.List, ctx)
		if err != nil {
			return "", err
		}
		op := " IN ("
		if n.Not {
			op = " NOT IN ("
		}
		return x + op + strings.Join(list, ", ") + ")", nil

	case *osi.Between:
		parts, err := r.renderAll([]osi.Expr{n.X, n.Lo, n.Hi}, ctx)
		if err != nil {
			return "", err
		}
		op := " BETWEEN "
		if n.Not {
			op = " NOT BETWEEN "
		}
		return parts[0] + op + parts[1] + " AND " + parts[2], nil

	case *osi.Case:
		return r.caseExpr(n, ctx)
	}
	return "", fmt.Errorf("cannot render expression node %T", e)
}

func (r *renderer) renderAll(es []osi.Expr, ctx *osi.Dataset) ([]string, error) {
	out := make([]string, len(es))
	for i, e := range es {
		s, err := r.render(e, ctx)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

func (r *renderer) ident(n *osi.Ident, ctx *osi.Dataset) (string, error) {
	if ctx != nil {
		// Inside a field expression every identifier is a physical column of
		// the field's own dataset. A `dataset.column` form is accepted and the
		// qualifier discarded, because the resolver has already proved it names
		// the same dataset.
		col := n.Parts[len(n.Parts)-1]
		return r.syn.QuoteIdent(r.aliases[ctx]) + "." + r.syn.QuoteIdent(col), nil
	}
	if len(n.Parts) != 2 {
		return "", fmt.Errorf("metric reference %q is not `dataset.field`", n.String())
	}
	f, ok := r.schema.Field(n.Parts[0] + "." + n.Parts[1])
	if !ok {
		return "", fmt.Errorf("metric references unknown field %q", n.String())
	}
	if _, joined := r.aliases[f.Dataset]; !joined {
		return "", fmt.Errorf("metric references %q but dataset %s is not in the plan",
			n.String(), f.Dataset.Name)
	}
	return r.fieldExpr(f)
}

func (r *renderer) literal(n *osi.Lit) (string, error) {
	switch n.Kind {
	case osi.LitString:
		// Model text, not caller input, but still escaped rather than trusted.
		return "'" + strings.ReplaceAll(n.Text, "'", "''") + "'", nil
	case osi.LitNumber, osi.LitBool, osi.LitNull, osi.LitInterval:
		return n.Text, nil
	}
	return "", fmt.Errorf("unknown literal kind %d", n.Kind)
}

func (r *renderer) call(n *osi.Call, ctx *osi.Dataset) (string, error) {
	if n.Over != "" {
		return "", fmt.Errorf("window function %s(...) OVER is not supported in v1", n.Name)
	}
	args, err := r.renderAll(n.Args, ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(n.Name)
	b.WriteString("(")
	if n.Distinct {
		b.WriteString("DISTINCT ")
	}
	sep := n.ArgSep
	if sep == "" {
		sep = ", "
	}
	b.WriteString(strings.Join(args, sep))
	b.WriteString(")")
	if len(n.WithinGroup) > 0 {
		wg, err := r.renderAll(n.WithinGroup, ctx)
		if err != nil {
			return "", err
		}
		b.WriteString(" WITHIN GROUP (ORDER BY ")
		b.WriteString(strings.Join(wg, ", "))
		b.WriteString(")")
	}
	return b.String(), nil
}

func (r *renderer) caseExpr(n *osi.Case, ctx *osi.Dataset) (string, error) {
	var b strings.Builder
	b.WriteString("CASE")
	if n.Operand != nil {
		op, err := r.render(n.Operand, ctx)
		if err != nil {
			return "", err
		}
		b.WriteString(" " + op)
	}
	for _, w := range n.Whens {
		cond, err := r.render(w.Cond, ctx)
		if err != nil {
			return "", err
		}
		then, err := r.render(w.Then, ctx)
		if err != nil {
			return "", err
		}
		b.WriteString(" WHEN " + cond + " THEN " + then)
	}
	if n.Else != nil {
		els, err := r.render(n.Else, ctx)
		if err != nil {
			return "", err
		}
		b.WriteString(" ELSE " + els)
	}
	b.WriteString(" END")
	return b.String(), nil
}
