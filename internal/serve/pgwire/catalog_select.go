package pgwire

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A restricted SELECT over the synthesised catalogue.
//
// Deliberately a different parser from the semantic one in parse.go, and
// deliberately not shared with it. They accept different things for
// different reasons: the semantic parser turns names into a plan.Request
// that the model must resolve, and this one reads rows out of a fixed
// in-memory table. Merging them would mean one grammar where a catalogue
// name and a metric name are both legal in the same position, which is
// exactly the confusion the two-surface split exists to prevent.
//
// The rule throughout: understand the whole statement or decline it. There
// is no partial evaluation, no ignored WHERE clause, no best effort. A
// declined statement falls through to a refusal that names what does work,
// which is recoverable; a filter quietly dropped is not, because the caller
// gets rows and believes them.
//
// What is accepted:
//
//	SELECT <items> FROM <relation> [[AS] alias]
//	  [WHERE <predicate> [AND <predicate>]...]
//	  [ORDER BY <ref|position> [ASC|DESC]...]
//	  [LIMIT <n>]
//
// What is declined, and is common in what real clients send: joins, CASE,
// casts, subqueries, OR, ANY() over an array, function calls, OFFSET. Those
// are where pg_catalog stops being a table and starts being a database.

// catalogSelect is a parsed restricted catalogue query.
type catalogSelect struct {
	rel   relation
	alias string
	items []selectItem
	where []predicate
	order []orderTerm
	limit int // -1 for none
}

type selectItem struct {
	star bool
	// column is the index into rel.columns, for a reference.
	column int
	// header is what the client sees, which is the alias when one is given.
	header string
	// literal is a constant the client selected, held as its rendered value.
	// Clients do send `SELECT 1 FROM ...` and a bare NULL to probe.
	literal   any
	isLiteral bool
}

type predicate struct {
	column int
	op     string // "=", "<>", "like", "isnull", "isnotnull", "in"
	values []any
}

type orderTerm struct {
	column     int
	descending bool
}

// evalCatalogSelect answers a statement out of the synthesised catalogue,
// reporting false when it does not fully understand it.
func evalCatalogSelect(sql string, schemas []namespaceSchema) (*result, bool) {
	q, ok := parseCatalogSelect(sql)
	if !ok {
		return nil, false
	}

	rows := q.rel.rows(schemas)

	filtered := make([][]any, 0, len(rows))
	for _, row := range rows {
		if matchesAll(row, q.where) {
			filtered = append(filtered, row)
		}
	}

	if len(q.order) > 0 {
		sort.SliceStable(filtered, func(i, j int) bool {
			for _, term := range q.order {
				cmp := compareCells(filtered[i][term.column], filtered[j][term.column])
				if cmp == 0 {
					continue
				}
				if term.descending {
					return cmp > 0
				}
				return cmp < 0
			}
			return false
		})
	}

	if q.limit >= 0 && len(filtered) > q.limit {
		filtered = filtered[:q.limit]
	}

	out := &result{tag: "SELECT", exact: true}
	for _, item := range q.items {
		switch {
		case item.star:
			out.columns = append(out.columns, q.rel.columns...)
			out.oids = append(out.oids, q.rel.oids...)
		case item.isLiteral:
			out.columns = append(out.columns, item.header)
			out.oids = append(out.oids, oidOf(item.literal))
		default:
			out.columns = append(out.columns, item.header)
			out.oids = append(out.oids, q.rel.oids[item.column])
		}
	}
	for _, row := range filtered {
		projected := make([]any, 0, len(out.columns))
		for _, item := range q.items {
			switch {
			case item.star:
				projected = append(projected, row...)
			case item.isLiteral:
				projected = append(projected, item.literal)
			default:
				projected = append(projected, row[item.column])
			}
		}
		out.rows = append(out.rows, projected)
	}
	return out, true
}

func matchesAll(row []any, preds []predicate) bool {
	for _, p := range preds {
		if !matchesPredicate(row[p.column], p) {
			return false
		}
	}
	return true
}

func matchesPredicate(cell any, p predicate) bool {
	switch p.op {
	case "isnull":
		return cell == nil
	case "isnotnull":
		return cell != nil
	case "like":
		text, ok := cell.(string)
		if !ok {
			return false
		}
		pattern, _ := p.values[0].(string)
		return likePattern(pattern).MatchString(text)
	case "in":
		for _, want := range p.values {
			if cellEquals(cell, want) {
				return true
			}
		}
		return false
	case "=":
		return cellEquals(cell, p.values[0])
	case "<>":
		// A null is not unequal to anything, it is unknown, and PostgreSQL
		// drops the row. Returning true here would surface rows a client
		// filtered out.
		return cell != nil && !cellEquals(cell, p.values[0])
	}
	return false
}

