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
`Last-Event-ID`; reading from the start of the stream rather than from `$`, because the
window between `GET /api/runs/{id}` and the subscribe would otherwise swallow an event; the
three-field entry (`event`, `run_id`, `data`) that lets the `event:` line be filled without
decoding the body; and `XTRIM MAXLEN 0` when a run restarts.

M26 adds the reader: `XRANGE` to replay, then `XREAD BLOCK` to follow.

### `GET /api/runs/{id}/events`

```
404                       the run does not exist       read it first
410                       the run is already terminal  see below
200 text/event-stream     otherwise
  Cache-Control: no-cache
  X-Accel-Buffering: no   a proxy must not buffer this
  replay  XRANGE key (<last-event-id> +    or  - +
  follow  XREAD BLOCK <SSE_READ_BLOCK> STREAMS key <last>
  close   on run.finished, on a run found terminal, or on disconnect
```

Each SSE frame is `id:` (the Redis id), `event:` (the event name), `data:` (the payload
JSON), then a blank line — the three fields S8 stored, mapped one to one. The payload is
`json.Marshal` output, which escapes newlines, so a frame is always a single `data:` line.

### A terminal run is answered 410, never streamed

A browser's `EventSource` reconnects about three seconds after the server closes a stream.
Replaying a finished run and closing would therefore loop forever — replay, close,
reconnect — and nothing on the server can break the loop. A non-200 response is the one
thing that does: `EventSource` fails the connection, fires `onerror` with
`readyState === CLOSED`, and stops.

The handler reads the run for its 404 anyway, so a terminal run gets 410 instead of a
stream. One rule covers three cases:

- **The run finished before the client subscribed.** It already has the whole timeline from
  `GET /api/runs/{id}`, which S8 requires it to fetch before subscribing. This is not a rare
  path: a run that fails in its first seconds — an unreachable tool server, say — is often
  terminal before the browser has subscribed, so **M28 must render the timeline from the
  fetch and treat events as updates to it**, not as the only source.
- **The stream has expired.** Past the 24-hour TTL there is nothing to replay, and
  synthesising events from MySQL would put the rendering logic in two places.
- **`run.finished` was never published.** S8 logs and ignores a failed publish by design, so
  a terminal run's stream may hold no terminal event. Waiting for one would block until the
  client gave up.

It also makes the normal case self-limiting: a run that finishes while a client is watching
sends `run.finished` and the handler closes; the browser reconnects once; that reconnect
gets 410 and stops. The front end calling `close()` on `run.finished` becomes an
optimisation rather than a correctness requirement.

The cost is a race. A run that finishes between the client's `GET /api/runs/{id}` and its
subscribe answers 410, and the client has to re-fetch to see the result — one extra request
on a path it already uses.

**410 follows `errTooLarge`'s pattern**: an `errStreamGone` sentinel wrapped by
`httpx.NotFoundErr`, turned into 410 in `renderError`. The kind stays `KindNotFound` — the
stream genuinely is not there — so the body's `code` is `not_found` and the status is what
separates "this run is over" from "no such run". 413 set this precedent: the exception is
stated once in `renderError` rather than as a sixth `httpx.Kind`.

### The write deadline is the concrete blocker

`cmd/api` sets `WriteTimeout`, 30s by default, and a run outlasts it. **The server would cut
its own stream.** Every write therefore extends the deadline through
`http.NewResponseController(c.Writer).SetWriteDeadline`, and the same controller's `Flush`
is called after every frame — without it gin buffers the response and nothing reaches the
browser until the handler returns, which is the failure this endpoint exists to prevent.

`ResponseController` reaches the real `http.ResponseWriter` because gin's own writer
implements `Unwrap()`. This is true of the pinned gin v1.12.0 and was not always true, so
it is a version assumption worth naming.

