package rest_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/observe"
)

// What the server may say about a request, and what it may not.
//
// The engine takes care not to put filter values, compiled SQL or result rows
// in the audit log. A request log is read by more people and shipped further,
// so the same values must not appear there either.

func TestLoggingIsOffUnlessAskedFor(t *testing.T) {
	h := server(t, false, govern.Identity{Subject: "tester"}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))

	if rec.Code != 200 {
		t.Fatalf("health should answer, got %d", rec.Code)
	}
	// Without a logger the chain is not wrapped at all, so no request id is
	// minted. A server nobody asked to log should do nothing extra.
	if got := rec.Header().Get("X-Request-Id"); got != "" {
		t.Errorf("an unlogged server should not label requests, got %q", got)
	}
}

func TestALoggedRequestIsIdentifiedToItsCaller(t *testing.T) {
	var buf bytes.Buffer
	lg := observe.NewLogger(&buf, observe.LogJSON, slog.LevelInfo)
	h := server(t, false, govern.Identity{Subject: "tester"}).WithLogging(lg).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))

	id := rec.Header().Get("X-Request-Id")
	if id == "" {
		t.Fatal("a logged request must be identified to its caller")
	}
	if !strings.Contains(buf.String(), id) {
		t.Errorf("the log does not carry the id the caller was given:\n%s", buf.String())
	}
}

// TestTheLoggedRouteIsOursNotTheirs.
//
// The route is taken from the server's own pattern table rather than from
// r.URL.Path. A path is caller controlled: logged, it puts their string in an
// operator's log, and as a metric label it hands them the label space.
func TestTheLoggedRouteIsOursNotTheirs(t *testing.T) {
	var buf bytes.Buffer
	lg := observe.NewLogger(&buf, observe.LogJSON, slog.LevelInfo)
	h := server(t, false, govern.Identity{Subject: "tester"}).WithLogging(lg).Handler()

	// A path that matches GET /v1/metrics/{name} with a hostile-looking name.
	req := httptest.NewRequest("GET", "/v1/metrics/"+strings.Repeat("x", 300), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, l)
		}
		route, _ := line["route"].(string)
		if strings.Contains(route, "xxxx") {
			t.Errorf("the caller's path became the route label: %q", route)
		}
		if route != "" && !strings.HasPrefix(route, "/v1/") {
			t.Errorf("unexpected route label %q", route)
		}
	}
}

// TestTheQueryBodyNeverReachesTheLog. The body carries filter values, which
// are customer identifiers, and they must not survive into a log line.
func TestTheQueryBodyNeverReachesTheLog(t *testing.T) {
	var buf bytes.Buffer
	lg := observe.NewLogger(&buf, observe.LogJSON, slog.LevelDebug)
	h := server(t, true, govern.Identity{Subject: "tester"}).WithLogging(lg).Handler()

	const customer = "anna@customer.example"
	body := `{"metrics":["sales.order_revenue"],` +
		`"filters":[{"field":"sales.customers.email_domain","op":"eq","value":"` + customer + `"}]}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, req)

	out := buf.String()
	if out == "" {
		t.Fatal("nothing was logged at all")
	}
	for _, forbidden := range []string{customer, "Bearer", token, "SELECT"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the log leaked %q:\n%s", forbidden, out)
		}
	}
}
