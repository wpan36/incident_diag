package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/id"
)

// Run statuses: the coarse lifecycle machine.
const (
	RunPending   = "PENDING"
	RunRunning   = "RUNNING"
	RunSucceeded = "SUCCEEDED"
	RunFailed    = "FAILED"
)

// Stop reasons: why a run ended, which is a separate question from whether it
// succeeded.
//
// A run stopped by a bound is SUCCEEDED with the matching reason, because
// hitting a bound produces a partial diagnosis and is a normal, tested outcome.
// Collapsing the two columns into one enum would force a choice between calling
// a useful partial result a failure and losing the fact that it was truncated.
const (
	StopCompleted    = "COMPLETED"
	StopMaxSteps     = "MAX_STEPS"
	StopMaxToolCalls = "MAX_TOOL_CALLS"
	StopTimeout      = "TIMEOUT"
	StopTokenBudget  = "TOKEN_BUDGET"
	StopError        = "ERROR"
	StopCancelled    = "CANCELLED"
)

// Run is one investigation of an incident.
type Run struct {
	ID         string
	IncidentID string
	Status     string
	StopReason *string

	// The budget that actually applied, recorded rather than read back from
	// configuration, so a run stays interpretable after the configuration
	// changes.
	MaxSteps           int
	MaxToolCalls       int
	MaxDurationSeconds int
	MaxPromptTokens    int

	Model            string
	StepCount        int
	ToolCallCount    int
	PromptTokens     int
	CompletionTokens int

	FinalResult json.RawMessage
	Error       *string
	Attempts    int
	StartedAt   *time.Time
	FinishedAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewRun is the caller-supplied part of a run.
type NewRun struct {
	IncidentID         string
	Model              string
	MaxSteps           int
	MaxToolCalls       int
	MaxDurationSeconds int
	MaxPromptTokens    int
}

// RunOutcome is everything a finished run records at once.
//
// Status must be RunSucceeded or RunFailed; a run that hit a bound is the
// former, with StopReason saying which bound.
type RunOutcome struct {
	Status      string
	StopReason  string
	FinalResult json.RawMessage
	Error       string

	StepCount        int
	ToolCallCount    int
	PromptTokens     int
	CompletionTokens int
}

// active_incident_id is deliberately absent: it is a generated column, and
// MySQL rejects any attempt to write one.
const runColumns = `id, incident_id, status, stop_reason, max_steps, max_tool_calls,
	max_duration_seconds, max_prompt_tokens, model, step_count, tool_call_count,
	prompt_tokens, completion_tokens, final_result, error, attempts,
	started_at, finished_at, created_at, updated_at`

func scanRun(s scanner) (Run, error) {
	var r Run
	var finalResult []byte
	err := s.Scan(&r.ID, &r.IncidentID, &r.Status, &r.StopReason, &r.MaxSteps, &r.MaxToolCalls,
		&r.MaxDurationSeconds, &r.MaxPromptTokens, &r.Model, &r.StepCount, &r.ToolCallCount,
		&r.PromptTokens, &r.CompletionTokens, &finalResult, &r.Error, &r.Attempts,
		&r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt)
	r.FinalResult = json.RawMessage(finalResult)
	return r, err
}

// CreateRun inserts a run in PENDING.
//
// It is the one insert in this package that can fail for a reason the client
// caused: uniq_active_run holds at most one PENDING or RUNNING run per
// incident, so a second start request while one is in flight is a conflict
// rather than a server fault. Any other duplicate-key violation is a bug and
// falls through to a 500 on purpose.
func (s *Store) CreateRun(ctx context.Context, in NewRun) (Run, error) {
	r := Run{
		ID:                 id.New(),
		IncidentID:         in.IncidentID,
		Status:             RunPending,
		MaxSteps:           in.MaxSteps,
		MaxToolCalls:       in.MaxToolCalls,
		MaxDurationSeconds: in.MaxDurationSeconds,
		MaxPromptTokens:    in.MaxPromptTokens,
		Model:              in.Model,
		CreatedAt:          now(),
	}
	r.UpdatedAt = r.CreatedAt

	const q = `INSERT INTO agent_runs
	           (id, incident_id, status, max_steps, max_tool_calls, max_duration_seconds,
	            max_prompt_tokens, model, created_at, updated_at)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, r.ID, r.IncidentID, r.Status, r.MaxSteps, r.MaxToolCalls,
		r.MaxDurationSeconds, r.MaxPromptTokens, r.Model, r.CreatedAt, r.UpdatedAt)
	if isDuplicateKey(err, keyActiveRun) {
		return Run{}, httpx.ConflictErr(err, "incident %s already has a run in progress", in.IncidentID)
	}
	if err != nil {
		return Run{}, dbError(err, "insert agent run")
	}
	return r, nil
}

// GetRun returns the run with the given id, or a not-found error.
func (s *Store) GetRun(ctx context.Context, runID string) (Run, error) {
	const q = `SELECT ` + runColumns + ` FROM agent_runs WHERE id = ?`
	r, err := scanRun(s.db.QueryRowContext(ctx, q, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, httpx.NotFoundErr(err, "run %s not found", runID)
	}
	if err != nil {
		return Run{}, dbError(err, "select agent run")
	}
	return r, nil
}

// ListRunsByIncident returns one page of an incident's runs, newest first.
func (s *Store) ListRunsByIncident(ctx context.Context, incidentID string, p PageParams) (Page[Run], error) {
	p = p.normalize()

	var c conditions
	c.add("incident_id = ?", incidentID)
	c.cursorBefore(p.Cursor)

	q := `SELECT ` + runColumns + ` FROM agent_runs` + c.where() + ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(c.args, p.fetchLimit())...)
	if err != nil {
		return Page[Run]{}, dbError(err, "list agent runs")
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return Page[Run]{}, dbError(err, "scan agent run")
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return Page[Run]{}, dbError(err, "list agent runs")
	}
	return paginate(out, p, func(r Run) string { return r.ID }), nil
}

// ClaimRun moves a run into RUNNING and reports whether it did.
//
// The same claim pattern as ClaimDocument, and for the same reason: false means
// this Kafka message is a redelivery of a run already started, and the worker
// stops rather than investigating the incident twice.
func (s *Store) ClaimRun(ctx context.Context, runID string) (bool, error) {
	const q = `UPDATE agent_runs
	              SET status = ?, attempts = attempts + 1, started_at = ?, updated_at = ?
	            WHERE id = ? AND status = ?`
	t := now()
	res, err := s.db.ExecContext(ctx, q, RunRunning, t, t, runID, RunPending)
	if err != nil {
		return false, dbError(err, "claim agent run")
	}
	return changed(res, "claim agent run")
}

// FinishRun writes the terminal state of a run, conditional on RUNNING so a
// late duplicate cannot overwrite a finished row.
func (s *Store) FinishRun(ctx context.Context, runID string, out RunOutcome) (bool, error) {
	const q = `UPDATE agent_runs
	              SET status = ?, stop_reason = ?, final_result = ?, error = ?,
	                  step_count = ?, tool_call_count = ?,
	                  prompt_tokens = ?, completion_tokens = ?,
	                  finished_at = ?, updated_at = ?
	            WHERE id = ? AND status = ?`
	t := now()
	res, err := s.db.ExecContext(ctx, q,
		out.Status, nullString(out.StopReason), nullJSON(out.FinalResult), nullString(out.Error),
		out.StepCount, out.ToolCallCount, out.PromptTokens, out.CompletionTokens,
		t, t, runID, RunRunning)
	if err != nil {
		return false, dbError(err, "finish agent run")
	}
	return changed(res, "finish agent run")
}
