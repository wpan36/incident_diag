package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/summary"
)

// Action types: what the agent decided to do on a step.
//
// ActionNone is a response that called no tool at all. It is persisted rather
// than dropped because these tables exist to make a run auditable, and "the
// model stopped calling tools" is the single most useful thing an evaluation
// can count. The column is VARCHAR rather than an ENUM precisely so this set
// can grow without a migration.
const (
	ActionRetrieve = "retrieve"
	ActionToolCall = "tool_call"
	ActionFinish   = "finish"
	ActionNone     = "none"
)

// Step outcomes.
const (
	StepOK    = "OK"
	StepError = "ERROR"
)

// Step is one iteration of the agent loop.
//
// Action holds the structured decision the model returned, never the reasoning
// behind it. No chain-of-thought is persisted anywhere in this schema.
type Step struct {
	ID          string
	RunID       string
	StepNumber  int
	ActionType  string
	Action      json.RawMessage
	Observation *string
	// ObservationBytes is the length of the observation before truncation, in
	// bytes of UTF-8.
	ObservationBytes int
	Truncated        bool
	Status           string
	Error            *string
	DurationMS       int
	CreatedAt        time.Time
}

// NewStep is the caller-supplied part of a step.
//
// Observation is the full text: the store truncates it and records the original
// length and the flag, so those three can never disagree.
type NewStep struct {
	RunID       string
	StepNumber  int
	ActionType  string
	Action      json.RawMessage
	Observation string
	Status      string
	Error       string
	Duration    time.Duration
}

const stepColumns = `id, run_id, step_number, action_type, action, observation_summary,
	observation_bytes, truncated, status, error, duration_ms, created_at`

func scanStep(s scanner) (Step, error) {
	var st Step
	var action []byte
	err := s.Scan(&st.ID, &st.RunID, &st.StepNumber, &st.ActionType, &action, &st.Observation,
		&st.ObservationBytes, &st.Truncated, &st.Status, &st.Error, &st.DurationMS, &st.CreatedAt)
	st.Action = json.RawMessage(action)
	return st, err
}

// CreateStep records one step of a run.
//
// A duplicate (run_id, step_number) violates uniq_step_number and surfaces as
// an internal error rather than a conflict: nothing a client does can cause it,
// so it is a bug in the agent loop and belongs in the logs as a 500.
func (s *Store) CreateStep(ctx context.Context, in NewStep) (Step, error) {
	text, length, truncated := summary.Cap(in.Observation)

	st := Step{
		ID:               id.New(),
		RunID:            in.RunID,
		StepNumber:       in.StepNumber,
		ActionType:       in.ActionType,
		Action:           in.Action,
		ObservationBytes: length,
		Truncated:        truncated,
		Status:           in.Status,
		DurationMS:       int(in.Duration.Milliseconds()),
		CreatedAt:        now(),
	}
	if text != "" {
		st.Observation = &text
	}
	if in.Error != "" {
		st.Error = &in.Error
	}

	const q = `INSERT INTO agent_steps
	           (id, run_id, step_number, action_type, action, observation_summary,
	            observation_bytes, truncated, status, error, duration_ms, created_at)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, st.ID, st.RunID, st.StepNumber, st.ActionType,
		nullJSON(st.Action), st.Observation, st.ObservationBytes, st.Truncated,
		st.Status, st.Error, st.DurationMS, st.CreatedAt)
	if err != nil {
		return Step{}, dbError(err, "insert agent step")
	}
	return st, nil
}

// ListStepsByRun returns a run's steps in the order they happened.
//
// It is not paginated: a run's step count is bounded by its own max_steps, so
// the whole timeline is a bounded result by construction.
func (s *Store) ListStepsByRun(ctx context.Context, runID string) ([]Step, error) {
	const q = `SELECT ` + stepColumns + ` FROM agent_steps WHERE run_id = ? ORDER BY step_number ASC`
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, dbError(err, "list agent steps")
	}
	defer rows.Close()

	out := []Step{}
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, dbError(err, "scan agent step")
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "list agent steps")
	}
	return out, nil
}
