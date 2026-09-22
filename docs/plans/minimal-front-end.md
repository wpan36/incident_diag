# Minimal Front End

**Tier B spec (M28).** Short by design: ADR 0007 fixed the shape, S8 fixed the response
types, and S9 fixed how a timeline is assembled.

## Problem

Everything works and nothing can be watched without `curl`. The page exists so the system
can be used and demonstrated, not as a design exercise — ADR 0005 says the front end is
functional, not polished.

## Technical Plan

### One file, embedded, served by `cmd/api`

`web/index.html` holds the markup, the styles and the script. ADR 0007 removed the build
step; splitting into three files would not bring it back, but it would add relative paths to
get wrong for no gain at this size.

It is embedded with `go:embed` and served at `/`, following `migrations/embed.go`: a binary
carries its own assets. Serving it from `cmd/api` also makes the page same-origin with the
API, so there is no CORS configuration at all — which is the kind of incidental complexity
ADR 0007 was avoiding.

The cost is that editing the page needs a rebuild. `go run ./cmd/api` is the loop anyway.

### The timeline is fetched, then updated

S9 requires this and it is not an optimisation. A run that fails in its first seconds is
terminal before the browser subscribes, and `GET /api/runs/{id}/events` answers 410 with no
events at all. So:

1. `GET /api/runs/{id}` renders the whole timeline.
2. `EventSource` applies events as updates: `step.completed` replaces or appends by
   `step_number`, `run.started` clears the timeline, `run.finished` updates the run and
   closes the stream.
3. `onerror` with `readyState === CLOSED` re-fetches the run once and stops. It does not
   reconnect: a 410 means the run is over, and reconnecting would be the loop S9's 410 rule
   exists to prevent.

Deduplication is by `step_number` within the current attempt, which is why `run.started`
clears rather than merges — a restarted run re-uses 1..n (ADR 0009).

### Everything else

Documents are polled — `GET /api/documents` every three seconds while any is `PENDING` or
`PROCESSING`, not at all otherwise. Ingestion has no event stream and does not need one for
a list that changes twice a minute.

Server text is written with `textContent`, never `innerHTML`. The corpus is uploaded by the
user and tool output comes from the operational world; neither is trusted markup.

`GET /api/search` is not in the page. It exists to make retrieval inspectable from a
terminal, and the agent is the consumer that matters.

## Alternatives

**A separate static server, or opening the file directly.** Both put the page on a different
origin from the API, which means CORS — configuration in exchange for nothing.

**Reading the page from disk rather than embedding it.** Edits without a rebuild, at the cost
of the binary depending on its working directory. The rest of the project embeds.

**Rendering the timeline from events alone.** Simpler, and wrong for exactly the case S9
names: the run that finished before the browser arrived.

## Verification

- `make check` — the page is embedded, so a missing file is a build failure.
- Manual, which is the point of this milestone: upload a runbook and watch it reach `READY`;
  file an incident; start a run and watch the timeline fill; confirm the diagnosis and its
  citations render; reload mid-run and confirm the timeline is whole.
