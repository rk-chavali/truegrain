// Package exec runs compiled SQL against a warehouse.
package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// DuckDB executes through the standalone `duckdb` command line binary.
//
// Why a subprocess rather than the cgo bindings: the bindings need a 64-bit C
// toolchain, and requiring one to run the quickstart loses most visitors at the
// first step. The CLI is a single self-contained executable on every platform.
// Build with the `cgoduckdb` tag to use the in-process driver instead, which
// binds parameters natively.
//
// The tradeoff this makes, stated plainly: the DuckDB CLI has no parameter
// binding, so values are rendered as typed SQL literals by bindLiteral below.
// That renderer accepts exactly five Go types, all of them produced by the
// planner's own validation, and rejects everything else. It is not a general
// string concatenation path and nothing a caller sends reaches it unvalidated.
// It is still the weakest link in this file, which is why it is short, total,
// and tested directly.
type DuckDB struct {
	binary string
	// database is the file to open, or ":memory:".
	database string
	timeout  time.Duration
}

// DuckDBOptions configure the executor.
type DuckDBOptions struct {
	// Database is a DuckDB file path, or empty for an in-memory database.
	Database string
	// Binary overrides the `duckdb` executable to run.
	Binary string
	// Timeout bounds a single query. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds a query so a runaway scan cannot hold a request open
// indefinitely.
const DefaultTimeout = 2 * time.Minute

// NewDuckDB locates the duckdb binary and returns an executor.
func NewDuckDB(opts DuckDBOptions) (*DuckDB, error) {
	binary := opts.Binary
	if binary == "" {
		binary = "duckdb"
	}
	path, err := findBinary(binary)
	if err != nil {
		return nil, fmt.Errorf(
			"duckdb command line binary not found: %w\n"+
				"run `make duckdb` to put it next to the truegrain binary, or install it from "+
				"https://duckdb.org/docs/installation/ (a single executable, no compiler needed). "+
				"`truegrain compile` needs no database at all", err)
	}
	db := opts.Database
	if db == "" {
		db = ":memory:"
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &DuckDB{binary: path, database: db, timeout: timeout}, nil
}

// Name identifies the executor, including the fact that DuckDB has no identity
// model, so a reader of `health` is not misled into thinking impersonation is
// happening here.
func (d *DuckDB) Name() string {
	return "duckdb-cli (" + d.database + "); DuckDB has no identity model, so queries do not run as the caller"
}

// Execute runs the statement and returns all rows.
//
// The identity is accepted and deliberately unused: DuckDB is a local file with
// no notion of a caller. Any access control in this configuration came from the
// engine's gate before this point.
func (d *DuckDB) Execute(ctx context.Context, _ govern.Identity, sql string, params []any) (*engine.Rows, error) {
	stmt, err := inline(sql, params)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.binary, "-json", "-readonly", d.database)
	cmd.Stdin = strings.NewReader(stmt + ";\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("query exceeded the %s timeout", d.timeout)
		}
		return nil, fmt.Errorf("duckdb: %s", msg)
	}

	return parseJSONRows(stdout.Bytes())
}

// Close releases nothing: each query is its own process.
func (d *DuckDB) Close() error { return nil }

// findBinary resolves the duckdb executable, falling back to the directory
// holding this program.
//
// The fallback matters because an MCP server is spawned by an agent host, which
// does not pass the shell PATH a developer set up by hand. Shipping the two
// executables side by side and looking next to ourselves is what makes
// `make duckdb` enough, with no PATH edits and nothing installed system wide.
func findBinary(name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		// An explicit path was given; use it as-is.
		return exec.LookPath(name)
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(filepath.Dir(self), name)
	if runtime.GOOS == "windows" {
		candidate += ".exe"
	}
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate, nil
	}
	return "", fmt.Errorf("%q is on neither PATH nor beside %s", name, filepath.Dir(self))
}

// parseJSONRows converts DuckDB's `-json` output into columns and rows.
//
// Column order is taken from the first object's key order, which the DuckDB CLI
// emits in select order.
func parseJSONRows(out []byte) (*engine.Rows, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return &engine.Rows{}, nil
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, fmt.Errorf("parsing duckdb output: %w", err)
	}
	if len(raw) == 0 {
		return &engine.Rows{}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	columns, err := firstObjectKeys(dec)
	if err != nil {
		return nil, err
	}

	rows := make([][]any, 0, len(raw))
	for _, obj := range raw {
		row := make([]any, len(columns))
		for i, c := range columns {
			var v any
			if rawVal, ok := obj[c]; ok {
				if err := json.Unmarshal(rawVal, &v); err != nil {
					return nil, fmt.Errorf("parsing column %q: %w", c, err)
				}
			}
			row[i] = v
		}
		rows = append(rows, row)
	}
	return &engine.Rows{Columns: columns, Rows: rows}, nil
}

