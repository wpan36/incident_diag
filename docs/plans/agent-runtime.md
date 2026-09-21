# Agent Runtime

**Tier A spec (S7).** Gates M21 (LLM client), M22 (types and `ContextBuilder`), M23 (bounded
loop). Resumption is out of scope — [ADR 0009](../adr/0009-restart-instead-of-resume.md).

**What this amends elsewhere.** `agent_steps.action_type` gains `none` and
`tool_calls.status` gains `REFUSED`. Both columns are `VARCHAR` rather than `ENUM`, which S1
chose precisely so that adding a value is not a migration. It also supersedes the M21 row in
`docs/roadmap.md`: there is no tolerant structured-output parser (native tool calling) and no
streaming in M21 — see *Streaming is M27's problem* below.

## Problem

The loop is the part of this project worth understanding, so it is written by hand rather
than delegated to a framework. What it needs deciding is how the model expresses a decision,
what the loop does with the context as a run grows, and what happens at each of its bounds.

S1 already fixed the persisted shape: `action_type` is one of a small set, so one action per
step and no parallel tool calls; `evidence` rows carry a `step_id` and a note saying why the
agent kept them.

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

`finish`'s four parameters are all `required`: `root_cause` and `affected_service` are
strings, `next_actions` is an array of strings, and `evidence` is an array of
`{step, note, document_id?}` which **may be empty** — a run cut short by a bound may honestly
have nothing worth citing. A `finish` whose `root_cause` is blank is treated as no tool call
at all (below); `required` is the only mechanism behind the system prompt's demand that every
claim rest on an observation.

Native tool calling rather than a hand-parsed JSON envelope: the provider enforces the
schema, so there is no tolerant parser to write and no code-block-unwrapping to get wrong.
The MCP tool schemas S6 defines translate directly into tool definitions.

The request sets `parallel_tool_calls: false` where the provider accepts it. If a response
carries several tool calls anyway, **the first is executed and the rest are dropped with a
warning**: S1's schema can express one action per step, and silently executing the others
would leave a tool call in the audit trail with no step to hang it on. **Only the executed
call is replayed into the context**: an assistant message whose other tool calls have no
paired `tool` reply is exactly the history some providers reject.

A call naming a tool that is not in the list is recorded like a refusal — a `tool_calls` row
with `status = REFUSED` whose observation lists the valid names — and spends one of
`MaxToolCalls`. That is how `ops-mcp` already answers an unknown service name (S6), and the
model recovers the same way.

### `search_knowledge` lives in the agent, not in `ops-mcp`

It needs `embed.Embedder` and `search.Client` in process; putting it behind MCP would send
the agent's own retrieval over HTTP for nothing, and `ops-mcp` is the boundary for the
*operational* world, not for this system's own storage.

It is `action_type = retrieve`, writes **no** `tool_calls` row, and **does not count toward
`MaxToolCalls`** — `agent_runs.tool_call_count` counts `tool_calls` rows, and S1 separated
the two action types for exactly this reason. `MaxSteps` is what bounds retrieval.

`k` defaults to **5**, not `search.DefaultK`. The observation cap is 8 KiB ≈ 2000 tokens and
a chunk runs to roughly 400, so ten hits do not fit and five do. A `k` that is missing, zero
or negative is 5 — `search.clampK` would otherwise read it as unset and return `DefaultK`.
Values above `search.MaxK` are clamped by `internal/search`, which already does it.

The observation is one block per hit, in rank order:

```
[01JBQ...] payment-service-runbook.md · Symptoms > Rising latency (0.83)
<chunk content>
```

The bracketed value is the `document_id`, which is what `finish` cites. Hits are dropped
**whole** from the end until the rendered text fits the cap, and the block ends with
`(3 of 5 hits shown; the rest did not fit)` when any were dropped. Cutting mid-hit would
hand the model half a chunk and a document id it cannot use.

**A failed retrieval is not a failed run.** When `Embed` or `Search` returns an error,
`ctx.Err()` is checked first — a cancelled run stops — and otherwise the step is `OK` with the
failure text as its observation and the reason in `agent_steps.error`, exactly as a failing
MCP tool is handled. The loop continues, and `ERROR` on a step stays reserved for
`action_type = none`.

### The context is a message sequence, and it is only assembled

`MaxSteps` × S1's 8 KiB observation cap is the ceiling on the context. At single-digit
`MaxSteps` that is roughly 16k tokens, comfortably inside the model's window, **so there is
nothing to prune**. The builder is the whole of it:

| | |
| --- | --- |
| system | the prompt (below) |
| user | the incident: title, description, service, `created_at` |
| per completed step | the `assistant` message the model returned — **the executed tool call only, never its `content`** — followed by the matching `role: "tool"` message carrying the provider's `tool_call_id` and the truncated observation |
| per `none` step | one `user` message saying the previous response called no tool and that one of the listed tools must be called. A step with no tool call has no `tool` reply to pair with, and dropping it silently would leave the model no sign that it misbehaved — which is what the retry exists to correct |

A rendered text transcript would be simpler, but a tool call sent in history without its
paired `tool` reply is rejected outright by some OpenAI-compatible providers, and M33 exists
to switch providers.

The system prompt states: the agent is a read-only SRE investigating this incident; it must
call exactly one tool per step; every claim in the final diagnosis must rest on an
observation it actually made; and it must call `finish` before its budget runs out. The
wording is M22's to write.

Observations are capped with `summary.Cap` — the same helper and the same 8 KiB that
`internal/store` applies when persisting, so the context and the audit row cannot disagree
about what was said. The loop keeps the **full** text for the step callback, because
`store.CreateStep` truncates and records the original length itself.

**The invariant.** This reasoning holds only while `MaxSteps` stays small, so it is enforced
rather than assumed: **`LoadAgent` fails when `MaxSteps × 8 KiB + 40 KiB` exceeds
`MaxPromptTokens`,** converted at a worst case of **4 bytes per token**. The 40 KiB is
everything the observations do not cover: the API caps an incident description at 8192
characters, which is 33 KiB of UTF-8 at worst, plus the system prompt and the tool-call
arguments. Without that margin the bound below can fire on a configuration this check just
accepted, and the forced `finish` would then be built from the same over-budget prompt.

The 4-bytes-per-token ratio is a local constant in `internal/config` with a comment pointing
at `internal/ingest/tokens.go`, rather than a call into that package: `config` is the bottom
of the dependency graph and `ingest → store → config`, so importing it would be an import
cycle. `LoadReconcile` already validates a relationship
between two values in the same struct, so this is the existing pattern.

### The token bound is a ceiling on one prompt

Before each call the assembled prompt is estimated with the same 4-bytes-per-token ratio; if
it exceeds `MaxPromptTokens` the run stops with `TOKEN_BUDGET`. It is a safety net rather
than an operating bound — the invariant above guarantees it cannot fire under a valid
configuration — and it is what makes the "no pruning" decision safe rather than lucky.

`agent_runs.prompt_tokens` and `completion_tokens` accumulate the `usage` the API returns.
They are **counters, not budgets**: nothing bounds a run's cumulative token spend, because
`MaxSteps`, `MaxToolCalls` and `MaxRunDuration` already bound its cost.

### The loop

```
claim the run          (the claim deletes the previous attempt's rows — ADR 0009)
loop:
  bounds check: MaxSteps, MaxToolCalls, MaxRunDuration
  build the context; estimate it; bounds check: MaxPromptTokens
  call the LLM with the tool list
  accumulate usage
  the tool call becomes the step's action:
    search_knowledge -> action_type retrieve
    an MCP tool      -> action_type tool_call, plus a tool_calls row
    finish           -> action_type finish, then stop
    no tool call     -> action_type none, status ERROR, retry once
  report the step through the callback, which persists it and publishes its event
```

`agent_steps.action` holds `{"tool": "<name>", "arguments": {…}}` for all three action types
and `{}` for `none`; `tool_calls.arguments` holds the arguments object alone. `duration_ms`
is the step's wall time, LLM call included. **`step_number` starts at 1**, and every
persisted step occupies one — including a `none` step, so a response that called no tool
spends a `MaxSteps` slot whether or not its retry succeeds.

**The callback returns the ids it wrote.** Its signature is
`func(context.Context, Step) (StepRef, error)`, where `StepRef` carries the `agent_steps.id`
and, when the step made one, the `tool_calls.id`. The loop keeps a step number → `StepRef`
map and resolves `finish`'s citations itself, so what it hands the callback is already
resolved and M23 owns the whole of evidence validation — its fake callback only has to return
counterfeit ids. A callback error ends the run `FAILED` with `stop_reason = ERROR`: a step
that could not be persisted means the audit trail is already wrong.

`context.Context` is threaded throughout, so cancelling a run cancels the in-flight LLM
request and tool call rather than waiting for them.

### Bounds produce a diagnosis, not an empty result

Bounds are checked before each step. When one is hit and the agent has not called `finish`,
the loop makes **one final call offering only the `finish` tool**, so the run ends with a
real partial diagnosis rather than a mechanical summary of its own steps.

