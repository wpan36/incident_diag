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
- `documents.go` — `Documents` (storage root, upload cap, chunk target and per-document
  chunk cap), `LoadDocuments`, and `DefaultDocumentStorageRoot`.
- `kafka.go` — `Kafka` (seed brokers, produce timeout), `LoadKafka` and `splitList`.
- `reconcile.go` — `Reconcile` (sweep interval, pending-after, batch, ingestion lease,
  attempt limit and the per-document timeout) and `LoadReconcile`.
- `embedding.go` — `Embedding` (provider endpoint, key, model, batch size, timeout, retry
  budget) and `LoadEmbedding`.
- `search.go` — `Search` (Elasticsearch URL, index alias, per-query timeout), `LoadSearch`
  and `DefaultIndexAlias`.
- `events.go` — `Events` (Redis URL, publish timeout, stream cap and TTL, plus the SSE
  heartbeat and read block) and `LoadEvents`.
- `agentworker.go` — `AgentWorker` (run lease, attempt limit, tool server and its timeout,
  plus the derived worst case and rebalance timeout), `LoadAgentWorker`,
  `worstCaseRunDuration` and `RebalanceMargin`.
- `metrics.go` — `Metrics` (the workers' listener address) and `LoadMetrics`.
- `tracing.go` — `Tracing` (OTLP endpoint, service name) and `LoadTracing`; an unset
  endpoint means tracing is off.
- `llm.go` — `LLM` (chat endpoint, key, model, per-attempt timeout, retry budget) and
  `LoadLLM`. A separate provider from the embeddings one: DeepSeek has no embeddings
  endpoint.
- `agent.go` — `Agent` (the four bounds on one run), `LoadAgent`, `BytesPerToken` and
  `WorstCasePromptTokens`.
- `opsmcp.go` — `OpsMCP` (Prometheus endpoint, probe targets, log root and services, and
  the per-tool limits), `LoadOpsMCP`, `parseTargets` and `checkAbsoluteHTTP`. Everything
  the agent can reach is in this one struct.
- `lab.go` — `Checkout` and `Payment`, the two Incident Lab services' settings, plus
  `DefaultLabLogDir`. Their variable names come from the knowledge corpus rather than from
  this package's conventions, because the runbooks the agent retrieves name them.
- `config_test.go` — unit tests for every loader. `isolate` blanks every variable any
  loader reads, so a developer's sourced `.env` cannot change a result. A new loader means
  adding its variables to that list, or its tests inherit the developer's shell.

## How it fits in

The bottom of the dependency graph: it imports nothing from this project, and everything
that needs settings imports it. Each binary calls the loaders it actually needs, so
`cmd/api` gets five and `cmd/ingestion-worker`, which serves no HTTP, gets no server
timeouts.

## Gotchas

- **The lab's variable names are not ours to tidy.** `CHECKOUT_PAYMENT_TIMEOUT`,
  `PAYMENT_POOL_SIZE` and `PAYMENT_PROCESSOR_LATENCY_MS` appear in `testdata/knowledge`,
  and the last is a millisecond count rather than a Go duration for the same reason. Their
  defaults are the values the corpus's postmortems quote.
- **Loading is deliberately split by concern, not by binary.** `Load` is shared, so a
  required `MYSQL_DSN` in it would stop `ops-mcp` and the Incident Lab services from
  starting over a database none of them opens. Add a new field to the narrowest struct
  that needs it, or add a new `LoadX`.
- **`Kafka.ProduceTimeout` is spent inside an HTTP request.** `POST /api/documents`
  produces before it answers, so this has to stay well below `HTTPServer.WriteTimeout` or
  an upload is cut off at the client while the handler is still waiting on a broker.
- **`Reconcile` holds the `INGEST_`-prefixed variables**, which reads oddly until you
  need them: the lease is what both `ClaimDocument` and the sweep mean by "abandoned", and
  they have to be the same value or the sweep re-enqueues rows the claim then refuses.
- **`LoadAgent` is the second loader that checks a relationship between two of its own
  values.** The agent never prunes its context, and what makes that safe is arithmetic:
  `AGENT_MAX_STEPS` observations of 8 KiB plus the incident and the system prompt must fit
  inside `AGENT_MAX_PROMPT_TOKENS`. Raising the step count alone is refused at startup
  rather than discovered as a `TOKEN_BUDGET` stop halfway through a run.
- **`BytesPerToken` and the 8 KiB observation cap are duplicated here on purpose.** This
  package imports nothing from the project: the real token estimator is in
  `internal/ingest`, which depends on this one through `store`, and the cap is
  `summary.LimitBytes`. `internal/agent` estimates prompts with `BytesPerToken`, so the
  invariant and the run-time bound cannot disagree.
- **`LoadAgentWorker` is the third relationship check, and the only one spanning four
  loaders.** `RUN_LEASE` must outlast a whole run, and a run can legitimately exceed
  `AGENT_MAX_RUN_DURATION` because bounds are checked between steps: the worst case adds
  one overrunning step and the forced finish. Counting only the model calls gives eleven
  minutes for a run that can take thirteen, and a lease sized from that figure is the
  failure the check exists to prevent — the sweep reclaims a live run, the second attempt
  deletes rows the first is still writing, and the two collide on
  `UNIQUE (run_id, step_number)`. It takes `Agent`, `LLM`, `Embedding` and `Search` as
  arguments rather than re-reading their variables, so a stale copy cannot drift.
- **`RebalanceMargin` is a constant, not a variable.** Its job is `internal/llm`'s retry
  back-off, which no per-attempt timeout covers; one minute — the value
  `cmd/ingestion-worker` already uses — absorbs it. There is no `AGENT_STEP_OVERHEAD`
  variable for the same reason inverted: nothing would tie one to `EMBED_TIMEOUT` or
  `EMBED_MAX_RETRIES`, so it would go stale, and a stale figure means the rebalance
  evicting a worker mid-run.
- **`LoadEvents` is separate from `LoadAgentWorker`** because two binaries need it: the
  agent worker writes these streams and `cmd/api` reads them for SSE. Loading is split by
  concern, not by binary. It is not in `Load` either, or `migrate` and `ops-mcp` would need
  a Redis. `cmd/agent-worker` loads `SSE_HEARTBEAT` and `SSE_READ_BLOCK` and ignores them; a
  fifteenth loader for two values one binary reads is the more expensive answer.
- **`LoadEvents` is the fourth loader checking a relationship between its own values.**
  `SSE_HEARTBEAT` has a five-second floor because the SSE write deadline is twice it — a
  one-second heartbeat would give a two-second deadline and cut a slow client mid-frame —
  and `SSE_READ_BLOCK` must be below it, because the heartbeat is only checked between two
  blocking reads.
- **`SEARCH_TIMEOUT` is on `Search` and applies to `Search` alone.** The bulk index and the
  delete-by-document belong to ingestion and are bounded by `INGEST_DOCUMENT_TIMEOUT`,
  which a ten-second cap would break.
- **`LoadReconcile` is the one loader that checks a relationship between two values**:
  `INGEST_DOCUMENT_TIMEOUT` must be shorter than `INGEST_LEASE`, or a handler is still
  working on a document another worker is already free to claim. It is validated here
  rather than left to a comment, because both values live in this struct.
- **The embedding dimensionality is deliberately not configurable.** It is a constant in
  `internal/embed`, from which `internal/search` builds its mapping; a wrong environment
  value would silently build a wrong mapping, which is the exact failure the dimension
  check exists to catch.
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
