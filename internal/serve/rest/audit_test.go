package rest_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// The audit log names every caller, every metric they touched and every field
// they were refused. Serving it is a wider grant than serving a query, so
// these tests are about who cannot read it.

func auditHandler(t *testing.T, readers []string, events ...govern.Event) http.Handler {
	t.Helper()
	recent := govern.NewRecentAudit(50)
	for _, e := range events {
		recent.Write(e)
	}
	return server(t, false, govern.Identity{Subject: "analyst@acme.com"}).
		WithAuditLog(recent, rest.NewAuditReaders(readers)).
		Handler()
}

// TestNotServedUntilSomebodyIsNamed. Off is the default, and it is 404 rather
// than 403 because the capability is absent rather than withheld.
func TestNotServedUntilSomebodyIsNamed(t *testing.T) {
	h := auditHandler(t, nil)
	res := do(t, h, "GET", "/v1/audit", "", token)
	if res.Code != http.StatusNotFound {
		t.Errorf("want 404 when no reader is configured, got %d: %s", res.Code, res.Body)
	}
}

// TestOnlyNamedReadersMayRead.
func TestOnlyNamedReadersMayRead(t *testing.T) {
	h := auditHandler(t, []string{"ops@acme.com"},
		govern.Event{Identity: "analyst@acme.com", Decision: "refused", RefusalCode: "fan_out_would_inflate"})

	// The server resolves every token to analyst@acme.com, who is not a reader.
	res := do(t, h, "GET", "/v1/audit", "", token)
	if res.Code != http.StatusForbidden {
		t.Errorf("want 403 for a caller who is not a reader, got %d: %s", res.Code, res.Body)
	}
	if contains(res.Body.String(), "fan_out_would_inflate") {
		t.Errorf("a refused caller was shown the record anyway:\n%s", res.Body)
	}
}

func TestANamedReaderSeesEveryCallersDecisions(t *testing.T) {
	// The reader is the identity this server resolves tokens to.
	h := auditHandler(t, []string{"analyst@acme.com"},
		govern.Event{Identity: "someone-else@acme.com", Decision: "refused",
			RefusalCode: "fan_out_would_inflate", Retry: "modify",
			Reason: "metric aggregates SUM over orders", Hint: "use line_revenue"},
		govern.Event{Identity: "third@acme.com", Decision: "allowed", RowCount: 2})

	res := do(t, h, "GET", "/v1/audit", "", token)
	if res.Code != http.StatusOK {
		t.Fatalf("a named reader was refused: %d %s", res.Code, res.Body)
	}

	var body struct {
		Events []govern.Event `json:"events"`
		Count  int            `json:"count"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if body.Count != 2 {
		t.Fatalf("want 2 events, got %d", body.Count)
	}
	// Newest first, because an operator opens this to see what just happened.
	if body.Events[0].Identity != "third@acme.com" {
		t.Errorf("want the newest event first, got %s", body.Events[0].Identity)
	}
	// The whole point: an operator sees other people's decisions.
	if body.Events[1].Identity != "someone-else@acme.com" {
		t.Errorf("a reader should see every caller, got %s", body.Events[1].Identity)
	}
	if body.Events[1].Hint == "" {
		t.Error("the hint is the actionable half of a refusal and must survive")
	}
}

// TestAnonymousIsNeverAReader.
//
// On an engine running without authentication every caller is anonymous. If an
// empty subject could match, naming any reader at all would publish the record
// to anyone who can reach the port.
func TestAnonymousIsNeverAReader(t *testing.T) {
	readers := rest.NewAuditReaders([]string{"ops@acme.com", "group:platform"})
	if readers.Allows(govern.Identity{}) {
		t.Error("an unauthenticated caller must never be a reader")
	}
	if readers.Allows(govern.Identity{Subject: ""}) {
		t.Error("an empty subject must never be a reader")
	}
	if readers.Allows(govern.Identity{Subject: "stranger@acme.com"}) {
		t.Error("an unnamed subject must not be a reader")
	}
	if !readers.Allows(govern.Identity{Subject: "ops@acme.com"}) {
		t.Error("a named subject should be a reader")
	}
	if !readers.Allows(govern.Identity{Subject: "x@acme.com", Groups: []string{"platform"}}) {
		t.Error("a named group should be a reader")
	}
	// A group name must not be usable as a subject, or `group:platform` would
	// let anyone who can set their own subject to that string in.
	if readers.Allows(govern.Identity{Subject: "platform"}) {
		t.Error("a group name must not match as a subject")
	}
}

func TestEmptyReadersIsDisabled(t *testing.T) {
	for _, entries := range [][]string{nil, {}, {""}, {"  "}, {"group:"}} {
		if rest.NewAuditReaders(entries).Enabled() {
			t.Errorf("%q should leave the endpoint disabled", entries)
		}
	}
}

// TestHealthDoesNotNameTheReaders. Who may read the audit log is itself worth
// not publishing, since it names the accounts worth attacking.
func TestHealthDoesNotNameTheReaders(t *testing.T) {
	readers := rest.NewAuditReaders([]string{"ops@acme.com", "group:platform"})
	desc := readers.Describe()
	if contains(desc, "ops@acme.com") || contains(desc, "platform") {
		t.Errorf("the description names its readers: %q", desc)
	}
	if !contains(desc, "2") {
		t.Errorf("the description should say how many, got %q", desc)
	}
}

func TestDecisionFilterNarrowsToRefusals(t *testing.T) {
	h := auditHandler(t, []string{"analyst@acme.com"},
		govern.Event{Identity: "a", Decision: "allowed"},
		govern.Event{Identity: "b", Decision: "refused", RefusalCode: "fan_out_would_inflate"},
		govern.Event{Identity: "c", Decision: "denied"})

	res := do(t, h, "GET", "/v1/audit?decision=refused", "", token)
	var body struct {
		Events []govern.Event `json:"events"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].Decision != "refused" {
		t.Errorf("want only the refusal, got %+v", body.Events)
	}
}
