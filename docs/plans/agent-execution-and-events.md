# Agent Execution and Events

**Tier A spec (S8).** Gates M24 (event bus) and M25 (agent-worker). It also owns the run
endpoints and the agent worker's configuration, which no earlier spec defined.

## Problem

The loop exists and is tested; nothing runs it. M25 supplies `agent.ReportStep` — the one
seam through which the agent package reaches a database or a broker — together with the
worker that consumes `agent.runs.v1` and the endpoints that start and read a run.

S1's API surface stops at documents, so `POST /api/incidents/{id}/runs` and
`GET /api/runs/{id}` are specified here or nowhere. S2 deferred the run lease, the agent
worker's rebalance timeout and the runs half of the reconciler to M25; S7 deferred the
budget the creating endpoint records. They land here.

## Technical Plan

### Three event types, not nine

The original brief listed nine — `retrieval.started`, `plan.created`, `tool.started` and so
on. The only consumer is SSE feeding a browser timeline, and a timeline needs three things:
the run started, what each step did and saw, the run ended.

| Event | When | Payload |
| --- | --- | --- |
| `run.started` | after the claim succeeds and the previous attempt is cleared | `Run` |
| `step.completed` | after the step's row is written | `Step` |
| `run.finished` | after the terminal row is written | `Run` |

The other six are sub-step granularity the UI would render as a single line anyway.

**`run.started` means "clear the timeline".** A restarted run re-uses step numbers 1..n, so
a client that drops duplicates by step number would discard the new attempt's steps as
copies of the old one's — the opposite of what ADR 0009 promises. Dropping by step number
is therefore scoped to the current attempt, and `run.started` ends the previous one.
`Run.attempts` says which attempt has begun.

**The cost, stated rather than left to be discovered:** `ReportStep` is called *after* a
step completes, so the browser shows nothing while one runs — an LLM call plus a tool call,
roughly four seconds. Closing that gap means another hook on the agent package's seam, which
has just been mean-reviewed and is clean. Changing a just-reviewed interface for a latency
nobody has measured is the wrong trade; M28 will have a UI to judge it with, and adding a
`step.started` event then is additive. M27's `answer.delta` is additive in the same way and
is S9's to define; this spec fixes the vocabulary M24 ships, not the vocabulary forever.

**The event payload reuses the API's wire types**, so the front end has one `Step` type
rather than two that have to agree. `Run` and `Step` are defined under *Response shapes*
below.

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

**Every publish carries its own timeout** (`REDIS_PUBLISH_TIMEOUT`, derived from the run's
context rather than replacing it). Without one, an unresponsive Redis would block a run
inside the call whose whole point is that its failure does not matter.

`run.finished` is published only when `FinishRun` reports that it wrote the row. A `false`
return means another attempt already finished this run, and its `run.finished` was published
then.

**The worker re-reads the run to build `run.started` and `run.finished`.** `ClaimRun` and
`FinishRun` return `(bool, error)` and not the row — a convention `internal/store` calls
load-bearing, since `false` is a legal outcome rather than a failure — so each payload comes
from a `GetRun` after the transition. Publishing the row as it was read *before* the claim
would put `PENDING`, a stale `attempts` and a null `started_at` into `run.started`.

### Streams

One stream per run, `run:<runID>:events`, written with `XADD` under `MAXLEN ~ 1000` and a
24-hour TTL refreshed on each publish. Each entry has three fields: `event`, `run_id` and
`data`, the last holding the payload JSON. The reader is M26's; splitting the name out of
the JSON is what lets it fill SSE's `event:` line without decoding the body.

Redis assigns each entry a `<ms>-<seq>` id, and that **is** SSE's `Last-Event-ID`. M26 needs
no cursor of its own, and reconnection is `XRANGE` from the last id the client saw.

**A restarted run empties its stream** (`XTRIM <key> MAXLEN 0`) at the same point the claim
clears its rows, so the stream holds exactly the current attempt. ADR 0009 promises that a
restarted timeline begins again rather than showing a spliced history, and a stream keeping
the previous attempt's `step.completed` entries would break that promise for any client
resuming from a `Last-Event-ID` older than the crash. `XTRIM` rather than `DEL` because
Redis keeps a trimmed stream's last generated id: the new attempt's entries are then
guaranteed to sort after the old ones, and the key keeps its TTL.

