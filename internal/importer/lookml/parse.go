package lookml

import (
	"fmt"
	"strings"
)

// LookML's shape, as a tree.
//
// Every construct is one of three things:
//
//	key: value                    a setting
//	key: name { ... }             a named block, like a view or a dimension
//	key: [a, b]                   a list
//
// Parsed into a generic node rather than into typed structs, because the
// language has a long tail of keys this importer does not read and a struct
// per construct would fail on the first one it had not heard of. A node
// tree ignores what it does not need and keeps the file parseable.

// node is one block.
type node struct {
	// Kind is the keyword: view, explore, dimension, measure, join.
	Kind string
	// Name is the identifier after the colon, where there is one.
	Name string
	// Settings are the scalar values directly inside this block.
	Settings map[string]string
	// Lists are the bracketed values.
	Lists map[string][]string
	// Children are the nested blocks, in file order.
	Children []*node
	Line     int
}

func (n *node) get(key string) string { return n.Settings[strings.ToLower(key)] }

func (n *node) is(key, want string) bool {
	return strings.EqualFold(n.get(key), want)
}

// childrenOf returns the nested blocks of one kind, in file order.
func (n *node) childrenOf(kind string) []*node {
	var out []*node
	for _, c := range n.Children {
		if strings.EqualFold(c.Kind, kind) {
			out = append(out, c)
		}
	}
	return out
}

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }

// parse reads one LookML file into a synthetic root block.
func parse(src string) (*node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	root := newNode("file", "", 1)

	for p.peek().kind != eof {
		if err := p.entry(root); err != nil {
			return nil, err
		}
	}
	return root, nil
}

func newNode(kind, name string, line int) *node {
	return &node{
		Kind: kind, Name: name, Line: line,
		Settings: map[string]string{},
		Lists:    map[string][]string{},
	}
}

// entry reads one `key: ...` into parent.
func (p *parser) entry(parent *node) error {
	key := p.next()
	if key.kind != word {
		return fmt.Errorf("line %d: expected a key, found %q", key.line, key.text)
	}

	// A bare `key { ... }` with no colon, which LookML allows for a few
	// constructs such as `derived_table`.
	if p.peek().text == "{" {
		child := newNode(key.text, "", key.line)
		if err := p.block(child); err != nil {
			return err
		}
		parent.Children = append(parent.Children, child)
		return nil
	}

	if p.peek().text != ":" {
		return fmt.Errorf("line %d: expected ':' after %q", key.line, key.text)
	}
	p.next()

	value := p.next()
	switch {
	case value.kind == raw:
		parent.Settings[strings.ToLower(key.text)] = value.text

	case value.text == "[":
		list, err := p.list()
		if err != nil {
			return err
		}
		parent.Lists[strings.ToLower(key.text)] = list

	case value.kind == word || value.kind == literal:
		// `view: orders {` is a named block; `type: number` is a setting.
		// The brace is what distinguishes them.
		if p.peek().text == "{" {
			child := newNode(key.text, value.text, key.line)
			if err := p.block(child); err != nil {
				return err
			}
			parent.Children = append(parent.Children, child)
			return nil
		}
		parent.Settings[strings.ToLower(key.text)] = value.text

	case value.text == "{":
		// An anonymous block, such as `filters: { field: x value: y }`.
		child := newNode(key.text, "", key.line)
		p.i-- // put the brace back
		if err := p.block(child); err != nil {
			return err
		}
		parent.Children = append(parent.Children, child)

	default:
		return fmt.Errorf("line %d: unexpected %q after %q", value.line, value.text, key.text)
	}
	return nil
}

func (p *parser) block(into *node) error {
	if p.next().text != "{" {
		return fmt.Errorf("line %d: expected '{'", into.Line)
	}
	for {
		t := p.peek()
		switch {
		case t.kind == eof:
			return fmt.Errorf("line %d: %s block is not closed", into.Line, into.Kind)
		case t.text == "}":
			p.next()
			return nil
		}
		if err := p.entry(into); err != nil {
			return err
		}
	}
}

func (p *parser) list() ([]string, error) {
	var out []string
	for {
		t := p.next()
		switch {
		case t.text == "]":
			return out, nil
		case t.text == ",":
			continue
		case t.kind == eof:
			return nil, fmt.Errorf("line %d: list is not closed", t.line)
		default:
			out = append(out, t.text)
		}
	}
}
