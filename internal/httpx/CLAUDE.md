# internal/httpx

## Purpose

Classifies errors well enough that an HTTP layer can choose a status code without knowing
where the error came from. Without it a handler either inspects the layers below it —
coupling it to their internals — or returns 500 for everything.

## Contents

- `error.go` — the `Kind` enum (`KindInternal`, `KindInvalid`, `KindNotFound`,
  `KindConflict`, `KindUnavailable`) with `String` and `Status`; the closed set of
  validation reason codes (`CodeRequired`, `CodeTooLong`, `CodeTooShort`,
  `CodeInvalidFormat`, `CodeInvalidValue`, `CodeInvalidType`); the `Error` type; the
  constructors `Invalid`/`InvalidErr`, `NotFound`/`NotFoundErr`, `Conflict`/`ConflictErr`,
  `Unavailable`/`UnavailableErr`, `InvalidFields` and `Internal`; and the inspectors
  `KindOf`, `StatusFor`, `Message` and `FieldsOf`.
- `error_test.go` — unit tests for classification through wrapping and for the
  standard-library cases.

## How it fits in

`store` returns errors already classified through this package, and `api` renders them.
That is the whole point of the split: the coupling is to a classification package with no
HTTP server dependency, so the store does not import a web framework.

## Gotchas

- **`Message` is client-facing; `Err` is for logs.** `Internal` deliberately does not
  derive its message from the wrapped error, because internal error text routinely
  contains connection strings, query fragments and file paths.
- **The kind set is small on purpose.** A classification with thirty members stops being
  applied consistently. Before adding one, check whether the caller can carry the
  distinction itself — `api` handles 413 with its own sentinel rather than a sixth kind.
- **`KindOf` classifies both `context.DeadlineExceeded` and `context.Canceled` as
  `KindUnavailable`**, even though neither was produced by this package — a dependency that
  did not answer in time is an availability problem rather than a bug, and a cancellation
  normally means the client went away. The alternative is every caller remembering to
  translate them. Anything else unclassified is `KindInternal`.
- **`Message` falls back to a generic string** for an error this package did not classify.
  An unclassified error reaching a handler is a bug, and its text must not be echoed to the
  caller.
- **Reason codes are a closed set** so a client can branch on them and a UI can localize
  them. Human detail belongs in `Message`, not in a new code.
- The JSON response envelope is *not* defined here — it belongs to the data model and API
  surface spec, and `api/errors.go` renders it. This package supplies only the three pieces
  that are properties of the error itself.
