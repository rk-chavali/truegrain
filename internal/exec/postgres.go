package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
)

// Postgres executes compiled SQL against a PostgreSQL server.
//
// The dialect has emitted golden-tested Postgres since v0.1.0 and nothing had
// ever run it, which made "four warehouses" a claim about the emitter rather
// than about the product. This closes that for three of four.
//
// Three properties are deliberate.
//
// Parameters are bound, never rendered. The Postgres emitter already writes
// native `$1` placeholders, so values pass through pgx untouched. This is the
// one executor with no literal renderer at all: DuckDB needs `bindLiteral`
// because its CLI cannot bind, and that renderer is the weakest line in this
// package. Nothing equivalent exists here and nothing should be added.
//
// Every query runs in a read-only transaction. The planner only ever emits
// SELECT, so this changes nothing today; it means a future bug in the emitter
// cannot write, rather than being trusted not to. `BEGIN READ ONLY` is enforced
// by the server, not by us.
//
// The credential never appears in this struct as anything but a parsed pool
// config. It never reaches an error either, because pgx redacts it: a parse
// failure renders `password=xxxxx` and a connection failure names only the user
// and database. Nothing here should ever put the DSN into a message by hand and
// undo that, which is what TestAConnectionStringNeverReachesAnError watches.
type Postgres struct {
	pool *pgxpool.Pool
	opts PostgresOptions
	// server is the version string, read once at connect, for Name.
	server string
}