A bound therefore means "stop starting new work", not "never exceed". The status is
`SUCCEEDED` with the matching `stop_reason`, because a bounded run that produced a diagnosis
is not a failure — S1's separation of lifecycle from outcome exists for this.

**`MaxToolCalls` is checked before the model chooses**, so a run that has exhausted it is
sent to `finish` even when its next action would have been a retrieval, which costs no tool
call. A bound says "stop starting new work"; making it depend on what the model picks next
would turn it into a gate and make it much harder to test.

| Bound | `stop_reason` |
| --- | --- |
| `MaxSteps` | `MAX_STEPS` |
| `MaxToolCalls` | `MAX_TOOL_CALLS` |
| `MaxRunDuration` | `TIMEOUT` |
| `MaxPromptTokens` | `TOKEN_BUDGET` |
| `finish` called | `COMPLETED` |

**`MaxRunDuration` is wall-clock, checked between steps; it is not a deadline on the run's
context.** If it were, the forced `finish` would run on an already-expired context and could
never succeed. The context carries only per-call deadlines: `LLM_TIMEOUT` for each LLM call,
including the forced one, and `mcpclient`'s own timeout for each tool call. This mirrors
`internal/ingest`, where the terminal writes run on a context detached from the document
deadline for the same reason.

**The forced `finish` occupies a step number**, so `step_count` can exceed `max_steps` by
one and the timeline shows why the run ended. If it too returns no usable `finish` call, it
is **not** retried: the run is `FAILED` with `stop_reason = ERROR` and an `error` naming the
bound that triggered it. A run with no diagnosis is not a success.

### Cancellation restarts the run; it does not end it

A cancelled context stops the loop immediately with no final call, and **nothing terminal is
written**. The run stays `RUNNING`, its Kafka offset uncommitted, and the lease reclaims and
restarts it exactly as ADR 0009 describes. Writing a terminal state here would be a claim
that the investigation is over, which is what the restart machinery exists to contradict.

`stop_reason = CANCELLED` therefore has no writer yet. It is reserved for the explicit
cancel endpoint S8 may add; until then it is dead vocabulary in S1, which is recorded as a
known limitation rather than removed.

### A tool has four outcomes, and only one of them is the loop's problem

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
| `Call` returns an error | `OK` | `ERROR` | check `ctx.Err()` first: if the run was cancelled, stop; otherwise the error text is the observation and the loop continues |

**The step is `OK` in all four.** A step's status says whether the step produced a usable
action and an observation, not whether the observation was good news. `ERROR` on a step is
reserved for `action_type = none`, where the model returned no tool call and there is
nothing to record.

**If `Refused` and a non-`OK` `Status` ever disagree, `Status` wins.** `mcpclient` already
reasons this way — a result the server explicitly marked as an error must never be recorded
as a success — so the two layers agree rather than each guessing.

**A refusal spends one of `MaxToolCalls`.** It consumed a call, and pretending otherwise
would let an agent that keeps guessing wrong run unbounded.

**A failing dependency does not end the run.** Prometheus being down is a reason to look at
the logs, not a reason to stop investigating, so the failure text becomes the observation
and the loop carries on. There is deliberately **no second bound** on repeated failures of
the same tool: `MaxToolCalls` already bounds the waste, and a per-tool failure counter would
be a second limit for a case the first one covers.

### A model that answers in prose

Native tool calling does not guarantee a tool call. A response with no tool call — or a
`finish` call whose arguments will not decode, or whose `root_cause` is blank — is a step
with `action_type = none`, `action = {}` and `status = ERROR`, carrying the failure in
`error`. The loop retries once; the explicit instruction to use a tool is the `user` message
the builder renders for a `none` step, so the retry is an ordinary next iteration rather than
a second code path. A second failure ends the run `FAILED` with `stop_reason = ERROR`.

The step is persisted rather than dropped because these three tables exist to make a run
auditable, and "the model stopped calling tools" is the single most useful thing an
evaluation can count. That is the same argument this spec makes for `REFUSED`.

Responses may carry `content` alongside `tool_calls`. That is the model's reasoning and
**is never persisted, nor replayed into the context**. S1's schema has nowhere to put it,
which is deliberate, and the tests assert it does not appear.

### Evidence

`finish` carries `evidence: [{step, note, document_id?}]`, and each item becomes one row:

| Column | Where it comes from |
| --- | --- |
| `step_id` | the step number resolved to that step's `id` |
| `source_type` | the cited step's `action_type`: `retrieve` → `retrieval`, `tool_call` → `tool` |
| `tool_call_id` | that step's `tool_calls` row, when `source_type` is `tool`; otherwise `NULL` |
| `document_id` | the cited `document_id`, **only if it appears among that step's hits**; otherwise `NULL` |
| `source_ref` | the cited hit's `Source` (the document's filename) for a retrieval, or `search_knowledge(<query>)` when no hit was identified; the tool name for a tool |
| `summary` | the cited hit's chunk for a retrieval, otherwise the cited step's observation; capped by the same helper |
| `note` | the model's reason, unchanged |

`document_id` is on the item because without it the `evidence` table cannot answer the one
question S1 built it for — which documents actually get cited — since a retrieval step
returns several. It is optional, and one field on a tool called once a run is a far smaller
ask than the rejected alternative of a field on every tool schema.

A step number or a `document_id` the model invents is **dropped with a warning** rather than
failing the run: a wrong citation should not discard a correct diagnosis. An invented step
drops the row; an invented document leaves `document_id` `NULL` and keeps it. A citation
naming a `none` step is dropped for the same reason: `source_type` has two values and neither
describes a step that did nothing.

The loop therefore keeps each retrieval step's hits — their `document_id`, `Source` and
content — until the run ends. That is what validates a cited `document_id` and what fills
`source_ref` and `summary` with the hit the model actually cited rather than all five.

`agent_runs.final_result` holds the `finish` arguments as they were given. What
`docs/architecture.md` calls the diagnosis's *references* is this evidence, not a separate
field; that sentence is updated alongside this spec.

### Streaming is M27's problem, not M21's

M21 does not stream. The final diagnosis arrives as the arguments of a `finish` tool call,
not as assistant `content`, so streaming it to the browser as `answer.delta` means streaming
**tool-call argument deltas** — a different mechanism from streaming prose, and one nothing
before M27 needs. Building it now would be building it against a consumer that does not
exist. M27 or S9 decides between streaming those deltas and re-rendering the completed
result; this spec records the consequence rather than pre-empting the choice.

### Configuration

| Variable | Default | |
| --- | --- | --- |
| `AGENT_MAX_STEPS` | 8 | |
| `AGENT_MAX_TOOL_CALLS` | 12 | |
| `AGENT_MAX_RUN_DURATION` | 5m | wall-clock |
| `AGENT_MAX_PROMPT_TOKENS` | 60000 | ceiling on one prompt |
| `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL` | see `.env.example` | already present |
| `LLM_TIMEOUT` | 60s | per attempt |
| `LLM_MAX_RETRIES` | 2 | |

Two loaders in `internal/config`, following the existing split by concern: `LoadLLM` (M21)
and `LoadAgent` (M22, and the invariant above). Every new variable goes into `.env.example`
and into `config_test.go`'s `isolate` list, or the tests inherit the developer's sourced
`.env`. Retry classification follows
`internal/embed`: 429 and 5xx and network failures are retried with backoff and a capped
`Retry-After`; a 400-family status is the request being wrong and is not.

The per-run budget and `model` are recorded on `agent_runs` **when the run is created**, so a
run stays interpretable after the configuration changes. The creating endpoint is S8's; it
reads both from these loaders.

## Alternatives

**A hand-parsed JSON action envelope.** Works on any provider, including ones without tool
calling. Rejected: it means writing and testing a tolerant parser for code-block wrapping,
trailing prose and missing fields, and model adherence is worse than with native tool
calling. The cost is a provider dependency that M33's switch test must exercise.

**A cumulative token budget for the run.** Direct cost control, and it matches reading S1's
`prompt_tokens` column as a budget. Rejected: the bounds on steps, tool calls and duration
already bound cost, and a cumulative budget would need a *second* number — the model's
context window — for the invariant that removed the pruning strategy. One knob, one meaning.

**A rendered text transcript as a single user message.** Simpler, no `tool_call_id`
bookkeeping. Rejected: it departs from the shape native tool calling is trained on, and some
providers reject a history whose tool calls have no paired replies, which M33 would find.

**A sliding context window with older observations summarized.** Saves tokens on long runs.
Rejected: under the current bounds it would never trigger, so it is a strategy and a test
suite for a situation that cannot arise.

**Evidence attached to each step.** Closer to how someone investigating actually takes
notes, and it survives a truncated run. Rejected: it puts an optional field on every tool
schema and asks the model for a second decision at every step. Revisit if truncated runs
turn out to matter.

**Resolving a citation's document automatically, to the step's top hit.** No burden on the
model at all. Rejected: "which documents get cited" would then measure "which documents rank
first", which is the retrieval metric M12 already reports rather than a second signal.

**No forced `finish` on a bound.** Simpler, and the run ends exactly at its budget.
Rejected: it makes a bounded run useless, and `docs/architecture.md` promises a partial
result.

