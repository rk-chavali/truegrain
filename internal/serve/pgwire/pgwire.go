// Package pgwire serves the semantic model over the PostgreSQL wire protocol,
// so that a BI tool can reach it.
//
// This is the surface that turns the engine from something an engineer calls
// into something a business uses. Tableau, Power BI, Metabase, Superset and
// Looker all speak PostgreSQL and none of them speak REST or MCP, so without
// this the governed model is unreachable from every tool that actually draws
// the charts somebody argues about.
//
// # The statement never reaches the warehouse
//
// This is the property everything else depends on. A caller's SQL is parsed
// into a plan.Request holding metric and dimension names, each of which must
// resolve against the loaded model or the request is refused. That request
// goes through engine.Query, exactly as a REST or MCP request does, and the
// warehouse SQL is emitted from the validated plan by the dialect layer as
// always. There is no concatenation path from a caller's bytes to a
// statement the warehouse runs, so SQL injection is not mitigated here, it is
// unrepresentable.
//
// The same follows for governance. Because the request goes through the same
// funnel, the compile-time gate, the fan-out refusal, row-level policy, the
// audit record and the concurrency cap all apply unchanged. A wire protocol
// that reached the executor directly would have been a governance bypass
// wearing a familiar port number.
//
// # What a caller may write
//
// A deliberately small grammar, documented in parse.go. It is small for the
// same reason the rest of the product is: a question this cannot express is
// usually a question the semantic model should not answer, and refusing at
// the grammar refuses earlier and with a better message than accepting and
// failing later.
//
// # What this is not
//
// Not a PostgreSQL. It synthesises the handful of catalogue answers clients
// need to connect and list fields, and refuses the rest rather than
// pretending. There is no write path, no transaction with meaning, no
// temporary table and no function call. A client that needs a real database
// should be pointed at one.
package pgwire

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
)

// ServerVersion is what this reports itself as.
//
// A real-looking major version, because clients gate features on it and one
// that parses as ancient turns off the extended protocol in some drivers.
// The suffix in version() says what this actually is, so nothing is
// pretending beyond the number a client needs to parse.
const ServerVersion = "15.0"

// Authenticator turns the credentials from a startup exchange into an
// identity.
//
// Separate from the REST Authenticator, which takes an *http.Request, because
// these are genuinely different exchanges and forcing one interface over both
// would mean faking an HTTP request to authenticate a socket. What matters is
// that both produce a govern.Identity and neither can produce one without a
// credential.
type Authenticator interface {
	// Authenticate is given the startup user and the password the client
	// sent. An error refuses the connection.
	Authenticate(user, password string) (govern.Identity, error)
}

// TokenAuth authenticates a bearer token presented as the PostgreSQL
// password.
//
// The password field is the only place in the protocol a client will put a
// secret, and every BI tool has a box for it, so a token goes there. The user
// field is ignored for authentication and used only for the connection log:
// treating it as part of the credential would invite somebody to think the
// pair is a username and password, and it is not.
type TokenAuth struct {
	Tokens map[string]govern.Identity
}

// Authenticate matches the token in constant time.
//
// Constant time because a naive map lookup leaks nothing but a comparison
// that returns early on the first wrong byte does, and a token is guessable
// one byte at a time if the comparison says how far it got.
func (t TokenAuth) Authenticate(_, password string) (govern.Identity, error) {
	if password == "" {
		return govern.Identity{}, errors.New("a token is required; put it in the password field")
	}
	var found govern.Identity
	var ok bool
	for token, id := range t.Tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(password)) == 1 {
			found, ok = id, true
		}
	}
	if !ok {
		return govern.Identity{}, errors.New("that token is not recognised")
	}
	return found, nil
}

// Server serves the wire protocol for one engine.
type Server struct {
	eng  atomic.Pointer[engine.Engine]
	auth Authenticator
	log  *slog.Logger

	// tls is nil when the operator accepted plaintext explicitly.
	tls *tls.Config

	// QueryTimeout bounds one statement.
	QueryTimeout time.Duration
}

