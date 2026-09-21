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
