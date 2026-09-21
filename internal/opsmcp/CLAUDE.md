# internal/opsmcp

## Purpose

The read-only tool surface the agent reaches the operational world through. Everything the
agent can do is what this package exposes.

## Contents

- `server.go` — `Server`, `New`, `MCP` (tool registration and descriptions), `Handler`
  (`/mcp` and `/healthz`).
- `result.go` — `Meta`, the result kinds, and the four constructors every tool returns
  through: `ok`, `refused`, `unknownName`, `failed`.
- `timeargs.go` — `parseTime` and `parseWindow`, shared by two tools.
- `prometheus.go` — `prometheus_query`: step resolution, the request, and the rendering.
- `probe.go` — `http_probe` and `joinProbePath`.
- `logs.go` — `read_service_logs`: the filter, `scanLog`, `parseRecord`, `renderRecords`.
- `*_test.go` — unit tests. The end-to-end tests live in `internal/mcpclient`, which drives
  a real server rather than a fake.

## How it fits in

`cmd/ops-mcp` is the whole binary. It depends on `internal/config` and `internal/summary`
and nothing else in the project — deliberately: this process must not be able to reach
MySQL, Kafka or Elasticsearch, and the import graph is where that is enforced.

## Gotchas

- **No tool accepts a path or a URL.** Each takes a service name resolved by map lookup
  against configuration, so path traversal is not defended against — it cannot be
  expressed. Do not add a tool that takes one.
- **`http_probe` is the exception that proves it**, because it joins a path. `//evil.example`
  and `http://evil.example` are both valid URL references that `ResolveReference` sends to
  another host, so `joinProbePath` rejects three spellings *and* re-checks the joined host
  afterwards. The tests are the specification here.
- **A refusal is a normal result, not an MCP error.** An error response is
  indistinguishable from the server being broken, and the model would spend one of its
  bounded tool calls learning nothing. `isError` is set only for a dependency that did not
  answer.
- **`Meta.Refused` is an addition to the spec's envelope.** The spec couples `isError` to
  `Kind`, so a refusal has to be `kind: ok` — which would record a wrongly-argued tool call
  as a success. This field keeps that distinguishable for the agent evaluation.
- **A 5xx is a successful probe.** The tool did its job and the answer is that the service
  is failing, which is what the agent wanted to know. Only failing to get a response is an
  error.
- **Limits are refused, never clamped.** Silently widening a step changes the numbers the
  agent reasons about without telling it; a refusal naming the limit costs one tool call.
- **The 8 KiB cap is applied once, in `ok`.** No tool applies it itself, so none can forget
  it and no two can disagree about where it falls.
- **Relative times are not accepted anywhere.** One format means one parser and no ambiguity
  about whose clock "now" belongs to.
- **`parseRecord` rejects a line missing time, level or msg** rather than using it partly. A
  record with a zero timestamp would silently pass or fail the time filter, and a wrong
  answer about when something happened is worse than a counted line.
- **`scanLog` reads the whole file every call** and keeps only the tail. Fine for a lab run,
  wrong for anything left running for days.
- **The HTTP client refuses redirects.** A redirect is a result worth showing the agent, and
  following one leaves the allowlisted host after the host check has passed.
