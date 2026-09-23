package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/mcpclient"
	"github.com/wpan36/incident_diag/internal/store"
)

// The ordinary shape of a run: retrieve, check a metric, diagnose.
func TestRunConverges(t *testing.T) {
	h := newHarness(t, generous(),
		llm.CallTurn("c1", ToolSearchKnowledge, map[string]any{"query": "payment latency"}),
		llm.CallTurn("c2", "prometheus_query", map[string]any{"query": "up"}),
		finishTurn(Citation{Step: 1, Note: "the runbook names the pool", DocumentID: "doc-1"},
			Citation{Step: 2, Note: "the metric confirms it"}),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

	if outcome.StepCount != 3 {
		t.Errorf("StepCount = %d, want 3", outcome.StepCount)
	}
	// Retrieval writes no tool_calls row, so only the Prometheus call counts.
	if outcome.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", outcome.ToolCallCount)
	}
	if outcome.PromptTokens != 300 || outcome.CompletionTokens != 60 {
		t.Errorf("usage = %d/%d, want 300/60", outcome.PromptTokens, outcome.CompletionTokens)
	}

	steps := stepsByNumber(h.reporter.recorded())
	if steps[1].ActionType != store.ActionRetrieve || steps[1].ToolCall != nil {
		t.Errorf("step 1 = %s with tool call %+v", steps[1].ActionType, steps[1].ToolCall)
	}
	if steps[2].ActionType != store.ActionToolCall || steps[2].ToolCall == nil {
		t.Fatalf("step 2 = %s with tool call %+v", steps[2].ActionType, steps[2].ToolCall)
	}
	if steps[2].ToolCall.Status != store.ToolCallOK {
		t.Errorf("step 2 tool call status = %s", steps[2].ToolCall.Status)
	}
	if steps[3].ActionType != store.ActionFinish {
		t.Errorf("step 3 = %s", steps[3].ActionType)
	}

	// final_result holds the finish arguments as they were given.
	var final FinalResult
	if err := json.Unmarshal(outcome.FinalResult, &final); err != nil {
		t.Fatalf("FinalResult is not the finish arguments: %v", err)
	}
	if final.AffectedService != "payment-service" {
		t.Errorf("AffectedService = %q", final.AffectedService)
	}

	// The citations resolved to the ids the callback returned, and the
	// retrieval's cited document survived because it was among that step's
	// hits.
	ev := steps[3].Evidence
	if len(ev) != 2 {
		t.Fatalf("evidence = %+v, want 2 rows", ev)
	}
	if ev[0].SourceType != store.SourceRetrieval || ev[0].StepID != "step-1" ||
		ev[0].DocumentID != "doc-1" || ev[0].SourceRef != "payment-runbook.md" {
		t.Errorf("retrieval evidence = %+v", ev[0])
	}
	if ev[0].ToolCallID != "" {
		t.Errorf("a retrieval citation must not carry a tool_call_id: %+v", ev[0])
	}
	if ev[1].SourceType != store.SourceTool || ev[1].StepID != "step-2" ||
		ev[1].ToolCallID != "call-2" || ev[1].SourceRef != "prometheus_query" {
		t.Errorf("tool evidence = %+v", ev[1])
	}
}

