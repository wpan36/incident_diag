# internal/wire

## Purpose

The response shapes that more than one process renders, plus the one timestamp format this
project emits. Specified by `docs/plans/agent-execution-and-events.md` (S8).

## Contents

- `time.go` — `TimeLayout`, `Time`, `NewTime`, `NullTime`. Moved here from `internal/api` in
  M24.
- `wire.go` — `Run`, `Step`, `ToolCall`, `Evidence`, `RunDetail` and their converters
  (`NewRun`, `NewStep`, `NewToolCall`, `NewEvidence`, `NewRunDetail`).
- `time_test.go`, `wire_test.go` — the timestamp format and the converters, both pure.

## How it fits in

It depends on `store` and `time` and nothing else, which is the point: `internal/agentrun`
publishes these types onto a Redis stream and `internal/api` returns them over HTTP, so the
front end has one type per entity rather than two that have to agree. `internal/api` is the
top of the dependency graph, so the worker importing it would invert that direction and
link gin into a binary that serves no HTTP.

`Incident` and `Document` stay in `internal/api`: moving them would be a rename with no
consumer asking for it.

## Gotchas

- **No `omitempty` on a nullable field.** A nullable column renders as an explicit `null`,
  never an absent key, so the key set is identical in every response — which is what makes
  M28's generated TypeScript types honest.
- **`Run.FinalResult` is a `*json.RawMessage`, not a `json.RawMessage`.** An empty
  `RawMessage` marshals to invalid JSON rather than to `null`, and a `RUNNING` run's is
  always empty. `nullJSON` is what converts the one to the other.
- **Timestamps are fixed-width microseconds.** Go's default is RFC 3339 *Nano*, which trims
  trailing zeros, so the same instant would serialize with a different number of digits
  depending on its value. `Time` also implements `UnmarshalJSON` purely so the type
  round-trips for tests and generated clients.
- **Neither `ToolCall` nor `Evidence` carries a `run_id`** — it is the run being rendered.
- **`Step.Evidence` is always an array, never `null`**, so a client that iterates it does
  not have to check first. `Step.ToolCall` *is* null on a step that made none, which is
  most of them: retrieval writes no `tool_calls` row.
- **A finish step's evidence points at *earlier* steps.** `NewRunDetail` attaches each row
  to the step its `step_id` names, which is where a timeline renders a citation — so the
  finish step's own `evidence` array is usually empty even though it is what produced them.
- **`RunDetail` is the one place this project has two shapes for one entity**, against
  `internal/api`'s convention. A listing carrying every run's whole timeline is the
  alternative, and it is worse. It is safe only because `max_steps` is a single digit.
- **A `RUNNING` run's four counters read zero.** `ClaimRun` resets them and only `FinishRun`
  writes them. A client wanting a live count counts `step.completed` events.
