// Package engine composes the parser, workspace, planner, governance gate,
// emitter and executor into the one path every interface uses.
//
// The composition is the security boundary. MCP, REST and the CLI are handed an
// Engine and nothing else: none of them holds a Dialect or an Executor, so none
// of them can produce SQL without the gate having run first. Governance that
// depends on every caller remembering to call a checker is governance that will
// eventually be forgotten.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rk-chavali/truegrain/internal/dialect"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/observe"
	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// CodeTooManyQueries is the refusal when the concurrency cap is reached. It
// is backpressure, not a verdict on the request, so it retries later.
const CodeTooManyQueries = "too_many_queries"

func init() { plan.RegisterRetry(CodeTooManyQueries, plan.RetryLater) }

// Executor runs compiled SQL against a warehouse.
//
// Identity is passed on every call rather than held on the connection because
// the engine impersonates the caller: queries execute as the calling workload,
// never as a shared admin account. An executor whose warehouse has no identity
// concept ignores it and says so in its Name.
type Executor interface {
	Name() string
	Execute(ctx context.Context, id govern.Identity, sql string, params []any) (*Rows, error)
	Close() error
}

// TransientClassifier is an Executor that can tell a passing failure from a
// real one.
//
// Optional, and the knowledge belongs here rather than in the engine: only
// the executor knows that pgx reports a dropped connection one way and
// BigQuery reports a rate limit another. An executor that does not implement
// this gets no retries, which is the conservative default. Guessing that an
// unclassifiable error was transient would retry a permission denial three
// times and a query that cost money twice.
type TransientClassifier interface {
	// Transient reports whether retrying the identical statement might
	// succeed.
	Transient(error) bool
}

// retryAttempts is how many times a transient failure is retried.
//
// Three total attempts. A warehouse that fails three times in under a second
// is having an outage rather than a blip, and more attempts would turn a
// caller's request into a long wait before the same failure.
const retryAttempts = 3

// retryBackoff is the pause before attempt n, starting at n=1.
const retryBackoff = 150 * time.Millisecond

// Rows is a materialized result set.
type Rows struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// JobID is the warehouse's identifier for the execution, when it has one.
	JobID string `json:"job_id,omitempty"`
	// BytesBilled is what the warehouse says the query cost, when it says.
	// Zero means not reported rather than free, so nothing should present it
	// as a spend of nothing: DuckDB bills nobody and reports nothing, and the
	// two are indistinguishable here on purpose.
	BytesBilled int64 `json:"bytes_billed,omitempty"`
}

// Engine serves one workspace.
type Engine struct {
	// environment is the project file overlay in force. Reported, never
	// acted on. See Config.Environment.
	environment string

	// rollupFreshness caches what is known about each declared rollup's
	// currency. See rollup.go.
	rollupFreshness freshness

	// origin is where this model was read from, carried only so health can
	// report it. Nothing behaves differently because of it.
	origin  Origin
	ws      *workspace.Workspace
	gate    *govern.Gate
	dialect dialect.Dialect
	exec    Executor

	// planners is one planner per available namespace. A namespace is a
	// complete planning universe, including anything it imported, so the
	// planner itself never learns that composition exists.
	planners map[string]*plan.Planner

	// cache holds recent results. Nil means every query reaches the
	// warehouse, which is the default.
	cache *Cache

	// slots caps concurrent warehouse execution. Nil means uncapped.
	//
	// It guards e.exec.Execute, which is the one point every surface funnels
	// through: the synchronous REST query, a job, an MCP tool call and the
	// CLI all reach the warehouse here and nowhere else. Capping it in the
	// REST handler would leave the other three uncapped, and the per-caller
	// job slots do not help: they bound one caller at four running jobs, so a
	// hundred callers still put four hundred queries on the warehouse.
	slots chan struct{}
}

