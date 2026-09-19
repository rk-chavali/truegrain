package observe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// These tests are about what cannot be said, not what can. The engine already
// declines to put filter values, compiled SQL or result rows in the audit log,
// and telemetry is the obvious way for all three to escape anyway.

// allowed is every attribute key this package is permitted to emit. A new key
// has to be added here deliberately, which is the review step the whole design
// is arranged around.
var allowed = map[string]bool{
	"truegrain.namespace":       true,
	"truegrain.model_version":   true,
	"truegrain.dialect":         true,
	"truegrain.outcome":         true,
	"truegrain.refusal_code":    true,
	"http.route":                true,
	"http.response.status_code": true,
	"truegrain.metric_count":    true,
	"truegrain.dimension_count": true,
	"truegrain.filter_count":    true,
	"truegrain.part_count":      true,
	"truegrain.row_count":       true,
}

// recorder installs a real tracer that keeps spans in memory, and restores
// whatever was there when the test finishes.
func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// TestASpanCarriesOnlyAllowedKeys drives every setter the type has and checks
// nothing else reached the exporter.
func TestASpanCarriesOnlyAllowedKeys(t *testing.T) {
	sr := recorder(t)

	ctx, span := StartQuery(context.Background(), "/v1/query")
	span.Shape(2, 3, 1).Model("sales", "sha256:abc", 2)
	_, child := StartExecute(ctx, "bigquery")
	child.End()
	span.Succeeded(42)

	spans := sr.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded")
	}
	for _, s := range spans {
		for _, kv := range s.Attributes() {
			if !allowed[string(kv.Key)] {
				t.Errorf("span %q carries unallowed attribute %q", s.Name(), kv.Key)
			}
		}
	}
}

// TestTheRequestNeverReachesASpan. The setters take counts, so there is no
// call that could attach one. This drives a request shaped like a real one and
// checks none of its content is anywhere in the recorded attributes.
func TestTheRequestNeverReachesASpan(t *testing.T) {
	sr := recorder(t)

	// The things a request carries that must never be exported: a filter
	// value that is somebody's email, and the statement that embeds the schema.
	const email = "anna@customer.example"
	const sql = "SELECT SUM(amount) FROM orders WHERE email = $1"

	ctx, span := StartQuery(context.Background(), "/v1/query")
	span.Shape(1, 1, 1).Model("sales", "sha256:abc", 1)
	_, child := StartExecute(ctx, "postgres")
	child.End()
	span.Succeeded(1)

	for _, s := range sr.Ended() {
		for _, kv := range s.Attributes() {
			v := kv.Value.String()
			if strings.Contains(v, email) || strings.Contains(v, sql) || strings.Contains(v, "SELECT") {
				t.Errorf("attribute %q leaked request content: %q", kv.Key, v)
			}
		}
	}
}

// TestARefusalIsNotAnError. A refusal is the engine working. Recording it as
// an error makes a dashboard of a healthy engine look like an incident, and
// the people watching learn to ignore it.
func TestARefusalIsNotAnError(t *testing.T) {
	sr := recorder(t)

	_, span := StartQuery(context.Background(), "/v1/query")
	span.Refused("fan_out_would_inflate")

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if got := spans[0].Status().Code.String(); got == "Error" {
		t.Errorf("a refusal was recorded as an error")
	}
	var outcome string
	for _, kv := range spans[0].Attributes() {
		if kv.Key == attrOutcome {
			outcome = kv.Value.String()
		}
	}
	if outcome != OutcomeRefused {
		t.Errorf("want outcome %q, got %q", OutcomeRefused, outcome)
	}
}

// TestAFailureIsAnError, and carries the reason, because that is the one place
// something unbounded is written and a trace is already privileged.
func TestAFailureIsAnError(t *testing.T) {
	sr := recorder(t)
	_, span := StartQuery(context.Background(), "/v1/query")
	span.Failed(errors.New("the warehouse closed the connection"))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if got := spans[0].Status().Code.String(); got != "Error" {
		t.Errorf("want an error status, got %q", got)
	}
}

// TestAnUnregisteredCodeCannotGrowTheLabelSpace.
//
// A refusal code becomes a metric label. Registered codes come from a table
// closed at init, so there are a few dozen. Anything else arrived without
// passing through that table, and a caller who can add label values can
// multiply the series a metrics backend stores until it falls over.
func TestAnUnregisteredCodeCannotGrowTheLabelSpace(t *testing.T) {
	registered := func(code string) bool { return code == "fan_out_would_inflate" }

	if got := safeCode("fan_out_would_inflate", registered); got != "fan_out_would_inflate" {
		t.Errorf("a registered code must be kept, got %q", got)
	}
	for _, hostile := range []string{
		"attacker-chosen-0001",
		strings.Repeat("x", 4096),
		"code\nwith\nnewlines",
	} {
		if got := safeCode(hostile, registered); got != "unregistered" {
			t.Errorf("unregistered code %.20q must be collapsed, got %q", hostile, got)
		}
	}
	if got := safeCode("", registered); got != "" {
		t.Errorf("no code should stay empty, got %q", got)
	}
}

