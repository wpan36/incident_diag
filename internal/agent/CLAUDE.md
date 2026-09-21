# internal/agent

## Purpose

The bounded investigation loop. One model call per step, one tool call per step, four
bounds that stop it starting new work, and a forced `finish` so a truncated run still ends
with a diagnosis. Specified by `docs/plans/agent-runtime.md` (S7).

It performs no I/O beyond the model and the tools: steps go to a callback that persists
them and publishes their events, which is what makes the whole package testable with no
database, no broker and no provider.

## Contents

- `agent.go` — the vocabulary: `Run`, `Incident`, `Budget`, `Action`, `Observation`,
  `Step`, `StepRef`, `ToolCall`, `Evidence`, `Citation`, `FinalResult`, the `ReportStep`
  callback, and the `ToolServer` and `ChunkSearcher` interfaces.
- `prompt.go` — `SystemPrompt`, the incident, observation, no-tool and forced-finish
  messages, and the JSON Schemas for `search_knowledge` and `finish`.
- `knowledge.go` — `Knowledge`, the in-process `search_knowledge`, and the hit rendering.
- `context.go` — `Turn`, `ContextBuilder` and `EstimateTokens`.
- `tools.go` — `Deps`, `Agent`, `New`, the one tool list from three sources, and the MCP
  call to `tool_calls.status` mapping.
- `loop.go` — `Run`, the bounds, the prose retry, the forced finish and the step reporting.
- `evidence.go` — decoding `finish`'s arguments and resolving its citations against the
  steps that happened.
- `integration_test.go` — `//go:build integration`, two tests over a real Elasticsearch
  index and a live in-process `ops-mcp`. The scripted one runs whenever the infrastructure
  is there; the one that adds the real provider skips unless `TEST_LLM_API_KEY` is set,
  because it costs a billed completion.

## How it fits in

`llm.Chatter` for the model, `mcpclient.Client` for the operational tools, `embed.Embedder`
plus `search.Client` for retrieval, `summary.Cap` for the observation cap, and `store`'s
constants for the persisted vocabulary — but never a `*store.Store`. `internal/agentrun`
supplies the callback that writes rows and publishes events, and calls `FinishRun` with the
outcome.

## Gotchas

- **`Run` returns an error only for cancellation, and then nothing terminal must be
  written.** The run stays `RUNNING`, its Kafka offset uncommitted, and the lease restarts
  it (ADR 0009). Every other ending, failures included, comes back as a `store.RunOutcome`
  to write. `stop_reason = CANCELLED` therefore has no writer.
- **A bound produces a diagnosis, not an empty result.** The run ends `SUCCEEDED` with the
  bound's stop reason, and the forced `finish` occupies a step number — so `step_count` can
  exceed `max_steps` by one. If that call also produces nothing usable it is *not* retried:
  the run is `FAILED` with `ERROR`.
- **`MaxRunDuration` is wall-clock, checked between steps, and is not a deadline on the
  context.** If it were, the forced `finish` would run on an expired context and could never
  succeed. Only per-call deadlines live on the context, the way `internal/ingest` detaches
  its terminal writes.
- **A step is `OK` even when its tool refused, failed or timed out.** A step's status says
  whether it produced a usable action and an observation, not whether the observation was
  good news. `ERROR` on a step means one thing only: the model called no usable tool.
- **Retrieval costs a step, not a tool call.** `search_knowledge` writes no `tool_calls`
  row, because `tool_call_count` counts those rows. An invented tool name, by contrast, is
  a `REFUSED` row and does spend one.
- **Only the first tool call in a response is executed, and only it is replayed.** An
  assistant message whose other calls have no paired `tool` reply is exactly the history
  some providers reject, and the schema can record only one action per step.
- **Arguments that are not valid JSON become a `none` step** rather than reaching a tool.
  They would otherwise go into two JSON columns that MySQL refuses.
- **The model's prose is never persisted or replayed.** The schema has nowhere for
  chain-of-thought, which is deliberate, and a test asserts it does not leak into either.
- **Every observation in the context is labelled `Step <n> observation:`, and `Turn.Number`
  is where that number comes from.** It is the only way the model learns the numbers
  `finish` cites: a `none` step spends a number without leaving an assistant turn to count,
  so a model counting its own turns would cite the step before the one it meant — and
  `resolveEvidence` would write that to `evidence` without a warning. The label is added
  after `summary.Cap`, so the audit row is unchanged.
- **A `none` step's instruction carries `Turn.Reason`.** Two of the three responses that get
  there did call a tool — `finish` with arguments that would not decode, and `finish` with a
  blank `root_cause` — so "call a tool" would be the wrong correction and the retry would
  repeat the mistake.
- **An observation is never sent to the provider empty.** A `tool` message with no `content`
  is rejected, and `internal/llm` does not retry a 400, so one tool returning nothing would
  end the run and end it again after the restart. `observationMessage` substitutes a
  placeholder.
- **`finish`'s arguments are decoded in two stages** (`decodeFinal`): the two strings
  strictly, then `next_actions` and each citation on its own. `json.Unmarshal` is all or
  nothing, so reading the evidence array as a whole would let one citation whose `step` is a
  string discard the diagnosis around it.
- **`MaxToolCalls` must stay below `MaxSteps` to mean anything.** A step makes at most one
  tool call, so a tool-call bound at or above the step count can never fire; the defaults are
  8 and 6.
- **The context is never pruned, and what makes that safe is `config.LoadAgent`'s
  invariant.** `EstimateTokens` must keep using `config.BytesPerToken`, or the run-time
  bound could fire on a configuration the loader had just accepted.
- **The loop keeps every retrieval's hits until the run ends.** That is what validates a
  cited `document_id` and what fills an evidence row with the one hit the model cited
  rather than with all five. An invented step or document is dropped with a warning: a
  wrong citation should not discard a correct diagnosis. A cited document that was real at
  index time but whose row has since gone is `internal/agentrun`'s to drop, on the same
  principle — this package cannot see MySQL.
