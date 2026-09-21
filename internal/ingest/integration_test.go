//go:build integration

// Integration tests for the ingestion handler. They need MySQL and
// Elasticsearch; the end-to-end test additionally needs Kafka.
//
//	make up && make test-integration
//
// The embedding provider is faked. What these tests are about is the state
// machine and the convergence properties around it, and a real provider would
// make them slow, non-deterministic and chargeable. The client's own behaviour
// is covered by unit tests in internal/embed.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/migrations"
)

const (
	testLease   = 10 * time.Minute
	testTimeout = 30 * time.Second
)

// runbook is a document shaped like the ones this system ingests: a title, a
// symptom, and short remediation steps under a shared parent.
const runbook = `# Payment Service

Owns card authorization for checkout-service.

## Latency

The p99 is the alert everyone sees first.

### Connection pool

Raise the pool size to 50.

### Timeouts

Set the client timeout to 2 seconds.
`

var schemaOnce sync.Once

// harness is one document pipeline: a real store, real file storage, a real
// index in an alias of its own, and a fake embedder.
type harness struct {
	store    *store.Store
	files    *files.Storage
	embedder *embed.Fake
	search   *search.Client
	handler  *Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not set; skipping the integration tests")
	}
	esURL := os.Getenv("TEST_ELASTICSEARCH_URL")
	if esURL == "" {
		t.Skip("TEST_ELASTICSEARCH_URL is not set; skipping the integration tests")
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

	storage, err := files.New(t.TempDir(), 10<<20)
	if err != nil {
		t.Fatalf("opening the test file storage: %v", err)
	}

	alias := "test_" + strings.ToLower(id.New())
	searchClient, err := search.New(config.Search{URL: esURL, IndexAlias: alias}, log.Discard())
	if err != nil {
		t.Fatalf("opening the test search client: %v", err)
	}
	if err := searchClient.EnsureIndex(ctx); err != nil {
		t.Fatalf("creating the test index: %v", err)
	}
	// The concrete index is named explicitly: Elasticsearch refuses wildcard
	// deletes by default, and a cleanup that silently does nothing leaves a
	// cluster full of test indices.
	t.Cleanup(func() {
		req, err := http.NewRequest(http.MethodDelete, esURL+"/"+alias+"_v1?ignore_unavailable=true", nil)
		if err != nil {
			return
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	})

	h := &harness{store: st, files: storage, embedder: &embed.Fake{}, search: searchClient}
	h.handler = NewHandler(Deps{
		Store:           st,
		Files:           storage,
		Embedder:        h.embedder,
		Search:          searchClient,
		Lease:           testLease,
		DocumentTimeout: testTimeout,
		Chunking:        ChunkOptions{TargetTokens: 400, MaxPerDocument: 2000},
		Logger:          log.Discard(),
	})
	return h
}

// newDocument writes a file and inserts the row that points at it, the way the
// upload handler does: the file reaches disk before the row exists.
func (h *harness) newDocument(t *testing.T, filename, content string) store.Document {
	t.Helper()

	documentID := id.New()
	saved, err := h.files.Save(documentID, filename, strings.NewReader(content))
	if err != nil {
		t.Fatalf("saving the file: %v", err)
	}

	format := store.FormatMarkdown
	if strings.HasSuffix(filename, ".txt") {
		format = store.FormatText
	}
	d, err := h.store.CreateDocument(context.Background(), store.NewDocument{
		ID:            documentID,
		Filename:      filename,
		StoragePath:   saved.Path,
		Format:        format,
		SizeBytes:     saved.Size,
		ContentSHA256: saved.SHA256,
		DocumentType:  store.DocumentTypeRunbook,
	})
	if err != nil {
		t.Fatalf("creating a document: %v", err)
	}
	return d
}

// newRow inserts a row whose file was never written, for the tests that are
// about the row rather than about its contents.
func (h *harness) newRow(t *testing.T) store.Document {
	t.Helper()
	documentID := id.New()
	d, err := h.store.CreateDocument(context.Background(), store.NewDocument{
		ID:            documentID,
		Filename:      "runbook.md",
		StoragePath:   documentID + "/runbook.md",
		Format:        store.FormatMarkdown,
		SizeBytes:     64,
		ContentSHA256: strings.Repeat("a", 64),
		DocumentType:  store.DocumentTypeRunbook,
	})
	if err != nil {
		t.Fatalf("creating a document: %v", err)
	}
	return d
}

func (h *harness) get(t *testing.T, documentID string) store.Document {
	t.Helper()
	d, err := h.store.GetDocument(context.Background(), documentID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	return d
}

func (h *harness) handle(t *testing.T, documentID string) {
	t.Helper()
	value, err := mq.Encode(mq.NewDocumentMessage(documentID))
	if err != nil {
		t.Fatalf("encoding the message: %v", err)
	}
	rec := mq.Record{Topic: mq.TopicDocumentsIngest, Key: documentID, Value: value}
	if err := h.handler.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestADocumentIsChunkedEmbeddedAndIndexed(t *testing.T) {
	h := newHarness(t)
	d := h.newDocument(t, "runbook.md", runbook)

	h.handle(t, d.ID)

	after := h.get(t, d.ID)
	if after.Status != store.DocumentReady {
		t.Fatalf("status = %q with reason %v, want READY", after.Status, after.FailureReason)
	}
	if after.ChunkCount < 2 {
		t.Errorf("chunk_count = %d, want the runbook's sections", after.ChunkCount)
	}
	if got := countChunks(t, h, d.ID); got != after.ChunkCount {
		t.Errorf("%d chunks are searchable but the row says %d", got, after.ChunkCount)
	}
	// The heading path is part of what was embedded, so it is part of what
	// retrieval returns.
	if texts := h.embedder.Texts(); len(texts) == 0 || !strings.Contains(texts[0], "Payment Service") {
		t.Errorf("the embedded text does not carry the heading path: %q", texts)
	}
}

// TestRedeliveryWhileTheWorkIsInFlightIsSkipped is the property ADR 0003 rests
// on: two deliveries of the same message never put two workers inside the same
// document. The claim is a conditional UPDATE, so the second finds zero rows
// affected and stops.
func TestRedeliveryWhileTheWorkIsInFlightIsSkipped(t *testing.T) {
	h := newHarness(t)
	d := h.newDocument(t, "runbook.md", runbook)

	// Another worker claimed it a moment ago and is working on it now.
	if claimed, err := h.store.ClaimDocument(context.Background(), d.ID, testLease); err != nil || !claimed {
		t.Fatalf("setting up the in-flight claim: claimed=%v err=%v", claimed, err)
	}

	h.handle(t, d.ID)

	after := h.get(t, d.ID)
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: the redelivery must not claim work someone else holds", after.Attempts)
	}
	if after.Status != store.DocumentProcessing {
		t.Errorf("status = %q, want it left PROCESSING for the worker that holds it", after.Status)
	}
	if h.embedder.Calls() != 0 {
		t.Errorf("the embedder was called %d times for a document this worker does not hold", h.embedder.Calls())
	}
}

// TestRepeatedDeliveryConvergesOnTheSameEndState is the other half of
// idempotency: however many times the message arrives, the row ends up saying
// the same thing, and it does not end up saying it twice.
func TestRepeatedDeliveryConvergesOnTheSameEndState(t *testing.T) {
	h := newHarness(t)
	d := h.newDocument(t, "runbook.md", runbook)

	for i := 0; i < 3; i++ {
		h.handle(t, d.ID)
	}

	after := h.get(t, d.ID)
	if after.Status != store.DocumentReady {
		t.Fatalf("status = %q, want READY", after.Status)
	}
	// READY is not claimable, so the second and third deliveries are refused
	// before any work happens. That is what keeps a redelivery cheap.
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a finished document must not be re-processed", after.Attempts)
	}
	if got := countChunks(t, h, d.ID); got != after.ChunkCount {
		t.Errorf("%d chunks for a chunk_count of %d: deliveries must not accumulate chunks", got, after.ChunkCount)
	}
}

// TestReprocessingConvergesRatherThanDuplicating is the crash-reclaim path. The
// state machine never allows READY -> PROCESSING, so the way a document is
// genuinely processed twice is a worker dying mid-flight and the lease handing
// it to another. What that second worker runs is exactly this sequence, and the
// leading delete is what makes it converge.
func TestReprocessingConvergesRatherThanDuplicating(t *testing.T) {
	h := newHarness(t)
	d := h.newDocument(t, "runbook.md", runbook)
	ctx := context.Background()

	first, err := h.handler.ingest(ctx, d.ID)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := h.handler.ingest(ctx, d.ID)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if first != second {
		t.Errorf("the two passes produced %d and %d chunks", first, second)
	}
	if got := countChunks(t, h, d.ID); got != first {
		t.Errorf("%d chunks after two passes, want %d", got, first)
	}
}

// TestABulkFailureLeavesNoChunksAtAll: a document indexed at sixty percent is
// the hardest kind of bad state to notice, because it shows up only as a
// retrieval score that is quietly worse than it should be.
func TestABulkFailureLeavesNoChunksAtAll(t *testing.T) {
	h := newHarness(t)
	// The mapping holds embed.Dimensions, so every item of the bulk is
	// rejected — the same shape of failure a mapping conflict has.
	h.embedder.Dims = 8
	d := h.newDocument(t, "runbook.md", runbook)

	h.handle(t, d.ID)

	after := h.get(t, d.ID)
	if after.Status != store.DocumentFailed {
		t.Fatalf("status = %q, want FAILED", after.Status)
	}
	if after.ChunkCount != 0 {
		t.Errorf("chunk_count = %d, want 0", after.ChunkCount)
	}
	if got := countChunks(t, h, d.ID); got != 0 {
		t.Errorf("%d chunks remain after a failed bulk, want none", got)
	}
}

// TestAHealthyDocumentKeepsItsChunksThroughAFailedReIngest is why embedding
// comes before the delete: one provider error must not cost retrieval a
// document it already had.
func TestAHealthyDocumentKeepsItsChunksThroughAFailedReIngest(t *testing.T) {
	h := newHarness(t)
	d := h.newDocument(t, "runbook.md", runbook)
	ctx := context.Background()

	indexed, err := h.handler.ingest(ctx, d.ID)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}

	h.embedder.Err = fmt.Errorf("the provider is down")
	if _, err := h.handler.ingest(ctx, d.ID); err == nil {
		t.Fatal("the second pass succeeded with a broken embedder")
	}
	if got := countChunks(t, h, d.ID); got != indexed {
		t.Errorf("%d chunks after a failed re-ingest, want the %d it already had", got, indexed)
	}
}

