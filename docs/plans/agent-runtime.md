# Agent Runtime

**Tier A spec (S7).** Gates M21 (LLM client), M22 (types and `ContextBuilder`), M23 (bounded
loop). Resumption is out of scope — [ADR 0009](../adr/0009-restart-instead-of-resume.md).

## Problem

The loop is the part of this project worth understanding, so it is written by hand rather
than delegated to a framework. What it needs deciding is how the model expresses a decision,
what the loop does with the context as a run grows, and what happens at each of its bounds.

S1 already fixed the persisted shape: `action_type` is `retrieve | tool_call | finish`, so
one action per step and no parallel tool calls; `evidence` rows carry a `step_id` and a note
saying why the agent kept them.

## Technical Plan

### One tool list from three sources

The model is handed a single list: `search_knowledge`, the tools discovered from `ops-mcp`,
and `finish`. It calls exactly one per step, using native OpenAI-compatible tool calling.

`finish` is not an MCP tool — it is how the loop ends — but presenting it identically means
the model has one mechanism rather than "call a tool, or else answer in prose".

| Tool | |
| --- | --- |
| `search_knowledge(query, service?, document_type?, k?)` | composes `embed` and `search`, the same composition `GET /api/search` performs |
| `prometheus_query`, `http_probe`, `read_service_logs` | discovered from `ops-mcp`; S6 owns their schemas |
| `finish(root_cause, affected_service, next_actions, evidence)` | ends the run |

Native tool calling rather than a hand-parsed JSON envelope: the provider enforces the
schema, so there is no tolerant parser to write and no code-block-unwrapping to get wrong.
The MCP tool schemas S6 defines translate directly into tool definitions.

### `ContextBuilder` assembles; it does not manage

`MaxSteps` × S1's 8 KiB observation cap is the ceiling on context. At single-digit
`MaxSteps` that is roughly 20k tokens, comfortably inside the model's window, **so there is
nothing to prune**. The builder concatenates the incident and every step's action and
observation summary. That is the whole of it.

This holds only while `MaxSteps` stays small, so it is enforced as an invariant rather than
assumed: **configuration loading fails when `MaxSteps × 8 KiB` estimated as tokens exceeds
`MaxPromptTokens`.** Without that check, raising `MaxSteps` later would silently invalidate
the reasoning that removed the pruning strategy. The estimate reuses
`ingest.EstimateTokens`.

Token accounting uses the `usage` field the API returns, accumulated into the
`prompt_tokens` and `completion_tokens` counters S1 put on `agent_runs`.

### The loop

```
claim the run, delete any previous attempt's rows   (ADR 0009)
loop:
  bounds check: MaxSteps, MaxToolCalls, MaxRunDuration
  build context
  call the LLM with the tool list
  accumulate usage; bounds check: MaxPromptTokens
  the tool call becomes the step's action:
    search_knowledge -> action_type retrieve
    an MCP tool      -> action_type tool_call, plus a tool_calls row
    finish           -> action_type finish, then stop
  persist the step with a truncated observation summary
  publish the step's event
```

`context.Context` is threaded throughout, so cancelling a run cancels the in-flight LLM
request and tool call rather than waiting for them.

### Bounds produce a diagnosis, not an empty result

Bounds are checked before each step, and the token bound after each call. When one is hit
and the agent has not called `finish`, the loop makes **one final call offering only the
`finish` tool**, so the run ends with a real partial diagnosis rather than a mechanical
summary of its own steps.

A bound therefore means "stop starting new work", not "never exceed". A run can spend one
call past its token budget. The status is `SUCCEEDED` with the matching `stop_reason`,
because a bounded run that produced a diagnosis is not a failure — S1's separation of
lifecycle from outcome exists for this.

Cancellation is the exception: it stops immediately with no final call, because the context
that would carry it is already cancelled.

### A tool has three outcomes, and only one of them is the loop's problem

`mcpclient.Result` carries two independent signals, and conflating them is the mistake this
section exists to prevent. `Refused` means the tool ran and declined — an unknown service, a
malformed timestamp, a limit exceeded — which the model can fix by calling again with
different arguments. `Status` being `ERROR` or `TIMEOUT` means the dependency did not
answer, which it cannot.

| Result | `agent_steps.status` | `tool_calls.status` | The loop |
| --- | --- | --- | --- |
| ok | `OK` | `OK` | continue |
| refused | `OK` | `REFUSED` | continue; the observation is the refusal, which names the valid options |
| error or timeout | `OK` | `ERROR` / `TIMEOUT` | continue; the observation is the failure |

**The step is `OK` in all three.** A step's status says whether the step produced a usable
action and an observation, not whether the observation was good news. `ERROR` on a step is
reserved for the case below, where the model returned no tool call at all and there is
nothing to record.