The deadline is extended to `now + 2 × SSE_HEARTBEAT`. The gap between two writes is bounded
by the heartbeat, so twice it states the invariant directly; borrowing
`HTTPServer.WriteTimeout` would mean plumbing a value meant for ordinary responses into this
package for a deadline that has nothing to do with it.

Not by removing `WriteTimeout` from the server. `cmd/ops-mcp` could do that because it serves
only MCP; `cmd/api` also serves ordinary endpoints that should keep the protection, and one
long-lived route should not disarm the whole API.

### The loop

```
run := GetRun(id)                        404 if missing, 410 if terminal
write the headers, flush
after := Last-Event-ID                   "" when absent or malformed

send := func(ev) error {                 one frame
    extend the deadline; write id:/event:/data:; flush
    after, lastWrite = ev.ID, now
    if ev.Name == run.finished { return errStreamDone }
    return nil
}

Replay(ctx, id, after, send)             any error, errStreamDone included, ends the request
for ctx.Err() == nil {
    Follow(ctx, id, after, send)         blocks up to SSE_READ_BLOCK
    if send wrote anything { continue }  an event is its own keepalive
    if GetRun(id) is terminal { return } the publish was dropped
    if now-lastWrite >= SSE_HEARTBEAT { write ": ping"; flush; lastWrite = now }
}
```

**The heartbeat is a timestamp, not a ticker.** The handler is blocked in `XREAD` most of
the time, so a `time.Ticker` would tick while it is blocked and then fire immediately on
return. Comparing against the last byte written is exact, and a busy run sends no pings at
all.

**The terminal re-read runs on every idle round** — one `GetRun` per client per
`SSE_READ_BLOCK`. That is the price of S8's "a failed publish is logged and ignored": the
stream cannot be trusted to announce the end, so MySQL is asked. At five seconds and a
handful of viewers it is noise beside the run itself.

`SSE_READ_BLOCK` must stay well below `SSE_HEARTBEAT`, since the heartbeat is only checked
between blocks. `LoadEvents` enforces it, the way `LoadReconcile` enforces
`INGEST_DOCUMENT_TIMEOUT < INGEST_LEASE`.

### Lifecycle

`r.Context()` is cancelled when the client disconnects, and the loop returns on it. Nothing
else owns a goroutine here: the handler blocks in `XREAD` itself rather than handing the
stream to a writer goroutine, so there is one thing to stop and it is the request.

The roadmap asks for an explicit goroutine leak test. It belongs here, and it proves the
handler returns — not that Redis released the connection, which is a different failure.

### Reconnection

A reconnecting client sends `Last-Event-ID`, and the replay is an **exclusive** range,
`XRANGE key (<id> +` (Redis 6.2 and later; compose pins `redis:8.2-alpine`). No event is
delivered twice and no sequence number is incremented by hand.

A `Last-Event-ID` that does not match `^\d+-\d+$` is treated as absent and replays from the
start, which is wasteful and correct — the browser already drops what it has by step number
within the current attempt (S8).

**When the replay yields nothing, the follow starts from `0`, never `$`.** An empty replay
means the stream is empty, not that it is up to date, and an event published between the
`XRANGE` and the `XREAD` would be lost — the window S8 rejected `$` for.

A client reconnecting across a restart resumes after an id the `XTRIM` left behind, and the
new attempt's entries sort after it, so it receives the new `run.started` and clears its
timeline. That is S8's mechanism working, not a case M26 handles.

### A Redis failure mid-stream

The headers are already sent, so there is no status code left. The handler logs the error
and closes the stream; the client reconnects, and either succeeds or takes its answer from
`GET /api/runs/{id}`. No synthetic error event: the vocabulary is the three names S8 fixed
(ADR 0010), and a fourth would have to be rendered by a front end that has a better source
for the same fact.

`redis.Nil` from a blocked `XREAD` is the block expiring, not a failure. It is the loop's
normal idle path.

### Configuration

