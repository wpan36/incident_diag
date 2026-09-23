// Package obs sets up tracing for every binary in this project.
//
// Tracing is off unless OTEL_EXPORTER_OTLP_ENDPOINT is set. Every test, every
// `go run` and CI have to work without a collector running, and an exporter
// retrying against a closed port would be a worse failure than no tracing: it
// would add latency and log noise to the very requests it is meant to explain.
package obs

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// The version has to match the one resource.Default() carries, or
	// resource.Merge refuses with a schema conflict.
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Version identifies this build in a trace. It is a constant because nothing
// here reads build info, and a wrong version is worse than an absent one.
const Version = "0.1.0"

// exportTimeout bounds flushing spans at shutdown. Shutdown is already racing
// the container runtime's grace period, and unsent spans are the least
// important thing in flight.
const exportTimeout = 5 * time.Second

// tracesPath is where an OTLP/HTTP collector receives spans.
const tracesPath = "/v1/traces"

// Setup installs the global tracer provider and propagator.
//
// The returned shutdown flushes pending spans; register it with the binary's
// shutdown.Group. It is safe to call when tracing is disabled.
func Setup(ctx context.Context, cfg Config, logger *slog.Logger) (func(context.Context) error, error) {
	// The propagator is installed either way. Without it, a disabled build
	// would strip the trace context from messages it forwards, breaking the
	// traces of the processes that do have tracing on.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		logger.Info("tracing is disabled", "reason", "OTEL_EXPORTER_OTLP_ENDPOINT is not set")
		return func(context.Context) error { return nil }, nil
	}

	endpoint, err := tracesURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("obs: open the trace exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.Service),
		semconv.ServiceVersion(Version),
	))
	if err != nil {
		return nil, fmt.Errorf("obs: build the trace resource: %w", err)
	}

	// Always-on sampling: a lab run is a handful of traces, and sampling would
	// hide the one being looked at.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(provider)
	logger.Info("tracing is enabled", "endpoint", cfg.Endpoint)

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, exportTimeout)
		defer cancel()
		return provider.Shutdown(ctx)
	}, nil
}

// tracesURL turns the configured base URL into the one the OTLP/HTTP exporter
// posts to.
//
// OTEL_EXPORTER_OTLP_ENDPOINT is a base — http://127.0.0.1:4318 — and the
// signal's path is appended to it. WithEndpointURL does not do that: it uses
// the URL as given, so a base would post to / and every export would come back
// 404 with the process none the wiser.
func tracesURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("obs: parse the trace endpoint %q: %w", base, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = tracesPath
	}
	return u.String(), nil
}

// Tracer returns a tracer for a package. It resolves through the global
// provider on every call, so a package can hold one at construction without
// caring whether Setup has run yet.
func Tracer(name string) trace.Tracer {
	return otel.Tracer("github.com/wpan36/incident_diag/" + name)
}

// Attributes used in more than one package, so that a query written against
// one of them finds every span that carries it.
const (
	AttrRunID      = attribute.Key("incident_diag.run_id")
	AttrIncidentID = attribute.Key("incident_diag.incident_id")
	AttrDocumentID = attribute.Key("incident_diag.document_id")
	AttrStepNumber = attribute.Key("incident_diag.step_number")
	AttrToolName   = attribute.Key("incident_diag.tool")
	AttrStopReason = attribute.Key("incident_diag.stop_reason")
	AttrModel      = attribute.Key("incident_diag.model")
)
