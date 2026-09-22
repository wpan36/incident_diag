# web

## Purpose

The whole front end: one static page, no build step, no dependencies (ADR 0007).

## Contents

- `index.html` — markup, styles and script in one file.
- `embed.go` — `web.Page`, the embedded bytes `cmd/api` serves at `/`.

## How it fits in

`internal/api` serves `web.Page` from the API's own origin, which is why nothing in this
project configures CORS. It consumes `internal/wire`'s shapes through the REST endpoints and
the SSE stream; there is no generated client and no type sharing beyond reading the same
JSON.

## Gotchas

- **The timeline is fetched, then updated.** `GET /api/runs/{id}` renders it and events apply
  on top. A run that fails in its first seconds is terminal before the page can subscribe,
  and the events endpoint answers 410 with nothing to replay — so events are never the only
  source.
- **Every ending re-reads the run.** `reload` re-renders the timeline from
  `GET /api/runs/{id}` on `run.finished` as well as on `onerror`. A failed publish is logged
  and ignored (ADR 0001), so a dropped `step.completed` leaves a hole — and `run.finished`
  is the one ending that closes the stream itself, so without the re-read it is the one
  ending that would keep the hole.
- **Selecting an incident drops the run on screen.** `closeTimeline` clears it and closes
  the stream, because `loadRuns`'s "show the newest run" is gated on no run being selected:
  without it the previous incident's timeline stayed up, with its stream still open.
- **A finish step's heading is just `finish`.** Its `action` is the whole diagnosis — 3KB in
  a real run — which the card above already renders. Other actions have their arguments
  clipped to 160 characters.
- **`onerror` does not reconnect.** A closed stream means the run is over; `EventSource`
  would otherwise retry every three seconds against an endpoint that answers 410 forever.
  It re-reads the run once and stops.
- **`run.started` clears the timeline** rather than merging. A restarted run re-uses step
  numbers 1..n (ADR 0009), so merging would drop the new attempt's steps as duplicates.
- **`action` and `final_result` arrive as JSON values, not strings.** They are
  `json.RawMessage` on the Go side, so `JSON.parse` on them throws. `asObject` accepts
  either; without it every step displayed its `action_type` instead of its tool name, which
  is how this was found.
- **Server text goes in through `textContent`.** The corpus is uploaded by whoever runs this
  and tool output comes from the operational world; neither is trusted markup.
- **Editing the page needs a rebuild**, because it is embedded. `go run ./cmd/api` is the
  loop.
