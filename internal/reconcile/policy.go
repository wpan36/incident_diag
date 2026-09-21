// Package reconcile re-enqueues rows that nothing will ever send a message for
// again — documents and agent runs alike.
//
// Two failures put a row in that state, and they are really one failure. A row
// created but never enqueued: the INSERT succeeded and the produce that should
// have followed it failed or never happened. A row claimed but never finished:
// a worker took it and died, leaving it PROCESSING, where the conditional claim
// will refuse every redelivery. In both cases the row is in a non-terminal
// state and no message is coming.
//
// This is why the project needs no transactional outbox. An outbox fixes the
// first case and leaves the second, so the system would carry an outbox table
// and a relay process and still need a lease and a sweep. The sweep alone
// covers both, reading a status column that already exists.
package reconcile

import (
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/store"
)

// backoffSchedule is how long a failed document waits before it is retried,
// indexed by the number of attempts already made.
//
// Two entries, because the attempt limit is three: the claim counts an attempt,
// so a document that has failed once reads attempts=1 and waits a minute, one
// that has failed twice reads 2 and waits five minutes, and one that has failed
// three times is out of attempts and never consults this table. A third entry
// would be unreachable.
var backoffSchedule = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
}

// Backoff returns how long a document with the given number of attempts must
// wait before being retried.
//
// Attempts past the end of the schedule get its last entry rather than an
// undefined value, so raising INGEST_MAX_ATTEMPTS adds five-minute retries
// instead of quietly producing a zero wait and a hot loop.
func Backoff(attempts int) time.Duration {
	if attempts < 1 {
		return backoffSchedule[0]
	}
	if attempts > len(backoffSchedule) {
		return backoffSchedule[len(backoffSchedule)-1]
	}
	return backoffSchedule[attempts-1]
}

// MinBackoff is the shortest wait in the schedule. The database query uses it
// to narrow the scan; Due makes the exact decision.
func MinBackoff() time.Duration {
	min := backoffSchedule[0]
	for _, d := range backoffSchedule[1:] {
		if d < min {
			min = d
		}
	}
	return min
}

// Policy is what the sweep considers stuck.
type Policy struct {
	Interval     time.Duration
	PendingAfter time.Duration
	Lease        time.Duration
	MaxAttempts  int
	Batch        int
}

// PolicyFrom builds a Policy from the loaded configuration.
func PolicyFrom(cfg config.Reconcile) Policy {
	return Policy{
		Interval:     cfg.Interval,
		PendingAfter: cfg.PendingAfter,
		Lease:        cfg.Lease,
		MaxAttempts:  cfg.MaxAttempts,
		Batch:        cfg.Batch,
	}
}

// storePolicy is the part of the policy the query can express.
func (p Policy) storePolicy() store.ReconcilePolicy {
	return store.ReconcilePolicy{
		PendingAfter: p.PendingAfter,
		Lease:        p.Lease,
		MaxAttempts:  p.MaxAttempts,
		MinBackoff:   MinBackoff(),
	}
}

// DueDocument reports whether a document needs a message produced for it at
// time at, and which category it falls into.
//
// This is the whole policy, as a pure function of the row's state. The database
// query that produced the candidate is deliberately looser — it narrows the
// scan — and this decides. Keeping the decision in one place is what stops the
// SQL and the intent from drifting apart, and it is what makes the policy
// testable as a table without a database.
//
// There is one of these per target rather than one that switches on both
// status vocabularies: runs use RUNNING where documents use PROCESSING, and a
// shared function reading both is how the two quietly drift.
func (p Policy) DueDocument(c store.ReconcileCandidate, at time.Time) (store.ReconcileCategory, bool) {
	switch c.Status {
	case store.DocumentPending:
		// The row exists and nothing has touched it since it was created. Long
		// enough after the insert, the produce that should have followed it is
		// presumed lost.
		return store.ReconcileNeverEnqueued, c.UpdatedAt.Before(at.Add(-p.PendingAfter))

	case store.DocumentProcessing:
		// A claim older than the lease belongs to a worker that is not coming
		// back. A NULL stamp is not reclaimable: only the claim writes this
		// status and it always sets the timestamp.
		if c.ProcessingStartedAt == nil {
			return store.ReconcileAbandoned, false
		}
		return store.ReconcileAbandoned, c.ProcessingStartedAt.Before(at.Add(-p.Lease))

	case store.DocumentFailed:
		// Out of attempts is terminal: the row keeps its failure_reason and
		// waits for a human.
		if c.Attempts >= p.MaxAttempts {
			return store.ReconcileRetryable, false
		}
		return store.ReconcileRetryable, c.UpdatedAt.Before(at.Add(-Backoff(c.Attempts)))

	default:
		// READY, or anything a later migration adds. A terminal row is not the
		// sweep's business.
		return "", false
	}
}

// DueRun is DueDocument for an agent run. Runs have two categories, not
// three.
//
// There is no retryable-failure category, because ClaimRun refuses a FAILED
// run by design. Re-enqueuing one would produce a message the claim always
// rejects, and since the sweep writes nothing and the claim never runs,
// attempts would never increase — so the row would match on every sweep,
// forever. A failed run is retried by starting a new one, which
// uniq_active_run permits as soon as the row is terminal.
func (p Policy) DueRun(c store.ReconcileCandidate, at time.Time) (store.ReconcileCategory, bool) {
	switch c.Status {
	case store.RunPending:
		// Created, and the produce that should have followed it is presumed
		// lost. Identical to a document's never-enqueued case.
		return store.ReconcileNeverEnqueued, c.UpdatedAt.Before(at.Add(-p.PendingAfter))

	case store.RunRunning:
		// A claim older than the lease belongs to a worker that is not coming
		// back. A NULL stamp is not reclaimable: only the claim writes this
		// status and it always sets the timestamp.
		if c.ProcessingStartedAt == nil {
			return store.ReconcileAbandoned, false
		}
		// MaxAttempts bounds restarts here, which is the category documents
		// leave unbounded: a run that kills the worker every time would
		// otherwise be reclaimed forever, and each restart deletes its rows
		// and spends real tokens. At the limit it is left RUNNING for a
		// human, visible through GET /api/incidents/{id}/runs.
		if c.Attempts >= p.MaxAttempts {
			return store.ReconcileAbandoned, false
		}
		return store.ReconcileAbandoned, c.ProcessingStartedAt.Before(at.Add(-p.Lease))

	default:
		// SUCCEEDED or FAILED. A terminal row is not the sweep's business.
		return "", false
	}
}

// RunPolicy is the shared sweep policy with the run-side lease and attempt
// limit substituted.
//
// PolicyFrom builds a Policy out of config.Reconcile alone and cannot express
// that: the interval, the pending-after and the batch are shared, while the
// lease and the attempt limit come from RUN_LEASE and RUN_MAX_ATTEMPTS.
func RunPolicy(cfg config.Reconcile, lease time.Duration, maxAttempts int) Policy {
	p := PolicyFrom(cfg)
	p.Lease = lease
	p.MaxAttempts = maxAttempts
	return p
}