Two new variables on `config.Events`, which already holds everything about these streams.
`cmd/agent-worker` loads and ignores them; a fifteenth loader for two values one binary
reads is the more expensive answer.

| Variable | Default | |
| --- | --- | --- |
| `SSE_HEARTBEAT` | `20s` | a comment frame when nothing else has been written for this long |
| `SSE_READ_BLOCK` | `5s` | one `XREAD BLOCK`; short enough to notice cancellation, long enough not to poll |

`LoadEvents` fails unless `5s <= SSE_HEARTBEAT` and `0 < SSE_READ_BLOCK < SSE_HEARTBEAT`.
The floor is there because the write deadline is derived from the heartbeat: a one-second
heartbeat would give a two-second deadline, and a slow client would be cut mid-frame.

`REDIS_PUBLISH_TIMEOUT` bounds the read calls too — it is this package's one "a Redis round
trip should not take longer than this" knob — and `Follow` uses
`SSE_READ_BLOCK + REDIS_PUBLISH_TIMEOUT`, so the context outlives the block it asked for.

## Alternatives

**Streaming the `finish` tool call's argument deltas** — M27 as planned. Rejected in
[ADR 0010](../adr/0010-no-answer-delta.md).

**Replaying a terminal run and closing, which this spec's first draft specified.** One path
for every client and no 410. It loops: the browser reconnects after every close, replays,
closes again, and only the page calling `close()` can stop it — which makes correctness
depend on the front end, and leaves the expired-stream case with no event to act on. S9's
goldfish test found this.

**A writer goroutine fed by a channel.** The usual SSE shape, and it would be needed if the
handler had two sources to select over. It has one, so the goroutine would exist only to be
leaked.

**Clearing the write deadline for this connection** (`SetWriteDeadline(time.Time{})`) rather
than extending it per write. One call instead of one per frame, and it is the targeted
version of removing `WriteTimeout`. Rejected: a client that stops reading would then block
the handler in `Write` forever, which is the leak the deadline prevents.

**Reading from `$` and relying on the REST fetch for history.** Fewer bytes on the wire, and
S8 already rejected it: the window between the fetch and the subscribe silently loses
whatever is published inside it.

**Synthesising `run.finished` from MySQL for an expired stream.** It would let one path
serve every case, at the cost of rendering one event from two sources — the divergence
ADR 0010 rejected the second LLM call for.

**Capping concurrent subscribers.** `XREAD BLOCK` holds a Redis connection per client, so an
unbounded endpoint is unbounded subscribers. Not added: this is a single-operator lab and a
cap would be a limit invented for a load nobody generates. Stated as a limitation instead.

## Detailed Implementation

### `internal/events` — the read side

```go
// ReadEvent is one entry read back from a run's stream.
type ReadEvent struct {
	ID   string          // Redis's <ms>-<seq>, which is SSE's Last-Event-ID
	Name string          // one of the three event names
	Data json.RawMessage // the payload, still encoded
}

// Reader is what the SSE endpoint reads a run's stream through.
type Reader interface {
	// Replay yields every entry after `after`, oldest first, and returns when
	// the stream's current end is reached. An empty `after` starts at the
	// beginning.
	Replay(ctx context.Context, runID, after string, fn func(ReadEvent) error) error

	// Follow blocks for at most SSE_READ_BLOCK and yields whatever arrived,
	// returning nil having yielded nothing when the block expires.
	Follow(ctx context.Context, runID, after string, fn func(ReadEvent) error) error
}
```

- **`ReadEvent` is not `Event`.** `Event` is the write side's shape — a name and a
  `Payload any` — and carries no id on purpose. What comes back has an id and undecoded
  bytes.
- **A callback rather than a channel or an `iter.Seq2`.** A channel needs a goroutine, which
  this design does not have; an error returned by `fn` stops the iteration and comes back
  out unchanged, which is how `errStreamDone` ends the request.
