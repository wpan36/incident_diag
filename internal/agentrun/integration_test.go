//go:build integration

// End-to-end tests for the agent worker's handler against a real MySQL and a
// real Redis, with a scripted llm.Fake standing in for the provider and no
// tool server at all. They need the compose stack:
//
//	make up && make test-integration
//
// What is under test here is the frame around the loop — claim, write,
// publish, finish — not the loop itself, which internal/agent tests without
// any infrastructure.
package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wpan36/incident_diag/internal/agent"
	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/events"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/internal/wire"
	"github.com/wpan36/incident_diag/migrations"
)

var schemaOnce sync.Once

// testLease is long enough that no test reclaims a run by accident; the ones
// that want a reclaim backdate the row instead.
const testLease = time.Hour

func testStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not set; skipping the agent worker integration tests")
	}
	schemaOnce.Do(func() {
		if _, err := migrations.Up(dsn, nil); err != nil {
			t.Fatalf("applying migrations to the test database: %v", err)
		}
	})

	st, err := store.Open(t.Context(), config.Database{
		DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 5, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// noSearch is a ChunkSearcher nothing in these tests calls: the scripted
// models below never ask for retrieval.
type noSearch struct{}

func (noSearch) Search(context.Context, []float32, search.Query) ([]search.Result, error) {
	return nil, errors.New("retrieval is not part of these tests")
}

func testHandler(t *testing.T, st *store.Store, pub events.Publisher, turns ...llm.Turn) *Handler {
	t.Helper()
	return NewHandler(Deps{
		Store:  st,
		Events: pub,
		LLM:    llm.NewFake(turns...),
		// Real enough to construct, and never reached.
		Knowledge: &agent.Knowledge{Embedder: &embed.Fake{}, Search: noSearch{}},
		// No tool server: the tool list is then search_knowledge and finish,
		// which is all these scripts call.
		Tools:  nil,
		Lease:  testLease,
		Logger: log.Discard(),
	})
}

// finishTurn is a well-formed finish call, the shortest possible run.
func finishTurn() llm.Turn {
	return llm.CallTurn("call_finish", agent.ToolFinish, map[string]any{
		"root_cause":       "the payment-service connection pool is saturated",
		"affected_service": "payment-service",
		"next_actions":     []string{"raise PAYMENT_POOL_SIZE"},
		"evidence":         []any{},
	})
}

func record(t *testing.T, runID string) mq.Record {
	t.Helper()
	value, err := mq.Encode(mq.NewRunMessage(runID))
	if err != nil {
		t.Fatalf("encoding the message: %v", err)
	}
	return mq.Record{Topic: mq.TopicAgentRuns, Key: runID, Value: value}
}

func pendingRun(t *testing.T, st *store.Store) store.Run {
	t.Helper()
	inc, err := st.CreateIncident(t.Context(), store.NewIncident{
		Title:       "payment-service latency spike",
		Description: "p99 above 2s since 10:05",
	})
	if err != nil {
		t.Fatalf("creating an incident: %v", err)
	}
	run, err := st.CreateRun(t.Context(), store.NewRun{
		IncidentID: inc.ID, Model: "deepseek-chat",
		MaxSteps: 8, MaxToolCalls: 6, MaxDurationSeconds: 300, MaxPromptTokens: 60000,
	})
	if err != nil {
		t.Fatalf("creating a run: %v", err)
	}
	return run
}

// TestAWholeRunWritesItsRowsThenPublishesThem is the ordering ADR 0001 fixes:
// nothing authoritative exists only in Redis, so every event describes
// something that is already in MySQL.
func TestAWholeRunWritesItsRowsThenPublishesThem(t *testing.T) {
	st := testStore(t)
	pub := &events.FakePublisher{}
	run := pendingRun(t, st)

	h := testHandler(t, st, pub, finishTurn())
	if err := h.Handle(t.Context(), record(t, run.ID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	after, err := st.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if after.Status != store.RunSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED (error: %v)", after.Status, after.Error)
	}
	if after.StepCount != 1 || after.ToolCallCount != 0 {
		t.Errorf("counters = %d steps / %d tool calls, want 1 and 0", after.StepCount, after.ToolCallCount)
	}
	if after.FinishedAt == nil {
		t.Error("finished_at is null on a terminal run")
	}

	// Three events, in order, one per thing that happened.
	if got, want := pub.Names(), []string{events.RunStarted, events.StepCompleted, events.RunFinished}; !equal(got, want) {
		t.Fatalf("published %v, want %v", got, want)
	}

	// run.started carries RUNNING and the incremented attempts, which is only
	// true because the worker re-reads the row after the claim rather than
	// publishing the one it read before.
	started := payload[wire.Run](t, pub.Events()[0])
	if started.Status != store.RunRunning {
		t.Errorf("run.started carries status %q, want RUNNING", started.Status)
	}
	if started.Attempts != 1 {
		t.Errorf("run.started carries attempts %d, want 1", started.Attempts)
	}
	if started.StartedAt == nil {
		t.Error("run.started carries a null started_at")
	}

	// step.completed carries the row as it was written, ids and all.
	steps, err := st.ListStepsByRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("the run wrote %d steps, want 1", len(steps))
	}
	step := payload[wire.Step](t, pub.Events()[1])
	if step.ID != steps[0].ID || step.ActionType != store.ActionFinish {
		t.Errorf("step.completed = %+v, want the finish row %s", step, steps[0].ID)
	}

	finished := payload[wire.Run](t, pub.Events()[2])
	if finished.Status != store.RunSucceeded || finished.FinalResult == nil {
		t.Errorf("run.finished = %+v, want the terminal row with its diagnosis", finished)
	}
}

// A failed publish costs a live timeline and never a fact, so the run has to
// succeed with Redis down.
func TestAFailedPublishDoesNotFailTheRun(t *testing.T) {
	st := testStore(t)
	pub := &events.FakePublisher{Err: errors.New("redis is down")}
	run := pendingRun(t, st)

	h := testHandler(t, st, pub, finishTurn())
	if err := h.Handle(t.Context(), record(t, run.ID)); err != nil {
		t.Fatalf("Handle with Redis down: %v", err)
	}

	after, err := st.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if after.Status != store.RunSucceeded {
		t.Errorf("status = %s, want SUCCEEDED: a publish failure is not a run failure (error: %v)",
			after.Status, after.Error)
	}
	steps, err := st.ListStepsByRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) != 1 {
		t.Errorf("the run wrote %d steps, want 1: the audit trail is unaffected", len(steps))
	}
}

// Under at-least-once delivery the same message arrives twice. The second
// delivery must not start a second investigation, and must publish nothing:
// its run.started would tell a browser to clear a timeline that is correct.
func TestARedeliveredMessageStartsNoSecondRun(t *testing.T) {
	st := testStore(t)
	pub := &events.FakePublisher{}
	run := pendingRun(t, st)

	// Two turns scripted, one consumed: if the second delivery ran the loop,
	// it would consume the other and write a second timeline.
	h := testHandler(t, st, pub, finishTurn(), finishTurn())
	rec := record(t, run.ID)
	if err := h.Handle(t.Context(), rec); err != nil {
		t.Fatalf("the first delivery: %v", err)
	}
	published := len(pub.Events())

	if err := h.Handle(t.Context(), rec); err != nil {
		t.Fatalf("the redelivery: %v", err)
	}
	if got := len(pub.Events()); got != published {
		t.Errorf("the redelivery published %d more events, want none", got-published)
	}

	steps, err := st.ListStepsByRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) != 1 {
		t.Errorf("the run has %d steps after a redelivery, want 1", len(steps))
	}
	after, err := st.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d after a redelivery, want 1: the claim refused it", after.Attempts)
	}
}

