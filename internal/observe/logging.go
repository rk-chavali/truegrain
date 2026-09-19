package observe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Logging, and what may never appear in it.
//
// A log is the most casually read thing a system produces and the least
// carefully reviewed. The rules are the audit log's rules, for the same
// reasons: no filter values, no compiled SQL, no result rows, no tokens, no
// request bodies. A caller's subject is recorded, because a log that cannot
// say who did something is not much of a log and the subject is already in
// the audit trail.
//
// What is not here is a logger threaded through every constructor. The request
// middleware puts one in the context and the handlers take it out, which means
// a line without a request id is a line written outside a request, and that is
// usually worth noticing on its own.

// LogFormat selects the encoding.
type LogFormat string

const (
	// LogText is for a person reading a terminal.
	LogText LogFormat = "text"
	// LogJSON is for anything that will be parsed. It is the default for a
	// server, because a log nobody can query is a log nobody reads.
	LogJSON LogFormat = "json"
)

// NewLogger builds the root logger.
func NewLogger(w io.Writer, format LogFormat, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == LogText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// ParseLevel maps a flag value onto a level. It is permissive about case and
// refuses anything it does not recognise, rather than silently choosing a
// level the operator did not ask for and then hiding the lines they wanted.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

// WithLogger puts lg in the context.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, lg)
}

// LoggerFrom returns the request's logger, or one that discards.
//
// Discarding rather than returning nil means no caller has to nil check before
// logging, and a missing logger degrades to silence instead of a panic on a
// path that was probably already going wrong.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if lg, ok := ctx.Value(loggerKey).(*slog.Logger); ok && lg != nil {
		return lg
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// RequestIDFrom returns the id assigned to this request, if there is one.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// newRequestID is 64 bits from crypto/rand.
//
// It is not a security boundary, unlike a job id, but it is echoed to the
// caller and a counter would tell every caller how much traffic the engine is
// serving and let them guess another request's id from their own.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A request that cannot be labelled is still a request worth serving.
		return "unidentified"
	}
	return hex.EncodeToString(b[:])
}

// Middleware assigns each request an id, logs its completion, and puts a
// logger carrying both in the context.
//
// route comes from the server's routing table, never from the request path. A
// path is caller controlled and would put whatever they sent into a log line
// and, worse, into a metric label.
func Middleware(lg *slog.Logger, route func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := newRequestID()

			attrs := []any{
				slog.String("request_id", id),
				slog.String("method", r.Method),
				slog.String("route", route(r)),
			}
			// A trace id in the log is what lets someone move from a line they
			// found to the span that explains it.
			if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
				attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
			}
			reqLog := lg.With(attrs...)

			ctx := WithLogger(context.WithValue(r.Context(), requestIDKey, id), reqLog)
			// Echoed so a caller reporting a problem can name the request, and
			// so an operator can find it without knowing when it happened.
			w.Header().Set("X-Request-Id", id)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			// Level by outcome: a refusal is the engine working and does not
			// deserve the same weight as a request that broke it.
			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			}
			reqLog.LogAttrs(ctx, level, "request",
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		})
	}
}

// statusRecorder remembers the status so the middleware can log it.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer, so wrapping does
// not silently take flushing away from a handler that streams.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
