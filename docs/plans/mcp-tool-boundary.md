# MCP Tool Boundary

**Tier A spec (S6).** Gates M16–M20. Written one phase early because it defines the log
format M13 emits and M19 reads.

## Problem

`ops-mcp` is where the agent touches the operational world. What the agent cannot do is
decided entirely by what this server exposes, so the tool signatures are the security
boundary rather than just an API.

The log format lands here for the same reason: M13's lab services emit it and M19's tool
parses it, and a contract decided in one of those milestones would be retrofitted onto the
other.

## Technical Plan

### No tool accepts a path or a URL

Each tool takes a name that the server resolves against its own configuration.

| Tool | Parameters |
| --- | --- |
| `prometheus_query` | `query`, `start?`, `end?`, `step?`; endpoint from config |
| `http_probe` | `service`, `path?` |
| `read_service_logs` | `service`, `since?`, `until?`, `min_level?`, `contains?`, `limit?` |

This replaces path jailing rather than implementing it carefully. A jail is code that has
to be correct; a tool that cannot express a path has nothing to get wrong.

The cost is that adding a target means editing configuration and restarting `ops-mcp`. For
this system that is the intended behaviour, not a limitation.

**This supersedes the description in `docs/architecture.md` and the M19 row in
`docs/roadmap.md`**, both of which describe directory jailing with symlink and `..`
rejection. Both are updated alongside this spec.

Service names are resolved by **map lookup**, never by joining caller input into a path or
a URL. A name that is not a key is an unknown name, so there is no string for a traversal
to hide in.

### Parameter formats

| Parameter | Format | Default |
| --- | --- | --- |
| `query` | PromQL, passed through | required |
| `start`, `end`, `since`, `until` | RFC 3339, UTC | see below |
| `step` | Go duration (`30s`, `1m`) | range ÷ 100, rounded up to the minimum step |
| `path` | absolute path, constrained below | `/` |
| `min_level` | `DEBUG` \| `INFO` \| `WARN` \| `ERROR`, advertised as a schema `enum` | `INFO` |
| `contains` | case-insensitive substring of the whole raw line | no filter |
| `limit` | integer | 200, maximum 1000 |

Relative times (`5m`, `now-1h`) are **not** accepted. The agent has the incident's
timestamps; one format means one parser and no ambiguity about which clock "now" is.

`prometheus_query` runs an instant query when neither `start` nor `end` is given and a
range query when both are. Exactly one of them is a rejected argument, not a guess.

`read_service_logs` returns the **last** `limit` matching records — a tail is what
debugging wants — rendered oldest first so the result reads as a timeline.

Its window is **`[since, until)`**: a record at exactly `since` is included and one at
exactly `until` is not, so adjacent windows tile without a record appearing in both. The
tool's description says so, because a half-open interval the caller has to guess at is a
silently wrong answer rather than a refusal.

`min_level` is advertised as a JSON Schema `enum` rather than only described in prose. A
description is advice; an enum is a constraint the provider can enforce before the call is
made, and spending one of the agent's bounded tool calls being told the value was wrong is
avoidable.

### `http_probe` joins a path, so the join is constrained

The "no URL" guarantee does not survive a naive join: with a base of
`http://payment-service:8080/`, Go's `ResolveReference` sends `//evil.example/x` and
`http://evil.example/x` straight to another host, because both are valid URL references.

`path` must therefore begin with exactly one `/`, must not begin with `//`, and must not
contain `://`. After the join, the resulting `Host` is compared against the configured
target's host and a mismatch is refused. The check is cheap and it is the one piece of
parsing this design could not remove.

The probe issues `GET` and does not follow redirects: a redirect is a result worth showing
the agent, and following one is another way to leave the allowlisted host.

### Unknown names return a listing, not an error

A call naming an unconfigured service returns a tool result naming the valid ones. The
model corrects itself on the next step instead of spending one of its bounded tool calls on
a dead end. A protocol-level error would be indistinguishable from the server being broken.

The same applies to every other argument the model can fix by itself — a malformed
timestamp, a `min_level` outside the enum, a `path` breaking the rules above, a PromQL
syntax error, a range or step outside the limits. Each returns a normal result naming the
rule that was violated and the limit that applies.

### Every result carries the same envelope

Each tool returns one **text block** — what the model reads — plus **structured content**
holding a `meta` object. M20 maps `meta` onto the `tool_calls` columns S1 fixed, so the
audit trail does not depend on parsing prose.

| `meta` field | |
| --- | --- |
| `kind` | `ok` \| `error` \| `timeout` → `tool_calls.status` |
| `original_bytes` | UTF-8 size of the text block before the cap → `result_bytes` |
| `truncated` | bool → `truncated` |
| `note` | optional one-line reason for a non-`ok` kind, or for a refused argument |

