package observe

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/rk-chavali/truegrain/internal/plan"
)

// The instruments are created once at init rather than when telemetry is
// configured, so no recording site has to check whether it is allowed to
// record. otel's global providers delegate: an instrument built before
// Start swaps itself onto the real provider the moment one is installed, and
// stays a no-op if none ever is.
var (
	queryDuration metric.Float64Histogram
	compileTime   metric.Float64Histogram
	refusalCount  metric.Int64Counter
	jobsRunning   metric.Int64UpDownCounter
	bytesBilled   metric.Int64Counter
	rowsReturned  metric.Int64Histogram

	bindErr error
)

func init() { bindErr = bind() }

func bind() error {
	m := otel.Meter(scope)
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	var err error
	queryDuration, err = m.Float64Histogram("truegrain.query.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Wall clock time of a query, from request to answer or refusal."))
	collect(err)

	// Separate from the total on purpose. A slow warehouse and a slow planner
	// need different responses, and a single number cannot tell them apart.
	compileTime, err = m.Float64Histogram("truegrain.compile.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time spent resolving, planning and governing, excluding the warehouse."))
	collect(err)

	refusalCount, err = m.Int64Counter("truegrain.refusals",
		metric.WithDescription("Queries declined at compile time, by refusal code."))
	collect(err)

	jobsRunning, err = m.Int64UpDownCounter("truegrain.jobs.running",
		metric.WithDescription("Asynchronous queries in flight on this replica."))
	collect(err)

	// Money. The one number worth alerting on before anything is visibly wrong.
	bytesBilled, err = m.Int64Counter("truegrain.warehouse.bytes_billed",
		metric.WithUnit("By"),
		metric.WithDescription("Bytes the warehouse reports as billed for executed queries."))
	collect(err)

	rowsReturned, err = m.Int64Histogram("truegrain.query.rows",
		metric.WithDescription("Rows returned by a query that ran to completion."))
	collect(err)

	return errors.Join(errs...)
}

// Span is a query in progress.
//
// It exposes no way to attach a value that came from a request. The setters
// take counts and strings drawn from closed sets, so a filter value, a column
// value or a compiled statement cannot reach a collector through this type
// even by mistake. That is the point of it being a type rather than a
// convention.
type Span struct {
	span  trace.Span
	start time.Time
}

// StartQuery begins a span for one query and returns a context carrying it.
//
// route is the handler that received it, which is a fixed string from the
// server's own routing table rather than the path the caller sent.
func StartQuery(ctx context.Context, route string) (context.Context, *Span) {
	ctx, s := otel.Tracer(scope).Start(ctx, "truegrain.query",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrRoute.String(route)))
	return ctx, &Span{span: s, start: time.Now()}
}

// StartCompile begins a child span for the part that does not touch the
// warehouse.
func StartCompile(ctx context.Context) (context.Context, *Span) {
	ctx, s := otel.Tracer(scope).Start(ctx, "truegrain.compile")
	return ctx, &Span{span: s, start: time.Now()}
}

// StartGovern begins a child span for the access decision. It is separate from
// compiling because "the gate is slow" and "the planner is slow" are different
// problems, and the gate can call out to a catalogue.
func StartGovern(ctx context.Context) (context.Context, *Span) {
	ctx, s := otel.Tracer(scope).Start(ctx, "truegrain.govern")
	return ctx, &Span{span: s, start: time.Now()}
}

// StartExecute begins a child span for the warehouse round trip.
func StartExecute(ctx context.Context, dialect string) (context.Context, *Span) {
	ctx, s := otel.Tracer(scope).Start(ctx, "truegrain.execute",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrDialect.String(dialect)))
	return ctx, &Span{span: s, start: time.Now()}
}

// Shape records the size of the request, never its content. How many metrics
// were asked for is an operational fact; which ones is a question about the
// caller's business.
func (s *Span) Shape(metrics, dimensions, filters int) *Span {
	if s == nil {
		return s
	}
	s.span.SetAttributes(
		attrMetrics.Int(metrics),
		attrDims.Int(dimensions),
		attrFilters.Int(filters),
	)
	return s
}

// Model records which workspace and revision answered, so a number that
// changed can be tied to the model change that changed it.
func (s *Span) Model(namespace, version string, parts int) *Span {
	if s == nil {
		return s
	}
	s.span.SetAttributes(
		attrNamespace.String(namespace),
		attrModel.String(version),
		attrParts.Int(parts),
	)
	return s
}

