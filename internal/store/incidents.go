package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/id"
)

// Incident is one filed incident.
//
// Service is a pointer because the column is nullable and "no service" has to
// stay distinguishable from "the empty string" all the way to the wire.
type Incident struct {
	ID          string
	Title       string
	Description string
	Service     *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewIncident is the caller-supplied part of an incident. The identifier and
// the timestamps are the store's to assign.
type NewIncident struct {
	Title       string
	Description string
	Service     *string
}

// incidentColumns is listed once so the SELECTs and scanIncident cannot drift
// apart. Adding a column to one and forgetting the other is a runtime error
// that no compiler catches.
const incidentColumns = `id, title, description, service, created_at, updated_at`

func scanIncident(s scanner) (Incident, error) {
	var in Incident
	err := s.Scan(&in.ID, &in.Title, &in.Description, &in.Service, &in.CreatedAt, &in.UpdatedAt)
	return in, err
}

// CreateIncident inserts an incident and returns it as stored.
//
// The row is returned rather than just the id so the handler can answer a 201
// with the full resource, and so that the timestamps a client sees are the ones
// actually written.
func (s *Store) CreateIncident(ctx context.Context, in NewIncident) (Incident, error) {
	inc := Incident{
		ID:          id.New(),
		Title:       in.Title,
		Description: in.Description,
		Service:     in.Service,
		CreatedAt:   now(),
	}
	inc.UpdatedAt = inc.CreatedAt

	const q = `INSERT INTO incidents (id, title, description, service, created_at, updated_at)
	           VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, inc.ID, inc.Title, inc.Description, inc.Service, inc.CreatedAt, inc.UpdatedAt)
	if err != nil {
		return Incident{}, dbError(err, "insert incident")
	}
	return inc, nil
}

// GetIncident returns the incident with the given id, or a not-found error.
func (s *Store) GetIncident(ctx context.Context, incidentID string) (Incident, error) {
	const q = `SELECT ` + incidentColumns + ` FROM incidents WHERE id = ?`
	inc, err := scanIncident(s.db.QueryRowContext(ctx, q, incidentID))
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, httpx.NotFoundErr(err, "incident %s not found", incidentID)
	}
	if err != nil {
		return Incident{}, dbError(err, "select incident")
	}
	return inc, nil
}

// ListIncidents returns one page of incidents, newest first.
func (s *Store) ListIncidents(ctx context.Context, p PageParams) (Page[Incident], error) {
	p = p.normalize()

	var c conditions
	c.cursorBefore(p.Cursor)

	q := `SELECT ` + incidentColumns + ` FROM incidents` + c.where() + ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(c.args, p.fetchLimit())...)
	if err != nil {
		return Page[Incident]{}, dbError(err, "list incidents")
	}
	defer rows.Close()

	var out []Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return Page[Incident]{}, dbError(err, "scan incident")
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return Page[Incident]{}, dbError(err, "list incidents")
	}
	return paginate(out, p, func(in Incident) string { return in.ID }), nil
}
