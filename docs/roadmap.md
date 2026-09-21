# Roadmap

34 implementation milestones across 7 phases, plus 7 Tier A specs. See
`docs/workflow.md` for what Tier A and Tier B mean and how each milestone is executed.

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
| M11 | Dense kNN retrieval with metadata filtering; `GET /api/search` for debugging (Tier B) | todo |
| M12 | Knowledge corpus in `testdata/knowledge/` (runbook, postmortem, service docs) and the evaluation set built from it; a repeatable Go test reporting Recall@1/3/5, baseline in `docs/rag-eval.md`. The corpus is written once here and reused by Phase C and the agent evaluation (Tier B) | todo |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase C — Incident Lab and observability base

| | Milestone | Status |
| --- | --- | --- |
| **S6** | **Spec: `mcp-tool-boundary`** — tool interfaces, read-only guarantees, the allowlist model, and the structured log format `read_service_logs` consumes. **Written here, one phase ahead of the milestones it gates**, because M13 emits that log format and M19 reads it; defining it once, before either exists, is why it moved | todo |
| M13 | `checkout-service` → `payment-service`, each with a business endpoint, `/health`, `/metrics`, and `/fault` injecting latency, 5xx and CPU load; structured JSON logs to a mounted directory, in the format S6 defines (Tier B) | todo |
| M14 | Prometheus scraping both services; Grafana with a provisioned datasource (Tier B) | todo |
| M15 | Fault scenarios: a script reproducing the payment-service latency incident and the other injectable faults, against the corpus M12 already wrote (Tier B) | todo |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase D — MCP tool layer

Specified by S6, which is written at the top of Phase C.

| | Milestone | Status |
| --- | --- | --- |
| M16 | `ops-mcp` skeleton: official MCP Go SDK, streamable HTTP, tool registry, allowlist configuration; own container | todo |
| M17 | `prometheus_query` (Tier B): instant and range queries, validation, LLM-friendly result summarization | todo |
| M18 | `http_probe` (Tier B): strict host allowlist, forced timeout, truncated body | todo |
| M19 | `read_service_logs` (Tier B): reads the log format S6 defines; jailed to configured directories with explicit symlink and `..` escape tests; filters by service, time window, pattern and line cap | todo |
| M20 | MCP client and tool adapter in the main application; unit tested against a fake MCP server | todo |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase E — Agent runtime

| | Milestone | Status |
| --- | --- | --- |
| **S7** | **Spec: `agent-runtime`** — types, state machine, context budget, limits, failure semantics, and resuming a run from its last persisted step | todo |
| M21 | LLM client: OpenAI-compatible chat completions with streaming, timeouts, bounded retries, token accounting; tolerant structured-output parser; scriptable fake | todo |
| M22 | Core types and `ContextBuilder`: `AgentRun`, `AgentStep`, `Action`, `Observation`, `ToolCall`, `Evidence`, `FinalResult`; context assembled within a token budget; no chain-of-thought persisted | todo |
| M23 | Bounded agent loop: `MaxSteps`, `MaxToolCalls`, `MaxRunDuration`, token budget. Deterministic tests for convergence, each limit, cancellation, malformed output and tool failure — no infrastructure required | todo |
| M23a | Checkpoint and resume: a run reclaimed after a crash continues from its last persisted step instead of restarting, rebuilding context from `agent_steps` and re-validating stale tool observations | todo |
| **S8** | **Spec: `agent-execution-and-events`** — event schema and worker idempotency | todo |
| M24 | Redis Streams event bus: event schema, publisher, capped and expiring streams | todo |
| M25 | `agent-worker`: run creation produces to Kafka; worker executes the loop, persists steps and tool calls, publishes events, writes the final result; redelivery does not duplicate a run; a lease-reclaimed run resumes rather than restarts | todo |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase F — Real time and UI

| | Milestone | Status |
| --- | --- | --- |
| **S9** | **Spec: `sse-streaming`** — reconnection semantics and goroutine lifecycle | todo |
| M26 | SSE endpoint reading the run's Redis Stream; `Last-Event-ID` reconnection; explicit goroutine leak tests | todo |
| M27 | Streaming answer: final diagnosis reaches the browser as `answer.delta` events, persisted once complete | todo |
| M28 | Minimal front end (Tier B): a single static HTML page with vanilla JS and the browser's native `EventSource` — upload, incident creation, run start, live timeline, final diagnosis with references. No build step | todo |

*Phase close:* `mean-review`, `CLAUDE.md`.

## Phase G — Observability, quality, polish

| | Milestone | Status |
| --- | --- | --- |
| M29 | OpenTelemetry across the API, Kafka produce/consume with context propagation, retrieval, embedding, LLM, MCP calls and agent runs — one run, one trace (Tier B) | todo |
| M30 | Prometheus metrics and two Grafana dashboards: agent run health and pipeline health (Tier B) | todo |
| M31 | End-to-end test: inject a fault, file an incident, run the agent, assert it used retrieval and tools and named the right service (Tier B) | todo |
| M32 | Agent evaluation (Tier B): run the agent against every injectable fault scenario and report diagnosis accuracy, steps, tool calls, token cost and latency percentiles in `docs/agent-eval.md`. Includes scenarios the agent should fail to diagnose, so the numbers mean something | todo |
| M33 | Provider-switch smoke test against a second hosted OpenAI-compatible provider; finish `docs/architecture.md`, demo script, screenshots, README | todo |

*Phase close:* full-repository `mean-review`, root `CLAUDE.md`.
