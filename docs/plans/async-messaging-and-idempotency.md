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

**The wire format is flat, and the Go types keep it flat.** `Envelope` holds only the three
fields every message shares and is embedded, not nested, so the JSON above is exactly what
goes on the wire — there is no `payload` object:

```go
type Envelope struct {
    SchemaVersion int       `json:"schema_version"`
    MessageID     string    `json:"message_id"`  // ULID, from internal/id
    ProducedAt    time.Time `json:"produced_at"`
}

type DocumentMessage struct {
    Envelope
    DocumentID string `json:"document_id"`
}

type RunMessage struct {
    Envelope
    RunID string `json:"run_id"`
}
```

One message type per topic rather than a generic envelope with a `json.RawMessage` payload:
a handler is written against the topic it consumes, so it should receive the type that topic
carries, checked at compile time, instead of decoding a second time. The cost is that adding
a topic means adding a type, which is the right amount of friction for a decision that
changes the contract between two services.

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

`KAFKA_PRODUCE_TIMEOUT` is spent **inside the HTTP request**: with the broker down, an
upload blocks for it before answering 201. It therefore has to stay comfortably below
`HTTP_WRITE_TIMEOUT` (30s by default), and the two are worth reading together before either
is changed.

### Consuming

One consumer group per worker: `ingestion-worker` on `documents.ingest.v1`, `agent-worker`
on `agent.runs.v1`. Automatic commit is disabled.

Records are handled one at a time in poll order, and **each record's offset is committed as
soon as its own handler returns** — not once per polled batch. A crash therefore replays at
most the one record that was in flight. Committing per batch would be fewer calls, but it
replays every already-finished record in the batch, and because `ClaimDocument` accepts
`FAILED` those replays would re-run documents that had just failed, burning an attempt and
skipping the reconciler's backoff. Per-record commit keeps that window to one message.
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

**A `FAILED` row does not record whether the failure was worth retrying.** `failure_reason`
is free text, so a file that can never be parsed and a five-second embedding outage leave
the same shape behind, and the reconciler will re-enqueue both until the attempt limit is
reached. A `retryable` flag on the row would separate them; it is not worth a schema change
while the attempt limit bounds the waste at two extra runs. Recorded as accepted below.

**Long-running handlers (M25, not M6).** An agent run may take minutes, up to
`MaxRunDuration`. If the handler blocks longer than the group's rebalance timeout, the
broker evicts the member, the message is redelivered, and `ClaimRun` then refuses it because
the row is `RUNNING`. The `agent-worker` group therefore sets session and rebalance timeouts
comfortably above `MaxRunDuration`.

This couples two settings, and the coupling has to be written down rather than discovered:
**raising `MaxRunDuration` requires raising the agent worker's rebalance timeout.** The
worker validates this at startup and refuses to run if the timeout is not greater.

None of this is implementable in M6: `MaxRunDuration` arrives with the bounded agent loop in
M23 and the `agent-worker` binary with M25. It is specified here because it constrains how
the run-side configuration is named and validated when M25 adds it, and because a consumer
group whose rebalance timeout is discovered after the fact is discovered through a production
symptom.

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
startup the same way the rebalance timeout is — both with M25.

Two workers processing the same document after a lease expiry is possible and harmless:
indexing deletes the document's existing chunks before writing, so the second run converges
on the same result rather than duplicating chunks.

### The reconciler

A goroutine owned by the worker, started with it and stopped by its context, sweeping every
`RECONCILE_INTERVAL` (default 30s). Each sweep produces a message for up to
`RECONCILE_BATCH` (default 100) rows in each of three categories:

| Category | Condition | Fixes |
| --- | --- | --- |
| Never enqueued | `status = 'PENDING'` and `updated_at < now - RECONCILE_PENDING_AFTER` | a failed or lost produce |
| Abandoned | `status = 'PROCESSING'` and `processing_started_at < now - lease` | a crashed worker |
| Retryable failure | `status = 'FAILED'` and `attempts < INGEST_MAX_ATTEMPTS` and `updated_at < now - backoff(attempts)` | a transient outage |

**The backoff schedule is 1 minute then 5 minutes, and that is the whole schedule.**
`attempts` is incremented by the claim, so after the first failure it reads 1, and the
document is re-enqueued once `updated_at` is `backoff(1) = 1m` old; after the second it
reads 2 and waits `backoff(2) = 5m`; after the third, `attempts < INGEST_MAX_ATTEMPTS` is
false and the row is left alone. Three attempts total, two waits. `backoff(n)` for any
`n` beyond the schedule returns the last entry, so raising `INGEST_MAX_ATTEMPTS` adds
5-minute retries rather than producing an undefined delay.

The queries order by `id`, so a backlog larger than `RECONCILE_BATCH` is drained oldest
first instead of in whatever order InnoDB happens to return.

The reconciler only produces messages. It never changes status itself, so there is exactly
one place where a state transition happens — the claim — and the reconciler cannot race it.

