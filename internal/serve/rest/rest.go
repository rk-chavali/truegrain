// Package rest exposes the engine over HTTP and JSON.
//
// The request body for /v1/query mirrors the MCP query tool exactly: one
// schema, two transports. There is no endpoint that accepts SQL.
package rest

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/observe"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Authenticator turns a request into the workload identity it runs as.
//
// The engine borrows the surrounding platform's identity rather than inventing
// its own. An implementation validates a bearer token or a signed identity
// token and returns the workload it represents; it must return an error for
// anything it cannot verify, because an unauthenticated request that resolves
// to an empty identity would be indistinguishable from an authenticated one.
type Authenticator interface {
	Authenticate(r *http.Request) (govern.Identity, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(*http.Request) (govern.Identity, error)

// Authenticate calls f.
func (f AuthenticatorFunc) Authenticate(r *http.Request) (govern.Identity, error) {
	return f(r)
}

// StaticTokens authenticates from a fixed bearer token to identity map. It
// exists for local development and single-tenant deployments. Tokens are read
// from the environment or a secrets file by the caller and never from source.
type StaticTokens struct {
	// Tokens maps a bearer token to the workload identity it represents.
	Tokens map[string]govern.Identity
}

// Authenticate resolves the Authorization header.
//
// Comparison is constant time and scans every configured token rather than
// looking one up. A map lookup on a shared secret answers faster for a miss
// than a hit, which over enough requests is a way to learn a token a byte at a
// time. There are never many static tokens, so the scan costs nothing.
func (s StaticTokens) Authenticate(r *http.Request) (govern.Identity, error) {
	token, err := bearerToken(r)
	if err != nil {
		return govern.Identity{}, err
	}
	var found govern.Identity
	var matched bool
	for candidate, id := range s.Tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			found, matched = id, true
		}
	}
	if !matched {
		// The message never distinguishes an unknown token from a malformed
		// one, which would let a caller probe for valid tokens.
		return govern.Identity{}, errors.New("unrecognised credentials")
	}
	return found, nil
}

// AnonymousAuth resolves every request to one fixed identity. It is correct for
// a local quickstart against fixture data and wrong for anything else, so the
// engine's health endpoint reports the governance configuration alongside it.
type AnonymousAuth struct{ Identity govern.Identity }

// Authenticate returns the fixed identity.
func (a AnonymousAuth) Authenticate(*http.Request) (govern.Identity, error) {
	return a.Identity, nil
}

// Server serves the REST API.
type Server struct {
	// eng is swapped rather than mutated, so a model reload never has to stop
	// serving. A request reads the pointer once and keeps the engine it got,
	// which means an in-flight query finishes against the model it started
	// with rather than half of each.
	eng  atomic.Pointer[engine.Engine]
	auth Authenticator
	jobs JobStore

	// cors allows a browser client served from another origin to read
	// responses. Off unless origins are named at startup.
	cors *CORS

	// log is nil until an operator asks for logging, and a nil logger here
	// means the handler chain is not wrapped at all rather than wrapped around
	// something that discards.
	log *slog.Logger

	// running holds the cancel function for each job this process is running.
	//
	// Kept here rather than in the store because a context.CancelFunc is a
	// pointer in this process's memory and cannot be written to a row. A
	// shared store makes a job visible everywhere; stopping the query still
	// only works on the replica running it, which is documented on JobStore
	// and reported by health.
	running sync.Map // job id -> context.CancelFunc

	// recentAudit holds the decisions this engine can show an operator, and
	// auditReaders names who may see them. Both are nil until configured, and
	// the endpoint answers 404 until they are.
	recentAudit  *govern.RecentAudit
	auditReaders *AuditReaders

	// testPath is the assertions this engine serves, empty until an
	// operator names one. See WithModelTests in checks.go.
	testPath string

	// previous is the model served before the one serving now, kept so
	// that /v1/diff can answer whether a reload moved a number. Nil until
	// the first reload, which is a different answer from "nothing changed"
	// and is reported as one.
	previous atomic.Pointer[engine.Engine]

	// reload asks the engine to re-read its model source, nil unless it
	// follows one. See deploy.go.
	reload  Reloader
	deploys deploying

	// doctorRuns is what the scheduled warehouse check has seen, nil until
	// an interval is configured. See schedule.go.
	doctorRuns *doctorHistory
}