// A message naming a run that was never created is a real problem rather than
// an ordinary redelivery, and it must not be an error that stalls anything.
func TestAMessageForAMissingRunIsLoggedAndSkipped(t *testing.T) {
	st := testStore(t)
	pub := &events.FakePublisher{}

	h := testHandler(t, st, pub)
	if err := h.Handle(t.Context(), record(t, id.New())); err != nil {
		t.Fatalf("Handle for a missing run: %v", err)
	}
	if n := len(pub.Events()); n != 0 {
		t.Errorf("published %d events for a run that does not exist", n)
	}
}

// A message nothing can decode leaves the row PENDING, which is the
// reconciler's never-enqueued category — and the sweep produces a fresh,
// well-formed message, so even an unknown schema version recovers.
func TestAnUndecodableMessageIsSkipped(t *testing.T) {
	st := testStore(t)
	pub := &events.FakePublisher{}
	h := testHandler(t, st, pub)

	for _, value := range [][]byte{[]byte("{not json"), []byte(`{"schema_version":99,"run_id":"nope"}`)} {
		if err := h.Handle(t.Context(), mq.Record{Topic: mq.TopicAgentRuns, Value: value}); err != nil {
			t.Errorf("Handle for %s: %v", value, err)
		}
	}
	if n := len(pub.Events()); n != 0 {
		t.Errorf("published %d events for messages that could not be decoded", n)
	}
}

