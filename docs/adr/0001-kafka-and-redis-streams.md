# ADR 0001: Use both Kafka and Redis Streams

**Status:** accepted
**Date:** 2026-09-20

## Context

The system has two asynchronous needs that look similar on the surface:

1. Hand long-running work (document ingestion, agent runs) to background workers.
2. Push agent execution events from a worker to however many browsers are watching a run.

Using one piece of infrastructure for both would be simpler to operate, and running both
invites the accusation that the stack is padded for its own sake.

## Decision

Use Kafka for work distribution and Redis Streams for event fan-out.

## Reasoning

The two needs have opposite requirements.

Work distribution needs **durability and replay**. If the agent worker crashes mid-run,
the message must still be there. Consumer groups, offsets and retention are exactly what
Kafka provides, and the at-least-once semantics force the workers to be idempotent, which
is a property worth having anyway.

Event fan-out needs **low latency and disposability**. Timeline events are only
interesting while someone is watching; the authoritative record of what the agent did is
in MySQL. Redis Streams gives capped, expiring streams, a blocking read for the SSE
handler, and `Last-Event-ID` style replay for a reconnecting browser, at a fraction of the
operational weight.

Forcing either onto the other would be worse. Kafka per-run topics for SSE would be
absurd — topic churn, heavyweight consumer group coordination for an ephemeral browser
connection. Redis Streams as the work queue would mean building consumer group
rebalancing and retention guarantees that Kafka already has, for jobs where losing a
message means losing a user's uploaded document.

## Consequences

- Two systems to run in Docker Compose, and two client libraries to learn.
- The boundary must stay clean: **nothing authoritative is ever only in Redis.** Any event
  published to Redis must correspond to a row already written to MySQL. Losing the Redis
  data may cost a live timeline; it must never cost a fact.
- Workers must be idempotent under redelivery — see [ADR 0003](0003-at-least-once-delivery.md).

## Alternatives considered

**Kafka only.** Rejected: per-run topics or a shared topic filtered per client both turn a
throwaway browser connection into a Kafka consumer group problem.

**Redis Streams only.** Rejected: acceptable for the event path, not for work that must
survive a worker crash.

**Neither — direct goroutines.** Rejected: the API process would own agent execution, so
restarting the API would kill running investigations, and the system would lose the
worker boundary that most of the interesting design (idempotency, backpressure, tracing
across a process boundary) depends on.
