// Package govern resolves column access before a plan becomes SQL.
//
// The principle from docs/05-governance.md: an unauthorized plan never becomes
// SQL. Execution-time enforcement is a second line of defence, not the first.
// If the only protection is the warehouse rejecting the query, then the query
// text, the schema and the shape of the denial have already left the building,
// and an agent retrying in a loop produces a stream of failures instead of one
// clear refusal.
//
// Threat model for this package:
//
//   - Fail open. A resolver that errors must deny. Every path below returns a
//     denial on error, and the Gate never treats an error as permission.
//   - Stale allow. A cached positive decision that outlives a revoked grant is
//     an access control bypass. The cache has a short TTL and never extends it
//     on read.
//   - Schema disclosure through denials. A refusal names the semantic object
//     the caller asked for, never the physical column or the tag, unless the
//     caller already holds metadata access.
//   - Bypass. Nothing here helps if a caller can reach the emitter directly, so
//     the composition in internal/engine routes every query through the Gate
//     and no interface holds a Dialect of its own.
package govern

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// Identity is the workload the query runs as. Grants go to service accounts and
// groups, never to individual humans, because people change teams and per-human
// deprovisioning becomes unmanageable.
type Identity struct {
	// Subject is the workload identity, typically a service account email.
	Subject string `json:"subject"`
	// Groups are the groups that identity belongs to.
	Groups []string `json:"groups,omitempty"`
	// Scopes bound what this credential may do, as opposed to what this
	// identity may read. Empty means unbounded, which is the default and what
	// every deployment had before scopes existed.
	//
	// The distinction matters: groups decide which columns are legible, scopes
	// decide whether this particular token may execute a query at all. A
	// continuous integration job that only validates a model should hold a
	// credential that cannot also read the warehouse, and without scopes the
	// blast radius of leaking that token is every row its identity can see.
	Scopes []string `json:"scopes,omitempty"`
	// TokenID is the credential's own identifier, the `jti` claim when the
	// issuer sets one. Empty for a shared token and for an issuer that does
	// not mint it.
	//
	// It exists so that one leaked credential can be revoked without
	// revoking the workload it belongs to. Without it the only lever is the
	// subject, which takes every one of that service account's tokens down
	// and therefore tends not to get pulled during an incident.
	//
	// Never logged and never audited: it identifies a bearer credential, and
	// a jti in a log is a smaller secret in a place secrets are not kept.
	TokenID string `json:"-"`
}

// Can reports whether this credential is permitted to do something.
//
// An identity with no scopes can do anything, because that is what every
// credential could do before scopes existed and silently revoking access on
// upgrade would be worse than the problem. Naming any scope opts in, and from
// then on the list is exhaustive.
func (i Identity) Can(scope string) bool {
	if len(i.Scopes) == 0 {
		return true
	}
	return slices.Contains(i.Scopes, scope)
}

func (i Identity) String() string {
	if i.Subject == "" {
		return "anonymous"
	}
	return i.Subject
}

// principals returns every name a grant may be keyed by, subject first.
func (i Identity) principals() []string {
	out := make([]string, 0, len(i.Groups)+1)
	if i.Subject != "" {
		out = append(out, i.Subject)
	}
	return append(out, i.Groups...)
}

// Ref identifies one readable thing in the model. The physical column is
// carried for resolvers that must look up warehouse metadata, and is never put
// into a message returned to a caller.
type Ref struct {
	// Origin is the `namespace.dataset` the field was defined in, which is what
	// policy resolves against. It is deliberately not the name the caller used:
	// a workspace may graft a dataset into another namespace under an alias,
	// and resolving against the alias would let `as:` detach a column from the
	// grant written against it.
	Origin string
	Field  string
	// Local is how the caller addressed the field in the namespace they queried,
	// used only for messages.
	Local  string
	Source string
	Column string
}

// Name is the governed semantic name, and the key every resolver matches on.
func (r Ref) Name() string { return r.Origin + "." + r.Field }

// Decision is the outcome of an access check.
type Decision struct {
	Allowed bool
	// Denied lists the refs the identity may not read. Empty when allowed.
	Denied []Ref
}

// Capabilities declares honestly what a resolver enforces.
type Capabilities struct {
	Resolver string `json:"resolver"`
	// ColumnLevel reports whether this resolver makes real column-level
	// decisions. False means it grants everything.
	ColumnLevel bool   `json:"column_level"`
	Note        string `json:"note"`
}

// PolicyResolver answers whether an identity may read a set of refs. One
// implementation per policy source.
type PolicyResolver interface {
	CanRead(ctx context.Context, id Identity, refs []Ref) (Decision, error)
	Capabilities() Capabilities
}

