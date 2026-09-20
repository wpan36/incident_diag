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

**Nullability and defaults.** Every column is `NOT NULL` unless the table below marks it
`NULL`. Every counter — `attempts`, `chunk_count`, `size_bytes`, `step_count`,
`tool_call_count`, `prompt_tokens`, `completion_tokens`, `observation_bytes`,
`result_bytes`, `duration_ms` — is `NOT NULL DEFAULT 0`, and every boolean is `NOT NULL
DEFAULT FALSE`. A counter that has not been reached yet reads as zero rather than `NULL`, so
aggregates over these columns need no `COALESCE` and the Go structs need no pointer fields.
`chunk_count` is 0 until a document reaches `READY`, which is also its correct value there.

**Foreign keys** follow ownership: `agent_steps`, `tool_calls` and `evidence` cascade from
`agent_runs`, and `tool_calls` and `evidence` also cascade from `agent_steps`.

`evidence.document_id` deliberately does not — it is `ON DELETE SET NULL`, because evidence
is not owned by the document it cites. Deleting a document should not erase the record that
an investigation relied on it; `source_ref` keeps the citation readable afterwards.

`agent_runs` does not cascade from `incidents` either, but that one is not a choice. MySQL
refuses a foreign key with `ON DELETE CASCADE` on a column that a `STORED` generated column
is derived from, and `active_incident_id` is derived from `incident_id` (see below), so the
two cannot both exist. `fk_agent_runs_incident` therefore has no `ON DELETE` clause, and
deleting an incident that still has runs is refused rather than taking the runs with it.
The generated column is worth more than the cascade: it cannot drift from `status`, whereas
the cascade protects a delete path that does not exist. Found while implementing M3 and
settled with the user.

Nothing deletes rows today, so all of this describes intent rather than a code path, but it
has to be in the DDL from the start.

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
| `chunk_count` | `INT` | 0 until set on `READY` |
| `attempts` | `INT` | incremented on each claim |
| `processing_started_at` | `DATETIME(6)` NULL | for lease reclaim |
| `created_at`, `updated_at` | `DATETIME(6)` | |

Indexes: `(status, id)` for listing by status and for the lease reclaim S2 will add — the
ingestion worker itself looks a document up by primary key, since the Kafka message carries
the ID — and `(service, document_type)` for filtered listing.

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

Indexes: `(run_id, id)` for reading a run's timeline, which is the common query, and a
`UNIQUE` key on `(run_id, step_number)`.

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

The `*_bytes` columns count **bytes of UTF-8**, not characters or runes, and so does the cap.
Truncation is nevertheless **rune-safe**: the cut falls on a rune boundary at or below 8 KiB,
never inside a multi-byte sequence. Slicing a Go string at a fixed byte offset would produce
invalid UTF-8, which a `utf8mb4` column will reject or silently mangle, and tool output
contains non-ASCII text often enough for this to be a matter of when rather than whether. One
shared helper in `internal/store` does the truncation and returns the summary, the original
byte length and the flag together, so no caller can record one without the others.

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

**DSN parameters.** `MYSQL_DSN` must carry `parseTime=true&loc=UTC`, and the store verifies
both at startup rather than trusting the operator, failing with an explicit message if either
is missing. Without `parseTime`, `go-sql-driver/mysql` hands a `DATETIME(6)` back as `[]byte`
and every scan into a `time.Time` fails; without `loc=UTC` the driver interprets those values
in the local zone, which quietly reintroduces exactly the machine-dependence that choosing
`DATETIME` over `TIMESTAMP` was meant to remove.

**Page parameters are normalized in the store, not only validated in the API.** The handler
rejects a limit outside `[1, 100]` rather than correcting it, and that stays true;
`PageParams.normalize` is for the callers that never reach a handler — the agent worker, a
maintenance job, a test. A zero value asks for a page of no rows, which the `limit+1`
arithmetic below cannot express and used to answer with a panic, so it is clamped to the
default. Every list method normalizes before it uses `Limit`, so the fetch and the
pagination cannot disagree about what it is.

