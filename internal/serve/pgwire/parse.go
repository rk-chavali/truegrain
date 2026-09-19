package pgwire

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/plan"
)

// Turning a BI tool's SELECT into a semantic request.
//
// The grammar, in full:
//
//	SELECT  <item> [, <item>]...
//	FROM    <namespace>
//	[WHERE  <condition> [AND <condition>]...]
//	[GROUP BY <expr> [, <expr>]...]
//	[ORDER BY <expr> [ASC|DESC] [, ...]]
//	[LIMIT  <n>]
//
// An <item> is a metric name, a dimension name, or an aggregate wrapping one
// of them, each optionally aliased with AS. A <condition> compares a dimension
// to a literal. Anything else is refused by name.
//
// Two things this deliberately does not do.
//
// It does not accept a join. Joins are the model's to decide: the planner
// derives them from declared foreign keys and refuses the ones that would
// inflate a sum, and a caller writing their own join would be choosing the
// grain by hand, which is the entire failure this product exists to prevent.
//
// It does not accept a subquery, a CTE, a set operation or a window function.
// Each would have to be pushed to the warehouse as text, and text pushed to
// the warehouse is the thing that must never exist here.
//
// GROUP BY is parsed and checked rather than used. The semantic layer already
// knows the grain of every metric, so the grouping is implied by which
// dimensions were selected; a GROUP BY that disagrees with the select list is
// a caller misunderstanding, and saying so is better than quietly using one.

// aggregates a BI tool wraps a measure in. The semantic model already knows
// how a metric aggregates, so these are accepted and unwrapped rather than
// applied: SUM(order_revenue) and order_revenue are the same request, which
// is what lets a tool that insists on writing an aggregate work unchanged.
var aggregates = map[string]bool{
	"sum": true, "count": true, "avg": true, "min": true, "max": true,
}

// query is a parsed statement, before names are checked against a model.
type query struct {
	// Fields are the select list in the order the caller wrote them, which is
	// the order the result columns must come back in.
	fields []field
	from   string
	// groupBy holds what the caller grouped by, for the consistency check.
	groupBy []string
	filters []plan.Filter
	orderBy []plan.Order
	limit   int
}

type field struct {
	// name is the metric or dimension as written, with any aggregate stripped.
	name string
	// alias is the output column name: the AS clause, or the name itself.
	alias string
	// star records SELECT *, which needs the model to expand.
	star bool
}

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }
func (p *parser) done() bool  { return p.toks[p.i].kind == tokEOF }
func (p *parser) at(s string) bool {
	return p.toks[p.i].is(s)
}

