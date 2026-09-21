//go:build integration

// The loop against real dependencies, in two halves.
//
//	make up && make test-integration
//
// TestRunAgainstRealRetrievalAndTools drives a scripted model over a real
// Elasticsearch index and a real ops-mcp, so the retrieval rendering and the
// MCP result mapping are exercised against something other than a fake. It
// needs the infrastructure and an embedding key, both of which
// make test-integration already requires.
//
// TestLiveRunProducesADiagnosis adds the provider. It is the only test that
// proves native tool calling works against the model this project ships with,
// which nothing else covers until M33's provider-switch test, and it costs a
// billed completion — so it skips unless TEST_LLM_API_KEY is set on purpose.
//
// ops-mcp runs in process over httptest rather than in its container, the way
// internal/mcpclient's tests run it: the point is that the agent and a real
// MCP server agree, not that Docker works. Prometheus is stubbed, because the
// lab's numbers depend on which scenario was last run and a test that asserts
// on them would be asserting on the scenario.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/eval"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/ingest"
	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mcpclient"
	"github.com/wpan36/incident_diag/internal/opsmcp"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
)

const liveCorpusDir = "../../testdata/knowledge"

// livePrometheusBody is what the stub answers every query with: a
// payment-service p99 well above its budget, which is the scenario the corpus
// has runbooks for.
const livePrometheusBody = `{"status":"success","data":{"resultType":"vector","result":[
  {"metric":{"__name__":"http_request_duration_seconds","quantile":"0.99","job":"payment-service"},"value":[1758448800,"3.4"]},
  {"metric":{"__name__":"http_request_duration_seconds","quantile":"0.99","job":"checkout-service"},"value":[1758448800,"3.6"]}]}}`

// livePaymentLog is the log fixture read_service_logs serves.
const livePaymentLog = `{"time":"2026-09-21T10:00:00Z","level":"WARN","service":"payment-service","msg":"connection pool wait","in_use":20,"pool_size":20,"wait_ms":2100}
{"time":"2026-09-21T10:00:05Z","level":"ERROR","service":"payment-service","msg":"connection pool exhausted","in_use":20,"pool_size":20}
`

// A scripted model over real retrieval and a real tool server. It costs no
// completion, so it runs whenever the infrastructure is there.
func TestRunAgainstRealRetrievalAndTools(t *testing.T) {
	ctx, embedder, searcher, tools := liveDependencies(t)

	script := llm.NewFake(
		llm.CallTurn("c1", ToolSearchKnowledge, map[string]any{
			"query": "payment-service p99 latency rising and connection pool saturated",
		}),
		llm.CallTurn("c2", "read_service_logs", map[string]any{
			"service": "payment-service", "min_level": "WARN",
		}),
		finishTurn(Citation{Step: 2, Note: "the logs show the pool exhausted"}),
	)

	reporter := &recorder{}
	a := New(Deps{
		LLM:       script,
		Knowledge: &Knowledge{Embedder: embedder, Search: searcher},
		Tools:     tools,
		Report:    reporter.report,
		Logger:    log.Discard(),
	})

	outcome, err := a.Run(ctx, liveRun())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Status != store.RunSucceeded || outcome.StopReason != store.StopCompleted {
		t.Fatalf("outcome = %s/%s: %s", outcome.Status, outcome.StopReason, outcome.Error)
	}

	steps := stepsByNumber(reporter.recorded())

	// Real retrieval: the observation carries a real document id in brackets
	// and text the corpus actually contains.
	retrieval := steps[1]
	t.Logf("retrieval observation:\n%s", retrieval.Observation)
	if retrieval.ActionType != store.ActionRetrieve || retrieval.Status != store.StepOK {
		t.Fatalf("step 1 = %s/%s: %s", retrieval.ActionType, retrieval.Status, retrieval.Error)
	}
	if !strings.HasPrefix(retrieval.Observation, "[") {
		t.Errorf("the observation does not start with a document id: %q", firstLine(retrieval.Observation))
	}
	if !strings.Contains(retrieval.Observation, "pool") {
		t.Errorf("the corpus was not searched: %q", firstLine(retrieval.Observation))
	}

	// Real ops-mcp: a tool_calls row with the server's own status.
	logs := steps[2]
	if logs.ActionType != store.ActionToolCall || logs.ToolCall == nil ||
		logs.ToolCall.Status != store.ToolCallOK {
		t.Fatalf("step 2 = %s with tool call %+v", logs.ActionType, logs.ToolCall)
	}
	if !strings.Contains(logs.Observation, "pool exhausted") {
		t.Errorf("the log fixture was not read: %q", firstLine(logs.Observation))
	}

	// The citation resolved against the tool call the callback recorded.
	ev := steps[3].Evidence
	if len(ev) != 1 || ev[0].SourceType != store.SourceTool || ev[0].ToolCallID == "" {
		t.Errorf("evidence = %+v", ev)
	}
}

