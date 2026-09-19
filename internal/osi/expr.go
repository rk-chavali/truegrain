package osi

import (
	"fmt"
	"strings"
)

// This file parses the Ossie expression language (core-spec/expression_language.md)
// into an AST.
//
// The engine needs this because the spec expresses metrics as SQL strings. The
// set of datasets a metric touches, and the aggregate function that decides
// whether the metric survives a fan-out join, are both facts that only exist
// inside the expression text. A YAML parser alone cannot plan.
//
// Scope is the spec's scalar and aggregate surface. Window functions and
// subqueries parse into an AST but the planner refuses them in v1, so an
// unsupported construct produces a named error rather than wrong SQL.

// Expr is a node in a parsed expression.
type Expr interface {
	exprNode()
}

// Ident is a possibly qualified reference: `amount`, `orders.amount`.
//
// Parts holds the source text of each part. Norm holds the spec's normalized
// form, which is what name resolution compares: regular identifiers uppercase,
// quoted identifiers exact. The spec is explicit that `"id"` does not match a
// column named `id` uppercased, so the two forms cannot share one field.
type Ident struct {
	Parts []string
	Norm  []string
}

// NormalizeIdent applies the spec's normalization: a regular identifier
// uppercases, a quoted one keeps its exact text.
func NormalizeIdent(text string, quoted bool) string {
	if quoted {
		return text
	}
	return strings.ToUpper(text)
}

// String renders the identifier in source form.
func (i *Ident) String() string { return strings.Join(i.Parts, ".") }

// NormKey is the comparable form of the whole reference.
func (i *Ident) NormKey() string { return strings.Join(i.Norm, ".") }

// addPart appends one part in both source and normalized form.
func (i *Ident) addPart(text string, quoted bool) {
	i.Parts = append(i.Parts, text)
	i.Norm = append(i.Norm, NormalizeIdent(text, quoted))
}

// Star is the `*` in COUNT(*).
type Star struct{}

// LitKind distinguishes literal forms so a renderer can requote correctly.
type LitKind int

const (
	LitNumber LitKind = iota
	LitString
	LitBool
	LitNull
	LitInterval
)

// Lit is a literal value. Text is the source form without surrounding quotes
// for strings.
type Lit struct {
	Kind LitKind
	Text string
}

// Call is a function invocation. Distinct records COUNT(DISTINCT x).
type Call struct {
	Name     string
	Args     []Expr
	Distinct bool
	// WithinGroup holds the ORDER BY expressions of an ordered-set aggregate,
	// `PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY amount)`. They are parsed
	// rather than captured as text because the columns they reference decide
	// which datasets the metric needs.
	WithinGroup []Expr
	// Over is the raw text of an OVER clause, retained so the planner can
	// refuse window functions by name instead of silently dropping them.
	Over string
	// ArgSep overrides the separator used when rendering arguments. It is
	// empty for ordinary comma-separated calls and " IN " for the spec's
	// POSITION(substr IN str) form.
	ArgSep string
}

// Unary is a prefix operator: NOT, -, +.
type Unary struct {
	Op string
	X  Expr
}

// Binary is an infix operator.
type Binary struct {
	Op   string
	L, R Expr
}

// When is one arm of a CASE expression.
type When struct{ Cond, Then Expr }

// Case is CASE [operand] WHEN ... THEN ... [ELSE ...] END.
type Case struct {
	Operand Expr
	Whens   []When
	Else    Expr
}

// Cast is CAST(x AS type).
type Cast struct {
	X    Expr
	Type string
}

// Extract is EXTRACT(part FROM x).
type Extract struct {
	Part string
	X    Expr
}

// In is `x [NOT] IN (list)`.
type In struct {
	X    Expr
	List []Expr
	Not  bool
}

// Between is `x [NOT] BETWEEN lo AND hi`.
type Between struct {
	X, Lo, Hi Expr
	Not       bool
}

// IsNull is `x IS [NOT] NULL`.
type IsNull struct {
	X   Expr
	Not bool
}

// Paren preserves author grouping so re-rendered SQL keeps its semantics.
type Paren struct{ X Expr }

func (*Ident) exprNode()   {}
func (*Star) exprNode()    {}
func (*Lit) exprNode()     {}
func (*Call) exprNode()    {}
func (*Unary) exprNode()   {}
func (*Binary) exprNode()  {}
func (*Case) exprNode()    {}
func (*Cast) exprNode()    {}
func (*Extract) exprNode() {}
func (*In) exprNode()      {}
func (*Between) exprNode() {}
func (*IsNull) exprNode()  {}
func (*Paren) exprNode()   {}