// firstObjectKeys reads key order from the first object in the array, because
// unmarshalling into a map loses it and the result columns must line up with
// the select list.
func firstObjectKeys(dec *json.Decoder) ([]string, error) {
	if _, err := dec.Token(); err != nil { // opening [
		return nil, err
	}
	if _, err := dec.Token(); err != nil { // opening {
		return nil, err
	}
	var keys []string
	depth := 0
	for dec.More() || depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				depth++
			case '}', ']':
				if depth == 0 {
					return keys, nil
				}
				depth--
			}
		case string:
			if depth == 0 {
				keys = append(keys, t)
				// skip the value
				var discard any
				if err := dec.Decode(&discard); err != nil {
					return nil, err
				}
			}
		}
	}
	return keys, nil
}

// inline substitutes bind parameters into the statement.
//
// Placeholders are the single `?` the DuckDB dialect emits, counted outside
// string literals so a `?` inside a model's own text is never mistaken for one.
func inline(sql string, params []any) (string, error) {
	if len(params) == 0 {
		return sql, nil
	}
	var b strings.Builder
	b.Grow(len(sql) + 32*len(params))

	next, inString := 0, false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '\'':
			inString = !inString
			b.WriteByte(c)
		case c == '?' && !inString:
			if next >= len(params) {
				return "", fmt.Errorf("statement has more placeholders than the %d parameters supplied", len(params))
			}
			lit, err := bindLiteral(params[next])
			if err != nil {
				return "", fmt.Errorf("parameter %d: %w", next+1, err)
			}
			b.WriteString(lit)
			next++
		default:
			b.WriteByte(c)
		}
	}
	if next != len(params) {
		return "", fmt.Errorf("statement has %d placeholders but %d parameters were supplied", next, len(params))
	}
	return b.String(), nil
}

// bindLiteral renders one validated value as a SQL literal.
//
// It is total over the five types the planner produces and returns an error for
// everything else, including any type a future change might introduce. The
// default branch is the important one: a new value type must fail loudly here
// rather than fall through to a fmt.Sprint that would put arbitrary text into
// the statement.
func bindLiteral(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'", nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	case plan.Date:
		// A DATE literal, not a timestamp at midnight. DuckDB would accept
		// either, but the two are different types and the model said which.
		return "DATE '" + x.String() + "'", nil
	case time.Time:
		return "TIMESTAMP '" + x.UTC().Format("2006-01-02 15:04:05.999999") + "'", nil
	}
	return "", fmt.Errorf(
		"refusing to render a %T as SQL; only values validated by the planner may be bound", v)
}

// Describe reports the columns DuckDB actually has for one table.
//
// Read through the information schema rather than DESCRIBE, because DESCRIBE
// on a table that does not exist is an error and a missing table is a finding
// rather than a failure to look.
func (d *DuckDB) Describe(ctx context.Context, source string) (map[string]string, bool, error) {
	schema, table := "main", strings.Trim(source, `"`)
	if parts := strings.SplitN(table, ".", 2); len(parts) == 2 {
		schema, table = parts[0], parts[1]
	}

	// Parameterised the same way every other statement here is: the source
	// comes from a model file, which is not a trust boundary, but building SQL
	// by concatenation is how the one that is eventually gets through.
	const q = `SELECT column_name, data_type FROM information_schema.columns
	           WHERE table_schema = ? AND table_name = ?`
	rows, err := d.Execute(ctx, govern.Identity{}, q, []any{schema, table})
	if err != nil {
		return nil, false, err
	}

	out := make(map[string]string, len(rows.Rows))
	for _, row := range rows.Rows {
		if len(row) < 2 {
			continue
		}
		name, _ := row[0].(string)
		kind, _ := row[1].(string)
		if name != "" {
			out[strings.ToLower(name)] = kind
		}
	}
	// No columns means no table. DuckDB has no empty tables in the catalogue.
	return out, len(out) > 0, nil
}
