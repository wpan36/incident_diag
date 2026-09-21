//go:build integration

// End-to-end tests for the HTTP surface against a real MySQL and a real
// directory. They need the compose stack:
//
//	make up && go test -tags=integration ./...
//
// They skip cleanly when TEST_MYSQL_DSN is unset. Use the make target rather
// than `go test -tags=integration ./...`: it passes -p 1, without which the
// store's migration test can drop the schema out from under these.
//
// Unlike the store's integration tests these do not truncate anything. Each one
// tags its rows with a service name unique to the test, so they neither depend
// on an empty database nor disturb each other — which also means they can run
// against a database that already has data in it.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/migrations"
)

var schemaOnce sync.Once

func liveRouter(t *testing.T) (http.Handler, *files.Storage) {
	t.Helper()
	h, fs, _ := liveRouterWithProducer(t)
	return h, fs
}

// liveRouterWithProducer is liveRouter for the tests that care what was
// enqueued as well as what was stored.
func liveRouterWithProducer(t *testing.T) (http.Handler, *files.Storage, *mq.FakeProducer) {
	t.Helper()

	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not set; skipping the integration tests")
	}
	// Once per test binary: the schema has to exist, and applying it is cheap
	// when it already does.
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

	fs, err := files.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatalf("preparing file storage: %v", err)
	}
	producer := &mq.FakeProducer{}
	return NewServer(Deps{
		Store: st, Files: fs, Producer: producer,
		// What POST /api/incidents/{id}/runs records on the row. The values
		// are the loaders' defaults; the endpoint does not let a request
		// override them.
		Agent:    config.Agent{MaxSteps: 8, MaxToolCalls: 6, MaxRunDuration: 5 * time.Minute, MaxPromptTokens: 60000},
		LLMModel: "deepseek-chat",
		Logger:   log.Discard(),
	}).Router(), fs, producer
}

// uniqueService returns a service name no other test will use, so a listing
// filtered by it sees only this test's rows.
func uniqueService(t *testing.T) string {
	t.Helper()
	return "svc-" + strings.ToLower(id.New())
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response is not the expected JSON: %v (%s)", err, rec.Body.String())
	}
	return v
}

// assertSameResource compares two responses by their JSON rather than with ==.
//
// The response structs hold pointers for nullable fields, so == compares the
// addresses: two decodes of the same body would never be equal. The JSON is
// what the contract is about anyway.
func assertSameResource[T any](t *testing.T, got, want T) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("fetch returned %s, want the same resource the create did: %s", gotJSON, wantJSON)
	}
}

func TestUploadThenFetchAndList(t *testing.T) {
	h, fs := liveRouter(t)
	service := uniqueService(t)
	const content = "# payment-service runbook\n\nCheck the connection pool first.\n"

	body, contentType := buildUpload(t,
		uploadPart{field: "file", filename: "payment-service-runbook.md", content: content},
		uploadPart{field: "document_type", content: "runbook"},
		uploadPart{field: "service", content: service},
	)
	req := httptest.NewRequest(http.MethodPost, "/api/documents", body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}

	created := decode[Document](t, rec)
	if got := rec.Header().Get("Location"); got != "/api/documents/"+created.ID {
		t.Errorf("Location = %q, want it to point at the new document", got)
	}
	if created.Status != store.DocumentPending {
		t.Errorf("status = %q, want PENDING", created.Status)
	}
	if created.Format != store.FormatMarkdown {
		t.Errorf("format = %q, want it derived from the .md extension", created.Format)
	}
	if created.SizeBytes != int64(len(content)) {
		t.Errorf("size_bytes = %d, want %d", created.SizeBytes, len(content))
	}
	if created.ChunkCount != 0 || created.FailureReason != nil {
		t.Errorf("chunk_count = %d failure_reason = %v, want 0 and null before ingestion",
			created.ChunkCount, created.FailureReason)
	}

	// storage_path must not be on the wire: it would tell a browser about the
	// container's layout and invite a client to construct one.
	if strings.Contains(rec.Body.String(), "storage_path") {
		t.Errorf("the response carries storage_path: %s", rec.Body.String())
	}
	for _, leaked := range []string{"attempts", "processing_started_at"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("the response carries %s, which is worker bookkeeping", leaked)
		}
	}

	// The file is on disk under <document_id>/<filename>.
	stored, err := os.ReadFile(fs.Root() + "/" + created.ID + "/payment-service-runbook.md")
	if err != nil {
		t.Fatalf("reading the stored file: %v", err)
	}
	if string(stored) != content {
		t.Error("the stored bytes differ from what was uploaded")
	}

	// Fetch returns the same shape as the create did.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/documents/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch status = %d, want 200", rec.Code)
	}
	assertSameResource(t, decode[Document](t, rec), created)

	// And the filters find it.
	for _, query := range []string{
		"?service=" + service,
		"?service=" + service + "&document_type=runbook",
	} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/documents"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s status = %d, want 200", query, rec.Code)
		}
		page := decode[list[Document]](t, rec)
		if len(page.Items) != 1 || page.Items[0].ID != created.ID {
			t.Fatalf("list%s returned %d items, want only %s", query, len(page.Items), created.ID)
		}
	}

	// A filter that does not match returns [], not null.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/documents?service="+service+"&document_type=postmortem", nil))
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("an empty listing rendered as %s, want items: []", rec.Body.String())
	}
}

