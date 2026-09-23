package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/wpan36/incident_diag/internal/log"
)

func TestSetupWithNoEndpointIsAWorkingNoOp(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{Service: "test"}, log.Discard())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	// A span still has to be creatable: every instrumented call site does it
	// unconditionally, and a nil tracer would panic in production the moment
	// tracing was switched off.
	ctx, span := Tracer("test").Start(context.Background(), "unit")
	span.SetAttributes(AttrRunID.String("01ABC"))
	span.End()
	if ctx == nil {
		t.Fatal("Start returned a nil context")
	}
	if span.SpanContext().IsValid() {
		t.Error("a disabled provider produced a recording span")
	}
}

func TestSetupInstallsThePropagatorEvenWhenDisabled(t *testing.T) {
	if _, err := Setup(context.Background(), Config{Service: "test"}, log.Discard()); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// The regression this guards: a disabled build that strips trace context
	// from the messages it forwards breaks the traces of the processes that do
	// have tracing on. The propagator has to be installed either way.
	carrier := propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)

	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("the propagator did not extract a valid span context")
	}
	if sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id = %s", sc.TraceID())
	}

	out := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, out)
	if out["traceparent"] == "" {
		t.Error("the propagator injected nothing")
	}
}

func TestSetupRejectsAnUnusableEndpoint(t *testing.T) {
	// A bad URL is a startup failure rather than silently disabled tracing:
	// the operator asked for tracing and has to be told they did not get it.
	if _, err := Setup(context.Background(), Config{Endpoint: "://nonsense", Service: "test"}, log.Discard()); err == nil {
		t.Fatal("a malformed endpoint was accepted")
	}
}

func TestTracesURL(t *testing.T) {
	// OTEL_EXPORTER_OTLP_ENDPOINT is a base URL. WithEndpointURL uses what it
	// is given, so without this every export posts to / and comes back 404
	// with the process none the wiser.
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:4318", "http://127.0.0.1:4318/v1/traces"},
		{"http://127.0.0.1:4318/", "http://127.0.0.1:4318/v1/traces"},
		{"https://collector.example:443", "https://collector.example:443/v1/traces"},
		// An explicit path is honoured: that is how a collector behind a
		// gateway prefix is reached.
		{"http://gw/otlp/v1/traces", "http://gw/otlp/v1/traces"},
	} {
		got, err := tracesURL(tc.in)
		if err != nil {
			t.Fatalf("tracesURL(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("tracesURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetupExportsToACollector(t *testing.T) {
	// The enabled path end to end, against a stand-in for Jaeger. It covers
	// two failures the disabled-path tests cannot see: a resource whose
	// semconv version conflicts with resource.Default()'s, which makes Setup
	// return an error, and an export that goes to the wrong path.
	posted := make(chan string, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case posted <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	shutdown, err := Setup(context.Background(), Config{Endpoint: collector.URL, Service: "test"}, log.Discard())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_, span := Tracer("test").Start(context.Background(), "unit")
	span.End()

	// Shutdown flushes, so by the time it returns the export has happened.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case path := <-posted:
		if path != tracesPath {
			t.Errorf("the exporter posted to %q, want %q", path, tracesPath)
		}
	default:
		t.Fatal("no span reached the collector")
	}
}
