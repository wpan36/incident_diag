package obs

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics live on the default registry rather than on one passed into every
// constructor.
//
// internal/lab does the opposite — a registry per service — because its tests
// build two whole services in one process and have to read them apart. The
// platform binaries are one process each, and threading a prometheus.Registerer
// through llm, search, mq and agent would be plumbing that buys nothing here.
// Tests assert against the collector directly with testutil.
//
// Naming follows what internal/lab already exports: seconds for durations,
// _total for counters, and the label set closed by construction. `tool` is an
// ops-mcp tool name, `topic` a Kafka topic, `status` and `stop_reason` the
// store's own constants — none of them can grow with traffic.

// Duration buckets.
//
// The defaults stop at 10s, which is where most of this system's interesting
// latency starts: a run is bounded at five minutes, an LLM call routinely takes
// ten to thirty seconds. So durations that are a whole operation get their own
// scale, and only retrieval — which is one Elasticsearch query — keeps the
// defaults.
var (
	// runBuckets reach past MaxRunDuration's default of five minutes.
	runBuckets = []float64{5, 15, 30, 60, 90, 120, 180, 240, 300, 420}

	// callBuckets cover one LLM call or one tool call.
	callBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}
)

var (
	// AgentRuns counts finished runs. A cancelled run is deliberately absent:
	// it has no terminal state (ADR 0009), and counting it would double-count
	// the restart that follows.
	AgentRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_runs_total",
		Help: "Agent runs that reached a terminal state, by status and stop reason.",
	}, []string{"status", "stop_reason"})

	// AgentRunDuration is wall time inside agent.Run, which excludes the
	// claim and the Kafka hop before it.
	AgentRunDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "agent_run_duration_seconds",
		Help:    "Wall time of one agent run.",
		Buckets: runBuckets,
	})

	// AgentSteps counts recorded steps. status is the step's own: OK means it
	// produced a usable action and an observation, not that the observation
	// was good news, so a refused tool is an OK step.
	AgentSteps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_steps_total",
		Help: "Agent steps recorded, by action type and step status.",
	}, []string{"action_type", "status"})

	// AgentToolCalls counts tool calls including the refusals, which is what
	// makes "how often does the agent call a tool wrongly" a query rather than
	// a log search.
	AgentToolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_tool_calls_total",
		Help: "Tool calls made by the agent, by tool and result status.",
	}, []string{"tool", "status"})

	AgentToolDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_tool_duration_seconds",
		Help:    "Duration of one tool call, as the agent saw it.",
		Buckets: callBuckets,
	}, []string{"tool"})

	// LLMRequestDuration covers every attempt of one Chat, so a retried call
	// is one observation rather than three.
	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_request_duration_seconds",
		Help:    "Duration of one chat completion, retries included.",
		Buckets: callBuckets,
	}, []string{"outcome"})

	// LLMTokens is what a token budget invites: cost.
	LLMTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_tokens_total",
		Help: "Tokens reported by the provider, by kind.",
	}, []string{"kind"})

	RetrievalDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "rag_retrieval_duration_seconds",
		Help:    "Duration of one kNN query against Elasticsearch.",
		Buckets: prometheus.DefBuckets,
	})

	// EmbeddingDuration is one Embed call, which is one batch on the query
	// side and several on the ingestion side. It is the other half of what a
	// search costs, and the half that leaves the machine.
	EmbeddingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "rag_embedding_duration_seconds",
		Help:    "Duration of one embedding request, retries included.",
		Buckets: callBuckets,
	})

	// ReconcileEnqueued counts rows the reconciler re-enqueued. A number that
	// is anything but zero means the dual write is leaking, which is the one
	// thing the reconciler exists to make visible rather than to hide.
	ReconcileEnqueued = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reconcile_enqueued_total",
		Help: "Rows re-enqueued by the reconciler, by target and category.",
	}, []string{"target", "category"})

	ReconcileProduceFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reconcile_produce_failures_total",
		Help: "Re-enqueues the reconciler could not produce, by target.",
	}, []string{"target"})

	// KafkaProcessingDuration is the handler's, not the poll's: it is what
	// answers whether the ingestion or the agent side is the backlog.
	KafkaProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kafka_processing_duration_seconds",
		Help:    "Duration of one message handler, by topic and outcome.",
		Buckets: runBuckets,
	}, []string{"topic", "outcome"})
)

// Outcome labels shared by the metrics above, so that a query written against
// one of them reads the same against the others.
const (
	OutcomeSuccess = "success"
	OutcomeError   = "error"
)

// OutcomeOf turns an error into the label the histograms use.
func OutcomeOf(err error) string {
	if err != nil {
		return OutcomeError
	}
	return OutcomeSuccess
}

// Token kinds, matching the provider's own split.
const (
	TokensPrompt     = "prompt"
	TokensCompletion = "completion"
)

// MetricsHandler serves the default registry.
//
// The workers mount it on their own listener; cmd/api mounts it on the router
// it already has.
func MetricsHandler() http.Handler { return promhttp.Handler() }

// NewMetricsServer builds the listener a worker exposes /metrics and /healthz
// on.
//
// /healthz comes along because a worker with no liveness endpoint is one
// Compose cannot restart on a hang, and the listener is there anyway. It
// touches nothing: readiness belongs to cmd/api, which is the process a client
// actually talks to.
//
// The timeouts are constants rather than configuration. This server answers two
// fixed, local endpoints, and HTTP_SERVER_* is shaped for an API that takes
// uploads and holds SSE connections open.
func NewMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", MetricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