func TestADocumentOverTheChunkCapFailsWithoutEmbedding(t *testing.T) {
	h := newHarness(t)
	h.handler.deps.Chunking = ChunkOptions{TargetTokens: 20, MaxPerDocument: 3}

	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(fmt.Sprintf("Paragraph %d, long enough to stand on its own as a chunk.\n\n", i))
	}
	d := h.newDocument(t, "notes.txt", b.String())

	h.handle(t, d.ID)

	after := h.get(t, d.ID)
	if after.Status != store.DocumentFailed {
		t.Fatalf("status = %q, want FAILED", after.Status)
	}
	if after.FailureReason == nil || !strings.Contains(*after.FailureReason, "limit of 3") {
		t.Errorf("failure_reason = %v, want it to name the cap", after.FailureReason)
	}
	if h.embedder.Calls() != 0 {
		t.Errorf("the embedder was called %d times for a document that failed the cap", h.embedder.Calls())
	}
}

func TestAnEmptyDocumentIsAPermanentFailure(t *testing.T) {
	h := newHarness(t)
	d := h.newDocument(t, "empty.md", "   \n\n\t\n")

	h.handle(t, d.ID)

	after := h.get(t, d.ID)
	if after.Status != store.DocumentFailed {
		t.Fatalf("status = %q, want FAILED: a READY document with no chunks would be a lie", after.Status)
	}
	if after.FailureReason == nil || !strings.Contains(*after.FailureReason, "no chunks") {
		t.Errorf("failure_reason = %v", after.FailureReason)
	}
}

