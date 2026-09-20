# Data Model and API Surface

**Tier A spec (S1).** Gates M3 (MySQL and store), M4 (API skeleton), M5 (document upload).

## Problem

Everything built after M5 depends on these tables and this API. The Kafka consumers get
their idempotency from the state machines defined here (ADR 0003), the SSE endpoint depends
on the run lifecycle, the front end depends on the response shapes, and retrieval
evaluation depends on evidence being queryable. Changing any of it later means a migration
plus a coordinated change across several milestones.

M2 deliberately stopped `internal/httpx` at error *kinds* and left the JSON error body
undecided, on the grounds that a Tier B milestone should not fix a cross-module contract.
This spec settles it.

## Technical Plan

### Identifiers

ULIDs as `CHAR(26)`, generated in the application with `github.com/oklog/ulid/v2`. They
sort by creation time, which makes them usable as a pagination cursor, and they are
readable in logs and URLs without decoding. The application knows the ID before the
`INSERT`, which matters once a row and a Kafka message have to be produced together.

### Schema

MySQL 8, InnoDB, `utf8mb4_0900_ai_ci`.

All timestamps are `DATETIME(6)` holding UTC, supplied by the application. `TIMESTAMP`
columns convert according to the server's time zone, which would make test results depend
on where they run.

Status columns are `VARCHAR`, not `ENUM`. Adding a value to an `ENUM` is a DDL change, and
the transition rules have to live in Go regardless, so the schema gains nothing by
duplicating the value set.

#### `documents`

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `CHAR(26)` | primary key |
| `filename` | `VARCHAR(255)` | as uploaded |
| `storage_path` | `VARCHAR(512)` | relative to the configured storage root |
| `format` | `VARCHAR(16)` | `markdown` \| `text` |
| `size_bytes` | `BIGINT` | |
| `content_sha256` | `CHAR(64)` | recorded, not used for deduplication |
| `service` | `VARCHAR(64)` NULL | retrieval filter |
| `document_type` | `VARCHAR(32)` | `runbook` \| `postmortem` \| `service_doc` |
| `status` | `VARCHAR(16)` | `PENDING` \| `PROCESSING` \| `READY` \| `FAILED` |
| `failure_reason` | `TEXT` NULL | set on `FAILED` |
| `chunk_count` | `INT` | set on `READY` |
| `attempts` | `INT` | incremented on each claim |
| `processing_started_at` | `DATETIME(6)` NULL | for lease reclaim |
| `created_at`, `updated_at` | `DATETIME(6)` | |

Indexes: `(status, id)` for the worker, `(service, document_type)` for filtered listing.

Storing the path relative to a configured root keeps the database independent of any host
layout, so the same rows work in Compose and on a developer's machine.

The content hash is recorded but not used to deduplicate. The same file uploaded with
different service metadata is genuinely ambiguous — one document or two? — and the answer
is not worth the complexity.

#### `incidents`

`id` `CHAR(26)` PK, `title` `VARCHAR(255)`, `description` `TEXT`, `service` `VARCHAR(64)`
NULL, `created_at`, `updated_at`.

No status column. Incidents here exist to give a run something to investigate; `OPEN` and
`CLOSED` would model a workflow this product does not have.

#### `agent_runs`

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `CHAR(26)` | primary key |
| `incident_id` | `CHAR(26)` | FK to `incidents` |
| `status` | `VARCHAR(16)` | `PENDING` \| `RUNNING` \| `SUCCEEDED` \| `FAILED` |
| `stop_reason` | `VARCHAR(24)` NULL | see below |
| `active_incident_id` | `CHAR(26)` | generated, see below |
| `max_steps`, `max_tool_calls`, `max_duration_seconds`, `max_prompt_tokens` | `INT` | the budget that actually applied |
| `model` | `VARCHAR(128)` | |
| `step_count`, `tool_call_count`, `prompt_tokens`, `completion_tokens` | `INT` | |
| `final_result` | `JSON` NULL | |
| `error` | `TEXT` NULL | |
| `attempts` | `INT` | |
| `started_at`, `finished_at` | `DATETIME(6)` NULL | |
| `created_at`, `updated_at` | `DATETIME(6)` | |

