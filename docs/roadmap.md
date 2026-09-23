# Roadmap

32 implementation milestones across 7 phases, plus 7 Tier A specs — **all done**. See
`docs/workflow.md` for what Tier A and Tier B mean and how each milestone is executed.
The project's own measurements are [`docs/agent-eval.md`](agent-eval.md) and
[`docs/rag-eval.md`](rag-eval.md).

**Status legend:** `done` · `in progress` · `todo`

`S*` entries are Tier A specs; each covers the milestones listed under it and must be
written, goldfish-tested and committed before those milestones are implemented.

## Phase A — Foundations

| | Milestone | Status |
| --- | --- | --- |
| M1 | Repository bootstrap: git, Go module, Makefile, `.gitignore`, `.env.example`, README skeleton | **done** |
| M2 | Infrastructure packages: `config`, `log`, `shutdown`, `httpx`. Unit tests only | **done** |
| **S1** | **Spec: `data-model-and-api-surface`** — schema and state machine contract | **done** |
| M3 | MySQL in Compose; migrations for `documents`, `incidents`, `agent_runs`, `agent_steps`, `tool_calls`, `evidence`; `database/sql` store with state transitions as conditional updates; integration tests behind `-tags=integration` that skip cleanly without infrastructure | **done** |
| M4 | Gin API skeleton: `/healthz`, request-id and logging middleware, uniform error responses, incident create/list/get | **done** |
| M5 | Document upload: multipart `POST /api/documents`, file written to the shared volume, row recorded `PENDING`, `GET /api/documents` | **done** |

*Phase close:* `mean-review`, then `CLAUDE.md` for the `internal/` packages.

## Phase B — Ingestion and RAG

| | Milestone | Status |
| --- | --- | --- |
| **S2** | **Spec: `async-messaging-and-idempotency`** — message schemas, delivery semantics, lease reclaim and the reconciler | **done** |
| M6 | Kafka (KRaft, single node) in Compose; `internal/mq` over franz-go; `documents.ingest.v1` and `agent.runs.v1`; at-least-once with database-level idempotency; lease reclaim and the reconciler; `ingestion-worker` with a placeholder handler; integration test for duplicate delivery | **done** |
| **S3** | **Spec: `document-ingestion-pipeline`** — chunking strategy, Elasticsearch mapping, failure semantics | **done** |
| M7 | Markdown/TXT parser and heading-aware, token-bounded chunker. Pure functions, heavily unit tested | **done** |
| M8 | `internal/embed`: OpenAI-compatible embedding client for hosted `BAAI/bge-m3` (SiliconFlow), with batching, timeouts and bounded retries | **done** |
| M9 | Elasticsearch single node; chunk mapping with `dense_vector`; bulk index and delete-by-document | **done** |
| M10 | `ingestion-worker` end to end: the M6 placeholder handler is replaced with parse → chunk → embed → index → `READY`/`FAILED` | **done** |
| M11 | Dense kNN retrieval with metadata filtering; `GET /api/search` for debugging (Tier B) | **done** |
| M12 | Knowledge corpus in `testdata/knowledge/` (runbook, postmortem, service docs) and the evaluation set built from it; a repeatable Go test reporting Recall@1/3/5, baseline in `docs/rag-eval.md`. The corpus is written once here and reused by Phase C and the agent evaluation. **It must contain deliberate confusables** — several documents sharing symptoms and metric names, so a query has to discriminate rather than match the only document on its topic; see the note below (Tier B) | **done** |

**M12's corpus contains deliberate confusables** — several documents sharing symptoms and
metric names — so a query has to discriminate rather than match the only document on its
topic. Baseline and what the numbers do and do not mean: `docs/rag-eval.md`.

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase C — Incident Lab and observability base

| | Milestone | Status |
| --- | --- | --- |
| **S6** | **Spec: `mcp-tool-boundary`** — tool interfaces, read-only guarantees, the allowlist model, and the structured log format `read_service_logs` consumes. **Written here, one phase ahead of the milestones it gates**, because M13 emits that log format and M19 reads it; defining it once, before either exists, is why it moved | **done** |
| M13 | `checkout-service` → `payment-service`, each with a business endpoint, `/health`, `/metrics`, and `/fault` injecting latency, 5xx and CPU load; structured JSON logs to a mounted directory, in the format S6 defines (Tier B) | **done** |
| M14 | Prometheus scraping both services; Grafana with a provisioned datasource (Tier B) | **done** |
| M15 | Fault scenarios: a script reproducing the payment-service latency incident and the other injectable faults, against the corpus M12 already wrote (Tier B) | **done** |

M13's spec is `docs/plans/incident-lab.md`; M14 and M15 share
`docs/plans/observability-and-fault-scenarios.md`. The whole lab runs behind compose's
`lab` profile: `make up-lab`, then `make scenario SCENARIO=payment-latency`.

**cAdvisor reports nothing on a Docker daemon using the containerd image store**, so
`container_cpu_*{name="checkout-service"}` can be empty; the spec's known limitations say
what that costs and what still works.

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase D — MCP tool layer

Specified by S6, which is written at the top of Phase C.

