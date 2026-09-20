//go:build integration

// Integration tests for the reconciler against a real MySQL. They need no
// broker: the producer is faked, because what is under test here is which rows
// the sweep decides to re-enqueue, not how a message reaches Kafka.
//
//	make up && make test-integration
package reconcile

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/migrations"
)

var schemaOnce sync.Once

// testStore returns a store against a database with no documents in it, so a
// sweep's counts are about this test's rows.
func testStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not set; skipping the integration tests")
	}
	schemaOnce.Do(func() {
		if _, err := migrations.Up(dsn, nil); err != nil {
			t.Fatalf("applying migrations to the test database: %v", err)
		}
	})

	ctx := context.Background()
	st, err := store.Open(ctx, config.Database{DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 5, ConnMaxLifetime: time.Minute})
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.DB().ExecContext(ctx, "DELETE FROM documents"); err != nil {
		t.Fatalf("clearing documents: %v", err)
	}
	return st
}

func testRunner(t *testing.T, st *store.Store) (*Runner, *mq.FakeProducer) {
	t.Helper()
	p := &mq.FakeProducer{}
	return NewRunner(st, p, Policy{
		Interval:     30 * time.Second,
		PendingAfter: time.Minute,
		Lease:        10 * time.Minute,
		MaxAttempts:  3,
		Batch:        100,
	}, log.Discard()), p
}

func newDocument(t *testing.T, st *store.Store) store.Document {
	t.Helper()
	documentID := id.New()
	d, err := st.CreateDocument(context.Background(), store.NewDocument{
		ID:            documentID,
		Filename:      "runbook.md",
		StoragePath:   documentID + "/runbook.md",
		Format:        store.FormatMarkdown,
		SizeBytes:     64,
		ContentSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		DocumentType:  store.DocumentTypeRunbook,
	})
	if err != nil {
		t.Fatalf("creating a document: %v", err)
	}
	return d
}

// backdate moves a row's bookkeeping timestamps into the past, so a test can
// reach a state that would otherwise need a ten-minute wait.
func backdate(t *testing.T, st *store.Store, documentID string, age time.Duration) {
	t.Helper()
	past := time.Now().UTC().Truncate(time.Microsecond).Add(-age)
	_, err := st.DB().ExecContext(context.Background(),
		`UPDATE documents SET updated_at = ?, processing_started_at = IF(processing_started_at IS NULL, NULL, ?) WHERE id = ?`,
		past, past, documentID)
	if err != nil {
		t.Fatalf("backdating document %s: %v", documentID, err)
	}
}