**A client reads the stream from `0`, not from `$`.** The window between
`GET /api/runs/{id}` and the subscribe would otherwise swallow any event published inside
it. `MAXLEN 1000` against a single-digit `MaxSteps` means the whole of a run's history is
always present, so replaying it costs nothing and the browser drops what it already has by
step number within the current attempt (see `run.started` above). This is why the detail
response carries no stream cursor. `Last-Event-ID` is for reconnection only.

### The endpoints

| | |
| --- | --- |
| `POST /api/incidents/{id}/runs` | 201 with the run; **409 when one is already in flight**; 404 when the incident does not exist |
| `GET /api/runs/{id}` | the run, its steps with their tool calls, and its evidence, inline |
| `GET /api/incidents/{id}/runs` | that incident's runs, cursor paginated; 404 when the incident does not exist |

The 409 comes from `uniq_active_run` rejecting the insert, not from a read-then-write check
that two requests could interleave. S1 built the generated column for this.

The 404 comes from reading the incident first. A run against a missing incident violates
`fk_agent_runs_incident`, and `dbError` does not classify a foreign-key failure as
not-found, so without the read the client gets a 500 for its own mistake. Sniffing the
constraint name instead would spread the technique `store` deliberately keeps to
`uniq_active_run`. The listing reads the incident for a smaller reason: an empty page and a
mistyped id must not look the same.

**The request body is empty**, and the budget is not overridable. S7 ties an invariant to
configuration loading — `MaxSteps` × the 8 KiB observation cap must fit the token ceiling,
which is what makes "the context is never pruned" true. A request that set its own
`max_steps` would give that invariant a second enforcement point, and a run that violated it
would break the no-pruning guarantee silently.

**The endpoint records the budget and the model on the row**, as S7 requires, so a run stays
interpretable after the configuration changes. `cmd/api` therefore calls `LoadAgent` and
`LoadLLM`, which adds `AGENT_*` and `LLM_MODEL` to what the API process needs.

**The worker takes the budget from the row and the model from its own configuration.** The
budget is why the row records it: it becomes the `agent.Budget` passed into `agent.Run`, so
a run keeps the bounds it was created with even if the environment has moved on. The model
cannot work the same way — `llm.Client` holds its model and `Chat` takes no override — so
`agent_runs.model` records what the API believed rather than instructing the worker. Both
processes read one `.env`, which is what makes the two agree; it is an assumption, not an
enforced invariant, and it is also why the lease check below may be made against the
worker's own `AGENT_MAX_RUN_DURATION`.

**It then produces to `agent.runs.v1`, keyed by the run id, and answers 201 whether or not
the produce succeeded**, logging a failure at error level. This is the same trade
`POST /api/documents` makes and for the same reason: the run genuinely was created, so 500
would be a lie and a client retrying on it would hit 409 from its own first attempt. The row
is `PENDING`, which is what the reconciler's never-enqueued category sweeps for.

`GET /api/runs/{id}` is not paginated. `MaxSteps` bounds the step count at single digits, and
a timeline is read whole or not at all. `ListRunsByIncident` already exists in the store,
written in M3 and unused until now.

### Response shapes

| Type | Holds |
| --- | --- |
| `Run` | `id`, `incident_id`, `status`, `stop_reason`, the four budget columns, `model`, the four counters, `final_result`, `error`, `attempts`, `started_at`, `finished_at`, `created_at`, `updated_at` |
| `Step` | `id`, `run_id`, `step_number`, `action_type`, `action`, `observation_summary`, `observation_bytes`, `truncated`, `status`, `error`, `duration_ms`, `created_at`, plus `tool_call` (one object or `null`) and `evidence` (an array, empty except on a `finish` step) |
| `ToolCall` | `id`, `step_id`, `tool_name`, `arguments`, `status`, `result_summary`, `result_bytes`, `truncated`, `error`, `duration_ms`, `created_at` |
| `Evidence` | `id`, `step_id`, `tool_call_id`, `source_type`, `source_ref`, `document_id`, `summary`, `note`, `created_at` |
| `RunDetail` | a `Run` plus `steps` |