// WithAuditLog serves recorded decisions to the named readers.
//
// Two arguments rather than one because they are two decisions. The buffer is
// what the engine remembers; the allowlist is who may read it. Neither implies
// the other, and an engine that keeps a buffer for its own health reporting
// should not publish it by having done so.
func (s *Server) WithAuditLog(recent *govern.RecentAudit, readers *AuditReaders) *Server {
	s.recentAudit = recent
	s.auditReaders = readers
	return s
}

// New builds a REST server.
func New(eng *engine.Engine, auth Authenticator) *Server {
	s := &Server{auth: auth, jobs: NewMemoryJobStore()}
	s.eng.Store(eng)
	return s
}

// engine returns the model currently being served.
//
// Read once per request and held, so a reload halfway through a request
// cannot change the model under it.
func (s *Server) engine() *engine.Engine { return s.eng.Load() }

// Serve swaps in a new model.
//
// The caller has already loaded and validated it: this only makes it the one
// answering. Nothing in flight is disturbed, because every request is holding
// the pointer it read when it started.
func (s *Server) Serve(eng *engine.Engine) {
	if old := s.eng.Swap(eng); old != nil {
		s.previous.Store(old)
	}
}

// WithLogging records the completion of every request and gives each one an id
// the caller is told, so a report about one request can be found later.
func (s *Server) WithLogging(lg *slog.Logger) *Server {
	s.log = lg
	return s
}

// WithCORS allows browser clients from the named origins.
//
// Needed when a console is served from somewhere other than this engine,
// which is the normal arrangement for a static build an operator deploys
// themselves.
func (s *Server) WithCORS(c *CORS) *Server {
	s.cors = c
	return s
}

// handlers is the single source of truth for what this API serves.
//
// The mux is built from it and api/openapi.yaml is checked against it by
// TestOpenAPIMatchesRoutes, so the published contract cannot drift from the
// running server. A generated client is only as good as that guarantee.
func (s *Server) handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/health":         s.health,
		"GET /v1/model/version":  s.modelVersion,
		"GET /v1/namespaces":     s.listNamespaces,
		"GET /v1/metrics":        s.listMetrics,
		"GET /v1/metrics/{name}": s.describeMetric,
		"GET /v1/dimensions":     s.listDimensions,
		"POST /v1/query":         s.query,
		"POST /v1/compile":       s.compile,
		"POST /v1/reload":        s.reloadNow,

		// Recorded decisions, served only to named readers. See audit.go.
		"GET /v1/audit": s.listAudit,

		// Asynchronous execution, for queries that outlive an agent's HTTP
		// timeout. See jobs.go.
		"POST /v1/jobs":        s.submitJob,
		"GET /v1/jobs/{id}":    s.getJob,
		"DELETE /v1/jobs/{id}": s.cancelJob,

		// Asking the running model about itself. See checks.go.
		"GET /v1/doctor":          s.doctor,
		"GET /v1/doctor/history":  s.doctorHistoryHandler,
		"POST /v1/tests":          s.runTests,
		"GET /v1/policy":          s.policy,
		"POST /v1/policy/explain": s.explainPolicy,
		"GET /v1/diff":            s.diff,
	}
}

// Patterns returns every route served, sorted, for the spec parity test.
func (s *Server) Patterns() []string {
	h := s.handlers()
	out := make([]string, 0, len(h))
	for pattern := range h {
		out = append(out, pattern)
	}
	sort.Strings(out)
	return out
}

// Handler returns the routed HTTP handler.
//
// Telemetry is attached per route rather than once around the mux, because the
// route has to come from this table and not from the path the caller sent. A
// path is caller controlled: logging it puts their string in an operator's log,
// and labelling a metric with it hands them the label space of the metrics
// backend. Binding each handler to the pattern that matched it makes the wrong
// thing unavailable rather than merely discouraged.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for pattern, handler := range s.handlers() {
		mux.Handle(pattern, s.instrument(pattern, handler))
	}
	// Instrumented like anything else, but registered from a separate
	// table: see unversioned() for why these are not part of the client
	// contract.
	for pattern, handler := range s.unversioned() {
		mux.Handle(pattern, s.instrument(pattern, handler))
	}
	if s.cors.Enabled() {
		return s.cors.Wrap(mux)
	}
	return mux
}

