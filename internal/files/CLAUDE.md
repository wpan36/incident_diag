# internal/files

## Purpose

Stores uploaded document files on disk, under a single root directory, hashing and
counting them as they stream in.

## Contents

- `files.go` — `Storage` with `New`, `Root`, `MaxBytes`, `Save` and `Remove`; the `Saved`
  result (`Path`, `Size`, `SHA256`); `ErrTooLarge`; and `checkName`.
- `files_test.go` — unit tests, including the traversal and oversize cases.

## How it fits in

Separate from `internal/store` because the two answer to different failures: MySQL holds
the metadata and the ingestion state, this holds the bytes. `api` writes through it; the
ingestion worker will read the same shared volume using the same
`config.Documents.StorageRoot`.

## Gotchas

- **Write ordering is deliberate and depended upon.** The file reaches disk before the row
  exists, so a crash leaves an orphaned file rather than a row pointing at nothing. Do not
  reorder this to "insert then write".
- **`Saved.Path` is relative to the root** (`<documentID>/<filename>`), which is what goes
  into `documents.storage_path`. Storing an absolute path would tie the row to one host
  layout. `Storage` makes its own root absolute at construction so nothing depends on the
  process's working directory.
- **The ULID directory is what makes collisions impossible** without consulting the
  database — two uploads of `runbook.md` are normal — while keeping the original filename
  keeps the shared volume readable when something goes wrong.
- **`Save` reads one byte past the limit** so exceeding it is detected rather than silently
  truncating the document to exactly the cap. On `ErrTooLarge` it leaves nothing behind.
- **The file is opened `O_EXCL`, not truncating.** A document id is generated per upload,
  so an existing file there means something is wrong and overwriting would hide it.
- **`checkName` is not redundant with the handler's validation.** This is the function that
  turns a name into a path, so it is the one place where being wrong lets an upload choose
  where it lands. `filepath.Base` is the test, plus an explicit backslash check for the
  Windows spelling that `Base` does not strip on Linux.
- **`Remove` is best effort**, called on upload failure paths where the request is already
  being answered with an error and a second error would hide the first.