func TestAMissingFileIsAPermanentFailure(t *testing.T) {
	h := newHarness(t)
	d := h.newRow(t)

	h.handle(t, d.ID)

	after := h.get(t, d.ID)
	if after.Status != store.DocumentFailed {
		t.Fatalf("status = %q, want FAILED", after.Status)
	}
	if after.FailureReason == nil || !strings.Contains(*after.FailureReason, "opening "+d.ID) {
		t.Errorf("failure_reason = %v", after.FailureReason)
	}
	// The reason a user reads back must not carry the server's storage root.
	if strings.Contains(*after.FailureReason, h.files.Root()) {
		t.Errorf("failure_reason = %v, want it to name the document rather than the server's layout", *after.FailureReason)
	}
}

// TestTheDocumentDeadlineIsRecordedRatherThanLeavingTheRowStuck: the terminal
// write runs on a context detached from the deadline, because the most useful
// thing a handler that has run out of time can do is say so.
func TestTheDocumentDeadlineIsRecordedRatherThanLeavingTheRowStuck(t *testing.T) {
	h := newHarness(t)
	h.handler.deps.DocumentTimeout = time.Nanosecond
	d := h.newDocument(t, "runbook.md", runbook)

	h.handle(t, d.ID)

	after := h.get(t, d.ID)
	if after.Status != store.DocumentFailed {
		t.Fatalf("status = %q, want FAILED rather than a row left PROCESSING for the lease to rescue", after.Status)
	}
	if after.FailureReason == nil || !strings.Contains(*after.FailureReason, "context deadline exceeded") {
		t.Errorf("failure_reason = %v, want it to name the deadline", after.FailureReason)
	}
}