// PostgresOptions configure the executor.
//
// There is no Password field and no DSN literal on the command line. The DSN
// arrives from the environment, because a connection string on a flag lands in
// shell history and in the process list, and one in a committed config file
// lands in git history.
type PostgresOptions struct {
	// DSN is a libpq connection string or URL, read from the environment by the
	// caller. Required.
	DSN string
	// AssumeCallerRole runs each query as the calling identity via
	// `SET LOCAL ROLE`, so the server's own row-level security applies to them
	// rather than to the connecting user.
	//
	// Off by default because it only works where a database role exists per
	// caller. Turning it on when they do not exist refuses every query, which
	// is the right failure but a surprising one to arrive at by default.
	AssumeCallerRole bool
	// RequireCallerRole refuses a caller whose role cannot be assumed rather
	// than falling back to the connecting user. Without it, a caller with no
	// matching role is served with the connection's own privileges, which is
	// the quiet version of no access control at all.
	RequireCallerRole bool
	// Timeout bounds a single query. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// NewPostgres connects and verifies the server is reachable.
//
// Connecting eagerly rather than lazily so a bad DSN fails at startup with one
// clear message, instead of on the first query of the first caller.
func NewPostgres(ctx context.Context, opts PostgresOptions) (*Postgres, error) {
	if strings.TrimSpace(opts.DSN) == "" {
		return nil, errors.New(
			"a PostgreSQL connection string is required; set it in an environment " +
				"variable and name that variable, rather than passing it on the command line")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	config, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("the PostgreSQL connection string could not be parsed: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}

	var server string
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version')").Scan(&server); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}

	return &Postgres{pool: pool, opts: opts, server: server}, nil
}

// Name says what this configuration enforces, and what it does not.
//
// Mirrors the BigQuery executor rather than printing a headline: an operator who
// reads "queries run as the caller" and stops there will believe the server's
// row-level security applies to people it does not apply to.
func (p *Postgres) Name() string {
	name := "postgres (server " + p.server + ")"

	switch {
	case !p.opts.AssumeCallerRole:
		name += "; every query runs as the connecting database user, so the server" +
			" cannot tell callers apart and its own row-level security does not" +
			" distinguish them"
	case p.opts.RequireCallerRole:
		name += "; each query runs as the caller's own database role, and a caller" +
			" with no such role is refused"
	default:
		name += "; each query runs as the caller's own database role where one exists," +
			" while any other caller runs as the connecting user"
	}

	// Postgres bills nobody, so there is no cap to report and no cost to read.
	// Said here because a reader comparing this to the BigQuery line will look
	// for the cap and should not conclude it was forgotten.
	return name + "; no spend cap, because PostgreSQL reports no query cost"
}

// Execute runs one statement in a read-only transaction.
func (p *Postgres) Execute(ctx context.Context, id govern.Identity, sql string, params []any) (*engine.Rows, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	// Nothing here writes, so rollback is the only exit. Committing a read-only
	// transaction would be equivalent and would invite someone to make it
	// writable later.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := p.assumeRole(ctx, tx, id); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, sql, params...)
	if err != nil {
		return nil, p.refuse(err)
	}
	defer rows.Close()

	out := &engine.Rows{}
	for _, field := range rows.FieldDescriptions() {
		out.Columns = append(out.Columns, field.Name)
	}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("postgres: reading a row: %w", err)
		}
		row := make([]any, len(values))
		for i, v := range values {
			row[i] = normalizePostgres(v)
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, p.refuse(err)
	}

	// BytesBilled stays zero: PostgreSQL bills nobody and reports no cost, and
	// engine.Rows documents zero as "not reported" rather than as free.
	return out, nil
}

// assumeRole switches the transaction to the caller's own database role.
//
// `SET LOCAL ROLE` takes an identifier and cannot be parameterized, so this is
// the one place in this file where a value reaches SQL as text. The value is the
// caller's identity, which arrives from a JWT, so it is the one input here an
// attacker controls.
//
// Two defences, in order. The name is validated against a strict character set,
// so anything that is not a plain role name is refused outright rather than
// escaped into something surprising. Then it is quoted with pgx's own
// identifier quoting. Validation alone would be enough and quoting alone would
// be enough; both are here because this is the path where being wrong is an
// authorization bypass rather than a broken query.
//
// LOCAL rather than SESSION: the pool reuses connections, and a SESSION role
// would outlive this query and apply to whichever caller got that connection
// next.
func (p *Postgres) assumeRole(ctx context.Context, tx pgx.Tx, id govern.Identity) error {
	if !p.opts.AssumeCallerRole {
		return nil
	}
	if id.Subject == "" {
		if p.opts.RequireCallerRole {
			return fmt.Errorf(
				"this engine is configured to run each query as the caller's own database role, " +
					"and this request carries no identity to assume")
		}
		return nil
	}
	if err := ValidateRoleName(id.Subject); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{id.Subject}.Sanitize()); err != nil {
		if p.opts.RequireCallerRole {
			// Deliberately does not echo the server's message, which names the
			// role and therefore the caller's identity.
			return fmt.Errorf(
				"this caller's database role could not be assumed, so the query was refused " +
					"rather than run with the connecting user's privileges")
		}
		return nil
	}
	return nil
}

// ValidateRoleName refuses anything that is not a plain PostgreSQL role name.
//
// Exported so the same rule can be asserted directly by a test rather than only
// through a live server. Letters, digits, underscore, and the three characters
// an identity provider actually puts in a subject: dot, hyphen and at-sign.
// Everything else, including a quote, a backslash, a semicolon or whitespace,
// is refused.
func ValidateRoleName(name string) error {
	if name == "" {
		return errors.New("a database role name is required")
	}
	if len(name) > 63 {
		// PostgreSQL truncates at NAMEDATALEN-1 silently, and a truncated role
		// name is a different role than the one that was asked for.
		return fmt.Errorf("%q is longer than PostgreSQL's 63 character limit for a role name, "+
			"and PostgreSQL would silently truncate it to a different role", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '.', r == '-', r == '@':
		default:
			return fmt.Errorf(
				"%q is not usable as a PostgreSQL role name: it may contain only letters, digits, "+
					"underscore, dot, hyphen and at-sign", name)
		}
	}
	return nil
}

// Close releases the pool.
func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

// Describe reports a table's column types, for `truegrain doctor`.
//
// Reads `information_schema.columns`, which is metadata and no rows. Parameters
// are bound, so the source name reaching this from a model file is a value and
// never part of the statement.
func (p *Postgres) Describe(ctx context.Context, source string) (map[string]string, bool, error) {
	schema, table := "public", strings.Trim(source, `"`)
	if name, rest, found := strings.Cut(table, "."); found {
		schema, table = name, rest
	}

	rows, err := p.pool.Query(ctx,
		`SELECT column_name, data_type
		 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2`,
		schema, table)
	if err != nil {
		return nil, false, fmt.Errorf("reading the schema of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var column, dataType string
		if err := rows.Scan(&column, &dataType); err != nil {
			return nil, false, fmt.Errorf("reading the schema of %s.%s: %w", schema, table, err)
		}
		out[strings.ToLower(column)] = strings.ToUpper(dataType)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("reading the schema of %s.%s: %w", schema, table, err)
	}

	// Empty means the table is absent or invisible to this credential, which is
	// reported as "not found" rather than as an error: doctor distinguishes a
	// missing table from an unreadable one by the message it prints, and an
	// error here would make every missing table look like a broken connection.
	return out, len(out) > 0, nil
}

// refuse turns a server error into something an operator can act on.
func (p *Postgres) refuse(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("query exceeded the %s timeout", p.opts.Timeout)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Code and message only. Detail and Hint can quote the offending row,
		// which would put warehouse data into a log line.
		return fmt.Errorf("postgres %s: %s", pgErr.Code, pgErr.Message)
	}
	return fmt.Errorf("postgres: %w", err)
}

// normalizePostgres renders a pgx value as something a JSON response, a CSV and
// a terminal table can all carry.
//
// NUMERIC is the case that matters. On BigQuery the equivalent shipped broken:
// every monetary column came back as `271/2` instead of `135.50`, because
// big.Rat renders a fraction. pgtype.Numeric has the same hazard in a different
// shape, so it is converted explicitly rather than left to a default.
func normalizePostgres(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case pgtype.Numeric:
		return numericString(t)
	case time.Time:
		// RFC 3339 rather than a Unix epoch: a timestamp in a result is read by
		// a human or pasted into a report at least as often as it is parsed.
		return t.UTC().Format(time.RFC3339)
	case [16]byte:
		// UUID. Left as its canonical text, not as an array of numbers.
		return fmt.Sprintf("%x-%x-%x-%x-%x", t[0:4], t[4:6], t[6:8], t[8:10], t[10:16])
	case []byte:
		return string(t)
	default:
		return v
	}
}