// ---------- lexer ----------

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tQIdent
	tNum
	tStr
	tOp
)

type token struct {
	kind tokKind
	text string
	col  int
}

// upper is the uppercased text, used for keyword comparison.
func (t token) upper() string { return strings.ToUpper(t.text) }

func (t token) isKeyword(kw string) bool {
	return t.kind == tIdent && t.upper() == kw
}

func (t token) isOp(op string) bool { return t.kind == tOp && t.text == op }

// multi-character operators, longest first so `<=` never lexes as `<` then `=`.
var multiOps = []string{"<>", "!=", "<=", ">=", "||"}

type lexer struct {
	src string
	pos int
}

func (l *lexer) lex() ([]token, error) {
	var out []token
	for {
		l.skipSpace()
		if l.pos >= len(l.src) {
			out = append(out, token{kind: tEOF, col: l.pos})
			return out, nil
		}
		start := l.pos
		c := l.src[l.pos]

		switch {
		case isIdentStart(c):
			for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
				l.pos++
			}
			out = append(out, token{tIdent, l.src[start:l.pos], start})

		case c == '"':
			s, err := l.quoted('"')
			if err != nil {
				return nil, err
			}
			out = append(out, token{tQIdent, s, start})

		case c == '\'':
			s, err := l.quoted('\'')
			if err != nil {
				return nil, err
			}
			out = append(out, token{tStr, s, start})

		case c >= '0' && c <= '9', c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]):
			l.number()
			out = append(out, token{tNum, l.src[start:l.pos], start})

		default:
			if op, ok := l.multiOp(); ok {
				out = append(out, token{tOp, op, start})
				continue
			}
			if !strings.ContainsRune("+-*/%=<>(),.", rune(c)) {
				return nil, fmt.Errorf("unexpected character %q at offset %d", c, start)
			}
			l.pos++
			out = append(out, token{tOp, string(c), start})
		}
	}
}

func (l *lexer) skipSpace() {
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case ' ', '\t', '\n', '\r':
			l.pos++
		case '-':
			// line comment
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
				for l.pos < len(l.src) && l.src[l.pos] != '\n' {
					l.pos++
				}
				continue
			}
			return
		default:
			return
		}
	}
}

func (l *lexer) multiOp() (string, bool) {
	for _, op := range multiOps {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.pos += len(op)
			return op, true
		}
	}
	return "", false
}

// quoted reads a q-delimited run, treating a doubled delimiter as an escape,
// which is the SQL standard for both ” and "".
func (l *lexer) quoted(q byte) (string, error) {
	l.pos++ // opening
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == q {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == q {
				b.WriteByte(q)
				l.pos += 2
				continue
			}
			l.pos++
			return b.String(), nil
		}
		b.WriteByte(c)
		l.pos++
	}
	return "", fmt.Errorf("unterminated %c-quoted literal", q)
}

func (l *lexer) number() {
	for l.pos < len(l.src) && (isDigit(l.src[l.pos]) || l.src[l.pos] == '.') {
		l.pos++
	}
	// exponent
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		save := l.pos
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		if l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
				l.pos++
			}
		} else {
			l.pos = save
		}
	}
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) || c == '$' }

// ---------- parser ----------

// ParseExpr parses one Ossie expression. The error names the offset so a model
// author can find the character, not just the field.
func ParseExpr(src string) (Expr, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("empty expression")
	}
	toks, err := (&lexer{src: src}).lex()
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	e, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tEOF {
		return nil, p.errorf("unexpected %q after end of expression", p.cur().text)
	}
	return e, nil
}

type parser struct {
	toks []token
	src  string
	i    int
}

func (p *parser) cur() token  { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("%s (at offset %d in %q)", fmt.Sprintf(format, args...), p.cur().col, p.src)
}

func (p *parser) expectOp(op string) error {
	if !p.cur().isOp(op) {
		return p.errorf("expected %q, found %q", op, p.cur().text)
	}
	p.i++
	return nil
}

func (p *parser) expectKeyword(kw string) error {
	if !p.cur().isKeyword(kw) {
		return p.errorf("expected %s, found %q", kw, p.cur().text)
	}
	p.i++
	return nil
}

// binding powers. Higher binds tighter.
const (
	bpOr      = 1
	bpAnd     = 2
	bpNot     = 3
	bpCompare = 4
	bpConcat  = 5
	bpAdd     = 6
	bpMul     = 7
	bpUnary   = 8
)

var compareOps = map[string]bool{"=": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}