// TestUnknownSchemaVersionIsRecordedAsAPermanentFailure covers the outcome that
// must not be retried: redelivering a message this build cannot understand
// would never succeed, so it is written down against the row instead.
func TestUnknownSchemaVersionIsRecordedAsAPermanentFailure(t *testing.T) {
	h := newHarness(t)
	d := h.newRow(t)

	msg := mq.NewDocumentMessage(d.ID)
	msg.SchemaVersion = 99
	value, err := mq.Encode(msg)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if err := h.handler.Handle(context.Background(),
		mq.Record{Topic: mq.TopicDocumentsIngest, Key: d.ID, Value: value}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	after := h.get(t, d.ID)
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
	h := newHarness(t)
	d := h.newRow(t)

	rec := mq.Record{Topic: mq.TopicDocumentsIngest, Key: d.ID, Value: []byte(`{"schema_version":`)}
	if err := h.handler.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle returned %v, want nil: a poison message is not a handler failure", err)
	}

	after := h.get(t, d.ID)
	if after.Status != store.DocumentPending || after.Attempts != 0 {
		t.Errorf("row = %s/%d attempts, want PENDING/0", after.Status, after.Attempts)
	}
}

// TestMessageForAMissingRowIsSurvivable: there is nothing to mark and
// redelivering it forever cannot help, so the handler commits and moves on.
func TestMessageForAMissingRowIsSurvivable(t *testing.T) {
	h := newHarness(t)
	h.handle(t, id.New())
}

// TestEndToEndThroughKafka runs the real path: a produced message reaches the
// consumer group, the handler claims the document, indexes it, and writes a
// terminal state.
func TestEndToEndThroughKafka(t *testing.T) {
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS is not set; skipping the end-to-end test")
	}
	h := newHarness(t)
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

	consumer, err := mq.NewConsumer(cfg, "test-"+strings.ToLower(id.New()), []string{topic}, log.Discard(),
		mq.WithRebalanceTimeout(testTimeout+time.Minute))
	if err != nil {
		t.Fatalf("opening the consumer: %v", err)
	}
	defer consumer.Close()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() {
		if err := consumer.Run(runCtx, h.handler.Handle); err != nil {
			t.Errorf("consumer returned %v, want nil", err)
		}
	}()

	d := h.newDocument(t, "runbook.md", runbook)
	if err := producer.Produce(ctx, topic, d.ID, mq.NewDocumentMessage(d.ID)); err != nil {
		t.Fatalf("Produce: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		after := h.get(t, d.ID)
		if after.Status == store.DocumentReady && after.ChunkCount > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document is %s after %d attempts with reason %v, want READY",
				after.Status, after.Attempts, after.FailureReason)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// countChunks reports how many of a document's chunks are searchable.
func countChunks(t *testing.T, h *harness, documentID string) int {
	t.Helper()

	url := fmt.Sprintf("%s/%s/_count", strings.TrimRight(os.Getenv("TEST_ELASTICSEARCH_URL"), "/"), h.search.Alias())
	body := fmt.Sprintf(`{"query":{"term":{"document_id":%q}}}`, documentID)

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the count request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("counting chunks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("counting chunks: %s", resp.Status)
	}

	var decoded struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding the count: %v", err)
	}
	return decoded.Count
}
