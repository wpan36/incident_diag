# Architecture

This document describes the agreed architecture of `incident_diag`. It is a living
document — when the design changes, this file changes with it, and the reasoning behind
individual decisions lives in `docs/adr/`.

## What the system does

A user uploads internal knowledge (runbooks, postmortems, service documentation), files an
incident such as "payment-service latency spike", and starts an agent run. The agent
investigates by retrieving relevant internal knowledge and calling read-only operational
tools, then produces a diagnosis with the evidence behind it. The user watches the
investigation happen in real time.

The agent is **read-only by construction**. It cannot run arbitrary commands, cannot write
to any system it investigates, and its tools are restricted by explicit allowlists.

## Component overview

```
                    ┌─────────────┐
                    │   Browser   │
                    │ static HTML │
                    └──────┬──────┘
                           │ REST + SSE
                    ┌──────▼──────────────────────────┐
                    │        api (Gin)                │
                    │  documents / incidents / runs   │
                    │  SSE endpoint                   │
                    └──┬────────┬────────────┬────────┘
                       │        │            │
              produce  │        │ read       │ read
                       │        │            │
        ┌──────────────▼──┐  ┌──▼────────┐ ┌─▼──────────────┐
        │     Kafka       │  │  MySQL    │ │ Redis Streams  │
        │ documents.      │  │ system of │ │ run events     │
        │   ingest.v1     │  │  record   │ │ (fan-out only) │
        │ agent.runs.v1   │  └──▲────────┘ └─▲──────────────┘
        └──┬───────────┬──┘     │            │
           │           │        │            │
  ┌────────▼──────┐ ┌──▼────────┴────────────┴──┐
  │  ingestion-   │ │      agent-worker         │
  │    worker     │ │  bounded agent loop       │
  └───┬───────┬───┘ └──┬──────────┬─────────┬───┘
      │       │        │          │         │
      │  ┌────▼────────▼───┐  ┌───▼─────┐ ┌─▼────────┐
      │  │ Elasticsearch   │  │  chat   │ │ ops-mcp  │
      │  │ chunks:         │  │  LLM    │ │ MCP over │
      │  │ dense_vector    │  │DeepSeek │ │  HTTP    │
      │  └────▲────────────┘  └─────────┘ └─┬────────┘
      │       │                             │
  ┌───▼───────┴──┐                  ┌───────▼─────────────────┐
  │ embeddings   │                  │     Incident Lab        │
  │ bge-m3       │                  │ checkout-service        │
  │(SiliconFlow) │                  │   -> payment-service    │
  └──────────────┘                  │ Prometheus / logs       │
                                    └─────────────────────────┘
```

## Storage responsibilities

**MySQL is the system of record.** Every durable entity and every state transition lives
here: `documents`, `incidents`, `agent_runs`, `agent_steps`, `tool_calls`. State
transitions are performed as conditional `UPDATE`s inside the store layer, which is what
makes workers idempotent under at-least-once delivery.

**Elasticsearch holds retrievable knowledge**, never authoritative state. A document's
chunks can always be rebuilt from the original file plus MySQL metadata. Each chunk
carries `document_id`, `chunk_id`, `chunk_index`, `service`, `document_type`, `source`,
`heading_path`, `content`, `embedding` and `indexed_at`.

Reads and writes go through the alias `chunks` rather than a concrete index, so changing
the embedding model can be done by building a second index alongside the first and
switching the alias, without retrieval going down for the reindex.

**Redis Streams carries agent run events.** This data is deliberately disposable: streams
are capped and expire. Losing it costs a live timeline, never a fact — the authoritative
record of what the agent did is in MySQL.

**Kafka is the durable task queue.** Two topics: `documents.ingest.v1` and
`agent.runs.v1`.

Kafka and Redis Streams both being present is intentional, not accidental duplication.
Kafka gives durable, replayable, at-least-once work distribution; Redis Streams gives
cheap low-latency fan-out to however many browsers are watching a run. See
[ADR 0001](adr/0001-kafka-and-redis-streams.md).

## Data flows

### Document ingestion

```
POST /api/documents
  -> write file to the shared volume
  -> MySQL: documents row, status PENDING
  -> produce to documents.ingest.v1
        -> ingestion-worker consumes
             -> parse (Markdown / TXT)
             -> chunk (heading-aware, token-bounded)
             -> embed (OpenAI-compatible /v1/embeddings)
             -> bulk index through the chunks alias
        -> MySQL: status READY, or FAILED with a reason
```

Redelivery is expected. The worker deletes any existing chunks for the `document_id`
before indexing, and the terminal status write is a conditional update, so reprocessing
the same message converges to the same state instead of duplicating chunks.

### Agent execution

```
POST /api/incidents/:id/runs
  -> MySQL: agent_runs row, status PENDING
  -> produce to agent.runs.v1
        -> agent-worker consumes
             -> bounded agent loop (see below)
             -> per step: MySQL agent_steps / tool_calls
             -> per step: publish event to Redis Stream
        -> MySQL: final status and diagnosis

GET /api/runs/:id/events   (browser)
  -> api reads the run's Redis Stream
  -> streams to the browser as SSE
```

The agent worker never holds a browser connection. It only publishes events. The API
server is the only component that terminates SSE, which is what lets workers restart,
scale, or crash without affecting connected clients, and lets a client reconnect with
`Last-Event-ID` and catch up from the stream.

## The agent runtime

The agent loop is written by hand rather than delegated to a framework, because the loop
is the part of this project worth understanding. Core concepts:

- `AgentRun` — one investigation, with a status and a budget
- `AgentStep` — one iteration of the loop
- `Action` — the structured decision the LLM returned
- `ToolCall` / `Observation` — a tool invocation and its result
- `Evidence` — an observation the agent chose to keep as supporting its conclusion
- `FinalResult` — probable root cause, affected service, supporting evidence, suggested
  next actions, references

The loop:

```
incident + user input
  -> ContextBuilder assembles a prompt within a token budget
  -> LLM returns a structured Action
  -> execute: RAG retrieval, or an MCP tool call, or finish
  -> Observation recorded, evidence optionally kept
  -> context updated
  -> repeat until the agent finishes or a bound is hit
```

Every run is bounded by `MaxSteps`, `MaxToolCalls`, `MaxRunDuration` and a token budget.
Unbounded loops are not possible: hitting a bound is a normal, tested outcome that
produces a partial result rather than an error.

**A run interrupted by a crash restarts.** The lease reclaims it, the previous attempt's
steps, tool calls and evidence are deleted, and it runs again from the beginning — the same
delete-then-redo convergence that lets a re-ingested document avoid duplicating its chunks.
Resuming from the last persisted step was specified and then dropped before it was written;
[ADR 0009](adr/0009-restart-instead-of-resume.md) says why. The rows exist to make a run
auditable, which is all they are for.

`context.Context` is threaded through the entire loop, so cancelling a run cancels the
in-flight LLM request and tool call rather than waiting for them.

**Model chain-of-thought is never stored.** Persisted state is limited to what can be
audited: the action taken, the tool call and its arguments, a summary of the tool result,
the evidence kept, the status, and the final result.

## Tools (MCP)

Operational tools live in a separate process, `ops-mcp`, speaking MCP over streamable
HTTP. Keeping them out of the agent worker makes the trust boundary explicit: the agent
can only do what this server exposes.

The first version exposes three read-only tools:

| Tool | Constraint |
| --- | --- |
| `prometheus_query` | fixed Prometheus endpoint from config; bounded range, step and series |
| `http_probe` | takes a service name, not a URL; forced timeout, truncated body |
| `read_service_logs` | takes a service name, not a path; parsed and filtered structurally |

No tool accepts a path or a URL. Each takes a name the server resolves against its own
configuration, so path traversal is not defended against — it cannot be expressed. See
[the tool boundary spec](plans/mcp-tool-boundary.md).

## LLM and embeddings

Both are reached over the OpenAI-compatible HTTP API and configured independently:

```
LLM_BASE_URL / LLM_API_KEY / LLM_MODEL
EMBEDDING_BASE_URL / EMBEDDING_API_KEY / EMBEDDING_MODEL
```

The runtime knows nothing about the provider. In practice the chat model is DeepSeek and
the embedding model is `BAAI/bge-m3` hosted by SiliconFlow, both reached over the same
protocol.

Neither runs locally, which is deliberate: the project requires no GPU and no model
weights, so anyone can clone it and run the whole stack. Vendor independence is
demonstrated by a smoke test that points `LLM_BASE_URL` at a second hosted provider and
runs an investigation unchanged. See [ADR 0006](adr/0006-hosted-embeddings-drop-vllm.md),
which supersedes ADR 0002.

## Retrieval

Dense vector retrieval with metadata filtering on `service` and `document_type`, measured
against a fixed evaluation set and tracked in `docs/rag-eval.md` with Recall@1/3/5.

Hybrid retrieval — BM25 and dense recall fused with RRF — was planned and then dropped.
Dense retrieval plus filtering plus a recall measurement already demonstrates the retrieval
work this system needs; fusion tuning is search-engine depth that this project is not
about, and the effort is better spent on the agent runtime. See
[ADR 0007](adr/0007-project-focus.md).

## Incident Lab

Two deliberately small Go services, `checkout-service` calling `payment-service`, each
exposing a business endpoint, `/health`, `/metrics`, and a `/fault` endpoint that injects
latency, 5xx errors or CPU load. Prometheus scrapes them for real, and they write
structured JSON logs to a directory that `read_service_logs` is allowed to read.

`payment-service` authorizes through a fixed-size pool of connections to a simulated card
processor, and that pool is the model: raising the processor's latency holds connections
longer, which fills the pool, which raises checkout's client latency, until checkout
abandons calls that succeeded on the other side. Every metric the corpus's runbooks name is
exported, so a retrieved runbook describes something that exists.

`cmd/lab-scenario` drives the lab into each of those failures and reports whether the
numbers moved, including a healthy control. See `docs/plans/incident-lab.md` and
`docs/plans/observability-and-fault-scenarios.md`.

This exists so the agent is evaluated against a system that actually breaks, rather than
against fixtures.

## Observability

OpenTelemetry instruments the HTTP API, Kafka produce and consume (trace context
propagated through headers), retrieval, embedding, LLM requests, MCP tool calls and the
agent run itself. A single agent run should appear as one coherent trace.

Prometheus metrics cover agent runs, steps, tool calls, LLM latency, retrieval latency and
Kafka processing latency, surfaced through two Grafana dashboards: agent run health and
pipeline health.

## Deliberate non-goals

No user accounts, authentication, RBAC, multi-tenancy or billing. No high availability —
MySQL, Kafka and Elasticsearch each run as a single node. No automated remediation. These
are scope decisions, recorded in [ADR 0005](adr/0005-scope-boundaries.md).