| | Milestone | Status |
| --- | --- | --- |
| M16 | `ops-mcp` skeleton: official MCP Go SDK, streamable HTTP, tool registry, allowlist configuration; own container | **done** |
| M17 | `prometheus_query` (Tier B): instant and range queries, validation, LLM-friendly result summarization | **done** |
| M18 | `http_probe` (Tier B): strict host allowlist, forced timeout, truncated body | **done** |
| M19 | `read_service_logs` (Tier B): reads the log format S6 defines; takes a service name rather than a path, so there is no traversal to defend against; filters by time window, minimum level and substring | **done** |
| M20 | MCP client and tool adapter in the main application, tested against a real `ops-mcp` rather than a fake, so both halves of the result envelope are exercised | **done** |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase E — Agent runtime

| | Milestone | Status |
| --- | --- | --- |
| **S7** | **Spec: `agent-runtime`** — types, state machine, context budget, limits and failure semantics | **done** |
| M21 | LLM client: OpenAI-compatible chat completions with native tool calling, timeouts, bounded retries, token accounting; scriptable fake. No streaming and no structured-output parser — S7 supersedes both | **done** |
| M22 | Core types and `ContextBuilder`: `AgentRun`, `AgentStep`, `Action`, `Observation`, `ToolCall`, `Evidence`, `FinalResult`; context assembled within a token budget; no chain-of-thought persisted | **done** |
| M23 | Bounded agent loop: `MaxSteps`, `MaxToolCalls`, `MaxRunDuration`, token budget. Deterministic tests for convergence, each limit, cancellation, malformed output and tool failure — no infrastructure required | **done** |
| **S8** | **Spec: `agent-execution-and-events`** — event schema, worker idempotency, and the run endpoints. `POST /api/incidents/{id}/runs` and `GET /api/runs/{id}` are in no spec: S1's API surface stops at documents, so without this there is no way to start or read a run over HTTP | **done** |
| M24 | Redis Streams event bus: event schema, publisher, capped and expiring streams | **done** |
| M25 | `agent-worker`: run creation produces to Kafka; worker executes the loop, persists steps and tool calls, publishes events, writes the final result; redelivery does not duplicate a run; a lease-reclaimed run restarts, deleting the previous attempt's rows first, the way re-ingesting a document does (ADR 0009) | **done** |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase F — Real time and UI

| | Milestone | Status |
| --- | --- | --- |
| **S9** | **Spec: `sse-streaming`** — reconnection semantics and goroutine lifecycle | **done** |
| M26 | SSE endpoint reading the run's Redis Stream; `Last-Event-ID` reconnection; explicit goroutine leak tests. M27's `answer.delta` was cancelled — [ADR 0010](adr/0010-no-answer-delta.md) | **done** |
| M28 | Minimal front end (Tier B): a single static HTML page with vanilla JS and the browser's native `EventSource` — upload, incident creation, run start, live timeline, final diagnosis with references. No build step | **done** |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase G — Observability, quality, polish

| | Milestone | Status |
| --- | --- | --- |
| M29 | OpenTelemetry across the API, Kafka produce/consume with context propagation, retrieval, embedding, LLM, MCP calls and agent runs — one run, one trace (Tier B) | done |
| M30 | Prometheus metrics and two Grafana dashboards: agent run health and pipeline health (Tier B) | done |
| M31 | End-to-end test: inject a fault, file an incident, run the agent, assert it used retrieval and tools and named the right service (Tier B) | done |
| M32 | Agent evaluation (Tier B): run the agent against every injectable fault scenario and report diagnosis accuracy, steps, tool calls, token cost and latency percentiles in `docs/agent-eval.md`. Includes scenarios the agent should fail to diagnose, so the numbers mean something | done |
| M33 | Provider-switch smoke test against a second hosted OpenAI-compatible provider; finish `docs/architecture.md`, demo script, README | done |

**M33 shipped without screenshots.** `make demo` produces them on demand and the README
carries a real run's trajectory as text instead, so nothing here depends on an image that
would go stale the next time the page changes.

**M32's spec is written after M31 has run**, not before: which scenarios discriminate is a
question about how the agent actually behaves, and the scenario set is the expensive part to
get wrong.

**What M32 has to be, to be worth reporting.** Three scenarios give an accuracy with four
possible values, which is a smoke test wearing a benchmark's clothes. Three requirements,
agreed before the milestone was specified:

- **Eight to ten scenarios, built around one symptom with several root causes.** A rising
  p99 on payment-service can be pool saturation, a slow processor, or a CPU regression, and
  the runbooks prescribe *opposite* remediations for them. Telling those apart is the
  product's core claim; one scenario per fault kind never tests it.
- **Several runs per scenario, reported as a distribution.** An LLM is not deterministic, so
  a single pass cannot say how much of an accuracy figure was luck.
- **The full trajectory of every run, not just the verdict.** "Wrong" and "wrong, but it
  queried the right metric at step 3 and misread it" are different problems, and only the
  second says what to change.

**A constraint the lab imposes.** `/fault` toggles latency, errors and CPU at runtime, but
`PAYMENT_POOL_SIZE` and `PAYMENT_PROCESSOR_LATENCY_MS` are startup configuration. So
"the pool is undersized" and "the pool is saturated because the processor is slow" — the
sharpest pair in the list above — cannot both be produced without either restarting a
service between scenarios or giving the lab runtime control of those two values. Whichever
M32 chooses, it is a decision that milestone has to make rather than discover.

*Phase close:* full-repository `mean-review`, root `CLAUDE.md`.