// AllowAll grants everything. It is the correct resolver for a local DuckDB
// quickstart and the wrong one for anything else, so its capabilities say so
// and every interface reports them.
type AllowAll struct{}

// CanRead allows every ref.
func (AllowAll) CanRead(context.Context, Identity, []Ref) (Decision, error) {
	return Decision{Allowed: true}, nil
}

// Capabilities states plainly that nothing is enforced.
func (AllowAll) Capabilities() Capabilities {
	return Capabilities{
		Resolver:    "allow-all",
		ColumnLevel: false,
		Note: "No access control. Every caller may read every column. " +
			"Use this only for local development against fixture data.",
	}
}

// Refusal codes returned by the gate.
const (
	CodeDenied        = "access_denied"
	CodePolicyFailure = "policy_unavailable"
	// CodePolicyIdentity is a policy source that will never answer for this
	// caller, as opposed to one that is temporarily down. An engine that
	// cannot act as the identity it was asked about is misconfigured, and no
	// amount of waiting changes that.
	CodePolicyIdentity = "policy_identity_unusable"
)

// IsGateCode reports whether this refusal came from the gate, which has
// already written its own audit event for it.
//
// A denial is a refusal, so anything auditing refusals generically will record
// a denial a second time unless it asks. Two events for one decision makes a
// governance report count every denial twice, and the second copy says
// `refused` where the first says `denied`, which is worse than a duplicate: it
// reports an access denial as a correctness problem.
func IsGateCode(code string) bool {
	switch code {
	case CodeDenied, CodePolicyFailure, CodePolicyIdentity:
		return true
	}
	return false
}

// Gate sits between the planner and the emitter. It is the only thing that
// decides whether a plan may become SQL.
type Gate struct {
	resolver PolicyResolver
	audit    AuditSink
	cache    *decisionCache
	rows     RowPolicies
	// environment stamps every event this gate writes. See Options.
	environment string
}

// RowPolicySource is a resolver that also declares which rows a caller may
// see. Optional: a resolver without row restrictions simply does not have it.
type RowPolicySource interface {
	Rows() RowPolicies
}

// RowPolicies returns the row restrictions this gate was built with.
func (g *Gate) RowPolicies() RowPolicies { return g.rows }

// WithRowPolicies attaches row-level restrictions.
func (g *Gate) WithRowPolicies(r RowPolicies) *Gate {
	g.rows = r
	return g
}

// Options configure a Gate.
type Options struct {
	// CacheTTL bounds how long a decision is reused. Zero disables caching.
	// A positive decision is never held longer than this, and a cache miss is
	// always preferred to a stale allow.
	CacheTTL time.Duration

	// Environment stamps every audited decision with the deployment it was
	// made in: dev, prod, or empty where the project file declares none.
	//
	// Set here rather than on each event, because events are built in half
	// a dozen places across two packages and the one that gets forgotten is
	// the denial. An audit trail where most entries name their environment
	// is worse than one where none do: the gaps look like a different
	// environment rather than like a missed field.
	Environment string
}

// DefaultCacheTTL matches the 60 to 300 second window in
// docs/05-governance.md. Short enough that a revoked grant takes effect
// quickly, long enough that a busy agent does not re-resolve on every call.
const DefaultCacheTTL = 60 * time.Second

// NewGate builds a gate. A nil audit sink discards events, which is acceptable
// for local use and never for a deployment that claims governance.
func NewGate(r PolicyResolver, audit AuditSink, opts Options) *Gate {
	if r == nil {
		// Refusing to default to AllowAll is deliberate. A gate constructed by
		// mistake must deny, not grant.
		r = denyAll{}
	}
	if audit == nil {
		audit = DiscardAudit{}
	}
	g := &Gate{resolver: r, audit: audit, environment: opts.Environment}
	// A resolver that declares row restrictions has them picked up here rather
	// than passed separately, so there is no way to configure one and forget
	// to attach the other.
	if src, ok := r.(RowPolicySource); ok {
		g.rows = src.Rows()
	}
	if opts.CacheTTL > 0 {
		g.cache = newDecisionCache(opts.CacheTTL)
	}
	return g
}

// Capabilities reports what the underlying resolver enforces.
func (g *Gate) Capabilities() Capabilities { return g.resolver.Capabilities() }