func (p *parser) accept(s string) bool {
	if p.at(s) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expect(s string) error {
	if p.accept(s) {
		return nil
	}
	return fmt.Errorf("expected %s, found %s", s, describe(p.peek()))
}

func describe(t token) string {
	if t.kind == tokEOF {
		return "the end of the statement"
	}
	return strconv.Quote(t.text)
}

// parseSelect parses one statement into a query.
//
// Errors here are refusals a caller can act on, so each one names what was
// found and, where there is one, what to write instead.
func parseSelect(sql string) (*query, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}

	if !p.accept("select") {
		return nil, fmt.Errorf(
			"only SELECT is supported, found %s. This engine compiles a semantic "+
				"model; it does not run statements against the warehouse",
			describe(p.peek()))
	}

	q := &query{}
	if err := p.parseSelectList(q); err != nil {
		return nil, err
	}

	if err := p.expect("from"); err != nil {
		return nil, fmt.Errorf(
			"%w. A query reads from one namespace, for example FROM retail", err)
	}
	name, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	q.from = name
	// An alias on the namespace is accepted and ignored: there is one table in
	// the statement, so nothing can be ambiguous.
	p.accept("as")
	if p.peek().kind == tokWord && !isClauseKeyword(p.peek().text) {
		p.next()
	}
	if p.accept(",") || p.at("join") || p.at("inner") || p.at("left") ||
		p.at("right") || p.at("full") || p.at("cross") {
		return nil, fmt.Errorf(
			"a join cannot be written here. Joins come from the model's declared " +
				"foreign keys, so that the planner can refuse the ones that would " +
				"inflate a sum. Select the dimensions you want and the joins follow")
	}

	if p.accept("where") {
		if q.filters, err = p.parseWhere(); err != nil {
			return nil, err
		}
	}
	if p.accept("group") {
		if err := p.expect("by"); err != nil {
			return nil, err
		}
		if q.groupBy, err = p.parseGroupBy(); err != nil {
			return nil, err
		}
	}
	if p.accept("having") {
		return nil, fmt.Errorf(
			"HAVING is not supported. Filter on a dimension with WHERE; filtering " +
				"on an aggregate would require the engine to compute it twice")
	}
	if p.accept("order") {
		if err := p.expect("by"); err != nil {
			return nil, err
		}
		if q.orderBy, err = p.parseOrderBy(); err != nil {
			return nil, err
		}
	}
	if p.accept("limit") {
		t := p.next()
		if t.kind != tokNumber {
			return nil, fmt.Errorf("LIMIT takes a number, found %s", describe(t))
		}
		n, err := strconv.Atoi(t.text)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("LIMIT takes a whole number, found %q", t.text)
		}
		q.limit = n
	}
	// Some clients append OFFSET 0. Accept that and nothing else, because a
	// real offset would need a stable sort the engine does not promise.
	if p.accept("offset") {
		t := p.next()
		if t.kind != tokNumber || t.text != "0" {
			return nil, fmt.Errorf(
				"OFFSET is not supported. Narrow the question with WHERE, or page " +
					"through a job over the REST API, which returns a stable cursor")
		}
	}
	p.accept(";")

	if !p.done() {
		return nil, fmt.Errorf("unexpected %s after the end of the query", describe(p.peek()))
	}
	return q, nil
}

func isClauseKeyword(s string) bool {
	switch strings.ToLower(s) {
	case "where", "group", "order", "limit", "offset", "having",
		"join", "inner", "left", "right", "full", "cross", "on", "union":
		return true
	}
	return false
}

func (p *parser) parseSelectList(q *query) error {
	if p.accept("distinct") {
		// Every semantic result is already grouped by its dimensions, so rows
		// are distinct by construction. Accepting it silently would be fine;
		// saying so is better, because a caller who wrote it believes it is
		// doing something.
		return fmt.Errorf(
			"DISTINCT is not needed and is not accepted: a semantic result is " +
				"grouped by the dimensions selected, so its rows are already distinct")
	}
	for {
		f, err := p.parseField()
		if err != nil {
			return err
		}
		q.fields = append(q.fields, f)
		if !p.accept(",") {
			return nil
		}
	}
}

func (p *parser) parseField() (field, error) {
	if p.accept("*") {
		return field{star: true}, nil
	}

	// An aggregate wrapping one name: SUM(order_revenue).
	if p.peek().kind == tokWord && aggregates[strings.ToLower(p.peek().text)] &&
		p.toks[p.i+1].is("(") {
		fn := p.next().text
		p.next() // (
		// COUNT(*) has no name to unwrap and no metric to map it to.
		if p.accept("*") {
			return field{}, fmt.Errorf(
				"COUNT(*) has no meaning here: a count is a metric the model " +
					"defines, because what counts as a row is a modelling decision. " +
					"Select a count metric by name")
		}
		p.accept("distinct")
		name, err := p.parseIdentifier()
		if err != nil {
			return field{}, err
		}
		if err := p.expect(")"); err != nil {
			return field{}, fmt.Errorf("%s( is not closed: %w", fn, err)
		}
		return field{name: name, alias: p.parseAlias(name)}, nil
	}

	name, err := p.parseIdentifier()
	if err != nil {
		return field{}, err
	}
	return field{name: name, alias: p.parseAlias(name)}, nil
}

