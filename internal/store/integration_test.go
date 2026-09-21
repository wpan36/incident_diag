//go:build integration

// Integration tests for the store. They need the MySQL from
// deploy/docker-compose.yml:
//
//	make up && go test -tags=integration ./...
//
// They skip cleanly when TEST_MYSQL_DSN is unset, so `go test ./...` stays
// green on a machine with no infrastructure.
//
// Use the make target rather than `go test -tags=integration ./...`: every
// integration package shares one database, and TestMigrationsReverseAndReapply
// below drops every table, so the packages have to run one at a time. The
// target passes -p 1 for exactly that reason.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/summary"
	"github.com/wpan36/incident_diag/migrations"
)

// testLease is the lease these tests claim with. It is long enough that no test
// trips the reclaim path by accident; the tests that are about the reclaim pass
// their own value.
const testLease = time.Hour

// tables in the order they must be truncated is irrelevant — the reset helper
// disables foreign key checks — but the list has to be complete, or a test
// inherits rows from the one before it.
var tables = []string{"evidence", "tool_calls", "agent_steps", "agent_runs", "incidents", "documents"}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not set; skipping the integration tests")
	}
	return dsn
}

func TestMain(m *testing.M) {
	if dsn := os.Getenv("TEST_MYSQL_DSN"); dsn != "" {
		if _, err := migrations.Up(dsn, nil); err != nil {
			fmt.Fprintf(os.Stderr, "applying migrations to the test database: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// testStore returns a store against an empty database.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN(t)

	ctx := context.Background()
	s, err := Open(ctx, config.Database{DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 5, ConnMaxLifetime: time.Minute})
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	reset(t, s)
	return s
}

// reset empties every table.
//
// TRUNCATE on a table referenced by a foreign key fails outright, so the
// statements are wrapped in SET FOREIGN_KEY_CHECKS = 0. That is a session
// variable, which is why this pins one connection for the whole sequence:
// issued through the pool, the setting and the TRUNCATEs could land on
// different connections and the disabling would have no effect.
//
// This is the only supported way to reset; tests do not issue their own
// TRUNCATEs.
func reset(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("pinning a connection: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("disabling foreign key checks: %v", err)
	}
	// Restored on the same connection, so the setting does not leak back into
	// the pool for the next caller to inherit.
	defer func() {
		if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
			t.Fatalf("restoring foreign key checks: %v", err)
		}
	}()

	for _, table := range tables {
		if _, err := conn.ExecContext(ctx, "TRUNCATE TABLE "+table); err != nil {
			t.Fatalf("truncating %s: %v", table, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func mustIncident(t *testing.T, s *Store) Incident {
	t.Helper()
	inc, err := s.CreateIncident(context.Background(), NewIncident{
		Title:       "payment-service latency spike",
		Description: "p99 above 2s since 10:05, checkout timing out",
		Service:     ptr("payment-service"),
	})
	if err != nil {
		t.Fatalf("creating an incident: %v", err)
	}
	return inc
}

func mustRunningRun(t *testing.T, s *Store, incidentID string) Run {
	t.Helper()
	ctx := context.Background()
	r, err := s.CreateRun(ctx, NewRun{IncidentID: incidentID, Model: "deepseek-chat", MaxSteps: 12})
	if err != nil {
		t.Fatalf("creating a run: %v", err)
	}
	claimed, err := s.ClaimRun(ctx, r.ID, testLease)
	if err != nil || !claimed {
		t.Fatalf("claiming the run: claimed=%v err=%v", claimed, err)
	}
	return r
}

func mustDocument(t *testing.T, s *Store) Document {
	t.Helper()
	documentID := id.New()
	d, err := s.CreateDocument(context.Background(), NewDocument{
		// The id is the caller's because the file on disk is already stored
		// under it by the time this row is written.
		ID:            documentID,
		Filename:      "payment-service-runbook.md",
		StoragePath:   documentID + "/payment-service-runbook.md",
		Format:        FormatMarkdown,
		SizeBytes:     8421,
		ContentSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Service:       ptr("payment-service"),
		DocumentType:  DocumentTypeRunbook,
	})
	if err != nil {
		t.Fatalf("creating a document: %v", err)
	}
	return d
}

// TestMigrationsReverseAndReapply checks that the down migration actually
// reverses the up migration. A down migration nobody has executed is a guess.
//
// It leaves the schema applied, so it does not matter where in the run it
// falls.
func TestMigrationsReverseAndReapply(t *testing.T) {
	dsn := testDSN(t)

	// Whatever happens below, the schema is back before the next test runs:
	// a failure partway through this one must not cascade into every other.
	t.Cleanup(func() {
		if _, err := migrations.Up(dsn, nil); err != nil {
			t.Fatalf("restoring the schema: %v", err)
		}
	})

	down, err := migrations.Down(dsn, nil)
	if err != nil {
		t.Fatalf("reversing the migrations: %v", err)
	}
	if down.To != 0 {
		t.Fatalf("schema version after down = %d, want 0", down.To)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	defer db.Close()
	for _, table := range tables {
		// information_schema rather than SHOW TABLES LIKE: SHOW does not take
		// a placeholder, and formatting the name into the statement is a habit
		// worth not having even in a test.
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM information_schema.tables
			  WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n)
		if err != nil {
			t.Fatalf("checking for %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("table %s still exists after the down migration", table)
		}
	}

	up, err := migrations.Up(dsn, nil)
	if err != nil {
		t.Fatalf("re-applying the migrations: %v", err)
	}
	if up.From != 0 || up.To != 1 {
		t.Fatalf("re-apply went %s, want 0 -> 1", up)
	}
}

func TestIncidentRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	created := mustIncident(t, s)
	fetched, err := s.GetIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("fetching the incident: %v", err)
	}

	// DATETIME(6) holds microseconds, and the store truncates before writing
	// precisely so that what comes back equals what went in.
	if !fetched.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at round-tripped as %v, want %v", fetched.CreatedAt, created.CreatedAt)
	}
	if fetched.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at came back in %v, want UTC", fetched.CreatedAt.Location())
	}
	if fetched.Service == nil || *fetched.Service != "payment-service" {
		t.Errorf("service = %v, want payment-service", fetched.Service)
	}
}

func TestIncidentWithoutAServiceStaysNull(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	created, err := s.CreateIncident(ctx, NewIncident{Title: "unknown outage", Description: "everything is slow"})
	if err != nil {
		t.Fatalf("creating the incident: %v", err)
	}
	fetched, err := s.GetIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("fetching the incident: %v", err)
	}
	if fetched.Service != nil {
		t.Fatalf("service = %q, want NULL", *fetched.Service)
	}
}

// TestCreateDocumentKeepsTheCallersID is the property the upload depends on:
// the file is written under <document_id>/<filename> before the row exists, so
// a store that generated its own id would leave storage_path pointing at a
// directory no document is named after.
func TestCreateDocumentKeepsTheCallersID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	d := mustDocument(t, s)
	if !strings.HasPrefix(d.StoragePath, d.ID+"/") {
		t.Fatalf("storage_path %q does not sit under the document id %s", d.StoragePath, d.ID)
	}

	_, err := s.CreateDocument(ctx, NewDocument{Filename: "x.md", Format: FormatMarkdown, DocumentType: DocumentTypeRunbook})
	if got := httpx.KindOf(err); got != httpx.KindInternal {
		t.Fatalf("an empty id was accepted or misclassified: kind = %s, err = %v", got, err)
	}
}

func TestGetIncidentIsNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.GetIncident(context.Background(), "01JBQ8K3M7VXFZ2N9WQYRT4HCD")
	if got := httpx.KindOf(err); got != httpx.KindNotFound {
		t.Fatalf("kind = %s, want not_found (err = %v)", got, err)
	}
}

