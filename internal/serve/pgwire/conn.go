package pgwire

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// conn is one authenticated client.
type conn struct {
	srv      *Server
	net      net.Conn
	be       *pgproto3.Backend
	identity govern.Identity
	user     string
	// portal holds the statement most recently parsed in the extended
	// protocol. One is enough: a client that pipelines several before
	// executing any is doing something this server does not support, and is
	// told so rather than silently given the wrong one.
	portal *preparedStatement
	// application is what the client called itself at startup. It is only
	// ever used to attribute an unrecognised catalogue statement in the log,
	// which is the difference between an operator knowing that Tableau sent
	// something and hearing that "the connection does not work".
	application string
}

type preparedStatement struct {
	name string
	sql  string
}

// startup performs the SSL negotiation, the startup exchange and
// authentication, and returns a connection ready to take statements.
func (s *Server) startup(nc net.Conn) (*conn, error) {
	// A client may open with SSLRequest, then start over on the encrypted
	// socket, so the loop runs at most twice.
	be := pgproto3.NewBackend(nc, nc)

	for attempt := 0; attempt < 2; attempt++ {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return nil, fmt.Errorf("reading the startup message: %w", err)
		}

		switch m := msg.(type) {
		case *pgproto3.SSLRequest:
			if s.tls == nil {
				// Say no rather than fail. A client configured for "prefer"
				// falls back to plaintext, which is what an operator who
				// started this without a certificate asked for.
				if _, err := nc.Write([]byte{'N'}); err != nil {
					return nil, err
				}
				continue
			}
			if _, err := nc.Write([]byte{'S'}); err != nil {
				return nil, err
			}
			tlsConn := tls.Server(nc, s.tls)
			if err := tlsConn.HandshakeContext(context.Background()); err != nil {
				return nil, fmt.Errorf("the TLS handshake failed: %w", err)
			}
			nc = tlsConn
			be = pgproto3.NewBackend(nc, nc)
			continue

		case *pgproto3.GSSEncRequest:
			// Not supported, and saying so lets the client fall back rather
			// than wait for a reply that never comes.
			if _, err := nc.Write([]byte{'N'}); err != nil {
				return nil, err
			}
			continue

		case *pgproto3.CancelRequest:
			// A cancel arrives on its own connection. Queries here are
			// bounded by the statement timeout and by the engine's own
			// per-query timeout, so there is nothing to signal; accepting and
			// closing is the correct no-op rather than an error the client
			// would surface to a user who pressed stop.
			return nil, errors.New("cancel request, nothing to cancel")

		case *pgproto3.StartupMessage:
			c := &conn{srv: s, net: nc, be: be, user: m.Parameters["user"]}
			if err := c.authenticate(); err != nil {
				c.fatal("28000", err.Error())
				return nil, err
			}
			if err := c.ready(m); err != nil {
				return nil, err
			}
			return c, nil

		default:
			return nil, fmt.Errorf("unexpected startup message %T", msg)
		}
	}
	return nil, errors.New("the client renegotiated encryption more than once")
}

// authenticate asks for the token and checks it.
//
// Cleartext rather than SCRAM, because the credential is a bearer token
// rather than a password: there is no verifier to store, and SCRAM over a
// token would add a handshake without adding a secret. That makes TLS the
// thing protecting it, which is why serving without a certificate takes an
// explicit flag.
func (c *conn) authenticate() error {
	if c.srv.auth == nil {
		// No authenticator is a deliberate local-development configuration,
		// the same one `serve rest` allows without -token-env. The identity
		// is anonymous and the governance gate treats it as such.
		c.identity = govern.Identity{Subject: "anonymous"}
		return nil
	}

	c.be.Send(&pgproto3.AuthenticationCleartextPassword{})
	if err := c.be.Flush(); err != nil {
		return err
	}

	msg, err := c.be.Receive()
	if err != nil {
		return fmt.Errorf("reading the password message: %w", err)
	}
	pw, ok := msg.(*pgproto3.PasswordMessage)
	if !ok {
		return fmt.Errorf("expected a password, got %T", msg)
	}

	id, err := c.srv.auth.Authenticate(c.user, pw.Password)
	if err != nil {
		// The client's message is deliberately vague; the log is not. Telling
		// a caller which half was wrong is how a token is enumerated.
		return errors.New("authentication failed")
	}
	c.identity = id
	return nil
}