// instrument wraps one handler with its own route label.
func (s *Server) instrument(pattern string, h http.HandlerFunc) http.Handler {
	route := pattern
	if _, path, ok := strings.Cut(pattern, " "); ok {
		// The method is recorded separately, so the label is the path alone.
		route = path
	}

	traced := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := observe.StartQuery(r.Context(), route)
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(rec, r.WithContext(ctx))
		span.Finished(rec.status)
	})

	if s.log == nil {
		return traced
	}
	return observe.Middleware(s.log, func(*http.Request) string { return route })(traced)
}

// statusWriter remembers the status so a span can carry it.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through the wrapper.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// listNamespaces exposes the workspace: who owns what, which namespaces are
// available, and what each exports. Without it the entire composition model is
// invisible to anything that is not the CLI.
func (s *Server) listNamespaces(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireScope(w, r, ScopeReadModel); !ok {
		return
	}
	h := s.engine().Health()
	out := make([]map[string]any, 0, len(h.Namespaces))
	for _, ns := range h.Namespaces {
		entry := map[string]any{
			"name":         ns.Name,
			"available":    ns.Available,
			"metric_count": ns.Metrics,
		}
		if ns.Digest != "" {
			entry["digest"] = ns.Digest
		}
		if len(ns.Owners) > 0 {
			entry["owners"] = ns.Owners
		}
		// The error is included because an operator needs to know why a
		// namespace is missing, and a caller needs to know the absence is a
		// failure rather than a model that never had those metrics.
		if ns.Error != "" {
			entry["error"] = ns.Error
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace":        h.Workspace,
		"workspace_digest": h.WorkspaceDigest,
		"namespaces":       out,
	})
}

// compile resolves access and returns the SQL without running it.
//
// It is not a way to see SQL you may not run: the governance gate runs exactly
// as it does for a query, so a denied request returns a refusal and no SQL. The
// audit records it as `compiled` rather than `allowed`, which keeps looking and
// running distinguishable in the evidence archive.
func (s *Server) compile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeReadModel)
	if !ok {
		return
	}
	req, ok := s.decodeRequest(w, r)
	if !ok {
		return
	}
	c, err := s.engine().DryRun(r.Context(), id, req)
	if err != nil {
		writeRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"compiled_sql":  c.SQL,
		"columns":       c.Columns,
		"dialect":       c.Dialect,
		"namespace":     c.Namespace,
		"parts":         c.Parts,
		"model_version": c.ModelVersion,
	})
}

// health reports the configuration and its limits. It is deliberately
// unauthenticated: it discloses no model content, and an operator needs to be
// able to read it before holding a credential.
//
// When the caller does present a credential, the identity it resolved to is
// echoed back. Without that, a caller cannot tell which identity the engine
// thinks it is talking to, and every governance decision made about them is
// unattributable from their side. It discloses nothing: it tells a caller only
// who they already are, and an unauthenticated request gets nothing at all.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	raw, err := json.Marshal(s.engine().Health())
	if err == nil {
		_ = json.Unmarshal(raw, &body)
	}
	// The note depends on which store is configured, so it is chosen here
	// rather than being a constant. A deployment that fixed the limitation
	// should not still be told it has it, and one that has not should be.
	body["enforcement_notes"] = append(notes(body), s.jobNote())
	body["job_store"] = s.jobs.Name()
	if id, err := s.auth.Authenticate(r); err == nil && id.Subject != "" {
		caller := map[string]any{"subject": id.Subject}
		if len(id.Groups) > 0 {
			caller["groups"] = id.Groups
		}
		body["caller"] = caller

		// A rotating credential file reports how many tokens are live,
		// because an operator halfway through a rotation wants to watch the
		// count go back to one rather than read the secret mount to check.
		// The path and the count, never a token.
		//
		// Inside the authenticated branch, unlike everything above it. This
		// route answers without a credential on purpose, so that a console
		// can show what a deployment enforces, and a server's filesystem
		// path and the number of credentials it holds are not things to
		// hand to a caller who has proved nothing.
		if rotating, ok := s.auth.(RotatingTokens); ok {
			body["credentials"] = map[string]any{
				"source":       rotating.Credentials.Path(),
				"active_now":   rotating.Credentials.Active(),
				"rotates_live": true,
			}
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) modelVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"model":         s.engine().ModelName(),
		"model_version": s.engine().ModelVersion(),
	})
}