// TestDocumentClaimIsIdempotent is the property ADR 0003 rests on: a
// redelivered message finds the work already claimed and stops, rather than
// starting it a second time.
func TestDocumentClaimIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := mustDocument(t, s)

	claimed, err := s.ClaimDocument(ctx, d.ID, testLease)
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	again, err := s.ClaimDocument(ctx, d.ID, testLease)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again {
		t.Fatal("the second claim succeeded; a redelivered message would start the work twice")
	}

	after, err := s.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Status != DocumentProcessing {
		t.Errorf("status = %s, want %s", after.Status, DocumentProcessing)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: the rejected claim must not count", after.Attempts)
	}
	if after.ProcessingStartedAt == nil {
		t.Error("processing_started_at was not set; the lease reclaim depends on it")
	}
}

// TestDocumentTerminalWriteRejectsALateDuplicate covers the other half: once a
// document is READY, a duplicate delivery of the work that produced it cannot
// rewrite the row.
func TestDocumentTerminalWriteRejectsALateDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := mustDocument(t, s)

	if claimed, err := s.ClaimDocument(ctx, d.ID, testLease); err != nil || !claimed {
		t.Fatalf("claiming: claimed=%v err=%v", claimed, err)
	}
	if ok, err := s.MarkDocumentReady(ctx, d.ID, 12); err != nil || !ok {
		t.Fatalf("marking ready: ok=%v err=%v", ok, err)
	}

	// The late duplicate, arriving after the row is terminal.
	ok, err := s.MarkDocumentReady(ctx, d.ID, 99)
	if err != nil {
		t.Fatalf("the duplicate terminal write errored: %v", err)
	}
	if ok {
		t.Fatal("the duplicate terminal write changed a finished row")
	}
	if ok, err := s.MarkDocumentFailed(ctx, d.ID, "late failure"); err != nil || ok {
		t.Fatalf("a late failure overwrote a READY row: ok=%v err=%v", ok, err)
	}

	after, err := s.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Status != DocumentReady || after.ChunkCount != 12 || after.FailureReason != nil {
		t.Fatalf("row after the duplicates = %s/%d/%v, want READY/12/<nil>",
			after.Status, after.ChunkCount, after.FailureReason)
	}
}

