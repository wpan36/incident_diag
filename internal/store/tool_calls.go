package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/summary"
)

// Tool call outcomes. TIMEOUT is separate from ERROR because a tool that ran
// out of time and a tool that refused the arguments call for different
// responses, both from the agent and from whoever reads the trail afterwards.
const (
	ToolCallOK      = "OK"
	ToolCallError   = "ERROR"
	ToolCallTimeout = "TIMEOUT"
)

// ToolCall is one invocation of an MCP tool within a step.
type ToolCall struct {
	ID       string
	RunID    string
	StepID   string
	ToolName string
	// Arguments is what the agent passed, recorded so a run can be replayed by
	// hand.
	Arguments json.RawMessage
	Status    string
	Result    *string
	// ResultBytes is the length of the result before truncation, in bytes of
	// UTF-8.
	ResultBytes int
	Truncated   bool
	Error       *string
	DurationMS  int
	CreatedAt   time.Time
}

// NewToolCall is the caller-supplied part of a tool call. Result is the full
// text; the store truncates it.
type NewToolCall struct {
	RunID     string
	StepID    string
	ToolName  string
	Arguments json.RawMessage
	Status    string
	Result    string
	Error     string
	Duration  time.Duration
}

const toolCallColumns = `id, run_id, step_id, tool_name, arguments, status,
	result_summary, result_bytes, truncated, error, duration_ms, created_at`

func scanToolCall(s scanner) (ToolCall, error) {
	var tc ToolCall
	var args []byte
	err := s.Scan(&tc.ID, &tc.RunID, &tc.StepID, &tc.ToolName, &args, &tc.Status,
		&tc.Result, &tc.ResultBytes, &tc.Truncated, &tc.Error, &tc.DurationMS, &tc.CreatedAt)
	tc.Arguments = json.RawMessage(args)
	return tc, err
}

// CreateToolCall records one tool invocation.
func (s *Store) CreateToolCall(ctx context.Context, in NewToolCall) (ToolCall, error) {
	text, length, truncated := summary.Cap(in.Result)

	tc := ToolCall{
		ID:          id.New(),
		RunID:       in.RunID,
		StepID:      in.StepID,
		ToolName:    in.ToolName,
		Arguments:   in.Arguments,
		Status:      in.Status,
		ResultBytes: length,
		Truncated:   truncated,
		DurationMS:  int(in.Duration.Milliseconds()),
		CreatedAt:   now(),
	}
	if text != "" {
		tc.Result = &text
	}
	if in.Error != "" {
		tc.Error = &in.Error
	}

	const q = `INSERT INTO tool_calls
	           (id, run_id, step_id, tool_name, arguments, status, result_summary,
	            result_bytes, truncated, error, duration_ms, created_at)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, tc.ID, tc.RunID, tc.StepID, tc.ToolName,
		nullJSON(tc.Arguments), tc.Status, tc.Result, tc.ResultBytes, tc.Truncated,
		tc.Error, tc.DurationMS, tc.CreatedAt)
	if err != nil {
		return ToolCall{}, dbError(err, "insert tool call")
	}
	return tc, nil
}

// ListToolCallsByRun returns a run's tool calls in the order they happened.
// Like the step timeline it is bounded by the run's own max_tool_calls.
func (s *Store) ListToolCallsByRun(ctx context.Context, runID string) ([]ToolCall, error) {
	const q = `SELECT ` + toolCallColumns + ` FROM tool_calls WHERE run_id = ? ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, dbError(err, "list tool calls")
	}
	defer rows.Close()

	out := []ToolCall{}
	for rows.Next() {
		tc, err := scanToolCall(rows)
		if err != nil {
			return nil, dbError(err, "scan tool call")
		}
		out = append(out, tc)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "list tool calls")
	}
	return out, nil
}
