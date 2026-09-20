# internal/log

## Purpose

Builds the structured logger and carries correlation identifiers through `context`, so
that an investigation spanning an HTTP request, a Kafka message, an agent run and several
tool calls in different processes can be reassembled from the logs afterwards.

## Contents

- `log.go` — `New` (a JSON `*slog.Logger` wrapped in `contextHandler`), `Discard` for
  tests, the `WithRequestID`/`RequestID` and `WithRunID`/`RunID` context pairs, the
  `KeyRequestID`/`KeyRunID` attribute keys, and the `contextHandler` itself.
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
- `KeyRequestID` and `KeyRunID` are the strings queries in a log backend are written
  against. They are not free to rename.
