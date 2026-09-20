package store

import (
	"context"
	"time"

	"github.com/wpan36/incident_diag/internal/id"
)

// Evidence sources.
const (
	SourceRetrieval = "retrieval"
	SourceTool      = "tool"
)

// Evidence is an observation the agent chose to keep as supporting its
// conclusion.
//
// It is a table rather than JSON inside a run's final result because "which
// documents actually get cited across runs" is then a GROUP BY, and that query
// is direct feedback on retrieval quality.
type Evidence struct {
	ID         string
	RunID      string
	StepID     string
	ToolCallID *string
	SourceType string
	// SourceRef keeps the citation readable even if the document it points at
	// is later deleted, which is why deleting a document only nulls DocumentID
	// rather than removing this row.
	SourceRef  string
	DocumentID *string
	Summary    string
	// Note is the agent's own reason for keeping this.
	Note      *string
	CreatedAt time.Time
}

// NewEvidence is the caller-supplied part of a piece of evidence.
type NewEvidence struct {
	RunID      string
	StepID     string
	ToolCallID string
	SourceType string
	SourceRef  string
	DocumentID string
	Summary    string
	Note       string
}

const evidenceColumns = `id, run_id, step_id, tool_call_id, source_type, source_ref,
	document_id, summary, note, created_at`

func scanEvidence(s scanner) (Evidence, error) {
	var e Evidence
	err := s.Scan(&e.ID, &e.RunID, &e.StepID, &e.ToolCallID, &e.SourceType, &e.SourceRef,
		&e.DocumentID, &e.Summary, &e.Note, &e.CreatedAt)
	return e, err
}

// CreateEvidence records one piece of kept evidence.
func (s *Store) CreateEvidence(ctx context.Context, in NewEvidence) (Evidence, error) {
	e := Evidence{
		ID:         id.New(),
		RunID:      in.RunID,
		StepID:     in.StepID,
		SourceType: in.SourceType,
		SourceRef:  in.SourceRef,
		Summary:    in.Summary,
		CreatedAt:  now(),
	}
	if in.ToolCallID != "" {
		e.ToolCallID = &in.ToolCallID
	}
	if in.DocumentID != "" {
		e.DocumentID = &in.DocumentID
	}
	if in.Note != "" {
		e.Note = &in.Note
	}

	const q = `INSERT INTO evidence
	           (id, run_id, step_id, tool_call_id, source_type, source_ref,
	            document_id, summary, note, created_at)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, e.ID, e.RunID, e.StepID, e.ToolCallID, e.SourceType,
		e.SourceRef, e.DocumentID, e.Summary, e.Note, e.CreatedAt)
	if err != nil {
		return Evidence{}, dbError(err, "insert evidence")
	}
	return e, nil
}

// ListEvidenceByRun returns everything a run kept, in the order it was kept.
func (s *Store) ListEvidenceByRun(ctx context.Context, runID string) ([]Evidence, error) {
	const q = `SELECT ` + evidenceColumns + ` FROM evidence WHERE run_id = ? ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, dbError(err, "list evidence")
	}
	defer rows.Close()

	out := []Evidence{}
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			return nil, dbError(err, "scan evidence")
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "list evidence")
	}
	return out, nil
}