- **Two methods, not one.** The handler has to act in the gaps between events — the
  heartbeat and the terminal re-read — so the blocking has to end in the handler's loop, not
  inside the reader's.
- **`Reader` is a second interface, not three more methods on `Publisher`.**
  `FakePublisher` exists so `internal/agentrun` can prove that a failed publish does not
  fail a run; making it implement a read it never calls would be a method with nothing to
  mean.
- **Cancellation is not an error.** When `ctx` ends, both return nil. A client disconnecting
  is this endpoint's normal ending, and making every caller filter `context.Canceled` out of
  its logging is a trap.

Both methods are implemented by the existing `*Client` and both use `StreamKey`.
`FakeReader` joins `FakePublisher` in `fake.go`: a slice of `ReadEvent`, an `Err` every call
returns, and a `Block` duration `Follow` waits before yielding nothing — so `internal/api`'s
tests need no Redis and still run in milliseconds.

### `internal/api` — `sse.go`

`streamRunEvents` on `*Server`, routed as `api.GET("/runs/:id/events", s.streamRunEvents)`.
It holds the loop above, the frame writer, `Last-Event-ID` parsing, the deadline extension
and the flush. `errStreamGone` and its 410 go in `errors.go` beside `errTooLarge`;
`errStreamDone` is local to `sse.go` and never reaches a client.

`Deps` gains `EventReader events.Reader` and `Events config.Events`, mirroring how it
already carries `Agent config.Agent` for a value the handlers need rather than re-read.

### `internal/config`

`Events` gains `Heartbeat` and `ReadBlock`; `LoadEvents` reads them, validates the ordering
above, and `Events.String` prints both.

### `cmd/api`

Calls `LoadEvents` and `events.New`, passes the client as `EventReader`, and closes it in
the shutdown sequence beside the other clients. **`/readyz` does not gain a Redis check** —
`internal/api`'s position is that a brief dependency outage should not get the process
restarted, and a Redis that is down costs a live timeline, not the API.

## Verification

- `make check`, against `FakeReader` — frame formatting, including a payload containing a
  newline; `Last-Event-ID` parsing, including a malformed one; the heartbeat firing only
  after silence; a terminal run answering 410 without opening a stream; a run whose stream
  never received `run.finished` closing on the terminal re-read; a goroutine leak test that
  disconnects mid-stream.
- `make test-integration`, against real Redis — a client connected before the run sees every
  event in order; a reconnect with `Last-Event-ID` resumes without duplicates; an empty
  replay followed by a publish delivers that publish; a restarted run's stream begins again.
- End to end: start a run and watch `curl -N` print the timeline as it happens, then re-issue
  the same `curl` and get 410.

## Known limitations, accepted

**No cap on concurrent subscribers.** Each holds a Redis connection while blocked, and
go-redis's pool is bounded (ten per CPU by default), so the failure is not unbounded
connections but the first subscriber past the pool waiting on it silently.

**A run stuck `RUNNING` streams heartbeats until the client disconnects.** S8 leaves a run
whose attempts are exhausted `RUNNING` for a human; past the 24-hour TTL its stream is empty
and the terminal re-read never fires. It cannot happen to a healthy run — `MaxRunDuration`
is five minutes and the TTL is refreshed on every publish.

**A Redis outage becomes a reconnect poll.** A non-terminal run answers 200, the replay then
fails, the stream closes, and `EventSource` reconnects about three seconds later — each cycle
costing one `GetRun` and one `XRANGE`. The 410 rule bounds the loop for a *finished* run but
not for a live one whose Redis is down. Harmless for one viewer in a lab; it is a poll, and
it is not stated anywhere else.

**The heartbeat interval is unmeasured**, like the S6 limits.

**The leak test proves the handler returns, not that Redis released the connection.**

**The last five to fifteen seconds of a run are silent** — the `finish` call — and then the
diagnosis appears whole. ADR 0010.
