package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// Picking up a model change without restarting.
//
// Until now a model edit meant restarting the process, which on anything
// holding state is wrong and on a rolling deploy is merely awkward. What made
// it awkward is that this is the normal case: a ConfigMap remount, a git-sync
// sidecar and a mounted volume all change the files under a running process
// and none of them can restart it.
//
// Polling rather than filesystem notifications, and polling the digest rather
// than modification times. A ConfigMap remount rewrites every file with a new
// timestamp and identical content, so mtimes would reload constantly; the
// digest is over the content, so it changes when the meaning does. Watching
// with inotify would also mean a dependency, and a directory of YAML is small
// enough that reading it every thirty seconds costs nothing.
//
// There is deliberately no reload endpoint. One would need its own
// authorization, and anything that can write the model files can already
// trigger this by writing them.

// watchModel reloads the served model when the workspace digest changes.
//
// It never replaces a working model with a broken one: a workspace that fails
// to load leaves the previous one answering and is reported. That is the whole
// safety property, because the alternative is a typo taking a layer down that
// every dashboard in the company reads from.
func watchModel(
	ctx context.Context,
	srv *rest.Server,
	lg *slog.Logger,
	interval time.Duration,
	load func() (*engine.Engine, func(), error),
	current string,
	trigger <-chan struct{},
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// The close for the engine currently serving. Held rather than called,
	// because the model it belongs to is answering requests.
	var closeCurrent func()
	defer func() {
		if closeCurrent != nil {
			closeCurrent()
		}
	}()

	// Reported once rather than every failed attempt. A model broken for an
	// hour should not write a hundred and twenty identical lines.
	var lastFailure string

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-trigger:
			// A deploy asked for this rather than the timer. Identical from
			// here on: the point of the endpoint is to skip the wait, not to
			// take a different path, because a path only pipelines exercise is
			// a path only pipelines find the bugs in.
		}

		next, closeNext, err := load()
		if err != nil {
			if msg := err.Error(); msg != lastFailure {
				lastFailure = msg
				lg.Warn("the model on disk will not load, so the previous one is still being served",
					slog.String("error", msg))
			}
			continue
		}
		lastFailure = ""

		if next.ModelVersion() == current {
			// Nothing changed. Close the engine just built rather than leak it.
			closeNext()
			continue
		}

		previous := current
		current = next.ModelVersion()
		srv.Serve(next)

		// Closed only after the swap, and only the one no longer serving.
		if closeCurrent != nil {
			closeCurrent()
		}
		closeCurrent = closeNext

		lg.Info("model reloaded",
			slog.String("from", previous),
			slog.String("to", current))
	}
}