// parseAlias reads an optional AS clause, defaulting to the name itself.
func (p *parser) parseAlias(name string) string {
	if p.accept("as") {
		t := p.next()
		if t.kind == tokWord || t.kind == tokQuoted {
			return t.text
		}
		return name
	}
	// A bare alias, with AS omitted, but only when it cannot be a clause
	// keyword: `SELECT region FROM x` must not read FROM as region's alias.
	if t := p.peek(); t.kind == tokQuoted ||
		(t.kind == tokWord && !isClauseKeyword(t.text) && !t.is("from") && !t.is("as")) {
		p.i++
		return t.text
	}
	return name
}

// parseIdentifier reads a name, which may be dotted and may be quoted.
//
// Both spellings arrive in practice. A tool that knows the column is called
// customers.region quotes the whole thing; one that treats it as a qualified
// column writes customers.region unquoted. They mean the same dimension here,
// because there is one table in the statement.
func (p *parser) parseIdentifier() (string, error) {
	t := p.next()
	if t.kind != tokWord && t.kind != tokQuoted {
		return "", fmt.Errorf("expected a name, found %s", describe(t))
	}
	name := t.text
	for p.at(".") {
		p.next()
		part := p.next()
		if part.kind != tokWord && part.kind != tokQuoted {
			return "", fmt.Errorf("expected a name after '.', found %s", describe(part))
		}
		name += "." + part.text
	}
	return name, nil
}

func (p *parser) parseGroupBy() ([]string, error) {
	var out []string
	for {
		t := p.peek()
		if t.kind == tokNumber {
			// GROUP BY 1, 2: positional, and consistent by construction.
			p.next()
			out = append(out, t.text)
		} else {
			name, err := p.parseIdentifier()
			if err != nil {
				return nil, err
			}
			out = append(out, name)
		}
		if !p.accept(",") {
			return out, nil
		}
	}
}

func (p *parser) parseOrderBy() ([]plan.Order, error) {
	var out []plan.Order
	for {
		var o plan.Order
		t := p.peek()
		if t.kind == tokNumber {
			p.next()
			o.Field = t.text
		} else {
			// An aggregate is allowed here for the same reason it is allowed
			// in the select list: ORDER BY SUM(x) and ORDER BY x name the same
			// output column once the model knows how x aggregates.
			if t.kind == tokWord && aggregates[strings.ToLower(t.text)] && p.toks[p.i+1].is("(") {
				p.next()
				p.next()
				name, err := p.parseIdentifier()
				if err != nil {
					return nil, err
				}
				if err := p.expect(")"); err != nil {
					return nil, err
				}
				o.Field = name
			} else {
				name, err := p.parseIdentifier()
				if err != nil {
					return nil, err
				}
				o.Field = name
			}
		}
		if p.accept("desc") {
			o.Desc = true
		} else {
			p.accept("asc")
		}
		// NULLS FIRST/LAST is accepted and dropped: the engine does not
		// promise a null ordering, and failing the whole query over it would
		// break tools that always emit it.
		if p.accept("nulls") {
			if !p.accept("first") {
				p.accept("last")
			}
		}
		out = append(out, o)
		if !p.accept(",") {
			return out, nil
		}
	}
}

func (p *parser) parseWhere() ([]plan.Filter, error) {
	var out []plan.Filter
	for {
		f, err := p.parseCondition()
		if err != nil {
			return nil, err
		}
		out = append(out, f)

		if p.accept("and") {
			continue
		}
		if p.at("or") {
			return nil, fmt.Errorf(
				"OR is not supported in a filter. Every condition is combined with " +
					"AND, which is what the semantic filter model expresses; an OR " +
					"across dimensions changes which rows a metric covers in a way " +
					"the grain check cannot see")
		}
		return out, nil
	}
}