// TestFailedDocumentCanBeReclaimed covers FAILED -> PROCESSING, which is what
// re-enqueuing a failed document relies on.
func TestFailedDocumentCanBeReclaimed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := mustDocument(t, s)

	if _, err := s.ClaimDocument(ctx, d.ID, testLease); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if ok, err := s.MarkDocumentFailed(ctx, d.ID, "embedding endpoint refused the batch"); err != nil || !ok {
		t.Fatalf("marking failed: ok=%v err=%v", ok, err)
	}

	reclaimed, err := s.ClaimDocument(ctx, d.ID, testLease)
	if err != nil || !reclaimed {
		t.Fatalf("re-claiming a failed document: claimed=%v err=%v", reclaimed, err)
	}
	after, err := s.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", after.Attempts)
	}

	// The reason from the previous attempt must not survive a success.
	if ok, err := s.MarkDocumentReady(ctx, d.ID, 3); err != nil || !ok {
		t.Fatalf("marking ready: ok=%v err=%v", ok, err)
	}
	final, err := s.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if final.FailureReason != nil {
		t.Errorf("failure_reason = %q, want it cleared on success", *final.FailureReason)
	}
}

func TestDocumentFilters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	want := mustDocument(t, s)
	other, err := s.CreateDocument(ctx, NewDocument{
		ID: id.New(), Filename: "oncall.txt", StoragePath: "x/oncall.txt", Format: FormatText,
		ContentSHA256: "0", DocumentType: DocumentTypeServiceDoc,
	})
	if err != nil {
		t.Fatalf("creating the second document: %v", err)
	}

	page, err := s.ListDocuments(ctx, DocumentFilter{Service: "payment-service"}, PageParams{Limit: 10})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != want.ID {
		t.Fatalf("service filter returned %d items, want only %s", len(page.Items), want.ID)
	}

	page, err = s.ListDocuments(ctx, DocumentFilter{DocumentType: DocumentTypeServiceDoc}, PageParams{Limit: 10})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != other.ID {
		t.Fatalf("type filter returned %d items, want only %s", len(page.Items), other.ID)
	}
}

// TestOneActiveRunPerIncident is the database-enforced half of the run
// lifecycle: uniq_active_run, not a read-then-write check two requests could
// interleave.
func TestOneActiveRunPerIncident(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	inc := mustIncident(t, s)

	first, err := s.CreateRun(ctx, NewRun{IncidentID: inc.ID, Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("creating the first run: %v", err)
	}

	_, err = s.CreateRun(ctx, NewRun{IncidentID: inc.ID, Model: "deepseek-chat"})
	if got := httpx.KindOf(err); got != httpx.KindConflict {
		t.Fatalf("second run while one is pending: kind = %s, want conflict (err = %v)", got, err)
	}
	if msg := httpx.Message(err); msg == "internal server error" {
		t.Error("the conflict reached the client as a generic internal error")
	}

	// Finishing the first run releases the incident: a terminal run has a NULL
	// generated column and NULLs do not collide.
	if claimed, err := s.ClaimRun(ctx, first.ID, testLease); err != nil || !claimed {
		t.Fatalf("claiming the first run: claimed=%v err=%v", claimed, err)
	}
	if ok, err := s.FinishRun(ctx, first.ID, RunOutcome{Status: RunSucceeded, StopReason: StopCompleted}); err != nil || !ok {
		t.Fatalf("finishing the first run: ok=%v err=%v", ok, err)
	}
	if _, err := s.CreateRun(ctx, NewRun{IncidentID: inc.ID, Model: "deepseek-chat"}); err != nil {
		t.Fatalf("creating a run after the first finished: %v", err)
	}
}

func TestRunClaimAndFinishAreIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mustRunningRun(t, s, mustIncident(t, s).ID)

	if again, err := s.ClaimRun(ctx, r.ID, testLease); err != nil || again {
		t.Fatalf("a redelivered run message claimed an already-running run: again=%v err=%v", again, err)
	}

	result := json.RawMessage(`{"probable_root_cause":"connection pool exhaustion"}`)
	ok, err := s.FinishRun(ctx, r.ID, RunOutcome{
		Status: RunSucceeded, StopReason: StopMaxSteps, FinalResult: result,
		StepCount: 12, ToolCallCount: 7, PromptTokens: 18000, CompletionTokens: 900,
	})
	if err != nil || !ok {
		t.Fatalf("finishing: ok=%v err=%v", ok, err)
	}
	if ok, err := s.FinishRun(ctx, r.ID, RunOutcome{Status: RunFailed, StopReason: StopError, Error: "late"}); err != nil || ok {
		t.Fatalf("a late duplicate overwrote a finished run: ok=%v err=%v", ok, err)
	}

	after, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("fetching the run: %v", err)
	}
	// A run stopped by a bound is a success carrying the reason it stopped.
	if after.Status != RunSucceeded || after.StopReason == nil || *after.StopReason != StopMaxSteps {
		t.Fatalf("run = %s/%v, want SUCCEEDED/MAX_STEPS", after.Status, after.StopReason)
	}
	if string(after.FinalResult) == "" {
		t.Error("final_result did not round-trip")
	}
	if after.FinishedAt == nil || after.StartedAt == nil {
		t.Error("started_at and finished_at should both be set")
	}
	if after.Error != nil {
		t.Errorf("error = %q, want NULL on a successful run", *after.Error)
	}
}

