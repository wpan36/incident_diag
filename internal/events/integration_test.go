//go:build integration

// Integration tests for the event bus against a real Redis. They need the
// compose stack:
//
//	make up && make test-integration
//
// They skip cleanly when TEST_REDIS_URL is unset. That URL points at a
// different database from REDIS_URL, so a test run cannot disturb whatever a
// locally running worker has published.
package events

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/log"
)

func testClient(t *testing.T) *Client {
	t.Helper()

	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set; skipping the event bus integration tests")
	}
	c, err := New(config.Events{
		URL:            url,
		PublishTimeout: 2 * time.Second,
		StreamMaxLen:   1000,
		StreamTTL:      time.Hour,
		Heartbeat:      5 * time.Second,
		// Short, because every idle Follow in these tests waits it out.
		ReadBlock: 200 * time.Millisecond,
	}, log.Discard())
	if err != nil {
		t.Fatalf("opening redis: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("redis is not reachable at TEST_REDIS_URL: %v", err)
	}
	return c
}

// entries reads a run's whole stream back, the way M26's reader will.
func entries(t *testing.T, c *Client, runID string) []redis.XMessage {
	t.Helper()
	msgs, err := c.rdb.XRange(context.Background(), StreamKey(runID), "-", "+").Result()
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	return msgs
}

func TestPublishWritesTheThreeFieldsAndAnExpiringKey(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	payload := map[string]any{"id": runID, "status": "RUNNING"}
	if err := c.Publish(ctx, runID, Event{Name: RunStarted, Payload: payload}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msgs := entries(t, c, runID)
	if len(msgs) != 1 {
		t.Fatalf("the stream holds %d entries, want 1", len(msgs))
	}

	// The name is its own field so M26 can fill SSE's event: line without
	// decoding the body.
	if got := msgs[0].Values[FieldEvent]; got != RunStarted {
		t.Errorf("%s = %v, want %s", FieldEvent, got, RunStarted)
	}
	if got := msgs[0].Values[FieldRunID]; got != runID {
		t.Errorf("%s = %v, want %s", FieldRunID, got, runID)
	}

	var decoded map[string]any
	data, ok := msgs[0].Values[FieldData].(string)
	if !ok {
		t.Fatalf("%s is %T, want a string", FieldData, msgs[0].Values[FieldData])
	}
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatalf("%s is not JSON: %v (%s)", FieldData, err, data)
	}
	if decoded["status"] != "RUNNING" {
		t.Errorf("payload = %v, want the event's own payload", decoded)
	}

	// The TTL is what keeps finished runs from accumulating forever.
	ttl, err := c.rdb.TTL(ctx, StreamKey(runID)).Result()
	if err != nil {
		t.Fatalf("reading the ttl: %v", err)
	}
	if ttl <= 0 || ttl > time.Hour {
		t.Errorf("ttl = %s, want a positive value at or below the configured hour", ttl)
	}
}

