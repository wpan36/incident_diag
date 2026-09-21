package reconcile

import (
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/store"
)

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		// Defensive: a FAILED row with no attempts recorded should not be
		// retried instantly.
		{0, time.Minute},
		{1, time.Minute},
		{2, 5 * time.Minute},
		// Past the end of the schedule the last entry repeats. Raising
		// INGEST_MAX_ATTEMPTS must add five-minute retries, not a zero wait and
		// a hot loop.
		{3, 5 * time.Minute},
		{10, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := Backoff(c.attempts); got != c.want {
			t.Errorf("Backoff(%d) = %s, want %s", c.attempts, got, c.want)
		}
	}
}

func TestMinBackoffMatchesTheSchedule(t *testing.T) {
	// The query narrows the scan with this value, so if it ever exceeded the
	// shortest wait the sweep would silently stop retrying the newest failures.
	if got := MinBackoff(); got != time.Minute {
		t.Errorf("MinBackoff() = %s, want 1m", got)
	}
	for _, d := range backoffSchedule {
		if d < MinBackoff() {
			t.Errorf("schedule contains %s, shorter than MinBackoff() = %s", d, MinBackoff())
		}
	}
}

func testPolicy() Policy {
	return Policy{
		Interval:     30 * time.Second,
		PendingAfter: time.Minute,
		Lease:        10 * time.Minute,
		MaxAttempts:  3,
		Batch:        100,
	}
}

func TestDueDocument(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	at := func(d time.Duration) *time.Time { t := ago(d); return &t }
	p := testPolicy()

	cases := []struct {
		name         string
		candidate    store.ReconcileCandidate
		wantCategory store.ReconcileCategory
		wantDue      bool
	}{
		{
			name:         "a fresh PENDING row is still being enqueued",
			candidate:    store.ReconcileCandidate{Status: store.DocumentPending, UpdatedAt: ago(5 * time.Second)},
			wantCategory: store.ReconcileNeverEnqueued,
		},
		{
			name:         "a PENDING row older than PendingAfter lost its produce",
			candidate:    store.ReconcileCandidate{Status: store.DocumentPending, UpdatedAt: ago(90 * time.Second)},
			wantCategory: store.ReconcileNeverEnqueued,
			wantDue:      true,
		},
		{
			name: "a PROCESSING row inside its lease is being worked on",
			candidate: store.ReconcileCandidate{Status: store.DocumentProcessing,
				UpdatedAt: ago(time.Minute), ProcessingStartedAt: at(time.Minute)},
			wantCategory: store.ReconcileAbandoned,
		},
		{
			name: "a PROCESSING row past its lease was abandoned",
			candidate: store.ReconcileCandidate{Status: store.DocumentProcessing,
				UpdatedAt: ago(20 * time.Minute), ProcessingStartedAt: at(20 * time.Minute)},
			wantCategory: store.ReconcileAbandoned,
			wantDue:      true,
		},
		{
			name: "a PROCESSING row with no stamp is left alone",
			candidate: store.ReconcileCandidate{Status: store.DocumentProcessing,
				UpdatedAt: ago(time.Hour)},
			wantCategory: store.ReconcileAbandoned,
		},
		{
			name: "a first failure waits a minute",
			candidate: store.ReconcileCandidate{Status: store.DocumentFailed,
				Attempts: 1, UpdatedAt: ago(30 * time.Second)},
			wantCategory: store.ReconcileRetryable,
		},
		{
			name: "a first failure is retried after a minute",
			candidate: store.ReconcileCandidate{Status: store.DocumentFailed,
				Attempts: 1, UpdatedAt: ago(2 * time.Minute)},
			wantCategory: store.ReconcileRetryable,
			wantDue:      true,
		},
		{
			name: "a second failure is not retried after only a minute",
			candidate: store.ReconcileCandidate{Status: store.DocumentFailed,
				Attempts: 2, UpdatedAt: ago(2 * time.Minute)},
			wantCategory: store.ReconcileRetryable,
		},
		{
			name: "a second failure is retried after five minutes",
			candidate: store.ReconcileCandidate{Status: store.DocumentFailed,
				Attempts: 2, UpdatedAt: ago(6 * time.Minute)},
			wantCategory: store.ReconcileRetryable,
			wantDue:      true,
		},
		{
			name: "a third failure is out of attempts and waits for a human",
			candidate: store.ReconcileCandidate{Status: store.DocumentFailed,
				Attempts: 3, UpdatedAt: ago(24 * time.Hour)},
			wantCategory: store.ReconcileRetryable,
		},
		{
			name:      "a READY row is not the sweep's business",
			candidate: store.ReconcileCandidate{Status: store.DocumentReady, UpdatedAt: ago(24 * time.Hour)},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			category, due := p.DueDocument(c.candidate, now)
			if due != c.wantDue {
				t.Errorf("due = %v, want %v", due, c.wantDue)
			}
			if category != c.wantCategory {
				t.Errorf("category = %q, want %q", category, c.wantCategory)
			}
		})
	}
}