func (s *Server) listMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeReadModel)
	if !ok {
		return
	}
	search := strings.ToLower(r.URL.Query().Get("search"))

	type metricJSON struct {
		Name        string   `json:"name"`
		Namespace   string   `json:"namespace"`
		Description string   `json:"description"`
		Datatype    string   `json:"datatype,omitempty"`
		Synonyms    []string `json:"synonyms,omitempty"`
		Dimensions  []string `json:"dimensions"`
	}
	out := []metricJSON{}
	for _, vm := range s.engine().Metrics() {
		m := vm.Metric
		if search != "" && !strings.Contains(strings.ToLower(m.Name+" "+m.Description), search) {
			continue
		}
		dims, err := s.engine().VisibleDimensions(r.Context(), id, &vm)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "policy_unavailable",
				"access could not be resolved", "")
			return
		}
		names := make([]string, len(dims))
		for i, d := range dims {
			names[i] = d.Qualified
		}
		out = append(out, metricJSON{
			Name: vm.Qualified, Namespace: vm.Namespace.Name,
			Description: m.Description, Datatype: m.Datatype,
			Synonyms: m.AIContext.Synonyms, Dimensions: names,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model": s.engine().ModelName(), "model_version": s.engine().ModelVersion(), "metrics": out,
	})
}

func (s *Server) describeMetric(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeReadModel)
	if !ok {
		return
	}
	vm, candidates, found := s.engine().FindMetric(r.PathValue("name"))
	if !found {
		hint := "GET /v1/metrics lists the available names"
		if len(candidates) > 0 {
			hint = "ambiguous across namespaces; use one of: " + strings.Join(candidates, ", ")
		}
		writeError(w, http.StatusNotFound, plan.CodeUnknownMetric,
			"no metric named "+r.PathValue("name"), hint)
		return
	}
	m := vm.Metric
	dims, err := s.engine().VisibleDimensions(r.Context(), id, &vm)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy_unavailable",
			"access could not be resolved", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        vm.Qualified,
		"namespace":   vm.Namespace.Name,
		"description": m.Description,
		"definition":  m.Expression.ANSI(),
		"datatype":    m.Datatype,
		"synonyms":    m.AIContext.Synonyms,
		"dimensions":  dimensionJSON(dims),
	})
}

func (s *Server) listDimensions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeReadModel)
	if !ok {
		return
	}
	var metric *engine.VisibleMetric
	if name := r.URL.Query().Get("metric"); name != "" {
		vm, candidates, found := s.engine().FindMetric(name)
		if !found {
			hint := "GET /v1/metrics lists the available names"
			if len(candidates) > 0 {
				hint = "ambiguous across namespaces; use one of: " + strings.Join(candidates, ", ")
			}
			writeError(w, http.StatusNotFound, plan.CodeUnknownMetric, "no metric named "+name, hint)
			return
		}
		metric = &vm
	}
	dims, err := s.engine().VisibleDimensions(r.Context(), id, metric)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy_unavailable",
			"access could not be resolved", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dimensions": dimensionJSON(dims)})
}

// maxBodyBytes bounds a request body. A semantic request is small; anything
// larger is a mistake or an attempt to exhaust memory.
const maxBodyBytes = 1 << 20