// TestOffUnlessAnEndpointIsGiven. An engine nobody is collecting from should
// open no connections, and the caller should not have to branch on it.
func TestOffUnlessAnEndpointIsGiven(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("a zero Config must be disabled")
	}
	if (Config{Endpoint: "   "}).Enabled() {
		t.Error("whitespace is not an endpoint")
	}
	shutdown, err := Start(context.Background(), Config{})
	if err != nil {
		t.Fatalf("starting with telemetry off: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must be callable even when nothing started")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutting down a disabled telemetry: %v", err)
	}
}

// TestAURLEndpointIsRefusedAtStartup. The exporter accepts it and treats the
// whole string as a hostname, so the only symptom is telemetry that never
// arrives and no error anywhere.
func TestAURLEndpointIsRefusedAtStartup(t *testing.T) {
	for _, bad := range []string{
		"http://collector:4317",
		"https://collector:4317",
		"collector:4317/v1/traces",
	} {
		if _, err := Start(context.Background(), Config{Endpoint: bad}); err == nil {
			t.Errorf("endpoint %q should have been refused", bad)
		}
	}
}

func TestSampleRatioIsClamped(t *testing.T) {
	for in, want := range map[float64]float64{-1: 0, 0: 0, 0.25: 0.25, 1: 1, 7: 1} {
		if got := ratio(in); got != want {
			t.Errorf("ratio(%v) = %v, want %v", in, got, want)
		}
	}
}

// TestTheLogNeverCarriesTheCredential.
//
// The middleware sees the whole request, including the bearer token and the
// body. A log line is read by more people than an audit file and is shipped
// further, so neither may appear.
func TestTheLogNeverCarriesTheCredential(t *testing.T) {
	const token = "supersecret-bearer-value"
	const filterValue = "anna@customer.example"

	var buf bytes.Buffer
	lg := NewLogger(&buf, LogJSON, slog.LevelDebug)
	h := Middleware(lg, func(*http.Request) string { return "/v1/query" })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	body := `{"metrics":["sales.order_revenue"],"filters":[{"field":"email","eq":"` + filterValue + `"}]}`
	req := httptest.NewRequest("POST", "/v1/query?secret="+filterValue, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if out == "" {
		t.Fatal("the middleware logged nothing at all")
	}
	for _, forbidden := range []string{token, filterValue, "Bearer", "Authorization"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the log leaked %q:\n%s", forbidden, out)
		}
	}
}

// TestEveryRequestIsIdentified, and the caller is told which one theirs was,
// so a bug report can name it.
func TestEveryRequestIsIdentified(t *testing.T) {
	var buf bytes.Buffer
	lg := NewLogger(&buf, LogJSON, slog.LevelInfo)

	var seen string
	h := Middleware(lg, func(*http.Request) string { return "/v1/health" })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = RequestIDFrom(r.Context())
			LoggerFrom(r.Context()).Info("handler ran")
			w.WriteHeader(http.StatusOK)
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))

	header := rec.Header().Get("X-Request-Id")
	if header == "" {
		t.Error("the caller was not told which request was theirs")
	}
	if seen != header {
		t.Errorf("the handler saw %q but the caller was told %q", seen, header)
	}
	if !strings.Contains(buf.String(), header) {
		t.Errorf("the log line does not carry the request id:\n%s", buf.String())
	}
	// Two requests must not share one.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/health", nil))
	if rec2.Header().Get("X-Request-Id") == header {
		t.Error("two requests were given the same id")
	}
}

// TestTheRouteIsOursNotTheirs. A path is caller controlled, and putting it in
// a log line or a metric label hands them the label space.
func TestTheRouteIsOursNotTheirs(t *testing.T) {
	var buf bytes.Buffer
	lg := NewLogger(&buf, LogJSON, slog.LevelInfo)
	h := Middleware(lg, func(*http.Request) string { return "/v1/metrics" })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/v1/metrics/../../etc/passwd", nil))

	if strings.Contains(buf.String(), "passwd") {
		t.Errorf("the caller's path reached the log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "/v1/metrics") {
		t.Errorf("the server's own route is missing:\n%s", buf.String())
	}
}

func TestParseLevelRefusesWhatItDoesNotKnow(t *testing.T) {
	if _, ok := ParseLevel("verbose"); ok {
		t.Error("an unknown level must be refused rather than silently defaulted")
	}
	for _, in := range []string{"debug", "INFO", " warn ", "Error", ""} {
		if _, ok := ParseLevel(in); !ok {
			t.Errorf("level %q should be accepted", in)
		}
	}
}

// TestStatusIsRecordedWithoutStealingTheWriter. Wrapping a ResponseWriter is
// how streaming quietly stops working, so the wrapper has to stay unwrappable.
func TestStatusIsRecordedWithoutStealingTheWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	if s.Unwrap() != http.ResponseWriter(rec) {
		t.Error("the real writer must stay reachable")
	}
	s.WriteHeader(http.StatusTeapot)
	s.WriteHeader(http.StatusOK) // a second call must not overwrite the first
	if s.status != http.StatusTeapot {
		t.Errorf("want the first status kept, got %d", s.status)
	}
}