func TestUploadWithoutAServiceStoresNull(t *testing.T) {
	h, _ := liveRouter(t)

	body, contentType := buildUpload(t,
		uploadPart{field: "file", filename: "oncall.txt", content: "who to call"},
		uploadPart{field: "document_type", content: "service_doc"},
	)
	req := httptest.NewRequest(http.MethodPost, "/api/documents", body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if created := decode[Document](t, rec); created.Service != nil || created.Format != store.FormatText {
		t.Fatalf("service = %v format = %q, want null and text", created.Service, created.Format)
	}
	if !strings.Contains(rec.Body.String(), `"service":null`) {
		t.Errorf("service rendered as something other than an explicit null: %s", rec.Body.String())
	}
}

func TestIncidentLifecycleOverHTTP(t *testing.T) {
	h, _ := liveRouter(t)
	service := uniqueService(t)

	req := httptest.NewRequest(http.MethodPost, "/api/incidents", strings.NewReader(
		`{"title":"payment-service latency spike","description":"p99 above 2s","service":"`+service+`"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	created := decode[Incident](t, rec)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/incidents/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch status = %d, want 200", rec.Code)
	}
	assertSameResource(t, decode[Incident](t, rec), created)

	// A missing incident is a 404 carrying the same request id as the header.
	rec = httptest.NewRecorder()
	missing := httptest.NewRequest(http.MethodGet, "/api/incidents/"+id.New(), nil)
	h.ServeHTTP(rec, missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := decode[errorBody](t, rec)
	if body.Error.RequestID != rec.Header().Get(HeaderRequestID) {
		t.Errorf("request_id %q does not match the %s header %q",
			body.Error.RequestID, HeaderRequestID, rec.Header().Get(HeaderRequestID))
	}
}

func TestReadyzReportsTheDatabase(t *testing.T) {
	h, _ := liveRouter(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("readyz = %d %s, want 200 and the ok body", rec.Code, rec.Body.String())
	}
}

// A title at the column's limit in characters, three times that in bytes.
// Validation counting bytes used to reject this, and the point of the test is
// that MySQL does not: VARCHAR(255) counts characters too, so the row goes in
// and comes back unchanged.
func TestAMultiByteTitleAtTheLimitRoundTrips(t *testing.T) {
	h, _ := liveRouter(t)

	title := strings.Repeat("测", maxTitleLen)
	body, err := json.Marshal(map[string]any{
		"title":       title,
		"description": strings.Repeat("描", maxDescriptionLen),
		"service":     uniqueService(t),
	})
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/incidents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	created := decode[Incident](t, rec)
	if created.Title != title {
		t.Errorf("the stored title is %d characters, want %d",
			utf8.RuneCountInString(created.Title), utf8.RuneCountInString(title))
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/incidents/"+created.ID, nil))
	assertSameResource(t, decode[Incident](t, rec), created)
}

// TestUploadEnqueuesIngestion covers the produce half of the dual write: the
// row is created and a message naming it goes to the ingestion topic, keyed by
// the document ID so that every message about one document lands on one
// partition.
func TestUploadEnqueuesIngestion(t *testing.T) {
	h, _, producer := liveRouterWithProducer(t)

	body, contentType := buildUpload(t,
		uploadPart{field: "file", filename: "runbook.md", content: "# runbook"},
		uploadPart{field: "document_type", content: "runbook"},
		uploadPart{field: "service", content: uniqueService(t)},
	)
	req := httptest.NewRequest(http.MethodPost, "/api/documents", body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	created := decode[Document](t, rec)

	msgs := producer.Messages()
	if len(msgs) != 1 {
		t.Fatalf("produced %d messages, want 1", len(msgs))
	}
	if msgs[0].Topic != mq.TopicDocumentsIngest {
		t.Errorf("topic = %q, want %q", msgs[0].Topic, mq.TopicDocumentsIngest)
	}
	if msgs[0].Key != created.ID {
		t.Errorf("key = %q, want the document id %q", msgs[0].Key, created.ID)
	}
	msg, ok := msgs[0].Msg.(mq.DocumentMessage)
	if !ok {
		t.Fatalf("produced %T, want mq.DocumentMessage", msgs[0].Msg)
	}
	if msg.DocumentID != created.ID {
		t.Errorf("document_id = %q, want %q", msg.DocumentID, created.ID)
	}
}

// TestUploadStillAnswers201WhenTheBrokerIsDown is the deliberate lie-free
// answer: the document was created, so 500 would be false and would make a
// retrying client upload a second copy. The row is PENDING and the reconciler
// is what makes that safe.
func TestUploadStillAnswers201WhenTheBrokerIsDown(t *testing.T) {
	h, _, producer := liveRouterWithProducer(t)
	producer.Err = errors.New("no broker available")

	body, contentType := buildUpload(t,
		uploadPart{field: "file", filename: "runbook.md", content: "# runbook"},
		uploadPart{field: "document_type", content: "runbook"},
		uploadPart{field: "service", content: uniqueService(t)},
	)
	req := httptest.NewRequest(http.MethodPost, "/api/documents", body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 even with the broker down (%s)", rec.Code, rec.Body.String())
	}

	// And the row is in the state the sweep looks for.
	created := decode[Document](t, rec)
	if created.Status != store.DocumentPending {
		t.Errorf("status = %q, want PENDING so the reconciler picks it up", created.Status)
	}
}
