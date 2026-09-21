# internal/agentrun

## Purpose

Consumes `agent.runs.v1` and executes one investigation per message: claim the run, run the
bounded loop, write its outcome. It is `internal/agent`'s one seam onto infrastructure.
Specified by `docs/plans/agent-execution-and-events.md` (S8).

## Contents

- `agentrun.go` — `Deps`, `Handler`, `NewHandler`, the `mq.Handler` (`Handle`),
  `investigate`, `finish` and `explainRefusedClaim`.
- `report.go` — `ReportStep`, the `agent.ReportStep` the loop is built with, and
  `liveDocuments`.
- `integration_test.go` — `//go:build integration`. Real MySQL and real Redis, with an
  `llm.Fake` and no tool server.

## How it fits in

It mirrors `internal/ingest`: the same decode/claim/terminal-write frame, and for the same
reason — the claim is a conditional UPDATE and `FinishRun` is conditional on the run still
being `RUNNING` under this attempt. `cmd/agent-worker` builds a `Handler` and hands
`Handler.Handle` to `mq.Consumer.Run`, alongside `reconcile.NewRunRunner`.

## Gotchas

- **The row is written before its event is published, and a failed publish is logged and
  ignored.** ADR 0001. An error from `ReportStep` ends the run `FAILED`, because that means
  the *row* could not be written; a publish failure is not such an error.
- **`NewHandler` builds the agent itself.** `agent.New` needs this package's `ReportStep`
  and this package needs the agent; constructing it inside `NewHandler` is what breaks that
  circle without a setter, which is why `Deps` carries the agent's dependencies rather than
  a built `*agent.Agent`.
- **The worker re-reads the run to build `run.started` and `run.finished`.** `ClaimRun` and
  `FinishRun` return `(bool, error)` and not the row, so each payload comes from a `GetRun`
  after the transition. Publishing the row as it was read *before* the claim would put
  `PENDING`, a stale `attempts` and a null `started_at` into `run.started`.
- **A claim that returns `false` publishes nothing and returns nil.** The run is already
  `RUNNING` under another attempt or already terminal, so the message is a redelivery of
  work that is done or in hand — and a `run.started` for it would tell a browser to clear a
  timeline that is correct.
- **`run.finished` is published only when `FinishRun` reports that it wrote the row.**
  `false` means the run is no longer this attempt's — another attempt finished it, or the
  lease expired and a later claim took it over — so the outcome computed here describes
  superseded work and is discarded with a warning. The attempt number comes from the
  `GetRun` after the claim and is part of `FinishRun`'s condition.
- **The run's context gets no deadline.** `MaxRunDuration` is wall-clock and checked between
  steps (S7); as a deadline it would expire before the forced `finish`, which is the call
  that turns a bound into a diagnosis. What bounds the damage instead is `RUN_LEASE`.
- **Shutdown cancels the run and writes nothing terminal.** The run stays `RUNNING`, the
  offset is uncommitted, and the lease reclaims it (ADR 0009) — so restarting the worker
  leaves that incident answering 409 until the lease expires. That is accepted, not a bug.
- **A message that cannot be decoded is logged and skipped, with nothing recorded against
  the row.** Unlike a document, the run stays `PENDING`, which is the reconciler's
  never-enqueued category, and the sweep produces a *fresh* well-formed message — so even
  an unknown schema version recovers.
- **`ReportStep` writes the step, then its `tool_calls` row, then its `evidence` rows**, in
  that order because the foreign keys require it, and not in one transaction. A crash
  between two of them leaves rows the restart deletes anyway.
- **A citation to a document that is no longer in MySQL is dropped, not fatal.** Retrieval's
  document ids come from Elasticsearch and `evidence.document_id`'s foreign key from MySQL,
  and the two diverge whenever a document row goes while its chunks are still indexed —
  which is exactly what `fk_evidence_document`'s `ON DELETE SET NULL` anticipates. Without
  `liveDocuments` the insert fails, `ReportStep` fails, and the run ends `FAILED` having
  thrown away a complete diagnosis. The row is still written: `source_ref` is what keeps a
  citation readable once its document has gone. A real smoke test found this; the loop
  already takes the same position on an invented document.
- **The browser sees nothing while a step runs**, around four seconds, because there is no
  `step.started`. Accepted for now; adding one in M28 is additive.