// ready sends the parameters a client reads at connect and the first
// ReadyForQuery.
func (c *conn) ready(m *pgproto3.StartupMessage) error {
	c.be.Send(&pgproto3.AuthenticationOk{})

	// A client caches these and some refuse to proceed without them.
	// standard_conforming_strings in particular decides how the client
	// escapes a backslash, and a client that guesses wrong sends values this
	// server would read differently than intended.
	for name, value := range settings(ServerVersion) {
		if name == "application_name" {
			value = m.Parameters["application_name"]
			c.application = value
		}
		c.be.Send(&pgproto3.ParameterStatus{Name: name, Value: value})
	}

	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	c.be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: key[:]})
	c.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	return c.be.Flush()
}

// loop reads statements until the client goes away.
func (c *conn) loop(ctx context.Context) {
	for {
		msg, err := c.be.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				c.srv.logger().Debug("pgwire connection ended", "reason", err.Error())
			}
			return
		}

		switch m := msg.(type) {
		case *pgproto3.Query:
			c.simpleQuery(ctx, m.String)
			if err := c.be.Flush(); err != nil {
				return
			}

		// The extended protocol. Every serious driver uses it, so answering
		// only the simple one would mean psql worked and nothing else did.
		case *pgproto3.Parse:
			c.portal = &preparedStatement{name: m.Name, sql: m.Query}
			c.be.Send(&pgproto3.ParseComplete{})

		case *pgproto3.Bind:
			// Parameters are not supported: the grammar takes literals, and a
			// $1 would have to be substituted into the statement before
			// parsing, which is the text-building this design exists to
			// avoid. A driver that binds is told so rather than given a
			// result computed from the wrong values.
			if len(m.Parameters) > 0 {
				c.errorResponse("0A000", "bound parameters are not supported; "+
					"send the values as literals in the statement")
				c.syncToReady()
				continue
			}
			c.be.Send(&pgproto3.BindComplete{})

		case *pgproto3.Describe:
			// Describing before execution would mean planning the query to
			// learn its shape, which costs a compile and a governance check
			// for a message the client mostly ignores. NoData is honest: the
			// shape arrives with the result.
			c.be.Send(&pgproto3.NoData{})

		case *pgproto3.Execute:
			if c.portal == nil {
				c.errorResponse("34000", "no statement has been parsed on this connection")
				c.syncToReady()
				continue
			}
			c.runStatement(ctx, c.portal.sql, false)

		case *pgproto3.Sync:
			c.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := c.be.Flush(); err != nil {
				return
			}

		case *pgproto3.Close:
			c.portal = nil
			c.be.Send(&pgproto3.CloseComplete{})

		case *pgproto3.Flush:
			if err := c.be.Flush(); err != nil {
				return
			}

		case *pgproto3.Terminate:
			return

		default:
			c.errorResponse("0A000", fmt.Sprintf("unsupported protocol message %T", msg))
			c.syncToReady()
		}
	}
}

// simpleQuery runs every statement in one Query message.
//
// A client may send several separated by semicolons. Each produces its own
// result, and one ReadyForQuery closes the batch.
func (c *conn) simpleQuery(ctx context.Context, sql string) {
	for _, stmt := range splitStatements(sql) {
		if !c.runStatement(ctx, stmt, true) {
			break // the first failure ends the batch, as PostgreSQL does
		}
	}
	c.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
}

