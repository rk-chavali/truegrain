package lookml

import (
	"fmt"
	"strings"
	"unicode"
)

// A lexer for LookML.
//
// LookML is not YAML and not JSON. It is its own small language, and the
// awkward part is that SQL is embedded in it terminated by a double
// semicolon:
//
//	dimension: order_id {
//	  primary_key: yes
//	  sql: ${TABLE}.order_id ;;
//	}
//
// So `sql:` and its relatives are lexed as a raw run of characters up to
// `;;`, not as ordinary tokens. Treating them as tokens would break on the
// first CASE expression, and treating everything as raw would lose the
// block structure this needs.

type kind int

const (
	eof kind = iota
	word
	// literal is a "quoted string".
	literal
	// raw is everything between `sql:` and `;;`, kept verbatim.
	raw
	punct // { } [ ] , :
)

type token struct {
	kind kind
	text string
	line int
}

// blockValued are the keys whose value runs to `;;` rather than to the end
// of the line.
//
// Listed rather than detected, because the terminator is what decides how
// to read the value and guessing wrong silently truncates somebody's SQL at
// the first space.
var blockValued = map[string]bool{
	"sql":               true,
	"sql_on":            true,
	"sql_table_name":    true,
	"sql_where":         true,
	"sql_always_where":  true,
	"sql_always_having": true,
	"sql_trigger_value": true,
	"sql_foreign_key":   true,
	"sql_distinct_key":  true,
	"sql_latitude":      true,
	"sql_longitude":     true,
	"sql_start":         true,
	"sql_end":           true,
	"html":              true,
	"expression":        true,
	"derived_table":     false, // a block, not a value
}

func lex(src string) ([]token, error) {
	var out []token
	runes := []rune(src)
	line := 1

	for i := 0; i < len(runes); {
		c := runes[i]

		switch {
		case c == '\n':
			line++
			i++

		case unicode.IsSpace(c):
			i++

		case c == '#':
			for i < len(runes) && runes[i] != '\n' {
				i++
			}

		case c == '"' || c == '\'':
			quote := c
			i++
			var b strings.Builder
			for {
				if i >= len(runes) {
					return nil, fmt.Errorf("line %d: unterminated string", line)
				}
				if runes[i] == '\\' && i+1 < len(runes) {
					b.WriteRune(runes[i+1])
					i += 2
					continue
				}
				if runes[i] == quote {
					i++
					break
				}
				if runes[i] == '\n' {
					line++
				}
				b.WriteRune(runes[i])
				i++
			}
			out = append(out, token{kind: literal, text: b.String(), line: line})

		case strings.ContainsRune("{}[],:", c):
			out = append(out, token{kind: punct, text: string(c), line: line})
			i++

			// A key whose value is SQL: read to `;;` verbatim.
			//
			// Decided here, at the colon, because that is the only point
			// where the preceding key is known. `sql: ${TABLE}.x ;;` and
			// `type: number` differ only in which key came first.
			if c == ':' && len(out) >= 2 {
				prev := out[len(out)-2]
				if prev.kind == word && blockValued[strings.ToLower(prev.text)] {
					value, consumed, ok := readToDoubleSemicolon(runes[i:])
					if !ok {
						return nil, fmt.Errorf(
							"line %d: %s: is not terminated by ;;", line, prev.text)
					}
					line += strings.Count(string(runes[i:i+consumed]), "\n")
					out = append(out, token{kind: raw, text: value, line: line})
					i += consumed
				}
			}

		default:
			start := i
			for i < len(runes) && !unicode.IsSpace(runes[i]) &&
				!strings.ContainsRune("{}[],:#\"'", runes[i]) {
				i++
			}
			if i == start {
				return nil, fmt.Errorf("line %d: unexpected character %q", line, string(c))
			}
			out = append(out, token{kind: word, text: string(runes[start:i]), line: line})
		}
	}

	return append(out, token{kind: eof, line: line}), nil
}

// readToDoubleSemicolon returns the text up to `;;`, and how much was
// consumed including the terminator.
//
// A single semicolon is ordinary SQL punctuation and must not end the
// value: `sql: CASE WHEN x THEN 1 END ;;` contains none, but a derived
// table routinely does.
func readToDoubleSemicolon(runes []rune) (string, int, bool) {
	for i := 0; i+1 < len(runes); i++ {
		if runes[i] == ';' && runes[i+1] == ';' {
			return strings.TrimSpace(string(runes[:i])), i + 2, true
		}
	}
	return "", 0, false
}
