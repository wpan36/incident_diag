# SSE Streaming

**Tier A spec (S9).** Gates M26. **M27 is cancelled** —
[ADR 0010](../adr/0010-no-answer-delta.md).

## Problem

S8 built the write side: three event types, one Redis stream per run, `XTRIM` on restart.
Nothing reads them. M26 is the reader, and the last piece between a working backend and
something a person can watch.

S7 deferred one question here by name — "M27 or S9 decides between streaming those deltas
and re-rendering the completed result". This spec answers it: neither.

## Technical Plan

### What S8 already fixed

Not re-decided here. `run:<runID>:events`; Redis's `<ms>-<seq>` serving as SSE's
`Last-Event-ID`; reading from `0` rather than `$`, because the window between
`GET /api/runs/{id}` and the subscribe would otherwise swallow an event; the three-field
entry (`event`, `run_id`, `data`) that lets the `event:` line be filled without decoding the
body; and `XTRIM MAXLEN 0` when a run restarts.

M26 adds the reader: `XRANGE` from the resume point to replay, then `XREAD BLOCK` to follow.

### `GET /api/runs/{id}/events`

```
404 unless the run exists              read first, so a mistyped id is an error
Content-Type: text/event-stream
X-Accel-Buffering: no                  proxies must not buffer this
replay  XRANGE key <from> +            <from> is Last-Event-ID+1, or 0
follow  XREAD BLOCK … STREAMS key <last>
close   after run.finished
```

Each SSE frame is `id:` (the Redis id), `event:` (the event name), `data:` (the payload
JSON) — the three fields S8 stored, mapped one to one.

### The write deadline is the concrete blocker

`cmd/api` sets `WriteTimeout`, 30s by default, and a run outlasts it. **The server would cut
its own stream.** Every write therefore extends the deadline through
`http.NewResponseController(w).SetWriteDeadline`.

Not by removing `WriteTimeout` from the server. `cmd/ops-mcp` could do that because it serves
only MCP; `cmd/api` also serves ordinary endpoints that should keep the protection, and one
long-lived route should not disarm the whole API.

### Lifecycle

`r.Context()` is cancelled when the client disconnects, and the reader returns on it. Nothing
else owns a goroutine here: the handler blocks in `XREAD` itself rather than handing the
stream to a writer goroutine, so there is one thing to stop and it is the request.

The roadmap asks for an explicit goroutine leak test. It belongs here, and it proves the
reader exits — not that Redis released the connection, which is a different failure.

### Termination and reconnection

Seeing `run.finished` sends it and closes. A client that connects after the run ended replays
to `run.finished` and closes immediately: the same path, no special case for a finished run.

A reconnecting client sends `Last-Event-ID`; the replay starts just after it, so no event is
delivered twice. A malformed `Last-Event-ID` is treated as absent and replays from `0`, which
is wasteful and correct — the browser already drops what it has by step number within the
current attempt (S8).

### Heartbeat

An SSE comment (`: ping`) every 20 seconds. `MaxRunDuration` is five minutes and a slow tool
call can leave tens of seconds without an event, which proxies and browsers cut. The interval
is a guess; nothing here has been measured against a real proxy.

### An expired stream

Past the 24-hour TTL the replay is empty and the connection closes. The client already has
the truth from `GET /api/runs/{id}`; synthesising events from MySQL would put the rendering
logic in two places.

### Configuration

`SSE_HEARTBEAT` (20s), `SSE_READ_BLOCK` (the `XREAD BLOCK` timeout, 5s — short enough that
cancellation is noticed promptly, long enough not to poll).

## Alternatives

**Streaming the `finish` tool call's argument deltas** — M27 as planned. Rejected in
[ADR 0010](../adr/0010-no-answer-delta.md).

**A writer goroutine fed by a channel.** The usual SSE shape, and it would be needed if the
handler had two sources to select over. It has one, so the goroutine would exist only to be
leaked.

**Reading from `$` and relying on the REST fetch for history.** Fewer bytes on the wire, and
S8 already rejected it: the window between the fetch and the subscribe silently loses
whatever is published inside it.

**Capping concurrent subscribers.** `XREAD BLOCK` holds a Redis connection per client, so an
unbounded endpoint is unbounded connections. Not added: this is a single-operator lab and a
cap would be a limit invented for a load nobody generates. Stated as a limitation rather than
implied away.

## Detailed Implementation

**M26 — `internal/events`** gains the read side: `Read(ctx, runID, from)` yielding events
until the context ends, built on `XRANGE` then `XREAD BLOCK`. The fake gains the same shape
so the handler's tests need no Redis.

**M26 — `internal/api`** gains `sse.go`: the handler, the frame writer, `Last-Event-ID`
parsing, the heartbeat ticker, and the deadline extension.

The event payloads are already `internal/wire` types (S8), so nothing new is rendered here.

## Verification

- `make check` — frame formatting; `Last-Event-ID` parsing including a malformed one; the
  heartbeat ticker; a goroutine leak test that disconnects mid-stream.
- `make test-integration` — a client connected before the run sees every event in order; one
  connecting after it finishes replays and closes; a reconnect with `Last-Event-ID` resumes
  without duplicates; a restarted run's stream begins again.
- End to end: start a run and watch `curl -N` print the timeline as it happens.

## Known limitations, accepted

**No cap on concurrent subscribers**, and each holds a Redis connection while blocked.

**The heartbeat interval is unmeasured**, like the S6 limits.

**The leak test proves the reader exits, not that Redis released the connection.**

**The last five to fifteen seconds of a run are silent** — the `finish` call — and then the
diagnosis appears whole. ADR 0010.
