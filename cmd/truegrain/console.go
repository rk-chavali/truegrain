package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rk-chavali/truegrain/internal/console"
	"github.com/rk-chavali/truegrain/internal/control"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"

	servecontrol "github.com/rk-chavali/truegrain/internal/serve/control"
)

// `truegrain serve console`: the control plane, the API and the UI, in one
// process on one port.
//
// This is the command that turns the engine from a thing engineers curl
// into a thing a team signs in to. It serves three things that used to
// need three deployments:
//
//	/            the console, embedded in the binary
//	/api/        accounts, sessions, people, warehouse connections
//	/v1/         the engine's REST API, unchanged
//
// One port matters more than it sounds. The oauth2-proxy recipe and every
// split deployment carry the same warning: the engine must not be
// reachable except through the thing that authenticates. With one process
// there is no second address to forget to firewall.
//
// The session cookie is what ties it together. A browser signs in here,
// and /v1/ resolves that cookie to a govern.Identity, so the governance
// gate that has always enforced column and row policy now enforces it
// against a person instead of against a shared token.

func cmdServeConsole(args []string) error {
	flags := flag.NewFlagSet("serve console", flag.ExitOnError)
	common := addCommon(flags)
	addr := flags.String("addr", "127.0.0.1:8080", "address to listen on")
	controlDSNEnv := flags.String("control-dsn-env", "TRUEGRAIN_CONTROL_DSN",
		"environment variable holding the control plane's own PostgreSQL connection"+
			" string. Its own database, never the warehouse: one holds who may log"+
			" in and the other holds somebody's business data")
	certFile := flags.String("tls-cert", "", "PEM certificate for this listener")
	keyFile := flags.String("tls-key", "", "PEM private key for this listener")
	behindProxy := flags.Bool("behind-proxy", false,
		"trust X-Forwarded-Proto and X-Forwarded-For. Off by default because a"+
			" client can send both, and trusting them unconditionally lets a"+
			" plaintext request claim to be secure and turns off the rate limiter")
	doctorEvery := flags.Duration("doctor-every", 0,
		"check the warehouse against the model this often, for example 30m;"+
			" zero never checks. Drift is found by looking regularly")
	testPath := flags.String("tests", "",
		"file or directory of model assertions, served at POST /v1/tests;"+
			" empty does not serve them at all")
	insecure := flags.Bool("insecure", false,
		"serve without TLS. Session cookies and passwords cross the network in"+
			" the clear, so this is for a laptop and a demo")
	obs := addObserve(flags)
	if err := flags.Parse(args); err != nil {
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

	// A console asks for a password over this connection. Serving that in
	// the clear is a decision somebody has to make on purpose.
	if *certFile == "" && *keyFile == "" && !*insecure && !isLoopback(*addr) {
		return fmt.Errorf(
			"refusing to serve the console on %s without TLS.\n"+
				"People type passwords into this and the session cookie is a\n"+
				"credential, so both would cross the network in the clear.\n"+
				"Pass -tls-cert and -tls-key, put it behind a proxy that\n"+
				"terminates TLS and add -behind-proxy, or pass -insecure if you\n"+
				"genuinely mean it", *addr)
	}
	if (*certFile == "") != (*keyFile == "") {
		return errors.New("-tls-cert and -tls-key go together")
	}

	// The encryption key, before anything else, because a control plane
	// that starts without one would either store warehouse credentials in
	// the clear or refuse them later, and the first is silent.
	cipher, err := control.CipherFromEnv()
	if err != nil {
		return err
	}

	dsn := os.Getenv(*controlDSNEnv)
	if dsn == "" {
		return fmt.Errorf(
			"-control-dsn-env names %s, which is not set. The control plane keeps\n"+
				"accounts in its own PostgreSQL database; point it at one", *controlDSNEnv)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := control.Open(ctx, dsn, cipher)
	if err != nil {
		return err
	}
	defer store.Close()

	// The engine is optional at startup. An operator brings the stack up,
	// signs in, and connects a warehouse before there is a model to serve,
	// and a control plane that refused to start without one would make
	// that impossible.
	var engineHandler http.Handler
	ex, closeExec, err := common.executor(ctx)
	if err != nil {
		return err
	}
	defer closeExec()

	/*
		Decisions are remembered, and owners and admins may read them.

		`serve rest` takes -audit-readers because it has no idea who its
		callers are: an operator names them by subject or group because
		nothing else can. The console does know. Every caller signed in,
		and the account carries a role, so the answer is already in the
		database and a flag would only be a second place to get it wrong.

		Admin and above, because the log records what everybody else
		asked. A member reading it would learn which questions their
		colleagues put to the warehouse, which is not theirs to know;
		they get 403 rather than 404, because the capability exists here
		and is being withheld from them specifically.
	*/
	recent := govern.NewRecentAudit(govern.DefaultRecentAudit)
	readers := rest.NewAuditReaders([]string{
		"group:" + servecontrol.RoleGroupPrefix + string(control.RoleOwner),
		"group:" + servecontrol.RoleGroupPrefix + string(control.RoleAdmin),
	})

	eng, closeAudit, err := common.engine(ex, false, recent)
	if err != nil {
		lg.Warn("no model is loaded, so the console will serve accounts and "+
			"connections but no data", slog.String("reason", err.Error()))
	} else {
		defer closeAudit()
		restServer := rest.New(eng, servecontrol.Authenticator{Store: store}).
			WithAuditLog(recent, readers).
			WithModelTests(*testPath).
			WithDoctorSchedule(ctx, *doctorEvery, lg)
		engineHandler = restServer.Handler()
	}

	var ui iofs.FS
	if console.Built() {
		ui = console.FS()
	} else {
		// Still served: the placeholder page explains itself, which is
		// better than a 404 that looks like a routing bug.
		ui = console.FS()
		lg.Warn("this binary has no console built into it; the API works and " +
			"the page will say so. Run `make console`, or use a release image")
	}

	srv := servecontrol.New(servecontrol.Options{
		Store:       store,
		Logger:      lg,
		Engine:      engineHandler,
		Console:     ui,
		BehindProxy: *behindProxy,
	})

	// Expired sessions are refused on every lookup, so this is
	// housekeeping rather than security: it keeps the table from growing
	// without bound on an instance that has been up for a year.
	go purgeSessions(ctx, store, lg)

	claimed, err := store.Claimed(ctx)
	if err != nil {
		return err
	}
	scheme := "http"
	if *certFile != "" {
		scheme = "https"
	}
	if !claimed {
		fmt.Fprintf(os.Stderr,
			"\n  This instance has not been set up yet.\n"+
				"  Open %s://%s and create the first account.\n"+
				"  Signup closes as soon as that account exists.\n\n",
			scheme, *addr)
	}

	httpServer := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
		// A browser holds connections open. Without these a slow client
		// can pin a goroutine and a file descriptor indefinitely, which is
		// a denial of service that needs no bandwidth.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	lg.Info("serving the console",
		slog.String("addr", *addr),
		slog.Bool("tls", *certFile != ""),
		slog.Bool("claimed", claimed),
		slog.Bool("console_embedded", console.Built()),
		slog.Bool("engine_loaded", engineHandler != nil))

	if *certFile != "" {
		err = httpServer.ListenAndServeTLS(*certFile, *keyFile)
	} else {
		err = httpServer.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// purgeSessions deletes rows both session clocks have passed.
func purgeSessions(ctx context.Context, store *control.Store, lg *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := store.PurgeExpiredSessions(ctx)
			if err != nil {
				lg.Warn("purging sessions failed", slog.String("error", err.Error()))
				continue
			}
			if n > 0 {
				lg.Info("purged expired sessions", slog.Int64("count", n))
			}
		}
	}
}