Both listings and `POST` return `Run`; `GET /api/runs/{id}` returns `RunDetail`.

**Five types, not three**: `tool_call` and `evidence` are nested objects, so they are wire
types too and M28 generates an interface for each. Neither carries `run_id` — it is the run
being rendered. `internal/api`'s conventions hold unchanged: no `omitempty` on a nullable
field, so every key is present in every response, and every timestamp goes through `Time`.

**A `RUNNING` run's counters read zero.** `ClaimRun` resets them and only `FinishRun` writes
them, so `step_count`, `tool_call_count` and the two token counts mean something on a
terminal run and nowhere else. A client that wants a live count counts `step.completed`.
Incrementing the row per step would be a second write per step for a number the steps
already carry.

**This is the one place the API has two shapes for one entity**, against the convention in
`internal/api/CLAUDE.md`. A listing that carried every run's whole timeline is the
alternative, and it is worse. The exception is named here rather than discovered in review.

A step has at most one tool call, so nesting it is the schema's own shape rather than a
join flattened by hand. Evidence nests under the step it cites — `evidence.step_id` is not
nullable — which is also where a timeline renders a citation. The consequence is that
`step.completed` for a `finish` step carries evidence pointing at earlier steps, and the
front end attaches each row to the step its `step_id` names.

### Packages

| Package | |
| --- | --- |
| `internal/events` | M24: the envelope, `Publisher`, stream naming, `MAXLEN`, TTL, `Trim`, and a fake |
| `internal/wire` | the five response types above and `Time`, moved out of `internal/api` |
| `internal/agentrun` | M25: the Kafka handler, the `ReportStep` implementation, and the worker's dependencies, mirroring `internal/ingest` |
| `cmd/agent-worker` | assembly and lifecycle only |

`internal/wire` exists because the event payload is the API's wire type and the worker
publishes it. `internal/api` is the top of the graph — nothing but `cmd/api` depends on it —
and having the worker import it would invert that and link gin into a binary that serves no
HTTP. `wire` depends on `store` and `time` and nothing else. `Incident` and `Document` stay
in `internal/api` for now; moving them is a rename with no consumer asking for it.

### The worker

```
consume agent.runs.v1
  claim the run, with the lease                    S2, S7 (the claim clears the attempt)
  read the run and its incident                    the budget comes from the row
  trim the run's event stream
  publish run.started
  agent.Run, with ReportStep writing the row then publishing
  FinishRun with the outcome
  re-read the run; publish run.finished
```

**A claim that returns `false` publishes nothing and returns nil**, logged at debug: the run
is already `RUNNING` under another attempt, or already terminal, and the message is a
redelivery of work that is done or in hand. The offset is committed either way, as
`internal/mq` requires.

**`ReportStep` writes the step row, then its `tool_calls` row, then its `evidence` rows**,
in that order because the foreign keys require it, and not in one transaction. A crash
between two of those writes leaves rows that the restart deletes anyway, so a transaction
would buy atomicity nothing reads.

**The claim clears the previous attempt itself.** S7 specifies `ClaimRun` as deleting the
previous attempt's `agent_steps` in the same transaction as the status update and resetting
`step_count`, `tool_call_count`, `prompt_tokens` and `completion_tokens` to zero;
`tool_calls` and `evidence` cascade from `agent_steps`, so the delete is one statement. It
belongs there rather than as a separate `DeleteRunSteps` the worker calls: state transitions
live in the store layer, and a crash between a claim and a separate delete would leave a
`RUNNING` run rendering the previous attempt's timeline as its own. This makes `ClaimRun`
the package's first multi-statement transaction.

The stream trim cannot join that transaction — Redis is not in it — so it runs immediately
after, and a failure is logged and ignored like any other Redis failure. The worst case is
the spliced timeline this spec is trying to avoid, on a restart of a run whose Redis was
already failing.