func TestLiveRunProducesADiagnosis(t *testing.T) {
	if os.Getenv("TEST_LLM_API_KEY") == "" {
		t.Skip("TEST_LLM_API_KEY is not set; skipping the live agent run")
	}

	llmCfg, err := config.LoadLLM()
	if err != nil {
		t.Fatalf("loading the LLM configuration: %v", err)
	}
	// The key the test was opted in with, so a run costs nothing unless it was
	// asked for explicitly.
	llmCfg.APIKey = os.Getenv("TEST_LLM_API_KEY")

	ctx, _, searcher, tools := liveDependencies(t)
	embedCfg, err := config.LoadEmbedding()
	if err != nil {
		t.Fatalf("loading the embedding configuration: %v", err)
	}
	embedder := embed.New(embedCfg, log.Discard())

	reporter := &recorder{}
	a := New(Deps{
		LLM:       llm.New(llmCfg, log.Discard()),
		Knowledge: &Knowledge{Embedder: embedder, Search: searcher},
		Tools:     tools,
		Report:    reporter.report,
		Logger:    log.Discard(),
	})

	outcome, err := a.Run(ctx, liveRun())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	steps := reporter.recorded()
	for _, s := range steps {
		t.Logf("step %d  %-10s %s", s.StepNumber, s.ActionType, firstLine(string(s.Action)))
	}
	t.Logf("outcome: %s/%s  steps=%d tool_calls=%d tokens=%d/%d",
		outcome.Status, outcome.StopReason, outcome.StepCount, outcome.ToolCallCount,
		outcome.PromptTokens, outcome.CompletionTokens)
	t.Logf("diagnosis: %s", outcome.FinalResult)

	if outcome.Status != store.RunSucceeded {
		t.Fatalf("status = %s (%s): %s", outcome.Status, outcome.StopReason, outcome.Error)
	}

	var final FinalResult
	if err := json.Unmarshal(outcome.FinalResult, &final); err != nil {
		t.Fatalf("the final result is not a finish call: %v", err)
	}
	if strings.TrimSpace(final.RootCause) == "" || strings.TrimSpace(final.AffectedService) == "" {
		t.Errorf("the diagnosis is empty: %+v", final)
	}

	// Native tool calling is a provider dependency, so the assertion that
	// matters is that the model actually used the tools rather than that it
	// named the right service — which is M31's and M32's job.
	var retrieved, called int
	for _, s := range steps {
		switch s.ActionType {
		case store.ActionRetrieve:
			retrieved++
		case store.ActionToolCall:
			called++
		case store.ActionNone:
			t.Errorf("step %d called no tool: %s", s.StepNumber, s.Error)
		}
	}
	if retrieved == 0 {
		t.Error("the run never searched the knowledge base")
	}
	if called == 0 {
		t.Error("the run never called an operational tool")
	}
	if outcome.PromptTokens == 0 {
		t.Error("the provider reported no token usage")
	}

	// The step numbers a real model cites are the ones the context labelled,
	// so its citations resolve to rows rather than being dropped. This is the
	// only test that can catch a context that does not say what those numbers
	// are — the scripted ones hand the model the right answer.
	finish := steps[len(steps)-1]
	if len(final.Evidence) > 0 && len(finish.Evidence) == 0 {
		t.Errorf("the model cited %d steps and none of them resolved: %+v",
			len(final.Evidence), final.Evidence)
	}
	for _, e := range finish.Evidence {
		if e.StepID == "" || e.SourceType == "" {
			t.Errorf("an evidence row resolved to nothing: %+v", e)
		}
	}
}

// liveDependencies is the setup both tests share: a real index built from the
// knowledge corpus and a real ops-mcp.
func liveDependencies(t *testing.T) (context.Context, embed.Embedder, *search.Client, *mcpclient.Client) {
	t.Helper()

	esURL := os.Getenv("TEST_ELASTICSEARCH_URL")
	if esURL == "" {
		t.Skip("TEST_ELASTICSEARCH_URL is not set; skipping the agent integration tests")
	}
	if os.Getenv("EMBEDDING_API_KEY") == "" {
		t.Skip("EMBEDDING_API_KEY is not set; skipping the agent integration tests")
	}

	embedCfg, err := config.LoadEmbedding()
	if err != nil {
		t.Fatalf("loading the embedding configuration: %v", err)
	}
	embedder := embed.New(embedCfg, log.Discard())

	ctx := context.Background()
	return ctx, embedder, liveIndex(ctx, t, esURL, embedder), liveToolServer(t)
}