// Each bound stops the run from starting new work and buys one final call, so
// the run ends with a real partial diagnosis. A bounded run is SUCCEEDED, with
// the stop reason saying which bound: lifecycle and outcome are separate
// columns for exactly this.
func TestBoundsForceAFinishAndSucceed(t *testing.T) {
	tests := []struct {
		name       string
		budget     Budget
		turns      []llm.Turn
		stopReason string
		wantSteps  int
		wantCalls  int
	}{
		{
			name:   "max steps",
			budget: Budget{MaxSteps: 2, MaxToolCalls: 12, MaxRunDuration: time.Minute, MaxPromptTokens: 60000},
			turns: []llm.Turn{
				llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"}),
				llm.CallTurn("c2", "read_service_logs", map[string]any{"service": "payment-service"}),
				finishTurn(),
			},
			stopReason: store.StopMaxSteps,
			// The forced finish occupies a step number, so the count exceeds
			// max_steps by one and the timeline shows why the run ended.
			wantSteps: 3,
			wantCalls: 3,
		},
		{
			name:   "max tool calls",
			budget: Budget{MaxSteps: 8, MaxToolCalls: 1, MaxRunDuration: time.Minute, MaxPromptTokens: 60000},
			turns: []llm.Turn{
				llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"}),
				finishTurn(),
			},
			stopReason: store.StopMaxToolCalls,
			wantSteps:  2,
			wantCalls:  2,
		},
		{
			name: "prompt tokens",
			// One token is a ceiling the first assembled prompt already
			// exceeds, so the forced finish is the whole run.
			budget:     Budget{MaxSteps: 8, MaxToolCalls: 12, MaxRunDuration: time.Minute, MaxPromptTokens: 1},
			turns:      []llm.Turn{finishTurn()},
			stopReason: store.StopTokenBudget,
			wantSteps:  1,
			wantCalls:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.budget, tc.turns...)

			outcome, err := h.agent.Run(context.Background(), h.run)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertOutcome(t, outcome, store.RunSucceeded, tc.stopReason)
			if outcome.StepCount != tc.wantSteps {
				t.Errorf("StepCount = %d, want %d", outcome.StepCount, tc.wantSteps)
			}
			if h.llm.Calls() != tc.wantCalls {
				t.Errorf("LLM calls = %d, want %d", h.llm.Calls(), tc.wantCalls)
			}
			if len(outcome.FinalResult) == 0 {
				t.Error("a bounded run must still produce a diagnosis")
			}

			// The final call offers finish and nothing else, and demands it.
			last := h.llm.Requests()[len(h.llm.Requests())-1]
			if len(last.Tools) != 1 || last.Tools[0].Name != ToolFinish {
				t.Errorf("the forced call offered %d tools", len(last.Tools))
			}
			if last.ToolChoice != llm.ChoiceRequired {
				t.Errorf("ToolChoice = %q, want %q", last.ToolChoice, llm.ChoiceRequired)
			}
			assertContains(t, last.Messages[len(last.Messages)-1].Content, tc.stopReason,
				"the forced call's last message")
		})
	}
}

// MaxRunDuration is wall-clock and checked between steps, so a run overruns it
// by the step that was already in flight plus the forced finish. It is not a
// deadline on the context: the forced finish would then run on an expired one
// and could never succeed.
func TestRunDurationBoundStopsAfterTheStepInFlight(t *testing.T) {
	h := newHarness(t,
		Budget{MaxSteps: 8, MaxToolCalls: 12, MaxRunDuration: 30 * time.Millisecond, MaxPromptTokens: 60000},
		llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"}),
		finishTurn(),
	)
	h.llm.Delay = 40 * time.Millisecond

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopTimeout)
	if outcome.StepCount != 2 {
		t.Errorf("StepCount = %d, want 2: one step plus the forced finish", outcome.StepCount)
	}
}

// A run with no diagnosis is not a success, and the forced call is not
// retried: a second one would overrun the bound again for the same reason the
// first did not work.
func TestForcedFinishThatFailsEndsTheRunFailed(t *testing.T) {
	h := newHarness(t,
		Budget{MaxSteps: 1, MaxToolCalls: 12, MaxRunDuration: time.Minute, MaxPromptTokens: 60000},
		llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"}),
		llm.TextTurn("I cannot tell you"),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunFailed, store.StopError)
	assertContains(t, outcome.Error, store.StopMaxSteps, "the run's error")
	if h.llm.Calls() != 2 {
		t.Errorf("LLM calls = %d, want 2: the forced finish is not retried", h.llm.Calls())
	}

	// It still occupies a step number, so the timeline shows what happened.
	steps := h.reporter.recorded()
	last := steps[len(steps)-1]
	if last.StepNumber != 2 || last.ActionType != store.ActionNone || last.Status != store.StepError {
		t.Errorf("last step = %+v", last)
	}
}

