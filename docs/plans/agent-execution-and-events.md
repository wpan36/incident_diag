# Agent Execution and Events

**Tier A spec (S8).** Gates M24 (event bus) and M25 (agent-worker). It also owns the run
endpoints, which no earlier spec defined.

## Problem

The loop exists and is tested; nothing runs it. M25 supplies `agent.ReportStep` — the one
seam through which the agent package reaches a database or a broker — together with the
worker that consumes `agent.runs.v1` and the endpoints that start and read a run.

S1's API surface stops at documents, so `POST /api/incidents/{id}/runs` and
`GET /api/runs/{id}` are specified here or nowhere.

## Technical Plan

### Three event types, not nine

The original brief listed nine — `retrieval.started`, `plan.created`, `tool.started` and so
on. The only consumer is SSE feeding a browser timeline, and a timeline needs three things:
the run started, what each step did and saw, the run ended.

| Event | When |
| --- | --- |
| `run.started` | after the claim succeeds and the previous attempt's rows are gone |
| `step.completed` | after the step's row is written |
| `run.finished` | after the terminal row is written |

The other six are sub-step granularity the UI would render as a single line anyway.

**The cost, stated rather than left to be discovered:** `ReportStep` is called *after* a
step completes, so the browser shows nothing while one runs — an LLM call plus a tool call,
roughly four seconds. Closing that gap means another hook on the agent package's seam, which
has just been mean-reviewed and is clean. Changing a just-reviewed interface for a latency
nobody has measured is the wrong trade; M28 will have a UI to judge it with, and adding a
`step.started` event then is additive.

**The event payload reuses the API's wire types.** A `step.completed` event carries exactly
the JSON `GET /api/runs/{id}` renders for that step, so the front end has one `Step` type
rather than two that have to agree.

The envelope is the event name, the run id, and the payload. Redis supplies the id and the
timestamp, so neither is invented here.

### The row first, then the event

ADR 0001 says nothing authoritative may exist only in Redis. Concretely:

**The MySQL row is written before its event is published, and a failed publish is logged and
ignored.**

A reconnecting client reads `GET /api/runs/{id}` and then subscribes, so it can never see an
event for something that is not yet persisted. A lost event costs a live timeline, never a
fact — which is exactly the division of labour ADR 0001 chose Redis for.

An error from `ReportStep` still ends the run `FAILED`, because that means the *row* could
not be written; the agent package already documents this. A publish failure is not such an
error.

### Streams

One stream per run, `run:<runID>:events`, written with `XADD` under `MAXLEN ~ 1000` and a
24-hour TTL refreshed on each publish.

Redis assigns each entry a `<ms>-<seq>` id, and that **is** SSE's `Last-Event-ID`. M26 needs
no cursor of its own, and reconnection is `XRANGE` from the last id the client saw.

### The endpoints

| | |
| --- | --- |
| `POST /api/incidents/{id}/runs` | 201 with the run; **409 when one is already in flight** |
| `GET /api/runs/{id}` | the run, its steps with their tool calls, and its evidence, inline |
| `GET /api/incidents/{id}/runs` | that incident's runs, cursor paginated |

The 409 comes from `uniq_active_run` rejecting the insert, not from a read-then-write check
that two requests could interleave. S1 built the generated column for this.

`GET /api/runs/{id}` is not paginated. `MaxSteps` bounds the step count at single digits, and
a timeline is read whole or not at all. `ListRunsByIncident` already exists in the store,
written in M3 and unused until now.

**The budget is not overridable per request.** S7 ties an invariant to configuration
loading — `MaxSteps` × the 8 KiB observation cap must fit the token ceiling, which is what
makes "the context is never pruned" true. A request that set its own `max_steps` would give
that invariant a second enforcement point, and a run that violated it would break the
no-pruning guarantee silently.

### The worker

```
consume agent.runs.v1
  claim the run, with the lease                    S2
  delete the previous attempt's rows               ADR 0009
  publish run.started
  agent.Run, with ReportStep writing the row then publishing
  FinishRun with the outcome; publish run.finished
```

Deleting the previous attempt is one statement. `agent_steps` cascades from `agent_runs`,
and `tool_calls` and `evidence` cascade from `agent_steps`, so
`DELETE FROM agent_steps WHERE run_id = ?` takes the children with it.

**The worker hosts a run reconciler.** S2 built one for documents on the principle that the
process consuming the work is the one that should look for work that never arrived. Runs
need the same three categories — never enqueued, abandoned, retryable failure — and not
optionally: a worker that dies leaves a run `RUNNING`, and `uniq_active_run` then makes that
incident reject every new run forever.

### Configuration

`REDIS_URL` (required), `RUN_LEASE`, `RUN_MAX_ATTEMPTS`, `EVENT_STREAM_MAXLEN`,
`EVENT_STREAM_TTL`. The reconciler's interval, batch size and pending window are already
configured and are shared with the document sweep.

`RUN_LEASE` must exceed `AGENT_MAX_RUN_DURATION`, or a run still working would be reclaimed
underneath itself. Configuration loading checks it, the way S7 checks its own invariant.

## Alternatives

**Nine event types, as the brief described.** Finer-grained, and it would remove the
four-second gap. Rejected for the reason above; revisit once M28 can show whether the gap
matters.

**Publishing inside the same transaction as the row.** Not possible — Redis is not in the
MySQL transaction — and wanting it is the reason ADR 0001 separates the two in the first
place.

**Paginating the steps in `GET /api/runs/{id}`.** Consistent with the other listings, but
`MaxSteps` already bounds the count and a partial timeline is not useful.

**Letting a request set the budget.** Convenient for M32's evaluation, which wants to compare
budgets. Rejected above; M32 can vary the configuration instead.

**One stream for all runs, filtered per client.** Fewer keys, but every SSE client would read
every run's events and discard most of them, and a single stream's `MAXLEN` would evict a
quiet run's history because a busy one filled it.

## Detailed Implementation

**M24 — `internal/events`**

The envelope, `Publisher` with `Publish(ctx, runID, event)`, stream naming, `MAXLEN` and TTL
handling, and a fake for tests. Redis added to `deploy/docker-compose.yml`. The reader is
M26's; this milestone writes.

**M25 — `cmd/agent-worker` and the endpoints**

The consumer, the `ReportStep` implementation that writes the row then publishes, the run
reconciler, and the three endpoints in `internal/api`.

`internal/store` gains `DeleteRunSteps` and `ListRunsToReconcile`, mirroring the document
methods S2 added.

## Verification

- `make check` — the envelope, stream key naming, the reconciler's categories for runs, and
  that `RUN_LEASE` below `AGENT_MAX_RUN_DURATION` fails configuration loading.
- `make test-integration` — an event is published only after its row exists; a failed publish
  does not fail the run; a redelivered run message does not start a second run; a reclaimed
  run deletes the previous attempt's steps; a second `POST` while one is in flight is 409;
  `GET /api/runs/{id}` renders a full timeline.
- End to end: file an incident, start a run, and watch the rows and the stream fill.

## Known limitations, accepted

**The timeline is silent for the duration of a step**, around four seconds. See above.

**`GET /api/runs/{id}` returning everything is safe only because `MaxSteps` is small.** This
is the second design to depend quietly on that invariant — S7's context builder is the
first — and the check that enforces it runs only at configuration load.

**A failed publish is invisible to the user**, who sees a timeline that simply stops. The row
is correct and a refresh fixes it, but nothing tells them to refresh.

**The reconciler's surface doubles** for a failure that only occurs when a worker crashes. It
is still necessary: the alternative is a permanently blocked incident.