**Shutdown cancels the run and writes nothing terminal.** S7 makes cancellation a restart:
the run stays `RUNNING`, the offset is uncommitted, and the lease reclaims it. So restarting
the worker leaves that incident answering 409 until the lease expires. That is the price of
a lease long enough to be safe (below), and it is accepted rather than worked around with a
`RUNNING → PENDING` transition nothing else needs.

### The run reconciler

The worker hosts it. S2 built one for documents on the principle that the process consuming
the work is the one that should look for work that never arrived, and a worker that dies
leaves a run `RUNNING`, which makes `uniq_active_run` reject every new run for that incident
until something reclaims it.

**Runs have two categories, not three.**

| Category | Condition |
| --- | --- |
| Never enqueued | `status = 'PENDING'` and `updated_at < now - RECONCILE_PENDING_AFTER` |
| Abandoned | `status = 'RUNNING'` and `started_at < now - RUN_LEASE` and `attempts < RUN_MAX_ATTEMPTS` |

`ClaimRun` counts the first claim too, so `attempts` is the number of claims and
`RUN_MAX_ATTEMPTS = 3` permits two restarts. That is `INGEST_MAX_ATTEMPTS`'s meaning
exactly, and the reason the condition is `<` rather than `<=`.

There is no retryable-failure category, because `ClaimRun` refuses a `FAILED` run by design.
Re-enqueuing one would produce a message the claim always rejects, and since the sweep
writes nothing and the claim never runs, `attempts` would never increase — so the row would
match on every sweep, forever. That is the silent endless loop `internal/store` warns about.
A failed run is retried by starting a new one, which `uniq_active_run` permits as soon as
the row is terminal.

`RUN_MAX_ATTEMPTS` therefore bounds *restarts*, which is the category documents leave
unbounded. A run that kills the worker every time would otherwise be reclaimed forever, and
each restart deletes its rows and spends real tokens. At the limit it is left `RUNNING` for
a human, visible through `GET /api/incidents/{id}/runs`.

**`internal/reconcile` gains a second target, not a second runner.** `Runner` keeps the
ticker, the per-sweep deadline, `Stats` and the logging; a small unexported `target` struct
supplies what differs — the listing function, the `Due` function, the topic, the message
constructor and the noun in the log line — and `NewDocumentRunner` and `NewRunRunner`
fill it in. `Due` has to be per target because runs use `RUNNING` where documents use
`PROCESSING`, and a shared function switching on both status vocabularies is how the two
quietly drift. `store.ReconcileCandidate.ProcessingStartedAt` carries `agent_runs.started_at`
for a run; the field keeps its name rather than renaming it across the document path.

The run policy reuses `RECONCILE_INTERVAL`, `RECONCILE_BATCH` and `RECONCILE_PENDING_AFTER`
and takes its lease and attempt limit from the run-side variables. `PolicyFrom` builds a
`Policy` out of `config.Reconcile` alone and cannot express that, so `NewRunRunner` takes
the shared `config.Reconcile` plus `RUN_LEASE` and `RUN_MAX_ATTEMPTS` and overrides the two.
`Stats` keeps its per-category map unchanged; the run target's log line names only the two
categories runs can produce, since a zero `retryable_failure` on every sweep would be noise
about a category that cannot happen.

### Configuration

`LoadAgentWorker`, following the existing split by concern: it is what `cmd/agent-worker`
needs and what no other binary does.

| Variable | Default | |
| --- | --- | --- |
| `REDIS_URL` | — | required by `cmd/agent-worker` (M24) and `cmd/api` (M26). Not in `Load`, or `migrate` and `ops-mcp` would need a Redis |
| `REDIS_PUBLISH_TIMEOUT` | `2s` | per `XADD` |
| `EVENT_STREAM_MAXLEN` | `1000` | `MAXLEN ~` |
| `EVENT_STREAM_TTL` | `24h` | refreshed on each publish |
| `RUN_LEASE` | `15m` | see the invariant below |
| `RUN_MAX_ATTEMPTS` | `3` | claims, mirroring `INGEST_MAX_ATTEMPTS`, so two restarts |
| `AGENT_TOOL_SERVER_URL` | — | required; `http://127.0.0.1:8084/mcp` from the host — see below |
| `AGENT_TOOL_TIMEOUT` | `30s` | `mcpclient`'s per-call deadline |
| `SEARCH_TIMEOUT` | `10s` | new, in `config.Search`: a deadline on one kNN query — see the invariant below |
| `REDIS_PORT` | `6379` | compose only, published for the host, like `MYSQL_PORT` |
| `TEST_REDIS_URL` | — | integration tests skip without it; `make test-integration` guards it the way it already guards MySQL, Kafka and Elasticsearch |