// runStatement answers one statement. It reports whether to continue a batch.
func (c *conn) runStatement(ctx context.Context, sql string, simple bool) bool {
	eng := c.srv.eng.Load()
	if eng == nil {
		c.errorResponse("57P03", "no model is loaded yet")
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, c.srv.QueryTimeout)
	defer cancel()

	schemas, err := schemaOf(ctx, eng, c.identity)
	if err != nil {
		c.errorResponse("58000", "listing the model failed: "+err.Error())
		return false
	}

	// Catalogue questions first: a client asks several before its first real
	// one, and none of them are semantic.
	if answer, handled := catalogAnswer(sql, schemas, ServerVersion); handled {
		c.sendResult(answer)
		return true
	}

	q, err := parseSelect(sql)
	if err != nil {
		if isCatalogProbe(sql) {
			c.logUnrecognisedCatalogue(sql)
			// A signpost rather than a parser complaint. Somebody who typed a
			// psql metacommand gets an error about joins otherwise, which
			// sends them looking at their model.
			c.errorResponse("0A000",
				"this is a semantic engine, not a PostgreSQL server: it "+
					"synthesises only the catalogue a client needs to connect and "+
					"list fields. Use \\dt, or SELECT table_name, column_name, "+
					"data_type FROM information_schema.columns. Everything else "+
					"about this model is in its own files, or over the REST API "+
					"at /v1/metrics and /v1/dimensions.")
			return false
		}
		c.errorResponse("42601", err.Error())
		return false
	}

	b, err := bindQuery(q, schemas)
	if err != nil {
		c.errorResponse("42703", err.Error())
		return false
	}

	res, err := eng.Query(ctx, c.identity, b.request)
	if err != nil {
		c.sendEngineError(err)
		return false
	}

	c.sendResult(project(res, b))
	return true
}

// project reorders the engine's columns into the order the caller asked for.
//
// The engine returns dimensions then metrics; a caller may have written them
// interleaved. A client reads positionally, so returning the engine's order
// under the caller's headers would put every value in the wrong column, and
// it would look plausible.
func project(res *engine.Result, b *bound) *result {
	index := map[string]int{}
	for i, name := range res.Columns {
		index[strings.ToLower(name)] = i
	}

	positions := make([]int, len(b.sources))
	for i, src := range b.sources {
		if at, ok := index[strings.ToLower(src)]; ok {
			positions[i] = at
			continue
		}
		positions[i] = -1
	}

	out := &result{columns: b.columns, tag: "SELECT"}
	for _, row := range res.Rows {
		projected := make([]any, len(positions))
		for i, at := range positions {
			if at >= 0 && at < len(row) {
				projected[i] = row[at]
			}
		}
		out.rows = append(out.rows, projected)
	}
	out.oids = columnTypes(b.measure, out.rows)
	return out
}

func (c *conn) sendResult(r *result) {
	if len(r.columns) == 0 {
		// SET, BEGIN and friends: a tag and nothing else.
		c.be.Send(&pgproto3.CommandComplete{CommandTag: []byte(r.tag)})
		return
	}

	fields := make([]pgproto3.FieldDescription, len(r.columns))
	for i, name := range r.columns {
		oid := uint32(oidText)
		if i < len(r.oids) {
			oid = r.oids[i]
		}
		fields[i] = pgproto3.FieldDescription{
			Name:         []byte(name),
			DataTypeOID:  oid,
			DataTypeSize: typeLength(oid),
			TypeModifier: -1,
			Format:       0, // text
		}
	}
	c.be.Send(&pgproto3.RowDescription{Fields: fields})

	for _, row := range r.rows {
		values := make([][]byte, len(r.columns))
		for i := range r.columns {
			if i < len(row) {
				values[i] = encodeText(row[i])
			}
		}
		c.be.Send(&pgproto3.DataRow{Values: values})
	}

	tag := r.tag
	if tag == "SELECT" || tag == "SHOW" {
		tag = fmt.Sprintf("SELECT %d", len(r.rows))
	}
	c.be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
}

