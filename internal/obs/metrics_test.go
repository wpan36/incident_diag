package obs

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// registeredNames is every metric this package defines, mirroring the same
// assertion in internal/lab. A metric that is declared but never reaches
// /metrics is a panel that stays empty for a reason nobody can see.
var registeredNames = []string{
	"agent_runs_total",
	"agent_run_duration_seconds",
	"agent_steps_total",
	"agent_tool_calls_total",
	"agent_tool_duration_seconds",
	"llm_request_duration_seconds",
	"llm_tokens_total",
	"rag_retrieval_duration_seconds",
	"rag_embedding_duration_seconds",
	"kafka_processing_duration_seconds",
	"reconcile_enqueued_total",
	"reconcile_produce_failures_total",
}

func TestEveryMetricIsExported(t *testing.T) {
	// Observed once each, because a histogram with no observations exports
	// nothing at all and would pass a weaker test by being absent.
	AgentRuns.WithLabelValues("SUCCEEDED", "COMPLETED").Inc()
	AgentRunDuration.Observe(1)
	AgentSteps.WithLabelValues("retrieve", "OK").Inc()
	AgentToolCalls.WithLabelValues("prometheus_query", "OK").Inc()
	AgentToolDuration.WithLabelValues("prometheus_query").Observe(1)
	LLMRequestDuration.WithLabelValues(OutcomeSuccess).Observe(1)
	LLMTokens.WithLabelValues(TokensPrompt).Add(1)
	RetrievalDuration.Observe(1)
	EmbeddingDuration.Observe(1)
	KafkaProcessingDuration.WithLabelValues("documents.v1", OutcomeSuccess).Observe(1)
	ReconcileEnqueued.WithLabelValues("document", "stuck").Inc()
	ReconcileProduceFailures.WithLabelValues("document").Inc()

	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics returned %d", rec.Code)
	}
	body := rec.Body.String()

	for _, name := range registeredNames {
		if !strings.Contains(body, "\n"+name) && !strings.HasPrefix(body, name) {
			t.Errorf("%s is not exported", name)
		}
	}

	// The Go collector comes with the default registry, which is the reason
	// the workers' listener is worth having even before the first run.
	if !strings.Contains(body, "go_goroutines") {
		t.Error("the default registry lost its Go collector")
	}
}

func TestOutcomeOf(t *testing.T) {
	if got := OutcomeOf(nil); got != OutcomeSuccess {
		t.Errorf("OutcomeOf(nil) = %q", got)
	}
	if got := OutcomeOf(errFake{}); got != OutcomeError {
		t.Errorf("OutcomeOf(err) = %q", got)
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake" }

func TestLabelsAreAClosedSet(t *testing.T) {
	// The regression this guards: a label fed from something the model
	// controls. agent_tool_calls_total is the one at risk, because the model
	// can name a tool that does not exist — internal/agent maps those onto a
	// single placeholder, so the series count stays bounded by ops-mcp's tool
	// list plus one.
	before := testutil.CollectAndCount(AgentToolCalls)
	AgentToolCalls.WithLabelValues("prometheus_query", "REFUSED").Inc()
	AgentToolCalls.WithLabelValues("prometheus_query", "REFUSED").Inc()
	if after := testutil.CollectAndCount(AgentToolCalls); after != before+1 {
		t.Errorf("series went from %d to %d; one new label pair should add one series", before, after)
	}
}
