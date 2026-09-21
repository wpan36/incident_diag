// Package events is the run event bus: one Redis stream per run, written
// here and read by the SSE endpoint in M26.
//
// Three event types, not nine. The only consumer is a browser timeline, and a
// timeline needs three things: the run started, what each step did and saw,
// the run ended. Finer granularity — retrieval.started, plan.created — is
// sub-step detail the UI would render as a single line anyway.
//
// Two rules govern everything here, both from ADR 0001: nothing authoritative
// exists only in Redis, and a failed publish is logged and ignored. The MySQL
// row is written before its event is published, and a reconnecting client
// reads GET /api/runs/{id} before it subscribes — so a lost event costs a live
// timeline, never a fact.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/wpan36/incident_diag/internal/config"
)

// The three event names. They are constants rather than configuration for the
// same reason mq's topic names are: they are the contract between a worker and
// a browser in a different process.
//
// run.started means "clear the timeline". A restarted run re-uses step numbers
// 1..n, so a client that drops duplicates by step number would discard the new
// attempt's steps as copies of the old one's — the opposite of what ADR 0009
// promises. Dropping by step number is therefore scoped to the current
// attempt, and run.started ends the previous one.
const (
	RunStarted    = "run.started"
	StepCompleted = "step.completed"
	RunFinished   = "run.finished"
)

// Stream entry field names. Splitting the event name out of the JSON is what
// lets M26 fill SSE's event: line without decoding the body.
const (
	FieldEvent = "event"
	FieldRunID = "run_id"
	FieldData  = "data"
)

// Event is one entry in a run's stream: the name that says what happened and
// the payload the client renders.
//
// There is no id and no timestamp. Redis assigns each entry a <ms>-<seq> id,
// and that id is SSE's Last-Event-ID, so inventing either here would be a
// second, disagreeing answer to a question Redis has already settled.
type Event struct {
	// Name is one of the three constants above.
	Name string

	// Payload is marshalled into the entry's data field. It is a wire type —
	// wire.Run or wire.Step — so the front end has one type per entity rather
	// than two that have to agree.
	Payload any
}

// Publisher is what the agent worker writes events through. The interface
// exists so the worker's tests can assert on what was published and can make
// a publish fail, which is the one behaviour worth proving: a failed publish
// must not fail the run.
type Publisher interface {
	// Publish appends one event to the run's stream.
	Publish(ctx context.Context, runID string, e Event) error

	// Trim empties a run's stream, which a restarted run does before it
	// publishes run.started.
	Trim(ctx context.Context, runID string) error
}

// Client is the Redis-backed Publisher.
type Client struct {
	rdb    *redis.Client
	cfg    config.Events
	logger *slog.Logger
}

// New opens the Redis connection.
//
// redis.ParseURL means the connection string is one variable rather than a
// host, a port and a password. Nothing is dialled here: go-redis connects
// lazily, which is the same position cmd/api takes on Kafka — a Redis that is
// down costs a live timeline, and refusing to start over it would cost the
// runs themselves.
func New(cfg config.Events, logger *slog.Logger) (*Client, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("events: REDIS_URL is not a valid redis connection string: %w", err)
	}
	return &Client{rdb: redis.NewClient(opts), cfg: cfg, logger: logger}, nil
}

// StreamKey is the key holding one run's events.
//
// One stream per run rather than one for everything: a shared stream would
// have every SSE client reading every run's events and discarding most of
// them, and its MAXLEN would evict a quiet run's history because a busy one
// filled it.
func StreamKey(runID string) string { return "run:" + runID + ":events" }

// Publish appends one event to the run's stream and refreshes its TTL.
//
// The caller has already written the row this event describes. A failure here
// is therefore logged and returned, and every caller in this project ignores
// the return: see the package comment.
//
// The timeout is its own, derived from ctx rather than replacing it. Without
// one, an unresponsive Redis would block a run inside the call whose whole
// point is that its failure does not matter.
func (c *Client) Publish(ctx context.Context, runID string, e Event) error {
	data, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("events: encode %s for run %s: %w", e.Name, runID, err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.PublishTimeout)
	defer cancel()

	key := StreamKey(runID)
	// MAXLEN ~ rather than an exact trim: Redis then removes whole nodes and
	// the cap is approximate, which costs nothing when MaxSteps is a single
	// digit and the limit is a thousand.
	err = c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		MaxLen: int64(c.cfg.StreamMaxLen),
		Approx: true,
		Values: map[string]any{FieldEvent: e.Name, FieldRunID: runID, FieldData: data},
	}).Err()
	if err != nil {
		return fmt.Errorf("events: publish %s for run %s: %w", e.Name, runID, err)
	}

	// Refreshed on every publish rather than set once at the first, so a run
	// that is still producing events cannot expire underneath a client
	// watching it.
	if err := c.rdb.Expire(ctx, key, c.cfg.StreamTTL).Err(); err != nil {
		return fmt.Errorf("events: refresh the ttl of run %s: %w", runID, err)
	}
	return nil
}

// Trim empties a run's stream.
//
// XTRIM MAXLEN 0 rather than DEL: Redis keeps a trimmed stream's last
// generated id, so the new attempt's entries are guaranteed to sort after the
// old ones, and the key keeps its TTL. ADR 0009 promises that a restarted
// timeline begins again rather than showing a spliced history, and a stream
// still holding the previous attempt's step.completed entries would break that
// promise for any client resuming from an older Last-Event-ID.
func (c *Client) Trim(ctx context.Context, runID string) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.PublishTimeout)
	defer cancel()

	if err := c.rdb.XTrimMaxLen(ctx, StreamKey(runID), 0).Err(); err != nil {
		return fmt.Errorf("events: trim the stream of run %s: %w", runID, err)
	}
	return nil
}

// Ping reports whether Redis is reachable.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.PublishTimeout)
	defer cancel()

	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("events: ping redis: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (c *Client) Close() error { return c.rdb.Close() }