// cellEquals compares a synthesised cell to a literal the client wrote.
//
// Types are compared, not rendered. `attrelid = '16384'` is not the same
// question as `attrelid = 16384` in PostgreSQL, and a comparison that
// stringified both would answer the first one, which a client never asks.
func cellEquals(cell, want any) bool {
	if cell == nil || want == nil {
		return false
	}
	switch c := cell.(type) {
	case string:
		w, ok := want.(string)
		return ok && c == w
	case bool:
		switch w := want.(type) {
		case bool:
			return c == w
		case string:
			// 't'/'f' is how a client writes a boolean into a catalogue
			// predicate, because that is how PostgreSQL renders one.
			return (c && (w == "t" || w == "true")) || (!c && (w == "f" || w == "false"))
		}
		return false
	case int64:
		w, ok := want.(int64)
		return ok && c == w
	case float64:
		switch w := want.(type) {
		case float64:
			return c == w
		case int64:
			return c == float64(w)
		}
		return false
	}
	return false
}

func compareCells(a, b any) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return 1 // nulls last, as PostgreSQL orders them ascending
		default:
			return -1
		}
	}
	switch x := a.(type) {
	case int64:
		if y, ok := b.(int64); ok {
			return cmp(x, y)
		}
	case float64:
		if y, ok := b.(float64); ok {
			return cmp(x, y)
		}
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y)
		}
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

