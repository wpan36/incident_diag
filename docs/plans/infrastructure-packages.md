# Infrastructure Packages (M2)

**Tier B** — mechanical foundations, no cross-module contract. No goldfish test.

## Problem

Every binary in this project (`api`, `ingestion-worker`, `agent-worker`, `ops-mcp`) needs
the same four things before it can do anything useful: read its configuration and fail
loudly if it is wrong, emit structured logs that carry request and run identifiers, shut
down cleanly when the container stops, and classify errors consistently. Writing these
four times, slightly differently each time, is how a codebase becomes inconsistent early.

These packages have no consumers yet — the first is the API server in M4. They are built
now because M3's integration tests and M4's handlers both assume them, and because
building them together keeps their conventions aligned.

## Technical Plan

Four packages under `internal/`, each small, each with unit tests only. No external
dependencies.

### `internal/config`

Loads configuration from the process environment and nothing else. Docker Compose supplies
`.env` through `env_file`; local runs source it in the shell. The Go code never parses a
`.env` file, so behaviour is identical in every environment.

Validation is **collecting, not fail-on-first**: a run with three missing variables reports
all three, rather than making the operator fix them one at a time.

The `Config` struct holds only what is knowable today — `Env`, `LogLevel`, `HTTPAddr`. It
grows as milestones need it (database DSN in M3, Kafka brokers in M6, embedding endpoint
in M8). Declaring fields before anything reads them would be speculation.

### `internal/log`

Builds a `*slog.Logger` writing JSON. The interesting part is a handler wrapper that pulls
correlation identifiers out of `context.Context` and attaches them to every record, so
call sites do not have to remember to pass them. Two identifiers exist now: `request_id`
for an HTTP request and `run_id` for an agent run.

### `internal/shutdown`

Two pieces:

- `Context(parent)` returns a context cancelled on `SIGINT` or `SIGTERM`, plus a stop
  function that restores default signal handling.
- `Group` registers named close functions and runs them in **reverse registration order**
  under a deadline, so resources tear down in the opposite order they were created, the
  way `defer` does. A close function that hangs must not prevent the rest from running,
  and every failure is reported rather than the first one winning.

### `internal/httpx`

Classifies errors by kind and maps a kind to an HTTP status. Constructors (`Invalid`,
`NotFound`, `Conflict`, `Internal`, `Unavailable`) wrap an underlying error so `errors.Is`
and `errors.As` keep working through the layers.

**Out of scope, deliberately:** the JSON error response body. That is part of the API
contract and belongs to the `data-model-and-api-surface` spec (S1). M2 provides
`StatusFor(err) int` and the error kinds; S1 decides what goes on the wire.

> **Revised by S1.** `data-model-and-api-surface.md` adds `Fields map[string]string` to
> `httpx.Error` so a validation failure can report every offending field in one response.
> That field is part of the envelope this section deferred, so it is decided there, not
> here.

## Alternatives

**`github.com/joho/godotenv` for `.env` parsing.** Convenient for `go run`, but it adds a
dependency and lets local behaviour diverge from container behaviour. Sourcing the file in
the shell costs one line and keeps one code path.

**A configuration library (viper, envconfig, koanf).** These earn their place when there
are many sources — files, flags, remote config. There is one source here, and the
hand-written loader is about sixty lines with better error messages than a generic
reflection-based one would produce.

**Defining the error envelope in M2.** Rejected: a Tier B milestone should not fix a
cross-module contract that a Tier A spec is scheduled to decide.

**Naming the package `logging` to avoid shadowing the standard library's `log`.** Not
needed — nothing in this repository uses the standard `log` package, `slog` having
replaced it.

## Detailed Implementation

| File | Contents |
| --- | --- |
| `internal/config/config.go` | `Config` struct, `Load()` |
| `internal/config/env.go` | the collecting loader: required/optional string, int, bool, duration |
| `internal/log/log.go` | `New()`, context key helpers, the context-aware handler |
| `internal/shutdown/shutdown.go` | `Context()`, `Group` |
| `internal/httpx/error.go` | `Kind`, `Error`, constructors, `StatusFor()` |

Each file gets a `_test.go` beside it. Tests cover: multiple missing variables reported
together, malformed values, defaults applied; identifiers flowing from context into log
output; close functions running in reverse order, a hanging close function hitting the
deadline without blocking the others; error kinds surviving `%w` wrapping and mapping to
the right status.
