package wire

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/store"
)

func ptr[T any](v T) *T { return &v }

func TestNewRunRendersAnAbsentFinalResultAsNull(t *testing.T) {
	// json.RawMessage is the empty slice on a RUNNING run, and marshalling
	// that directly produces invalid JSON rather than null.
	body, err := json.Marshal(NewRun(store.Run{Status: store.RunRunning}))
	if err != nil {
		t.Fatalf("marshalling a running run: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the response is not valid JSON: %v (%s)", err, body)
	}
	for _, key := range []string{"final_result", "error", "stop_reason", "started_at", "finished_at"} {
		v, present := decoded[key]
		if !present {
			t.Errorf("%s is absent; a nullable field renders as an explicit null", key)
		}
		if v != nil {
			t.Errorf("%s = %v, want null", key, v)
		}
	}
}

func TestNewRunKeepsTheBudgetTheRunWasCreatedWith(t *testing.T) {
	r := NewRun(store.Run{
		MaxSteps: 8, MaxToolCalls: 6, MaxDurationSeconds: 300, MaxPromptTokens: 60000,
		Model: "deepseek-chat", FinalResult: json.RawMessage(`{"root_cause":"pool"}`),
	})
	if r.MaxSteps != 8 || r.MaxToolCalls != 6 || r.MaxDurationSeconds != 300 || r.MaxPromptTokens != 60000 {
		t.Errorf("budget not carried: %+v", r)
	}
	if r.FinalResult == nil || string(*r.FinalResult) != `{"root_cause":"pool"}` {
		t.Errorf("FinalResult = %v, want the arguments as they were given", r.FinalResult)
	}
}

// A run's timeline is four listings, and the nesting is done in Go rather than
// in SQL. The finish step is the interesting case: its evidence points at the
// steps that produced it, not at itself.
func TestNewRunDetailNestsToolCallsAndEvidenceUnderTheirSteps(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	run := store.Run{ID: "run", Status: store.RunSucceeded}

	steps := []store.Step{
		{ID: "step-1", RunID: "run", StepNumber: 1, ActionType: store.ActionRetrieve,
			Action: json.RawMessage(`{"tool":"search_knowledge"}`), Status: store.StepOK, CreatedAt: now},
		{ID: "step-2", RunID: "run", StepNumber: 2, ActionType: store.ActionToolCall,
			Action: json.RawMessage(`{"tool":"prometheus_query"}`), Status: store.StepOK, CreatedAt: now},
		{ID: "step-3", RunID: "run", StepNumber: 3, ActionType: store.ActionFinish,
			Action: json.RawMessage(`{"tool":"finish"}`), Status: store.StepOK, CreatedAt: now},
	}
	// Retrieval writes no tool_calls row; only step 2 has one.
	toolCalls := []store.ToolCall{
		{ID: "call-1", RunID: "run", StepID: "step-2", ToolName: "prometheus_query",
			Arguments: json.RawMessage(`{}`), Status: store.ToolCallOK, CreatedAt: now},
	}
	// Written by the finish step, but citing the steps that produced them.
	evidence := []store.Evidence{
		{ID: "ev-1", RunID: "run", StepID: "step-1", SourceType: store.SourceRetrieval,
			SourceRef: "runbook.md", DocumentID: ptr("doc-1"), Summary: "raise the pool", CreatedAt: now},
		{ID: "ev-2", RunID: "run", StepID: "step-2", ToolCallID: ptr("call-1"),
			SourceType: store.SourceTool, SourceRef: "prometheus_query", Summary: "saturated", CreatedAt: now},
	}

	detail := NewRunDetail(run, steps, toolCalls, evidence)
	if len(detail.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(detail.Steps))
	}

	if detail.Steps[0].ToolCall != nil {
		t.Error("a retrieval step carries a tool call; retrieval writes no tool_calls row")
	}
	if tc := detail.Steps[1].ToolCall; tc == nil || tc.ID != "call-1" {
		t.Errorf("step 2's tool call = %v, want call-1", tc)
	}
	if detail.Steps[2].ToolCall != nil {
		t.Error("the finish step carries a tool call")
	}

	// Each row attaches to the step its step_id names, which is where a
	// timeline renders a citation.
	if got := detail.Steps[0].Evidence; len(got) != 1 || got[0].ID != "ev-1" {
		t.Errorf("step 1's evidence = %v, want just ev-1", got)
	}
	if got := detail.Steps[1].Evidence; len(got) != 1 || got[0].ID != "ev-2" {
		t.Errorf("step 2's evidence = %v, want just ev-2", got)
	}
	if got := detail.Steps[2].Evidence; len(got) != 0 {
		t.Errorf("the finish step's own evidence = %v, want none: its citations point at earlier steps", got)
	}
}

func TestAStepWithNoEvidenceRendersAnEmptyArray(t *testing.T) {
	body, err := json.Marshal(NewStep(store.Step{ID: "step-1"}, nil, nil))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var decoded struct {
		Evidence *[]Evidence `json:"evidence"`
		ToolCall *ToolCall   `json:"tool_call"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the response is not valid JSON: %v (%s)", err, body)
	}
	if decoded.Evidence == nil {
		t.Error("evidence is null; it must always be an array")
	}
	if decoded.ToolCall != nil {
		t.Error("tool_call is set on a step that made none")
	}
}

// Neither nested type carries a run_id: it is the run being rendered.
func TestNestedTypesDoNotRepeatTheRunID(t *testing.T) {
	for name, v := range map[string]any{
		"tool_call": NewToolCall(store.ToolCall{RunID: "run", Arguments: json.RawMessage(`{}`)}),
		"evidence":  NewEvidence(store.Evidence{RunID: "run"}),
	} {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshalling %s: %v", name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		if _, present := decoded["run_id"]; present {
			t.Errorf("%s carries a run_id", name)
		}
	}
}
