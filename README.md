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

Requires Go 1.26+ and Docker with Compose v2+. No GPU: the chat and embedding models are
both reached over hosted OpenAI-compatible APIs.

Copy `.env.example` to `.env` and fill in your LLM credentials. The Makefile
loads that file and hands it to both docker compose and the Go binaries, so
nothing needs exporting by hand.

## Architecture

<!-- TODO(M33): overview diagram; see docs/architecture.md for the detail -->

## Development

```sh
make check            # gofmt + go vet + go test + go test -race
make up               # start the local infrastructure (MySQL today)
make migrate-up       # apply the schema
make test-integration # also runs the tests that need the compose stack (serially)
make down             # stop it again; make down-clean also drops the data
```

`make check` never needs infrastructure. The integration tests skip themselves unless
`TEST_MYSQL_DSN` is set, so they are opt-in rather than a hidden prerequisite; `.env`
supplies it. Run these through `make` — `.env` is not a shell script and sourcing it
fails on the unquoted DSN.

Design documents live in `docs/plans/`, architecture decision records in `docs/adr/`.
