package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mcpclient"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
)

// recorder is a fake ReportStep. It hands back counterfeit ids, which is all
// the loop needs to resolve citations, and keeps every step for the
// assertions.
type recorder struct {
	mu    sync.Mutex
	steps []Step

	// FailAt makes the callback fail on that step number. Zero never fails.
	FailAt int

	// Hook runs before each step is recorded. It is how a test cancels a run
	// from inside it.
	Hook func(Step)
}

func (r *recorder) report(_ context.Context, s Step) (StepRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.Hook != nil {
		r.Hook(s)
	}
	if r.FailAt == s.StepNumber {
		return StepRef{}, fmt.Errorf("the database is down")
	}
	r.steps = append(r.steps, s)

	ref := StepRef{StepID: fmt.Sprintf("step-%d", s.StepNumber)}
	if s.ToolCall != nil {
		ref.ToolCallID = fmt.Sprintf("call-%d", s.StepNumber)
	}
	return ref, nil
}

func (r *recorder) recorded() []Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Step(nil), r.steps...)
}

// fakeTools is a ToolServer.
type fakeTools struct {
	mu      sync.Mutex
	tools   []mcpclient.Tool
	results map[string]mcpclient.Result
	errs    map[string]error
	calls   []string
}

func newFakeTools(names ...string) *fakeTools {
	f := &fakeTools{results: map[string]mcpclient.Result{}, errs: map[string]error{}}
	for _, n := range names {
		f.tools = append(f.tools, mcpclient.Tool{
			Name: n, Description: n, InputSchema: json.RawMessage(`{"type":"object"}`),
		})
		f.results[n] = mcpclient.Result{Text: n + " says everything is fine", Status: mcpclient.StatusOK}
	}
	return f
}

func (f *fakeTools) Tools() []mcpclient.Tool { return f.tools }

func (f *fakeTools) Call(_ context.Context, name string, _ json.RawMessage) (mcpclient.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if err := f.errs[name]; err != nil {
		return mcpclient.Result{}, err
	}
	return f.results[name], nil
}

func (f *fakeTools) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeSearch is a ChunkSearcher.
type fakeSearch struct {
	hits []search.Result
	err  error

	// last is the query the agent actually asked for, which is how the k
	// default and the filters are checked.
	last search.Query
}

func (f *fakeSearch) Search(_ context.Context, _ []float32, q search.Query) ([]search.Result, error) {
	f.last = q
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func hit(documentID, source, heading, content string, score float64) search.Result {
	return search.Result{
		Chunk: search.Chunk{
			DocumentID: documentID, Source: source, HeadingPath: heading, Content: content,
		},
		Score: score,
	}
}

// harness is one configured agent plus the fakes behind it.
type harness struct {
	agent    *Agent
	llm      *llm.Fake
	tools    *fakeTools
	searcher *fakeSearch
	reporter *recorder
	run      Run
}

func newHarness(t *testing.T, budget Budget, turns ...llm.Turn) *harness {
	t.Helper()

	h := &harness{
		llm:      llm.NewFake(turns...),
		tools:    newFakeTools("prometheus_query", "read_service_logs"),
		searcher: &fakeSearch{hits: []search.Result{hit("doc-1", "payment-runbook.md", "Symptoms", "the pool saturates", 0.83)}},
		reporter: &recorder{},
	}
	h.run = Run{
		ID: "run-1",
		Incident: Incident{
			ID: "inc-1", Title: "checkout is slow", Description: "p99 above 3s",
			Service: "checkout-service", CreatedAt: time.Unix(1700000000, 0).UTC(),
		},
		Budget: budget,
	}
	h.agent = New(Deps{
		LLM:       h.llm,
		Knowledge: &Knowledge{Embedder: &embed.Fake{}, Search: h.searcher},
		Tools:     h.tools,
		Report:    h.reporter.report,
		Logger:    log.Discard(),
	})
	return h
}

// generous is a budget no test hits by accident.
func generous() Budget {
	return Budget{MaxSteps: 8, MaxToolCalls: 12, MaxRunDuration: time.Minute, MaxPromptTokens: 60000}
}

// finishArgs is a well-formed finish call.
func finishArgs(citations ...Citation) map[string]any {
	if citations == nil {
		citations = []Citation{}
	}
	return map[string]any{
		"root_cause":       "the payment-service connection pool is saturated",
		"affected_service": "payment-service",
		"next_actions":     []string{"raise PAYMENT_POOL_SIZE"},
		"evidence":         citations,
	}
}

func finishTurn(citations ...Citation) llm.Turn {
	return llm.CallTurn("call_finish", ToolFinish, finishArgs(citations...))
}

// stepsByNumber indexes what the recorder saw.
func stepsByNumber(steps []Step) map[int]Step {
	out := map[int]Step{}
	for _, s := range steps {
		out[s.StepNumber] = s
	}
	return out
}

func assertOutcome(t *testing.T, got store.RunOutcome, status, stopReason string) {
	t.Helper()
	if got.Status != status || got.StopReason != stopReason {
		t.Errorf("outcome = %s/%s, want %s/%s (error: %s)",
			got.Status, got.StopReason, status, stopReason, got.Error)
	}
}

func assertContains(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s = %q, want it to contain %q", what, got, want)
	}
}

// errAlwaysFails is what a fake dependency returns when a test needs it down.
var errAlwaysFails = errors.New("this dependency is down")