**Parallel tool calls per step.** Faster. Rejected by S1's schema, and it would make budget
accounting and event ordering harder for a latency gain nobody is waiting on.

## Detailed Implementation

**M21 — `internal/llm`**

OpenAI-compatible chat completions with tool calling. `Message` carries `Role`, `Content`,
`ToolCalls` and `ToolCallID`, because the context is a real message sequence. Tool
definitions take the MCP `InputSchema` as raw JSON, unchanged. Timeout per attempt, bounded
retry classified as above, `usage` extraction. A scriptable `Fake` returning a predetermined
sequence of responses — including ones with no tool call and ones with several — which is
what makes M23 deterministic. No streaming.

**M22 — `internal/agent` types and context**

`Run`, `Step`, `StepRef`, `Action`, `Observation`, `ToolCall`, `Evidence`, `FinalResult`;
the system prompt; `ContextBuilder`; `search_knowledge` and its rendering; the `Tool`
interface the loop calls, small and present because the tests need fakes. `LoadAgent` and
the invariant land here too.

**M23 — the loop**

The algorithm above, the bounds, the prose retry, the forced finish, and evidence
resolution. No infrastructure in its tests: the loop reports steps through a callback, so
M23 runs against a scripted `llm.Fake` and fake tools.

**What M25 owes this spec.** Persisting and event publishing are M25 and S8, but two pieces
belong to decisions made here:

- `ClaimRun` deletes the previous attempt's rows in the same transaction as the claim, and
  resets `step_count`, `tool_call_count`, `prompt_tokens` and `completion_tokens` to zero.
  `DELETE FROM agent_steps WHERE run_id = ?` is sufficient on its own: `tool_calls` and
  `evidence` cascade from `agent_steps`. The four counters are written only by `FinishRun`,
  so a reclaimed run's are already zero — the reset is one clause guarding a future writer,
  not a correction.
- The callback writes the step, its `tool_calls` row and, on `finish`, the `evidence` rows,
  publishes each step's event, and returns the `StepRef` the loop needs to resolve citations.

## Verification

- `make check` — the loop against a scripted fake LLM and fake tools: convergence; each of
  the four bounds, including that the forced `finish` runs and the status is `SUCCEEDED`
  with the right `stop_reason`; a forced `finish` that itself fails leaving the run `FAILED`
  with `stop_reason = ERROR`; external cancellation stopping immediately with no final call
  and no terminal write; a prose response recorded as `action_type = none` and its retry; a
  response carrying two tool calls; a call naming an unknown tool; a refused tool call
  recorded as `REFUSED` and a failing one as `ERROR`, both leaving the step `OK` and the run
  going; `mcpclient.Call` returning an error; a retrieval whose `Search` fails, leaving the
  step `OK` and the run going; a `finish` whose `root_cause` is blank; an invented evidence
  step number, an invented `document_id` and a citation naming a `none` step; and that no
  `content` reaches the recorded steps or the context.
- `ContextBuilder`: the message sequence pairs every tool call with a `tool` reply —
  including after a response whose extra tool calls were dropped — a `none` step renders as
  its `user` instruction, and an over-cap observation is truncated whole-hit for retrieval.
- `LoadAgent` fails when `MaxSteps` violates the context invariant.
- `make test-integration` — one real run against DeepSeek with a live `ops-mcp`.

## Known limitations, accepted

**A run can exceed its bounds by one step and one call.** The forced `finish` is one LLM
call past the token ceiling, and because bounds are checked between steps, a run can overrun
`MaxRunDuration` by a whole step plus that call — each of them up to
`LLM_TIMEOUT × (1 + LLM_MAX_RETRIES)`, three minutes at the defaults. Bounded and
deliberate.

**Cumulative token spend is unbounded.** In the worst case an eight-step run sends roughly
70k prompt tokens in total. The step and duration bounds are what limit it.

**`stop_reason = CANCELLED` has no writer.** Cancellation restarts the run instead of ending
it, so the value waits for an explicit cancel endpoint.

**An interrupted run looks stuck.** It stays `RUNNING` until the lease expires, exactly as an
interrupted document stays `PROCESSING`.

**Evidence exists only if `finish` runs.** A cancelled run has steps and tool calls but no
evidence rows. The timeline is still auditable.

**A provider that omits `usage` leaves the token counters at zero**, with a warning. M33's
switch test is where that would first show up.

**Native tool calling is a provider dependency.** A provider without it cannot run this
agent, so M33's switch test must exercise a tool call rather than only checking that the
endpoint answers.