// Cancellation restarts a run; it does not end it. Nothing terminal is
// written, so the row stays RUNNING and the lease reclaims it (ADR 0009).
func TestCancellationStopsWithoutATerminalWrite(t *testing.T) {
	h := newHarness(t, generous(),
		llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"}),
		finishTurn(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	h.reporter.Hook = func(s Step) {
		if s.StepNumber == 1 {
			cancel()
		}
	}

	outcome, err := h.agent.Run(ctx, h.run)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if outcome.Status != "" || outcome.StopReason != "" || len(outcome.FinalResult) != 0 {
		t.Errorf("a cancelled run returned an outcome to write: %+v", outcome)
	}
	if h.llm.Calls() != 1 {
		t.Errorf("LLM calls = %d, want 1: there is no final call after cancellation", h.llm.Calls())
	}
}

// A response with no tool call is a step, not a lost turn: "the model stopped
// calling tools" is the single most useful thing an evaluation can count.
func TestProseIsRecordedAsNoneAndRetriedOnce(t *testing.T) {
	h := newHarness(t, generous(),
		llm.TextTurn("I think it is the connection pool"),
		finishTurn(),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

	steps := stepsByNumber(h.reporter.recorded())
	none := steps[1]
	if none.ActionType != store.ActionNone || none.Status != store.StepError {
		t.Errorf("step 1 = %s/%s, want none/ERROR", none.ActionType, none.Status)
	}
	if string(none.Action) != "{}" {
		t.Errorf("a none step's action = %s, want {}", none.Action)
	}
	// The retry is an ordinary next iteration: the instruction is a user
	// message the builder renders, not a second code path.
	retry := h.llm.Requests()[1].Messages
	// -2, not -1: the budget line is always the last message.
	last := retry[len(retry)-2].Content
	assertContains(t, last, "exactly one of the tools", "the retry's last message")
	// The instruction carries why the step was unusable and which step number
	// it spent, so the retry can correct the actual mistake.
	assertContains(t, last, "Step 1 was not usable", "the retry's last message")
	assertContains(t, last, "called no tool", "the retry's last message")
}

func TestTwoProseResponsesInARowFailTheRun(t *testing.T) {
	h := newHarness(t, generous(), llm.TextTurn("one"), llm.TextTurn("two"))

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunFailed, store.StopError)
	if outcome.StepCount != 2 {
		t.Errorf("StepCount = %d, want 2", outcome.StepCount)
	}
}

// agent_steps records one action per step, so only the first call is executed
// — and only it is replayed, because an assistant message whose other calls
// have no paired reply is the history some providers reject.
func TestSeveralToolCallsInOneResponseExecuteOnlyTheFirst(t *testing.T) {
	h := newHarness(t, generous(),
		llm.CallsTurn(
			llm.NewToolCall("c1", "prometheus_query", map[string]any{"query": "up"}),
			llm.NewToolCall("c2", "read_service_logs", map[string]any{"service": "payment-service"}),
		),
		finishTurn(),
	)

	if _, err := h.agent.Run(context.Background(), h.run); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called := h.tools.called(); len(called) != 1 || called[0] != "prometheus_query" {
		t.Errorf("tools called = %v, want only prometheus_query", called)
	}

	// The replayed history carries one call and one reply.
	// The budget line is last, so the call and its reply are the two before it.
	replayed := h.llm.Requests()[1].Messages
	assistant := replayed[len(replayed)-3]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "c1" {
		t.Errorf("assistant message = %+v", assistant)
	}
	if replayed[len(replayed)-2].ToolCallID != "c1" {
		t.Errorf("tool reply = %+v", replayed[len(replayed)-2])
	}
}

// An invented tool name is recorded the way ops-mcp answers an unknown service
// name, so the model recovers the same way — and it spends a tool call,
// because an agent that keeps guessing must not run unbounded.
func TestAnUnknownToolIsRefusedAndCostsAToolCall(t *testing.T) {
	h := newHarness(t, generous(),
		llm.CallTurn("c1", "restart_the_database", map[string]any{}),
		finishTurn(),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)
	if outcome.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", outcome.ToolCallCount)
	}
	if len(h.tools.called()) != 0 {
		t.Errorf("an unknown tool reached the server: %v", h.tools.called())
	}

	step := stepsByNumber(h.reporter.recorded())[1]
	if step.Status != store.StepOK || step.ToolCall == nil ||
		step.ToolCall.Status != store.ToolCallRefused {
		t.Fatalf("step 1 = %+v", step)
	}
	// The observation lists the valid names, which is what lets the model
	// recover on its next step.
	assertContains(t, step.Observation, "prometheus_query", "the refusal")
	assertContains(t, step.Observation, ToolFinish, "the refusal")
}

// Four outcomes, one of which the loop treats differently. Refused means the
// tool ran and declined, which the model can fix; a non-OK status means the
// dependency did not answer, which it cannot. The step is OK in all of them:
// its status says whether it produced a usable action and an observation, not
// whether the observation was good news.
func TestToolFailuresLeaveTheStepOKAndTheRunGoing(t *testing.T) {
	tests := []struct {
		name       string
		result     mcpclient.Result
		err        error
		wantStatus string
	}{
		{
			name:       "refused",
			result:     mcpclient.Result{Text: "unknown service 'ghost'", Refused: true, Status: mcpclient.StatusOK, Note: "unknown service"},
			wantStatus: store.ToolCallRefused,
		},
		{
			name:       "error",
			result:     mcpclient.Result{Text: "prometheus is unreachable", Status: mcpclient.StatusError, Note: "dial failed"},
			wantStatus: store.ToolCallError,
		},
		{
			name:       "timeout",
			result:     mcpclient.Result{Text: "no answer in 10s", Status: mcpclient.StatusTimeout, Note: "timed out"},
			wantStatus: store.ToolCallTimeout,
		},
		{
			// A server that marks a result as an error must never have it
			// recorded as something the model merely got wrong.
			name:       "refused and error disagree",
			result:     mcpclient.Result{Text: "both", Refused: true, Status: mcpclient.StatusError},
			wantStatus: store.ToolCallError,
		},
		{
			name:       "the call itself fails",
			err:        errors.New("the transport is broken"),
			wantStatus: store.ToolCallError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, generous(),
				llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"}),
				finishTurn(),
			)
			h.tools.results["prometheus_query"] = tc.result
			h.tools.errs["prometheus_query"] = tc.err

			outcome, err := h.agent.Run(context.Background(), h.run)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// A failing dependency is a reason to look elsewhere, not a reason
			// to stop investigating.
			assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

			step := stepsByNumber(h.reporter.recorded())[1]
			if step.Status != store.StepOK {
				t.Errorf("step status = %s, want OK", step.Status)
			}
			if step.ToolCall == nil || step.ToolCall.Status != tc.wantStatus {
				t.Errorf("tool call = %+v, want status %s", step.ToolCall, tc.wantStatus)
			}
			if outcome.ToolCallCount != 1 {
				t.Errorf("ToolCallCount = %d, want 1: a failed call still spent one", outcome.ToolCallCount)
			}
		})
	}
}

