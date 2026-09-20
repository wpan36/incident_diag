# internal/store

## Purpose

The persistence layer over MySQL, written as hand-rolled `database/sql` rather than an ORM.
It owns the state machines: every transition is a conditional UPDATE, which is what makes
the Kafka consumers idempotent (ADR 0003).

## Contents

- `store.go` — `Store` with `Open`, `New`, `DB`, `Ping` and `Close`; DSN verification and
  redaction; error classification (`dbError`, `isUnavailable`, `isDuplicateKey`); the
  cursor pagination machinery (`PageParams`, `Page[T]`, `paginate`, `conditions`); and the
  shared helpers `now`, `truncateSummary`, `changed`, `nullString` and `nullJSON`.
- `documents.go` — `Document`, `NewDocument`, `DocumentFilter`, the `PENDING` →
  `PROCESSING` → `READY`/`FAILED` transitions (`ClaimDocument`, `MarkDocumentReady`,
  `MarkDocumentFailed`), plus create, get and list.
- `incidents.go` — `Incident`, `NewIncident`, create, get and list. No state machine.
- `runs.go` — `Run`, `NewRun`, `RunOutcome`, the run statuses and the separate stop
  reasons, `ClaimRun`, `FinishRun`, plus create, get and list-by-incident.
- `steps.go` — `Step`, `NewStep`, the action types and step outcomes, create and
  list-by-run.
- `tool_calls.go` — `ToolCall`, `NewToolCall`, the tool-call statuses, create and
  list-by-run.
- `evidence.go` — `Evidence`, `NewEvidence`, the evidence sources, create and list-by-run.
- `store_test.go` — unit tests for the pure parts: DSN verification, truncation,
  pagination arithmetic, error classification.
- `integration_test.go` — `//go:build integration`. Real MySQL, including the migration
  reverse-and-reapply test.

## How it fits in

Sits between the schema in `migrations/` and everything that reads or writes it: `api`
today, the ingestion and agent workers later. It imports `config` for the DSN and pool
settings, and `httpx` to classify what it returns.

## Gotchas

- **A transition method returns `(bool, error)`, and `false` is not a failure.** Zero rows
  affected means the transition was not legal from the current state, which under
  at-least-once delivery normally means a redelivery of work already done. A caller that
  treats `false` as an error breaks idempotency; a caller that ignores it entirely loses
  the only signal that the work was already performed.
- **Errors leave here already classified by `httpx`.** Add a new query and you must route
  its error through `dbError`, or a handler will render a raw SQL failure.
- **Duplicate keys are not handled centrally.** A 1062 is not conflict-shaped in general —
  violating `(run_id, step_number)` is a bug in the agent loop, not a client's 409 — so only
  callers that know which constraint is meaningful call `isDuplicateKey`, currently for
  `uniq_active_run`. Everything else lands on the internal branch and reaches the logs as a
  500.
- **`isDuplicateKey` matches on the constraint name inside the driver's error text**, which
  is the only identifier MySQL provides. An integration test asserts both branches so a
  driver upgrade that changes the wording fails loudly instead of turning conflicts into
  500s.
- **The DSN must carry `parseTime=true&loc=UTC`, and `Open` checks the text as well as the
  parsed value.** Without `parseTime`, every scan into a `time.Time` fails; without
  `loc=UTC` the driver reads `DATETIME(6)` in the local zone, reintroducing exactly the
  machine-dependence that choosing `DATETIME` over `TIMESTAMP` removed. The text check is
  there because the driver happens to default `Loc` to UTC today, and a DSN that is correct
  by accident is not something to rely on.
- **A DSN carries the database password.** Driver errors quote it, so anything derived from
  one goes through `redactDSN` before it can reach a log.
- **`now()` truncates to microseconds** because `DATETIME(6)` holds six fractional digits
  and MySQL rounds anything longer — a nanosecond-precision `time.Time` would not compare
  equal to the value read back.
- **Pagination fetches `limit + 1` rows.** Inferring the last page from "fewer rows than
  limit" is wrong whenever the final page is exactly full, which is precisely the case
  nobody tests. Cursors are raw ULIDs because ids sort by creation time; the comparison is
  `id < ?` since listings are newest first.
- **`normalize` is for callers that do not come through a handler.** The API rejects an
  out-of-range limit rather than correcting it; the worker or a test passing the zero value
  would otherwise ask for a page of no rows.
- **`truncateSummary` returns the summary, the original length and the truncated flag
  together** so no caller can record one without the others, and it cuts on a rune boundary
  — slicing at a fixed byte offset produces invalid UTF-8 that a `utf8mb4` column will
  reject or mangle. The 8 KiB cap is a guess and should be revisited once there is real
  tool output to look at.
- **Integration tests need `make up` and run with `-p 1`.** Every integration package shares
  one database and the migration test drops every table, so they must not run in parallel.
  Use the make target, not a bare `go test -tags=integration ./...`. They skip cleanly when
  `TEST_MYSQL_DSN` is unset.
- **No chain-of-thought is persisted anywhere in this schema.** `Step.Action` holds the
  structured decision, never the reasoning behind it.