func cmp[T int64 | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// likePattern turns a SQL LIKE pattern into a regexp.
//
// Everything outside % and _ is quoted, so a pattern holding a regexp
// metacharacter matches it literally rather than being interpreted. Clients
// do send patterns containing dots.
func likePattern(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	// The pattern is built from quoted input, so this cannot fail to
	// compile; MustCompile says so rather than adding an error path that
	// nothing can reach.
	return regexp.MustCompile(b.String())
}

// parseCatalogSelect reads the restricted grammar, reporting false for
// anything outside it.
func parseCatalogSelect(sql string) (*catalogSelect, bool) {
	toks, err := lex(sql)
	if err != nil {
		return nil, false
	}
	p := &catalogParser{toks: toks}

	if !p.take("select") {
		return nil, false
	}
	// The select list is read before the relation is known, so it is held as
	// raw references and resolved once FROM has been parsed. A client writes
	// the columns first and there is nothing to resolve them against yet.
	refs, ok := p.selectList()
	if !ok {
		return nil, false
	}
	if !p.take("from") {
		return nil, false
	}
	name, ok := p.qualifiedName()
	if !ok {
		return nil, false
	}
	rel, ok := findRelation(name)
	if !ok {
		return nil, false
	}

	q := &catalogSelect{rel: rel, limit: -1}

	// An alias, with or without AS. Anything else here is a join or a
	// comma-separated second relation, neither of which this evaluates.
	p.take("as")
	if p.peek().kind == tokWord && !p.peekIsKeyword() {
		q.alias = strings.ToLower(p.next().text)
	}

	// The unqualified relation name is also a legal qualifier, which is what
	// a client writes when it did not alias.
	short := name
	if at := strings.LastIndex(short, "."); at >= 0 {
		short = short[at+1:]
	}
	qualifiers := []string{short, name}
	if q.alias != "" {
		qualifiers = append(qualifiers, q.alias)
	}

	for _, ref := range refs {
		item, ok := resolveItem(ref, rel, qualifiers)
		if !ok {
			return nil, false
		}
		q.items = append(q.items, item)
	}

	if p.take("where") {
		for {
			pred, ok := p.predicate(rel, qualifiers)
			if !ok {
				return nil, false
			}
			q.where = append(q.where, pred)
			if !p.take("and") {
				break
			}
		}
	}

	if p.take("order") {
		if !p.take("by") {
			return nil, false
		}
		for {
			term, ok := p.orderTerm(rel, qualifiers, q.items)
			if !ok {
				return nil, false
			}
			q.order = append(q.order, term)
			if !p.take(",") {
				break
			}
		}
	}

	if p.take("limit") {
		t := p.next()
		if t.kind != tokNumber {
			return nil, false
		}
		n, err := strconv.Atoi(t.text)
		if err != nil || n < 0 {
			return nil, false
		}
		q.limit = n
	}

	p.take(";")
	if p.peek().kind != tokEOF {
		// Trailing anything: OFFSET, a second statement, a FOR UPDATE. Not
		// understood, so not answered.
		return nil, false
	}
	return q, true
}

// rawRef is a select-list entry before the relation is known.
type rawRef struct {
	star      bool
	qualifier string
	name      string
	alias     string
	literal   any
	isLiteral bool
}

func resolveItem(ref rawRef, rel relation, qualifiers []string) (selectItem, bool) {
	if ref.star {
		if ref.qualifier != "" && !containsFold(qualifiers, ref.qualifier) {
			return selectItem{}, false
		}
		return selectItem{star: true}, true
	}
	if ref.isLiteral {
		header := ref.alias
		if header == "" {
			header = "?column?"
		}
		return selectItem{isLiteral: true, literal: ref.literal, header: header}, true
	}
	if ref.qualifier != "" && !containsFold(qualifiers, ref.qualifier) {
		// A qualifier naming something other than this relation means a
		// join. Answering with this relation's rows would be an answer to a
		// different question.
		return selectItem{}, false
	}
	at := columnIndex(rel, ref.name)
	if at < 0 {
		return selectItem{}, false
	}
	header := ref.alias
	if header == "" {
		header = rel.columns[at]
	}
	return selectItem{column: at, header: header}, true
}

func columnIndex(rel relation, name string) int {
	for i, have := range rel.columns {
		if strings.EqualFold(have, name) {
			return i
		}
	}
	return -1
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

// catalogParser walks the token stream. Bounds are safe because lex always
// terminates the stream with a tokEOF that matches nothing.
type catalogParser struct {
	toks []token
	i    int
}

func (p *catalogParser) peek() token { return p.toks[p.i] }

func (p *catalogParser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *catalogParser) take(text string) bool {
	if p.toks[p.i].is(text) {
		p.i++
		return true
	}
	return false
}

// peekIsKeyword reports whether the next word is one that ends the FROM
// clause, so a keyword is never swallowed as a table alias.
func (p *catalogParser) peekIsKeyword() bool {
	for _, kw := range []string{"where", "order", "limit", "group", "having",
		"union", "join", "inner", "left", "right", "full", "cross", "on",
		"offset", "for", "as"} {
		if p.toks[p.i].is(kw) {
			return true
		}
	}
	return false
}

func (p *catalogParser) selectList() ([]rawRef, bool) {
	var out []rawRef
	for {
		ref, ok := p.selectItem()
		if !ok {
			return nil, false
		}
		out = append(out, ref)
		if !p.take(",") {
			return out, true
		}
	}
}

func (p *catalogParser) selectItem() (rawRef, bool) {
	if p.take("*") {
		return rawRef{star: true}, true
	}

	t := p.peek()
	switch t.kind {
	case tokString:
		p.next()
		return p.withAlias(rawRef{isLiteral: true, literal: t.text})
	case tokNumber:
		p.next()
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return rawRef{}, false
		}
		return p.withAlias(rawRef{isLiteral: true, literal: n})
	case tokWord, tokQuoted:
		if t.kind == tokWord && strings.EqualFold(t.text, "null") {
			p.next()
			return p.withAlias(rawRef{isLiteral: true, literal: nil})
		}
		name, qualifier, star, ok := p.reference()
		if !ok {
			return rawRef{}, false
		}
		if star {
			return rawRef{star: true, qualifier: qualifier}, true
		}
		return p.withAlias(rawRef{qualifier: qualifier, name: name})
	}
	return rawRef{}, false
}

// reference reads `name`, `qualifier.name` or `qualifier.*`.
func (p *catalogParser) reference() (name, qualifier string, star, ok bool) {
	t := p.next()
	if t.kind != tokWord && t.kind != tokQuoted {
		return "", "", false, false
	}
	name = t.text
	for p.peek().is(".") {
		p.next()
		if p.peek().is("*") {
			p.next()
			return "", name, true, true
		}
		nt := p.next()
		if nt.kind != tokWord && nt.kind != tokQuoted {
			return "", "", false, false
		}
		qualifier, name = name, nt.text
	}
	if p.peek().is("(") {
		return "", "", false, false // a function call
	}
	if p.peek().is("::") {
		return "", "", false, false // a cast
	}
	return name, qualifier, false, true
}

func (p *catalogParser) withAlias(ref rawRef) (rawRef, bool) {
	if p.take("as") {
		t := p.next()
		if t.kind != tokWord && t.kind != tokQuoted {
			return rawRef{}, false
		}
		ref.alias = t.text
		return ref, true
	}
	// A bare word after an item is an alias, unless it is the keyword that
	// ends the list.
	if p.peek().kind == tokWord && !p.peekIsKeyword() && !p.peek().is("from") {
		ref.alias = p.next().text
	}
	return ref, true
}

func (p *catalogParser) qualifiedName() (string, bool) {
	t := p.next()
	if t.kind != tokWord && t.kind != tokQuoted {
		return "", false
	}
	name := t.text
	for p.peek().is(".") {
		p.next()
		nt := p.next()
		if nt.kind != tokWord && nt.kind != tokQuoted {
			return "", false
		}
		name += "." + nt.text
	}
	return strings.ToLower(name), true
}

func (p *catalogParser) predicate(rel relation, qualifiers []string) (predicate, bool) {
	name, qualifier, star, ok := p.reference()
	if !ok || star {
		return predicate{}, false
	}
	if qualifier != "" && !containsFold(qualifiers, qualifier) {
		return predicate{}, false
	}
	at := columnIndex(rel, name)
	if at < 0 {
		// A predicate on a column this does not synthesise. Ignoring it
		// would return rows the client asked to exclude, so the statement is
		// declined instead.
		return predicate{}, false
	}

	switch {
	case p.take("is"):
		if p.take("not") {
			if !p.take("null") {
				return predicate{}, false
			}
			return predicate{column: at, op: "isnotnull"}, true
		}
		if !p.take("null") {
			return predicate{}, false
		}
		return predicate{column: at, op: "isnull"}, true

	case p.take("in"):
		if !p.take("(") {
			return predicate{}, false
		}
		var values []any
		for {
			v, ok := p.literal()
			if !ok {
				return predicate{}, false
			}
			values = append(values, v)
			if p.take(",") {
				continue
			}
			if !p.take(")") {
				return predicate{}, false
			}
			return predicate{column: at, op: "in", values: values}, true
		}

	case p.take("like"):
		v, ok := p.literal()
		if !ok {
			return predicate{}, false
		}
		return predicate{column: at, op: "like", values: []any{v}}, true

	case p.take("="):
		v, ok := p.literal()
		if !ok {
			return predicate{}, false
		}
		return predicate{column: at, op: "=", values: []any{v}}, true

	case p.take("<>"), p.take("!="):
		v, ok := p.literal()
		if !ok {
			return predicate{}, false
		}
		return predicate{column: at, op: "<>", values: []any{v}}, true
	}
	return predicate{}, false
}

func (p *catalogParser) literal() (any, bool) {
	t := p.next()
	switch {
	case t.kind == tokString:
		return t.text, true
	case t.kind == tokNumber:
		if n, err := strconv.ParseInt(t.text, 10, 64); err == nil {
			return n, true
		}
		f, err := strconv.ParseFloat(t.text, 64)
		return f, err == nil
	case t.is("true"):
		return true, true
	case t.is("false"):
		return false, true
	case t.is("null"):
		return nil, true
	}
	return nil, false
}

func (p *catalogParser) orderTerm(rel relation, qualifiers []string, items []selectItem) (orderTerm, bool) {
	var term orderTerm

	if t := p.peek(); t.kind == tokNumber {
		p.next()
		// ORDER BY 2 refers to the second output column, and a star makes
		// that position ambiguous without expanding it. Declining is
		// cheaper than being subtly wrong about which column was meant.
		n, err := strconv.Atoi(t.text)
		if err != nil || n < 1 || n > len(items) {
			return orderTerm{}, false
		}
		item := items[n-1]
		if item.star || item.isLiteral {
			return orderTerm{}, false
		}
		term.column = item.column
	} else {
		name, qualifier, star, ok := p.reference()
		if !ok || star {
			return orderTerm{}, false
		}
		if qualifier != "" && !containsFold(qualifiers, qualifier) {
			return orderTerm{}, false
		}
		at := columnIndex(rel, name)
		if at < 0 {
			return orderTerm{}, false
		}
		term.column = at
	}

	switch {
	case p.take("desc"):
		term.descending = true
	case p.take("asc"):
	}
	return term, true
}
