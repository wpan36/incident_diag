# internal/llm

## Purpose

An OpenAI-compatible chat completions client with native tool calling: timeouts, bounded
retry, token accounting, and a scriptable fake. Deliberately thin — the provider enforces
the tool schemas, so there is no parser here for JSON the model wrote in prose.

## Contents

- `llm.go` — the wire vocabulary (`Message`, `ToolCall`, `FunctionCall`, `Tool`, `Request`,
  `Response`, `Usage`), the `Chatter` interface, `Client`, `New`, `Chat`, the retry
  classification and `Retry-After` handling; `APIError`, `ErrBadResponse`.
- `fake.go` — `Fake`, a scriptable `Chatter`, with `NewFake`, `CallTurn`, `CallsTurn`,
  `TextTurn` and `NewToolCall`; `ErrScriptExhausted`.
- `llm_test.go` — unit tests against `httptest`, including retry classification and a
  provider that reports no usage.
- `provider_integration_test.go` — M33: a second OpenAI-compatible provider makes a real
  tool call, skipping unless `TEST_ALT_LLM_BASE_URL` is set.

## How it fits in

`internal/agent` depends on `Chatter` and nothing else here. `config.LoadLLM` supplies the
endpoint, key and model, and switching provider is those three variables — which
`provider_integration_test.go` checks against SiliconFlow's Qwen3-8B, by making it call a
tool rather than by checking that the endpoint answers.

## Gotchas

- **No streaming.** The diagnosis arrives as the arguments of a `finish` tool call, not as
  assistant content, so streaming it means streaming tool-call argument deltas. That was
  M27, and it was cancelled — see [ADR 0010](../../docs/adr/0010-no-answer-delta.md).
- **`Arguments` is a string holding JSON**, because that is what the wire format carries.
  It is passed through unchanged so a provider's formatting cannot change what the audit
  trail records; the agent validates it before it reaches a JSON column.
- **The 400 family is not retried, which is the opposite of `internal/embed`.** There, 401
  and 403 are retried so an ingestion backlog heals itself once an operator fixes the key.
  Here a run is one interactive request, and spending its budget on a request that cannot
  succeed only delays the failure.
- **`parallel_tool_calls: false` is sent whenever tools are**, and `tool_choice` only when
  the caller sets it. Both are provider dependencies M33's switch test has to exercise; the
  agent's "execute the first call and drop the rest" rule is what covers a provider that
  ignores the first of them.
- **A response with no `usage` leaves the counters at zero, with a warning.** Estimating
  instead would put a number in `agent_runs` that nobody can reconcile with the bill.
- **The HTTP client has no timeout of its own.** `LLM_TIMEOUT` is applied per attempt
  through the context, so a retry gets a fresh one.
- **`Fake` ships in the package, not in a `_test.go` file**, following `embed.Fake`: the
  code that most needs testing without a provider is `internal/agent`. An exhausted script
  is an error rather than a repeated last response, so a bound test that made one call too
  many fails instead of passing while testing nothing.