The price of not writing anything is that a row keeps matching until a worker claims it: a
sweep every 30 seconds re-produces the same message for a row that is enqueued but not yet
picked up. That is harmless and it is not free — see the accepted limitations.

The batch limit matters: without it, a backlog that builds up while Kafka is down becomes a
thundering herd the moment it returns.

The reconciler is the reason this design needs no transactional outbox. It performs the
same job — making sure every row that should have a message eventually gets one — driven
off the status column that already exists, with the `(status, id)` index S1 created for
exactly this.

### Package layout

`internal/mq`, thin wrappers over franz-go rather than an abstraction over messaging:

- `Envelope`, `DocumentMessage`, `RunMessage` — the types above, plus
  `NewDocumentMessage(id)` and `NewRunMessage(id)`, which stamp the schema version, a fresh
  ULID `message_id` and `produced_at`. Producers never fill an envelope by hand.
- `Producer` — `Produce(ctx, topic, key string, msg any) error`: JSON-marshals `msg` as it
  is, sets the `content-type` and `traceparent` headers, and waits for the broker's
  acknowledgement. It does not inject envelope fields — the constructors did that, which is
  why the wire format can stay flat. An interface, because the reconciler's unit tests need
  a fake.
- `Consumer` — a group consumer owning the poll and per-record commit loop and exiting on
  context cancellation, driven by `Handler func(ctx context.Context, rec Record) error`
  where `Record` carries the key, value and headers. `internal/mq` stays unaware of which
  message type a topic carries.
- `Decode[T](data []byte) (T, error)` — the generic decode the handler calls on
  `rec.Value`. It checks `schema_version` and returns a distinguishable
  `ErrUnknownSchemaVersion`, so the handler can mark the row `FAILED` rather than retry.
  Unknown fields are ignored.
- `EnsureTopics(ctx, brokers, specs) error` — idempotent creation.

**`EnsureTopics` runs in `ingestion-worker` and creates both topics**, even though nothing
consumes `agent.runs.v1` until M25: `cmd/api` is a producer only and does not create topics,
so an API started before the worker will fail its produce until the worker comes up. That is
the already-accepted 201-plus-reconciler path, not a new failure mode, and it keeps topic
creation in one place instead of racing two processes.

The ingestion and agent handlers live in their own packages. M6 delivers `internal/mq`, the
topics, the compose service and the reconciler's policy; the real ingestion handler is M10.

### Store additions

- `ClaimDocument` and `ClaimRun` take a lease duration.
- `ListDocumentsToReconcile(ctx, policy ReconcilePolicy, limit int) ([]ReconcileCandidate, error)`,
  where a candidate is the document ID and the category that matched, and `limit` applies
  **per category**. Returning the category rather than a bare ID is what lets the log and
  the later metric say whether a sweep is repairing lost produces or reclaiming crashed
  workers, which are very different things to see rising. One query per category, ordered
  by `id`.
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
| `KAFKA_PORT` | `9092` | compose only, published for the host |
| `TEST_KAFKA_BROKERS` | — | integration tests skip without it |

`KAFKA_PORT` exists for the same reason `MYSQL_PORT` does: a broker already listening on
9092 would otherwise break the setup quietly, and the published port has to agree with the
host half of `TEST_KAFKA_BROKERS`. All of these go into `.env.example` with the same kind
of comment the MySQL block has.

The run-side settings — the run lease and the agent worker's session and rebalance timeouts
— are **not** added here. They depend on `MaxRunDuration`, which M23 introduces, and they
belong to the `LoadAgentWorker` that M25 adds.

Consumer group names and topic names are constants, not configuration: they are part of the
contract between producer and consumer, and making them settable invites two services
disagreeing.

### Compose

Kafka in KRaft mode, single node, from the official `apache/kafka` image (combined
controller and broker, one listener published on `KAFKA_PORT` and an internal listener for
the worker container), alongside the existing MySQL service. The health check runs
`kafka-broker-api-versions.sh` against the broker's own listener, because `make up` uses
`--wait` and "the container started" is not the same as "the broker answers".

`make up` starts both. `make test-integration` gains a guard on `TEST_KAFKA_BROKERS`
alongside the existing `TEST_MYSQL_DSN` one, for the reason already written there: a run
that reports success while every Kafka test silently skipped is worse than one that fails.

The image choice is the one unilateral call in this spec — `apache/kafka` is the upstream
image and needs no vendor-specific environment variables, but `bitnami/kafka` would work
and is more widely written about.

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

**An envelope wrapping an opaque payload.** One `Handler func(ctx, Envelope) error` for
every topic, with the body as `json.RawMessage`. Rejected because it buys uniformity nobody
needs — there are two topics and each has exactly one consumer — and pays for it with a
nested wire format and a second decode step inside every handler.

**Committing once per polled batch.** Fewer commit calls, but a crash replays every record
in the batch, and a replay of an already-failed document re-runs it immediately instead of
after the reconciler's backoff.