// TestDuplicateStepNumberIsNotAClientConflict is the other half of the 1062
// translation: only uniq_active_run means "conflict". A repeated step number is
// a bug in the agent loop and has to reach the logs as a 500.
func TestDuplicateStepNumberIsNotAClientConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mustRunningRun(t, s, mustIncident(t, s).ID)

	step := NewStep{
		RunID: r.ID, StepNumber: 1, ActionType: ActionRetrieve,
		Action: json.RawMessage(`{"query":"payment-service latency"}`),
		Status: StepOK, Observation: "3 chunks retrieved", Duration: 120 * time.Millisecond,
	}
	if _, err := s.CreateStep(ctx, step); err != nil {
		t.Fatalf("creating the first step: %v", err)
	}
	_, err := s.CreateStep(ctx, step)
	if got := httpx.KindOf(err); got != httpx.KindInternal {
		t.Fatalf("kind = %s, want internal (err = %v)", got, err)
	}
	if msg := httpx.Message(err); msg != "internal server error" {
		t.Errorf("client message = %q, want the generic one", msg)
	}
}

func TestStepToolCallAndEvidenceRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	inc := mustIncident(t, s)
	r := mustRunningRun(t, s, inc.ID)
	doc := mustDocument(t, s)

	step, err := s.CreateStep(ctx, NewStep{
		RunID: r.ID, StepNumber: 1, ActionType: ActionToolCall,
		Action:      json.RawMessage(`{"tool":"prometheus_query","arguments":{"query":"up"}}`),
		Observation: "instant query returned 4 series", Status: StepOK, Duration: 340 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("creating the step: %v", err)
	}

	tc, err := s.CreateToolCall(ctx, NewToolCall{
		RunID: r.ID, StepID: step.ID, ToolName: "prometheus_query",
		Arguments: json.RawMessage(`{"query":"up"}`), Status: ToolCallOK,
		Result: "up{job=\"payment-service\"} 1", Duration: 210 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("creating the tool call: %v", err)
	}

	ev, err := s.CreateEvidence(ctx, NewEvidence{
		RunID: r.ID, StepID: step.ID, ToolCallID: tc.ID, SourceType: SourceTool,
		SourceRef: "prometheus_query:up", Summary: "payment-service is up", Note: "rules out a hard outage",
	})
	if err != nil {
		t.Fatalf("creating the evidence: %v", err)
	}

	retrieved, err := s.CreateEvidence(ctx, NewEvidence{
		RunID: r.ID, StepID: step.ID, SourceType: SourceRetrieval,
		SourceRef: doc.Filename + "#connection-pool", DocumentID: doc.ID,
		Summary: "the runbook describes pool exhaustion with these symptoms",
	})
	if err != nil {
		t.Fatalf("creating the retrieval evidence: %v", err)
	}

	steps, err := s.ListStepsByRun(ctx, r.ID)
	if err != nil || len(steps) != 1 || steps[0].ID != step.ID {
		t.Fatalf("listing steps: %d rows, err %v", len(steps), err)
	}
	if steps[0].Observation == nil || steps[0].Truncated {
		t.Errorf("observation = %v truncated = %v, want the text stored whole", steps[0].Observation, steps[0].Truncated)
	}
	if steps[0].DurationMS != 340 {
		t.Errorf("duration_ms = %d, want 340", steps[0].DurationMS)
	}

	calls, err := s.ListToolCallsByRun(ctx, r.ID)
	if err != nil || len(calls) != 1 || calls[0].ID != tc.ID {
		t.Fatalf("listing tool calls: %d rows, err %v", len(calls), err)
	}

	found, err := s.ListEvidenceByRun(ctx, r.ID)
	if err != nil || len(found) != 2 {
		t.Fatalf("listing evidence: %d rows, err %v", len(found), err)
	}
	if found[0].ID != ev.ID || found[1].ID != retrieved.ID {
		t.Error("evidence came back out of order")
	}
	if found[1].DocumentID == nil || *found[1].DocumentID != doc.ID {
		t.Error("the retrieval evidence lost its document reference")
	}
}

// TestEvidenceSurvivesItsDocument is the one foreign key that does not cascade:
// deleting a document must not erase the record that an investigation relied on
// it, and source_ref keeps the citation readable afterwards.
func TestEvidenceSurvivesItsDocument(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mustRunningRun(t, s, mustIncident(t, s).ID)
	doc := mustDocument(t, s)

	step, err := s.CreateStep(ctx, NewStep{
		RunID: r.ID, StepNumber: 1, ActionType: ActionRetrieve,
		Action: json.RawMessage(`{"query":"pool"}`), Status: StepOK,
	})
	if err != nil {
		t.Fatalf("creating the step: %v", err)
	}
	if _, err := s.CreateEvidence(ctx, NewEvidence{
		RunID: r.ID, StepID: step.ID, SourceType: SourceRetrieval,
		SourceRef: "payment-service-runbook.md#connection-pool", DocumentID: doc.ID,
		Summary: "pool exhaustion",
	}); err != nil {
		t.Fatalf("creating the evidence: %v", err)
	}

	// Nothing in the application deletes documents today; this asserts the DDL
	// says what the spec says it says.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM documents WHERE id = ?", doc.ID); err != nil {
		t.Fatalf("deleting the document: %v", err)
	}

	found, err := s.ListEvidenceByRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("listing evidence: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("evidence rows = %d, want the citation to survive", len(found))
	}
	if found[0].DocumentID != nil {
		t.Errorf("document_id = %q, want NULL", *found[0].DocumentID)
	}
	if found[0].SourceRef == "" {
		t.Error("source_ref is empty; the citation is no longer readable")
	}
}