// ADR 0009 promises a restarted timeline begins again rather than showing a
// spliced history. XTRIM rather than DEL is what makes the second attempt's
// entries sort after the first's, so a client resuming from an older
// Last-Event-ID cannot be handed the previous attempt's steps.
func TestTrimEmptiesTheStreamAndKeepsIdsIncreasing(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	for i := 0; i < 3; i++ {
		if err := c.Publish(ctx, runID, Event{Name: StepCompleted, Payload: map[string]int{"step": i + 1}}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	before := entries(t, c, runID)
	if len(before) != 3 {
		t.Fatalf("the first attempt left %d entries, want 3", len(before))
	}
	lastOld := before[len(before)-1].ID

	if err := c.Trim(ctx, runID); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if got := entries(t, c, runID); len(got) != 0 {
		t.Fatalf("the stream still holds %d entries after a trim", len(got))
	}

	if err := c.Publish(ctx, runID, Event{Name: RunStarted, Payload: map[string]int{"attempt": 2}}); err != nil {
		t.Fatalf("Publish after the trim: %v", err)
	}
	after := entries(t, c, runID)
	if len(after) != 1 {
		t.Fatalf("the second attempt left %d entries, want 1", len(after))
	}
	// Redis ids are <ms>-<seq> and compare as strings within one stream.
	if after[0].ID <= lastOld {
		t.Errorf("the new attempt's first id %s does not sort after the old attempt's last id %s",
			after[0].ID, lastOld)
	}

	// The key keeps its TTL through the trim, which DEL would have dropped.
	ttl, err := c.rdb.TTL(ctx, StreamKey(runID)).Result()
	if err != nil {
		t.Fatalf("reading the ttl: %v", err)
	}
	if ttl <= 0 {
		t.Errorf("ttl = %s, want the key still to expire", ttl)
	}
}

// Trimming a run that never published anything is what a first attempt does,
// and it must not be an error.
func TestTrimmingAStreamThatDoesNotExistIsFine(t *testing.T) {
	c := testClient(t)
	if err := c.Trim(context.Background(), id.New()); err != nil {
		t.Errorf("Trim on a missing stream: %v", err)
	}
}

// collect drains a reader call into a slice, discarding the last id read —
// which the tests that care about it assert on directly.
func collect(t *testing.T, call func(func(ReadEvent) error) (string, error)) []ReadEvent {
	t.Helper()
	var got []ReadEvent
	if _, err := call(func(ev ReadEvent) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("reading: %v", err)
	}
	return got
}

// A client that subscribed before the run started sees everything, in order,
// with the ids Redis assigned.
func TestReplayYieldsTheWholeStreamInOrder(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	for _, name := range []string{RunStarted, StepCompleted, RunFinished} {
		if err := c.Publish(ctx, runID, Event{Name: name, Payload: map[string]string{"name": name}}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	got := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Replay(ctx, runID, "", fn) })
	if len(got) != 3 {
		t.Fatalf("Replay yielded %d events, want 3", len(got))
	}
	for i, want := range []string{RunStarted, StepCompleted, RunFinished} {
		if got[i].Name != want {
			t.Errorf("event %d is %s, want %s", i, got[i].Name, want)
		}
		if got[i].ID == "" {
			t.Errorf("event %d has no id", i)
		}
		var payload map[string]string
		if err := json.Unmarshal(got[i].Data, &payload); err != nil {
			t.Errorf("event %d payload is not JSON: %v", i, err)
		} else if payload["name"] != want {
			t.Errorf("event %d payload = %v, want %s", i, payload, want)
		}
	}
}

// A reconnecting client resumes after its Last-Event-ID and is sent nothing
// twice. The range is exclusive, so no sequence number is incremented by hand.
func TestReplayAfterAnIDIsExclusive(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	for i := 0; i < 3; i++ {
		if err := c.Publish(ctx, runID, Event{Name: StepCompleted, Payload: map[string]int{"step": i + 1}}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	all := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Replay(ctx, runID, "", fn) })
	if len(all) != 3 {
		t.Fatalf("Replay yielded %d events, want 3", len(all))
	}

	got := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Replay(ctx, runID, all[0].ID, fn) })
	if len(got) != 2 {
		t.Fatalf("resuming after %s yielded %d events, want 2", all[0].ID, len(got))
	}
	if got[0].ID == all[0].ID {
		t.Errorf("resuming after %s delivered it again", all[0].ID)
	}
}

// An error returned by the callback stops the iteration and comes back
// unchanged, which is how the SSE handler ends a request from inside a frame
// write.
func TestReplayStopsAtTheCallbacksError(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	for i := 0; i < 3; i++ {
		if err := c.Publish(ctx, runID, Event{Name: StepCompleted, Payload: i}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	seen := 0
	_, err := c.Replay(ctx, runID, "", func(ReadEvent) error {
		seen++
		return errTest
	})
	if err != errTest {
		t.Errorf("Replay returned %v, want the callback's own error", err)
	}
	if seen != 1 {
		t.Errorf("the callback saw %d events, want it to stop at the first", seen)
	}
}

// Follow blocks and returns having yielded nothing when the block expires.
// redis.Nil is the idle path, not a failure.
func TestFollowReturnsNothingWhenNothingArrives(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()

	start := time.Now()
	got := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Follow(ctx, runID, "", fn) })
	if len(got) != 0 {
		t.Errorf("Follow yielded %d events on an empty stream", len(got))
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("Follow returned after %s, want it to block for its configured 200ms", elapsed)
	}
}

