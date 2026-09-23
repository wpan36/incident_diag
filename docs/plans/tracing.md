# Tracing

**Tier B spec (M29).**

## Problem

Four processes take part in one investigation, and the only thing tying them together today
is a `run_id` in the logs. A trace shows the shape — where the time went, which tool call was
slow, whether the LLM or Elasticsearch was the wait — which logs cannot.

## Technical Plan

### One shared setup, off by default

`internal/obs` exposes `Setup(ctx, service string) (shutdown func(context.Context) error,
err error)`. Each binary calls it once and registers the shutdown with its `shutdown.Group`.

**Tracing is a no-op unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set.** Every test, every
`go run`, and CI must work without Jaeger running, and an exporter that retries against a
closed port would be a worse failure than no tracing at all.

### Where the spans come from

| Boundary | How |
| --- | --- |
| HTTP server | `otelgin` middleware in `internal/api`; `obs.Handler` in `cmd/ops-mcp`, which serves one handler and has no router |
| HTTP client | `obs.Transport` (`otelhttp`) on every outgoing client: embedding, LLM, Prometheus, probe and MCP |
| Kafka | `kotel`, franz-go's own plugin, which writes and reads the trace context in record headers |
| LLM, MCP tool, retrieval, embedding, agent step | spans created by hand where the call is made |

The cross-process work is three libraries and no hand-written propagation. That matters more
than the span count: a trace that breaks at a process boundary is not a trace.

Instrumenting the shared plumbing once covers both pipelines — ingestion and investigation —
rather than one at a time.

### The agent run is the root

`agent.Run` opens a span that the whole investigation hangs from, so "one run, one trace" is
literally true. The span carries the run id, the incident id, the model and the stop reason;
each step is a child, and each tool call a child of its step.

The producing side of `agent.runs.v1` is where the trace starts, so the API request that
created the run and the worker that executed it are one trace, not two.

### Deployment

`jaegertracing/all-in-one` in Compose: OTLP in on 4318, UI on 16686, memory storage. No
separate collector — the binaries export straight to it. Traces do not survive a restart,
which is correct for a lab.

### Configuration

`OTEL_EXPORTER_OTLP_ENDPOINT` (unset disables tracing), `OTEL_SERVICE_NAME` defaulted per
binary, `OTEL_TRACES_SAMPLER` left at the SDK default (always-on: a lab run is a handful of
traces and sampling would hide the one being looked at).

## Alternatives

**A stdout exporter and no container.** No new infrastructure, and nothing anyone can read: a
trace across four processes is tens of JSON objects in a log, and being able to *see* the
shape is the whole value.

**Grafana Tempo.** Traces and dashboards in one UI. Rejected: a configuration file and a
storage volume for no advantage a single-operator lab can use.

**Hand-written propagation instead of `kotel` and `otelgin`.** Fewer dependencies, and the
one place where a subtle bug silently splits every trace in two.

## Verification

- `make check` — `Setup` with no endpoint returns a working no-op, and the binaries start
  without Jaeger.
- Manual: `make up-lab`, run an investigation, open Jaeger, and confirm one trace spans the
  API request, the Kafka hop, the agent's steps and its LLM and MCP calls.

## Known limitations, accepted

**No sampling.** Fine at a handful of runs, wrong at any volume.

**Memory storage.** Traces are gone after a restart.

**The lab services are not instrumented.** They are the system under investigation, not part
of the platform, and tracing them would blur that line.
