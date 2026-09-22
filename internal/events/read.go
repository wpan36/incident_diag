package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// ReadEvent is one entry read back from a run's stream.
//
// It is not Event. Event is the write side's shape — a name and a Payload any
// — and carries no id on purpose, because Redis assigns one. What comes back
// has that id and a payload that is still bytes: the SSE endpoint copies it
// onto the wire without decoding it.
type ReadEvent struct {
	// ID is Redis's <ms>-<seq>, which is SSE's Last-Event-ID.
	ID string

	// Name is one of the three event names.
	Name string

	// Data is the payload, still encoded.
	Data json.RawMessage
}

// Reader is what the SSE endpoint reads a run's stream through.
//
// It is a second interface rather than three more methods on Publisher.
// FakePublisher exists so internal/agentrun can prove that a failed publish
// does not fail a run; making it implement a read it never calls would be a
// method with nothing to mean.
//
// Both methods take a callback rather than returning a channel or an iterator:
// a channel would need a goroutine, which the SSE handler deliberately does
// not have. An error returned by fn stops the iteration and comes back out
// unchanged, which is how the handler ends a request from inside a frame
// write.
//
// Cancellation is not an error. When ctx ends, both return nil — a client
// disconnecting is this endpoint's normal ending, and making every caller
// filter context.Canceled out of its logging is a trap.
type Reader interface {
	// Replay yields every entry after `after`, oldest first, and returns when
	// the stream's current end is reached. An empty `after` starts at the
	// beginning.
	Replay(ctx context.Context, runID, after string, fn func(ReadEvent) error) error

	// Follow blocks for at most ReadBlock and yields whatever arrived,
	// returning nil having yielded nothing when the block expires.
	Follow(ctx context.Context, runID, after string, fn func(ReadEvent) error) error
}

// Replay implements Reader.
//
// The range is exclusive of `after` — XRANGE key (<id> +, Redis 6.2 and later
// — so a reconnecting client is never sent an event twice and no sequence
// number is incremented by hand.
func (c *Client) Replay(ctx context.Context, runID, after string, fn func(ReadEvent) error) error {
	start := "-"
	if after != "" {
		start = "(" + after
	}

	readCtx, cancel := context.WithTimeout(ctx, c.cfg.PublishTimeout)
	defer cancel()

	msgs, err := c.rdb.XRange(readCtx, StreamKey(runID), start, "+").Result()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("events: replay the stream of run %s: %w", runID, err)
	}
	return c.deliver(runID, msgs, fn)
}

// Follow implements Reader.
//
// The context outlives the block it asked for: REDIS_PUBLISH_TIMEOUT is this
// package's one "a Redis round trip should not take longer than this" knob, so
// the deadline is the block plus that, not the block alone.
//
// redis.Nil from a blocked XREAD is the block expiring, not a failure. It is
// this call's normal idle result.
func (c *Client) Follow(ctx context.Context, runID, after string, fn func(ReadEvent) error) error {
	// Never $. An empty replay means the stream is empty, not that it is up to
	// date, and $ would lose whatever was published between the two calls —
	// the window S8 rejected it for.
	last := after
	if last == "" {
		last = "0"
	}

	readCtx, cancel := context.WithTimeout(ctx, c.cfg.ReadBlock+c.cfg.PublishTimeout)
	defer cancel()

	streams, err := c.rdb.XRead(readCtx, &redis.XReadArgs{
		Streams: []string{StreamKey(runID), last},
		Block:   c.cfg.ReadBlock,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) || ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("events: follow the stream of run %s: %w", runID, err)
	}

	for _, s := range streams {
		if err := c.deliver(runID, s.Messages, fn); err != nil {
			return err
		}
	}
	return nil
}

// deliver hands each entry to fn, stopping at the first error fn returns.
//
// An entry missing either field is skipped rather than failing the stream:
// only this package writes these entries, so one that does not parse is a bug
// or a manual XADD, and neither is worth ending a client's timeline over.
func (c *Client) deliver(runID string, msgs []redis.XMessage, fn func(ReadEvent) error) error {
	for _, m := range msgs {
		ev, ok := readEvent(m)
		if !ok {
			c.logger.Warn("skipping a malformed entry in a run's event stream",
				"run_id", runID, "entry_id", m.ID)
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

// readEvent converts one stream entry, reporting whether it had the fields
// Publish writes.
func readEvent(m redis.XMessage) (ReadEvent, bool) {
	name, _ := m.Values[FieldEvent].(string)
	data, _ := m.Values[FieldData].(string)
	if name == "" || data == "" {
		return ReadEvent{}, false
	}
	return ReadEvent{ID: m.ID, Name: name, Data: json.RawMessage(data)}, true
}
