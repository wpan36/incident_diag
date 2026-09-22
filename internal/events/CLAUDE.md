# internal/events

## Purpose

The run event bus: one Redis stream per run, written by the agent worker and read by the
SSE endpoint. Specified by `docs/plans/agent-execution-and-events.md` (S8) for the write
side and `docs/plans/sse-streaming.md` (S9) for the read side.

## Contents

- `events.go` — the three event names, the stream entry field names, `Event`, the
  `Publisher` interface, the Redis-backed `Client` with `Publish`, `Trim`, `Ping` and
  `Close`, and `StreamKey`.
- `read.go` — `ReadEvent`, the `Reader` interface, and the `Client`'s `Replay` (`XRANGE`)
  and `Follow` (`XREAD BLOCK`).
- `fake.go` — `FakePublisher`, which records what would have been published, and
  `FakeReader`, a slice-backed `Reader` with an `Add` a test publishes through, plus `Fail`
  and `SetBlock`.
- `events_test.go` — the stream key, the event names, the fakes.
- `integration_test.go` — `//go:build integration`. Real Redis, via `TEST_REDIS_URL`.

## How it fits in

`internal/agentrun` writes and `internal/api`'s SSE endpoint reads. Payloads are
`internal/wire` types, so the browser gets the same JSON here that `GET /api/runs/{id}`
returns. `cmd/agent-worker` and `cmd/api` each own a `Client`.

## Gotchas

- **The MySQL row is written before its event is published, and a failed publish is logged
  and ignored.** ADR 0001: nothing authoritative lives only in Redis. A reconnecting client
  reads `GET /api/runs/{id}` and *then* subscribes, so it can never see an event for
  something unpersisted. A lost event costs a live timeline, never a fact.
- **`run.started` means "clear the timeline".** A restarted run re-uses step numbers 1..n,
  so a client dropping duplicates by step number would discard the new attempt's steps as
  copies of the old one's. Dropping by step number is scoped to the current attempt, and
  `run.started` ends the previous one. `Run.attempts` says which attempt has begun.
- **Three event types, not nine.** The only consumer is a browser timeline, which needs the
  run started, what each step did, and the run ended. `answer.delta` was cancelled outright
  (ADR 0010); anything further is additive and waits for a UI to judge the need with.
- **`Reader` is a second interface, not three more methods on `Publisher`.** `FakePublisher`
  exists so `internal/agentrun` can prove a failed publish does not fail a run; making it
  implement a read it never calls would be a method with nothing to mean.
- **A callback, not a channel or an iterator.** A channel needs a goroutine, and the SSE
  handler deliberately owns none — it blocks in `Follow` itself. An error from the callback
  stops the iteration and comes back unchanged, which is how the handler ends a request
  from inside a frame write.
- **Both reads return the last entry id they *read*, which is the caller's next `after`.**
  Not the id of the last entry they yielded: a malformed entry is skipped and still moves
  the cursor. A caller that tracked only deliveries would ask for that entry on every round,
  and `XREAD` returns immediately while an entry is waiting — so the SSE loop spun at full
  CPU, re-reading the run from MySQL each time, instead of idling. Measured at ~40k rounds
  per second before the fix.
- **`Replay`'s range is exclusive** — `XRANGE key (<id> +`, Redis 6.2 and later — so a
  reconnecting client is never sent an event twice and no sequence number is incremented by
  hand.
- **`Follow` starts at `0`, never `$`.** An empty replay means the stream is empty, not that
  it is up to date, and `$` would lose whatever was published between the two calls.
- **Cancellation is not an error.** Both reads return nil when the caller's context ends: a
  client disconnecting is the SSE endpoint's normal ending, and making every caller filter
  `context.Canceled` out of its logging is a trap. `redis.Nil` from a blocked `XREAD` is
  likewise the block expiring, not a failure.
- **`Follow`'s context outlives the block it asked for** — `SSE_READ_BLOCK` plus
  `REDIS_PUBLISH_TIMEOUT` — or the deadline would fire on the read it is bounding.
- **No id and no timestamp in the envelope.** Redis assigns each entry a `<ms>-<seq>` id and
  that id *is* SSE's `Last-Event-ID`, so inventing either here would be a second,
  disagreeing answer.
- **`XTRIM MAXLEN 0`, never `DEL`.** Redis keeps a trimmed stream's last generated id, so a
  new attempt's entries are guaranteed to sort after the old ones and the key keeps its TTL.
  `DEL` drops both, leaving the two attempts ordered only by the wall clock.
- **Every call carries its own timeout**, derived from the caller's context rather than
  replacing it. Without one an unresponsive Redis would block a run inside the call whose
  whole point is that its failure does not matter.
- **The TTL is refreshed on every publish**, not set once, so a run still producing events
  cannot expire underneath a client watching it.
- **`New` dials nothing.** go-redis connects lazily, matching `cmd/api`'s position on Kafka:
  a Redis that is down costs a live timeline, and refusing to start over it would cost the
  runs themselves.
- **The event name is its own stream field**, separate from `data`, so M26 can fill SSE's
  `event:` line without decoding the payload.
- **One stream per run.** A shared stream would have every SSE client reading every run's
  events and discarding most of them, and its `MAXLEN` would evict a quiet run's history
  because a busy one filled it.
- **`REDIS_URL` may carry a password**, so `config.Events.String` does not print it.
- **`FakeReader`'s error and block go through `Fail` and `SetBlock`, not exported fields.**
  A test saying "Redis failed mid-stream" has to say it while the handler is already blocked
  in `Follow`, and an unsynchronised field would make that test a `-race` failure.
  `deliverFake` mirrors `Client.deliver`, skip rule included, so the fake cannot hide the
  spin that rule caused.
- **Integration tests use `TEST_REDIS_URL`**, which points at a different Redis database
  from `REDIS_URL` so a test run cannot disturb a locally running worker's streams.
