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
- `prometheus.go` — `prometheus_query`: step resolution, the request, the result-type
  decoding and the rendering.
- `probe.go` — `http_probe` and `joinProbePath`.
- `logs.go` — `read_service_logs`: the filter, `scanLog`, `readLine`, `parseRecord`,
  `renderRecords`.
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
- **Prometheus's reply decides how it renders, not the request.** `up[5m]` is an instant
  query that returns a matrix, and reading its series as single values prints a `0` that is
  not in the data — from which the model concludes the service is down. Branch on
  `Data.ResultType`, never on whether a range was asked for.
- **Only 200, 400 and 422 are query API responses.** A 404 or a 401 means this server is
  not talking to Prometheus's query API, which the model cannot fix; reporting it as a
  rejected query would have it rewrite a good query until its budget ran out.
- **Limits are refused, never clamped.** Silently widening a step changes the numbers the
  agent reasons about without telling it; a refusal naming the limit costs one tool call.
- **An over-cap refusal names the metrics it matched.** A refusal that only states the rule
  is useless to an agent querying `{job="x"}` *because* it does not know the names — M32
  spent 35 of 240 tool calls in that loop. `overCapNote` answers with the distinct
  `__name__` values, which are already in the response, the way `unknownName` answers a
  wrong service with the valid ones.
- **The 8 KiB cap is applied once, in `ok`,** marker included — the cap is the size of what
  leaves the server, not of the part before the note saying it was cut. No tool applies it
  itself, so none can forget it and no two can disagree about where it falls.
- **`http_probe` says when it truncated the body.** A body that stops mid-stream with
  nothing saying so reads to the model like a service returning malformed output.
- **Relative times are not accepted anywhere.** One format means one parser and no ambiguity
  about whose clock "now" belongs to.
- **`parseRecord` rejects a line missing time, level or msg** rather than using it partly. A
  record with a zero timestamp would silently pass or fail the time filter, and a wrong
  answer about when something happened is worse than a counted line.
- **`scanLog` reads the whole file every call** and keeps only the tail. Fine for a lab run,
  wrong for anything left running for days. It also bounds itself with `LogTimeout`, since
  the scan rather than a dependency is what can run long here.
- **`readLine` is hand-rolled because a `bufio.Scanner` cannot continue past an over-long
  line.** One corrupt line would cost every record in the file, and with no rotation that
  service's log would stay unreadable for good. An over-long line is counted as
  unparseable, like any other line that could not be read.
- **The HTTP client refuses redirects.** A redirect is a result worth showing the agent, and
  following one leaves the allowlisted host after the host check has passed.
- **`Handler` builds the MCP server once**, not per request. The SDK documents that
  returning the same server from `getServer` is fine, one server serves any number of
  sessions, and the tools hold no per-session state.
- **`min_level` carries a schema `enum`, built from the same ordered level list as the
  refusal message**, so the two cannot come to disagree about which levels exist. The
  struct tag can carry a description but not an enum, so the schema is inferred and then
  amended in `logsInputSchema`.