func (p *parser) parseCondition() (plan.Filter, error) {
	if p.accept("(") {
		f, err := p.parseCondition()
		if err != nil {
			return f, err
		}
		return f, p.expect(")")
	}
	if p.at("not") {
		return plan.Filter{}, fmt.Errorf(
			"NOT is not supported. Write the negative operator directly, for " +
				"example <> or NOT IN")
	}

	name, err := p.parseIdentifier()
	if err != nil {
		return plan.Filter{}, err
	}
	f := plan.Filter{Dimension: name}

	switch {
	case p.accept("is"):
		if p.accept("not") {
			if err := p.expect("null"); err != nil {
				return f, err
			}
			f.Op = plan.OpIsNotNil
			return f, nil
		}
		if err := p.expect("null"); err != nil {
			return f, err
		}
		f.Op = plan.OpIsNull
		return f, nil

	case p.accept("in"), p.at("not") && p.toks[p.i+1].is("in"):
		negated := false
		if p.accept("not") {
			p.accept("in")
			negated = true
		}
		if err := p.expect("("); err != nil {
			return f, err
		}
		values, err := p.parseValueList()
		if err != nil {
			return f, err
		}
		if err := p.expect(")"); err != nil {
			return f, err
		}
		f.Op = plan.OpIn
		if negated {
			f.Op = plan.OpNotIn
		}
		f.Values = values
		return f, nil

	case p.accept("between"):
		lo, err := p.parseValue()
		if err != nil {
			return f, err
		}
		if err := p.expect("and"); err != nil {
			return f, err
		}
		hi, err := p.parseValue()
		if err != nil {
			return f, err
		}
		f.Op = plan.OpBetween
		f.Values = []any{lo, hi}
		return f, nil

	case p.at("like"), p.at("ilike"):
		return f, fmt.Errorf(
			"LIKE is not supported. The semantic filter model has no pattern " +
				"match, because a pattern is a predicate the engine cannot check " +
				"against a dimension's declared type")
	}

	op := p.next()
	switch op.text {
	case "=":
		f.Op = plan.OpEq
	case "<>":
		f.Op = plan.OpNe
	case ">":
		f.Op = plan.OpGt
	case ">=":
		f.Op = plan.OpGte
	case "<":
		f.Op = plan.OpLt
	case "<=":
		f.Op = plan.OpLte
	default:
		return f, fmt.Errorf("unsupported comparison %s in a filter", describe(op))
	}

	v, err := p.parseValue()
	if err != nil {
		return f, err
	}
	f.Values = []any{v}
	return f, nil
}

func (p *parser) parseValueList() ([]any, error) {
	var out []any
	for {
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if !p.accept(",") {
			return out, nil
		}
	}
}

// parseValue reads one literal.
//
// The value becomes an element of plan.Filter.Values and is carried to the
// warehouse as a bound parameter. It is never rendered into SQL text here,
// which is what makes the quoting rules in the lexer a correctness concern
// rather than a security one.
func (p *parser) parseValue() (any, error) {
	t := p.next()
	switch {
	case t.kind == tokString:
		return t.text, nil
	case t.kind == tokNumber:
		if n, err := strconv.ParseInt(t.text, 10, 64); err == nil {
			return n, nil
		}
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", t.text)
		}
		return f, nil
	case t.is("true"):
		return true, nil
	case t.is("false"):
		return false, nil
	case t.is("null"):
		return nil, fmt.Errorf(
			"comparing to NULL never matches. Write IS NULL or IS NOT NULL")
	case t.kind == tokWord:
		// DATE '2026-01-01' and similar typed literals: the type prefix is
		// dropped, because the dimension's declared type already decides how
		// the value is bound.
		if s := p.peek(); s.kind == tokString {
			p.next()
			return s.text, nil
		}
		return nil, fmt.Errorf(
			"%q is not a literal value. A filter compares a dimension to a "+
				"constant; comparing two columns is not expressible here", t.text)
	}
	return nil, fmt.Errorf("expected a value, found %s", describe(t))
}
