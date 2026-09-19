package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/pgwire"
)

// `truegrain serve postgres` speaks the PostgreSQL wire protocol, so that a BI
// tool can reach the model.
//
// It is the only surface a business intelligence tool can use. Tableau, Power
// BI, Metabase, Superset and Looker all speak PostgreSQL and none of them
// speak REST or MCP, so before this the governed model was reachable only
// from code.
//
// What arrives is parsed into a semantic request and never forwarded as text.
// See internal/serve/pgwire for why that is a structural property rather than
// an escaping rule.

func cmdServePostgres(args []string) error {
	fs := flag.NewFlagSet("serve postgres", flag.ExitOnError)
	common := addCommon(fs)
	addr := fs.String("addr", "127.0.0.1:5439",
		"address to listen on; 5439 rather than 5432 so this cannot be mistaken"+
			" for a PostgreSQL server on the same host")
	credentials := fs.String("credentials", "", "YAML file of bearer tokens,"+
		" re-read when it changes so a token can be rotated without restarting"+
		" the listener; the same file `serve rest` takes")
	tokenEnv := fs.String("token-env", "",
		"environment variable holding the bearer token a client sends as its"+
			" password; without it the server is unauthenticated")
	certFile := fs.String("tls-cert", "", "PEM certificate for this listener")
	keyFile := fs.String("tls-key", "", "PEM private key for this listener")
	insecure := fs.Bool("insecure", false,
		"serve without TLS. The token travels in the password field, so without"+
			" a certificate it is readable by anything on the network path")
	timeout := fs.Duration("query-timeout", 2*time.Minute, "bound one statement")
	revocations := fs.String("revocations", "",
		"YAML deny list of revoked subjects and token ids, re-read when it"+
			" changes; the same file `serve rest` takes")
	obs := addObserve(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	lg, shutdownTelemetry, err := obs.start(context.Background())
	if err != nil {
		return err
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(flush); err != nil {
			lg.Warn("telemetry did not flush", slog.String("error", err.Error()))
		}
	}()

	// A credentials file asks clients for a token just as -token-env does,
	// so the plaintext guard has to see it too.
	tlsConfig, err := loadTLS(*certFile, *keyFile, *tokenEnv+*credentials, *insecure)
	if err != nil {
		return err
	}

	ex, closeExec, err := common.executor(context.Background())
	if err != nil {
		return err
	}
	defer closeExec()

	eng, closeAudit, err := common.engine(ex, false, nil)
	if err != nil {
		return err
	}
	defer closeAudit()

	auth, err := postgresAuth(*credentials, *tokenEnv, common.id())
	if err != nil {
		return err
	}

	if *revocations != "" {
		// The same list the REST surface uses. A revocation honoured on one
		// surface and not the other is worse than none: an operator would see
		// the credential refused by the API and not know a dashboard still
		// worked with it.
		list, err := govern.LoadRevocations(*revocations)
		if err != nil {
			return err
		}
		if auth == nil {
			return errors.New(
				"-revocations needs -credentials or -token-env: there is nothing to revoke on an " +
					"unauthenticated server, and configuring one would read as " +
					"protection that is not there")
		}
		auth = pgwire.Revoking{Inner: auth, List: list}
		fmt.Fprintf(os.Stderr, "revocation list %s (%d entries)\n",
			*revocations, list.Count())
	}

	srv := pgwire.New(eng, auth).WithLogging(lg)
	srv.QueryTimeout = *timeout
	if tlsConfig != nil {
		srv = srv.WithTLS(tlsConfig)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	describe(os.Stderr, eng, *addr, tlsConfig != nil, auth != nil)
	return srv.ListenAndServe(ctx, *addr)
}

// postgresAuth builds the authenticator, or returns nil for an
// unauthenticated server.
//
// The token is read from a named variable rather than a flag, the same rule
// the rest of the binary follows: a secret on a command line is in shell
// history and in the process list.
func postgresAuth(credentials, tokenEnv string, id govern.Identity) (pgwire.Authenticator, error) {
	if credentials != "" {
		if tokenEnv != "" {
			return nil, errors.New(
				"-credentials and -token-env both configure bearer tokens. Use " +
					"-credentials: it is the one that can be rotated without a restart")
		}
		creds, err := govern.LoadCredentials(credentials)
		if err != nil {
			return nil, err
		}
		return pgwire.RotatingTokenAuth{Credentials: creds}, nil
	}
	if tokenEnv == "" {
		return nil, nil
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil, fmt.Errorf(
			"-token-env names %s, which is not set. Export the token clients will "+
				"send as their password, or drop -token-env to serve without "+
				"authentication", tokenEnv)
	}
	return pgwire.TokenAuth{Tokens: map[string]govern.Identity{token: id}}, nil
}

// loadTLS reads the certificate, and refuses the combination that would put a
// token on the wire in cleartext without the operator having said so.
func loadTLS(certFile, keyFile, tokenEnv string, insecure bool) (*tls.Config, error) {
	switch {
	case certFile != "" && keyFile == "", certFile == "" && keyFile != "":
		return nil, errors.New("-tls-cert and -tls-key go together")

	case certFile == "":
		// Authentication without encryption is the dangerous case, and it is
		// the one that needs saying out loud. A client sends the token in the
		// password field of the startup exchange, so on a plaintext socket it
		// is readable by anything on the path. Without authentication there
		// is no secret to leak, and refusing would block the local quickstart
		// for no gain.
		if tokenEnv != "" && !insecure {
			return nil, errors.New(
				"refusing to ask clients for a token over an unencrypted socket.\n" +
					"The token is sent in the password field of the startup exchange, so " +
					"without TLS it is readable by anything on the network path.\n" +
					"Give -tls-cert and -tls-key, put this behind a TLS-terminating proxy " +
					"and bind it to localhost, or pass -insecure if you accept that")
		}
		return nil, nil
	}

	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the TLS certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		// TLS 1.2 is the floor. Some BI tools still ship older drivers, so
		// requiring 1.3 would lock out the clients this surface exists for,
		// but nothing below 1.2 is defensible.
		MinVersion: tls.VersionTLS12,
	}, nil
}

// describe prints what a client should connect to, because the connection
// string is the first thing anybody needs and working it out from flags is a
// small barrier at exactly the wrong moment.
func describe(w *os.File, eng interface{ ModelName() string }, addr string, secure, authenticated bool) {
	mode := "no TLS"
	if secure {
		mode = "TLS"
	}
	who := "no authentication"
	if authenticated {
		who = "token in the password field"
	}
	fmt.Fprintf(w, "truegrain postgres: model %s on %s (%s, %s)\n",
		eng.ModelName(), addr, mode, who)
	fmt.Fprintf(w, "\n  psql \"host=%s dbname=truegrain\"\n\n", hostOf(addr))
}

func hostOf(addr string) string {
	host, port := addr, ""
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			host, port = addr[:i], addr[i+1:]
			break
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		return host
	}
	return host + " port=" + port
}
