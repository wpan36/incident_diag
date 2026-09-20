# How We Work

This project is built with Claude Code, and the process is part of the deliverable. The
rules and skills in `.claude/` are committed for that reason. This document explains how
they fit together and who invokes what.

Everything written in this repository — code, comments, documents, commit messages — is in
English.

## The core constraint: sessions lose memory

Two of the skills below (`goldfish-test`, `mean-review`) only work in a session that has
no memory of the discussion that produced the work. Sessions also get cleared for ordinary
reasons. So:

**Every decision must end up in `docs/`. A decision that only exists in a conversation is
a decision that has been lost.**

The test to apply is the one `write-spec` states: could a fresh session, given only the
spec and the relevant `CLAUDE.md` files, implement this correctly? If the answer depends
on something only the original conversation knows, it has to be written down.

## Two standing rules for Claude

### Stop and ask when a decision is uncertain

Any ambiguous or genuinely open decision stops and asks, rather than picking a default and
continuing. This covers contract-shaped choices (schema, message and event formats, API
shape, tool parameters), implementation paths with real trade-offs, new dependencies or
infrastructure, and anything where the request has more than one reasonable reading.

The exception is a detail with an overwhelming default that is cheap to change later —
naming, log wording, test layout. Those get decided and mentioned in the milestone
summary.

"I'll do it this way for now and you can change it later" is not a way around this.
Contract-shaped decisions are not cheap to change.

### Announce the points where the user must act

The skills are invoked by the user, not by Claude, because they depend on session
boundaries Claude cannot create. On reaching one of those points, Claude stops and says
exactly what to invoke and whether it needs a new session.

## The loop

| Step | Who | Session |
| --- | --- | --- |
| `/design-discussion` — scope the feature, discuss in prose, no code | user invokes | current, Plan Mode |
| `/write-spec` — write `docs/plans/<slug>.md` | user invokes | same session |
| commit the spec | | `docs: spec for <slug>` |
| `/goldfish-test docs/plans/<slug>.md` — comprehension, critique, readiness | user invokes | **new session**, Plan Mode |
| fix any gaps found, amend the spec commit | | |
| implement the milestones covered by the spec | | same session, after leaving Plan Mode |
| `/mean-review` — adversarial review of the finished code against its spec | user invokes | **new session** |
| `/context-hierarchy` — refresh the nested `CLAUDE.md` files | user invokes | at phase boundaries |

The `sycophant-challenge` rule applies throughout: every technical proposal is followed by
genuine self-critique, unprompted. The `no-code-during-design` rule keeps design and
implementation phases separate. The `context-sync` rule keeps `CLAUDE.md` files from going
stale as the source changes.

## Which milestones get the full treatment

Running design discussion, spec writing and a goldfish test for all 33 milestones would
cost more than it returns. The dividing line is fixed in advance so it does not get
re-argued each time.

**Tier A — full design → spec → goldfish-test.** A milestone qualifies if it:

- defines a cross-module **contract** (database schema, message format, event schema, tool
  interface, log format), or
- introduces a **state machine**, or concurrency and lifecycle semantics, or
- establishes a **security boundary** (path jailing, allowlists), or
- involves an **algorithmic choice with trade-offs** (chunking strategy, retrieval fusion,
  context budgeting).

**Tier B — a short spec, then implement.** Mechanical work that fills in an already-agreed
contract: adding another MCP tool, wiring a Grafana dashboard, writing a demo service,
building a front-end page. The spec is a half page in `docs/plans/`; no goldfish test.

Ten Tier A specs are planned; see `docs/roadmap.md`.

## Every milestone ends the same way

1. `make check` — gofmt, `go vet`, `go test`, `go test -race`
2. A smoke test of whatever is runnable
3. Per the `context-sync` rule: check whether any affected directory's `CLAUDE.md` is now
   stale, and update it
4. Update the status in `docs/roadmap.md`
5. Summarize changed files, design choices and known limitations
6. Propose a commit message

At the end of each phase: `mean-review` in a fresh session, then a `context-hierarchy`
pass adding the next level of `CLAUDE.md` files.

## Commits

Specs are committed separately from and before their implementation, so the history reads
as design-first:

```
docs: spec for agent-runtime
feat: implement bounded agent loop with execution limits
```

`.claude/` is committed. `.env` never is.
