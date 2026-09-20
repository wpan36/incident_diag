//go:build integration

// Integration tests for the ingestion handler. The idempotency tests need only
// MySQL, because a redelivery is just the handler running twice; the end-to-end
// test additionally needs Kafka.
//
//	make up && make test-integration
package ingest

import (
	"context"
	"os"
	"strings"
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

const testLease = 10 * time.Minute

var schemaOnce sync.Once

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
	return st
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

func recordFor(t *testing.T, msg any, key string) mq.Record {
	t.Helper()
	value, err := mq.Encode(msg)
	if err != nil {
		t.Fatalf("encoding the message: %v", err)
	}
	return mq.Record{Topic: mq.TopicDocumentsIngest, Key: key, Value: value}
}

// TestRedeliveryWhileTheWorkIsInFlightIsSkipped is the property ADR 0003 rests
// on and the one M6 exists to demonstrate: two deliveries of the same message
// never put two workers inside the same document. The claim is a conditional
// UPDATE, so the second one finds zero rows affected and stops.
func TestRedeliveryWhileTheWorkIsInFlightIsSkipped(t *testing.T) {
	st := testStore(t)
	h := NewHandler(st, testLease, log.Discard())
	ctx := context.Background()

	d := newDocument(t, st)

	// Another worker claimed it a moment ago and is working on it now. This is
	// what the handler sees when a rebalance replays a record someone else
	// already picked up.
	if claimed, err := st.ClaimDocument(ctx, d.ID, testLease); err != nil || !claimed {
		t.Fatalf("setting up the in-flight claim: claimed=%v err=%v", claimed, err)
	}

	if err := h.Handle(ctx, recordFor(t, mq.NewDocumentMessage(d.ID), d.ID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	after, err := st.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: the redelivery must not claim work someone else holds", after.Attempts)
	}
	if after.Status != store.DocumentProcessing {
		t.Errorf("status = %q, want it left PROCESSING for the worker that holds it", after.Status)
	}
}

// TestRepeatedDeliveryConvergesOnTheSameEndState is the other half of
// idempotency: however many times the message arrives, the row ends up saying
// the same thing. It does not end up saying it twice.
//
// It also documents a cost that is easy to be surprised by. ClaimDocument
// accepts FAILED -> PROCESSING, because that is how a re-enqueued document is
// retried at all, so a redelivery arriving after a failure does start the work
// again and does burn an attempt — skipping the backoff the reconciler would
// have applied. Per-record commit keeps that to the one record a crash was
// holding, and INGEST_MAX_ATTEMPTS bounds it. With the M6 placeholder, which
// fails every document, it is visible on every redelivery.
func TestRepeatedDeliveryConvergesOnTheSameEndState(t *testing.T) {
	st := testStore(t)
	h := NewHandler(st, testLease, log.Discard())
	ctx := context.Background()

	d := newDocument(t, st)
	rec := recordFor(t, mq.NewDocumentMessage(d.ID), d.ID)

	for i := 0; i < 3; i++ {
		if err := h.Handle(ctx, rec); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	after, err := st.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Status != store.DocumentFailed {
		t.Errorf("status = %q, want FAILED from the placeholder handler", after.Status)
	}
	if after.FailureReason == nil || !strings.Contains(*after.FailureReason, "not implemented") {
		t.Errorf("failure_reason = %v, want the placeholder's reason", after.FailureReason)
	}
	if after.ChunkCount != 0 {
		t.Errorf("chunk_count = %d, want 0: nothing should accumulate across deliveries", after.ChunkCount)
	}
	if after.Attempts != 3 {
		t.Errorf("attempts = %d, want 3: each delivery of a failed document is an attempt", after.Attempts)
	}
}

// TestUnknownSchemaVersionIsRecordedAsAPermanentFailure covers the outcome that
// must not be retried: redelivering a message this build cannot understand
// would never succeed, so it is written down against the row instead.
func TestUnknownSchemaVersionIsRecordedAsAPermanentFailure(t *testing.T) {
	st := testStore(t)
	h := NewHandler(st, testLease, log.Discard())
	ctx := context.Background()

	d := newDocument(t, st)
	msg := mq.NewDocumentMessage(d.ID)
	msg.SchemaVersion = 99
	if err := h.Handle(ctx, recordFor(t, msg, d.ID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	after, err := st.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Status != store.DocumentFailed {
		t.Fatalf("status = %q, want FAILED", after.Status)
	}
	if after.FailureReason == nil || !strings.Contains(*after.FailureReason, "schema version") {
		t.Errorf("failure_reason = %v, want it to name the schema version", after.FailureReason)
	}
}

// TestUndecodableMessageTouchesNothing: the record names no row anyone can
// trust, so there is nothing to mark. The row it was about is still PENDING and
// the reconciler will produce a well-formed message for it.
func TestUndecodableMessageTouchesNothing(t *testing.T) {
	st := testStore(t)
	h := NewHandler(st, testLease, log.Discard())
	ctx := context.Background()

	d := newDocument(t, st)
	rec := mq.Record{Topic: mq.TopicDocumentsIngest, Key: d.ID, Value: []byte(`{"schema_version":`)}
	if err := h.Handle(ctx, rec); err != nil {
		t.Fatalf("Handle returned %v, want nil: a poison message is not a handler failure", err)
	}

	after, err := st.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Status != store.DocumentPending || after.Attempts != 0 {
		t.Errorf("row = %s/%d attempts, want PENDING/0", after.Status, after.Attempts)
	}
}

// TestMessageForAMissingRowIsSurvivable: there is nothing to mark and
// redelivering it forever cannot help, so the handler commits and moves on.
func TestMessageForAMissingRowIsSurvivable(t *testing.T) {
	st := testStore(t)
	h := NewHandler(st, testLease, log.Discard())

	rec := recordFor(t, mq.NewDocumentMessage(id.New()), id.New())
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle returned %v, want nil", err)
	}
}

// TestEndToEndThroughKafka runs the real path: a produced message reaches the
// consumer group, the handler claims the document and writes a terminal state.
func TestEndToEndThroughKafka(t *testing.T) {
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS is not set; skipping the end-to-end test")
	}
	st := testStore(t)
	ctx := context.Background()

	cfg := config.Kafka{Brokers: strings.Split(brokers, ","), ProduceTimeout: 10 * time.Second}
	topic := "test." + strings.ToLower(id.New())
	topicCtx, cancelTopic := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTopic()
	if err := mq.EnsureTopics(topicCtx, cfg.Brokers, mq.TopicSpec{Name: topic, Partitions: 3, Replication: 1}); err != nil {
		t.Fatalf("creating the topic: %v", err)
	}

	producer, err := mq.NewProducer(cfg)
	if err != nil {
		t.Fatalf("opening the producer: %v", err)
	}
	defer producer.Close()

	consumer, err := mq.NewConsumer(cfg, "test-"+strings.ToLower(id.New()), []string{topic}, log.Discard())
	if err != nil {
		t.Fatalf("opening the consumer: %v", err)
	}
	defer consumer.Close()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	h := NewHandler(st, testLease, log.Discard())
	go func() {
		if err := consumer.Run(runCtx, h.Handle); err != nil {
			t.Errorf("consumer returned %v, want nil", err)
		}
	}()

	d := newDocument(t, st)
	if err := producer.Produce(ctx, topic, d.ID, mq.NewDocumentMessage(d.ID)); err != nil {
		t.Fatalf("Produce: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		after, err := st.GetDocument(ctx, d.ID)
		if err != nil {
			t.Fatalf("fetching the document: %v", err)
		}
		if after.Status == store.DocumentFailed && after.Attempts == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document is %s after %d attempts, want FAILED after 1", after.Status, after.Attempts)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