// A failed retrieval is not a failed run either, and it leaves ERROR on a step
// meaning only one thing: the model called no tool.
func TestAFailedRetrievalLeavesTheStepOK(t *testing.T) {
	h := newHarness(t, generous(),
		llm.CallTurn("c1", ToolSearchKnowledge, map[string]any{"query": "payment latency"}),
		finishTurn(),
	)
	h.searcher.err = errors.New("elasticsearch is down")

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

	step := stepsByNumber(h.reporter.recorded())[1]
	if step.ActionType != store.ActionRetrieve || step.Status != store.StepOK {
		t.Errorf("step 1 = %s/%s", step.ActionType, step.Status)
	}
	if step.ToolCall != nil {
		t.Error("a retrieval must not write a tool_calls row")
	}
	assertContains(t, step.Error, "elasticsearch is down", "the step's error")
	if outcome.ToolCallCount != 0 {
		t.Errorf("ToolCallCount = %d, want 0", outcome.ToolCallCount)
	}
}

// A finish with nothing in root_cause is no diagnosis at all, so it is treated
// as no tool call: required is the only mechanism behind the prompt's demand
// that a claim rest on an observation.
func TestABlankRootCauseIsNotAFinish(t *testing.T) {
	blank := finishArgs()
	blank["root_cause"] = "   "

	h := newHarness(t, generous(),
		llm.CallTurn("c1", ToolFinish, blank),
		finishTurn(),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

	step := stepsByNumber(h.reporter.recorded())[1]
	if step.ActionType != store.ActionNone || step.Status != store.StepError {
		t.Errorf("step 1 = %s/%s, want none/ERROR", step.ActionType, step.Status)
	}
	assertContains(t, step.Error, "root_cause", "the step's error")
}

// Arguments that will not decode reach two JSON columns and no tool could act
// on them, so they are refused before either.
func TestArgumentsThatAreNotJSONBecomeANoneStep(t *testing.T) {
	h := newHarness(t, generous(),
		llm.CallTurn("c1", "prometheus_query", "{not json"),
		finishTurn(),
	)

	if _, err := h.agent.Run(context.Background(), h.run); err != nil {
		t.Fatalf("Run: %v", err)
	}
	step := stepsByNumber(h.reporter.recorded())[1]
	if step.ActionType != store.ActionNone {
		t.Errorf("step 1 = %s, want none", step.ActionType)
	}
	if len(h.tools.called()) != 0 {
		t.Errorf("unparseable arguments reached the tool server: %v", h.tools.called())
	}
}

// A wrong citation should not discard a correct diagnosis.
func TestInventedCitationsAreDroppedNotFatal(t *testing.T) {
	h := newHarness(t, generous(),
		llm.TextTurn("thinking out loud"), // becomes step 1, a none
		llm.CallTurn("c1", ToolSearchKnowledge, map[string]any{"query": "payment latency"}),
		finishTurn(
			Citation{Step: 99, Note: "a step that never happened"},
			Citation{Step: 1, Note: "a step that did nothing"},
			Citation{Step: 2, Note: "a document this step did not return", DocumentID: "doc-invented"},
		),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

	steps := stepsByNumber(h.reporter.recorded())
	ev := steps[3].Evidence
	if len(ev) != 1 {
		t.Fatalf("evidence = %+v, want only the retrieval citation", ev)
	}
	// An invented document leaves document_id empty and keeps the row; the
	// source_ref falls back to the query, and the summary to the whole
	// observation.
	if ev[0].DocumentID != "" {
		t.Errorf("an invented document_id was kept: %+v", ev[0])
	}
	if ev[0].SourceRef != ToolSearchKnowledge+"(payment latency)" {
		t.Errorf("SourceRef = %q", ev[0].SourceRef)
	}
	assertContains(t, ev[0].Summary, "the pool saturates", "the evidence summary")
}

// A citation the loop cannot even decode is dropped like one that names a step
// that never happened: json.Unmarshal is all or nothing, so reading the
// evidence array as a whole would let one bad item discard a correct
// diagnosis.
func TestAnUndecodableCitationDoesNotDiscardTheDiagnosis(t *testing.T) {
	// step as a string, and next_actions as a bare string where the schema
	// asks for an array — both things a model does and neither of which says
	// anything about whether the diagnosis is right.
	args := `{"root_cause":"the pool is saturated","affected_service":"payment-service",` +
		`"next_actions":"raise the pool size",` +
		`"evidence":[{"step":"1","note":"unreadable"},{"step":1,"note":"readable"}]}`

	h := newHarness(t, generous(),
		llm.CallTurn("c1", ToolSearchKnowledge, map[string]any{"query": "payment latency"}),
		llm.CallTurn("call_finish", ToolFinish, args),
	)

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunSucceeded, store.StopCompleted)

	steps := stepsByNumber(h.reporter.recorded())
	if steps[2].ActionType != store.ActionFinish {
		t.Fatalf("step 2 = %s, want finish", steps[2].ActionType)
	}
	ev := steps[2].Evidence
	if len(ev) != 1 || ev[0].Note != "readable" {
		t.Errorf("evidence = %+v, want only the citation that decoded", ev)
	}
	// final_result still holds the arguments as they were given, unrepaired.
	assertContains(t, string(outcome.FinalResult), `"step":"1"`, "final_result")
}

// A step that cannot be persisted means the audit trail is already wrong, so
// the run ends rather than continuing to write against it.
func TestACallbackFailureFailsTheRun(t *testing.T) {
	h := newHarness(t, generous(),
		llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"}),
		finishTurn(),
	)
	h.reporter.FailAt = 1

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunFailed, store.StopError)
	assertContains(t, outcome.Error, "could not be recorded", "the run's error")
	if h.llm.Calls() != 1 {
		t.Errorf("LLM calls = %d, want 1", h.llm.Calls())
	}
}

