# Async Messaging and Idempotency

**Tier A spec (S2).** Gates M6. The mechanisms defined here are used by M10
(ingestion worker) and M25 (agent worker).

## Problem

Two kinds of work have to leave the HTTP request and finish in a background process:
ingesting a document and running an agent. Kafka carries that work, and because delivery is
at-least-once (ADR 0003), every consumer must be idempotent.

S1 built the idempotency primitives — `ClaimDocument` and `ClaimRun` are conditional
updates that return false on a redelivery — and then deferred one thing to this spec: what
reclaims a row that is stuck. That deferral turns out to cover two separate failures which
are really the same failure.

**A row was created but never enqueued.** `POST /api/documents` writes a `PENDING` row and
then produces to Kafka. If the produce fails, or the process dies between the two, the row
sits `PENDING` forever and no message will ever arrive for it. This is the dual-write
problem, and S1 did not address it at all.

**A row was claimed but never finished.** A worker claims a document (`PENDING →
PROCESSING`) and then crashes. Even if the offset was never committed and the message is
redelivered, `ClaimDocument` now returns false because the status is `PROCESSING`, so the
worker skips it. The document is stuck permanently. For runs this is worse, because
`uniq_active_run` means the incident then rejects every new run with 409.

Both are the same condition: **a row is in a non-terminal state and nothing will ever send
a message for it again.** One mechanism fixes both.

## Technical Plan

### The governing principle

**Kafka offsets are not the record of progress. MySQL is.**

Offsets exist only to avoid redundant work. Every message is committed once its handler
returns, whether the work succeeded, was skipped as a duplicate, or failed permanently —
because in all three cases the outcome is already recorded in MySQL. Nothing about
correctness depends on an offset.

This is what makes the rest of the design simple, and it is the opposite of the common
instinct to withhold a commit until the work succeeds. Withholding blocks the partition
behind a message that will never succeed, which converts one bad document into a stalled
pipeline.

### Topics

| Topic | Partitions | Replication | Key |
| --- | --- | --- | --- |
| `documents.ingest.v1` | 3 | 1 | `document_id` |
| `agent.runs.v1` | 3 | 1 | `run_id` |

Three partitions on a single broker, so consumer group assignment and rebalancing are real
behaviour that integration tests can exercise, and so work can actually run in parallel.
Replication 1 because there is one broker; ADR 0005 already rules out high availability.

Keying by entity ID puts every message about one document on one partition, so redeliveries
and a reconciler's re-enqueue cannot be processed concurrently by two consumers.

Topics are created explicitly and idempotently at worker startup through franz-go's `kadm`.
Broker auto-creation is disabled: it silently invents a topic with default settings when a
name is misspelled, and it would give one partition rather than three.

Retention is the broker default of seven days. Messages are disposable — a lost message
costs a re-enqueue from the reconciler, never a lost document.

### Message format

Messages are **thin**. They carry the entity's ID and nothing else of substance:

```json
{
  "schema_version": 1,
  "message_id": "01JBQ8K3M7VXFZ2N9WQYRT4HCD",
  "produced_at": "2026-09-20T22:04:11.482913Z",
  "document_id": "01JBQ8K2X4RN0P7ZC3VD8SMGWA"
}
```

`agent.runs.v1` is identical with `run_id` in place of `document_id`.

A fat message carrying the document's `service`, `document_type` or path would be a second
copy of state that MySQL owns, and it goes stale the moment anything edits the row between
produce and consume. The worker looks the row up; that read is cheap and always current.

`message_id` is for correlating logs, not for deduplication — the status column does that.

**Headers**, not payload fields, carry transport concerns: `content-type:
application/json` and the W3C `traceparent` that M29 needs to join the producer's span to
the consumer's.

Decoding tolerates unknown fields so a later version can add one. A message with a
`schema_version` this build does not know is a permanent failure, handled as below.

### Producing

franz-go with the idempotent producer enabled and `acks=all`. A produce that the broker
did not acknowledge is a failure the caller sees.

`POST /api/documents` writes the row, then produces with a timeout, then responds **201
regardless of whether the produce succeeded**, logging a produce failure at error level.

This looks wrong and is deliberate. The document genuinely was created, so 500 would be a
lie, and a client that retried on 500 would upload a second copy. The row is `PENDING`, the
reconciler will enqueue it within a minute, and the only cost of the failed produce is that
delay. Being able to answer honestly here is the first payoff of having a reconciler.

### Consuming

One consumer group per worker: `ingestion-worker` on `documents.ingest.v1`, `agent-worker`
on `agent.runs.v1`. Automatic commit is disabled; records are committed explicitly after
their handler returns.

Records are handled one at a time in poll order and the batch is committed afterwards.
Parallelism comes from partitions and from running more worker instances, not from
concurrency inside one worker — which keeps the failure model small enough to reason about.