func TestStorePolicyCarriesTheLeaseUnchanged(t *testing.T) {
	// The sweep and the claim have to agree about the lease, or the sweep
	// re-enqueues rows the claim then refuses — an invisible, wasteful loop.
	p := testPolicy()
	sp := p.storePolicy()
	if sp.Lease != p.Lease {
		t.Errorf("store policy lease = %s, want %s", sp.Lease, p.Lease)
	}
	if sp.MaxAttempts != p.MaxAttempts {
		t.Errorf("store policy max attempts = %d, want %d", sp.MaxAttempts, p.MaxAttempts)
	}
	if sp.MinBackoff != MinBackoff() {
		t.Errorf("store policy min backoff = %s, want %s", sp.MinBackoff, MinBackoff())
	}
}

func TestDueRun(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	at := func(d time.Duration) *time.Time { t := ago(d); return &t }
	// The run policy's lease and attempt limit come from RUN_LEASE and
	// RUN_MAX_ATTEMPTS rather than the INGEST_ ones.
	p := testPolicy()
	p.Lease = 15 * time.Minute

	cases := []struct {
		name         string
		candidate    store.ReconcileCandidate
		wantCategory store.ReconcileCategory
		wantDue      bool
	}{
		{
			name:         "a fresh PENDING run is still being enqueued",
			candidate:    store.ReconcileCandidate{Status: store.RunPending, UpdatedAt: ago(5 * time.Second)},
			wantCategory: store.ReconcileNeverEnqueued,
		},
		{
			name:         "a PENDING run older than PendingAfter lost its produce",
			candidate:    store.ReconcileCandidate{Status: store.RunPending, UpdatedAt: ago(90 * time.Second)},
			wantCategory: store.ReconcileNeverEnqueued,
			wantDue:      true,
		},
		{
			name: "a RUNNING run inside its lease is being investigated",
			candidate: store.ReconcileCandidate{Status: store.RunRunning, Attempts: 1,
				UpdatedAt: ago(10 * time.Minute), ProcessingStartedAt: at(10 * time.Minute)},
			wantCategory: store.ReconcileAbandoned,
		},
		{
			name: "a RUNNING run past its lease was abandoned by a dead worker",
			candidate: store.ReconcileCandidate{Status: store.RunRunning, Attempts: 1,
				UpdatedAt: ago(20 * time.Minute), ProcessingStartedAt: at(20 * time.Minute)},
			wantCategory: store.ReconcileAbandoned,
			wantDue:      true,
		},
		{
			name: "a RUNNING run with no stamp is left alone",
			candidate: store.ReconcileCandidate{Status: store.RunRunning, Attempts: 1,
				UpdatedAt: ago(time.Hour)},
			wantCategory: store.ReconcileAbandoned,
		},
		{
			// RUN_MAX_ATTEMPTS bounds restarts, which is the category
			// documents leave unbounded: a run that kills the worker every
			// time would otherwise be reclaimed forever, and each restart
			// deletes its rows and spends real tokens.
			name: "a run at the attempt limit is left RUNNING for a human",
			candidate: store.ReconcileCandidate{Status: store.RunRunning, Attempts: 3,
				UpdatedAt: ago(time.Hour), ProcessingStartedAt: at(time.Hour)},
			wantCategory: store.ReconcileAbandoned,
		},
		{
			// There is no retryable-failure category for runs. ClaimRun
			// refuses a FAILED run, so re-enqueuing one would produce a
			// message the claim always rejects and attempts would never
			// increase — the row would match on every sweep, forever.
			name: "a FAILED run is never a candidate",
			candidate: store.ReconcileCandidate{Status: store.RunFailed, Attempts: 1,
				UpdatedAt: ago(24 * time.Hour)},
		},
		{
			name: "a SUCCEEDED run is not the sweep's business",
			candidate: store.ReconcileCandidate{Status: store.RunSucceeded,
				UpdatedAt: ago(24 * time.Hour)},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			category, due := p.DueRun(c.candidate, now)
			if due != c.wantDue {
				t.Errorf("due = %v, want %v", due, c.wantDue)
			}
			if category != c.wantCategory {
				t.Errorf("category = %q, want %q", category, c.wantCategory)
			}
		})
	}
}

func TestRunPolicyOverridesOnlyTheLeaseAndTheAttemptLimit(t *testing.T) {
	// The interval, the pending-after and the batch are shared with the
	// document sweep; the other two are the run's own.
	cfg := config.Reconcile{
		Interval:     30 * time.Second,
		PendingAfter: time.Minute,
		Batch:        100,
		Lease:        10 * time.Minute,
		MaxAttempts:  9,
	}
	p := RunPolicy(cfg, 15*time.Minute, 3)

	if p.Lease != 15*time.Minute {
		t.Errorf("Lease = %s, want the run lease of 15m", p.Lease)
	}
	if p.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", p.MaxAttempts)
	}
	if p.Interval != cfg.Interval || p.PendingAfter != cfg.PendingAfter || p.Batch != cfg.Batch {
		t.Errorf("RunPolicy changed a shared value: %+v", p)
	}
}