Plus per-tool counters: `series` and `points` for `prometheus_query`; `matched`,
`returned` and `unparseable_lines` for `read_service_logs`.

The text block is rendered per tool as:

- **`prometheus_query`** — one line per series, rendered by the `resultType` in the reply
  rather than by the shape of the request, since `up[5m]` is an instant query that returns
  a matrix. Vector: the label set and the value. Matrix: the label set, point count, min,
  max, mean and last, with the timestamps of the min and the max, then up to 20 evenly
  spaced points. Scalar and string: the single value. Raw JSON is never passed through.
- **`http_probe`** — status code, latency in milliseconds, `Content-Type`, and the body
  truncated to 2 KiB, with the truncation stated in the text. A 5xx is a **successful
  probe** (`kind: ok`); only a failure to get a response is not.
- **`read_service_logs`** — one line per record as `time level msg key=value …`, which is
  cheaper in tokens than re-emitting JSON.

The 8 KiB cap from S1 is applied once, in the server, to the rendered text — marker
included, so the cap is the size of what leaves the server. `ops-mcp` does not import
`internal/store`, so the rune-safe truncation helper S1 describes lives in a small shared
package both can use — one implementation, not two that must agree.

### Errors the model cannot fix

A dependency that does not answer — a probe connection refused, a missing or unreadable log
file, Prometheus unreachable — returns an MCP `isError` result with `meta.kind` set to
`error`, or `timeout` when `ops-mcp`'s own deadline expired. These are the only cases where
`isError` is set; everything the model could correct is an ordinary result, per the section
above.

Prometheus is classified by status code: `200`, `400` and `422` carry a query API response,
so a rejected query is refused and the model can rewrite it. Any other status means this
server is not talking to the query API at all — a wrong URL, an auth proxy — which is an
error, because reporting it as a bad query would have the model rewrite a good one until
its budget ran out.

### Resource limits

Prometheus is read-only, so the risk is an expensive query rather than an unsafe one.

| Limit | Guess |
| --- | --- |
| Prometheus query timeout | 10s |
| Maximum range | 6h |
| Minimum step | 15s |
| Maximum series | 50 |
| Probe timeout | 5s |
| Probe body | 2 KiB |
| Log read timeout | 10s |
| Result cap | 8 KiB (S1) |

Violations are **refused, not clamped**. Silently widening a step changes the numbers the
agent reasons about without telling it; a refusal naming the limit costs one tool call and
leaves the model able to ask again correctly. The series cap is enforced on the response,
since PromQL cannot express it.

**The specific limits are guesses** and are revisited once real tool output exists.

#### The series cap answers with the metric names it matched

M32 is the real tool output this said to wait for, and it falsified the claim above for
one case: 35 of 240 tool calls were refused, almost all of them queries like
`{job="payment-service"}` or `count by (__name__) (...)` — the agent asking what metrics
exist. "Narrow it with a label matcher or an aggregation" is advice it could not take,
because those already are aggregations and it did not know the names to narrow to. One run
spent three consecutive tool calls in that loop and then ran out of budget before reaching
the metric that would have diagnosed the incident.

So an over-cap refusal now carries the distinct `__name__` values the query matched, up to
eighty. That is the same move `unknownName` already makes for a wrong service — answer with
the valid set rather than with a rule — and it costs nothing, because the labels are in the
response that has already arrived.

A fourth tool over `/api/v1/label/__name__/values` was considered. It would not violate the
"no paths, no URLs" boundary, but it adds to a surface deliberately kept at three when an
existing refusal can carry the same answer.

### The log format

Every process in the system logs through `internal/log`, so one package defines the shape.
The schema is written down here regardless, because M19 parses it and "whatever that Go
package emits" is not something a future non-Go emitter could satisfy:

| Field | |
| --- | --- |
| `time` | RFC 3339, **UTC**, millisecond precision — `2026-09-21T10:04:05.123Z` |
| `level` | `DEBUG` \| `INFO` \| `WARN` \| `ERROR` |
| `msg` | |
| `service` | constant per process |
| *(other)* | free-form attributes (`request_id`, `run_id`, …), preserved but not interpreted |

Two pieces of this do not exist yet and are part of M13:

- **`internal/log.New` gains a `service` parameter.** Setting it with `With` at each
  binary's startup would work until someone forgets; the package already argues this way
  about correlation ids. Two call sites change.
- **Timestamps are forced to UTC** through the handler. Containers do not agree on a time
  zone, and a time-window filter across services that disagree is silently wrong.

One append-only file per service, `<service>.log` under a configured root. No rotation:
this is a lab, and rotation would add a file-discovery problem the "no paths" decision
exists to avoid. The lab services write to `io.MultiWriter(os.Stdout, file)` so
`docker compose logs` still shows them.

