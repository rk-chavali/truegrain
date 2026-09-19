package govern

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// The audit log is what makes this layer defensible in a security review, so it
// is a feature rather than debug output.
//
// What is deliberately absent: filter values, the compiled SQL text, and result
// rows. A filter value can be a customer email or an account number, and the
// SQL text embeds the schema. The hash is enough to prove which statement ran
// and to correlate with the warehouse's own job log, without this file becoming
// a second copy of the data it governs.

// Event is one auditable decision.
type Event struct {
	Time     time.Time `json:"time"`
	Identity string    `json:"identity"`
	// Environment is the deployment the decision was made in: dev, prod, or
	// empty where the project file declares none.
	//
	// On every event rather than assumed from the log's location, because
	// the whole point of an audit trail is answering a question months
	// later, and "which deployment was this" is the first thing anybody
	// asks about a number they do not believe.
	Environment  string `json:"environment,omitempty"`
	ModelName    string `json:"model_name,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`
	// Namespace is the workspace namespace the request resolved to. It is the
	// facet an evidence query filters on first: "who queried finance metrics".
	Namespace  string   `json:"namespace,omitempty"`
	Metrics    []string `json:"metrics,omitempty"`
	Dimensions []string `json:"dimensions,omitempty"`
	// Rollups names the pre-aggregated tables the answer was read from,
	// empty when it came from the base fact.
	//
	// Recorded because a rollup is a second copy of a number, and the
	// question somebody asks about a number they do not believe is where
	// it came from. Without this the audit log says a query ran and cannot
	// say which table answered it.
	Rollups []string `json:"rollups,omitempty"`
	// Decision is one of allowed, refused, denied, compiled or error.
	//
	// refused and denied are both the engine saying no, and they are separate
	// because they mean opposite things about the caller. denied is access: you
	// may not read this. refused is correctness: this question cannot be
	// answered accurately, and the hint names one that can. Collapsing them
	// would make a governance report count every fan-out as an access incident.
	Decision string `json:"decision"`
	// RefusalCode identifies which refusal, for a decision of refused. It is
	// drawn from the registry that closes at init, so it is a bounded set.
	RefusalCode string `json:"refusal_code,omitempty"`
	// Retry is what a caller should do about a refusal: modify, later or never.
	Retry string `json:"retry,omitempty"`
	// Reason is the refusal in the engine's own words, as the caller received
	// it. Safe to record: a refusal names semantic objects and never a value
	// from the request.
	Reason string `json:"reason,omitempty"`
	// Hint is the actionable half of a refusal, naming what would answer.
	Hint string `json:"hint,omitempty"`
	// DeniedFields names the semantic fields refused. Present only on denials,
	// and written only to the audit sink, never returned to the caller.
	DeniedFields []string `json:"denied_fields,omitempty"`
	// SQLHash identifies the compiled statement without disclosing it.
	SQLHash string `json:"sql_hash,omitempty"`
	Dialect string `json:"dialect,omitempty"`
	// JobID is the warehouse's own identifier for the execution, when there
	// was one, so an auditor can join these two logs.
	JobID string `json:"job_id,omitempty"`
	// RowCount is present for a query that ran to completion.
	RowCount int `json:"row_count,omitempty"`
	// BytesBilled is what the warehouse says the query cost. Zero means not
	// reported rather than free: DuckDB bills nobody and says nothing, and the
	// two are indistinguishable here on purpose.
	BytesBilled int64 `json:"bytes_billed,omitempty"`
	// DurationMS is the wall clock time of the warehouse round trip.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Error is the failure reason for a decision of error.
	Error string `json:"error,omitempty"`
}

// AuditSink receives events. Write must not block for long and must not panic:
// a failing audit sink degrades observability, and taking the query path down
// with it would be worse.
type AuditSink interface {
	Write(Event)
}

// DiscardAudit drops every event.
type DiscardAudit struct{}

// Write discards the event.
func (DiscardAudit) Write(Event) {}

// JSONLAudit writes one JSON object per line. It is the default sink and the
// format Cloud Logging ingests without a parser.
type JSONLAudit struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSONLAudit writes events to w.
func NewJSONLAudit(w io.Writer) *JSONLAudit {
	return &JSONLAudit{enc: json.NewEncoder(w)}
}

// Write emits one line. An encoding failure is swallowed on purpose: there is
// nowhere useful to report it from inside an audit sink, and the alternative is
// crashing the query path over a log line.
func (a *JSONLAudit) Write(e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.enc.Encode(e)
}

// Multi writes one event to several sinks.
//
// The usual pairing is the file, which is the durable record an auditor reads,
// and RecentAudit, which is what an operator can see without shelling into the
// container. Neither replaces the other: a ring buffer forgets, and a file is
// not something a console can page through.
type Multi []AuditSink

// Write fans out. A sink that panics would take the query path with it, so
// each is called in its own deferred recovery: an audit sink misbehaving must
// degrade the record, never the answer.
func (m Multi) Write(e Event) {
	for _, sink := range m {
		func() {
			defer func() { _ = recover() }()
			sink.Write(e)
		}()
	}
}

// DefaultRecentAudit bounds what an operator can page back through. Large
// enough to cover an incident someone is actively looking at, small enough
// that the memory cost is fixed and knowable.
const DefaultRecentAudit = 1000

// RecentAudit keeps the last N events in memory so an operator can read them.
//
// Bounded on purpose. An unbounded buffer on a busy engine is a memory leak
// with a governance justification, and the file sink already holds everything
// for anyone who needs the whole record.
type RecentAudit struct {
	mu     sync.Mutex
	ring   []Event
	next   int
	filled bool
}

// NewRecentAudit builds a buffer of size n. A non-positive n uses the default.
func NewRecentAudit(n int) *RecentAudit {
	if n <= 0 {
		n = DefaultRecentAudit
	}
	return &RecentAudit{ring: make([]Event, n)}
}

// Write records an event, overwriting the oldest once full.
func (r *RecentAudit) Write(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ring[r.next] = e
	r.next = (r.next + 1) % len(r.ring)
	if r.next == 0 {
		r.filled = true
	}
}

// Recent returns up to limit events, newest first.
func (r *RecentAudit) Recent(limit int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := r.next
	if r.filled {
		total = len(r.ring)
	}
	if limit <= 0 || limit > total {
		limit = total
	}

	out := make([]Event, 0, limit)
	for i := range limit {
		// Walk backwards from the most recently written slot.
		idx := (r.next - 1 - i + len(r.ring)) % len(r.ring)
		out = append(out, r.ring[idx])
	}
	return out
}

// MemoryAudit collects events in memory. Tests assert against it; the real-GCP
// governance suite in docs/06-validation.md asserts that a denial was recorded
// with the identity and the metric.
type MemoryAudit struct {
	mu     sync.Mutex
	events []Event
}

// Write appends the event.
func (a *MemoryAudit) Write(e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

// Events returns a copy of everything recorded.
func (a *MemoryAudit) Events() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Event(nil), a.events...)
}