**Everything but the infrastructure runs on the host.** `deploy/docker-compose.yml` has no
`api` or `ingestion-worker` service and gains no `agent-worker` one: the binaries are run
against the containers, which is why `MYSQL_PORT`, `OPS_MCP_PORT` and now `REDIS_PORT` are
published at all. So `AGENT_TOOL_SERVER_URL` is a host URL, and because `ops-mcp` sits
behind compose's `lab` profile, the agent worker needs `make up-lab` rather than `make up`
— it refuses to start without a tool server.

**The lease invariant.** S7's accepted limitation is that bounds are checked between steps,
so a run may overrun `MaxRunDuration` by one full step plus the forced `finish`. A step is
not only its model call: it is a model call *and* either an MCP tool call or a retrieval.
The forced `finish` is a model call alone.

```
llmStep      = LLM_TIMEOUT × (1 + LLM_MAX_RETRIES)                       // 3m
retrieval    = EMBED_TIMEOUT × (1 + EMBED_MAX_RETRIES) + SEARCH_TIMEOUT  // 2m10s
stepOverhead = max(AGENT_TOOL_TIMEOUT, retrieval)                        // 2m10s
worstCase    = AGENT_MAX_RUN_DURATION + 2 × llmStep + stepOverhead       // 13m10s
```

`LoadAgentWorker` fails unless `RUN_LEASE` exceeds `worstCase` plus the rebalance margin
below. Counting only the model calls gives eleven minutes for a run that can legitimately
take thirteen, and a lease sized from that figure is the failure the check exists to
prevent: the sweep reclaims a run that is still working, the second attempt deletes rows
the first is still writing, and the two collide on `UNIQUE (run_id, step_number)` — which
`internal/store` classifies as a bug, not a conflict, so the run ends `FAILED` with a 500 in
the log. The formula spans four loaders, so `LoadAgentWorker` takes `Agent`, `LLM`,
`Embedding` and `Search` as arguments rather than re-reading their variables.

**`SEARCH_TIMEOUT` is new because `internal/search` is the one external client with no
deadline of its own.** `embed` has `EMBED_TIMEOUT`, `llm` has `LLM_TIMEOUT`, `mcpclient`
takes one at `Connect`; the Elasticsearch client has retries and no timeout, and S7 gives
the run's context no deadline on purpose, so a hung Elasticsearch today bounds a run at
nothing at all and no `worstCase` is honest. It applies to `Search` only — the bulk index
and the delete-by-document belong to ingestion and are already bounded by
`INGEST_DOCUMENT_TIMEOUT`, which a ten-second cap would break.

**The rebalance margin's job is the retry back-off.** `internal/llm` sleeps between attempts
— up to ten seconds each, so forty seconds across the two model calls counted above — and
no per-attempt timeout covers it. One minute, the value `cmd/ingestion-worker` already uses,
absorbs that with room left, which is why the margin stays a constant instead of becoming
another variable.

**The rebalance timeout is derived, not configured**, as `internal/mq` already requires: it
is `worstCase + 1m`, passed through `mq.WithRebalanceTimeout`. This is S2's other coupled
setting — a handler outlasting it gets the member evicted and the message redelivered. The
session timeout is left at franz-go's default: it heartbeats independently of the poll loop,
so the rebalance timeout is the one that binds, which is why `mq` exposes only that one.