**Duplicate keys.** A 1062 is not conflict-shaped in general: `agent_steps` has a `UNIQUE`
key on `(run_id, step_number)`, and violating that one is a bug in the agent loop, not
something a client should see as a 409. So the translation is per constraint, not blanket.
`go-sql-driver/mysql` exposes the violation as a `*mysql.MySQLError` whose `Message` names
the offending key, and the store matches on `uniq_active_run` specifically, mapping it to
`httpx.Conflict` with a message naming the incident. Any other 1062 stays `httpx.Internal`,
so a programmer error surfaces as a 500 and reaches the logs instead of being dressed up as
a legitimate client conflict. Matching on the key name means depending on the text of a
driver error, which is unpleasant but is the only identifier MySQL provides; an integration
test asserts both branches so a driver upgrade that changes the wording fails loudly.

### API surface

```
GET    /healthz                    liveness, touches nothing              200
GET    /readyz                     readiness, pings MySQL            200 | 503
POST   /api/incidents              create                            201 | 400
GET    /api/incidents              list, cursor paginated            200 | 400
GET    /api/incidents/{id}         fetch                             200 | 404
POST   /api/documents       (M5)   multipart upload            201 | 400 | 413
GET    /api/documents       (M5)   list, filter by service/type      200 | 400
GET    /api/documents/{id}  (M5)   fetch, includes status            200 | 404
```

A successful create returns **201** with a `Location` header pointing at the new resource
and the created resource as the body, so a client never has to follow up with a `GET` to
learn the generated ID. Any endpoint can additionally return 500 `internal` or, when MySQL
is unreachable, 503 `unavailable`.

**Every request body is bounded before it is read.** A JSON body is capped at **64 KiB**
and the upload's multipart envelope at `DOCUMENT_MAX_UPLOAD_BYTES` plus 1 MiB; exceeding
either is **413** with `code: "invalid"`. Without the JSON bound the decoder reads whatever
arrives into memory and only then discovers the field is too long — a 200 MB body took the
API process from 40 MB resident to 688 MB before answering 400, a whole-process cost the
client chooses and the server pays. The cap is far above anything legitimate: the largest
valid body is an incident, whose longest fields are a 255-character title and an
8192-character description, at most 33 KiB of UTF-8 even when every character is four
bytes. Found by the M5 mean-review.

Liveness and readiness are separate endpoints so that a brief MySQL outage does not cause
the orchestrator to restart an otherwise healthy process. Both return a body of
`{"status": "ok"}` on success; `/readyz` returns the standard error envelope with
`code: "unavailable"` on failure. The body exists so that a human running `curl` sees
something, and is deliberately not extended with dependency detail — a readiness probe that
reports which dependency failed is a monitoring endpoint wearing the wrong hat.

JSON fields are snake_case.

**Timestamps** are RFC 3339 in UTC with fixed microsecond precision:
`2026-09-20T10:11:12.345678Z`. Go's default marshalling of `time.Time` is RFC 3339 *Nano*,
which trims trailing zeros, so the same instant would serialize to a different number of
digits depending on its value. The API therefore formats through one exported layout
constant — `2006-01-02T15:04:05.000000Z07:00` — applied by a small `api.Time` wrapper type
used in every response struct, rather than by remembering to format at each call site.

### Response shapes

**One shape per entity, shared by list and fetch.** A list item is not a reduced projection
of the fetched resource: the front end then has one type per entity instead of two, and
nothing has to be re-fetched just to render a detail view. Neither entity is large enough
for the saved bytes to matter.

**A nullable column renders as an explicit `null`, never an absent key.** The set of keys is
therefore the same in every response, which is what makes the generated TypeScript types in
M28 honest — `omitempty` would make `service` optional in the type system for a reason that
has nothing to do with the domain, and would make an empty string and an absent value
indistinguishable on the wire.

An incident:

```json
{
  "id": "01JBQ8K3M7VXFZ2N9WQYRT4HCD",
  "title": "payment-service latency spike",
  "description": "p99 above 2s since 10:05, checkout timing out",
  "service": "payment-service",
  "created_at": "2026-09-20T10:11:12.345678Z",
  "updated_at": "2026-09-20T10:11:12.345678Z"
}
```

A document:

```json
{
  "id": "01JBQ8M0YB4C3D2E1F0G9H8J7K",
  "filename": "payment-service-runbook.md",
  "format": "markdown",
  "size_bytes": 8421,
  "content_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "service": "payment-service",
  "document_type": "runbook",
  "status": "READY",
  "failure_reason": null,
  "chunk_count": 12,
  "created_at": "2026-09-20T09:00:00.000000Z",
  "updated_at": "2026-09-20T09:00:04.117000Z"
}
```

