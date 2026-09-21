// Package wire holds the response shapes that more than one process renders.
//
// It exists because an event payload is the API's wire type: the agent worker
// publishes a Run and a Step onto a Redis stream, and the browser reads the
// same JSON there that GET /api/runs/{id} returns. internal/api is the top of
// this project's dependency graph — nothing but cmd/api imports it — so having
// the worker import it would invert that direction and link gin into a binary
// that serves no HTTP.
//
// The conventions are internal/api's, unchanged. No omitempty on a nullable
// field, so every key is present in every response and the generated
// TypeScript types stay honest. Every timestamp goes through Time.
//
// Incident and Document stay in internal/api: moving them would be a rename
// with no consumer asking for it.
package wire

import (
	"encoding/json"

	"github.com/wpan36/incident_diag/internal/store"
)

// Run is the wire shape of one investigation.
//
// The budget columns are here because a run records the bounds it was created
// with: reading them back from configuration would make an old run
// uninterpretable the moment the environment changed.
//
// A RUNNING run's four counters read zero. ClaimRun resets them and only
// FinishRun writes them, so they mean something on a terminal run and nowhere
// else — a client that wants a live count counts step.completed events.
type Run struct {
	ID         string  `json:"id"`
	IncidentID string  `json:"incident_id"`
	Status     string  `json:"status"`
	StopReason *string `json:"stop_reason"`

	MaxSteps           int `json:"max_steps"`
	MaxToolCalls       int `json:"max_tool_calls"`
	MaxDurationSeconds int `json:"max_duration_seconds"`
	MaxPromptTokens    int `json:"max_prompt_tokens"`

	Model            string `json:"model"`
	StepCount        int    `json:"step_count"`
	ToolCallCount    int    `json:"tool_call_count"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`

	// FinalResult is finish's arguments as the model gave them, or null. It is
	// a pointer because an empty json.RawMessage marshals to invalid JSON
	// rather than to null.
	FinalResult *json.RawMessage `json:"final_result"`
	Error       *string          `json:"error"`

	// Attempts counts claims, so a run that has never been reclaimed reads 1.
	// A client uses it to tell one attempt's timeline from the next.
	Attempts   int   `json:"attempts"`
	StartedAt  *Time `json:"started_at"`
	FinishedAt *Time `json:"finished_at"`
	CreatedAt  Time  `json:"created_at"`
	UpdatedAt  Time  `json:"updated_at"`
}

// NewRun converts a stored run.
func NewRun(r store.Run) Run {
	return Run{
		ID:                 r.ID,
		IncidentID:         r.IncidentID,
		Status:             r.Status,
		StopReason:         r.StopReason,
		MaxSteps:           r.MaxSteps,
		MaxToolCalls:       r.MaxToolCalls,
		MaxDurationSeconds: r.MaxDurationSeconds,
		MaxPromptTokens:    r.MaxPromptTokens,
		Model:              r.Model,
		StepCount:          r.StepCount,
		ToolCallCount:      r.ToolCallCount,
		PromptTokens:       r.PromptTokens,
		CompletionTokens:   r.CompletionTokens,
		FinalResult:        nullJSON(r.FinalResult),
		Error:              r.Error,
		Attempts:           r.Attempts,
		StartedAt:          NullTime(r.StartedAt),
		FinishedAt:         NullTime(r.FinishedAt),
		CreatedAt:          NewTime(r.CreatedAt),
		UpdatedAt:          NewTime(r.UpdatedAt),
	}
}

// Step is the wire shape of one iteration of the agent loop.
//
// Neither ToolCall nor Evidence carries a run_id: it is the run being
// rendered. A step has at most one tool call, so nesting it is the schema's
// own shape rather than a join flattened by hand, and evidence nests under the
// step it cites because evidence.step_id is not nullable — which is also where
// a timeline renders a citation.
//
// The consequence is that a finish step's evidence points at earlier steps.
// The front end attaches each row to the step its step_id names.
type Step struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	StepNumber int             `json:"step_number"`
	ActionType string          `json:"action_type"`
	Action     json.RawMessage `json:"action"`

	ObservationSummary *string `json:"observation_summary"`
	ObservationBytes   int     `json:"observation_bytes"`
	Truncated          bool    `json:"truncated"`

	Status     string  `json:"status"`
	Error      *string `json:"error"`
	DurationMS int     `json:"duration_ms"`
	CreatedAt  Time    `json:"created_at"`

	ToolCall *ToolCall  `json:"tool_call"`
	Evidence []Evidence `json:"evidence"`
}