// An unreachable model is not something a run can investigate around: there is
// nothing to decide the next step with.
func TestAnUnreachableModelFailsTheRun(t *testing.T) {
	h := newHarness(t, generous(), llm.Turn{Err: errors.New("the provider is down")})

	outcome, err := h.agent.Run(context.Background(), h.run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertOutcome(t, outcome, store.RunFailed, store.StopError)
	assertContains(t, outcome.Error, "the provider is down", "the run's error")
	if outcome.StepCount != 0 {
		t.Errorf("StepCount = %d, want 0", outcome.StepCount)
	}
}

// The model's reasoning has nowhere to go in this schema, which is deliberate.
func TestTheModelsProseIsNeverPersistedOrReplayed(t *testing.T) {
	const secret = "my private chain of thought"
	turn := llm.CallTurn("c1", "prometheus_query", map[string]any{"query": "up"})
	turn.Response.Content = secret

	h := newHarness(t, generous(), turn, finishTurn())

	if _, err := h.agent.Run(context.Background(), h.run); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, s := range h.reporter.recorded() {
		if strings.Contains(string(s.Action)+s.Observation+s.Error, secret) {
			t.Errorf("step %d recorded the model's reasoning: %+v", s.StepNumber, s)
		}
	}
	for i, req := range h.llm.Requests() {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, secret) {
				t.Errorf("request %d replayed the model's reasoning: %+v", i, m)
			}
		}
	}
}

// The tool list is one list from three sources, and it is built once.
func TestTheToolListIsSearchThenTheServersThenFinish(t *testing.T) {
	h := newHarness(t, generous(), finishTurn())

	if _, err := h.agent.Run(context.Background(), h.run); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var names []string
	for _, tool := range h.llm.Requests()[0].Tools {
		names = append(names, tool.Name)
	}
	want := []string{ToolSearchKnowledge, "prometheus_query", "read_service_logs", ToolFinish}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", names, want)
	}
}
