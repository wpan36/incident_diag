package store

import (
	"context"
	"time"
)

// ReconcileCategory says why a row needs a message produced for it. It is
// carried through to the logs, because "the sweep re-enqueued 40 rows" is a
// very different thing to see depending on which of these it was.
type ReconcileCategory string

const (
	// ReconcileNeverEnqueued is a row whose produce failed or was lost: it was
	// created and no message was ever sent for it.
	ReconcileNeverEnqueued ReconcileCategory = "never_enqueued"

	// ReconcileAbandoned is a row a worker claimed and then died holding.
	ReconcileAbandoned ReconcileCategory = "abandoned"

	// ReconcileRetryable is a row that failed and has attempts left.
	ReconcileRetryable ReconcileCategory = "retryable_failure"
)

// ReconcilePolicy is the part of the sweep's policy that can be expressed in
// SQL. It is deliberately looser than the real policy for the retryable
// category: the query filters on the shortest backoff in the schedule and the
// caller applies the exact per-attempt wait, so there is one place — Go — where
// "is this row due?" is decided, and the database is only asked to narrow the
// scan to rows that could plausibly be.
type ReconcilePolicy struct {
	// PendingAfter is how long a row may sit PENDING before its produce is
	// presumed lost.
	PendingAfter time.Duration

	// Lease is how long a claim holds a row before it counts as abandoned. It
	// is the same value ClaimDocument is called with; if the two disagreed, the
	// sweep would re-enqueue rows the claim then refuses.
	Lease time.Duration

	// MaxAttempts is the claim count at which a failed row is left for a human.
	MaxAttempts int

	// MinBackoff is the shortest wait in the backoff schedule.
	MinBackoff time.Duration
}

// ReconcileCandidate is a row the sweep should look at, with the state the
// policy needs in order to decide.
type ReconcileCandidate struct {
	ID                  string
	Category            ReconcileCategory
	Status              string
	Attempts            int
	UpdatedAt           time.Time
	ProcessingStartedAt *time.Time
}

// ListDocumentsToReconcile returns up to limit documents per category that may
// need a message produced for them.
//
// One query per category rather than one with a compound predicate: each of
// these uses the (status, id) index cleanly, each carries its own limit, and
// each returns rows already labelled with why they were selected. A single
// query would have to derive the category again afterwards from the same
// columns, which is the sort of duplicated reasoning that goes stale.
//
// The limit is per category on purpose. A hundred abandoned rows must not stop
// the sweep from noticing a row that was never enqueued at all.
func (s *Store) ListDocumentsToReconcile(ctx context.Context, p ReconcilePolicy, limit int) ([]ReconcileCandidate, error) {
	if limit < 1 {
		return nil, nil
	}
	t := now()

	queries := []struct {
		category ReconcileCategory
		where    string
		args     []any
	}{
		{
			category: ReconcileNeverEnqueued,
			where:    `status = ? AND updated_at < ?`,
			args:     []any{DocumentPending, t.Add(-p.PendingAfter)},
		},
		{
			category: ReconcileAbandoned,
			where:    `status = ? AND processing_started_at < ?`,
			args:     []any{DocumentProcessing, t.Add(-p.Lease)},
		},
		{
			category: ReconcileRetryable,
			where:    `status = ? AND attempts < ? AND updated_at < ?`,
			args:     []any{DocumentFailed, p.MaxAttempts, t.Add(-p.MinBackoff)},
		},
	}

	var out []ReconcileCandidate
	for _, q := range queries {
		batch, err := s.reconcileQuery(ctx, q.category, q.where, q.args, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

// reconcileQuery runs one category's query.
//
// Ordered by id, which for a ULID means oldest first: a backlog larger than the
// batch is drained from the front rather than in whatever order InnoDB happens
// to return, so nothing at the back starves.
func (s *Store) reconcileQuery(ctx context.Context, category ReconcileCategory,
	where string, args []any, limit int) ([]ReconcileCandidate, error) {

	q := `SELECT id, status, attempts, updated_at, processing_started_at
	        FROM documents
	       WHERE ` + where + `
	       ORDER BY id LIMIT ?`

	rows, err := s.db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, dbError(err, "list documents to reconcile")
	}
	defer rows.Close()

	var out []ReconcileCandidate
	for rows.Next() {
		c := ReconcileCandidate{Category: category}
		if err := rows.Scan(&c.ID, &c.Status, &c.Attempts, &c.UpdatedAt, &c.ProcessingStartedAt); err != nil {
			return nil, dbError(err, "scan reconcile candidate")
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(err, "list documents to reconcile")
	}
	return out, nil
}