// ToolCall is the wire shape of one MCP invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	StepID    string          `json:"step_id"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
	Status    string          `json:"status"`

	ResultSummary *string `json:"result_summary"`
	ResultBytes   int     `json:"result_bytes"`
	Truncated     bool    `json:"truncated"`

	Error      *string `json:"error"`
	DurationMS int     `json:"duration_ms"`
	CreatedAt  Time    `json:"created_at"`
}

// Evidence is the wire shape of one citation the agent kept.
type Evidence struct {
	ID         string  `json:"id"`
	StepID     string  `json:"step_id"`
	ToolCallID *string `json:"tool_call_id"`
	SourceType string  `json:"source_type"`
	SourceRef  string  `json:"source_ref"`
	DocumentID *string `json:"document_id"`
	Summary    string  `json:"summary"`
	Note       *string `json:"note"`
	CreatedAt  Time    `json:"created_at"`
}

// RunDetail is a run with its whole timeline.
//
// It is the one place this project has two shapes for one entity, against the
// convention internal/api otherwise holds to. The alternative — a listing that
// carried every run's whole timeline — is worse.
type RunDetail struct {
	Run
	Steps []Step `json:"steps"`
}

// NewToolCall converts a stored tool call.
func NewToolCall(tc store.ToolCall) ToolCall {
	return ToolCall{
		ID:            tc.ID,
		StepID:        tc.StepID,
		ToolName:      tc.ToolName,
		Arguments:     tc.Arguments,
		Status:        tc.Status,
		ResultSummary: tc.Result,
		ResultBytes:   tc.ResultBytes,
		Truncated:     tc.Truncated,
		Error:         tc.Error,
		DurationMS:    tc.DurationMS,
		CreatedAt:     NewTime(tc.CreatedAt),
	}
}

// NewEvidence converts one stored piece of evidence.
func NewEvidence(e store.Evidence) Evidence {
	return Evidence{
		ID:         e.ID,
		StepID:     e.StepID,
		ToolCallID: e.ToolCallID,
		SourceType: e.SourceType,
		SourceRef:  e.SourceRef,
		DocumentID: e.DocumentID,
		Summary:    e.Summary,
		Note:       e.Note,
		CreatedAt:  NewTime(e.CreatedAt),
	}
}

// NewStep converts one stored step together with the rows that hang off it.
//
// toolCall is nil for a step that invoked none; evidence is empty except on a
// finish step. Both are the caller's to select, because the store lists them
// per run rather than per step.
func NewStep(st store.Step, toolCall *store.ToolCall, evidence []store.Evidence) Step {
	s := Step{
		ID:                 st.ID,
		RunID:              st.RunID,
		StepNumber:         st.StepNumber,
		ActionType:         st.ActionType,
		Action:             st.Action,
		ObservationSummary: st.Observation,
		ObservationBytes:   st.ObservationBytes,
		Truncated:          st.Truncated,
		Status:             st.Status,
		Error:              st.Error,
		DurationMS:         st.DurationMS,
		CreatedAt:          NewTime(st.CreatedAt),
		// Always an array, never null: a client that iterates it should not
		// have to check first.
		Evidence: make([]Evidence, 0, len(evidence)),
	}
	if toolCall != nil {
		tc := NewToolCall(*toolCall)
		s.ToolCall = &tc
	}
	for _, e := range evidence {
		s.Evidence = append(s.Evidence, NewEvidence(e))
	}
	return s
}

// NewRunDetail assembles a run's whole timeline from the four listings the
// store returns.
//
// The nesting is done here rather than in SQL because the store lists each
// table per run, and a join flattened by hand is what the response shape is
// trying not to be.
func NewRunDetail(r store.Run, steps []store.Step, toolCalls []store.ToolCall,
	evidence []store.Evidence) RunDetail {

	byStep := make(map[string]store.ToolCall, len(toolCalls))
	for _, tc := range toolCalls {
		byStep[tc.StepID] = tc
	}
	evidenceByStep := make(map[string][]store.Evidence, len(evidence))
	for _, e := range evidence {
		evidenceByStep[e.StepID] = append(evidenceByStep[e.StepID], e)
	}

	out := RunDetail{Run: NewRun(r), Steps: make([]Step, 0, len(steps))}
	for _, st := range steps {
		var tc *store.ToolCall
		if found, ok := byStep[st.ID]; ok {
			tc = &found
		}
		out.Steps = append(out.Steps, NewStep(st, tc, evidenceByStep[st.ID]))
	}
	return out
}

// nullJSON renders an absent JSON column as null rather than as the empty
// string, which is not valid JSON and which json.Marshal refuses.
func nullJSON(b json.RawMessage) *json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return &b
}