Indexes: `(incident_id, id)`, `(status, id)`.

**Lifecycle and outcome are separate columns.** `status` is the coarse machine.
`stop_reason` says why the run ended: `COMPLETED`, `MAX_STEPS`, `MAX_TOOL_CALLS`,
`TIMEOUT`, `TOKEN_BUDGET`, `ERROR`, `CANCELLED`.

A run stopped by a bound is `SUCCEEDED` with a `stop_reason`, because hitting a bound
produces a partial diagnosis and is a normal, tested outcome. Collapsing the two into one
enum would force a choice between calling a useful partial result a failure and losing the
fact that it was truncated.

Recording the budget that applied, rather than relying on the configuration file, keeps a
run interpretable after the configuration changes.

**One active run per incident is enforced by the database**, not by a read-then-write check
that two requests could interleave. MySQL has no partial unique index, so a stored
generated column holds the incident ID while the run is in flight and `NULL` once it is
terminal. `NULL`s do not collide under a `UNIQUE` index, so finished runs stop competing:

```sql
active_incident_id CHAR(26) GENERATED ALWAYS AS (
  CASE WHEN status IN ('PENDING','RUNNING') THEN incident_id END
) STORED,
UNIQUE KEY uniq_active_run (active_incident_id)
```

#### `agent_steps`

`id` PK, `run_id` FK, `step_number` `INT` (unique per run), `action_type` (`retrieve` |
`tool_call` | `finish`), `action` `JSON`, `observation_summary` `MEDIUMTEXT` NULL,
`observation_bytes` `INT`, `truncated` `BOOL`, `status` (`OK` | `ERROR`), `error` `TEXT`
NULL, `duration_ms` `INT`, `created_at`.

`action` holds the structured decision the model returned, never the reasoning behind it.
No chain-of-thought is persisted anywhere in this schema.

#### `tool_calls`

`id` PK, `run_id` FK, `step_id` FK, `tool_name` `VARCHAR(64)`, `arguments` `JSON`, `status`
(`OK` | `ERROR` | `TIMEOUT`), `result_summary` `MEDIUMTEXT` NULL, `result_bytes` `INT`,
`truncated` `BOOL`, `error` `TEXT` NULL, `duration_ms` `INT`, `created_at`.

Index: `(run_id, id)`.

#### `evidence`

`id` PK, `run_id` FK, `step_id` FK, `tool_call_id` FK NULL, `source_type` (`retrieval` |
`tool`), `source_ref` `VARCHAR(255)`, `document_id` FK NULL (set when `source_type` is
`retrieval`), `summary` `TEXT`, `note` `TEXT` NULL (why the agent kept it), `created_at`.

Indexes: `(run_id, id)`, `(document_id)`.

A table rather than JSON because "which documents actually get cited across runs" is then a
`GROUP BY`. That query is direct feedback on retrieval quality, which M12 and M32 exist to
measure.

#### Truncation

`observation_summary` and `result_summary` are capped at **8 KiB**, with the pre-truncation
length in the matching `*_bytes` column and a `truncated` flag. A Prometheus range query
can return megabytes, and an audit trail nobody can read is not an audit trail.

### State machines

Both are enforced in the store layer as conditional updates. Enforcing them in SQL rather
than in Go is precisely what makes the Kafka consumers idempotent under at-least-once
delivery (ADR 0003).

**Documents:** `PENDING → PROCESSING → READY | FAILED`, plus `FAILED → PROCESSING` when a
failed document is re-enqueued.

```sql
UPDATE documents
   SET status = 'PROCESSING', attempts = attempts + 1,
       processing_started_at = ?, updated_at = ?
 WHERE id = ? AND status IN ('PENDING','FAILED');
```

Zero rows affected means this message is a redelivery of work already claimed, and the
worker stops. Terminal writes are conditional on `status = 'PROCESSING'`, so a late
duplicate cannot overwrite a finished row.

**Runs:** `PENDING → RUNNING → SUCCEEDED | FAILED`, claimed the same way.

