# internal/events

## Purpose

The run event bus: one Redis stream per run, written here and read by the SSE endpoint in
M26. Specified by `docs/plans/agent-execution-and-events.md` (S8).

## Contents

- `events.go` — the three event names, the stream entry field names, `Event`, the
  `Publisher` interface, the Redis-backed `Client` with `Publish`, `Trim`, `Ping` and
  `Close`, and `StreamKey`.
- `fake.go` — `FakePublisher`, which records what would have been published.
- `events_test.go` — the stream key, the event names, the fake.
- `integration_test.go` — `//go:build integration`. Real Redis, via `TEST_REDIS_URL`.

## How it fits in

`internal/agentrun` writes; nothing reads yet. Payloads are `internal/wire` types, so the
browser gets the same JSON here that `GET /api/runs/{id}` returns. `cmd/agent-worker` owns
the `Client`'s lifetime.

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
  run started, what each step did, and the run ended. `step.started` and `answer.delta` are
  additive and belong to M27/M28, once there is a UI to judge the need with.
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
- **Integration tests use `TEST_REDIS_URL`**, which points at a different Redis database
  from `REDIS_URL` so a test run cannot disturb a locally running worker's streams.
