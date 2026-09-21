# internal/reconcile

## Purpose

Re-enqueues rows that nothing will ever send a message for again — documents and agent runs
alike: the sweep, and the policy that decides what counts as stuck.

## Contents

- `policy.go` — `Policy`, `PolicyFrom`, `RunPolicy`, `DueDocument`, `DueRun`, and the
  `backoffSchedule` with `Backoff` and `MinBackoff`.
- `runner.go` — the unexported `target`, `Runner`, `NewDocumentRunner`, `NewRunRunner`,
  `Run`, `Sweep` and `Stats`.
- `policy_test.go` — the policy as a table, with no database.
- `integration_test.go` — the sweep against real MySQL with a `mq.FakeProducer`.

## How it fits in

`cmd/ingestion-worker` and `cmd/agent-worker` each own a sweep alongside their consumer: it
has to run somewhere, and the process that consumes the work is the one that should look
for work that never arrived. The specs are
`docs/plans/async-messaging-and-idempotency.md` (documents) and
`docs/plans/agent-execution-and-events.md` (runs).

## Gotchas

- **This is why the project needs no transactional outbox.** Two failures put a row in a
  stuck state and they are really one failure: a row created but never enqueued, and a row
  claimed but never finished. An outbox fixes the first and leaves the second, so the system
  would carry an outbox table and a relay process and *still* need a lease and a sweep. The
  sweep alone covers both, reading a status column that already exists.
- **The runner produces messages and changes nothing.** That is the property the whole design
  rests on: there is exactly one place where a document's state moves — the claim — so the
  sweep cannot race it, and the worst a redundant message can do is be refused.
- **One `Runner`, two targets.** The ticker, the per-sweep deadline, `Stats` and the logging
  are the same job whichever table is swept; the unexported `target` supplies the listing
  function, the `Due` function, the topic, the message constructor and the noun. `Due` is
  per target because runs use `RUNNING` where documents use `PROCESSING`, and one function
  switching on both status vocabularies is how the two quietly drift.
- **`store.ReconcileCandidate.ProcessingStartedAt` carries `agent_runs.started_at` for a
  run.** The field keeps its name rather than being renamed across the document path: both
  columns mean when the claim was taken.
- **A run's log line names only the two categories runs can have.** A zero
  `retryable_failure` on every run sweep would be noise about a category that cannot happen.
- **`NewRunRunner` takes `config.Reconcile` plus `RUN_LEASE` and `RUN_MAX_ATTEMPTS`**, not a
  `Policy`. The interval, the pending-after and the batch are shared with the document
  sweep; `PolicyFrom` builds a `Policy` out of `config.Reconcile` alone and cannot express
  the other two, which is what `RunPolicy` is for.
- **The query narrows, the policy decides.** `ListDocumentsToReconcile` is deliberately looser
  than `Due`, so the SQL and the intent cannot drift apart and the policy stays testable as a
  table without a database.
- **`backoffSchedule` has two entries because the attempt limit is three.** The claim counts
  an attempt, so a document that failed once reads `attempts=1`, one that failed twice reads
  2, and one that failed three times is out of attempts and never consults the table. A third
  entry would be unreachable. Attempts past the end get the last entry rather than an
  undefined value, so raising `INGEST_MAX_ATTEMPTS` adds five-minute retries instead of a hot
  loop.
- **A `PROCESSING` row with a NULL `processing_started_at` is not reclaimable.** Only the
  claim writes that status and it always sets the timestamp, so a NULL means something else
  wrote the row and the sweep should not guess.
- **`INGEST_DOCUMENT_TIMEOUT` must stay below `INGEST_LEASE`**, or a document still being
  worked on can be claimed a second time. `config.LoadReconcile` validates the pair at
  startup; the handler is where the deadline is applied.
- **The first sweep is immediate**, because the most likely reason this process is starting is
  that the last one died, possibly holding claims.
- **A sweep gets one interval and no more.** Overrunning would stack sweeps on top of each
  other, and a sweep that slow is a symptom worth seeing rather than something to wait out. A
  failed sweep is not retried early: the next tick is the retry.
- **A produce failure is counted and skipped, not fatal.** Nothing was written, so the row
  still matches next time, and one unreachable partition should not stop the rest.
- **An empty sweep logs at debug.** It is the common case and must not fill the log.
