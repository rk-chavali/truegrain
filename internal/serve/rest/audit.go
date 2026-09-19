package rest

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Reading the audit log over HTTP, which is a wider grant than it looks.
//
// The file records every caller, every metric they touched and every field
// they were refused. That is the point of it, and it is also why serving it to
// an ordinary caller is wrong: it discloses which teams query which parts of
// the model, and it tells anyone curious exactly which fields are protected
// and therefore worth attacking. The jobs API answered the same question by
// scoping to the owner, but an operator opening this wants everyone's activity,
// which is precisely what a normal caller must never have.
//
// So it is off until an operator names who may read it, by subject or by
// group. Off is not a hedge: an engine where nobody was named exposes nothing
// and answers 404, because a capability nobody configured does not exist.

// AuditReaders is the allowlist for reading recorded decisions.
//
// Empty means the endpoint is not served at all. That is the default, and it
// matches how cross-origin access and policy tags already behave: capability
// off until somebody says who gets it.
type AuditReaders struct {
	subjects []string
	groups   []string
}

// NewAuditReaders builds the allowlist. An entry beginning with `group:` names
// a group; anything else names a subject.
func NewAuditReaders(entries []string) *AuditReaders {
	out := &AuditReaders{}
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if group, ok := strings.CutPrefix(entry, "group:"); ok {
			if group = strings.TrimSpace(group); group != "" {
				out.groups = append(out.groups, group)
			}
			continue
		}
		out.subjects = append(out.subjects, entry)
	}
	return out
}

// Enabled reports whether anybody may read the audit log.
func (a *AuditReaders) Enabled() bool {
	return a != nil && (len(a.subjects) > 0 || len(a.groups) > 0)
}

// Allows reports whether this identity is one of the named readers.
//
// An anonymous caller is never a reader. On an engine running without
// authentication every caller is anonymous, and the alternative would publish
// the whole record to anyone who can reach the port.
func (a *AuditReaders) Allows(id govern.Identity) bool {
	if !a.Enabled() || id.Subject == "" {
		return false
	}
	if slices.Contains(a.subjects, id.Subject) {
		return true
	}
	for _, group := range id.Groups {
		if slices.Contains(a.groups, group) {
			return true
		}
	}
	return false
}

// Describe reports the configuration for health, without naming the readers.
// Who may read the audit log is itself worth not publishing.
func (a *AuditReaders) Describe() string {
	if !a.Enabled() {
		return "not served"
	}
	return "served to " + strconv.Itoa(len(a.subjects)+len(a.groups)) + " named reader(s)"
}

// maxAuditPage bounds one response. The buffer is bounded too, so this only
// stops a caller asking for the whole of it in one request.
const maxAuditPage = 200

// listAudit serves recorded decisions, newest first.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	// 404 rather than 403 when nobody is configured: the capability is absent,
	// not withheld, and saying "forbidden" would imply there is something here
	// to be let into.
	if !s.auditReaders.Enabled() || s.recentAudit == nil {
		writeError(w, http.StatusNotFound, "audit_not_served",
			"this engine does not serve recorded decisions",
			"start it with -audit-readers naming who may read them")
		return
	}

	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	// 403 here, unlike above: the endpoint exists and this caller is not one of
	// its readers. There is no oracle to protect, because the endpoint's
	// existence is already visible in health.
	if !s.auditReaders.Allows(id) {
		writeError(w, http.StatusForbidden, "not_an_audit_reader",
			"this identity may not read recorded decisions",
			"ask an operator to add it to -audit-readers")
		return
	}

	limit := maxAuditPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_limit",
				"limit must be a positive whole number", "omit it for the default")
			return
		}
		limit = min(n, maxAuditPage)
	}

	events := s.recentAudit.Recent(limit)
	if decision := r.URL.Query().Get("decision"); decision != "" {
		events = onlyDecision(events, decision)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
		// Named so a caller knows this is a window and not the whole record.
		"note": "the most recent decisions held in memory; the audit file is the complete record",
	})
}

// onlyDecision filters in place of a query language. Decisions are a closed set
// of five words, and the one an operator wants is almost always "refused".
func onlyDecision(events []govern.Event, decision string) []govern.Event {
	out := make([]govern.Event, 0, len(events))
	for _, e := range events {
		if e.Decision == decision {
			out = append(out, e)
		}
	}
	return out
}
