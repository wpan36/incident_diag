# internal/mcpclient

## Purpose

Connects to `ops-mcp` and turns its results into the terms the audit trail records.

## Contents

- `mcpclient.go` — `Connect`, `Client`, `Tools`, `Call`, `Close`; `Tool`, `Result`, and the
  meta decoding.
- `mcpclient_test.go` — drives a real `opsmcp` server over `httptest`.

## How it fits in

The agent worker's only route to the operational world. It knows the meta envelope and
nothing about individual tools: names, schemas and descriptions are discovered at connect
time and handed to the model unchanged, so adding a tool to `ops-mcp` needs no change here.

## Gotchas

- **The tests run a real `opsmcp` server, not a fake.** What is worth testing is that the
  two halves of the meta envelope agree, and a fake would have been written from the same
  reading of the spec as the client.
- **`InputSchema` is carried as raw JSON.** It goes to the model unchanged; re-modelling a
  JSON Schema in Go only to marshal it back would be a second place for the two to disagree.
- **Tools are listed once, at connect.** `ops-mcp` registers them at startup and never
  changes them, so a tool added to a running server is not picked up until the agent worker
  restarts — which is the same deployment step that added it.
- **`Call` returns an error only when the call itself failed.** A tool that ran and reported
  a problem is a `Result` with a non-OK status, because that is something the agent reasons
  about rather than something that should end its run.
- **`Status` comes from structured content, never from the text.** The audit trail must not
  depend on parsing the prose the model reads.
- **A result whose meta will not decode still reaches the model**, with a warning logged and
  `OriginalBytes` measured locally. Inventing numbers for the audit row would be worse than
  saying the accounting is missing.
