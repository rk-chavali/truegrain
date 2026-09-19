package rest

import (
	"log/slog"
	"net/http"
	"sync"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Telling the engine to look at its repository again.
//
// The engine already follows git on a timer, so this endpoint is not how a
// model arrives. It is how a pipeline stops waiting for the next tick: CI
// merges, calls this, and the model is live in a second rather than in a
// minute.
//
// It carries no model. That is the whole design, not an omission. A route that
// accepted a model would be a second way into production, one that skips the
// pull request, the checks and the `diff` that says which numbers move. Git
// stays the only source of truth, and this only says "look now", so the worst
// a stolen deploy credential can do is make the engine read the repository it
// was already reading.
//
// The reply says what the engine went and looked at, not what it found. A
// sync is not instant and reporting a commit before the swap happened would be
// a lie a pipeline would then assert as fact.

// Reloader asks the engine to re-read its model source.
//
// Returns an error only when the request could not be made at all. A model
// that fails to load is not this function's failure: the watcher keeps serving
// the previous one and reports it, which is the behaviour a deploy endpoint
// must not quietly change.
type Reloader func() error

// WithReload makes POST /v1/reload available.
//
// Nil, which is the default, leaves the route answering 404. An engine reading
// a model from a path has nothing to re-read, and an endpoint that accepted
// the call and did nothing would let a pipeline believe it had deployed.
func (s *Server) WithReload(fn Reloader) *Server {
	s.reload = fn
	return s
}

// deploying guards against a pile-up.
//
// Ten pipelines merging at once should produce one fetch, not ten. The
// engine's own trigger channel coalesces, but answering "accepted" ten times
// would tell ten callers they each caused a sync.
type deploying struct {
	mu sync.Mutex
	on bool
}

func (d *deploying) begin() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.on {
		return false
	}
	d.on = true
	return true
}

func (d *deploying) done() {
	d.mu.Lock()
	d.on = false
	d.mu.Unlock()
}

// reloadNow handles POST /v1/reload.
func (s *Server) reloadNow(w http.ResponseWriter, r *http.Request) {
	// 404 rather than 403 when there is nothing to reload, matching the audit
	// endpoint: the capability is absent, not withheld, and "forbidden" would
	// imply there is something here to be let into.
	if s.reload == nil {
		writeError(w, http.StatusNotFound, "reload_not_served",
			"this engine does not follow a repository, so there is nothing to re-read",
			"start it with -models git+https://... to have it follow one")
		return
	}

	id, ok := s.requireScope(w, r, ScopeDeploy)
	if !ok {
		return
	}

	if !s.deploys.begin() {
		// 202 rather than 429. A caller asking for a sync while one is running
		// is getting what they asked for, and telling a pipeline it was rate
		// limited would have it retry something that is already happening.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": "already running",
			"note":   "a sync was already in progress, so this request joined it",
		})
		return
	}
	defer s.deploys.done()

	err := s.reload()
	if err != nil {
		// The repository could not be reached or read. The previous model is
		// still serving, which is the thing a caller most needs to know: this
		// is a failed deploy, not a broken engine.
		writeError(w, http.StatusBadGateway, "reload_failed", err.Error(),
			"the previously loaded model is still being served")
		s.noteDeploy(id, "failed", err)
		return
	}

	s.noteDeploy(id, "accepted", nil)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "reading",
		"note": "the engine is re-reading its model source. Poll /v1/health and " +
			"compare origin.commit to know when the new model is the one answering.",
	})
}

// noteDeploy records who asked for a deploy.
//
// The request log, not the audit log, because the audit log records decisions
// about *queries* and this is a decision about the engine. That split is a
// known gap rather than a design: "who deployed" and "who was granted access"
// both belong in the same trail an auditor reads, and today neither is there.
// Until that is fixed this is the record, and it names the identity so the
// answer exists somewhere.
func (s *Server) noteDeploy(id govern.Identity, outcome string, err error) {
	if s.log == nil {
		return
	}
	fields := []any{
		slog.String("identity", id.Subject),
		slog.String("outcome", outcome),
	}
	if err != nil {
		fields = append(fields, slog.String("error", err.Error()))
	}
	s.log.Info("a deploy was requested", fields...)
}