// sendEngineError turns a refusal into something a client shows a user.
//
// The hint goes in the Hint field, which psql prints and most tools surface,
// because the hint is the actionable half: a fan-out refusal names the
// metrics that would answer the question correctly, and losing that would
// leave somebody staring at a rejection with nothing to do about it.
func (c *conn) sendEngineError(err error) {
	var r *plan.Refusal
	if !errors.As(err, &r) {
		c.errorResponse("58000", err.Error())
		return
	}
	c.be.Send(&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     sqlState(r),
		Message:  r.Reason,
		Hint:     r.Hint,
		// The refusal code travels where a client can read it
		// programmatically, so an integration can branch on the kind of
		// refusal rather than on the sentence.
		Detail: "truegrain refusal: " + r.Code + " (retry: " + string(r.Retry()) + ")",
	})
}

// sqlState maps a refusal onto the SQLSTATE a client will recognise.
//
// Clients branch on these. Returning one code for everything would make a
// governance denial indistinguishable from a typo, and a driver would retry
// the denial.
func sqlState(r *plan.Refusal) string {
	switch r.Retry() {
	case plan.RetryLater:
		return "53000" // insufficient_resources: retrying may work
	case plan.RetryModify:
		return "42601" // syntax_error_or_access_rule_violation: the request is wrong
	}
	switch r.Code {
	case govern.CodeDenied:
		return "42501" // insufficient_privilege
	}
	return "42601"
}

func (c *conn) errorResponse(code, message string) {
	c.be.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: code, Message: message})
}

// fatal reports a failure that ends the connection, before ReadyForQuery has
// ever been sent.
func (c *conn) fatal(code, message string) {
	c.be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: message})
	_ = c.be.Flush()
}

// syncToReady returns the connection to a usable state after an error in the
// extended protocol, where the client waits for ReadyForQuery.
func (c *conn) syncToReady() {
	c.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	_ = c.be.Flush()
}

// splitStatements divides a simple-query batch on semicolons.
//
// Quote-aware, because a semicolon inside a literal is data. Splitting on
// every semicolon would cut 'a;b' in half and change which rows a filter
// matches.
func splitStatements(sql string) []string {
	var out []string
	var current strings.Builder
	inString, inIdent := false, false

	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case inString:
			current.WriteRune(ch)
			if ch == '\'' {
				if i+1 < len(runes) && runes[i+1] == '\'' {
					current.WriteRune(runes[i+1])
					i++
					continue
				}
				inString = false
			}
		case inIdent:
			current.WriteRune(ch)
			if ch == '"' {
				if i+1 < len(runes) && runes[i+1] == '"' {
					current.WriteRune(runes[i+1])
					i++
					continue
				}
				inIdent = false
			}
		case ch == '\'':
			inString = true
			current.WriteRune(ch)
		case ch == '"':
			inIdent = true
			current.WriteRune(ch)
		case ch == ';':
			if s := strings.TrimSpace(current.String()); s != "" {
				out = append(out, s)
			}
			current.Reset()
		default:
			current.WriteRune(ch)
		}
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// logUnrecognisedCatalogue records a catalogue statement this server could
// not answer, with the name the client gave itself.
//
// The synthesised catalogue covers what clients are known to send, and a
// client will eventually send something else: a join, a cast, a CASE, an
// array predicate. When that happens the caller gets a refusal, which is
// correct, and without this the operator gets a bug report saying the tool
// does not work and no way to find out what it asked for.
//
// The statement text is logged in full up to a cap. It is catalogue
// boilerplate a driver generated, not caller data: nothing in this branch
// resolved against the model, and a semantic query never reaches here. The
// cap is there because a generated catalogue query can run to kilobytes.
func (c *conn) logUnrecognisedCatalogue(sql string) {
	const cap = 2000
	if len(sql) > cap {
		sql = sql[:cap] + "... (truncated)"
	}
	c.srv.logger().Warn("unrecognised catalogue statement",
		"application_name", c.application,
		"user", c.user,
		"sql", sql)
}