// Check resolves access for every field the plan reads. It returns a refusal
// when any is denied, and that refusal names semantic objects only.
func (g *Gate) Check(ctx context.Context, id Identity, s *resolve.Schema, p *plan.Plan) error {
	refs := refsFor(s, p)

	decision, err := g.resolve(ctx, id, refs)
	if err != nil {
		// Deny on error. A policy source that cannot be reached is not
		// permission; treating it as permission is the bug that turns a
		// governance control into a no-op during an outage.
		g.audit.Write(Event{
			Time: time.Now().UTC(), Identity: id.Subject,
			ModelName: p.ModelName, ModelVersion: p.ModelVersion, Namespace: p.Namespace,
			Metrics: names(p.Metrics), Dimensions: dimNames(p.Dimensions),
			Decision: "error", Error: err.Error(),
		})
		// A resolver that already knows its failure is permanent says so, and
		// that answer is passed through rather than relabelled. The default
		// below is classified RetryLater, so flattening every failure into it
		// tells an agent to wait and try again after a misconfiguration that
		// will never resolve, and the agent does exactly that, forever.
		var refusal *plan.Refusal
		if errors.As(err, &refusal) {
			return refusal
		}
		return &plan.Refusal{Code: CodePolicyFailure,
			Reason: "access could not be resolved, so the query was not run",
			Hint:   "this is a policy source failure, not a permission problem; retrying may succeed"}
	}

	if !decision.Allowed {
		denied := deniedNames(decision.Denied)
		g.audit.Write(Event{
			Time: time.Now().UTC(), Identity: id.Subject,
			ModelName: p.ModelName, ModelVersion: p.ModelVersion, Namespace: p.Namespace,
			Metrics: names(p.Metrics), Dimensions: dimNames(p.Dimensions),
			Decision: "denied", DeniedFields: denied,
		})
		// Name the semantic objects the caller asked for. The physical column
		// and the governing tag stay out of the message.
		return &plan.Refusal{Code: CodeDenied,
			Subject: strings.Join(blame(s, p, decision.Denied), ", "),
			Reason: fmt.Sprintf("this identity may not read %s",
				plural(blame(s, p, decision.Denied))),
			Hint: "ask the owner of these definitions for access, or request metrics and dimensions that do not depend on them"}
	}
	return nil
}

// resolve consults the cache then the resolver.
func (g *Gate) resolve(ctx context.Context, id Identity, refs []Ref) (Decision, error) {
	if g.cache != nil {
		if d, ok := g.cache.get(id, refs); ok {
			return d, nil
		}
	}
	d, err := g.resolver.CanRead(ctx, id, refs)
	if err != nil {
		return Decision{}, err
	}
	if g.cache != nil {
		g.cache.put(id, refs, d)
	}
	return d, nil
}

// VisibleDimensions filters a dimension list down to what an identity may read.
// An interface offers only these, so a caller is never invited to ask for
// something that will be refused.
func (g *Gate) VisibleDimensions(ctx context.Context, id Identity, fields []*osi.Field) ([]*osi.Field, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	refs := make([]Ref, len(fields))
	for i, f := range fields {
		refs[i] = refFor(f)
	}
	d, err := g.resolver.CanRead(ctx, id, refs)
	if err != nil {
		return nil, err
	}
	denied := map[string]bool{}
	for _, r := range d.Denied {
		denied[r.Name()] = true
	}
	out := make([]*osi.Field, 0, len(fields))
	for _, f := range fields {
		// Compare on the governed name, which is what the resolver decided
		// against. Matching on the local name would silently re-offer a denied
		// dimension whenever a workspace addressed it under an alias.
		if !denied[f.GovernedName()] {
			out = append(out, f)
		}
	}
	return out, nil
}

// Audit records a successful query. The gate writes denials itself; the caller
// reports the allowed case once the SQL exists, so the hash can be included.
// Audit records a decision, stamping the deployment it was made in.
//
// The stamp only fills an empty field, so a caller that already knows more
// than the gate does, a replayed event or one forwarded from elsewhere,
// keeps what it brought.
func (g *Gate) Audit(e Event) {
	if e.Environment == "" {
		e.Environment = g.environment
	}
	g.audit.Write(e)
}

// refsFor turns every field a plan reads into a governance ref.
func refsFor(s *resolve.Schema, p *plan.Plan) []Ref {
	fields := p.Fields(s)
	out := make([]Ref, len(fields))
	for i, f := range fields {
		out[i] = refFor(f)
	}
	return out
}

func refFor(f *osi.Field) Ref {
	r := Ref{
		Origin: f.Dataset.Origin(),
		Field:  f.Name,
		Local:  f.QualifiedName(),
		Source: f.Dataset.Source,
	}
	// Best effort physical column: a field whose expression is a bare
	// identifier maps to exactly one column. A computed field has no single
	// column, and a resolver that needs one must treat an empty Column as
	// "every column this expression reads", which is the conservative reading.
	if id, ok := f.Expression.AST.(*osi.Ident); ok {
		r.Column = id.Parts[len(id.Parts)-1]
	}
	return r
}

