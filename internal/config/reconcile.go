package config

import (
	"fmt"
	"time"
)

// Reconcile is the ingestion sweep's policy: how often to look for rows that
// nothing will ever send a message for again, and what counts as one.
//
// Lease and MaxAttempts are read from INGEST_-prefixed variables because they
// are properties of ingestion rather than of the sweep — the lease is also what
// ClaimDocument uses to decide a row was abandoned — but they live here because
// the sweep is the only thing that needs all of them together.
type Reconcile struct {
	// Interval is how often a sweep runs.
	Interval time.Duration

	// PendingAfter is how long a row may sit PENDING before the sweep assumes
	// its produce was lost. It has to exceed the time between the INSERT and
	// the produce that follows it, which is milliseconds, with enough margin
	// that a slow request does not get a redundant second message.
	PendingAfter time.Duration

	// Batch caps how many rows one sweep re-enqueues per category. Without a
	// cap, a backlog that built up while Kafka was down becomes a thundering
	// herd the moment it comes back.
	Batch int

	// Lease is how long a claim holds a document before another worker may
	// take it. It must exceed the longest legitimate processing time, or a slow
	// document is claimed a second time while the first attempt is still
	// working on it.
	Lease time.Duration

	// MaxAttempts is how many times a document may be claimed before it is left
	// FAILED for a human. Attempts are counted by the claim, so this is a count
	// of claims, not of retries after the first one.
	MaxAttempts int
}

// LoadReconcile reads the sweep policy from the environment, reporting every
// problem it finds at once.
func LoadReconcile() (Reconcile, error) {
	var e env

	r := Reconcile{
		Interval:     e.optionalDuration("RECONCILE_INTERVAL", 30*time.Second),
		PendingAfter: e.optionalDuration("RECONCILE_PENDING_AFTER", 60*time.Second),
		Batch:        e.optionalInt("RECONCILE_BATCH", 100),
		Lease:        e.optionalDuration("INGEST_LEASE", 10*time.Minute),
		MaxAttempts:  e.optionalInt("INGEST_MAX_ATTEMPTS", 3),
	}

	if r.Interval <= 0 {
		e.fail("RECONCILE_INTERVAL must be greater than zero")
	}
	if r.PendingAfter <= 0 {
		e.fail("RECONCILE_PENDING_AFTER must be greater than zero")
	}
	if r.Batch < 1 {
		e.fail("RECONCILE_BATCH must be at least 1, got %d", r.Batch)
	}
	if r.Lease <= 0 {
		e.fail("INGEST_LEASE must be greater than zero")
	}
	if r.MaxAttempts < 1 {
		e.fail("INGEST_MAX_ATTEMPTS must be at least 1, got %d", r.MaxAttempts)
	}

	if err := e.err(); err != nil {
		return Reconcile{}, err
	}
	return r, nil
}

// String renders the configuration for startup logging.
func (r Reconcile) String() string {
	return fmt.Sprintf("interval=%s pending_after=%s batch=%d lease=%s max_attempts=%d",
		r.Interval, r.PendingAfter, r.Batch, r.Lease, r.MaxAttempts)
}
