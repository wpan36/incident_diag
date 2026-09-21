# internal/log

## Purpose

Builds the structured logger and carries correlation identifiers through `context`, so
that an investigation spanning an HTTP request, a Kafka message, an agent run and several
tool calls in different processes can be reassembled from the logs afterwards.

## Contents

- `log.go` — `New` (a JSON `*slog.Logger` wrapped in `contextHandler`), `Discard` for
  tests, the `WithRequestID`/`RequestID` and `WithRunID`/`RunID` context pairs, the
  `KeyRequestID`/`KeyRunID`/`KeyService` attribute keys, the `utcMillisecondTime`
  replacement, and the `contextHandler` itself.
- `log_test.go` — unit tests, including the grouping and attribute-isolation cases below.

## How it fits in

Imported by every binary and by anything that logs. It depends only on the standard
library, which keeps it usable from the lowest layers without an import cycle.

## Gotchas

- **Identifiers are attached by the handler, not at call sites.** Adding them by hand
  would mean relying on nobody ever forgetting. Put the id on the context once, at the
  edge, and every record below it carries the id.
- **`contextHandler` replays its `WithAttrs`/`WithGroup` chain for a reason.** A handler
  with an open group would otherwise nest the identifiers as
  `{"detail":{"request_id":...}}` and break every query written against a top-level
  `request_id`. When a group is in play it applies the identifiers to the original
  ungrouped handler and replays the chain on top. Changing this code without a test for
  the grouped case is how that regresses silently.
- **`appendOp` copies before appending.** Handlers derived from a common parent must not
  share backing storage, or one logger's attributes leak into another's.
- **JSON in every environment**, including local development. Pretty console output would
  mean debugging a problem in one format and reading it in another.
- **The record format is a contract, not this package's preference.**
  `docs/plans/mcp-tool-boundary.md` fixes it, because `read_service_logs` parses it: a UTC
  RFC 3339 timestamp at millisecond precision, `level`, `msg`, `service`, and free-form
  attributes. `KeyRequestID`, `KeyRunID` and `KeyService` are the strings queries are
  written against, and none of them is free to rename.
- **`service` is a parameter of `New` rather than something a binary adds with `With`.** A
  required argument cannot be forgotten. An empty string omits the attribute, which is what
  the package's own tests use.
- **Timestamps are forced to UTC and truncated to milliseconds** in `ReplaceAttr`.
  Containers do not agree on a time zone, so a `read_service_logs` time window spanning two
  services would otherwise be silently wrong.
