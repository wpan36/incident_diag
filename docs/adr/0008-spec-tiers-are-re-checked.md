# ADR 0008: A spec's tier is re-checked when an earlier decision changes it

**Status:** accepted
**Date:** 2026-09-21

## Context

`docs/workflow.md` fixes four criteria for Tier A — cross-module contract, state machine or
concurrency lifecycle, security boundary, algorithmic choice with trade-offs — and says
they are fixed in advance so the tier of each milestone does not get re-argued every time.

That was aimed at the wrong failure. Nobody re-argued a tier; the problem was that a tier
quietly stopped being true.

**S4 `retrieval-and-evaluation` qualified for exactly one reason: retrieval fusion was an
algorithmic choice with trade-offs.** ADR 0007 removed hybrid retrieval. The justification
was deleted and the tier outlived it, because nothing in the process asks "which earlier
decisions did this one invalidate?" What remained in S4 was a kNN query with a metadata
filter, against a mapping S3 fixes, returned through API conventions S1 fixes — a spec that
satisfies none of the four criteria, still holding a gate that costs a design discussion and
a dedicated fresh session.

**S5 `incident-lab-and-log-contract` had a related but distinct problem.** It qualified on
one part of its content — the structured log format, which is the input contract for
`read_service_logs` — while the rest, a `/fault` endpoint with three switches, qualified on
nothing. A spec that is Tier A because of one paragraph is a sign the paragraph is in the
wrong spec.

## Decision

**S4 and S5 are downgraded to Tier B.** M11, M12, M13, M14 and M15 each get a short spec in
`docs/plans/` and no goldfish test.

**The log format moves into S6, and S6 moves ahead of Phase C.** It is written before the
lab services that emit the format and before the tool that reads it, so one definition
serves both. S6 still gates M16–M20 in Phase D; only the spec moves.

**When a decision changes what a spec contains, that spec's tier is re-checked against the
four criteria.** This is now stated in `docs/workflow.md` rather than left to whoever
notices.

## Reasoning

The alternative reading — that S4 should keep its gate because more review is never harmful
— is wrong in a way worth naming. The goldfish test costs a fresh session and a round of
design discussion. Spending that on a spec with no open decisions does not make the spec
better; it trains everyone to treat the tier as ceremony rather than as a signal about where
the risk is. The criteria are only useful if they are applied honestly in both directions.

Moving S6 rather than moving the log format into a Phase D spec is the part worth
explaining. Leaving S6 in Phase D would have put the contract *after* the code that emits
it: M13 would have invented a format in Phase C and M19 would have had to accept it, which
is the original ordering with extra steps. Writing S6 first inverts that.

## Consequences

- S6 designs `prometheus_query`, `http_probe` and `read_service_logs` before the Incident
  Lab exists to point them at. This is the same "contract before consumer" shape being
  fixed, relocated rather than eliminated — but a smaller bet, because those tools target a
  standard Prometheus HTTP API and ordinary health endpoints. The log format was the only
  part this project had to invent, and it is now invented next to its reader.
- S6 grows. It is already the security-boundary spec, and adding a data format to it is a
  real cost; it stays the single largest remaining spec.
- Seven Tier A specs remain: S1, S2, S3, S6, S7, S8, S9.
- The roadmap now shows one spec sitting in a phase it does not gate. That reads as an
  error unless the reason is written down, which is part of why this ADR exists.

## Alternatives considered

**Keep S4 as Tier A.** Rejected above: a gate with nothing to catch devalues the gates that
have something to catch.

**Downgrade S5 but leave the log format in it.** Keeps the phases tidy and the contract
badly placed. This is the arrangement the original roadmap chose, and it was recorded as a
bet at the time.

**Move the log format into S6 and leave S6 in Phase D.** Puts the contract after the code
that emits it, so M13 would define a provisional format and M19 would finalize it, with
rework in between.

**Downgrade S3 as well.** Considered and rejected. Chunking is a genuine algorithmic choice
— heading-aware against fixed-window against semantic — and the Elasticsearch mapping fixes
the embedding dimensionality semi-permanently, since ADR 0006 notes that changing the model
means reindexing everything.