// Refused ends the span for a declined query.
//
// A refusal is not an error: the engine working correctly is the reason it
// happened. Marking it Error would make a dashboard of a healthy engine look
// like an incident and train everyone to ignore it.
func (s *Span) Refused(code string) {
	if s == nil {
		return
	}
	safe := safeCode(code, plan.IsRegisteredCode)
	s.span.SetAttributes(attrOutcome.String(OutcomeRefused), attrCode.String(safe))
	s.span.SetStatus(codes.Ok, "refused")
	s.span.End()
}

// Failed ends the span for a query that broke.
//
// The error text is recorded on the span but never on a metric. A warehouse
// error can quote the statement that failed, so this is the one place where
// something unbounded is written, and it is written to a trace an operator
// already has permission to read rather than to a label.
func (s *Span) Failed(err error) {
	if s == nil {
		return
	}
	s.span.SetAttributes(attrOutcome.String(OutcomeError))
	s.span.SetStatus(codes.Error, "failed")
	s.span.RecordError(err)
	s.span.End()
}

// Succeeded ends the span for a query that returned rows.
func (s *Span) Succeeded(rows int) {
	if s == nil {
		return
	}
	s.span.SetAttributes(attrOutcome.String(OutcomeAllowed), attrRows.Int(rows))
	s.span.SetStatus(codes.Ok, "")
	s.span.End()
}

// Finished ends a request span with an HTTP status.
//
// Only 5xx is an error. A 4xx is very often this engine refusing a question it
// cannot answer correctly, which is the product working, and a trace backend
// that paints those red teaches everyone to stop looking at red.
func (s *Span) Finished(status int) {
	if s == nil {
		return
	}
	s.span.SetAttributes(attrStatus.Int(status))
	if status >= 500 {
		s.span.SetStatus(codes.Error, "")
	} else {
		s.span.SetStatus(codes.Ok, "")
	}
	s.span.End()
}

// End closes a child span that carries no outcome of its own.
func (s *Span) End() {
	if s != nil {
		s.span.End()
	}
}

// Elapsed is how long the span has been open.
func (s *Span) Elapsed() time.Duration {
	if s == nil {
		return 0
	}
	return time.Since(s.start)
}

// RecordQuery files the metrics for one finished query.
//
// dialect and outcome are closed sets. code is bounded by safeCode. Nothing
// here varies with the caller or with what they asked for, so the number of
// series this produces is fixed by the build.
func RecordQuery(ctx context.Context, dialect, outcome, code string, d time.Duration, rows int) {
	attrs := []attribute.KeyValue{
		attrDialect.String(dialect),
		attrOutcome.String(outcome),
	}
	if outcome == OutcomeRefused {
		attrs = append(attrs, attrCode.String(safeCode(code, plan.IsRegisteredCode)))
		refusalCount.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
	set := metric.WithAttributes(attrs...)
	queryDuration.Record(ctx, d.Seconds(), set)
	if outcome == OutcomeAllowed {
		rowsReturned.Record(ctx, int64(rows), set)
	}
}

// RecordCompile files how long the part that never touches a warehouse took.
func RecordCompile(ctx context.Context, dialect string, d time.Duration) {
	compileTime.Record(ctx, d.Seconds(), metric.WithAttributes(attrDialect.String(dialect)))
}

// RecordBytesBilled files what the warehouse says a query cost.
//
// It is a counter rather than a gauge because the question people ask is what
// was spent over a window, and a gauge cannot answer that after the fact.
func RecordBytesBilled(ctx context.Context, dialect string, n int64) {
	if n <= 0 {
		return
	}
	bytesBilled.Add(ctx, n, metric.WithAttributes(attrDialect.String(dialect)))
}

// JobStarted and JobFinished move the in-flight gauge.
//
// They take no context deliberately. The decrement happens on whichever
// goroutine reached the terminal state, which is frequently not the one that
// started the job and often has no request context left, and a gauge that
// quietly stopped decrementing because a caller had nothing to pass is a gauge
// that climbs forever and gets someone paged at three in the morning.
func JobStarted()  { jobsRunning.Add(context.Background(), 1) }
func JobFinished() { jobsRunning.Add(context.Background(), -1) }