// New builds a server.
//
// The engine is held behind an atomic pointer for the reason the REST server
// holds one: a reload swaps the model without disturbing a query in flight,
// and each connection reads the pointer once per statement.
func New(eng *engine.Engine, auth Authenticator) *Server {
	s := &Server{auth: auth, QueryTimeout: 2 * time.Minute}
	s.eng.Store(eng)
	return s
}

// Serve swaps in a new model. In-flight statements keep the one they started
// with.
func (s *Server) Serve(eng *engine.Engine) { s.eng.Store(eng) }

// WithTLS terminates TLS on this listener.
//
// Strongly preferred, because the token travels in the password field of the
// startup exchange and is therefore on the wire in cleartext without it.
func (s *Server) WithTLS(c *tls.Config) *Server {
	s.tls = c
	return s
}

// WithLogging records connections and refusals.
func (s *Server) WithLogging(lg *slog.Logger) *Server {
	s.log = lg
	return s
}

// ListenAndServe accepts connections until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // a clean shutdown, not a failure
			}
			return fmt.Errorf("accepting a connection: %w", err)
		}
		go s.handle(ctx, conn)
	}
}

// logger is the configured logger, or one that discards.
func (s *Server) logger() *slog.Logger {
	if s.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.log
}

// handle runs one connection from startup to termination.
//
// Every exit path closes the socket. A panic in one connection must not take
// the listener down with it: a malformed message from one client is not a
// reason to disconnect every dashboard.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			s.logger().Error("pgwire connection panicked",
				"remote", conn.RemoteAddr().String(), "panic", r)
		}
	}()

	c, err := s.startup(conn)
	if err != nil {
		// Startup failures are logged without the credential and without the
		// client's own strings, which are attacker controlled.
		s.logger().Warn("pgwire startup refused",
			"remote", conn.RemoteAddr().String(), "reason", err.Error())
		return
	}
	s.logger().Info("pgwire connected",
		"remote", conn.RemoteAddr().String(), "identity", c.identity.Subject)

	c.loop(ctx)
}

// Revoking wraps an authenticator with a deny list.
//
// The same list the REST surface uses, checked the same way. A revocation
// that applied to one surface and not the other would be the worst kind of
// half-measure: an operator would revoke a credential, see it refused on the
// API, and not know it still worked from a dashboard.
type Revoking struct {
	Inner Authenticator
	List  *govern.Revocations
}

// Authenticate verifies the credential, then checks whether it has been
// turned off. Verification first, so an unauthenticated caller cannot probe
// the list by watching which users fail differently.
func (r Revoking) Authenticate(user, password string) (govern.Identity, error) {
	id, err := r.Inner.Authenticate(user, password)
	if err != nil {
		return govern.Identity{}, err
	}
	if _, revoked := r.List.Check(id); revoked {
		return govern.Identity{}, errors.New("this credential has been revoked")
	}
	return id, nil
}

// RotatingTokenAuth authenticates a bearer token from a file that reloads
// when it changes, so a credential can be rotated without restarting the
// listener.
//
// The same credential file the REST surface reads. A token honoured on one
// surface and not the other is the same failure as a revocation applied to
// one and not the other: an operator sees the API accept it and does not
// know a dashboard no longer works, or the reverse.
type RotatingTokenAuth struct {
	Credentials *govern.Credentials
}

// Authenticate matches the password field against the current file.
func (r RotatingTokenAuth) Authenticate(_, password string) (govern.Identity, error) {
	if password == "" {
		return govern.Identity{}, errors.New("a token is required; put it in the password field")
	}
	id, ok := r.Credentials.Lookup(password)
	if !ok {
		return govern.Identity{}, errors.New("that token is not recognised")
	}
	return id, nil
}