// decodeRequest reads and validates a semantic request body. Query and compile
// share it so the two can never drift into accepting different shapes.
func (s *Server) decodeRequest(w http.ResponseWriter, r *http.Request) (plan.Request, bool) {
	var req plan.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	// Unknown fields are rejected rather than ignored: a caller who misspells
	// `dimensions` should be told, not silently given an ungrouped total.
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(),
			"the body is metrics, dimensions, filters, grain, limit and order_by; see api/openapi.yaml")
		return req, false
	}
	return req, true
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeRunQuery)
	if !ok {
		return
	}
	req, ok := s.decodeRequest(w, r)
	if !ok {
		return
	}

	if !s.engine().CanExecute() {
		c, err := s.engine().Compile(r.Context(), id, req)
		if err != nil {
			writeRefusal(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"columns": c.Columns, "rows": []any{}, "row_count": 0,
			"compiled_sql": c.SQL, "model_version": c.ModelVersion, "dialect": c.Dialect,
			"note": "this engine is configured to compile but not execute; no rows were fetched",
		})
		return
	}

	res, err := s.engine().Query(r.Context(), id, req)
	if err != nil {
		writeRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// identify authenticates, writing a 401 and returning false on failure.
func (s *Server) identify(w http.ResponseWriter, r *http.Request) (govern.Identity, bool) {
	id, err := s.auth.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="semantic"`)
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error(), "")
		return govern.Identity{}, false
	}
	return id, true
}

func dimensionJSON(ds []engine.VisibleDimension) []map[string]any {
	out := make([]map[string]any, len(ds))
	for i, vd := range ds {
		f := vd.Field
		d := map[string]any{
			"name": vd.Qualified, "namespace": vd.Namespace.Name,
			"datatype": f.Datatype, "is_time": f.IsTime,
		}
		if f.Description != "" {
			d["description"] = f.Description
		}
		if len(f.AIContext.Synonyms) > 0 {
			d["synonyms"] = f.AIContext.Synonyms
		}
		if f.IsTime {
			d["grains"] = plan.Grains()
		}
		out[i] = d
	}
	return out
}

// errorBody is the one error shape every endpoint returns.
type errorBody struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Hint   string `json:"hint,omitempty"`
	// Retry is what an agent branches on: modify the request, repeat it later,
	// or stop. Without it a caller either gives up on a typo or loops forever
	// on a denial.
	Retry plan.Retry `json:"retry"`
}

// writeRefusal maps a structured refusal onto a status code. A governance
// denial is 403 and never 200 with an empty body: a silent empty result trains
// a caller to report a wrong number confidently.
func writeRefusal(w http.ResponseWriter, err error) {
	var r *plan.Refusal
	if !errors.As(err, &r) {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	status := http.StatusBadRequest
	switch r.Code {
	case govern.CodeDenied:
		status = http.StatusForbidden
	case govern.CodePolicyFailure:
		status = http.StatusServiceUnavailable
	case plan.CodeUnknownMetric, plan.CodeUnknownDimension:
		status = http.StatusNotFound
	case plan.CodeFanOut, plan.CodeAmbiguousJoin, plan.CodeNoJoinPath, plan.CodeUnsupported:
		// The request is well formed and the engine refuses to answer it.
		status = http.StatusUnprocessableEntity
	case CodeTooManyJobs, engine.CodeTooManyQueries:
		// Backpressure, not a verdict on the request. 429 is what every HTTP
		// client and service mesh already knows how to back off from, and
		// anything else trains them to retry immediately or to give up.
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, errorBody{Code: r.Code, Reason: r.Reason, Hint: r.Hint, Retry: r.Retry()})
}

func writeError(w http.ResponseWriter, status int, code, reason, hint string) {
	writeJSON(w, status, errorBody{
		Code: code, Reason: reason, Hint: hint, Retry: plan.RetryFor(code),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// A write failure here means the client is gone; there is nowhere to
	// report it and the response is already committed.
	_ = enc.Encode(body)
}

// WithJobStore replaces the in-memory job store with a shared one.
//
// What this buys is a deployment that can run more than one replica: a job
// submitted on one is visible from all of them. What it does not buy is
// cancelling a query running in another process, which no store can do.
// See JobStore.
func (s *Server) WithJobStore(store JobStore) *Server {
	s.jobs = store
	return s
}

// JobStoreName describes the store, for health.
func (s *Server) JobStoreName() string { return s.jobs.Name() }

// JobsAreShared reports whether this deployment can run more than one
// replica without a caller losing track of their job.
func (s *Server) JobsAreShared() bool { return s.jobs.Durable() }

// RotatingTokens authenticates from a bearer token file that reloads when it
// changes, so a credential can be replaced without a restart.
//
// StaticTokens reads the environment once and is right for a single-tenant
// deployment that never rotates. This is the same thing for one that does:
// an operator adds the new token to the file, rolls clients onto it, and
// removes the old one, with both valid in between. That overlap is the whole
// mechanism, and it is the part an environment variable cannot do.
//
// Thin on purpose. The file, the reload and the constant-time scan all live
// in govern.Credentials, next to the revocation list that works the same
// way, because a deployment usually wants both and they should not drift
// apart.
type RotatingTokens struct {
	Credentials *govern.Credentials
}

// Authenticate resolves the Authorization header against the current file.
func (r RotatingTokens) Authenticate(req *http.Request) (govern.Identity, error) {
	token, err := bearerToken(req)
	if err != nil {
		return govern.Identity{}, err
	}
	id, ok := r.Credentials.Lookup(token)
	if !ok {
		// The same message for an unknown token, an expired one and a
		// malformed header. The difference between them is what a prober is
		// looking for.
		return govern.Identity{}, govern.ErrUnrecognised
	}
	return id, nil
}
