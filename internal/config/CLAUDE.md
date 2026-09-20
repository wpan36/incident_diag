# internal/config

## Purpose

Loads service configuration from the process environment and refuses to hand back
anything partly valid. The environment is the only source: Compose supplies `.env` through
`env_file` and a local shell sources the same file, so there is exactly one code path.

## Contents

- `env.go` — the `env` collector and its typed readers (`requiredString`, `optionalString`,
  `optionalInt`, `optionalInt64`, `optionalBool`, `optionalDuration`, `oneOf`). Every reader
  records a problem instead of returning an error, and `err()` joins them.
- `config.go` — `Config` (`Env`, `LogLevel`, `HTTPAddr`) and `Load`, shared by every binary.
- `database.go` — `Database` (DSN, pool sizes, connection lifetime) and `LoadDatabase`.
- `httpserver.go` — `HTTPServer` (the five server timeouts) and `LoadHTTPServer`.
- `documents.go` — `Documents` (storage root, upload cap), `LoadDocuments`, and
  `DefaultDocumentStorageRoot`.
- `config_test.go` — unit tests for all four loaders. `isolate` blanks every variable any
  loader reads, so a developer's sourced `.env` cannot change a result.

## How it fits in

The bottom of the dependency graph: it imports nothing from this project, and everything
that needs settings imports it. Each binary calls the loaders it actually needs, so
`cmd/api` gets all four and a worker that touches no HTTP server does not get its timeouts.

## Gotchas

- **Loading is deliberately split by concern, not by binary.** `Load` is shared, so a
  required `MYSQL_DSN` in it would stop `ops-mcp` and the Incident Lab services from
  starting over a database none of them opens. Add a new field to the narrowest struct
  that needs it, or add a new `LoadX`.
- **Every loader collects all its problems.** Never return early on the first bad value:
  an operator with three variables wrong should learn that in one restart. A binary
  calling several loaders still gets each one's complete list.
- **A blank value is treated as unset.** `lookup` trims and reports `"   "` as absent,
  because a key left empty in `.env` is a common mistake and a default beats
  `HTTP_ADDR must be host:port, got ""`.
- **On error the loaders return the zero struct**, not a half-filled one — a caller that
  ignores the error must not get something that looks usable. Internally, a malformed
  value still yields the default so the remaining checks produce useful messages.
- **`String()` exists to keep secrets out of logs.** `Database.String` redacts the DSN.
  Adding a secret-bearing field means updating the relevant `String`; never `%+v` one of
  these structs at a call site.
- Some `env` helpers have no `Config` caller yet. They are tested directly on purpose, so
  the milestone that adds the first field using one does not also have to debug it.
