# incident_diag

An agentic SRE diagnosis platform in Go. A user uploads internal runbooks and postmortems,
files an incident, and a bounded read-only AI agent investigates it using RAG plus
Prometheus, log and HTTP-probe tools, streaming its progress to the browser over SSE.

It is a learning project, not a commercial product. It must be genuinely runnable,
testable and understandable — not a demo assembled to name-drop a stack.

## Start here

New session? Read these, in this order:

1. **`docs/workflow.md`** — how this project is built, who invokes which skill, and what
   must be written down before implementation starts. Read this before doing anything.
2. **`docs/roadmap.md`** — the 33 milestones and which ones are done. This is the source
   of truth for current progress.
3. **`docs/architecture.md`** — components, data flows, storage responsibilities.
4. **`docs/adr/`** — why the significant decisions were made the way they were. Read the
   ones relevant to what you are touching before proposing a change to them.
5. **`docs/plans/<slug>.md`** — the spec for the specific feature being implemented.

Nothing important lives only in a conversation. If a decision is not in `docs/`, it has
been lost, and the right move is to ask rather than to guess.

## Two rules that override convenience

**Stop and ask when a decision is uncertain.** Schema shapes, message and event formats,
API surfaces, tool parameters, new dependencies, trade-off-laden implementation paths, and
any request with more than one reasonable reading — all of these stop and ask rather than
picking a default. Only details with an overwhelming industry default and a cheap reversal
(naming, log wording, test layout) get decided unilaterally, and those get mentioned in
the milestone summary.

**Announce when the user has to act.** `design-discussion`, `write-spec`, `goldfish-test`
and `mean-review` are invoked by the user, and the last two require a brand new session
with no memory of the prior discussion. On reaching one of those points, stop and say
exactly what to invoke and whether it needs a fresh session.

## Language

Everything in this repository is written in English: code, comments, documents, commit
messages, specs.

## Coding rules

- Idiomatic Go; composition over unnecessary abstraction.
- Define an interface only when an implementation genuinely needs to be swapped or faked
  in tests — not speculatively.
- `context.Context` is threaded through everything; every external call has a timeout.
- Every goroutine has a clear owner and a clear exit condition. Leak tests where it
  matters.
- Wrap errors with context (`fmt.Errorf("...: %w", err)`). `panic` is for programmer
  errors only.
- Secrets come from the environment. `.env` is never committed; `.claude/` always is.
- State transitions live in the store layer and are conditional updates, which is what
  makes the Kafka consumers idempotent.

## Every milestone ends with

`make check` (gofmt, vet, test, race) → smoke test → refresh any stale `CLAUDE.md` →
update `docs/roadmap.md` → summarize changed files, design choices and known limitations →
propose a commit message. Commits are made by the user, not automatically.

## Layout

```
cmd/         api, migrate, ingestion-worker, agent-worker, ops-mcp, and the Incident Lab services
internal/    the packages those binaries are built from
migrations/  numbered SQL schema migrations, embedded into the binaries that apply them
web/         single static HTML page, no build step
deploy/      docker compose, Prometheus config, Grafana dashboards
testdata/    knowledge corpus and the RAG evaluation set
docs/        workflow, roadmap, architecture, ADRs, feature specs
.claude/     rules and skills — committed deliberately, they are part of the project
```

Directories gain their own `CLAUDE.md` as they are built, maintained by the
`context-hierarchy` and `context-sync` skills. Most of the layout above does not exist
yet; `docs/roadmap.md` says what does.
