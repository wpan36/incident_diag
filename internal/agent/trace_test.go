package agent

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/obs"
	"github.com/wpan36/incident_diag/internal/store"
)

// This is the only test in the package that installs a tracer provider. The
// global provider delegates once: tracers taken before the first SetTracerProvider
// — including this package's — keep pointing at whatever that first call
// installed, so a second installer elsewhere would record nothing.
func TestRunRecordsSpans(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))

	h := newHarness(t, generous(),
		llm.CallTurn("c1", ToolSearchKnowledge, map[string]any{"query": "payment latency"}),
		llm.CallTurn("c2", "prometheus_query", map[string]any{"query": "up"}),
		finishTurn(),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

	spans := exporter.GetSpans()
	var root tracetest.SpanStub
	var steps []tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case "agent.run":
			root = s
		case "agent.step":
			steps = append(steps, s)
		}
	}

	if root.Name == "" {
		t.Fatalf("no agent.run span; got %v", names(spans))
	}
	if root.Parent.IsValid() {
		t.Errorf("agent.run has a parent, but nothing above it started a span")
	}
	if len(steps) != 3 {
		t.Fatalf("agent.step spans = %d, want 3", len(steps))
	}

	// Every step hangs off the run, which is what makes one run one trace.
	for _, s := range steps {
		if s.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Errorf("a step's parent is %s, want the run span", s.Parent.SpanID())
		}
	}

	attrs := map[string]string{}
	for _, kv := range root.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs[string(obs.AttrRunID)] != h.run.ID {
		t.Errorf("run id on the root span = %q, want %q", attrs[string(obs.AttrRunID)], h.run.ID)
	}
	if attrs[string(obs.AttrIncidentID)] != h.run.Incident.ID {
		t.Errorf("incident id on the root span = %q", attrs[string(obs.AttrIncidentID)])
	}
	if attrs[string(obs.AttrStopReason)] != store.StopCompleted {
		t.Errorf("stop reason on the root span = %q, want %q",
			attrs[string(obs.AttrStopReason)], store.StopCompleted)
	}
}

func names(spans tracetest.SpanStubs) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}
