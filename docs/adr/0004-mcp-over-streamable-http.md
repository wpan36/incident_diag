# ADR 0004: Run ops-mcp as a service over streamable HTTP

**Status:** accepted
**Date:** 2026-09-20

## Context

The agent's operational tools — Prometheus queries, HTTP probes, log reads — are exposed
through the Model Context Protocol using the official Go SDK. MCP supports two transports:
stdio, where the client spawns the server as a child process, and streamable HTTP, where
the server is a long-running network service.

stdio is the more common MCP deployment, since most MCP servers are launched by a desktop
client on demand.

## Decision

Run `ops-mcp` as its own long-lived container in Docker Compose, speaking MCP over
streamable HTTP.

## Reasoning

The deployment model is Docker Compose, and a service is the natural unit there. With
stdio, the agent worker image would have to contain the `ops-mcp` binary too, and the
worker would have to manage subprocess lifecycle — spawn, health, restart, zombie reaping
— which is work the container runtime already does.

Running it as a service also makes the trust boundary visible rather than implied. The
tool server is a separate process with its own configuration and its own allowlists; it
can be probed, restarted and instrumented independently, and the fact that the agent can
only reach the operational world through it is legible from the Compose file alone.

It also lets multiple agent workers share one tool server, which matters if worker
concurrency is ever increased.

## Consequences

- `ops-mcp` needs its own health endpoint and its own OpenTelemetry setup.
- The transport is network-based, so calls need explicit timeouts on the client side —
  which they need anyway under this project's coding rules.
- The server is reachable by anything on the Compose network. It is read-only and
  allowlisted by design, but it is not authenticated, which is acceptable only because
  nothing here is exposed publicly.

## Alternatives considered

**stdio subprocess.** The canonical MCP setup and slightly simpler to secure, since
nothing listens on a port. Rejected on the packaging and lifecycle grounds above.

**Skip MCP and call the tools as in-process Go functions.** Simpler, but loses the point:
MCP is one of the technologies the project exists to demonstrate, and the process boundary
is what makes the agent's read-only constraint structural rather than a convention.