// ADR 0009: a reclaimed run begins its timeline again rather than splicing two
// attempts together. That means deleting the previous attempt's rows, resetting
// the counters, and emptying the stream — and the new entries must sort after
// the old ones, or a client resuming from an older Last-Event-ID gets the
// previous attempt's steps.
func TestAReclaimedRunStartsItsTimelineAgain(t *testing.T) {
	st := testStore(t)
	redisClient := testRedis(t)
	run := pendingRun(t, st)

	first := testHandler(t, st, redisClient, finishTurn())
	if err := first.Handle(t.Context(), record(t, run.ID)); err != nil {
		t.Fatalf("the first attempt: %v", err)
	}
	firstSteps, err := st.ListStepsByRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(firstSteps) != 1 {
		t.Fatalf("the first attempt wrote %d steps, want 1", len(firstSteps))
	}
	oldEntries := streamEntries(t, redisClient, run.ID)
	if len(oldEntries) != 3 {
		t.Fatalf("the first attempt left %d stream entries, want 3", len(oldEntries))
	}
	lastOldID := oldEntries[len(oldEntries)-1].ID

	// Put the run back into the state a crashed worker leaves: RUNNING, with a
	// claim older than the lease.
	past := time.Now().UTC().Truncate(time.Microsecond).Add(-2 * time.Hour)
	if _, err := st.DB().ExecContext(t.Context(),
		`UPDATE agent_runs SET status = ?, stop_reason = NULL, final_result = NULL,
		        finished_at = NULL, started_at = ?, updated_at = ? WHERE id = ?`,
		store.RunRunning, past, past, run.ID); err != nil {
		t.Fatalf("returning the run to RUNNING: %v", err)
	}

	second := testHandler(t, st, redisClient, finishTurn())
	if err := second.Handle(t.Context(), record(t, run.ID)); err != nil {
		t.Fatalf("the second attempt: %v", err)
	}

	secondSteps, err := st.ListStepsByRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(secondSteps) != 1 {
		t.Fatalf("after the reclaim the run has %d steps, want 1", len(secondSteps))
	}
	if secondSteps[0].ID == firstSteps[0].ID {
		t.Error("the second attempt reused the first attempt's step row")
	}
	// Step numbers restart, which is exactly why run.started has to mean
	// "clear the timeline".
	if secondSteps[0].StepNumber != 1 {
		t.Errorf("the second attempt's first step is number %d, want 1", secondSteps[0].StepNumber)
	}

	after, err := st.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if after.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", after.Attempts)
	}
	if after.StepCount != 1 {
		t.Errorf("step_count = %d, want the second attempt's 1: the claim resets the counters", after.StepCount)
	}

	newEntries := streamEntries(t, redisClient, run.ID)
	if len(newEntries) != 3 {
		t.Fatalf("the stream holds %d entries, want only the second attempt's 3", len(newEntries))
	}
	if newEntries[0].ID <= lastOldID {
		t.Errorf("the second attempt's first entry %s does not sort after the first attempt's last %s",
			newEntries[0].ID, lastOldID)
	}
	if got := newEntries[0].Values[events.FieldEvent]; got != events.RunStarted {
		t.Errorf("the stream begins with %v, want %s", got, events.RunStarted)
	}
}