So the ordering the loader enforces is
`AGENT_MAX_RUN_DURATION < worstCase < rebalance timeout < RUN_LEASE`, which at the defaults
is `5m < 13m10s < 14m10s < 15m`. Fifty seconds of headroom is thin, and deliberately so:
raising `EMBED_MAX_RETRIES` or `LLM_TIMEOUT` should fail at startup with a message naming
the lease, not reclaim live runs in production.

**The Redis client is `github.com/redis/go-redis/v9`**, the maintained client, with
`redis.ParseURL` reading `REDIS_URL` so the connection string is one variable rather than a
host, a port and a password. `rueidis` is faster and irrelevant at this volume.

**The tool server connection is made once at startup**, and the worker refuses to start if
`ops-mcp` is unreachable. `mcpclient.Connect` lists tools once by design, and a worker that
started without them would claim runs it cannot investigate — the same reasoning that makes
`EnsureTopics` a startup step.

## Alternatives

**Nine event types, as the brief described.** Finer-grained, and it would remove the
four-second gap. Rejected for the reason above; revisit once M28 can show whether the gap
matters.

**Publishing inside the same transaction as the row.** Not possible — Redis is not in the
MySQL transaction — and wanting it is the reason ADR 0001 separates the two in the first
place.

**`DeleteRunSteps` as a store method the worker calls after the claim.** Makes the restart
visible in the worker's own code, which reads well. Rejected: it moves half a state
transition out of the store layer and opens a window where a `RUNNING` run shows the previous
attempt's steps.

**Keeping the retryable-failure category, by letting `ClaimRun` accept `FAILED`.** ADR 0009's
delete-on-restart actually makes a re-claimable failed run coherent — the objection recorded
in S1 and `internal/store`, that a failed investigation has rows against it, no longer
holds. Rejected for now because it reopens a settled decision to automate a retry the user
can perform with one request, and because `internal/llm` already retries the transient
failure this would cover.

**A `last_event_id` field on `GET /api/runs/{id}`.** Would let M26 subscribe from exactly
where the snapshot ended. Rejected: the read and the subscribe still are not atomic, so it
buys precision it cannot guarantee, and replaying a ten-entry stream costs nothing.

**`DEL` rather than `XTRIM MAXLEN 0` on restart.** Simpler to read. Rejected: it drops the
key's TTL and its last generated id, so a new attempt's entries are ordered only by the wall
clock.

**Paginating the steps in `GET /api/runs/{id}`.** Consistent with the other listings, but
`MaxSteps` already bounds the count and a partial timeline is not useful.

**Letting a request set the budget.** Convenient for M32's evaluation, which wants to compare
budgets. Rejected above; M32 can vary the configuration instead.

**One stream for all runs, filtered per client.** Fewer keys, but every SSE client would read
every run's events and discard most of them, and a single stream's `MAXLEN` would evict a
quiet run's history because a busy one filled it.

**The worker importing `internal/api` for the wire types.** No new package. Rejected: it
inverts the dependency direction that package's `CLAUDE.md` states and links gin into the
worker.

**A single `AGENT_STEP_OVERHEAD` variable** in place of deriving `stepOverhead`. One
number, no extra loader arguments. Rejected: nothing ties it to `EMBED_TIMEOUT` or
`EMBED_MAX_RETRIES`, so raising either leaves a stale figure behind — and the consequence
of a stale figure is the rebalance evicting a worker mid-run, which is what the derivation
is for.

**Bounding the Elasticsearch call inside `internal/agent`** with a `context.WithTimeout`
around the retrieval, instead of adding `SEARCH_TIMEOUT` to `internal/search`. One line and
it touches no finished package's public surface. Rejected: `search` would become the only
external client whose deadline is applied by its caller, and `GET /api/search` would stay
unbounded.

**An `agent-worker` service in Compose.** Rejected: no application binary runs in Compose
today, and adding the first one would split how the project is run in two.

## Detailed Implementation

**M24 — `internal/events` and `internal/wire`**

The envelope, `Publisher` with `Publish(ctx, runID, event)` and `Trim(ctx, runID)`, stream
naming, `MAXLEN`, TTL, the publish timeout, and a fake for tests. `internal/wire` with the
five response types and `Time`, and `internal/api` updated to use them. Redis added to
`deploy/docker-compose.yml` with a health check, `.env.example` and the `make
test-integration` guard updated. The reader is M26's; this milestone writes.