**A handler has exactly three outcomes, and all three commit:**

| Outcome | Meaning | MySQL state |
| --- | --- | --- |
| Done | the work completed | `READY`, or `FAILED` with a reason |
| Skipped | the claim returned false; another delivery already has it | unchanged |
| Failed permanently | undecodable message, unknown `schema_version`, or bounded retries exhausted | `FAILED` with a reason |

Transient failures — the embedding API returning 503, Elasticsearch briefly unavailable —
are retried **inside the handler** with bounded backoff, and become a permanent failure
once the budget is spent. They are never retried by withholding a commit.

If the message names a row that does not exist at all, there is nothing to mark. The
handler logs at error level and commits, because redelivering it forever cannot help.

**Long-running handlers.** An agent run may take minutes, up to `MaxRunDuration`. If the
handler blocks longer than the group's rebalance timeout, the broker evicts the member, the
message is redelivered, and `ClaimRun` then refuses it because the row is `RUNNING`. The
`agent-worker` group therefore sets session and rebalance timeouts comfortably above
`MaxRunDuration`.

This couples two settings, and the coupling has to be written down rather than discovered:
**raising `MaxRunDuration` requires raising the agent worker's rebalance timeout.** The
worker validates this at startup and refuses to run if the timeout is not greater.

### Lease reclaim

`ClaimDocument` and `ClaimRun` gain a lease clause, so a row abandoned long enough can be
claimed again:

```sql
UPDATE documents
   SET status = 'PROCESSING', attempts = attempts + 1,
       processing_started_at = ?, updated_at = ?
 WHERE id = ?
   AND ( status IN ('PENDING','FAILED')
      OR ( status = 'PROCESSING' AND processing_started_at < ? ) )
```

The last parameter is `now - lease`. `ClaimRun` is the same shape against `started_at`.

The lease must exceed the longest legitimate processing time, or a slow document gets
claimed twice while the first attempt is still working. Ingestion's lease defaults to 10
minutes; the run lease defaults to `MaxRunDuration` plus a margin and is validated at
startup the same way the rebalance timeout is.

Two workers processing the same document after a lease expiry is possible and harmless:
indexing deletes the document's existing chunks before writing, so the second run converges
on the same result rather than duplicating chunks.

### The reconciler

A goroutine owned by the worker, started with it and stopped by its context, sweeping every
`RECONCILE_INTERVAL` (default 30s). Each sweep produces a message for up to
`RECONCILE_BATCH` (default 100) rows in each of three categories:

| Category | Condition | Fixes |
| --- | --- | --- |
| Never enqueued | `status = 'PENDING'` and `updated_at < now - 60s` | a failed or lost produce |
| Abandoned | `status = 'PROCESSING'` and `processing_started_at < now - lease` | a crashed worker |
| Retryable failure | `status = 'FAILED'` and `attempts < 3` and `updated_at < now - backoff(attempts)` | a transient outage |

Backoff on attempts is 1, 5 and 15 minutes. After three attempts a document stays `FAILED`
with its reason visible, and needs a human.

The reconciler only produces messages. It never changes status itself, so there is exactly
one place where a state transition happens — the claim — and the reconciler cannot race it.

The batch limit matters: without it, a backlog that builds up while Kafka is down becomes a
thundering herd the moment it returns.

The reconciler is the reason this design needs no transactional outbox. It performs the
same job — making sure every row that should have a message eventually gets one — driven
off the status column that already exists, with the `(status, id)` index S1 created for
exactly this.

### Package layout

`internal/mq`, thin wrappers over franz-go rather than an abstraction over messaging:

- `Producer` — `Produce(ctx, topic, key string, msg any) error`, envelope encoding and
  trace headers included. An interface, because the reconciler's unit tests need a fake.
- `Consumer` — a group consumer driven by a `Handler func(ctx, Envelope) error`, owning
  the poll and commit loop and exiting on context cancellation.
- `EnsureTopics(ctx, brokers, specs) error` — idempotent creation.
- `Envelope` — encode and decode, with the schema version check.

The ingestion and agent handlers live in their own packages. M6 delivers `internal/mq`, the
topics, the compose service and the reconciler's policy; the real ingestion handler is M10.

### Store additions

- `ClaimDocument` and `ClaimRun` take a lease duration.
- `ListDocumentsToReconcile(ctx, policy, limit) ([]string, error)` returning IDs only,
  one query per category or one with a compound predicate.
- The equivalent for runs lands with M25.

### Configuration added