// numericString renders a NUMERIC without losing its scale.
func numericString(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}
	if n.NaN {
		return "NaN"
	}
	// Float8Value would round; the whole point of NUMERIC is that it does not.
	value, err := n.Value()
	if err != nil {
		return nil
	}
	if s, ok := value.(string); ok {
		return s
	}
	if n.Int != nil {
		return new(big.Float).SetInt(n.Int).Text('f', -1)
	}
	return value
}

// Transient reports whether retrying the identical statement might succeed.
//
// Deliberately a short allow list rather than a deny list. The dangerous
// mistake is treating a real failure as passing: a permission denial retried
// three times is three audit entries and the same answer, a syntax error
// retried is a bug hidden behind latency, and a statement cancelled for
// exceeding a timeout retried is the timeout again at triple the cost.
// Anything not named here is final.
//
// Retrying a read is safe because the planner only emits SELECT; see
// engine.executeWithRetry for why that matters.
func (p *Postgres) Transient(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context is the caller giving up, not the
	// warehouse faltering. Retrying would ignore them.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// A connection that went away between the pool handing it over and the
	// statement running. The commonest transient failure in practice, and the
	// one a pooled client sees after a warehouse restart or a failover.
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 53 is insufficient resources, class 57 is operator
		// intervention, class 58 is a system error: the server saying it
		// could not serve this now rather than that the request was wrong.
		//
		// 57014 is excluded from class 57 on purpose: query_canceled is
		// usually the statement timeout, and retrying a query that ran too
		// long runs it too long again.
		switch pgErr.Code {
		case "57014":
			return false
		case "40001", "40P01":
			// serialization_failure and deadlock_detected. Neither can happen
			// to a read-only transaction that takes no locks, and both are
			// textbook retryable, so they are named rather than relied on.
			return true
		}
		switch pgErr.Code[:2] {
		case "53", "57", "58", "08":
			return true
		}
		return false
	}
	return false
}
