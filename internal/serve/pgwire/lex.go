package pgwire

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// A lexer for the restricted SELECT this server accepts.
//
// Hand-written rather than pulled from a full PostgreSQL parser, and that is a
// decision rather than an omission. The grammar below is deliberately small:
// the whole product is a thing that refuses questions it cannot answer
// correctly, and a parser that accepts every legal PostgreSQL statement would
// spend its life rejecting what it had just finished parsing. Refusing at the
// grammar is refusing earlier, with a better message, in a tenth of the code.
//
// What this means for security is more important than the size. A token stream
// never becomes warehouse SQL. It becomes a plan.Request holding metric and
// dimension names, which must resolve against the loaded model, and filter
// values, which become bound parameters. There is no concatenation path from
// anything here to a statement the warehouse runs.

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokWord
	// tokQuoted is a "double quoted" identifier. It stays distinct from
	// tokWord because a quoted identifier is case-sensitive and is never a
	// keyword: "select" is a column called select.
	tokQuoted
	tokString // 'single quoted' literal
	tokNumber
	tokPunct // , ( ) . * = < > etc
)

type token struct {
	kind tokenKind
	text string
	// pos is the byte offset, so an error can point at the offending token
	// rather than at the statement as a whole.
	pos int
}

func (t token) is(text string) bool {
	return (t.kind == tokWord || t.kind == tokPunct) && strings.EqualFold(t.text, text)
}

// lex turns a statement into tokens.
//
// It rejects rather than skips anything it does not recognise, because a lexer
// that silently drops a character it does not understand changes the meaning
// of the statement it is about to hand on.
func lex(sql string) ([]token, error) {
	var out []token
	runes := []rune(sql)

	for i := 0; i < len(runes); {
		c := runes[i]
		start := i

		switch {
		case unicode.IsSpace(c):
			i++

		// -- line comment. BI tools annotate statements with them.
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for i < len(runes) && runes[i] != '\n' {
				i++
			}

		// /* block comment */, which some drivers use to tag a query.
		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			i += 2
			closed := false
			for i+1 < len(runes) {
				if runes[i] == '*' && runes[i+1] == '/' {
					i += 2
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated block comment")
			}

		case c == '"':
			i++
			var b strings.Builder
			for {
				if i >= len(runes) {
					return nil, fmt.Errorf("unterminated quoted identifier")
				}
				if runes[i] == '"' {
					// "" inside a quoted identifier is one literal quote.
					if i+1 < len(runes) && runes[i+1] == '"' {
						b.WriteRune('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteRune(runes[i])
				i++
			}
			out = append(out, token{kind: tokQuoted, text: b.String(), pos: start})

		case c == '\'':
			i++
			var b strings.Builder
			for {
				if i >= len(runes) {
					return nil, fmt.Errorf("unterminated string literal")
				}
				if runes[i] == '\'' {
					// '' is an escaped quote. Getting this wrong is how a
					// value ends up truncated at the quote inside it.
					if i+1 < len(runes) && runes[i+1] == '\'' {
						b.WriteRune('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteRune(runes[i])
				i++
			}
			out = append(out, token{kind: tokString, text: b.String(), pos: start})

		case unicode.IsDigit(c):
			for i < len(runes) && (unicode.IsDigit(runes[i]) || runes[i] == '.') {
				i++
			}
			text := string(runes[start:i])
			if _, err := strconv.ParseFloat(text, 64); err != nil {
				return nil, fmt.Errorf("%q is not a number", text)
			}
			out = append(out, token{kind: tokNumber, text: text, pos: start})

		case c == '_' || unicode.IsLetter(c):
			for i < len(runes) && (runes[i] == '_' || runes[i] == '$' ||
				unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i])) {
				i++
			}
			out = append(out, token{kind: tokWord, text: string(runes[start:i]), pos: start})

		// Two-character operators before one-character ones, or <= lexes as
		// < followed by =, which parses as something else entirely.
		case c == '<' && i+1 < len(runes) && (runes[i+1] == '=' || runes[i+1] == '>'):
			out = append(out, token{kind: tokPunct, text: string(runes[i : i+2]), pos: start})
			i += 2
		case c == '>' && i+1 < len(runes) && runes[i+1] == '=':
			out = append(out, token{kind: tokPunct, text: ">=", pos: start})
			i += 2
		case c == '!' && i+1 < len(runes) && runes[i+1] == '=':
			out = append(out, token{kind: tokPunct, text: "<>", pos: start})
			i += 2

		case strings.ContainsRune(",()*.=<>;", c):
			out = append(out, token{kind: tokPunct, text: string(c), pos: start})
			i++

		// An operator the grammar does not implement still tokenises.
		//
		// The statement will be refused anyway, and refusing it for what it
		// asks rather than for a character in it is the difference between
		// "LIKE is not supported" and "unexpected character ~", which sends
		// somebody looking for a typo they did not make.
		case strings.ContainsRune("!~@#%^&|/+-:[]{}?", c):
			out = append(out, token{kind: tokPunct, text: string(c), pos: start})
			i++

		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", string(c), start)
		}
	}

	return append(out, token{kind: tokEOF, pos: len(runes)}), nil
}