// --- helpers ---------------------------------------------------------------

// testRedis is the real publisher, for the tests that read the stream back.
func testRedis(t *testing.T) *events.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set; skipping the event bus part of these tests")
	}
	c, err := events.New(config.Events{
		URL: url, PublishTimeout: 2 * time.Second, StreamMaxLen: 1000, StreamTTL: time.Hour,
	}, log.Discard())
	if err != nil {
		t.Fatalf("opening redis: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// streamEntries reads a run's stream the way M26's reader will. It dials
// separately rather than reaching into events.Client, which keeps that
// package's internals private until M26 gives it a reader of its own.
func streamEntries(t *testing.T, _ *events.Client, runID string) []redis.XMessage {
	t.Helper()
	opts, err := redis.ParseURL(os.Getenv("TEST_REDIS_URL"))
	if err != nil {
		t.Fatalf("parsing TEST_REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	msgs, err := rdb.XRange(t.Context(), events.StreamKey(runID), "-", "+").Result()
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	return msgs
}

// payload decodes what a published event carried. The fake keeps the value
// rather than the JSON, so this round-trips it the way Redis would.
func payload[T any](t *testing.T, p events.Published) T {
	t.Helper()
	raw, err := json.Marshal(p.Event.Payload)
	if err != nil {
		t.Fatalf("marshalling the payload of %s: %v", p.Event.Name, err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding the payload of %s: %v (%s)", p.Event.Name, err, raw)
	}
	return v
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Retrieval's idea of which documents exist comes from Elasticsearch and
// evidence.document_id's comes from MySQL, and the two diverge whenever a
// document row is deleted while its chunks are still indexed — which is the
// case fk_evidence_document's ON DELETE SET NULL anticipates.
//
// A citation to such a document must not fail the evidence write, because a
// failed write fails ReportStep, which ends the run FAILED and discards a
// complete diagnosis. The row is written with a null document_id and its
// source_ref intact, matching what the loop already does with an invented
// document.
func TestACitationToADeletedDocumentDoesNotFailTheRun(t *testing.T) {
	st := testStore(t)
	pub := &events.FakePublisher{}
	run := pendingRun(t, st)

	// A finish step whose evidence names a document that is not in MySQL,
	// built through the ReportStep seam directly: getting the loop to produce
	// one would mean seeding Elasticsearch, and the seam is what is under
	// test.
	h := testHandler(t, st, pub)
	step, err := st.CreateStep(t.Context(), store.NewStep{
		RunID: run.ID, StepNumber: 1, ActionType: store.ActionRetrieve,
		Action: json.RawMessage(`{"tool":"search_knowledge"}`), Status: store.StepOK,
	})
	if err != nil {
		t.Fatalf("creating the cited step: %v", err)
	}

	gone := id.New()
	ref, err := h.ReportStep(t.Context(), agent.Step{
		RunID: run.ID, StepNumber: 2, ActionType: store.ActionFinish,
		Action: json.RawMessage(`{"tool":"finish"}`), Status: store.StepOK,
		Evidence: []agent.Evidence{{
			StepID: step.ID, SourceType: store.SourceRetrieval,
			SourceRef: "payment-service-runbook.md", DocumentID: gone,
			Summary: "raise the pool size", Note: "the runbook says so",
		}},
	})
	if err != nil {
		t.Fatalf("ReportStep with a citation to a deleted document: %v", err)
	}
	if ref.StepID == "" {
		t.Error("ReportStep returned no step id")
	}

	rows, err := st.ListEvidenceByRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("listing evidence: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("wrote %d evidence rows, want 1: the citation is kept, only its id is dropped", len(rows))
	}
	if rows[0].DocumentID != nil {
		t.Errorf("document_id = %v, want null", *rows[0].DocumentID)
	}
	// source_ref is what keeps the citation readable once the document has
	// gone, which is why the row is still worth writing.
	if rows[0].SourceRef != "payment-service-runbook.md" || rows[0].Summary != "raise the pool size" {
		t.Errorf("the citation lost its text: %+v", rows[0])
	}
}