**`storage_path` is never returned**, and neither are `attempts` or `processing_started_at`.
The path is a server filesystem detail that would tell a browser about the container's
layout and would invite a client to construct one; the other two are worker bookkeeping that
means nothing outside the ingestion pipeline. `content_sha256` is returned, because a client
that just uploaded a file can use it to confirm what arrived.

**Cursor pagination.** `?limit=20&cursor=<ulid>`, newest first, implemented as `WHERE id <
? ORDER BY id DESC LIMIT ?`. Default limit 20, maximum 100. The response is `{"items":
[...], "next_cursor": "..."}` with `next_cursor` omitted on the last page — and `items` is
always an array, `[]` rather than `null`, when nothing matches. ULIDs sort by creation time,
so the cursor needs no encoding and the window does not shift under concurrent inserts.

**`next_cursor` comes from a `limit+1` fetch.** The query asks for one row more than the
caller wanted; if that extra row exists it is dropped from `items` and `next_cursor` is set
to the ID of the last item returned. Inferring the last page from "fewer rows than `limit`"
instead would be wrong whenever the final page is exactly full, handing the client a cursor
that fetches nothing — a bug that only appears when the row count happens to be a multiple
of the page size, which is precisely when nobody is testing.

**Bad pagination input is rejected, not corrected.** A `limit` that is not an integer, is
below 1, or is above 100, and a `cursor` that is not a valid ULID, all return 400 `invalid`
with the offending parameter in `fields`. Silently clamping `limit=1000` to 100 would let a
client believe it had received a complete result; the same argument the validation rules
below rest on applies to query parameters. An absent `limit` or `cursor` is not an error —
it takes the default.

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

**`fields` lives on `httpx.Error`.** The struct gains `Fields map[string]string`, nil on
every error except a validation error. This revises the M2 surface described in
`docs/plans/infrastructure-packages.md`, which deliberately left the error *body* to this
spec; the field is part of that same contract, so it is decided here and M2's spec gets a
pointer back to this one. The alternative — a validation error type local to `internal/api`
that wraps `httpx.Invalid` — keeps `httpx` narrower but makes the render helper do a second
`errors.As` and gives two error types the same job. `httpx` already carries `Kind` and
`Message` for the wire; carrying the third piece of the same envelope is consistent rather
than a new concern.

`fields` maps a **request field name** to a **short reason code**, not to prose. The codes
are a closed set: `required`, `too_long`, `too_short`, `invalid_format`, `invalid_value`,
`invalid_type`. A client can branch on them and a UI can localize them, neither of which
works against a sentence. Human detail belongs in `message`.

When exactly one field is at fault, `message` describes it — `"title is required"`. When
more than one is, `message` is the fixed string `"request validation failed"` and `fields`
carries the detail, rather than `message` growing a comma-joined list that no client can
parse and every client ends up displaying.

**Request IDs.** Middleware generates a ULID per request, or honours an inbound
`X-Request-ID`, stores it in the context with `log.WithRequestID`, echoes it in the
`X-Request-ID` response header, and includes it in every error body. `internal/log` already
attaches it to every record, so a user quoting an ID leads straight to the matching log
line.

An inbound `X-Request-ID` is **validated before it is trusted**: at most 128 characters, and
only `[A-Za-z0-9_-]`. Anything else is discarded and a fresh ULID generated, without
failing the request — the header is a convenience for tracing across services, not user
input worth rejecting a request over. The bound matters because this value is written into
every structured log record for the request and echoed in the response body, so an
unvalidated header is an unbounded client-controlled string in both places, with newlines
available to anyone who wants to forge a log line.

**Validation.** `title` non-blank and at most 255 characters; `description` at most 8192;
`service`, when present, matching `^[a-z0-9][a-z0-9-]{0,63}$`. Leading and trailing
whitespace is trimmed from `title` and `service` before validation, so a title of `"   "` is
`required`, not a 3-character title. All offending fields are reported in one response
rather than one per request.

