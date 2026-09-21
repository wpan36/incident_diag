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
to be correct; a tool that cannot express a path has nothing to get wrong. The same applies
to `http_probe`: with no URL to parse there is no way to reach a host the allowlist does
not name.

The cost is that adding a target means editing configuration and restarting `ops-mcp`. For
this system that is the intended behaviour, not a limitation.

**This supersedes the description in `docs/architecture.md` and the M19 row in
`docs/roadmap.md`**, both of which describe directory jailing with symlink and `..`
rejection. Both are updated alongside this spec.

### Unknown names return a listing, not an error

A call naming an unconfigured service returns a tool result naming the valid ones. The
model corrects itself on the next step instead of spending one of its bounded tool calls on
a dead end. A protocol-level error would be indistinguishable from the server being broken.

### The log format

The lab services use `internal/log`, so the format is defined by code that already exists
and is tested, and every process in the system emits the same shape.

The schema is written down here regardless, because M19 parses it and "whatever that Go
package emits" is not something a future non-Go emitter could satisfy:

| Field | |
| --- | --- |
| `time` | RFC 3339 with offset |
| `level` | `DEBUG` \| `INFO` \| `WARN` \| `ERROR` |
| `msg` | |
| `service` | constant per process |
| *(other)* | free-form attributes, preserved but not interpreted |

One append-only file per service under a configured root, named for the service. No
rotation: this is a lab, and rotation would add a file-discovery problem the "no paths"
decision exists to avoid.

`read_service_logs` parses line by line and filters on time window, minimum level and
substring. Time-window filtering is the capability the agent most needs — "the five minutes
around when the alert fired" — and the one thing a text grep cannot do. Unparseable lines
are counted and the count is reported in the result, never silently dropped.

### Resource limits

Prometheus is read-only, so the risk is an expensive query rather than an unsafe one. The
server forces a timeout, a maximum range, a minimum step and a maximum series count.
`http_probe` forces a timeout and truncates the body. Every tool result is capped at 8 KiB
per S1, with the pre-truncation size reported.

**The specific limits are guesses** and are revisited once real tool output exists.

### Transport and deployment

Streamable HTTP, own container, no authentication — ADR 0004, unchanged. `ops-mcp` exposes
its own health endpoint.

### Scope boundary with S7

M20 bridges MCP tools onto the agent's tool interface, which S7 defines. This spec stops at
the MCP-side contract — name, arguments, result, error — which maps onto the `tool_calls`
columns S1 already fixed.

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

## Detailed Implementation

**M16** — `cmd/ops-mcp`: official MCP Go SDK over streamable HTTP, tool registry, health
endpoint, and the configuration holding the Prometheus URL, the probe targets and the log
sources. Unknown-name handling and the 8 KiB result cap live here, once, rather than in
each tool.

**M17** — `prometheus_query`: instant when no range is given, range otherwise. Results are
summarized for a model rather than passed through as raw JSON.

**M18** — `http_probe`: resolves `service` against the target map, forces a timeout,
returns status, latency and a truncated body.

**M19** — `read_service_logs`: resolves `service` to a file, parses and filters, reports
the unparseable count.

**M20** — the MCP client and tool adapter in the main application, unit tested against a
fake MCP server.

## Verification

- `make check` — argument validation, level and time parsing, the unparseable-line count,
  truncation. All pure.
- `make test-integration` — each tool against real Prometheus, a real HTTP endpoint and a
  real log file; an unknown name returns a listing rather than an error.
- Manual: `ops-mcp` answers `tools/list` and one call of each tool.

## Known limitations, accepted

**The resource limits are unmeasured**, the same category as the CJK token coefficient in
S3.

**Structured parsing can drop what a grep would have shown.** Counting unparseable lines
mitigates this; it does not remove it.

**A second log stream per service needs a configuration entry**, not a parameter. That is
the cost of the no-paths decision.