### Store layer

`internal/store`, hand-written `database/sql` over `github.com/go-sql-driver/mysql`. One
file per entity plus `store.go` for the handle, pool settings and the shared transition
helpers.

The store returns errors already classified by `internal/httpx`, so a handler can map an
error to a status without inspecting SQL internals. The coupling is to a classification
package that has no HTTP server dependency, which is what it was built for.

A duplicate-key violation on `uniq_active_run` (MySQL error 1062) is translated into
`httpx.Conflict` at the point of insert, so the 409 carries a useful message.

### API surface

```
GET    /healthz                    liveness, touches nothing
GET    /readyz                     readiness, pings MySQL
POST   /api/incidents              create
GET    /api/incidents              list, cursor paginated
GET    /api/incidents/{id}         fetch
POST   /api/documents              multipart upload              (M5)
GET    /api/documents              list, filter by service/type  (M5)
GET    /api/documents/{id}         fetch, includes status        (M5)
```

Liveness and readiness are separate endpoints so that a brief MySQL outage does not cause
the orchestrator to restart an otherwise healthy process.

JSON fields are snake_case. Timestamps are RFC 3339 in UTC with microsecond precision.

**Cursor pagination.** `?limit=20&cursor=<ulid>`, newest first, implemented as `WHERE id <
? ORDER BY id DESC LIMIT ?`. Default limit 20, maximum 100. The response is `{"items":
[...], "next_cursor": "..."}` with `next_cursor` omitted on the last page. ULIDs sort by
creation time, so the cursor needs no encoding and the window does not shift under
concurrent inserts.

**Error envelope.** `code` is exactly `httpx.Kind.String()`: `invalid`, `not_found`,
`conflict`, `unavailable`, `internal`. `message` is `httpx.Message(err)`, which returns a
generic string for anything this application did not classify, so internal error text
cannot reach a client. `fields` is present only on validation errors.

```json
{
  "error": {
    "code": "invalid",
    "message": "title is required",
    "request_id": "01JBQ8K3M7VXFZ2N9WQYRT4HCD",
    "fields": { "title": "required" }
  }
}
```

**Request IDs.** Middleware generates a ULID per request, or honours an inbound
`X-Request-ID`, stores it in the context with `log.WithRequestID`, echoes it in the
`X-Request-ID` response header, and includes it in every error body. `internal/log` already
attaches it to every record, so a user quoting an ID leads straight to the matching log
line.

**Validation.** `title` non-blank and at most 255 characters; `description` at most 8192;
`service`, when present, matching `^[a-z0-9][a-z0-9-]{0,63}$`. All offending fields are
reported in one response rather than one per request.

### Configuration added

`MYSQL_DSN` (required), `DB_MAX_OPEN_CONNS` (default 25), `DB_MAX_IDLE_CONNS` (25),
`DB_CONN_MAX_LIFETIME` (5m). M4 adds HTTP server timeouts; M5 adds `DOCUMENT_STORAGE_ROOT`
and an upload size cap. All use the typed helpers already present in
`internal/config/env.go`.

### Migrations

`golang-migrate` with the `iofs` source and SQL embedded via `go:embed`, so a binary
carries its own migrations. Files are `migrations/000001_initial_schema.{up,down}.sql`,
creating all six tables.

A dedicated `cmd/migrate` binary applies them. In Compose it becomes a service the others
wait on with `depends_on: { condition: service_completed_successfully }`. Migrating on API
startup would be simpler but would leave the workers racing a schema that may not exist
yet. golang-migrate's MySQL driver takes an advisory lock, so concurrent runs are safe
regardless.

## Alternatives

**`ENUM` status columns.** Self-documenting in the schema, but every added value is a DDL
change and the transition rules still have to live in Go.

**Sentinel errors in the store, translated in handlers.** Keeps the store free of any HTTP
notion, at the cost of a translation step every caller has to remember. Rejected because
`httpx` carries no server dependency and defaults to 500, so the failure mode of forgetting
is safe in one direction only — and it is the direction that leaks.