**A limit of *n* characters means *n* runes, not *n* bytes.** MySQL counts `VARCHAR(255)`
in characters, so a byte-counting check rejects a 100-character Chinese title the column
would have stored — and tells the client, untruthfully, that it was longer than 255
characters. Every length check goes through `utf8.RuneCountInString`. The `description`
column is `TEXT`, which is 65535 *bytes*, so 8192 characters fits there in the worst case
too. The one bound that stays in bytes is the multipart form value below, because it is
enforced while the part is still being read and there is no string to count yet.

### Configuration added

`MYSQL_DSN` (required), `DB_MAX_OPEN_CONNS` (default 25), `DB_MAX_IDLE_CONNS` (25),
`DB_CONN_MAX_LIFETIME` (5m). All use the typed helpers already present in
`internal/config/env.go`.

**These do not go into `config.Load()`.** That function is shared by every binary in the
project, so a required `MYSQL_DSN` there would stop `ops-mcp` and the two Incident Lab
services from starting over a database none of them touches. Instead a second function,
`config.LoadDatabase() (Database, error)`, returns the four values above and is called only
by `cmd/api`, `cmd/migrate`, `ingestion-worker` and `agent-worker`. Each binary declares
what it actually needs, `Load` keeps its promise from M2 to hold only what everything reads,
and the collecting-validation behaviour is unchanged — a binary that calls both reports the
problems from both at once, so the operator still sees one complete list.

The same pattern applies to what comes later: M5 adds `DOCUMENT_STORAGE_ROOT` (default
`/var/lib/incident_diag/documents`) and `DOCUMENT_MAX_UPLOAD_BYTES` (default 10 MiB) to a
loader used by the API and the ingestion worker, and M4 adds HTTP server timeouts to one
used by the API alone.

