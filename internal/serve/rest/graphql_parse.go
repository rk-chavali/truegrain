package rest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// A parser for the GraphQL this endpoint accepts.
//
// Hand-written and restricted, for the reason the SQL grammar is: this
// schema has three fields, so a full GraphQL parser would spend its life
// rejecting what it had just finished parsing, and a library would be a
// dependency carrying fragments, directives, unions and subscriptions that
// this surface will never have.
//
// What is accepted:
//
//	{ metrics { name description } }
//	query Q($r: [String!]) { query(metrics: ["x"], dimensions: $r) { rows } }
//
// Arguments may be literals or variables. Selection sets are parsed and
// then ignored, because the response shape is fixed: a caller asking for
// a subset of fields gets all of them, which is wasteful and honest, and
// is better than pretending to a field-level projection this does not do.

// graphQLOp is one parsed operation.
type graphQLOp struct {
	// field is the top-level field: metrics, dimensions, query, __schema.
	field string
	// args are the literal arguments, already decoded.
	args map[string]any
	// varRefs maps an argument name to the variable it referenced, so the
	// value can be taken from the request's variables.
	varRefs map[string]string
}

// arg returns an argument, resolving a variable reference against the
// request's variables.
func (o *graphQLOp) arg(name string, variables map[string]any) any {
	if v, ok := o.args[name]; ok {
		return v
	}
	if ref, ok := o.varRefs[name]; ok {
		if v, ok := variables[ref]; ok {
			return v
		}
	}
	return nil
}

// parseGraphQL reads the operation out of a document.
func parseGraphQL(doc string) (*graphQLOp, error) {
	if strings.TrimSpace(doc) == "" {
		return nil, fmt.Errorf("the query is empty")
	}
	if strings.Contains(doc, "mutation") {
		return nil, fmt.Errorf(
			"there are no mutations: this engine has no write path, and a " +
				"semantic layer that could write would be a different product")
	}
	if strings.Contains(doc, "subscription") {
		return nil, fmt.Errorf(
			"there are no subscriptions here. To be told when a question is " +
				"refused, configure a webhook, which delivers the same audit " +
				"event with its code and hint")
	}
	if strings.Contains(doc, "...") {
		return nil, fmt.Errorf(
			"fragments are not supported: the response shape is fixed, so there " +
				"is nothing for a fragment to compose")
	}

	toks := lexGraphQL(doc)
	i := 0

	// An optional `query Name($v: Type)` header before the selection set.
	if i < len(toks) && toks[i] == "query" {
		i++
		for i < len(toks) && toks[i] != "{" {
			i++
		}
	}
	if i >= len(toks) || toks[i] != "{" {
		return nil, fmt.Errorf("expected a selection set, starting with {")
	}
	i++

	if i >= len(toks) {
		return nil, fmt.Errorf("the selection set is empty")
	}
	op := &graphQLOp{
		field:   toks[i],
		args:    map[string]any{},
		varRefs: map[string]string{},
	}
	i++

	if i < len(toks) && toks[i] == "(" {
		i++
		var err error
		if i, err = parseArgs(toks, i, op); err != nil {
			return nil, err
		}
	}
	return op, nil
}

// parseArgs reads `name: value` pairs up to the closing parenthesis.
func parseArgs(toks []string, i int, op *graphQLOp) (int, error) {
	for i < len(toks) {
		if toks[i] == ")" {
			return i + 1, nil
		}
		if toks[i] == "," {
			i++
			continue
		}
		name := toks[i]
		i++
		if i >= len(toks) || toks[i] != ":" {
			return i, fmt.Errorf("expected ':' after argument %q", name)
		}
		i++
		if i >= len(toks) {
			return i, fmt.Errorf("argument %q has no value", name)
		}

		if strings.HasPrefix(toks[i], "$") {
			op.varRefs[name] = strings.TrimPrefix(toks[i], "$")
			i++
			continue
		}

		value, next, err := parseValue(toks, i)
		if err != nil {
			return i, fmt.Errorf("argument %q: %w", name, err)
		}
		op.args[name] = value
		i = next
	}
	return i, fmt.Errorf("the argument list is not closed")
}

// parseValue reads one literal: a string, a number, a list or an object.
func parseValue(toks []string, i int) (any, int, error) {
	if i >= len(toks) {
		return nil, i, fmt.Errorf("expected a value")
	}
	t := toks[i]

	switch {
	case t == "[":
		var list []any
		i++
		for i < len(toks) && toks[i] != "]" {
			if toks[i] == "," {
				i++
				continue
			}
			v, next, err := parseValue(toks, i)
			if err != nil {
				return nil, i, err
			}
			list = append(list, v)
			i = next
		}
		if i >= len(toks) {
			return nil, i, fmt.Errorf("a list is not closed")
		}
		return list, i + 1, nil

	case t == "{":
		obj := map[string]any{}
		i++
		for i < len(toks) && toks[i] != "}" {
			if toks[i] == "," {
				i++
				continue
			}
			key := toks[i]
			i++
			if i >= len(toks) || toks[i] != ":" {
				return nil, i, fmt.Errorf("expected ':' after %q", key)
			}
			i++
			v, next, err := parseValue(toks, i)
			if err != nil {
				return nil, i, err
			}
			obj[strings.Trim(key, `"`)] = v
			i = next
		}
		if i >= len(toks) {
			return nil, i, fmt.Errorf("an object is not closed")
		}
		return obj, i + 1, nil

	case strings.HasPrefix(t, `"`):
		var s string
		if err := json.Unmarshal([]byte(t), &s); err != nil {
			return nil, i, fmt.Errorf("%s is not a valid string", t)
		}
		return s, i + 1, nil

	case t == "true":
		return true, i + 1, nil
	case t == "false":
		return false, i + 1, nil
	case t == "null":
		return nil, i + 1, nil
	}

	if n, err := strconv.ParseFloat(t, 64); err == nil {
		return n, i + 1, nil
	}
	// A bare word is an enum in GraphQL. Treated as a string, because the
	// only enums this would ever have are metric and dimension names, and
	// those are strings everywhere else in the product.
	return t, i + 1, nil
}

// lexGraphQL splits a document into tokens.
//
// Strings stay whole, punctuation separates, and comments and commas are
// dropped by the parser rather than here so the token stream stays a
// faithful reading of the input.
func lexGraphQL(doc string) []string {
	var out []string
	runes := []rune(doc)

	for i := 0; i < len(runes); {
		c := runes[i]
		switch {
		case unicode.IsSpace(c):
			i++

		case c == '#':
			for i < len(runes) && runes[i] != '\n' {
				i++
			}

		case c == '"':
			start := i
			i++
			for i < len(runes) {
				if runes[i] == '\\' {
					i += 2
					continue
				}
				if runes[i] == '"' {
					i++
					break
				}
				i++
			}
			out = append(out, string(runes[start:i]))

		case strings.ContainsRune("{}()[]:,$!=", c):
			out = append(out, string(c))
			i++

		default:
			start := i
			for i < len(runes) && !unicode.IsSpace(runes[i]) &&
				!strings.ContainsRune("{}()[]:,$!=#\"", runes[i]) {
				i++
			}
			if i == start {
				i++
				continue
			}
			out = append(out, string(runes[start:i]))
		}
	}

	// A variable is `$` then a name; rejoin them so an argument value reads
	// as one token.
	var joined []string
	for i := 0; i < len(out); i++ {
		if out[i] == "$" && i+1 < len(out) {
			joined = append(joined, "$"+out[i+1])
			i++
			continue
		}
		joined = append(joined, out[i])
	}
	return joined
}
