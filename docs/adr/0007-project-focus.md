# ADR 0007: Shift weight from retrieval to the agent runtime

**Status:** accepted
**Date:** 2026-09-21

## Context

The milestone plan was written before anyone counted how its effort was distributed. Doing
so after Phase A and M6 were finished gave an uncomfortable answer: retrieval occupied
eight of thirty-three milestones — roughly a quarter of the project — while the agent
runtime, the thing the product is named after, occupied five.

That distribution is defensible for a system whose value is search quality. It is the wrong
one for a system whose value is running an agent reliably: sandboxing, scheduling, state
management, recovery from failure, observability and evaluation. Those are what the project
set out to build and what its README claims it is about.

Two specs that would have locked the old distribution in — S3 for the ingestion pipeline
and S7 for the agent runtime — had not been written yet, so the correction was free.

## Decision

**Removed:**

- **Hybrid retrieval (BM25 + dense, fused with RRF), and its spec.** Dense retrieval with
  metadata filtering, measured by Recall@1/3/5 against a fixed set, already demonstrates
  the retrieval engineering this system needs. Fusion tuning is search-engine depth.
- **The front-end build toolchain.** The minimal UI becomes a single static HTML page using
  the browser's native `EventSource`, with no Vite, React, TypeScript or npm. The UI exists
  to make the system demonstrable; a build pipeline for it is overhead.
- **A duplicated corpus.** Evaluation needed a knowledge corpus and the Incident Lab
  separately created one. The corpus is now written once, and the fault-scenario milestone
  reuses it.

**Added:**

- **Checkpoint and resume.** A run interrupted by a worker crash continues from its last
  persisted step instead of restarting. Every step, tool call and piece of evidence is
  already written to MySQL as it happens, so the checkpoint data exists; what was missing
  was the ability to read it back.

  **Reversed by [ADR 0009](0009-restart-instead-of-resume.md)** before any of it was
  written. Storing a checkpoint is cheap; using one is not, for the reasons this ADR's own
  consequences section lists. A reclaimed run restarts instead.
- **Agent-level evaluation.** Retrieval had a measured quality number and the agent had
  none. The new milestone runs the agent against every injectable fault scenario and
  reports diagnosis accuracy, step and tool-call counts, token cost and latency
  percentiles, including scenarios it is expected to fail.

## Consequences

- The project no longer claims hybrid retrieval. `docs/rag-eval.md` will report dense
  retrieval only, which means one number rather than a comparison.
- The UI cannot grow beyond a page or two without reintroducing a build step. That is an
  accepted ceiling, not an oversight.
- Resuming a run is harder than it looks and S7 has to answer it properly: an LLM is
  non-deterministic, so a resumed run does not necessarily make the same decisions as the
  interrupted one would have; a tool observation captured minutes ago may no longer be
  true; and the context budget state has to be rebuilt along with the context. Resume means
  *continue from a checkpoint*, not *replay*.
- Agent evaluation is easy to fool. The faults are injected by the same project that writes
  the agent's prompts, so the scenario set must include cases the agent should not be able
  to diagnose, and `docs/agent-eval.md` has to state plainly that the data is synthetic.
- Milestone count is roughly unchanged. This was a redistribution of effort, not a
  reduction, and it does not make the remaining work shorter.

## Alternatives considered

**Keep hybrid retrieval and cut something else.** It is a self-contained piece of work with
a clear measurement attached. Rejected because the effort buys retrieval quality that an
evaluation set this small cannot demonstrate convincingly anyway.

**Add sandboxed tool execution** — running restricted analysis scripts inside a disposable,
network-less, read-only container. The most valuable addition considered, and the one this
project most obviously lacks. Rejected for now on two grounds: it contradicts the standing
principle that the agent is read-only and allowlisted, which would need rewriting rather
than amending; and a shallow implementation would be worse than none, because container
isolation done badly invites exactly the failure it claims to prevent.

**Leave the plan alone.** The plan was agreed and the work so far has followed it. Rejected
because the specs that would have cemented it were not yet written, and a plan is worth
less than being right about what the project is for.