**Where these values come from locally.** `.env` at the repository root, loaded by the
Makefile and exported to every recipe, and handed to docker compose with
`--project-directory .`. Both halves are needed and neither is obvious. Compose takes its
project directory from the compose file's own location, so `docker compose -f
deploy/docker-compose.yml` reads `deploy/.env` and ignores the root one — the MySQL
container would come up on the built-in defaults while the Go binaries used a DSN from a
file compose never saw, and the two would disagree about the port. And `.env` is not a
shell script: the DSN contains `(`, `)` and `&`, so `source .env` is a syntax error, and
even the lines that parse are not exported to a child process. The values therefore stay
unquoted, because make and compose both parse the file directly and quotes would become
part of the value.

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

**`omitempty` on nullable response fields.** Smaller payloads and idiomatic Go, but it makes
every nullable field optional in the front end's type system and makes "absent" and "empty"
indistinguishable. Rejected in favour of explicit `null`.

**Clamping an out-of-range `limit`.** The more common convention, and forgiving of a client
that asks for 1000 rows. Rejected because the client then cannot tell a clamped page from a
complete one.

**A validation error type local to `internal/api`.** Would keep `httpx` free of anything
response-shaped. Rejected because `httpx` already owns two thirds of the envelope, and two
error types doing one job is worse than one package knowing slightly more.

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
- `internal/config` — `LoadDatabase()` and the four database variables, alongside the
  existing `Load()` rather than inside it.
- Integration tests behind `-tags=integration`, skipping cleanly when `TEST_MYSQL_DSN` is
  unset so `go test ./...` stays green without infrastructure. Tables are reset between
  tests by a helper that wraps the `TRUNCATE`s in `SET FOREIGN_KEY_CHECKS = 0` and restores
  it afterwards — `TRUNCATE` on a table referenced by a foreign key fails outright, so the
  helper is the only supported way to reset and tests do not issue their own.
- `deploy/docker-compose.yml` — MySQL 8 with a health check; `make up` / `make down`.

**M4**

- `cmd/api` — Gin server with read, write and idle timeouts, wired to
  `shutdown.Context` and a `shutdown.Group`.
- `internal/api` — request-ID middleware, request logging middleware, the error render
  helper producing the envelope above, the `api.Time` wrapper and the response types,
  validation helpers, the shared cursor-parameter parser, incident handlers.
- `internal/httpx` — `Fields` added to `Error`, with a constructor for a validation error.
- `/healthz` returns 200 unconditionally; `/readyz` pings MySQL with a short timeout and
  returns 503 with `code: "unavailable"` on failure.

**M5**

- Multipart upload capped by size and restricted to `.md` and `.txt`, written under
  `DOCUMENT_STORAGE_ROOT` with a generated path, hashed while streaming to disk.
- Document list with `service` and `document_type` filters, and fetch by ID.

#### The upload request

`POST /api/documents`, `multipart/form-data`, with three parts:

| Part | Required | Rules |
| --- | --- | --- |
| `file` | yes | extension `.md` or `.txt`, at most `DOCUMENT_MAX_UPLOAD_BYTES` |
| `document_type` | yes | `runbook` \| `postmortem` \| `service_doc` |
| `service` | no | `^[a-z0-9][a-z0-9-]{0,63}$` |

`document_type` is required and `service` is optional, matching the nullability the schema
already has. Defaulting `document_type` would be friendlier to a `curl` smoke test and would
produce a corpus of mislabelled documents that M11's metadata filter then cannot separate;
requiring `service` as well would reject documents that genuinely are not about one service,
such as a general on-call runbook.

`format` is derived from the extension — `.md` → `markdown`, `.txt` → `text` — rather than
being a fourth form field. It is a property of the file, not a claim the client should be
able to make about it, and the ingestion parser in M7 has to trust it.

A file that is too large returns **413** with `code: "invalid"`; the envelope has no
distinct kind for it and inventing one in `httpx` is not worth it. 413 is the only status
this API returns that is not `httpx.Kind.Status()` — `KindInvalid` maps to 400 — so a
too-large error is *marked*, with a sentinel the render helper recognises, rather than each
handler passing a status of its own. Letting handlers choose was tried and was wrong: the
envelope can also overflow midway through streaming the file, a path that never reaches the
handler's own size check, and it answered 500 `internal server error` — telling the client
this server was broken, and writing an ERROR log record, for a request the client alone made
too big. Marking the error states the rule once, so a path nobody thought about inherits it.

An over-long `document_type` or `service` part is **not** one of these. It is a field the
client got wrong, so it is the usual 400 with `fields: {"service": "too_long"}`; reporting
it as an unreadable body sent the client looking at its multipart encoding instead of at the
value it sent. Everything else that fails validation is likewise a 400 with `fields`.

**The stored path is `<document_id>/<sanitized_filename>`**, relative to
`DOCUMENT_STORAGE_ROOT`. The ULID directory makes collisions impossible without consulting
the database, which matters because two uploads of `runbook.md` are a normal thing for a
user to do, and keeping the original name in the path keeps the shared volume readable when
something goes wrong. Sanitizing means taking `filepath.Base` and rejecting any name that is
not left intact by it — an upload is not allowed to influence where it lands. The file is
written, hashed and size-counted in a single streaming pass; the row is inserted only after
the file is safely on disk, so a crash leaves an orphaned file rather than a row pointing at
nothing.

## Verification

- `make check` — unit tests for validation, cursor handling, transition rules and the error
  envelope, none of which need infrastructure. Specifically: a multi-field validation
  failure produces one response carrying every offending field and the fixed `message`;
  rejected `limit` and `cursor` values; an over-long and a newline-bearing inbound
  `X-Request-ID` are both replaced rather than echoed; the timestamp layout renders a
  whole-second instant with six digits; rune-safe truncation of a string whose 8 KiB
  boundary falls inside a multi-byte character; a title of exactly 255 Chinese characters
  is accepted and one of 256 is not, so the limits are counted in runes; a JSON body over
  64 KiB is 413 while one just under it still reaches field validation; a multipart
  envelope that overflows **while the file part is streaming** is 413 rather than 500,
  with nothing left on disk; an over-long `service` or `document_type` part is 400 with
  that field in `fields`; `paginate` given an unnormalized zero limit returns an empty
  page instead of panicking.
- `make up` then `make test-integration` — the schema applies from empty and the
  down migration reverses it; a second claim of the same document returns zero rows
  affected; a terminal write from a late duplicate does not overwrite a finished row; a
  second active run for one incident is rejected by `uniq_active_run` and surfaces as
  `httpx.Conflict`, while a duplicate `(run_id, step_number)` surfaces as `httpx.Internal`;
  paging over more than one page returns every row exactly once, **including the case where
  the total row count is an exact multiple of the page size**, which must not yield a
  trailing empty page; an incident whose title is 255 Chinese characters — 765 bytes —
  round-trips through `VARCHAR(255)` unchanged; a list call with an unset `PageParams`
  returns rows rather than panicking.
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
