# ADR 0003: At-least-once delivery with database-level idempotency

**Status:** accepted
**Date:** 2026-09-20

## Context

Both Kafka consumers — the ingestion worker and the agent worker — will occasionally see
the same message twice: a rebalance, a crash between doing the work and committing the
offset, or a manual replay. Processing a document twice must not double its chunks, and
consuming an agent run message twice must not start a second investigation.

Kafka offers transactional exactly-once semantics for Kafka-to-Kafka pipelines, but the
side effects here are writes to MySQL and Elasticsearch, which those transactions do not
cover.

## Decision

Accept at-least-once delivery and make every consumer idempotent through MySQL, which is
already the system of record.

Concretely:

- Work is claimed with a **conditional update** — `UPDATE ... SET status = 'RUNNING'
  WHERE id = ? AND status = 'PENDING'`. A redelivered message finds zero rows affected and
  stops, rather than starting a duplicate.
- Terminal writes are conditional on the expected prior status, so a late duplicate cannot
  overwrite a finished record.
- Ingestion deletes existing chunks for a `document_id` before indexing, making the
  Elasticsearch write converge rather than accumulate.
- State transitions are centralized in the store layer, so the legal transitions are
  defined once rather than re-implemented per caller.

## Reasoning

Exactly-once would be a lie here: the atomic unit that matters spans Kafka, MySQL and
Elasticsearch, and no amount of Kafka transaction configuration makes those one
transaction. Two-phase commit or an outbox with deduplication would buy correctness that
the conditional-update approach already provides, at far higher cost.

Idempotency also happens to be the property worth learning and worth being able to
explain, which matters for a project with a teaching purpose.

## Consequences

- Duplicate delivery must be an explicitly tested case, not an assumption. Integration
  tests deliver the same message twice and assert the end state is unchanged.
- Status columns carry real semantics; the state machine has to be written down in the
  data model spec, not left implicit.
- A worker that dies mid-run leaves a row in `RUNNING`. A stale-run reaper is not in scope
  for the first version; such runs are visible and can be ended manually. This is a known
  gap, recorded here rather than quietly ignored.

## Alternatives considered

**Kafka exactly-once semantics.** Rejected: does not cover the external side effects that
actually need protection.

**A dedicated processed-message table keyed by message ID.** Workable, but a second source
of truth about progress alongside the status column. The status column alone is simpler
and sufficient.