func TestSweepReEnqueuesARowThatWasNeverEnqueued(t *testing.T) {
	st := testStore(t)
	r, producer := testRunner(t, st)
	ctx := context.Background()

	lost := newDocument(t, st)
	backdate(t, st, lost.ID, 5*time.Minute)

	// Created just now: its produce may still be in flight, and re-enqueuing it
	// would be a duplicate message for no reason. The assertions below are
	// exact counts, so this row being left alone is part of what they check.
	newDocument(t, st)

	stats, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Enqueued[store.ReconcileNeverEnqueued] != 1 || stats.Total() != 1 {
		t.Fatalf("stats = %+v, want exactly one never_enqueued", stats)
	}

	msgs := producer.Messages()
	if len(msgs) != 1 {
		t.Fatalf("produced %d messages, want 1", len(msgs))
	}
	if msgs[0].Topic != mq.TopicDocumentsIngest || msgs[0].Key != lost.ID {
		t.Errorf("produced %s/%s, want %s/%s", msgs[0].Topic, msgs[0].Key, mq.TopicDocumentsIngest, lost.ID)
	}
	if msg, ok := msgs[0].Msg.(mq.DocumentMessage); !ok || msg.DocumentID != lost.ID {
		t.Errorf("produced %+v, want a DocumentMessage for %s", msgs[0].Msg, lost.ID)
	}

	// The row is untouched: the sweep produces and never writes, which is what
	// keeps it from racing the claim.
	after, err := st.GetDocument(ctx, lost.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Status != store.DocumentPending || after.Attempts != 0 {
		t.Errorf("row after the sweep = %s/%d attempts, want PENDING/0", after.Status, after.Attempts)
	}
}

func TestSweepReclaimsAnAbandonedRowButNotABusyOne(t *testing.T) {
	st := testStore(t)
	r, producer := testRunner(t, st)
	ctx := context.Background()

	abandoned := newDocument(t, st)
	if _, err := st.ClaimDocument(ctx, abandoned.ID, 10*time.Minute); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	backdate(t, st, abandoned.ID, 30*time.Minute)

	working := newDocument(t, st)
	if _, err := st.ClaimDocument(ctx, working.ID, 10*time.Minute); err != nil {
		t.Fatalf("claiming: %v", err)
	}

	stats, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Enqueued[store.ReconcileAbandoned] != 1 || stats.Total() != 1 {
		t.Fatalf("stats = %+v, want exactly one abandoned", stats)
	}
	if keys := producer.Keys(); len(keys) != 1 || keys[0] != abandoned.ID {
		t.Errorf("produced %v, want just %s", keys, abandoned.ID)
	}
}

func TestSweepRetriesAFailedRowOnItsBackoffUntilItsAttemptsRunOut(t *testing.T) {
	st := testStore(t)
	r, producer := testRunner(t, st)
	ctx := context.Background()

	fail := func(d store.Document, times int) {
		t.Helper()
		for i := 0; i < times; i++ {
			if _, err := st.ClaimDocument(ctx, d.ID, 10*time.Minute); err != nil {
				t.Fatalf("claiming: %v", err)
			}
			if _, err := st.MarkDocumentFailed(ctx, d.ID, "embedding endpoint refused the batch"); err != nil {
				t.Fatalf("marking failed: %v", err)
			}
		}
	}

	// One attempt, thirty seconds ago: inside the one-minute backoff.
	tooSoon := newDocument(t, st)
	fail(tooSoon, 1)
	backdate(t, st, tooSoon.ID, 30*time.Second)

	// One attempt, two minutes ago: due.
	due := newDocument(t, st)
	fail(due, 1)
	backdate(t, st, due.ID, 2*time.Minute)

	// Two attempts, two minutes ago: the second wait is five minutes, so not
	// yet. This is the case the SQL alone would get wrong — it selects on the
	// shortest backoff and the policy makes the real decision.
	secondWait := newDocument(t, st)
	fail(secondWait, 2)
	backdate(t, st, secondWait.ID, 2*time.Minute)

	// Three attempts: out of budget, and it stays FAILED for a human to look at.
	exhausted := newDocument(t, st)
	fail(exhausted, 3)
	backdate(t, st, exhausted.ID, time.Hour)

	stats, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Total() != 1 || stats.Enqueued[store.ReconcileRetryable] != 1 {
		t.Fatalf("stats = %+v, want exactly one retryable_failure", stats)
	}
	if keys := producer.Keys(); len(keys) != 1 || keys[0] != due.ID {
		t.Errorf("produced %v, want just %s", keys, due.ID)
	}
}

func TestSweepCountsAFailedProduceAndLeavesTheRowAlone(t *testing.T) {
	st := testStore(t)
	r, producer := testRunner(t, st)
	producer.Err = errors.New("no broker available")
	ctx := context.Background()

	lost := newDocument(t, st)
	backdate(t, st, lost.ID, 5*time.Minute)

	stats, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Failed != 1 || stats.Total() != 0 {
		t.Fatalf("stats = %+v, want one failure and nothing enqueued", stats)
	}

	// Nothing was written, so the row still matches and the next sweep tries
	// again. That is the whole recovery mechanism for a broker outage.
	producer.Err = nil
	stats, err = r.Sweep(ctx)
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if stats.Total() != 1 {
		t.Fatalf("stats = %+v, want the row re-enqueued once the broker is back", stats)
	}
}

func TestSweepRespectsTheBatchLimit(t *testing.T) {
	st := testStore(t)
	r, producer := testRunner(t, st)
	r.policy.Batch = 2
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d := newDocument(t, st)
		backdate(t, st, d.ID, 5*time.Minute)
	}

	stats, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Total() != 2 {
		t.Fatalf("sweep produced %d messages, want the batch limit of 2", stats.Total())
	}
	if len(producer.Keys()) != 2 {
		t.Errorf("produced %d messages, want 2", len(producer.Keys()))
	}
}

func TestRunSweepsImmediatelyAndStopsWithItsContext(t *testing.T) {
	st := testStore(t)
	r, producer := testRunner(t, st)
	// Longer than the test: if anything is produced, it was the immediate first
	// sweep, not a tick.
	r.policy.Interval = time.Hour

	lost := newDocument(t, st)
	backdate(t, st, lost.ID, 5*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for len(producer.Keys()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the runner produced nothing within 5s; the first sweep is not immediate")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation; the goroutine leaks")
	}
}
