# internal/id

## Purpose

Generates the identifiers used for every row in this project: ULIDs rendered as 26
Crockford base32 characters.

## Contents

- `id.go` — `New`, `NewAt` (for tests that need identifiers in a known order), `Valid`,
  and the shared mutex-guarded monotonic entropy source.
- `id_test.go` — unit tests, including ordering within a single millisecond and
  concurrent generation.

## How it fits in

Imported by `store` (row ids), `api` (request ids and the document id created before the
upload is streamed) and anything else that needs an identifier before an INSERT.

## Gotchas

- **Three properties are being bought, and all three are load-bearing.** IDs sort by
  creation time, so the primary key doubles as the pagination cursor and no ordering
  column is needed; they are readable in a log line or a URL, which `BINARY(16)` is not;
  and the application knows the value before the INSERT, which matters as soon as a row
  and a Kafka message carrying its id must be produced together. Swapping the scheme
  breaks cursor pagination in `store`.
- **The entropy source is shared and mutex-guarded on purpose.** Monotonicity is a
  property of a single source: two independent generators can emit out-of-order ids within
  the same millisecond.
- **`New` panics if the entropy source fails.** For `crypto/rand` that means the operating
  system has stopped providing randomness, which has no useful recovery, and an error
  return would put an impossible branch at every call site.
- `Valid` exists to reject a malformed pagination cursor before it reaches SQL — a client
  error, not an empty page.