// TestPagingReturnsEveryRowExactlyOnce includes the case the limit+1 fetch
// exists for: a total that is an exact multiple of the page size must not yield
// a trailing empty page.
func TestPagingReturnsEveryRowExactlyOnce(t *testing.T) {
	for _, total := range []int{0, 1, 5, 6, 9} {
		t.Run(fmt.Sprintf("%d rows", total), func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()

			created := make([]string, 0, total)
			for i := range total {
				inc, err := s.CreateIncident(ctx, NewIncident{
					Title:       fmt.Sprintf("incident %d", i),
					Description: "generated",
				})
				if err != nil {
					t.Fatalf("creating incident %d: %v", i, err)
				}
				created = append(created, inc.ID)
			}

			const limit = 3
			var seen []string
			cursor := ""
			for pages := 0; ; pages++ {
				if pages > total+2 {
					t.Fatal("paging did not terminate")
				}
				page, err := s.ListIncidents(ctx, PageParams{Limit: limit, Cursor: cursor})
				if err != nil {
					t.Fatalf("listing: %v", err)
				}
				if pages > 0 && len(page.Items) == 0 {
					t.Fatal("a cursor produced an empty page; the last page was not detected")
				}
				for _, in := range page.Items {
					seen = append(seen, in.ID)
				}
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
			}

			if len(seen) != total {
				t.Fatalf("saw %d rows, want %d", len(seen), total)
			}
			// Newest first: the reverse of creation order, with no repeats.
			for i, id := range seen {
				if want := created[total-1-i]; id != want {
					t.Fatalf("row %d = %s, want %s", i, id, want)
				}
			}
		})
	}
}

func TestTruncationIsRecordedOnTheRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mustRunningRun(t, s, mustIncident(t, s).ID)

	// Deliberately non-ASCII so the stored value has to be valid UTF-8 for
	// MySQL to accept it into a utf8mb4 column.
	long := ""
	for len(long) < summary.LimitBytes+512 {
		long += "世界 "
	}

	step, err := s.CreateStep(ctx, NewStep{
		RunID: r.ID, StepNumber: 1, ActionType: ActionToolCall,
		Action: json.RawMessage(`{"tool":"prometheus_query"}`), Status: StepOK, Observation: long,
	})
	if err != nil {
		t.Fatalf("creating the step: %v", err)
	}
	if !step.Truncated || step.ObservationBytes != len(long) {
		t.Fatalf("step = truncated %v, %d bytes; want true, %d", step.Truncated, step.ObservationBytes, len(long))
	}

	steps, err := s.ListStepsByRun(ctx, r.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("listing steps: %d rows, err %v", len(steps), err)
	}
	stored := steps[0]
	if !stored.Truncated || stored.ObservationBytes != len(long) {
		t.Fatalf("stored = truncated %v, %d bytes; want true, %d", stored.Truncated, stored.ObservationBytes, len(long))
	}
	if stored.Observation == nil || len(*stored.Observation) > summary.LimitBytes {
		t.Fatalf("stored summary exceeds the cap")
	}
	if *stored.Observation != long[:len(*stored.Observation)] {
		t.Error("the stored summary is not a prefix of the original; MySQL mangled it")
	}
}

// A list call from an in-process caller that never filled in a limit. The API
// cannot produce it, but the agent worker and any maintenance job can, and it
// used to panic rather than return a page.
func TestListWithAnUnsetLimitUsesTheDefault(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := range 3 {
		if _, err := s.CreateIncident(ctx, NewIncident{
			Title:       fmt.Sprintf("incident %d", i),
			Description: "generated",
		}); err != nil {
			t.Fatalf("creating incident %d: %v", i, err)
		}
	}

	page, err := s.ListIncidents(ctx, PageParams{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Items) != 3 {
		t.Errorf("got %d rows, want 3", len(page.Items))
	}
	if page.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty: three rows fit in one default page", page.NextCursor)
	}
}

// --- lease reclaim and the reconcile query ----------------------------------

