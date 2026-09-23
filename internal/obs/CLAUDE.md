# internal/obs

## Purpose

Observability, shared by all four binaries: tracing setup, the metric catalogue, and the
listener the two workers serve `/metrics` on.

## Contents

- `obs.go` — `Setup`, `Tracer`, `Version`, `tracesURL`, and the `Attr*` keys used in more
  than one package.
- `metrics.go` — every metric this project exports, the shared outcome labels, and
  `NewMetricsServer` for the two workers.
- `config.go` — `Config`, filled by `config.LoadTracing`.
- `http.go` — `Transport` (outgoing) and `Handler` (incoming, for the one server that has
  no router).
- `obs_test.go` — the disabled path, the propagator, a malformed endpoint, the exporter's
  URL, and an export against a stand-in collector.
- `metrics_test.go` — every metric reaches /metrics, and the tool label stays closed.

## How it fits in

`cmd/*` call `Setup` first and register its shutdown first, so it flushes last. The spec is
`docs/plans/tracing.md`.

Spans come from four places: `otelgin` in `internal/api`, `obs.Handler` in `cmd/ops-mcp`,
`kotel` in `internal/mq`, and hand-written spans in `internal/agent` (the run and each
step), `internal/llm`, `internal/mcpclient`, `internal/embed` and `internal/search`.

## Gotchas

**The propagator is installed even when tracing is disabled.** Otherwise a process with no
endpoint would strip the trace context from the messages it forwards, breaking the traces
of the processes that do have it on.

**`Tracer` resolves through the global provider on every span**, so a package can hold one
in a package-level variable without caring whether `Setup` has run. But the global provider
*delegates only once*: a tracer taken before the first `otel.SetTracerProvider` keeps
pointing at whatever that first call installed. In a test binary, only one test may install
a provider — see `internal/agent/trace_test.go`.

**Metrics are on the default registry**, so every binary that imports this package for
tracing also exports all of them — `ops-mcp` publishes `agent_runs_total` as a permanent
zero. Sums across targets are still right, and the alternative is a `prometheus.Registerer`
threaded through every constructor.

**`OTEL_EXPORTER_OTLP_ENDPOINT` is a base URL**, not the traces URL. `tracesURL` appends
`/v1/traces`; without it every export 404s and nothing says so.

**Nothing here reads the environment.** `internal/config` owns that, so this package has no
opinion about variable names and `internal/config` keeps its rule of importing nothing from
this project.