`read_service_logs` parses line by line and filters on time window, minimum level and
substring. Time-window filtering is the capability the agent most needs — "the five minutes
around when the alert fired" — and the one thing a text grep cannot do. Unparseable lines
are counted and the count is reported in the result, never silently dropped — including a
line over the length bound, which is skipped rather than ending the read.

### Configuration

Environment only, per `internal/config`: a new `LoadOpsMCP` beside the existing loaders,
using the same `splitList` as `LoadKafka` rather than introducing the repository's first
config file.

```
OPS_MCP_PROMETHEUS_URL=http://prometheus:9090
OPS_MCP_PROBE_TARGETS=checkout-service=http://checkout-service:8080,payment-service=http://payment-service:8080
OPS_MCP_LOG_ROOT=/var/log/lab
OPS_MCP_LOG_SERVICES=checkout-service,payment-service
OPS_MCP_LOG_TIMEOUT=10s
```

Limits are optional variables over the defaults tabled above. The log services are listed
explicitly rather than discovered by scanning the root, which is the same file-discovery
problem under another name.

This is the weakest part of the design: a `KEY=VALUE` list in an environment variable is
readable at two entries and unreadable at twenty. A config file wins as soon as the target
list stops being "the Incident Lab".

### Transport and deployment

Streamable HTTP, own container, no authentication — ADR 0004, unchanged. One listener on
`HTTP_ADDR`: MCP at `/mcp`, health at `/healthz`, matching `cmd/api`. The dependency is
`github.com/modelcontextprotocol/go-sdk`, pinned when M16 adds it.

### Scope boundary with S7

M20 bridges MCP tools onto the agent's tool interface, which S7 defines. This spec stops at
the MCP-side contract — name, arguments, result, error — which maps onto the `tool_calls`
columns S1 already fixed, through the `meta` object above.

## Alternatives

**Path parameters with jail validation**, as `docs/architecture.md` currently describes.
More flexible, and the jail is a well-understood piece of work. Rejected because it is a
piece of work that has to be correct, and removing the parameter removes the requirement.

**Predefined PromQL templates.** Fully controlled and predictable. Rejected: it limits the
agent to the questions we thought of in advance, which is the part of the system this
project exists to exercise.

**Raw log lines with substring matching only.** Simplest, and nothing is lost to a parse
failure. Rejected: no time-window filtering, and raw JSON is expensive in tokens.

**Rotation with a file-discovery parameter.** Realistic for production, and it reintroduces
exactly the path handling this design removes.

**A `service`-only `http_probe` with no `path`.** Would make the "no URL to parse" claim
literally true. Rejected: probing only `/health` cannot distinguish a service that is up
from one whose business endpoint is failing, which is most of what the agent is for.

**Clamping over-limit queries instead of refusing them.** Saves a tool call from the
bounded budget. Rejected: it changes the answer without saying so, and a wrong conclusion
costs more than a retry.

## Detailed Implementation

**M16** — `cmd/ops-mcp`: MCP Go SDK over streamable HTTP, tool registry, health endpoint,
`LoadOpsMCP`. Unknown-name handling, argument validation, the `meta` envelope and the 8 KiB
cap live here, once, rather than in each tool.

**M17** — `prometheus_query`: instant when no range is given, range otherwise; the
summarization above; the range, step and series limits.

**M18** — `http_probe`: resolves `service` against the target map, validates `path` and
re-checks the joined host, forces a timeout, returns status, latency and a truncated body.

**M19** — `read_service_logs`: resolves `service` to a file, parses and filters, tails to
`limit`, reports the unparseable count.

**M20** — the MCP client and tool adapter in the main application, mapping `meta` onto
`tool_calls`; unit tested against a fake MCP server.

## Verification

- `make check` — argument validation, level and time parsing, the unparseable-line count,
  the tail-to-`limit` behaviour, truncation. All pure. The `path` cases (`//evil.example`,
  `http://evil.example`, `../..`, missing leading slash) are explicit tests, and so is the
  post-join host equality check.
- `make test-integration` — each tool against real Prometheus, a real HTTP endpoint and a
  real log file; an unknown name returns a listing rather than an error; a stopped
  dependency returns `isError` with `meta.kind` set.
- Manual: `ops-mcp` answers `tools/list` and one call of each tool.

## Known limitations, accepted

**The resource limits are unmeasured**, the same category as the CJK token coefficient in
S3.

**Structured parsing can drop what a grep would have shown.** Counting unparseable lines
mitigates this; it does not remove it.

**A second log stream per service needs a configuration entry**, not a parameter. That is
the cost of the no-paths decision.

**Log files grow without bound and every call scans from the start.** Fine for a lab run,
wrong for anything left running for days.

**The agent cannot read the platform's own logs** — only the services listed in
`OPS_MCP_LOG_SERVICES`. Debugging `api` or `ingestion-worker` stays a human's job.