**Evidence as JSON inside `final_result`.** Fewer moving parts and closer to how the UI
consumes it, but opaque to SQL. Rejected because measuring retrieval quality is an explicit
goal.

**Offset pagination.** Simpler and yields a total count for the UI, but rows shift between
requests. With ULID primary keys a cursor costs almost nothing.

**UUIDv7 as `BINARY(16)`.** Half the index size and equally sortable, but every manual
query needs `HEX()`/`UNHEX()`, which is friction paid continuously during development.

**`BIGINT AUTO_INCREMENT`.** Fastest and smallest, but the ID only exists after the
`INSERT`, and sequential public identifiers leak volume.

**Deduplicating documents by content hash.** Rejected: ambiguous when the metadata differs.

**Splitting the initial migration by phase.** Would honour the project's rule about not
building ahead of need, since four tables have no consumer until M25. Rejected because the
schema is settled by this spec either way, and landing it as one coherent artifact is worth
more than deferring half of it.

**Migrating on API startup.** Rejected: the workers would race the schema.

## Detailed Implementation

**M3**

- `internal/id` — ULID generation wrapping `oklog/ulid/v2`, monotonic within a millisecond.
- `migrations/000001_initial_schema.{up,down}.sql` — all six tables; the down migration
  drops them in dependency order.
- `cmd/migrate` — applies embedded migrations against `MYSQL_DSN`, logs what it applied,
  exits non-zero on failure.
- `internal/store` — `store.go` (handle, pool configuration, `sql.ErrNoRows` and error 1062
  translation), `incidents.go`, `documents.go`, `runs.go`, `steps.go`, `tool_calls.go`,
  `evidence.go`. Cursor paging helper shared across list methods.
- `internal/config` — the four database variables.
- Integration tests behind `-tags=integration`, skipping cleanly when `TEST_MYSQL_DSN` is
  unset so `go test ./...` stays green without infrastructure. Tables are truncated between
  tests.
- `deploy/docker-compose.yml` — MySQL 8 with a health check; `make up` / `make down`.

**M4**

- `cmd/api` — Gin server with read, write and idle timeouts, wired to
  `shutdown.Context` and a `shutdown.Group`.
- `internal/api` — request-ID middleware, request logging middleware, the error render
  helper producing the envelope above, validation helpers, incident handlers.
- `/healthz` returns 200 unconditionally; `/readyz` pings MySQL with a short timeout and
  returns 503 with `code: "unavailable"` on failure.

**M5**

- Multipart upload capped by size and restricted to `.md` and `.txt`, written under
  `DOCUMENT_STORAGE_ROOT` with a generated path, hashed while streaming to disk.
- Document list with `service` and `document_type` filters, and fetch by ID.

## Verification

- `make check` — unit tests for validation, cursor handling, transition rules and the error
  envelope, none of which need infrastructure.
- `make up` then `go test -tags=integration ./...` — the schema applies from empty and the
  down migration reverses it; a second claim of the same document returns zero rows
  affected; a terminal write from a late duplicate does not overwrite a finished row; a
  second active run for one incident is rejected by `uniq_active_run`; paging over more
  than one page returns every row exactly once.
- Manual smoke: create an incident, list it, fetch it, then request a missing one and
  confirm the 404 body carries a `request_id` matching both the `X-Request-ID` response
  header and the corresponding log line.

## Known limitations, accepted

**A crashed worker blocks its incident.** Nothing yet reclaims a run left in `RUNNING` by a
worker that died, so `uniq_active_run` makes that incident permanently return 409. The
columns a lease needs (`attempts`, `started_at`, `processing_started_at`) exist here; the
reclaim policy belongs to the async messaging spec (S2), which owns delivery semantics. No
worker exists until M25 and S2 lands well before it, so the deadlock is not reachable in
the interim. This is a deliberate boundary between two specs, not an oversight.

**Four tables have no consumer until M25.** `agent_runs`, `agent_steps`, `tool_calls` and
`evidence` are created by the initial migration although nothing reads or writes them until
the agent worker exists.

**The 8 KiB truncation cap is a guess.** It is a single constant and should be revisited
once real tool output exists in M17 to M19.
