package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/gitsync"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// Deploying without polling.
//
// A team that deploys from a pipeline does not want a timer as well: the
// merge is the event, and a poll on top of it only adds a window in which the
// engine is serving something nobody asked it to serve yet. That team sets
// -reload=0, and for a while doing so silently took the deploy endpoint away
// with the timer, because one condition gated both.
//
// The endpoint follows the model source, never the interval.

// TestAZeroIntervalStillAnswersADeploy.
//
// The mechanical half: the watcher used to build a ticker unconditionally,
// and time.NewTicker panics on zero, so this path could not even run.
func TestAZeroIntervalStillAnswersADeploy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loaded := make(chan struct{}, 4)
	load := func() (*engine.Engine, func(), error) {
		loaded <- struct{}{}
		// Reporting a load failure is the cheapest way to return from here
		// without building a real engine: watchModel treats it as "keep
		// serving the previous model", which is the behaviour under test
		// anyway. What matters is that the trigger woke it at all.
		return nil, nil, errStub
	}

	trigger := make(chan struct{}, 1)
	go watchModel(ctx, &rest.Server{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		0, load, "sha256:before", trigger)

	trigger <- struct{}{}
	select {
	case <-loaded:
	case <-time.After(5 * time.Second):
		t.Fatal("a deploy did not reach the watcher with no timer configured")
	}

	// And nothing fires on its own, because no interval was asked for.
	select {
	case <-loaded:
		t.Error("the watcher re-read the model without being asked, with interval zero")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestTheDeployEndpointFollowsTheSourceNotTheTimer states the rule the wiring
// in main.go applies, so a future edit that reattaches it to -reload has to
// change a test that says why it must not.
func TestTheDeployEndpointFollowsTheSourceNotTheTimer(t *testing.T) {
	for _, c := range []struct {
		models string
		served bool
		why    string
	}{
		{"git+https://github.com/acme/models.git#main:models", true,
			"a git-backed engine has a repository to re-read"},
		{"git+ssh://git@github.com/acme/models.git", true, "ssh is a repository too"},
		{"./testdata/workspace", false, "a path-backed engine has nothing to re-read"},
		{"/etc/truegrain/models", false, "an absolute path is still a path"},
	} {
		got := strings.HasPrefix(c.models, gitsync.Prefix)
		if got != c.served {
			t.Errorf("%s: deploy served = %v, want %v (%s)", c.models, got, c.served, c.why)
		}
	}
}

var errStub = stubError("the model is not built in this test")

type stubError string

func (e stubError) Error() string { return string(e) }