// infixBP returns the binding power of the operator at the cursor, or 0 when
// the cursor is not on an infix operator.
func (p *parser) infixBP() int {
	t := p.cur()
	switch {
	case t.isKeyword("OR"):
		return bpOr
	case t.isKeyword("AND"):
		return bpAnd
	case t.isKeyword("IS"), t.isKeyword("IN"), t.isKeyword("BETWEEN"), t.isKeyword("LIKE"), t.isKeyword("ILIKE"):
		return bpCompare
	case t.isKeyword("NOT"):
		// only infix as part of NOT IN / NOT BETWEEN / NOT LIKE
		if p.i+1 < len(p.toks) {
			n := p.toks[p.i+1]
			if n.isKeyword("IN") || n.isKeyword("BETWEEN") || n.isKeyword("LIKE") || n.isKeyword("ILIKE") {
				return bpCompare
			}
		}
		return 0
	case t.kind == tOp && compareOps[t.text]:
		return bpCompare
	case t.isOp("||"):
		return bpConcat
	case t.isOp("+"), t.isOp("-"):
		return bpAdd
	case t.isOp("*"), t.isOp("/"), t.isOp("%"):
		return bpMul
	}
	return 0
}

func (p *parser) expr(minBP int) (Expr, error) {
	left, err := p.prefix()
	if err != nil {
		return nil, err
	}
	for {
		bp := p.infixBP()
		if bp == 0 || bp < minBP {
			return left, nil
		}
		left, err = p.infix(left, bp)
		if err != nil {
			return nil, err
		}
	}
}

func (p *parser) infix(left Expr, bp int) (Expr, error) {
	not := false
	if p.cur().isKeyword("NOT") {
		not = true
		p.i++
	}
	t := p.next()

	switch {
	case t.isKeyword("IS"):
		neg := false
		if p.cur().isKeyword("NOT") {
			neg = true
			p.i++
		}
		if err := p.expectKeyword("NULL"); err != nil {
			return nil, err
		}
		return &IsNull{X: left, Not: neg}, nil

	case t.isKeyword("IN"):
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		list, err := p.exprList()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return &In{X: left, List: list, Not: not}, nil

	case t.isKeyword("BETWEEN"):
		// AND inside BETWEEN binds tighter than a boolean AND, so parse each
		// bound above bpAnd.
		lo, err := p.expr(bpAnd + 1)
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		hi, err := p.expr(bpAnd + 1)
		if err != nil {
			return nil, err
		}
		return &Between{X: left, Lo: lo, Hi: hi, Not: not}, nil

	case t.isKeyword("LIKE"), t.isKeyword("ILIKE"):
		right, err := p.expr(bp + 1)
		if err != nil {
			return nil, err
		}
		op := t.upper()
		if not {
			op = "NOT " + op
		}
		return &Binary{Op: op, L: left, R: right}, nil

	case t.isKeyword("AND"), t.isKeyword("OR"):
		right, err := p.expr(bp + 1)
		if err != nil {
			return nil, err
		}
		return &Binary{Op: t.upper(), L: left, R: right}, nil

	default: // arithmetic, comparison, concat
		right, err := p.expr(bp + 1)
		if err != nil {
			return nil, err
		}
		return &Binary{Op: t.text, L: left, R: right}, nil
	}
}

func (p *parser) prefix() (Expr, error) {
	t := p.cur()
	switch {
	case t.isKeyword("NOT"):
		p.i++
		x, err := p.expr(bpNot)
		if err != nil {
			return nil, err
		}
		return &Unary{Op: "NOT", X: x}, nil

	case t.isOp("-"), t.isOp("+"):
		p.i++
		x, err := p.expr(bpUnary)
		if err != nil {
			return nil, err
		}
		return &Unary{Op: t.text, X: x}, nil

	case t.isOp("("):
		p.i++
		x, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return &Paren{X: x}, nil

	case t.isOp("*"):
		p.i++
		return &Star{}, nil

	case t.kind == tNum:
		p.i++
		return &Lit{Kind: LitNumber, Text: t.text}, nil

	case t.kind == tStr:
		p.i++
		return &Lit{Kind: LitString, Text: t.text}, nil

	case t.kind == tQIdent:
		p.i++
		id := &Ident{}
		id.addPart(t.text, true)
		return p.qualify(id)

	case t.kind == tIdent:
		return p.identOrKeyword()
	}
	return nil, p.errorf("unexpected %q", t.text)
}