| Variable | Default | |
| --- | --- | --- |
| `KAFKA_BROKERS` | — | required, comma separated |
| `KAFKA_PRODUCE_TIMEOUT` | `10s` | |
| `INGEST_LEASE` | `10m` | |
| `INGEST_MAX_ATTEMPTS` | `3` | |
| `RECONCILE_INTERVAL` | `30s` | |
| `RECONCILE_PENDING_AFTER` | `60s` | |
| `RECONCILE_BATCH` | `100` | |
| `TEST_KAFKA_BROKERS` | — | integration tests skip without it |

Consumer group names and topic names are constants, not configuration: they are part of the
contract between producer and consumer, and making them settable invites two services
disagreeing.

### Compose

Kafka in KRaft mode, single node, with a health check, alongside the existing MySQL
service. `make up` starts both; `make test-integration` additionally requires
`TEST_KAFKA_BROKERS`.

## Alternatives

**Transactional outbox.** The textbook answer to dual writes: insert the row and an outbox
row in one transaction, and have a relay publish from the outbox. It is strictly more
correct for the enqueue path, and it is a term worth being able to discuss. Rejected
because it solves only half the problem — a row stuck in `PROCESSING` still needs a lease
and something to re-enqueue it — so the project would carry both an outbox table with a
relay process *and* a reconciler. The reconciler alone covers both cases, reading a status
column that already exists.

**A manual re-ingest endpoint instead of automatic reclaim.** Least code, and stuck work
stays visible. Rejected because the system then does not heal, so any demo that hits a
transient failure needs hand-holding, and it would add an endpoint S1's API contract does
not have.

**Withholding the commit until the work succeeds.** The common instinct, and wrong here: a
message that can never succeed blocks its partition forever.

**Kafka exactly-once semantics.** Already rejected in ADR 0003 — the side effects that need
protecting are in MySQL and Elasticsearch, which Kafka transactions do not cover.

**One partition per topic.** Simplest and globally ordered, but consumer group assignment,
rebalancing and parallel processing all become untestable, and those are among the things
this milestone exists to demonstrate.

**Fat messages carrying document metadata.** Saves a database read, at the cost of a second
copy of state that can go stale between produce and consume.

**Treating `FAILED` as terminal.** Simpler semantics, but a five-second outage of the
embedding provider would then require re-ingesting every affected document by hand.

## Detailed Implementation

**M6 delivers:**

- `deploy/docker-compose.yml` — Kafka (KRaft, single node) with a health check.
- `internal/mq` — `Envelope` encode/decode, `Producer`, group `Consumer`, `EnsureTopics`,
  plus a fake producer for tests.
- `internal/config` — the Kafka and reconciler variables above, using the existing typed
  helpers.
- `internal/store` — lease parameters on both claim methods; `ListDocumentsToReconcile`.
- `internal/reconcile` — the sweep policy as a pure function over row state plus a runner
  goroutine with an explicit lifecycle.
- `cmd/api` — produce to `documents.ingest.v1` after creating a document; still 201 on
  produce failure.
- A placeholder consumer that claims and immediately marks the document failed with "not
  implemented", so the loop is exercisable end to end before M10 replaces it.

**Test isolation.** Integration tests suffix topic and consumer group names with a ULID, so
parallel or repeated runs cannot see each other's messages, and no test depends on a
cleanly wiped broker.

## Verification

- `make check` — envelope encoding including unknown fields and an unknown schema version;
  the reconciler policy as a table test over row age, status and attempt count; backoff
  computation. None of it needs infrastructure.
- `make test-integration` with Kafka and MySQL running:
  - produce and consume a message; the handler sees it exactly once
  - deliver the same message twice; exactly one claim succeeds, the second is skipped
  - kill and restart a consumer mid-batch; no work is lost
  - two consumers in one group; partitions are assigned across both
  - a `PENDING` row that was never produced is picked up by the reconciler
  - a stale `PROCESSING` row can be reclaimed; a fresh one cannot
  - a `FAILED` row under the attempt limit is retried after its backoff; one over the
    limit is left alone
- Manual smoke: upload a document with Kafka stopped, observe the 201 and the logged
  produce failure, start Kafka, and watch the reconciler enqueue it within a minute.

## Known limitations, accepted

**Every worker instance sweeps.** Running two ingestion workers means two reconcilers
producing the same re-enqueue messages. It is harmless — the claim is conditional, so only
one wins — but it is wasted work. A `GET_LOCK` around the sweep would fix it and is not
worth the complexity while Compose runs one instance.

**The reconciler polls.** Every 30 seconds against an indexed status column, which is
cheap, but it is polling rather than an event.

**A document that fails three times needs a human**, and there is no endpoint for that
person to use. It is visible through `GET /api/documents` with its `failure_reason`.

**Two coupled settings.** `MaxRunDuration` must stay below the agent worker's rebalance
timeout, and the run lease must stay above it. The worker validates both at startup rather
than trusting anyone to remember.