// An empty replay means the stream is empty, not that it is up to date. The
// follow therefore starts at 0 rather than $, and an event published after the
// replay is still delivered — the window S8 rejected $ for.
func TestFollowAfterAnEmptyReplayDeliversWhatArrivesNext(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	if got := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Replay(ctx, runID, "", fn) }); len(got) != 0 {
		t.Fatalf("a stream that was never written replayed %d events", len(got))
	}
	if err := c.Publish(ctx, runID, Event{Name: RunStarted, Payload: map[string]string{"id": runID}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Follow(ctx, runID, "", fn) })
	if len(got) != 1 || got[0].Name != RunStarted {
		t.Fatalf("Follow yielded %v, want the event published after the replay", got)
	}
}

// Follow returns as soon as an event arrives, rather than waiting out its
// block.
func TestFollowWakesOnAPublish(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	publisher := testClient(t)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = publisher.Publish(ctx, runID, Event{Name: StepCompleted, Payload: map[string]int{"step": 1}})
	}()

	got := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Follow(ctx, runID, "", fn) })
	if len(got) != 1 || got[0].Name != StepCompleted {
		t.Fatalf("Follow yielded %v, want the published step", got)
	}
}

// A client disconnecting is this endpoint's normal ending, so a cancelled
// context is not an error and every caller is spared filtering one out.
func TestCancellationIsNotAnError(t *testing.T) {
	c := testClient(t)
	runID := id.New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Replay(ctx, runID, "", func(ReadEvent) error { return nil }); err != nil {
		t.Errorf("Replay on a cancelled context: %v", err)
	}
	if _, err := c.Follow(ctx, runID, "", func(ReadEvent) error { return nil }); err != nil {
		t.Errorf("Follow on a cancelled context: %v", err)
	}
}

// A restarted run trims its stream, so a client resuming from an older id
// receives the new attempt's run.started and clears its timeline — ADR 0009's
// mechanism, read from the side that has to render it.
func TestAReaderResumingAcrossARestartSeesTheNewAttempt(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	runID := id.New()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	if err := c.Publish(ctx, runID, Event{Name: RunStarted, Payload: map[string]int{"attempt": 1}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	first := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Replay(ctx, runID, "", fn) })
	if len(first) != 1 {
		t.Fatalf("the first attempt replayed %d events, want 1", len(first))
	}

	if err := c.Trim(ctx, runID); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if err := c.Publish(ctx, runID, Event{Name: RunStarted, Payload: map[string]int{"attempt": 2}}); err != nil {
		t.Fatalf("Publish after the trim: %v", err)
	}

	got := collect(t, func(fn func(ReadEvent) error) (string, error) { return c.Replay(ctx, runID, first[0].ID, fn) })
	if len(got) != 1 || got[0].Name != RunStarted {
		t.Fatalf("resuming across the restart yielded %v, want the new attempt's run.started", got)
	}
}

// A malformed entry is skipped, and the cursor still moves past it.
//
// This is the regression for a spin: deliver used to skip without reporting
// the id, so the caller re-read the same entry on every round — and XREAD
// returns immediately while an entry is waiting, so an idle Follow stopped
// idling. The assertion is the wall clock: the second Follow has nothing left
// to read and must wait out its block.
func TestFollowAdvancesPastAMalformedEntry(t *testing.T) {
	c := testClient(t)
	runID := id.New()
	ctx := context.Background()
	t.Cleanup(func() { c.rdb.Del(ctx, StreamKey(runID)) })

	// An entry Publish would never write: the data field is missing.
	if err := c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey(runID),
		Values: map[string]any{FieldEvent: RunStarted, FieldRunID: runID},
	}).Err(); err != nil {
		t.Fatalf("writing a malformed entry: %v", err)
	}

	var yielded int
	count := func(ReadEvent) error { yielded++; return nil }

	last, err := c.Follow(ctx, runID, "", count)
	if err != nil {
		t.Fatalf("following: %v", err)
	}
	if yielded != 0 {
		t.Errorf("the malformed entry was yielded %d times, want 0", yielded)
	}
	if last == "" {
		t.Fatal("Follow reported no id read, so the caller would re-read the malformed entry forever")
	}

	start := time.Now()
	if _, err := c.Follow(ctx, runID, last, count); err != nil {
		t.Fatalf("following again: %v", err)
	}
	if elapsed := time.Since(start); elapsed < c.cfg.ReadBlock {
		t.Errorf("the second Follow returned after %s, want it to block for %s", elapsed, c.cfg.ReadBlock)
	}
}
