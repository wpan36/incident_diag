# incident_diag

An agentic SRE diagnosis platform. You upload your internal runbooks, postmortems and
service documentation, file an incident, and a bounded AI agent investigates it: it
retrieves relevant internal knowledge, queries Prometheus, reads service logs, probes
health endpoints, and produces a diagnosis backed by the evidence it collected. You watch
it work in real time.

The agent is strictly read-only. It never runs arbitrary commands and never changes the
systems it investigates.

## What it does

**You give it your knowledge.** Upload Markdown or text runbooks, postmortems and service
docs. They are chunked on their own structure, embedded, and indexed in Elasticsearch as
dense vectors. Ingestion runs through Kafka, so an upload answers immediately and a worker
that dies mid-document picks it up again.

**You file an incident and start a run.** One run per incident at a time, enforced by the
database rather than by hope.

**The agent investigates, within bounds.** Every step is one LLM call that picks one tool:
search the knowledge base, query Prometheus, read a service's logs, probe an HTTP endpoint,
or finish. It stops at whichever bound it reaches first — steps, tool calls, wall clock or
prompt size — and a run that stopped at a bound still produces a diagnosis.

**You watch it happen.** Each step is published to a Redis stream and pushed to the browser
over SSE. Reconnecting replays what you missed, because the stream id doubles as
`Last-Event-ID`.

**The diagnosis cites its evidence.** `root_cause`, the affected service, what to do next,
and a citation on each claim pointing at the step that produced it — the Prometheus query,
the log lines, the retrieved chunk. A citation that no longer resolves is dropped rather
than shown.

### Here is a real run

Against the Incident Lab below, with a latency fault injected into payment-service:

```
step 1   retrieve   search_knowledge    "payment latency runbook"
step 2   retrieve   search_knowledge    "connection pool saturation"
step 3   tool_call  prometheus_query    histogram_quantile(0.99, ... job="payment-service")
step 4   tool_call  prometheus_query    payment_pool_in_use / payment_pool_size
step 5   tool_call  prometheus_query    payment_processor_latency_seconds
...
step 13  finish
```

> **payment-service** — payment-service itself is slow: its p99 is ~4.96s, far above its
> documented 800ms budget. Its connection pool is nearly saturated as a *consequence* of
> that, not as the cause, so raising `PAYMENT_POOL_SIZE` would not help. checkout-service's
> own p99 is 0.099s, so checkout is not the source; the 504s are checkout abandoning calls
> payment-service could not answer in time.

The agent was told none of this. It was given the incident text, the corpus, and the tools.

### How good is it, actually

Measured, not claimed. [`docs/agent-eval.md`](docs/agent-eval.md) runs the agent against
every fault the lab can produce — several root causes behind one symptom, plus two cases
where the right answer is "nothing is wrong" — three times each, and reports the accuracy
with every run's full trajectory. [`docs/rag-eval.md`](docs/rag-eval.md) does the same for
retrieval.

## Running it

Requires Go 1.26+ and Docker with Compose v2+. No GPU: the chat and embedding models are
both reached over hosted OpenAI-compatible APIs.

```sh
cp .env.example .env     # then fill in LLM_API_KEY and EMBEDDING_API_KEY
make up-lab              # infrastructure + the Incident Lab + Prometheus, Grafana, Jaeger
make migrate-up          # apply the schema

# Three processes, in three terminals. The Makefile loads .env for them.
go run ./cmd/api              # :8080, and serves the page
go run ./cmd/ingestion-worker # :8085 for /metrics
go run ./cmd/agent-worker     # :8086 for /metrics

make demo                # upload the corpus, break the lab, file an incident, follow it
```

Then open <http://127.0.0.1:8080>. The demo prints the links for the traces
(<http://127.0.0.1:16686>) and the dashboards (<http://127.0.0.1:3000>).

`.env` is not a shell script — it holds an unquoted MySQL DSN with parentheses in it, which
bash would try to parse. Run things through `make`, which passes it to both Docker Compose
and the Go binaries.

### Breaking it on purpose

The Incident Lab is two small Go services — `checkout-service` calls `payment-service`,
which has a connection pool in front of a simulated card processor — that export
Prometheus metrics and write logs, and break on demand. Their metric names, routes and
alert thresholds come from the knowledge corpus rather than from this repository's
conventions, because the point is to give the agent someone else's estate to diagnose.

```sh
make scenario                            # list the scenarios
make scenario SCENARIO=payment-latency   # break it and report what moved
```

Seven faults and a healthy control. Several of them present identically from the outside —
"checkout is returning 504" — and differ only in why, which is what makes the evaluation
mean anything.

## Architecture

```
    Browser  ──REST──►  api  ──produce──►  Kafka  ──►  ingestion-worker  ──►  Elasticsearch
       ▲                 │                   │                                (dense vectors)
       └────SSE──────────┘                   │
                         │                   ▼
                       MySQL  ◄────────  agent-worker  ──►  chat LLM
                   (system of record)        │
                         ▲                   ├──►  ops-mcp  ──►  Prometheus / logs / probes
                         │                   │     (MCP over HTTP: the whole trust boundary)
                    Redis Streams  ◄─────────┘
                     (run events)
```

Five decisions worth knowing about:

- **MySQL is the system of record.** Kafka carries "this document needs work", not the
  work. Every state transition is a conditional `UPDATE`, which is what makes the consumers
  idempotent under at-least-once delivery ([ADR 0003](docs/adr/0003-at-least-once-delivery.md)).
- **A reconciler sweeps stuck rows.** An upload writes a row and produces a message, and
  those cannot be one transaction; the reconciler is what makes the gap survivable instead
  of silent.
- **The agent's reach is one process.** Everything it can do to the operational world is
  what `ops-mcp` exposes, which is legible from the Compose file alone. No tool takes a path
  or a URL, so path traversal cannot be expressed rather than being defended against.
- **Native tool calling, no parser.** The provider enforces the tool schemas, so there is no
  tolerant JSON parser for model output anywhere in this repository.
- **One run, one trace.** OpenTelemetry follows an investigation from the API request
  through the Kafka hop into the agent's steps, its LLM calls and ops-mcp.

[`docs/architecture.md`](docs/architecture.md) has the detail;
[`docs/adr/`](docs/adr/) has the decisions and what they cost.

## Development

```sh
make check            # gofmt + go vet + go test + go test -race
make up               # just the infrastructure (MySQL, Kafka, Elasticsearch, Redis)
make test-integration # the tests that need it, including one real investigation
make agent-eval       # the full agent evaluation; every run is billed
make down             # stop; make down-clean also drops the data
```

`make check` never needs infrastructure. The integration tests skip themselves unless
`TEST_MYSQL_DSN`, `TEST_KAFKA_BROKERS`, `TEST_ELASTICSEARCH_URL` and `TEST_REDIS_URL` are
set — `.env` supplies them, and `make test-integration` refuses to run without them rather
than reporting a green pass in which everything skipped. The end-to-end test and the
evaluation additionally need `TEST_LLM_API_KEY`, and both need the two workers *stopped*:
they join those consumer groups themselves.

How this project is built — the skills, the rules, and which milestones get a full design
pass — is in [`docs/workflow.md`](docs/workflow.md). Progress is in
[`docs/roadmap.md`](docs/roadmap.md).
