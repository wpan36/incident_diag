# internal/mq

## Purpose

A thin wrapper over franz-go: the message envelope, a producer, a consumer group, and
idempotent topic creation.

## Contents

- `mq.go` — topic and group names, topic geometry, header names, `TopicSpec`,
  `DefaultTopics`.
- `message.go` — `Envelope`, the `Message` interface, `DocumentMessage`, `RunMessage`,
  `Encode`, the generic `Decode`, and the two decoding errors.
- `producer.go` — the `Producer` interface and its franz-go `Client`. `kotel` hooks write
  the trace context into the record headers.
- `consumer.go` — `Record`, `Handler`, `Consumer` with `Run`, and `WithRebalanceTimeout`.
  `handle` opens the process span, then hands the handler the *poll* context with that span
  attached, because the record's own context carries no cancellation.
- `topics.go` — `EnsureTopics` and the metadata wait.
- `fake.go` — `FakeProducer`, which records what would have been produced.
- `message_test.go` — unit tests. `integration_test.go` — real broker.

## How it fits in

`internal/api` produces after an upload and after a run is created; `internal/reconcile`
produces for rows nothing will send a message for again; `internal/ingest` and
`internal/agentrun` are the handlers on the other side. Both workers call `EnsureTopics` at
startup, so either can be the first one up. The specs are
`docs/plans/async-messaging-and-idempotency.md` and
`docs/plans/agent-execution-and-events.md`.

## Gotchas

- **Kafka offsets are not the record of progress; MySQL is.** Offsets exist only to avoid
  redundant work, so every record is committed once its handler returns, whatever the handler
  concluded. Withholding a commit cannot mean anything anyway — an offset is a position, so
  committing the next record commits this one too — and the only way to make it mean something
  is to stop consuming, which turns one bad message into a stalled partition.
- **It is deliberately not an abstraction over messaging.** There is one broker technology in
  this project and there will not be a second.
- **Topic and group names are constants, not configuration.** They are the contract between a
  producer and a consumer in a different process, and making them settable is how two services
  end up disagreeing about where work goes.
- **The idempotent producer is turned off, which is the opposite of the usual advice.**
  franz-go will not fail a record already sent while producing idempotently, so neither the
  context deadline nor `RecordDeliveryTimeout` applies: with the broker stopped mid-request an
  upload handler blocked for 223 seconds in the smoke test. Delivery is at-least-once by
  design, and a duplicate message costs exactly one refused claim, so exactly-once delivery
  buys nothing here and costs an unbounded handler.
- **Automatic topic creation is off and stays off.** A broker that invents a topic on first
  produce turns a misspelled name into a silent second queue nothing consumes — with one
  partition rather than three.
- **The produce key is the entity ID.** That is what puts every message about one document on
  one partition, so a redelivery and a reconciler's re-enqueue cannot be processed by two
  consumers at once.
- **`Envelope` is embedded, not nested**, so the JSON on the wire is flat. A nested payload
  would buy one `Handler` signature per topic — uniformity nothing here needs — and cost a
  second decode inside every handler.
- **`Decode` returns the value anyway on `ErrUnknownSchemaVersion`.** The JSON parsed, so the
  entity ID is trustworthy and the caller needs it to record the permanent failure against the
  right row. Every other error returns the zero value.
- **Exactly one `AllowRebalance` per poll, on every path out of `Run`.**
  `BlockRebalanceOnPoll` makes a pending rebalance wait for that call, so returning without it
  deadlocks the group manager — and therefore `Close`, which is trying to leave the group.
- **`WithRebalanceTimeout` belongs to whatever bounds the handler**, plus a margin, and is
  derived from it rather than configured separately. A member whose handler outlasts it is
  evicted, its message redelivered, and the claim then refuses it because the row is already
  `PROCESSING`.
- **`ConsumeResetOffset(AtStart)` is set explicitly although it is franz-go's default**,
  because it is load-bearing and is the opposite of the Java client's: starting at the end
  would silently drop everything produced before the worker first came up.
- **`EnsureTopics` waits for metadata rather than trusting it.** A topic is accepted by the
  controller before it is servable, so describing it immediately can report
  `UNKNOWN_TOPIC_OR_PARTITION` for a topic that definitely exists — which would make two
  workers starting together a coin flip. A pre-existing topic with too few partitions is an
  error, because that is what a broker-auto-created topic looks like.
- **`traceHeaders` is deliberately empty until M29.** Writing a `traceparent` today would mean
  inventing a trace ID that no span ever belonged to.
