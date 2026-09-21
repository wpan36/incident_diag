# ADR 0009: A reclaimed run restarts; it does not resume

**Status:** accepted
**Date:** 2026-09-21
**Amends:** [ADR 0007](0007-project-focus.md)

## Context

ADR 0007 added checkpoint and resume: a run interrupted by a worker crash would continue
from its last persisted step. The argument was that `agent_steps`, `tool_calls` and
`evidence` are already written as the run proceeds, so the checkpoint data exists and only
the ability to read it back was missing.

That argument is about the cost of *storing* a checkpoint. It says nothing about the cost of
*using* one, and ADR 0007's own consequences section listed what using one requires: an LLM
is non-deterministic, so a resumed run does not necessarily make the decisions the
interrupted one would have; a tool observation captured minutes ago may no longer be true;
and the context budget state has to be rebuilt alongside the context.

No part of it is written yet, so this costs nothing to reverse.

## Decision

A run reclaimed by the lease **restarts from step zero**. Re-claiming deletes the previous
attempt's `agent_steps`, `tool_calls` and `evidence` first, so the run converges rather than
accumulating two attempts' rows.

M23a is removed. S7 covers the loop, its bounds and its failure semantics, and no longer
covers resumption.

## Reasoning

This is the pattern the codebase already uses. Document ingestion deletes a document's
existing chunks before indexing, which is what lets a re-claimed document converge instead
of duplicating. Runs now converge the same way, so there is one idea to understand rather
than two.

What it costs is one investigation's LLM and tool calls after a crash. Runs are bounded by
`MaxSteps`, `MaxToolCalls` and `MaxRunDuration`, one worker is running, and a crash is rare,
so the wasted work is bounded and infrequent — which is not true of the correctness
questions resumption raises.

The rows are still written as the run proceeds. Their reason is auditability, which is what
ADR 0007 said it was upgrading and is now what it remains.

## Consequences

- `agent_runs.attempts` counts restarts rather than resumptions, and remains the signal that
  a run has crashed repeatedly.
- The delete-before-restart is a store-layer operation and belongs with the claim, not in the
  agent loop. S7 specifies it.
- Nothing partial is ever visible: a restarted run's timeline begins again, so an SSE client
  reconnecting after a crash sees the run start over rather than a spliced history. That is
  honest about what happened.
- If long, expensive runs ever make restarting unacceptable, this is the decision to
  revisit. It is not a decision that constrains anything else.

## Alternatives considered

**Keep resume as specified in ADR 0007.** The more capable behaviour, and the more accurate
answer to a question the project is not being asked. Rejected on complexity: see the
project's standing preference for the simplest design that works, in `CLAUDE.md`.

**Resume only retrieval, re-run tools.** Halves the waste and keeps most of the difficulty,
since the context budget still has to be rebuilt and the retrieved chunks still have to be
reconciled with a fresh plan.

**Let a restarted run append to the previous attempt's steps.** Avoids the delete, but then
`step_number` collides with the `UNIQUE (run_id, step_number)` key S1 defined, and the
timeline shows two interleaved attempts as one investigation.