// Config builds an Engine.
type Config struct {
	// ModelPath is a workspace root, a directory of Ossie files, or one file.
	ModelPath string
	// Origin says where this model came from, for health to report. Empty on a
	// model read from a path, which needs no explanation; set when it was
	// followed from a repository, where "which commit is this" is the first
	// question asked about a number somebody disputes.
	Origin Origin
	// Environment is the project file overlay in force, empty when none is.
	//
	// Carried by the engine only so it reaches health and every audit
	// event. Nothing here behaves differently because of it: an
	// environment is a set of configuration values, and an engine that
	// also checked the name would be a second, hidden place for the two to
	// disagree. The value of recording it is that a number can be traced
	// back to the deployment that produced it, which is otherwise
	// guesswork the moment there is more than one.
	Environment string
	// Discover overrides namespace discovery with explicit globs.
	Discover []string
	// Strict fails the load when any namespace fails. Commands where a human is
	// watching set it; a server does not.
	Strict bool
	// Dialect names the target warehouse.
	Dialect string
	// Resolver decides column access. Nil means deny everything, which is the
	// safe default for a misconfigured deployment.
	Resolver govern.PolicyResolver
	// Audit receives every decision. Nil discards.
	Audit govern.AuditSink
	// Executor runs SQL. Nil produces a compile-only engine, which is a
	// complete and useful configuration: `truegrain compile` needs no database.
	Executor Executor
	// CacheTTL bounds how long an access decision is reused.
	CacheTTL time.Duration
	// MaxConcurrentQueries caps how many queries this process runs against the
	// warehouse at once. Zero is unlimited.
	//
	// Off by default because the right number is a property of the warehouse
	// and not of this engine: BigQuery absorbs hundreds, a small Postgres
	// struggles past a couple of dozen, and a cap guessed here would either
	// throttle a deployment that was fine or fail to protect one that was not.
	// The reference deployments set it; a bare binary reports that it is unset
	// rather than pretending to a protection it does not have.
	MaxConcurrentQueries int
	// Cache serves a repeated question from memory instead of the
	// warehouse. Nil disables it, which is the default: a cache is a
	// deliberate trade of freshness for latency and cost, and nothing
	// should make that trade on an operator's behalf.
	Cache *Cache
}

// New loads a workspace and builds an engine over it.
func New(cfg Config) (*Engine, error) {
	ws, err := workspace.Load(cfg.ModelPath, workspace.Options{
		Discover: cfg.Discover,
		Strict:   cfg.Strict,
	})
	if err != nil {
		return nil, err
	}
	return NewFromWorkspace(ws, cfg)
}

// NewFromWorkspace builds an engine over an already loaded workspace, which is
// what a caller needs when the policy resolver has to be built against the same
// workspace the engine will serve.
func NewFromWorkspace(ws *workspace.Workspace, cfg Config) (*Engine, error) {
	d, err := dialect.Get(cfg.Dialect)
	if err != nil {
		return nil, err
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = govern.DefaultCacheTTL
	}
	e := &Engine{
		environment: cfg.Environment,
		origin:      cfg.Origin,
		ws:          ws,
		gate: govern.NewGate(cfg.Resolver, cfg.Audit, govern.Options{
			CacheTTL:    ttl,
			Environment: cfg.Environment,
		}),
		dialect:  d,
		exec:     cfg.Executor,
		planners: map[string]*plan.Planner{},
	}
	for _, ns := range ws.Available() {
		e.planners[strings.ToUpper(ns.Name)] = plan.NewWithRollups(ns.Schema, ns.Rollups)
	}
	if cfg.MaxConcurrentQueries > 0 {
		e.slots = make(chan struct{}, cfg.MaxConcurrentQueries)
	}
	e.cache = cfg.Cache
	if len(e.planners) == 0 {
		return nil, fmt.Errorf("workspace at %s has no usable namespace", ws.Root)
	}
	return e, nil
}

// QueryConcurrency reports the configured cap, or zero when uncapped.
//
// Read by health, because a deployment with no cap should say so rather than
// let an operator assume one exists.
func (e *Engine) QueryConcurrency() int { return cap(e.slots) }

// acquire takes an execution slot, or refuses.
//
// It refuses rather than queues. Queuing would turn saturation into every
// caller timing out at once, and an agent cannot tell a slow answer from a
// stuck one; a refusal carrying RetryLater tells it exactly what happened and
// what to do. It is the same shape as the per-caller job limit, which already
// refuses with too_many_jobs rather than making a caller wait.
func (e *Engine) acquire() error {
	if e.slots == nil {
		return nil
	}
	select {
	case e.slots <- struct{}{}:
		return nil
	default:
		return &plan.Refusal{
			Code: CodeTooManyQueries,
			Reason: fmt.Sprintf(
				"this engine is already running its limit of %d concurrent queries",
				cap(e.slots)),
			Hint: "retry shortly, or submit the query as a job so it is not holding " +
				"a connection open while it waits",
		}
	}
}

func (e *Engine) release() {
	if e.slots != nil {
		<-e.slots
	}
}

