//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/internal/wire"
)

// testRunLease is any value comfortably longer than a test: these tests drive
// the state machine by hand rather than waiting for a lease to expire.
const testRunLease = time.Hour

// liveStore opens a second handle on the same database the router uses.
//
// The run endpoints do not write steps, tool calls or evidence — the agent
// worker does — so a test that wants a timeline to fetch has to build one
// through the store directly.
func liveStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), config.Database{
		DSN: os.Getenv("TEST_MYSQL_DSN"), MaxOpenConns: 5, MaxIdleConns: 5, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newIncidentOverHTTP creates an incident the run tests can hang off.
func newIncidentOverHTTP(t *testing.T, h http.Handler) Incident {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/incidents", strings.NewReader(
		`{"title":"payment-service latency spike","description":"p99 above 2s","service":"`+
			uniqueService(t)+`"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating an incident: status = %d (%s)", rec.Code, rec.Body.String())
	}
	return decode[Incident](t, rec)
}

func postRun(t *testing.T, h http.Handler, incidentID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	// The body is empty, and the budget is not overridable: config.LoadAgent
	// ties the no-pruning invariant to configuration loading, and a request
	// that set its own max_steps would give that a second enforcement point.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/incidents/"+incidentID+"/runs", nil))
	return rec
}

func TestStartingARunRecordsItsBudgetAndEnqueuesIt(t *testing.T) {
	h, _, producer := liveRouterWithProducer(t)
	inc := newIncidentOverHTTP(t, h)

	rec := postRun(t, h, inc.ID)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	run := decode[wire.Run](t, rec)

	if got := rec.Header().Get("Location"); got != "/api/runs/"+run.ID {
		t.Errorf("Location = %q, want it to point at the new run", got)
	}
	if run.IncidentID != inc.ID {
		t.Errorf("incident_id = %q, want %q", run.IncidentID, inc.ID)
	}
	if run.Status != store.RunPending {
		t.Errorf("status = %q, want %q", run.Status, store.RunPending)
	}
	// Recorded on the row so the run stays interpretable after the
	// configuration changes.
	if run.MaxSteps != 8 || run.MaxToolCalls != 6 ||
		run.MaxDurationSeconds != 300 || run.MaxPromptTokens != 60000 {
		t.Errorf("budget not recorded: %+v", run)
	}
	if run.Model != "deepseek-chat" {
		t.Errorf("model = %q, want the API's configured model", run.Model)
	}
	// A RUNNING or PENDING run's counters read zero: only FinishRun writes
	// them.
	if run.StepCount != 0 || run.Attempts != 0 || run.StartedAt != nil {
		t.Errorf("a PENDING run is not blank: %+v", run)
	}

	msgs := producer.Messages()
	if len(msgs) != 1 {
		t.Fatalf("produced %d messages, want 1", len(msgs))
	}
	if msgs[0].Topic != mq.TopicAgentRuns || msgs[0].Key != run.ID {
		t.Errorf("produced %s/%s, want %s/%s", msgs[0].Topic, msgs[0].Key, mq.TopicAgentRuns, run.ID)
	}
	if msg, ok := msgs[0].Msg.(mq.RunMessage); !ok || msg.RunID != run.ID {
		t.Errorf("produced %+v, want a RunMessage for %s", msgs[0].Msg, run.ID)
	}
}

// The 409 comes from uniq_active_run refusing the insert, not from a
// read-then-write check that two requests could interleave.
func TestASecondRunWhileOneIsInFlightIsAConflict(t *testing.T) {
	h, _, _ := liveRouterWithProducer(t)
	inc := newIncidentOverHTTP(t, h)

	if rec := postRun(t, h, inc.ID); rec.Code != http.StatusCreated {
		t.Fatalf("the first run: status = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := postRun(t, h, inc.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("the second run: status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if body := decode[errorBody](t, rec); body.Error.Code != "conflict" {
		t.Errorf("error code = %q, want conflict", body.Error.Code)
	}
}

// A run against a missing incident violates fk_agent_runs_incident, and
// dbError does not classify a foreign-key failure as not-found — so without
// the read the client would get a 500 for its own mistake.
func TestARunAgainstAMissingIncidentIsNotFound(t *testing.T) {
	h, _, producer := liveRouterWithProducer(t)

	for _, incidentID := range []string{id.New(), "not-a-ulid"} {
		rec := postRun(t, h, incidentID)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST for incident %q: status = %d, want 404 (%s)",
				incidentID, rec.Code, rec.Body.String())
		}

		// The listing reads the incident for a smaller reason: an empty page
		// and a mistyped id must not look the same.
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/incidents/"+incidentID+"/runs", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET for incident %q: status = %d, want 404 (%s)",
				incidentID, rec.Code, rec.Body.String())
		}
	}

	if n := len(producer.Messages()); n != 0 {
		t.Errorf("produced %d messages for incidents that do not exist", n)
	}
}

// GET /api/runs/{id} is the snapshot a client reads before it subscribes, so
// it has to carry the whole timeline: every step, each step's tool call, and
// the evidence the run kept.
func TestGetRunRendersAWholeTimeline(t *testing.T) {
	h, _, _ := liveRouterWithProducer(t)
	st := liveStore(t)
	ctx := t.Context()

	inc := newIncidentOverHTTP(t, h)
	created := decode[wire.Run](t, postRun(t, h, inc.ID))

	if claimed, err := st.ClaimRun(ctx, created.ID, testRunLease); err != nil || !claimed {
		t.Fatalf("claiming the run: claimed=%v err=%v", claimed, err)
	}

	// A retrieval step, which writes no tool_calls row.
	retrieval, err := st.CreateStep(ctx, store.NewStep{
		RunID: created.ID, StepNumber: 1, ActionType: store.ActionRetrieve,
		Action:      json.RawMessage(`{"tool":"search_knowledge","arguments":{"query":"pool"}}`),
		Observation: "1. payment-service runbook — raise the pool size", Status: store.StepOK,
	})
	if err != nil {
		t.Fatalf("creating the retrieval step: %v", err)
	}

	// A tool step, which writes one.
	toolStep, err := st.CreateStep(ctx, store.NewStep{
		RunID: created.ID, StepNumber: 2, ActionType: store.ActionToolCall,
		Action:      json.RawMessage(`{"tool":"prometheus_query","arguments":{"query":"up"}}`),
		Observation: "pool_in_use 20/20", Status: store.StepOK,
	})
	if err != nil {
		t.Fatalf("creating the tool step: %v", err)
	}
	call, err := st.CreateToolCall(ctx, store.NewToolCall{
		RunID: created.ID, StepID: toolStep.ID, ToolName: "prometheus_query",
		Arguments: json.RawMessage(`{"query":"up"}`), Status: store.ToolCallOK,
		Result: "pool_in_use 20/20",
	})
	if err != nil {
		t.Fatalf("creating the tool call: %v", err)
	}

	// A finish step whose evidence cites the two steps before it.
	finish, err := st.CreateStep(ctx, store.NewStep{
		RunID: created.ID, StepNumber: 3, ActionType: store.ActionFinish,
		Action: json.RawMessage(`{"tool":"finish","arguments":{"root_cause":"the pool is saturated"}}`),
		Status: store.StepOK,
	})
	if err != nil {
		t.Fatalf("creating the finish step: %v", err)
	}
	for _, e := range []store.NewEvidence{
		{RunID: created.ID, StepID: retrieval.ID, SourceType: store.SourceRetrieval,
			SourceRef: "payment-service-runbook.md", Summary: "raise the pool size", Note: "the runbook says so"},
		{RunID: created.ID, StepID: toolStep.ID, ToolCallID: call.ID,
			SourceType: store.SourceTool, SourceRef: "prometheus_query", Summary: "pool_in_use 20/20"},
	} {
		if _, err := st.CreateEvidence(ctx, e); err != nil {
			t.Fatalf("creating evidence: %v", err)
		}
	}

	if _, err := st.FinishRun(ctx, created.ID, 1, store.RunOutcome{
		Status: store.RunSucceeded, StopReason: store.StopCompleted,
		FinalResult: json.RawMessage(`{"root_cause":"the pool is saturated"}`),
		StepCount:   3, ToolCallCount: 1, PromptTokens: 4200, CompletionTokens: 310,
	}); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	detail := decode[wire.RunDetail](t, rec)

	if detail.Status != store.RunSucceeded || detail.StepCount != 3 || detail.ToolCallCount != 1 {
		t.Errorf("the terminal row is not rendered: %+v", detail.Run)
	}
	if detail.FinalResult == nil {
		t.Error("final_result is null on a succeeded run")
	}
	if len(detail.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(detail.Steps))
	}

	// Retrieval costs a step, not a tool call.
	if detail.Steps[0].ToolCall != nil {
		t.Error("the retrieval step carries a tool call")
	}
	if tc := detail.Steps[1].ToolCall; tc == nil || tc.ID != call.ID {
		t.Errorf("step 2's tool call = %v, want %s", tc, call.ID)
	}
	// Evidence nests under the step it cites, which is where a timeline
	// renders a citation — so the finish step's own list is empty.
	if got := len(detail.Steps[0].Evidence); got != 1 {
		t.Errorf("step 1 carries %d pieces of evidence, want 1", got)
	}
	if got := len(detail.Steps[1].Evidence); got != 1 {
		t.Errorf("step 2 carries %d pieces of evidence, want 1", got)
	}
	if got := len(detail.Steps[2].Evidence); got != 0 {
		t.Errorf("the finish step carries %d of its own, want 0: its citations point at earlier steps", got)
	}
	if detail.Steps[2].ID != finish.ID {
		t.Errorf("the last step is %s, want the finish step %s", detail.Steps[2].ID, finish.ID)
	}

	// A missing run is a 404, not a 500.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/"+id.New(), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a missing run: status = %d, want 404", rec.Code)
	}
}

func TestListingAnIncidentsRuns(t *testing.T) {
	h, _, _ := liveRouterWithProducer(t)
	st := liveStore(t)
	inc := newIncidentOverHTTP(t, h)

	// An incident with no runs is an empty array, never null.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/incidents/"+inc.ID+"/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("an empty listing is %s, want an empty array", rec.Body.String())
	}

	// uniq_active_run allows a second run only once the first is terminal, so
	// each one is finished before the next is started.
	var ids []string
	for i := 0; i < 3; i++ {
		run := decode[wire.Run](t, postRun(t, h, inc.ID))
		ids = append(ids, run.ID)
		if claimed, err := st.ClaimRun(t.Context(), run.ID, testRunLease); err != nil || !claimed {
			t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
		}
		if _, err := st.FinishRun(t.Context(), run.ID, 1, store.RunOutcome{
			Status: store.RunSucceeded, StopReason: store.StopCompleted,
		}); err != nil {
			t.Fatalf("finishing: %v", err)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/incidents/"+inc.ID+"/runs?limit=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	page := decode[list[wire.Run]](t, rec)
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("first page = %d items, cursor %q; want 2 and a cursor", len(page.Items), page.NextCursor)
	}
	// Newest first.
	if page.Items[0].ID != ids[2] || page.Items[1].ID != ids[1] {
		t.Errorf("first page = %s, %s; want the two newest", page.Items[0].ID, page.Items[1].ID)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/incidents/"+inc.ID+"/runs?limit=2&cursor="+page.NextCursor, nil))
	second := decode[list[wire.Run]](t, rec)
	if len(second.Items) != 1 || second.Items[0].ID != ids[0] {
		t.Errorf("second page = %+v, want just the oldest run", second.Items)
	}
	if second.NextCursor != "" {
		t.Errorf("next_cursor = %q on the last page, want it absent", second.NextCursor)
	}
}