// backdate moves a document's bookkeeping timestamps into the past, which is
// how these tests reach states that would otherwise need a ten-minute wait.
func backdate(t *testing.T, s *Store, documentID string, age time.Duration) {
	t.Helper()
	past := now().Add(-age)
	_, err := s.DB().ExecContext(context.Background(),
		`UPDATE documents SET updated_at = ?, processing_started_at = IF(processing_started_at IS NULL, NULL, ?) WHERE id = ?`,
		past, past, documentID)
	if err != nil {
		t.Fatalf("backdating document %s: %v", documentID, err)
	}
}

// TestAbandonedDocumentIsReclaimedOnlyAfterItsLease is the fix for the failure
// S1 left open: a worker that claims a document and dies leaves it PROCESSING,
// where every redelivery is refused and the document is stuck forever.
func TestAbandonedDocumentIsReclaimedOnlyAfterItsLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := mustDocument(t, s)

	if claimed, err := s.ClaimDocument(ctx, d.ID, testLease); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}

	// Still inside the lease: the worker holding it may simply be slow, and
	// stealing the document would have two workers embedding the same file.
	if again, err := s.ClaimDocument(ctx, d.ID, time.Hour); err != nil || again {
		t.Fatalf("a fresh PROCESSING row was reclaimed: claimed=%v err=%v", again, err)
	}

	backdate(t, s, d.ID, 30*time.Minute)

	reclaimed, err := s.ClaimDocument(ctx, d.ID, 10*time.Minute)
	if err != nil || !reclaimed {
		t.Fatalf("reclaiming an abandoned document: claimed=%v err=%v", reclaimed, err)
	}
	after, err := s.GetDocument(ctx, d.ID)
	if err != nil {
		t.Fatalf("fetching the document: %v", err)
	}
	if after.Attempts != 2 {
		t.Errorf("attempts = %d, want 2: the reclaim is an attempt", after.Attempts)
	}
	if after.ProcessingStartedAt == nil || after.ProcessingStartedAt.Before(now().Add(-time.Minute)) {
		t.Errorf("processing_started_at = %v, want it restamped by the reclaim", after.ProcessingStartedAt)
	}
}