// identOrKeyword handles the constructs that start with a bare word: the
// keyword forms first, then a plain identifier or function call.
func (p *parser) identOrKeyword() (Expr, error) {
	switch p.cur().upper() {
	case "CASE":
		return p.caseExpr()
	case "CAST":
		return p.castExpr()
	case "EXTRACT":
		return p.extractExpr()
	case "INTERVAL":
		return p.intervalExpr()
	case "TRUE", "FALSE":
		t := p.next()
		return &Lit{Kind: LitBool, Text: t.upper()}, nil
	case "NULL":
		p.i++
		return &Lit{Kind: LitNull, Text: "NULL"}, nil
	}

	if p.cur().upper() == "POSITION" && p.i+1 < len(p.toks) && p.toks[p.i+1].isOp("(") {
		return p.positionExpr()
	}

	name := p.next().text
	if p.cur().isOp("(") {
		return p.call(name)
	}
	id := &Ident{}
	id.addPart(name, false)
	return p.qualify(id)
}

// qualify consumes any `.part` suffixes, producing a qualified identifier.
func (p *parser) qualify(id *Ident) (Expr, error) {
	for p.cur().isOp(".") {
		p.i++
		t := p.cur()
		if t.kind != tIdent && t.kind != tQIdent {
			return nil, p.errorf("expected an identifier after '.', found %q", t.text)
		}
		p.i++
		id.addPart(t.text, t.kind == tQIdent)
	}
	return id, nil
}

