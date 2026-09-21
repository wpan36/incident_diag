# internal/api

## Purpose

The HTTP surface: routing, middleware, request validation and the response shapes. Two
things hold across every endpoint — every failure renders the same envelope, built from the
error's `httpx` classification rather than from whatever the handler happened to know; and
every response timestamp goes through `wire.Time`, so the format is a property of the type
instead of something each handler must remember.

## Contents

- `server.go` — `Deps`, `Server`, `NewServer`, `Router` (gin routes and middleware order),
  `/healthz` and `/readyz`.
- `middleware.go` — `requestID` (honours a validated inbound `X-Request-ID`, otherwise
  mints a ULID), `requestLogger`, and `recovery`.
- `errors.go` — the `errorBody` envelope, `renderError`, `renderCreated`, the `errTooLarge`
  sentinel and `tooLarge`, and `logAttrs`.
- `validate.go` — the `validation` collector, the field length limits and
  `serviceNamePattern`.
- `page.go` — the `list[T]` response shape and `parsePageParams`.
- `responses.go` — the wire shapes `Incident`, `Document` and `SearchResult`, and their
  converters. The run shapes live in `internal/wire`, and so does `Time`.
- `incidents.go` — create, list and get, plus `decodeJSON` and `maxJSONBodyBytes`.
- `documents.go` — the streaming multipart upload, the accepted extensions and document
  types, `enqueueIngestion`, plus list and get.
- `runs.go` — `POST /api/incidents/:id/runs`, `GET /api/runs/:id`,
  `GET /api/incidents/:id/runs`, plus `enqueueRun` and `newRunList`.
- `search.go` — `GET /api/search`: parameter validation, then embed the query and retrieve.
- `api_test.go`, `documents_test.go`, `search_test.go` — unit tests against the router with
  fakes.
- `integration_test.go`, `runs_integration_test.go` — `//go:build integration`. Real MySQL
  and a real directory.

## How it fits in

The top of the `internal/` graph: it depends on `store`, `files`, `mq`, `embed`, `search`,
`wire`, `config`, `httpx`, `id` and
`log`, and nothing depends on it except `cmd/api`. It is where `httpx` kinds become status codes
and where stored types become wire types.

## Gotchas

- **Middleware order is load-bearing.** `requestID` runs first so the log record and the
  error envelope can both see the identifier; `recovery` sits inside the logger so a panic
  still produces a request record. `gin.New`, not `gin.Default` — the default engine
  installs gin's own logger and recovery, which would write a second, differently shaped
  line per request and answer a panic with an unparseable body.
- **413 is the one status not derived from an `httpx` kind.** An oversize file, multipart
  envelope or JSON body is `KindInvalid`, which would render 400. The `errTooLarge` sentinel
  keeps that exception stated once in `renderError`, rather than letting each handler pass
  its own status — a handler that forgets answers 400 to a request it never read, which is a
  lie the client cannot detect.
- **Validation collects every problem, and the first complaint about a field wins.** A later,
  vaguer check must not overwrite a specific one: an upload whose filename has the wrong
  extension has already been told something useful, and the "file is required" check that
  follows would otherwise send the client looking for the wrong mistake.
- **Lengths are counted in runes, not bytes.** MySQL counts `VARCHAR(255)` in characters, so
  a byte count would reject a 100-character Chinese title and claim it was over 255
  characters. Every length check goes through `utf8.RuneCountInString`.
- **Bad pagination input is rejected, not corrected.** Silently clamping `limit=1000` to 100
  would let a client believe it had a complete result. `parsePageParams` records into the
  caller's `validation` so a listing with filters reports everything at once.
- **No `omitempty` on nullable fields.** A nullable column renders as an explicit `null`,
  never an absent key, so the key set is identical in every response — which is what makes
  the generated TypeScript types in M28 honest.
- **Responses use one shape per entity for both list and fetch.** A list item is not a
  reduced projection, so the front end has one type per entity and never re-fetches just to
  render a detail view.
- **`Document` deliberately omits three columns.** `storage_path` would tell a browser about
  the container's layout and invite a client to construct one; `attempts` and
  `processing_started_at` are ingestion bookkeeping. `content_sha256` is returned so an
  uploader can confirm what arrived.
- **`GET /api/runs/{id}` is the one endpoint with a second shape for its entity.** A
  listing returns `wire.Run` and the fetch returns `wire.RunDetail`, against the convention
  below. A listing carrying every run's whole timeline is the alternative, and it is worse.
  The exception is named here rather than discovered in review.
- **A run's budget is not overridable and the request body is empty.** `config.LoadAgent`
  ties the no-pruning invariant to configuration loading, and a request that set its own
  `max_steps` would give that invariant a second enforcement point where a violating run
  would break the guarantee silently. The budget and the model are recorded on the row
  instead, which is why `cmd/api` now loads `AGENT_*` and `LLM_MODEL` although it runs no
  agent.