// TestAbandonedRunIsReclaimedOnlyAfterItsLease is the same property for runs,
// where being stuck is worse: uniq_active_run makes the incident reject every
// new run with 409 until the row moves.
func TestAbandonedRunIsReclaimedOnlyAfterItsLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// mustRunningRun creates the run and claims it, which is the state a
	// worker dies in.
	r := mustRunningRun(t, s, mustIncident(t, s).ID)

	if again, err := s.ClaimRun(ctx, r.ID, time.Hour); err != nil || again {
		t.Fatalf("a fresh RUNNING row was reclaimed: claimed=%v err=%v", again, err)
	}

	past := now().Add(-30 * time.Minute)
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE agent_runs SET started_at = ?, updated_at = ? WHERE id = ?`, past, past, r.ID); err != nil {
		t.Fatalf("backdating the run: %v", err)
	}

	reclaimed, err := s.ClaimRun(ctx, r.ID, 10*time.Minute)
	if err != nil || !reclaimed {
		t.Fatalf("reclaiming an abandoned run: claimed=%v err=%v", reclaimed, err)
	}
}

func testReconcilePolicy() ReconcilePolicy {
	return ReconcilePolicy{
		PendingAfter: time.Minute,
		Lease:        10 * time.Minute,
		MaxAttempts:  3,
		MinBackoff:   time.Minute,
	}
}

// categories indexes candidates by document id, which is what the assertions
// below are actually about.
func categories(cs []ReconcileCandidate) map[string]ReconcileCategory {
	out := make(map[string]ReconcileCategory, len(cs))
	for _, c := range cs {
		out[c.ID] = c.Category
	}
	return out
}

func TestListDocumentsToReconcileFindsEachCategory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Never enqueued: created, and the produce that should have followed it
	// never happened.
	lost := mustDocument(t, s)
	backdate(t, s, lost.ID, 5*time.Minute)

	// Just created: its produce may still be in flight.
	fresh := mustDocument(t, s)

	// Abandoned: claimed by a worker that died.
	abandoned := mustDocument(t, s)
	if _, err := s.ClaimDocument(ctx, abandoned.ID, testLease); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	backdate(t, s, abandoned.ID, 30*time.Minute)

	// Being worked on right now.
	working := mustDocument(t, s)
	if _, err := s.ClaimDocument(ctx, working.ID, testLease); err != nil {
		t.Fatalf("claiming: %v", err)
	}

	// Failed once, long enough ago to be due.
	failed := mustDocument(t, s)
	if _, err := s.ClaimDocument(ctx, failed.ID, testLease); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if _, err := s.MarkDocumentFailed(ctx, failed.ID, "embedding endpoint refused the batch"); err != nil {
		t.Fatalf("marking failed: %v", err)
	}
	backdate(t, s, failed.ID, 5*time.Minute)

	// Failed and out of attempts: terminal, waiting for a human.
	exhausted := mustDocument(t, s)
	for i := 0; i < 3; i++ {
		if _, err := s.ClaimDocument(ctx, exhausted.ID, testLease); err != nil {
			t.Fatalf("claiming: %v", err)
		}
		if _, err := s.MarkDocumentFailed(ctx, exhausted.ID, "still broken"); err != nil {
			t.Fatalf("marking failed: %v", err)
		}
	}
	backdate(t, s, exhausted.ID, time.Hour)

	// Finished, and none of the sweep's business.
	ready := mustDocument(t, s)
	if _, err := s.ClaimDocument(ctx, ready.ID, testLease); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if _, err := s.MarkDocumentReady(ctx, ready.ID, 12); err != nil {
		t.Fatalf("marking ready: %v", err)
	}
	backdate(t, s, ready.ID, time.Hour)

	got, err := s.ListDocumentsToReconcile(ctx, testReconcilePolicy(), 100)
	if err != nil {
		t.Fatalf("ListDocumentsToReconcile: %v", err)
	}
	found := categories(got)

	want := map[string]ReconcileCategory{
		lost.ID:      ReconcileNeverEnqueued,
		abandoned.ID: ReconcileAbandoned,
		failed.ID:    ReconcileRetryable,
	}
	for documentID, category := range want {
		if found[documentID] != category {
			t.Errorf("document %s: category = %q, want %q", documentID, found[documentID], category)
		}
	}
	for _, documentID := range []string{fresh.ID, working.ID, exhausted.ID, ready.ID} {
		if category, ok := found[documentID]; ok {
			t.Errorf("document %s was selected as %q, want it left alone", documentID, category)
		}
	}
}

// TestListDocumentsToReconcileLimitsEachCategory is why the limit exists: a
// backlog that built up while Kafka was down must not come back as one
// thundering herd.
func TestListDocumentsToReconcileLimitsEachCategory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		d := mustDocument(t, s)
		backdate(t, s, d.ID, 5*time.Minute)
	}

	got, err := s.ListDocumentsToReconcile(ctx, testReconcilePolicy(), 2)
	if err != nil {
		t.Fatalf("ListDocumentsToReconcile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want the limit of 2", len(got))
	}
	// Ordered by id, which for ULIDs means oldest first: a backlog drains from
	// the front instead of starving whatever InnoDB happens to return last.
	if got[0].ID > got[1].ID {
		t.Errorf("candidates are not ordered by id: %s then %s", got[0].ID, got[1].ID)
	}
}

// TestClaimRunDeletesThePreviousAttemptAndResetsTheCounters is what makes
// ADR 0009's promise true for a run: a restarted timeline begins again rather
// than splicing two investigations together.
//
// It belongs in the claim rather than in a DeleteRunSteps the worker calls
// afterwards, because a crash between the two would leave a RUNNING run
// rendering the previous attempt's timeline as its own.
func TestClaimRunDeletesThePreviousAttemptAndResetsTheCounters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mustRunningRun(t, s, mustIncident(t, s).ID)

	// A first attempt that got somewhere: a step, its tool call, and a piece
	// of evidence citing it.
	step, err := s.CreateStep(ctx, NewStep{
		RunID: r.ID, StepNumber: 1, ActionType: ActionToolCall,
		Action: json.RawMessage(`{"tool":"prometheus_query"}`),
		Status: StepOK, Observation: "p99 is 2.4s",
	})
	if err != nil {
		t.Fatalf("creating a step: %v", err)
	}
	tc, err := s.CreateToolCall(ctx, NewToolCall{
		RunID: r.ID, StepID: step.ID, ToolName: "prometheus_query",
		Arguments: json.RawMessage(`{"query":"up"}`), Status: ToolCallOK, Result: "1",
	})
	if err != nil {
		t.Fatalf("creating a tool call: %v", err)
	}
	if _, err := s.CreateEvidence(ctx, NewEvidence{
		RunID: r.ID, StepID: step.ID, ToolCallID: tc.ID,
		SourceType: SourceTool, SourceRef: "prometheus_query", Summary: "p99 is 2.4s",
	}); err != nil {
		t.Fatalf("creating evidence: %v", err)
	}
	// Counters the first attempt would have written had it finished.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE agent_runs SET step_count = 4, tool_call_count = 3,
		        prompt_tokens = 1200, completion_tokens = 340 WHERE id = ?`, r.ID); err != nil {
		t.Fatalf("seeding the counters: %v", err)
	}

	past := now().Add(-30 * time.Minute)
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE agent_runs SET started_at = ?, updated_at = ? WHERE id = ?`, past, past, r.ID); err != nil {
		t.Fatalf("backdating the run: %v", err)
	}

	reclaimed, err := s.ClaimRun(ctx, r.ID, 10*time.Minute)
	if err != nil || !reclaimed {
		t.Fatalf("reclaiming: claimed=%v err=%v", reclaimed, err)
	}

	steps, err := s.ListStepsByRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("the reclaim left %d steps of the previous attempt", len(steps))
	}
	// tool_calls and evidence cascade from agent_steps, which is what makes
	// the delete one statement.
	calls, err := s.ListToolCallsByRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("listing tool calls: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("the reclaim left %d tool calls; the cascade did not fire", len(calls))
	}
	ev, err := s.ListEvidenceByRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("listing evidence: %v", err)
	}
	if len(ev) != 0 {
		t.Errorf("the reclaim left %d pieces of evidence; the cascade did not fire", len(ev))
	}

	after, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if after.StepCount != 0 || after.ToolCallCount != 0 ||
		after.PromptTokens != 0 || after.CompletionTokens != 0 {
		t.Errorf("counters after the reclaim = %d/%d/%d/%d, want all zero",
			after.StepCount, after.ToolCallCount, after.PromptTokens, after.CompletionTokens)
	}
	if after.Attempts != 2 {
		t.Errorf("attempts = %d, want 2: the reclaim is an attempt", after.Attempts)
	}
	if after.Status != RunRunning {
		t.Errorf("status = %s, want RUNNING", after.Status)
	}
}

// A refused claim must delete nothing: the run belongs to another attempt that
// is still writing to it.
func TestARefusedClaimLeavesThePreviousAttemptsStepsAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mustRunningRun(t, s, mustIncident(t, s).ID)

	if _, err := s.CreateStep(ctx, NewStep{
		RunID: r.ID, StepNumber: 1, ActionType: ActionRetrieve,
		Action: json.RawMessage(`{"tool":"search_knowledge"}`), Status: StepOK,
	}); err != nil {
		t.Fatalf("creating a step: %v", err)
	}

	// Inside its lease, so the claim is refused.
	claimed, err := s.ClaimRun(ctx, r.ID, time.Hour)
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if claimed {
		t.Fatal("a fresh RUNNING run was reclaimed")
	}

	steps, err := s.ListStepsByRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) != 1 {
		t.Errorf("a refused claim left %d steps, want the 1 the live attempt wrote", len(steps))
	}
}

func TestListRunsToReconcileFindsBothCategories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := testReconcilePolicy()
	// The run lease, which is longer than a document's.
	p.Lease = 15 * time.Minute

	// Never enqueued: created, and the produce never happened.
	lost, err := s.CreateRun(ctx, NewRun{IncidentID: mustIncident(t, s).ID, Model: "m", MaxSteps: 8})
	if err != nil {
		t.Fatalf("creating a run: %v", err)
	}
	backdateRun(t, s, lost.ID, 5*time.Minute)

	// Abandoned: claimed by a worker that died holding it.
	abandoned := mustRunningRun(t, s, mustIncident(t, s).ID)
	backdateRun(t, s, abandoned.ID, 30*time.Minute)

	// Fresh, and being worked on right now.
	mustRunningRun(t, s, mustIncident(t, s).ID)

	// A failed run is never a candidate: ClaimRun refuses one, so a message
	// for it would be rejected forever and attempts would never increase.
	failed := mustRunningRun(t, s, mustIncident(t, s).ID)
	if _, err := s.FinishRun(ctx, failed.ID, RunOutcome{Status: RunFailed, StopReason: StopError,
		Error: "the model could not be called"}); err != nil {
		t.Fatalf("failing a run: %v", err)
	}
	backdateRun(t, s, failed.ID, time.Hour)

	got, err := s.ListRunsToReconcile(ctx, p, 100)
	if err != nil {
		t.Fatalf("ListRunsToReconcile: %v", err)
	}
	found := categories(got)
	if len(found) != 2 {
		t.Fatalf("found %d candidates (%v), want exactly the two stuck runs", len(found), found)
	}
	if found[lost.ID] != ReconcileNeverEnqueued {
		t.Errorf("the never-enqueued run is %q, want %q", found[lost.ID], ReconcileNeverEnqueued)
	}
	if found[abandoned.ID] != ReconcileAbandoned {
		t.Errorf("the abandoned run is %q, want %q", found[abandoned.ID], ReconcileAbandoned)
	}
}

// The attempt limit is in the SQL for runs, not only in the policy: a run that
// kills the worker every time must stop being reclaimed.
func TestListRunsToReconcileStopsAtTheAttemptLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := testReconcilePolicy()
	p.Lease = 15 * time.Minute

	r := mustRunningRun(t, s, mustIncident(t, s).ID)
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE agent_runs SET attempts = ? WHERE id = ?`, p.MaxAttempts, r.ID); err != nil {
		t.Fatalf("setting attempts: %v", err)
	}
	backdateRun(t, s, r.ID, time.Hour)

	got, err := s.ListRunsToReconcile(ctx, p, 100)
	if err != nil {
		t.Fatalf("ListRunsToReconcile: %v", err)
	}
	if _, found := categories(got)[r.ID]; found {
		t.Error("a run at the attempt limit is still a candidate; it should be left for a human")
	}
}

// backdateRun moves a run's bookkeeping timestamps into the past, so a test
// can reach a state that would otherwise need a fifteen-minute wait.
func backdateRun(t *testing.T, s *Store, runID string, age time.Duration) {
	t.Helper()
	past := now().Add(-age)
	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE agent_runs SET updated_at = ?, started_at = IF(started_at IS NULL, NULL, ?) WHERE id = ?`,
		past, past, runID); err != nil {
		t.Fatalf("backdating run %s: %v", runID, err)
	}
}
