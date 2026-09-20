# incident_diag

An agentic SRE diagnosis platform. You upload your internal runbooks, postmortems and
service documentation, file an incident, and a bounded AI agent investigates it: it
retrieves relevant internal knowledge, queries Prometheus, reads service logs, probes
health endpoints, and produces a diagnosis backed by the evidence it collected. You watch
it work in real time.

The agent is strictly read-only. It never runs arbitrary commands and never changes the
systems it investigates.

> **Status: under construction.** The sections below are filled in as the system is built.

## What it does

<!-- TODO(M33): user-facing feature walkthrough with screenshots -->

## Running it

<!-- TODO(M33): docker compose quickstart -->

Requires Go 1.26+, Docker with Compose v2+, and an NVIDIA GPU for the local embedding
server.

Copy `.env.example` to `.env` and fill in your LLM credentials.

## Architecture

<!-- TODO(M33): overview diagram; see docs/architecture.md for the detail -->

## Development

```sh
make check            # gofmt + go vet + go test + go test -race
make test-integration # also runs the tests that need the compose stack
```

Design documents live in `docs/plans/`, architecture decision records in `docs/adr/`.