**M25 — `internal/agentrun`, `cmd/agent-worker` and the endpoints**

The consumer and handler, the `ReportStep` implementation that writes the step, its
`tool_calls` row and its `evidence` rows and then publishes, the run reconciler target, and
the three endpoints in `internal/api`.

`internal/store` gains `ListRunsToReconcile`; `ClaimRun` gains the delete-and-reset inside
its transaction. `internal/config` gains `LoadAgentWorker` and `SEARCH_TIMEOUT` on `Search`;
`internal/search` applies it to `Search`. `internal/reconcile` gains the `target` split and
`NewRunRunner`, and `cmd/ingestion-worker` moves to `NewDocumentRunner`.

`cmd/agent-worker` calls `Load`, `LoadDatabase`, `LoadKafka`, `LoadEmbedding`, `LoadSearch`,
`LoadLLM`, `LoadAgent`, `LoadReconcile` and `LoadAgentWorker`, connects to `ops-mcp`, calls
`EnsureTopics`, and runs the consumer and the run reconciler under `internal/shutdown`.

## Verification

- `make check` — the envelope and stream key naming; the run `Due` table over status, age and
  attempts, including that a `FAILED` run is never a candidate and that a run at
  `RUN_MAX_ATTEMPTS` is left alone; `LoadAgentWorker` failing when `RUN_LEASE` is below the
  worst case, that `stepOverhead` follows `EMBED_MAX_RETRIES` rather than a constant, and
  the derived rebalance timeout sitting between them; the wire converters, including that a
  `finish` step carries its evidence and other steps carry none.
- `make test-integration` — an event is published only after its row exists; a failed publish
  does not fail the run; a redelivered run message does not start a second run and publishes
  nothing; `run.started` carries `RUNNING` and the incremented `attempts`; a reclaimed
  run deletes the previous attempt's steps, resets the counters and empties the stream, and
  the new attempt's entry ids sort after the old ones; a second `POST` while one is in flight
  is 409 and one against a missing incident is 404, as is a listing for one;
  `GET /api/runs/{id}` renders a full timeline; the run sweep re-enqueues a stale `RUNNING`
  run and ignores a fresh one.
- End to end: file an incident, start a run, and watch the rows and the stream fill.

## Known limitations, accepted

**The timeline is silent for the duration of a step**, around four seconds. See above.

**`GET /api/runs/{id}` returning everything is safe only because `MaxSteps` is small.** This
is the second design to depend quietly on that invariant — S7's context builder is the
first — and the check that enforces it runs only at configuration load.

**A failed publish is invisible to the user**, who sees a timeline that simply stops. The row
is correct and a refresh fixes it, but nothing tells them to refresh.

**Restarting the worker blocks that incident for the lease.** An in-flight run stays
`RUNNING`, so the incident answers 409 for up to `RUN_LEASE` — fifteen minutes at the
defaults — before the sweep reclaims it. A safe lease and a quick recovery are the same
number pulling in two directions, and safety wins.

**A failed run is not retried automatically.** The user starts a new one. `attempts` counts
claims, so a run that has never been reclaimed reads 1.

**A `RUNNING` run reports zero steps and zero tokens.** Only `FinishRun` writes the
counters. The timeline is the live record; the row catches up at the end.

**Nothing enforces that the API and the worker read the same configuration.** The budget
travels on the row, but `agent_runs.model` records what the API believed while the worker
uses its own `LLM_MODEL`, and the lease invariant is checked against the worker's
`AGENT_MAX_RUN_DURATION` rather than the one the run was created with. One `.env` makes
them agree, and nothing checks that there is one.

**The reconciler's surface doubles** for a failure that only occurs when a worker crashes. It
is still necessary: the alternative is a permanently blocked incident.

**`internal/wire` splits the response types from the handlers that render them.** One entity's
shape and its converter now live one package away from its endpoint, which is the price of
letting a non-HTTP binary publish the same JSON.
