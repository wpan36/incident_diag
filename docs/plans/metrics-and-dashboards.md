# Metrics and Dashboards

**Tier B spec (M30).**

## Problem

Prometheus scrapes the two lab services and nothing else. The platform that investigates them
is unmeasured, so "is the agent getting slower" and "how much does a run cost" have no
answer.

## Technical Plan

### `client_golang`, not OTel metrics

The lab services already export through `client_golang` and Prometheus already scrapes them.
Adding OTel metrics with a Prometheus exporter would put two metric systems in one repository
for the same output. Tracing is OTel (M29) because there is no second option there; metrics
are not in that position.

### The metrics

| | |
| --- | --- |
| `agent_runs_total{status,stop_reason}` | counter |
| `agent_run_duration_seconds` | histogram |
| `agent_steps_total{action_type,status}` | counter |
| `agent_tool_calls_total{tool,status}` | counter |
| `agent_tool_duration_seconds{tool}` | histogram |
| `llm_request_duration_seconds{outcome}` | histogram |
| `llm_tokens_total{kind}` | counter — cost is the question a budget invites |
| `rag_retrieval_duration_seconds` | histogram |
| `kafka_processing_duration_seconds{topic,outcome}` | histogram |
| `rag_embedding_duration_seconds` | histogram |
| `reconcile_enqueued_total{target,category}` | counter |
| `reconcile_produce_failures_total{target}` | counter |

The last three are not in the original list and were added because the Pipeline dashboard
below names embedding latency and reconciler activity, which nothing else measured.

`tool` is the tool name, which is a closed set from `ops-mcp`, so the label cannot explode.
A name the model invented is recorded as `__unknown__` rather than as itself: the model
chooses that string, and a label it controls is an unbounded one.
`status` on tool calls includes `REFUSED`, which is what makes "how often does the agent call
a tool wrongly" a query rather than a log search.

### The workers need a listener

`cmd/ingestion-worker` and `cmd/agent-worker` have no HTTP server. Each gets one that serves
`/metrics` and `/healthz` and nothing else, on `METRICS_ADDR`. Prometheus scrapes four targets
instead of two.

`/healthz` comes along because a worker with no liveness endpoint is a worker Compose cannot
restart on a hang, and the listener is there anyway.

### Two dashboards

**Agent runs** — run rate by outcome, run duration, steps and tool calls per run, tool
failures and refusals by tool, tokens per run.

**Pipeline** — ingestion throughput and failures, Kafka processing latency by topic,
retrieval latency, embedding latency, reconciler activity.

Provisioned as JSON under `deploy/grafana/provisioning`, the way the datasource already is,
so `make up-lab` brings them up rather than someone importing them by hand.

## Alternatives

**OTel metrics with a Prometheus exporter.** One instrumentation API for traces and metrics.
Rejected above.

**One dashboard.** Fewer panels to maintain, and it mixes two questions asked by two
different people at two different times.

**Scraping the workers through a pushgateway.** Avoids the listener, and it is the wrong tool:
these are long-lived processes, not batch jobs.

## Verification

- `make check` — metric registration and label cardinality are unit tested; a duplicate
  registration panics at startup rather than in production.
- Manual: `make up-lab`, run an investigation, and confirm both dashboards fill.

## Known limitations, accepted

**Histogram buckets are guesses**, like the S6 limits, and are the thing to revisit once M32
has real latency numbers.

**No alerting.** ADR 0005 already rules out Alertmanager; the dashboards are for looking at.

**Every binary exports every metric.** They are package-level variables on the default
registry, so importing `internal/obs` for tracing registers the lot — `ops-mcp` publishes
`agent_runs_total` as a permanent zero. Sums across targets stay correct and the alternative,
a registry threaded through every constructor, is plumbing this system does not need.
