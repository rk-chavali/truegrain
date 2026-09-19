// Package observe is the only place in this engine that talks to a telemetry
// backend, and the only place allowed to decide what may be said about a query.
//
// Telemetry is the easiest way to undo the care taken everywhere else. The
// audit log deliberately records no filter values, no compiled SQL and no
// result rows, because a filter value is a customer email or an account number
// and the SQL text embeds the schema. A span attribute or a log field is just
// as durable, is shipped to a third party, and is read by more people. So the
// same rule holds here, and rather than restating it as a convention this
// package enforces it by shape: nothing exported takes a value drawn from a
// request. The setters take counts, and the few strings they take are drawn
// from closed sets this build controls.
//
// Everything is off unless an endpoint is configured. An engine nobody is
// collecting from opens no connections and starts no exporters, and the
// no-op providers otel installs by default make every call in this package a
// few nanoseconds of nothing.
package observe

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// scope names every span and instrument this engine produces, so an operator
// collecting from several services can tell which is which.
const scope = "github.com/rk-chavali/truegrain"

// Config describes where telemetry goes. A zero Config disables it.
type Config struct {
	// Endpoint is an OTLP gRPC collector, as host:port. Empty disables
	// telemetry entirely.
	Endpoint string
	// Insecure sends to the collector without TLS. It exists because a
	// collector on the same host or inside the same pod is the common case,
	// and it has to be asked for rather than inferred.
	Insecure bool
	// Service names this deployment in the collector. Two engines serving
	// different workspaces are different services.
	Service string
	// Version is the build, for correlating a regression with a release.
	Version string
	// SampleRatio is the fraction of traces recorded, between 0 and 1.
	// Metrics are never sampled: a sampled counter is a wrong counter.
	SampleRatio float64
}

// Enabled reports whether telemetry will be sent anywhere.
func (c Config) Enabled() bool { return strings.TrimSpace(c.Endpoint) != "" }

// Shutdown flushes and closes the exporters. Callers must invoke it, or the
// last few seconds of telemetry before an exit are lost, which is exactly the
// telemetry someone is looking for after a crash.
type Shutdown func(context.Context) error

// Start configures the global trace and meter providers from cfg and returns
// the function that tears them down.
//
// With telemetry disabled it installs nothing and returns a no-op shutdown, so
// the caller does not branch. The providers otel starts with are already
// no-ops, which is what makes every recording site in this package free when
// nobody is collecting.
func Start(ctx context.Context, cfg Config) (Shutdown, error) {
	noop := func(context.Context) error { return nil }
	if !cfg.Enabled() {
		return noop, nil
	}
	if err := validateEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(defaulted(cfg.Service, "truegrain")),
		semconv.ServiceVersion(cfg.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("describing this service to the collector: %w", err)
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}

	traceExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("connecting traces to %s: %w", cfg.Endpoint, err)
	}
	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		// The trace exporter is already up; leaving it running would leak a
		// connection for a start that failed.
		_ = traceExp.Shutdown(ctx)
		return nil, fmt.Errorf("connecting metrics to %s: %w", cfg.Endpoint, err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		// ParentBased so a caller that is already tracing keeps one trace
		// across the boundary instead of this engine starting a second.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio(cfg.SampleRatio)))),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	// Without a propagator the engine starts a fresh trace for a request that
	// arrived inside someone else's, which is the difference between seeing
	// why an agent was slow and seeing that it was.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	// The instruments were built at init against the no-op provider and have
	// just delegated themselves onto the real one. Rebuilding them here would
	// orphan every reference the recording sites already hold.
	if bindErr != nil {
		return nil, fmt.Errorf("building instruments: %w", bindErr)
	}

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// validateEndpoint rejects what the exporter would otherwise accept quietly.
//
// otlptracegrpc wants host:port and treats a URL as a hostname, so a
// perfectly reasonable looking `http://collector:4317` resolves to nothing and
// the only symptom is telemetry that never arrives. Saying so at startup is
// the difference between a typo and an afternoon.
func validateEndpoint(endpoint string) error {
	e := strings.TrimSpace(endpoint)
	if strings.Contains(e, "://") {
		u, err := url.Parse(e)
		if err == nil && u.Host != "" {
			return fmt.Errorf("the OTLP endpoint is host:port, not a URL: use %q, and -otlp-insecure if it is plain gRPC", u.Host)
		}
		return fmt.Errorf("the OTLP endpoint is host:port, not a URL: %q", e)
	}
	if strings.ContainsAny(e, "/?#") {
		return fmt.Errorf("the OTLP endpoint is host:port with no path: %q", e)
	}
	return nil
}

func defaulted(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// ratio clamps rather than rejects. A sampling ratio out of range is a
// misconfiguration that should not stop an engine from serving queries, and
// both ends of the clamp are a defensible reading of what was meant.
func ratio(r float64) float64 {
	switch {
	case r <= 0:
		return 0
	case r >= 1:
		return 1
	default:
		return r
	}
}

// attrs is the allowlist. Nothing in this package builds an attribute.Key
// anywhere else, so the set of things that can be said about a query is this
// list and no more.
var (
	attrNamespace = attribute.Key("truegrain.namespace")
	attrModel     = attribute.Key("truegrain.model_version")
	attrDialect   = attribute.Key("truegrain.dialect")
	attrOutcome   = attribute.Key("truegrain.outcome")
	attrCode      = attribute.Key("truegrain.refusal_code")
	attrRoute     = attribute.Key("http.route")
	attrStatus    = attribute.Key("http.response.status_code")
	attrMetrics   = attribute.Key("truegrain.metric_count")
	attrDims      = attribute.Key("truegrain.dimension_count")
	attrFilters   = attribute.Key("truegrain.filter_count")
	attrParts     = attribute.Key("truegrain.part_count")
	attrRows      = attribute.Key("truegrain.row_count")
)

// Outcomes are a closed set, because they label a metric.
const (
	OutcomeAllowed = "allowed"
	OutcomeRefused = "refused"
	OutcomeError   = "error"
)

// safeCode bounds what can become a metric label.
//
// A refusal code is registered at init from a table this build controls, so a
// registered code is one of a few dozen known strings. Anything else reached
// here without passing through that table and could have been chosen by the
// caller, and an attacker who can add a label value can multiply the series a
// metrics backend stores until it falls over.
func safeCode(code string, registered func(string) bool) string {
	if code == "" {
		return ""
	}
	if registered(code) {
		return code
	}
	return "unregistered"
}