**`tool_calls.status` gains `REFUSED`.** The column is `VARCHAR`, not an `ENUM`, precisely
so that adding a value is not a schema change — S1 says so in as many words. Without it a
wrongly-argued tool call is indistinguishable from a correct one in the audit trail, which
is exactly what the agent evaluation wants to count.

**A refusal spends one of `MaxToolCalls`.** It consumed a call, and pretending otherwise
would let an agent that keeps guessing wrong run unbounded.

**A failing dependency does not end the run.** Prometheus being down is a reason to look at
the logs, not a reason to stop investigating, so the failure text becomes the observation
and the loop carries on. There is deliberately **no second bound** on repeated failures of
the same tool: `MaxToolCalls` already bounds the waste, and a per-tool failure counter would
be a second limit for a case the first one covers.

### A model that answers in prose

Native tool calling does not guarantee a tool call. A response with no tool call is a step
with `status = ERROR`. The loop retries once with an explicit instruction to use a tool; a
second failure ends the run with `stop_reason = ERROR`.

Responses may carry `content` alongside `tool_calls`. That is the model's reasoning and
**is never persisted**. S1's schema has nowhere to put it, which is deliberate, and the
tests assert it does not appear.

### Evidence

`finish` carries `evidence: [{step, note}]`. Persisting resolves each step number to that
step's `id`, so the row still points at the step that produced the observation and S1's
table shape is unchanged. A step number the model invents is dropped with a warning rather
than failing the run — a wrong citation should not discard a correct diagnosis.

### Configuration

`AGENT_MAX_STEPS` (8), `AGENT_MAX_TOOL_CALLS` (12), `AGENT_MAX_RUN_DURATION` (5m),
`AGENT_MAX_PROMPT_TOKENS` (60000), `LLM_TIMEOUT` (60s), `LLM_MAX_RETRIES` (2). The
per-run budget is recorded on `agent_runs` when the run is created, so a run stays
interpretable after the configuration changes.

## Alternatives

**A hand-parsed JSON action envelope.** Works on any provider, including ones without tool
calling. Rejected: it means writing and testing a tolerant parser for code-block wrapping,
trailing prose and missing fields, and model adherence is worse than with native tool
calling. The cost is a provider dependency that M33's switch test must exercise.

**A sliding context window with older observations summarized.** Saves tokens on long runs.
Rejected: under the current bounds it would never trigger, so it is a strategy and a test
suite for a situation that cannot arise.

**Evidence attached to each step.** Closer to how someone investigating actually takes
notes, and it survives a truncated run. Rejected: it puts an optional field on every tool
schema and asks the model for a second decision at every step. Revisit if truncated runs
turn out to matter.

**No forced `finish` on a bound.** Simpler, and the run ends exactly at its budget.
Rejected: it makes a bounded run useless, and `docs/architecture.md` promises a partial
result.

**Parallel tool calls per step.** Faster. Rejected by S1's schema, and it would make budget
accounting and event ordering harder for a latency gain nobody is waiting on.

## Detailed Implementation

**M21 — `internal/llm`**

OpenAI-compatible chat completions with tool calling: request and response types, tool
definitions, timeout, bounded retry, `usage` extraction. A scriptable `Fake` returning a
predetermined sequence, which is what makes M23 deterministic.

**M22 — `internal/agent` types and context**

`Run`, `Step`, `Action`, `Observation`, `ToolCall`, `Evidence`, `FinalResult`; the system
prompt; `ContextBuilder`; the `Tool` interface the loop calls. The interface is small and
exists because the tests need fakes.

**M23 — the loop**

The algorithm above, the bounds, the prose retry, the forced finish, and evidence
resolution. No infrastructure in its tests.

Persisting and event publishing are M25 and S8; the loop reports steps through a callback
so that M23 can be tested without a store.

## Verification

- `make check` — the loop against a scripted fake LLM and fake tools: convergence; each of
  the four bounds, including that the forced `finish` runs and the status is `SUCCEEDED`
  with the right `stop_reason`; external cancellation stopping immediately with no final
  call; a prose response and its retry; a refused tool call recorded as `REFUSED` and a
  failing one as `ERROR`, both leaving the step `OK` and the run going; an invented evidence
  step number; and that no `content` reaches the recorded steps.
- Configuration loading fails when `MaxSteps` violates the context invariant.
- `make test-integration` — one real run against DeepSeek with a live `ops-mcp`.

## Known limitations, accepted

**A run can exceed its token budget by one call**, the forced `finish`. Bounded and
deliberate.

**Evidence exists only if `finish` runs.** A cancelled run has steps and tool calls but no
evidence rows. The timeline is still auditable.

**Native tool calling is a provider dependency.** A provider without it cannot run this
agent, so M33's switch test must exercise a tool call rather than only checking that the
endpoint answers.