- **The 409 on a second run comes from `uniq_active_run` refusing the insert**, not from a
  read-then-write check two requests could interleave. The 404 comes from reading the
  incident first: a run against a missing one violates `fk_agent_runs_incident`, and
  `dbError` does not classify a foreign-key failure as not-found, so without the read the
  client would get a 500 for its own mistake. The runs listing reads it for a smaller
  reason — an empty page and a mistyped id must not look the same.
- **A failed produce does not fail a run start either**, for the reason the upload has:
  the run was created, so 500 would be a lie, and a client retrying on it would hit 409
  from its own first attempt. The row is `PENDING`, which the run reconciler sweeps for.
- **`GET /api/runs/{id}` is not paginated.** `max_steps` bounds the step count at single
  digits and a partial timeline is not useful. That makes it the second design to depend
  quietly on that invariant, after S7's context builder.
- **Timestamps are fixed-width microseconds.** Go's default is RFC 3339 *Nano*, which trims
  trailing zeros, so the same instant would serialize with a different number of digits
  depending on its value. `Time` also implements `UnmarshalJSON` purely so the type
  round-trips for tests and generated clients. It moved to `internal/wire` in M24, because
  the agent worker publishes the same shapes onto a Redis stream.
- **The upload is a single streaming pass and the cleanup order matters.** Parts arrive in
  whatever order the client sent them, so the file may be written before `document_type` has
  been seen; `readUpload` cleans up its own errors, and the handler removes the file on a
  validation failure or a failed INSERT. The row is inserted only after the file is on disk,
  so a crash leaves an orphan rather than a dangling row.
- **There are two distinct size limits.** `files.MaxBytes` caps the file itself;
  `MaxBytesReader` with `multipartOverhead` caps the whole request, which is the backstop
  against a client streaming an unbounded number of individually small parts.
- **`format` is derived from the extension, not taken as a form field**, because it is a
  property of the file rather than a claim the client should make — the ingestion parser has
  to trust it. `document_type` is required rather than defaulted, because a default produces
  a corpus of mislabelled documents that the metadata filter cannot separate.
- **A failed produce does not fail the upload.** `POST /api/documents` writes the row,
  produces to `documents.ingest.v1`, and answers 201 whether or not the produce succeeded,
  logging the failure at error level. The document genuinely was created, so 500 would be a
  lie and a client retrying on it would upload a second copy; the row is `PENDING`, which is
  exactly what the reconciler sweeps for. This is the one place the API knowingly returns
  success for a partly completed operation, and it is safe only because the reconciler
  exists.
- **The produce happens inside the request, so its timeout is an API concern.**
  `KAFKA_PRODUCE_TIMEOUT` is spent before the response is written, which is why it has to
  stay well below `HTTP_WRITE_TIMEOUT`.
- **`/readyz` will not say which dependency failed.** That is a monitoring endpoint wearing
  the wrong hat, and it publishes the shape of the deployment to anyone who can reach the
  port. It is separate from `/healthz` so a brief MySQL outage does not get the process
  restarted.
- **An inbound `X-Request-ID` is validated before it is trusted.** It is written into every
  log record and echoed in the body, so the character set excludes anything that could forge
  a log line — newlines above all, since each record is one line of JSON. Anything
  unacceptable is replaced rather than rejected: the header is a convenience, not user input
  worth failing a request over.
- **Integration tests need `make up` and `-p 1`** (the store's migration test can otherwise
  drop the schema underneath them), and skip cleanly without `TEST_MYSQL_DSN`. Unlike the
  store's, they truncate nothing — each test tags its rows with a unique service name, so
  they can run against a database that already has data.

- **`NewServer` takes a `Deps` struct, mirroring `ingest.Deps`.** Six positional parameters,
  three of which tests pass as nil or as a fake, is how the wrong one gets passed without
  the compiler noticing.
- **`GET /api/search` is the only reason this package depends on `embed` and `search`**, and
  it is why `cmd/api` now needs the embedding configuration. An inspection endpoint that
  could not embed its query would be missing the point rather than saving a dependency. The
  agent in Phase E calls `search.Client` in process; it does not go over HTTP.
- **The search handler calls the embedder and the search client separately and classifies
  their failures separately.** "The embedding provider is rate limiting" and "Elasticsearch
  is down" are different operational problems, and a single 503 that does not say which one
  wastes the first minute of every investigation into it. This is why `search.Search` takes
  a vector rather than text.
- **`/api/search` is not paginated** although it reuses `list[T]`. A kNN result has no stable
  cursor — an index write can reorder it — and re-running the query is cheap. Reusing the
  type keeps one response family across the API; `next_cursor` is simply never set.