// liveRun is the incident both tests investigate: the checkout latency the
// corpus has two competing runbooks for.
func liveRun() Run {
	return Run{
		ID: id.New(),
		Incident: Incident{
			ID:    id.New(),
			Title: "checkout-service p99 latency above 3 seconds",
			Description: "Since about 10:00 UTC, POST /orders on checkout-service is returning " +
				"504s and its p99 latency is above 3 seconds. Nothing was deployed today.",
			Service:   "checkout-service",
			CreatedAt: time.Now().UTC(),
		},
		Budget: Budget{
			MaxSteps: 6, MaxToolCalls: 4,
			MaxRunDuration: 3 * time.Minute, MaxPromptTokens: 60000,
		},
	}
}

// liveIndex builds a throwaway index from the knowledge corpus.
func liveIndex(ctx context.Context, t *testing.T, esURL string, embedder embed.Embedder) *search.Client {
	t.Helper()

	alias := "agent_" + strings.ToLower(id.New())
	searcher, err := search.New(config.Search{URL: esURL, IndexAlias: alias}, log.Discard())
	if err != nil {
		t.Fatalf("opening the search client: %v", err)
	}
	if err := searcher.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	t.Cleanup(func() { deleteLiveIndices(t, esURL, alias) })

	manifest, err := eval.LoadManifest(liveCorpusDir)
	if err != nil {
		t.Fatal(err)
	}
	chunking := ingest.ChunkOptions{TargetTokens: 400, MaxPerDocument: 2000}

	for _, doc := range manifest.Documents {
		raw, err := os.ReadFile(filepath.Join(liveCorpusDir, doc.Source))
		if err != nil {
			t.Fatalf("reading %s: %v", doc.Source, err)
		}
		chunks, err := ingest.ChunkDocument(store.FormatMarkdown, raw, chunking)
		if err != nil {
			t.Fatalf("chunking %s: %v", doc.Source, err)
		}

		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Content
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			t.Fatalf("embedding %s: %v", doc.Source, err)
		}

		documentID := id.New()
		indexed := make([]search.Chunk, len(chunks))
		for i, c := range chunks {
			indexed[i] = search.Chunk{
				DocumentID:   documentID,
				ChunkID:      search.ChunkID(documentID, c.Index),
				ChunkIndex:   c.Index,
				Service:      doc.Service,
				DocumentType: doc.DocumentType,
				Source:       doc.Source,
				HeadingPath:  c.HeadingPath,
				Content:      c.Content,
				Embedding:    vectors[i],
				IndexedAt:    time.Now().UTC(),
			}
		}
		if err := searcher.IndexChunks(ctx, indexed); err != nil {
			t.Fatalf("indexing %s: %v", doc.Source, err)
		}
	}
	return searcher
}

// liveToolServer starts a real ops-mcp over httptest and connects to it.
func liveToolServer(t *testing.T) *mcpclient.Client {
	t.Helper()

	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, livePrometheusBody)
	}))
	t.Cleanup(prometheus.Close)

	logRoot := t.TempDir()
	for service, body := range map[string]string{
		"payment-service":  livePaymentLog,
		"checkout-service": `{"time":"2026-09-21T10:00:06Z","level":"ERROR","service":"checkout-service","msg":"payment call timed out","status":504}` + "\n",
	} {
		if err := os.WriteFile(filepath.Join(logRoot, service+".log"), []byte(body), 0o600); err != nil {
			t.Fatalf("writing the %s log fixture: %v", service, err)
		}
	}

	cfg := config.OpsMCP{
		PrometheusURL:     prometheus.URL,
		ProbeTargets:      map[string]string{"payment-service": prometheus.URL},
		LogRoot:           logRoot,
		LogServices:       []string{"payment-service", "checkout-service"},
		PrometheusTimeout: 10 * time.Second,
		MaxRange:          6 * time.Hour,
		MinStep:           15 * time.Second,
		MaxSeries:         50,
		ProbeTimeout:      5 * time.Second,
		ProbeBodyBytes:    2 << 10,
		LogTimeout:        10 * time.Second,
		MaxLogLines:       1000,
	}

	srv := httptest.NewServer(opsmcp.New(cfg, log.Discard()).Handler())
	t.Cleanup(srv.Close)

	c, err := mcpclient.Connect(context.Background(), srv.URL+"/mcp", 15*time.Second, log.Discard())
	if err != nil {
		t.Fatalf("connecting to ops-mcp: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func deleteLiveIndices(t *testing.T, esURL, alias string) {
	t.Helper()
	target := strings.TrimSuffix(esURL, "/") + "/" + alias + "," + alias + "_v1?ignore_unavailable=true"
	req, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		t.Logf("cleanup: building the delete request: %v", err)
		return
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("cleanup: deleting %s: %v", alias, err)
		return
	}
	res.Body.Close()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