// executeWithRetry runs the statement, retrying a failure the executor
// classifies as passing.
//
// Retrying a read is safe here for a reason worth stating rather than
// assuming: the planner only ever emits SELECT, so re-running the identical
// statement cannot double an effect, and the statement is identical because
// it was compiled once before the first attempt. If a write path is ever
// added, this becomes unsafe and has to be reconsidered rather than
// inherited.
//
// A flaky warehouse used to surface as a failed query, which an agent reads
// as "this question cannot be answered" and gives up on. One dropped
// connection should not look like a refusal.
//
// The execution slot stays held across the retries, deliberately: a caller
// whose query is being retried is still occupying the warehouse, and letting
// the slot go would let the cap be exceeded by exactly the callers having
// trouble.
func (e *Engine) executeWithRetry(ctx context.Context, id govern.Identity, c *Compiled) (*Rows, error) {
	classifier, canClassify := e.exec.(TransientClassifier)

	var err error
	for attempt := 1; ; attempt++ {
		var rows *Rows
		rows, err = e.exec.Execute(ctx, id, c.SQL, c.Params)
		if err == nil {
			return rows, nil
		}
		// Not retryable, out of attempts, or an executor that cannot tell:
		// return the warehouse's own error unchanged, because it is the most
		// informative thing anybody will see.
		if !canClassify || attempt >= retryAttempts || !classifier.Transient(err) {
			return nil, err
		}
		// The caller's own deadline wins. Sleeping past it would turn a
		// transient failure into a timeout, which reads as a slow warehouse
		// rather than a flaky one.
		wait := retryBackoff * time.Duration(attempt)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
}

// Workspace exposes the loaded workspace for metadata listings.
func (e *Engine) Workspace() *workspace.Workspace { return e.ws }

// Schema returns the only namespace's schema when there is exactly one, which
// is what single-team callers and the existing tests expect.
func (e *Engine) Schema() *resolve.Schema {
	if ns, ok := e.ws.Single(); ok {
		return ns.Schema
	}
	return nil
}

// ModelName names the workspace: the single namespace when there is one, and
// otherwise a summary.
func (e *Engine) ModelName() string {
	if ns, ok := e.ws.Single(); ok {
		return ns.Name
	}
	names := make([]string, 0, len(e.ws.Available()))
	for _, ns := range e.ws.Available() {
		names = append(names, ns.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ModelVersion is the workspace digest: the hash over every namespace digest.
func (e *Engine) ModelVersion() string { return e.ws.Digest }

// CanExecute reports whether this engine was built with an executor.
func (e *Engine) CanExecute() bool { return e.exec != nil }

// NamespaceHealth reports one namespace's state.
type NamespaceHealth struct {
	Name      string   `json:"name"`
	Available bool     `json:"available"`
	Digest    string   `json:"digest,omitempty"`
	Owners    []string `json:"owners,omitempty"`
	Metrics   int      `json:"metric_count"`
	Error     string   `json:"error,omitempty"`
}

// Health describes what this deployment actually enforces. It is reported
// verbatim by every interface, because a caller deciding whether to trust the
// layer needs the limits and not just the features.
type Health struct {
	// Environment is the project file overlay in force, omitted when none
	// is. First field because it is the one that decides what every other
	// field in this response means.
	Environment string `json:"environment,omitempty"`
	Workspace   string `json:"workspace"`
	// Origin is the repository and commit this model was followed from,
	// omitted when it was read from a path.
	Origin *Origin `json:"origin,omitempty"`
	// Rollups are the declared pre-aggregated tables and what the last
	// freshness probe said about each. Omitted when none is declared.
	Rollups         []RollupHealth       `json:"rollups,omitempty"`
	WorkspaceDigest string               `json:"workspace_digest"`
	SpecVersion     string               `json:"ossie_spec_version"`
	Namespaces      []NamespaceHealth    `json:"namespaces"`
	Dialect         dialect.Capabilities `json:"dialect"`
	Governance      govern.Capabilities  `json:"governance"`
	Executor        string               `json:"executor"`
	Metrics         int                  `json:"metric_count"`
	Dimensions      int                  `json:"dimension_count"`
	SupportedOps    []plan.Op            `json:"supported_filter_ops"`
	SupportedGrains []plan.Grain         `json:"supported_grains"`
	// MaxConcurrentQueries is the cap on simultaneous warehouse execution.
	// Zero means uncapped, which is reported in EnforcementNotes rather than
	// left for an operator to infer from a missing field.
	MaxConcurrentQueries int `json:"max_concurrent_queries"`
	// CacheTTLSeconds is how stale an answer may be. Zero means every
	// query reaches the warehouse.
	CacheTTLSeconds  int      `json:"cache_ttl_seconds"`
	EnforcementNotes []string `json:"enforcement_notes"`
}

// Health reports the engine's configuration and its honest limits.
func (e *Engine) Health() Health {
	h := Health{
		Environment:          e.environment,
		Rollups:              e.Rollups(),
		Workspace:            e.ModelName(),
		WorkspaceDigest:      e.ws.Digest,
		Origin:               e.reportedOrigin(),
		Dialect:              e.dialect.Capabilities(),
		Governance:           e.gate.Capabilities(),
		Executor:             "none (compile only)",
		SupportedOps:         plan.Ops(),
		SupportedGrains:      plan.Grains(),
		MaxConcurrentQueries: e.QueryConcurrency(),
	}
	if e.cache != nil {
		h.CacheTTLSeconds = int(e.cache.TTL().Seconds())
	}
	for _, ns := range e.ws.Namespaces() {
		nh := NamespaceHealth{
			Name:      ns.Name,
			Available: ns.Available(),
			Digest:    ns.Digest,
			Owners:    ns.Owners(),
		}
		if ns.Available() {
			nh.Metrics = len(ns.Model.Metrics)
			h.Metrics += nh.Metrics
			h.Dimensions += len(ns.Schema.Dimensions())
			if h.SpecVersion == "" {
				h.SpecVersion = ns.Model.SpecVersion
			}
		} else if ns.Err != nil {
			nh.Error = ns.Err.Error()
		}
		h.Namespaces = append(h.Namespaces, nh)
	}
	if e.exec != nil {
		h.Executor = e.exec.Name()
	}

	if failed := e.ws.Failed(); len(failed) > 0 {
		names := make([]string, len(failed))
		for i, ns := range failed {
			names[i] = ns.Name
		}
		sort.Strings(names)
		h.EnforcementNotes = append(h.EnforcementNotes, fmt.Sprintf(
			"%d namespace(s) failed to load and are unavailable: %s. Queries touching them are refused.",
			len(failed), strings.Join(names, ", ")))
	}
	if !h.Governance.ColumnLevel {
		h.EnforcementNotes = append(h.EnforcementNotes,
			"No column-level access control is configured: every caller can read every column.")
	}
	if !h.Dialect.ColumnLevelSecurity {
		h.EnforcementNotes = append(h.EnforcementNotes,
			"The target warehouse enforces no column-level security of its own, so the engine's gate is the only control and it applies only to queries made through this engine.")
	}
	// Row restrictions this engine applies are reported separately from row
	// security the warehouse applies, because they cover different callers and
	// an operator reading one as the other would believe more is enforced than
	// is. The count is given and the principals are not: who is restricted is
	// itself worth not publishing.
	if rows := e.gate.RowPolicies(); len(rows) > 0 {
		h.EnforcementNotes = append(h.EnforcementNotes, fmt.Sprintf(
			"Row-level restrictions are applied to %d principal(s) by this engine. "+
				"A caller named by none of them sees every row.",
			rows.Principals()))
	} else if !h.Dialect.RowLevelSecurity {
		h.EnforcementNotes = append(h.EnforcementNotes,
			"The target warehouse enforces no row-level security and this engine applies none, so no row filtering happens beyond the filters in the request.")
	}
	if !h.Dialect.RowLevelSecurity && len(e.gate.RowPolicies()) > 0 {
		h.EnforcementNotes = append(h.EnforcementNotes,
			"The warehouse enforces no row-level security of its own, so these restrictions apply only to queries made through this engine.")
	}
	// Availability rather than access, but the same rule applies: a limit
	// nobody set should be stated, not left to be assumed. Without a cap one
	// caller can put as many queries on the warehouse as it can open
	// connections, and the per-caller job slots do not bound the total.
	// A cached answer is a correct answer from a moment ago, which is a
	// different thing from a correct answer, and somebody reconciling a
	// dashboard against the warehouse needs to know the difference exists.
	if e.cache != nil {
		h.EnforcementNotes = append(h.EnforcementNotes, fmt.Sprintf(
			"Results are cached for up to %s, so an answer may reflect the "+
				"warehouse as it was that long ago. Every cached answer is still "+
				"audited and still went through the governance gate when it was "+
				"first computed.", e.cache.TTL()))
	}
	if e.exec != nil && h.MaxConcurrentQueries == 0 {
		h.EnforcementNotes = append(h.EnforcementNotes,
			"No cap on concurrent queries is configured, so one caller can saturate the warehouse. Set -max-concurrent-queries to bound it.")
	}
	return h
}

// Compiled is a plan that has passed the governance gate and become SQL.
type Compiled struct {
	SQL          string   `json:"compiled_sql"`
	Params       []any    `json:"-"`
	Columns      []string `json:"columns"`
	Dialect      string   `json:"dialect"`
	Namespace    string   `json:"namespace"`
	ModelVersion string   `json:"model_version"`
	// Parts is how many facts were aggregated separately and joined. More than
	// one means the query spanned grains or namespaces.
	Parts int        `json:"parts"`
	Plan  *plan.Plan `json:"-"`
	// Rollups names the pre-aggregated tables this statement read, empty
	// when it read the base fact.
	//
	// On the response, not only in the log. A caller disputing a number is
	// already reading the compiled SQL that comes back with it, and "this
	// came from a pre-aggregated table" is the first thing that changes
	// what they should check.
	Rollups []string `json:"rollups,omitempty"`
}

// Refusal codes this package adds to the planner's.
const (
	CodeCrossNamespace       = "cross_namespace_query"
	CodeNamespaceUnavailable = "namespace_unavailable"
)

// Compile plans a request, resolves access, and emits SQL. It never executes.
//
// The gate runs between planning and emission, over every part. A denied
// request returns before any SQL exists, which is what makes the guarantee in
// docs/05-governance.md true rather than aspirational: there is no compiled
// statement to leak and no warehouse job to cancel.
func (e *Engine) Compile(ctx context.Context, id govern.Identity, req plan.Request) (compiled *Compiled, err error) {
	ctx, span := observe.StartCompile(ctx)
	span.Shape(len(req.Metrics), len(req.Dimensions), len(req.Filters))
	defer func() {
		observe.RecordCompile(ctx, e.dialect.Name(), span.Elapsed())
		span.End()
		// Recorded here rather than in Query, because this is where every
		// caller routes through. Query, DryRun and the job submission path all
		// compile first, and auditing in any one of them leaves the others
		// silent: a refusal through /v1/jobs left no trace until a live engine
		// showed it missing from a record that had the same refusal from
		// /v1/query sitting in it.
		e.auditRefusal(id, req, e.dialect.Name(), err, span.Elapsed())
	}()

	if len(req.Metrics) == 0 {
		return nil, &plan.Refusal{Code: plan.CodeNoMetrics,
			Reason: "a request must name at least one metric",
			Hint:   "call list_metrics to see what is available"}
	}
	limit, err := boundLimit(req.Limit)
	if err != nil {
		return nil, err
	}

	groups, err := e.groupMetrics(req.Metrics)
	if err != nil {
		return nil, err
	}
	dims, err := e.resolveShared(req.Dimensions, "dimension")
	if err != nil {
		return nil, err
	}
	// Row policy filters are added here, before planning, so the planner and
	// the gate both see the narrowed request and no separate path exists that
	// could produce a statement without them. They combine with the caller's
	// own filters by AND, so a caller narrowing a query can never widen it,
	// and there is no request field that names them, so none can be removed.
	filters := e.rowFilters(id, groups, req.Filters)

	parts, err := e.buildParts(ctx, groups, dims, filters, req.Grain)
	if err != nil {
		return nil, err
	}

	combined := &dialect.Combined{Parts: parts, Limit: limit}
	if combined.OrderBy, err = plan.BindOrder(req.OrderBy, combined.Columns()); err != nil {
		return nil, err
	}

	// Every part is checked before any of them is emitted. Checking only the
	// first would let a metric in a later part escape governance entirely,
	// which is the most expensive possible bug in this file.
	//
	// The gate gets its own span: it can call out to a catalogue, so "the gate
	// is slow" and "the planner is slow" are different problems with different
	// answers, and one number for both cannot tell them apart.
	gctx, gspan := observe.StartGovern(ctx)
	for _, part := range parts {
		if err := e.gate.Check(gctx, id, part.Schema, part.Plan); err != nil {
			gspan.End()
			return nil, err
		}
	}
	gspan.End()

	sql, params, err := e.dialect.EmitCombined(combined)
	if err != nil {
		return nil, err
	}
	var rollups []string
	for _, part := range parts {
		if part.Plan.Rollup != nil {
			rollups = append(rollups, part.Plan.Rollup.Name)
		}
	}
	return &Compiled{
		Rollups:      rollups,
		SQL:          sql,
		Params:       params,
		Columns:      combined.Columns(),
		Dialect:      e.dialect.Name(),
		Namespace:    namespacesOf(parts),
		ModelVersion: e.ws.Digest,
		Parts:        len(parts),
		Plan:         parts[0].Plan,
	}, nil
}

// DryRun compiles a request without executing it, and records that the caller
// inspected it.
//
// It is deliberately not a cheaper Compile. Two things make it safe to expose:
// the governance gate runs on exactly the same path, so a caller cannot read
// the SQL for a metric they may not query; and the decision is audited as
// `compiled` rather than `allowed`, so an agent enumerating the model by
// repeatedly compiling leaves a trail that is distinguishable from one that
// actually ran queries.
func (e *Engine) DryRun(ctx context.Context, id govern.Identity, req plan.Request) (*Compiled, error) {
	c, err := e.Compile(ctx, id, req)
	if err != nil {
		return nil, err
	}
	e.gate.Audit(govern.Event{
		Time: time.Now().UTC(), Identity: id.Subject,
		ModelName: c.Namespace, ModelVersion: c.ModelVersion, Namespace: c.Namespace,
		Metrics: req.Metrics, Dimensions: req.Dimensions,
		Decision: "compiled", SQLHash: sqlHash(c.SQL), Dialect: c.Dialect,
	})
	return c, nil
}

// boundLimit applies the default and the cap. It lives here rather than in the
// planner because a multi-fact query limits the joined result, not a part.
func boundLimit(n int) (int, error) {
	switch {
	case n == 0:
		return plan.DefaultLimit, nil
	case n < 0:
		return 0, &plan.Refusal{Code: plan.CodeBadLimit,
			Reason: fmt.Sprintf("limit must be positive, got %d", n)}
	case n > plan.MaxLimit:
		return 0, &plan.Refusal{Code: plan.CodeBadLimit,
			Reason: fmt.Sprintf("limit %d exceeds the maximum of %d", n, plan.MaxLimit),
			Hint:   "narrow the request with filters or a coarser grain"}
	}
	return n, nil
}

// namespacesOf lists the namespaces a compiled query touched, deduplicated.
func namespacesOf(parts []dialect.Part) string {
	seen := map[string]bool{}
	var names []string
	for _, p := range parts {
		if !seen[p.Namespace] {
			seen[p.Namespace] = true
			names = append(names, p.Namespace)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Result is a query response. It always carries the compiled SQL and the model
// version that produced it, which is how a disputed number gets settled.
type Result struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowCount     int      `json:"row_count"`
	CompiledSQL  string   `json:"compiled_sql"`
	Namespace    string   `json:"namespace"`
	ModelVersion string   `json:"model_version"`
	Dialect      string   `json:"dialect"`
}

// Query compiles and executes a request.
func (e *Engine) Query(ctx context.Context, id govern.Identity, req plan.Request) (*Result, error) {
	whole := time.Now()
	dialect := e.dialect.Name()

	c, err := e.Compile(ctx, id, req)
	if err != nil {
		// Compile has already audited this refusal. Recording it again here
		// would double count every fan-out in a governance report.
		observe.RecordQuery(ctx, dialect, outcomeOf(err), plan.CodeOf(err), time.Since(whole), 0)
		return nil, err
	}
	if e.exec == nil {
		err := &plan.Refusal{Code: "no_executor",
			Reason: "this engine has no execution driver, so it can compile but not run queries",
			Hint:   "use compile to see the SQL, or start the engine with an executor configured"}
		observe.RecordQuery(ctx, dialect, observe.OutcomeRefused, err.Code, time.Since(whole), 0)
		e.auditRefusal(id, req, dialect, err, time.Since(whole))
		return nil, err
	}

	// The cache, after the gate and before the warehouse.
	//
	// After the gate is the whole of what makes it safe: by here Compile
	// has checked this caller's column access and folded their row policy
	// into the statement, so a hit is a result they were authorized to see.
	// A cache consulted before the gate would be an access control bypass
	// with a latency benefit.
	if hit, key, ok := e.cached(id, c); ok {
		observe.RecordQuery(ctx, dialect, observe.OutcomeAllowed, "", time.Since(whole), 0)
		// Audited like any other answered query, and marked, because an
		// operator reconciling a number against warehouse billing needs to
		// know which answers never reached the warehouse.
		e.gate.Audit(govern.Event{
			Time: time.Now().UTC(), Identity: id.Subject,
			ModelName: c.Namespace, ModelVersion: c.ModelVersion, Namespace: c.Namespace,
			Metrics: req.Metrics, Dimensions: req.Dimensions,
			SQLHash: sqlHash(c.SQL), Dialect: c.Dialect,
			Decision: "allowed", RowCount: len(hit.Rows),
			DurationMS: time.Since(whole).Milliseconds(),
		})
		_ = key
		return hit, nil
	}

	// Backpressure before the warehouse is touched, and audited like every
	// other refusal: an operator asking why a report came back empty needs to
	// see that the engine was saturated, not an unexplained gap.
	if err := e.acquire(); err != nil {
		refusal := err.(*plan.Refusal)
		observe.RecordQuery(ctx, dialect, observe.OutcomeRefused, refusal.Code, time.Since(whole), 0)
		e.auditRefusal(id, req, dialect, refusal, time.Since(whole))
		return nil, refusal
	}
	defer e.release()

	ectx, espan := observe.StartExecute(ctx, dialect)
	espan.Model(c.Namespace, c.ModelVersion, c.Parts)
	start := time.Now()
	rows, err := e.executeWithRetry(ectx, id, c)
	elapsed := time.Since(start)
	hash := sqlHash(c.SQL)

	base := govern.Event{
		Time: time.Now().UTC(), Identity: id.Subject,
		ModelName: c.Namespace, ModelVersion: c.ModelVersion, Namespace: c.Namespace,
		Metrics: req.Metrics, Dimensions: req.Dimensions, Rollups: c.Rollups,
		SQLHash: hash, Dialect: c.Dialect, DurationMS: elapsed.Milliseconds(),
	}

	if err != nil {
		base.Decision = "error"
		base.Error = err.Error()
		e.gate.Audit(base)
		espan.Failed(err)
		observe.RecordQuery(ctx, dialect, observe.OutcomeError, "", time.Since(whole), 0)
		return nil, fmt.Errorf("executing query: %w", err)
	}

	base.Decision = "allowed"
	base.JobID = rows.JobID
	base.RowCount = len(rows.Rows)
	base.BytesBilled = rows.BytesBilled
	e.gate.Audit(base)

	espan.Succeeded(len(rows.Rows))
	// What the warehouse says this cost. It is the number worth alerting on
	// before anything is visibly wrong, and it is zero for engines that do not
	// report it rather than being guessed at.
	observe.RecordBytesBilled(ctx, dialect, rows.BytesBilled)
	observe.RecordQuery(ctx, dialect, observe.OutcomeAllowed, "", time.Since(whole), len(rows.Rows))

	out := &Result{
		Columns:      rows.Columns,
		Rows:         rows.Rows,
		RowCount:     len(rows.Rows),
		CompiledSQL:  c.SQL,
		Namespace:    c.Namespace,
		ModelVersion: c.ModelVersion,
		Dialect:      c.Dialect,
	}
	// Only a successful answer is stored. A refusal is cheap to recompute
	// and never reaches the warehouse, and caching one would keep refusing
	// a caller whose access was just granted; an error should be retried
	// rather than remembered.
	if e.cache != nil {
		e.cache.put(cacheKey(id.Subject, c.SQL, c.Dialect, c.ModelVersion, c.Params), out)
	}
	return out, nil
}

// auditRefusal records a question this engine declined to answer.
//
// The gate audits denials, which are about access. This audits refusals, which
// are about correctness, and they are kept as separate decisions because they
// mean opposite things: a denial says the caller may not read something, a
// refusal says nobody can be told this accurately and names what can be.
// Reporting the two together would make a governance review count every
// fan-out as an access incident.
//
// The reason and hint are recorded verbatim. That is safe by construction: a
// refusal names metrics, dimensions and relationships, all of which are model
// vocabulary, and never a value from the request.
func (e *Engine) auditRefusal(id govern.Identity, req plan.Request, dialect string, err error, elapsed time.Duration) {
	var refusal *plan.Refusal
	if !errors.As(err, &refusal) {
		// Not a refusal: the engine broke rather than declined, and the caller
		// above records it as an error.
		return
	}
	// A denial is a refusal, and the gate has already written it as `denied`.
	// Recording it again here would report an access denial a second time as a
	// correctness problem, which is worse than a duplicate.
	if govern.IsGateCode(refusal.Code) {
		return
	}

	e.gate.Audit(govern.Event{
		Time:         time.Now().UTC(),
		Identity:     id.Subject,
		ModelVersion: e.ws.Digest,
		Metrics:      req.Metrics,
		Dimensions:   req.Dimensions,
		Decision:     "refused",
		RefusalCode:  refusal.Code,
		Retry:        string(refusal.Retry()),
		Reason:       refusal.Reason,
		Hint:         refusal.Hint,
		Dialect:      dialect,
		DurationMS:   elapsed.Milliseconds(),
	})
}

// rowFilters returns the caller's filters plus every row policy that applies.
//
// A policy is matched per namespace, so a query touching only marketing never
// carries a restriction written for sales. A filter that does not resolve is
// not dropped: buildParts refuses the query, which is the correct direction,
// because silently ignoring a restriction is a data leak wearing the costume
// of a permissive default.
func (e *Engine) rowFilters(id govern.Identity, groups []*factGroup, requested []plan.Filter) []plan.Filter {
	policies := e.gate.RowPolicies()
	if len(policies) == 0 {
		return requested
	}

	// The caller's own first, so a compiled statement reads in the order it
	// was asked for with the restrictions appended.
	out := slices.Clone(requested)
	seen := map[string]bool{}
	for _, g := range groups {
		if g.ns == nil || seen[g.ns.Name] {
			continue
		}
		seen[g.ns.Name] = true
		out = append(out, policies.For(id, g.ns.Name)...)
	}
	return out
}

// outcomeOf separates the engine declining from the engine breaking.
//
// The distinction is the whole point of the metric. A refusal is this system
// working: it found a question it could not answer correctly and said so. An
// error is a warehouse that went away or a bug. Counting them together gives
// an error rate that rises when governance is doing its job, and a team that
// learns to ignore it.
func outcomeOf(err error) string {
	if plan.CodeOf(err) != "" {
		return observe.OutcomeRefused
	}
	return observe.OutcomeError
}

// VisibleMetric pairs a metric with the namespace that owns it.
type VisibleMetric struct {
	Namespace *workspace.Namespace
	Metric    *osi.Metric
	Qualified string
}

// Metrics returns every metric in the workspace, qualified, in namespace order.
func (e *Engine) Metrics() []VisibleMetric {
	var out []VisibleMetric
	for _, ns := range e.ws.Available() {
		for _, m := range ns.Model.Metrics {
			out = append(out, VisibleMetric{
				Namespace: ns, Metric: m, Qualified: workspace.Qualify(ns.Name, m.Name),
			})
		}
	}
	return out
}

// FindMetric resolves a metric name the way a request would.
func (e *Engine) FindMetric(name string) (VisibleMetric, []string, bool) {
	ref, candidates, ok := e.ws.FindMetric(name)
	if !ok {
		return VisibleMetric{}, candidates, false
	}
	return VisibleMetric{Namespace: ref.Namespace, Metric: ref.Metric, Qualified: ref.Qualified}, nil, true
}

// VisibleDimension is a dimension plus the namespace that offers it.
type VisibleDimension struct {
	Namespace *workspace.Namespace
	Field     *osi.Field
	Qualified string
}

// VisibleDimensions returns the dimensions this identity may read. A dimension
// the caller cannot access is not offered at all, so nobody is invited to ask
// for something that will be refused.
//
// When forMetric is set, only dimensions reachable from that metric's namespace
// are returned, because a dimension in another namespace could never be joined
// to it anyway.
func (e *Engine) VisibleDimensions(ctx context.Context, id govern.Identity, forMetric *VisibleMetric) ([]VisibleDimension, error) {
	var out []VisibleDimension
	spaces := e.ws.Available()
	if forMetric != nil {
		spaces = []*workspace.Namespace{forMetric.Namespace}
	}
	for _, ns := range spaces {
		fields := ns.Schema.Dimensions()
		if forMetric != nil {
			fields = ns.Schema.DimensionsFor(forMetric.Metric)
		}
		allowed, err := e.gate.VisibleDimensions(ctx, id, fields)
		if err != nil {
			return nil, err
		}
		for _, f := range allowed {
			out = append(out, VisibleDimension{
				Namespace: ns, Field: f,
				Qualified: workspace.Qualify(ns.Name, f.QualifiedName()),
			})
		}
	}
	return out, nil
}

// sqlHash identifies a compiled statement without disclosing it. The audit log
// records this rather than the SQL text, which would embed the schema.
func sqlHash(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// suggest offers the closest known names so a caller, and especially an agent,
// can correct itself rather than retrying the same mistake.
func suggest(given string, known []string) string {
	if len(known) == 0 {
		return "the workspace declares none"
	}
	lower := strings.ToLower(given)
	var near []string
	for _, k := range known {
		lk := strings.ToLower(k)
		if strings.Contains(lk, lower) || strings.Contains(lower, lastSegment(lk)) {
			near = append(near, k)
		}
	}
	sort.Strings(near)
	if len(near) > 3 {
		near = near[:3]
	}
	if len(near) > 0 {
		return "did you mean " + strings.Join(near, ", ") + "?"
	}
	if len(known) > 10 {
		known = known[:10]
	}
	return "available: " + strings.Join(known, ", ")
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func init() {
	// Both of these need the model or the deployment to change, not the
	// request. A caller can drop the offending dimension, which is why they are
	// modify rather than never: the hint names exactly what to drop.
	plan.RegisterRetry(CodeCrossNamespace, plan.RetryModify)
	plan.RegisterRetry(CodeNamespaceUnavailable, plan.RetryLater)
	plan.RegisterRetry("no_executor", plan.RetryNever)
}

// Origin says where a served model came from.
//
// Reported by health rather than used for anything. The engine behaves
// identically whichever way a model arrived; what changes is an operator's
// ability to answer "is the thing serving the thing we merged", which is
// otherwise a guess involving ssh and a directory listing.
//
// Deliberately not per-object. Every namespace in a workspace comes from one
// source today, and an origin on each metric would be four hundred copies of
// one fact plus the standing implication that they could differ.
type Origin struct {
	// Repository is the clone URL, without credentials. Empty for a path.
	Repository string `json:"repository,omitempty"`
	// Ref is the branch, tag or commit asked for, empty when the remote's
	// default branch was followed.
	Ref string `json:"ref,omitempty"`
	// Commit is the revision actually serving. This is the field worth
	// reading: Ref says what was asked for and can move, Commit says what
	// answered.
	Commit string `json:"commit,omitempty"`
	// Subdir is the directory inside the repository holding the workspace.
	Subdir string `json:"subdirectory,omitempty"`
}

// FromGit reports whether this model was followed from a repository.
func (o Origin) FromGit() bool { return o.Repository != "" }

// Short renders the commit the way a person quotes one.
func (o Origin) Short() string {
	if len(o.Commit) > 7 {
		return o.Commit[:7]
	}
	return o.Commit
}

// reportedOrigin returns the origin only when there is one worth reporting.
//
// A model read from a path has no origin to describe, and an empty object in
// the response would invite a reader to wonder what it means.
func (e *Engine) reportedOrigin() *Origin {
	if !e.origin.FromGit() {
		return nil
	}
	o := e.origin
	return &o
}