**Another Kafka client.** ADR 0001 chose Kafka, not a client library. franz-go is picked
here: it supports KRaft, the idempotent producer and consumer groups without a separate
cluster-admin dependency, and `kadm` covers topic creation. `sarama` and
`confluent-kafka-go` would both work; the latter needs cgo and librdkafka, which is a real
cost for a project meant to build with `go build` alone.

## Detailed Implementation

**M6 delivers:**

- `deploy/docker-compose.yml` — Kafka (KRaft, single node) with a health check;
  `.env.example` and `Makefile` updated for the new variables and the integration guard.
- `internal/mq` — the message types and their constructors, `Decode`, `Producer`, group
  `Consumer`, `EnsureTopics`, plus a fake producer for tests.
- `internal/config` — the Kafka and reconciler variables above, using the existing typed
  helpers. `LoadKafka` and `LoadReconcile` rather than fields on `Load`, following the
  existing split by concern — `cmd/migrate` must not need a broker.
- `internal/store` — lease parameters on both claim methods; `ListDocumentsToReconcile`.
- `internal/reconcile` — the sweep policy as a pure function over row state plus a runner
  goroutine with an explicit lifecycle.
- `cmd/api` — produce to `documents.ingest.v1` after creating a document; still 201 on
  produce failure.
- **`cmd/ingestion-worker`** — a new binary, added here rather than at M10: `EnsureTopics`,
  the consumer group, the reconciler goroutine, and the same graceful shutdown path
  `cmd/api` uses. Its handler is a placeholder that claims the document and immediately
  marks it failed with "not implemented", so the whole loop — produce, consume, claim,
  terminal write, reconcile — is exercisable end to end and observable in a smoke test
  before M10 replaces the handler and nothing else.

The placeholder makes the retry policy visible: every uploaded document is claimed, failed,
re-enqueued after a minute, failed, re-enqueued after five, and left `FAILED` with three
attempts. That is the design working, and it is worth knowing before watching the logs.

**Test isolation.** Integration tests suffix topic and consumer group names with a ULID, so
parallel or repeated runs cannot see each other's messages, and no test depends on a
cleanly wiped broker.

## Verification

- `make check` — message encoding and decoding, including that the JSON is flat, that an
  unknown field is ignored and that an unknown `schema_version` returns
  `ErrUnknownSchemaVersion`; the reconciler policy as a table test over row age, status and
  attempt count; `backoff(n)` including `n` past the end of the schedule. None of it needs
  infrastructure.
- `make test-integration` with Kafka and MySQL running:
  - produce and consume a message; the handler sees it exactly once
  - deliver the same message twice; exactly one claim succeeds, the second is skipped
  - kill a consumer mid-batch and restart it; no work is lost, and only the record that was
    in flight is redelivered — the ones already committed are not
  - two consumers in one group; partitions are assigned across both
  - a `PENDING` row that was never produced is picked up by the reconciler
  - a stale `PROCESSING` row can be reclaimed; a fresh one cannot
  - a `FAILED` row under the attempt limit is retried after its backoff; one at the limit
    is left alone
  - `EnsureTopics` twice in a row succeeds and leaves three partitions
- Manual smoke: upload a document with Kafka stopped, observe the 201 and the logged
  produce failure, start Kafka, and watch the reconciler enqueue it within a minute — then
  watch the placeholder handler fail it and the retry schedule play out.

## Known limitations, accepted

**Every worker instance sweeps.** Running two ingestion workers means two reconcilers
producing the same re-enqueue messages. It is harmless — the claim is conditional, so only
one wins — but it is wasted work. A `GET_LOCK` around the sweep would fix it and is not
worth the complexity while Compose runs one instance.

**The reconciler polls.** Every 30 seconds against an indexed status column, which is
cheap, but it is polling rather than an event.

**A row is re-enqueued on every sweep until it is claimed.** Because the reconciler writes
nothing, a row it has already produced a message for still matches on the next sweep. When
the worker keeps up this costs at most one duplicate; when it does not — a single worker
draining a backlog serially at tens of seconds per document, which is exactly what M10's
embedding calls will look like — the same hundred rows are re-produced every 30 seconds for
as long as the drain takes. The claim keeps the result correct and the key keeps the
ordering safe, so the cost is wasted messages and noisy logs. A `last_enqueued_at` column
would fix it, at the price of the reconciler writing to the rows it is supposed to only
read, which is the property that keeps it free of races with the claim. Revisit if M10's
logs are unreadable.

**Permanent and transient failures are not distinguished.** A file that can never be parsed
is retried on the same schedule as a brief provider outage. The attempt limit caps the waste
at two extra runs, which is cheaper than a `retryable` column and the discipline of setting
it correctly at every failure site.

**A document that fails three times needs a human**, and there is no endpoint for that
person to use. It is visible through `GET /api/documents` with its `failure_reason`.

**Two coupled settings.** `MaxRunDuration` must stay below the agent worker's rebalance
timeout, and the run lease must stay above it. The worker validates both at startup rather
than trusting anyone to remember — a validation that lands with M25, since neither setting
exists before it.
