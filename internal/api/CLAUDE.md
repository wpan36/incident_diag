# internal/api

## Purpose

The HTTP surface: routing, middleware, request validation and the response shapes. Two
things hold across every endpoint — every failure renders the same envelope, built from the
error's `httpx` classification rather than from whatever the handler happened to know; and
every response timestamp goes through `api.Time`, so the format is a property of the type
instead of something each handler must remember.

## Contents

- `server.go` — `Server`, `NewServer` (store, file storage, `mq.Producer`, logger),
  `Router` (gin routes and middleware order), `/healthz` and `/readyz`.
- `middleware.go` — `requestID` (honours a validated inbound `X-Request-ID`, otherwise
  mints a ULID), `requestLogger`, and `recovery`.
- `errors.go` — the `errorBody` envelope, `renderError`, `renderCreated`, the `errTooLarge`
  sentinel and `tooLarge`, and `logAttrs`.
- `validate.go` — the `validation` collector, the field length limits and
  `serviceNamePattern`.
- `page.go` — the `list[T]` response shape and `parsePageParams`.
- `time.go` — `TimeLayout`, the `Time` type and `NullTime`.
- `responses.go` — the wire shapes `Incident` and `Document` and their converters.
- `incidents.go` — create, list and get, plus `decodeJSON` and `maxJSONBodyBytes`.
- `documents.go` — the streaming multipart upload, the accepted extensions and document
  types, `enqueueIngestion`, plus list and get.
- `api_test.go`, `documents_test.go` — unit tests against the router with fakes.
- `integration_test.go` — `//go:build integration`. Real MySQL and a real directory.

## How it fits in

The top of the `internal/` graph: it depends on `store`, `files`, `mq`, `httpx`, `id` and
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
- **Timestamps are fixed-width microseconds.** Go's default is RFC 3339 *Nano*, which trims
  trailing zeros, so the same instant would serialize with a different number of digits
  depending on its value. `Time` also implements `UnmarshalJSON` purely so the type
  round-trips for tests and generated clients.
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