func (p *parser) call(name string) (Expr, error) {
	p.i++ // consume '('
	c := &Call{Name: strings.ToUpper(name)}
	if p.cur().isKeyword("DISTINCT") {
		c.Distinct = true
		p.i++
	}
	if !p.cur().isOp(")") {
		args, err := p.exprList()
		if err != nil {
			return nil, err
		}
		c.Args = args
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if p.cur().isKeyword("WITHIN") {
		wg, err := p.withinGroup()
		if err != nil {
			return nil, err
		}
		c.WithinGroup = wg
	}
	if p.cur().isKeyword("OVER") {
		over, err := p.rawOver()
		if err != nil {
			return nil, err
		}
		c.Over = over
	}
	return c, nil
}

// positionExpr parses the spec's `POSITION(substr IN str)`, a REQUIRED string
// function whose IN is part of the call rather than the set-membership
// operator. The substring argument is parsed above comparison precedence so the
// IN is not swallowed as an infix operator.
func (p *parser) positionExpr() (Expr, error) {
	p.i++ // POSITION
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	sub, err := p.expr(bpCompare + 1)
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("IN"); err != nil {
		return nil, err
	}
	str, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &Call{Name: "POSITION", Args: []Expr{sub, str}, ArgSep: " IN "}, nil
}

// withinGroup parses `WITHIN GROUP (ORDER BY expr [ASC|DESC] [, ...])`.
func (p *parser) withinGroup() ([]Expr, error) {
	p.i++ // WITHIN
	if err := p.expectKeyword("GROUP"); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if err := p.expectKeyword("ORDER"); err != nil {
		return nil, err
	}
	if err := p.expectKeyword("BY"); err != nil {
		return nil, err
	}
	var out []Expr
	for {
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if p.cur().isKeyword("ASC") || p.cur().isKeyword("DESC") {
			p.i++
		}
		if !p.cur().isOp(",") {
			break
		}
		p.i++
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return out, nil
}

// rawOver captures an OVER clause verbatim. The planner refuses window
// functions in v1; capturing rather than dropping means the refusal can name
// the construct.
func (p *parser) rawOver() (string, error) {
	p.i++ // OVER
	if !p.cur().isOp("(") {
		return "", p.errorf("expected '(' after OVER")
	}
	depth, start := 0, p.cur().col
	for p.cur().kind != tEOF {
		switch {
		case p.cur().isOp("("):
			depth++
		case p.cur().isOp(")"):
			depth--
			if depth == 0 {
				end := p.cur().col + 1
				p.i++
				return p.src[start:end], nil
			}
		}
		p.i++
	}
	return "", p.errorf("unterminated OVER clause")
}

func (p *parser) exprList() ([]Expr, error) {
	var out []Expr
	for {
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if !p.cur().isOp(",") {
			return out, nil
		}
		p.i++
	}
}

func (p *parser) caseExpr() (Expr, error) {
	p.i++ // CASE
	c := &Case{}
	if !p.cur().isKeyword("WHEN") {
		op, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		c.Operand = op
	}
	for p.cur().isKeyword("WHEN") {
		p.i++
		cond, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("THEN"); err != nil {
			return nil, err
		}
		then, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, When{Cond: cond, Then: then})
	}
	if len(c.Whens) == 0 {
		return nil, p.errorf("CASE requires at least one WHEN")
	}
	if p.cur().isKeyword("ELSE") {
		p.i++
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		c.Else = e
	}
	if err := p.expectKeyword("END"); err != nil {
		return nil, err
	}
	return c, nil
}

func (p *parser) castExpr() (Expr, error) {
	p.i++ // CAST
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	x, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	typ, err := p.typeName()
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &Cast{X: x, Type: typ}, nil
}

// typeName reads a type, including a parenthesised precision such as
// DECIMAL(18, 2).
func (p *parser) typeName() (string, error) {
	if p.cur().kind != tIdent {
		return "", p.errorf("expected a type name, found %q", p.cur().text)
	}
	var b strings.Builder
	b.WriteString(p.next().upper())
	if p.cur().isOp("(") {
		b.WriteString("(")
		p.i++
		for !p.cur().isOp(")") {
			if p.cur().kind == tEOF {
				return "", p.errorf("unterminated type precision")
			}
			b.WriteString(p.next().text)
			if p.cur().isOp(",") {
				b.WriteString(", ")
				p.i++
			}
		}
		p.i++
		b.WriteString(")")
	}
	return b.String(), nil
}

func (p *parser) extractExpr() (Expr, error) {
	p.i++ // EXTRACT
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if p.cur().kind != tIdent {
		return nil, p.errorf("expected a date part, found %q", p.cur().text)
	}
	part := p.next().upper()
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	x, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &Extract{Part: part, X: x}, nil
}

// intervalExpr parses `INTERVAL '<n>' <unit>` and retains the whole thing as a
// literal, because no planning decision depends on its internals.
func (p *parser) intervalExpr() (Expr, error) {
	p.i++ // INTERVAL
	if p.cur().kind != tStr && p.cur().kind != tNum {
		return nil, p.errorf("expected a quantity after INTERVAL, found %q", p.cur().text)
	}
	qty := p.next()
	text := "INTERVAL '" + qty.text + "'"
	if p.cur().kind == tIdent {
		text += " " + p.next().upper()
	}
	return &Lit{Kind: LitInterval, Text: text}, nil
}

// ---------- analysis ----------

// Walk calls fn for every node, depth first. Returning false from fn stops
// descent into that node's children.
func Walk(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	switch n := e.(type) {
	case *Call:
		for _, a := range n.Args {
			Walk(a, fn)
		}
		for _, a := range n.WithinGroup {
			Walk(a, fn)
		}
	case *Unary:
		Walk(n.X, fn)
	case *Binary:
		Walk(n.L, fn)
		Walk(n.R, fn)
	case *Paren:
		Walk(n.X, fn)
	case *Cast:
		Walk(n.X, fn)
	case *Extract:
		Walk(n.X, fn)
	case *IsNull:
		Walk(n.X, fn)
	case *In:
		Walk(n.X, fn)
		for _, a := range n.List {
			Walk(a, fn)
		}
	case *Between:
		Walk(n.X, fn)
		Walk(n.Lo, fn)
		Walk(n.Hi, fn)
	case *Case:
		Walk(n.Operand, fn)
		for _, w := range n.Whens {
			Walk(w.Cond, fn)
			Walk(w.Then, fn)
		}
		Walk(n.Else, fn)
	}
}

// Refs returns every identifier referenced by the expression, in source order.
// This is how the planner learns which datasets a metric needs.
func Refs(e Expr) []*Ident {
	var out []*Ident
	Walk(e, func(n Expr) bool {
		if id, ok := n.(*Ident); ok {
			out = append(out, id)
		}
		return true
	})
	return out
}

// Aggregates returns every aggregate call in the expression.
func Aggregates(e Expr) []*Call {
	var out []*Call
	Walk(e, func(n Expr) bool {
		if c, ok := n.(*Call); ok && c.Over == "" && IsAggregate(c.Name) {
			out = append(out, c)
		}
		return true
	})
	return out
}

// WindowCalls returns every call carrying an OVER clause, which v1 refuses.
func WindowCalls(e Expr) []*Call {
	var out []*Call
	Walk(e, func(n Expr) bool {
		if c, ok := n.(*Call); ok && c.Over != "" {
			out = append(out, c)
		}
		return true
	})
	return out
}

// HasAggregateInside reports whether any aggregate appears strictly below an
// aggregate, which is invalid SQL and a common hand-authoring mistake.
func HasAggregateInside(c *Call) bool {
	for _, a := range c.Args {
		found := false
		Walk(a, func(n Expr) bool {
			if inner, ok := n.(*Call); ok && IsAggregate(inner.Name) {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}