// blame maps denied refs back to the metrics and dimensions the caller named,
// so the refusal talks about what they asked for.
func blame(sch *resolve.Schema, p *plan.Plan, denied []Ref) []string {
	deniedSet := map[string]bool{}
	for _, r := range denied {
		deniedSet[r.Name()] = true
	}
	var out []string
	for _, d := range p.Dimensions {
		if deniedSet[d.Field.GovernedName()] {
			out = append(out, "dimension "+d.Field.QualifiedName())
		}
	}
	for _, m := range p.Metrics {
		for _, f := range metricFields(sch, m) {
			if deniedSet[f.GovernedName()] {
				out = append(out, "metric "+m.Metric.Name)
				break
			}
		}
	}
	for _, f := range p.Filters {
		if deniedSet[f.Field.GovernedName()] {
			out = append(out, "filter on "+f.Field.QualifiedName())
		}
	}
	if len(out) == 0 {
		// A denied field that no requested object explains means the plan reads
		// it indirectly. Say so without naming it.
		return []string{"a field required by this request"}
	}
	sort.Strings(out)
	return dedupe(out)
}

// metricFields returns the fields a metric's expression reads.
func metricFields(sch *resolve.Schema, m plan.MetricSelect) []*osi.Field {
	var out []*osi.Field
	for _, id := range osi.Refs(m.Metric.Expression.AST) {
		if len(id.Norm) != 2 {
			continue
		}
		if f, ok := sch.Field(id.Parts[0] + "." + id.Parts[1]); ok {
			out = append(out, f)
		}
	}
	return out
}

func plural(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items, ", ")
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func deniedNames(refs []Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name()
	}
	sort.Strings(out)
	return out
}

func names(ms []plan.MetricSelect) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Metric.Name
	}
	return out
}

func dimNames(ds []plan.DimSelect) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Field.QualifiedName()
	}
	return out
}

// denyAll is the resolver a misconstructed Gate falls back to.
type denyAll struct{}

func (denyAll) CanRead(_ context.Context, _ Identity, refs []Ref) (Decision, error) {
	return Decision{Allowed: false, Denied: refs}, nil
}

func (denyAll) Capabilities() Capabilities {
	return Capabilities{Resolver: "deny-all", ColumnLevel: true,
		Note: "No policy resolver was configured, so every request is denied."}
}

// decisionCache holds resolved decisions for a bounded time.
//
// Only whole-request decisions are cached, keyed by identity and the exact ref
// set, because a per-ref cache would let a new combination assemble an answer
// from entries granted under different policy versions.
type decisionCache struct {
	ttl time.Duration
	mu  sync.Mutex
	// ponytail: one map with lazy eviction on read. A policy cache holds at
	// most identities times distinct queries entries and each is tiny; swap in
	// an LRU if that ever stops being true.
	entries map[string]cacheEntry
}

type cacheEntry struct {
	decision Decision
	expires  time.Time
}

func newDecisionCache(ttl time.Duration) *decisionCache {
	return &decisionCache{ttl: ttl, entries: map[string]cacheEntry{}}
}

func cacheKey(id Identity, refs []Ref) string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name()
	}
	sort.Strings(names)
	principals := append([]string(nil), id.principals()...)
	sort.Strings(principals)
	return strings.Join(principals, ",") + "\x00" + strings.Join(names, ",")
}

func (c *decisionCache) get(id Identity, refs []Ref) (Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[cacheKey(id, refs)]
	if !ok {
		return Decision{}, false
	}
	if time.Now().After(e.expires) {
		// Expired entries are dropped rather than refreshed. Reading must never
		// extend the life of a positive decision.
		delete(c.entries, cacheKey(id, refs))
		return Decision{}, false
	}
	return e.decision, true
}

func (c *decisionCache) put(id Identity, refs []Ref, d Decision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cacheKey(id, refs)] = cacheEntry{decision: d, expires: time.Now().Add(c.ttl)}
}

func init() {
	// A denial is final for this identity: no rearrangement of the request
	// helps, and an agent that keeps trying is generating audit noise and
	// nothing else. A policy source that could not be reached is the opposite,
	// and the very same request may succeed once it is back.
	plan.RegisterRetry(CodeDenied, plan.RetryNever)
	plan.RegisterRetry(CodePolicyFailure, plan.RetryLater)
	// Permanent, unlike the one above. The policy source is reachable and
	// working; this engine simply cannot ask it on this caller's behalf,
	// which is a deployment to fix rather than a moment to wait out.
	plan.RegisterRetry(CodePolicyIdentity, plan.RetryNever)
}
