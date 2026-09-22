# ADR 0010: No `answer.delta`; the diagnosis arrives whole

**Status:** accepted
**Date:** 2026-09-22

## Context

M27 was to stream the final diagnosis to the browser token by token as `answer.delta`
events. S7 noticed the obstacle and deferred the decision by name to S9: the diagnosis is
the `root_cause` **argument of a `finish` tool call**, not assistant content, so streaming it
means streaming tool-call argument deltas rather than prose.

## Decision

Cancel M27. The diagnosis arrives whole, in the `run.finished` event.

## Reasoning

Extracting `root_cause` from argument deltas means parsing partial JSON: tracking string
escapes and `\u` sequences across delta boundaries, with no guarantee the field arrives
first. S7 chose native tool calling precisely so that no tolerant parser had to be written,
and this would reintroduce one in a harder form for a presentational gain.

The alternatives are worse in ways this project has already been bitten by. A second,
non-tool call to restream the prose would give the diagnosis two sources, so what the user
read could differ from `agent_runs.final_result` — the same class of divergence as the
Elasticsearch and MySQL document ids that the M25 smoke test caught. Re-rendering the
completed result is not streaming at all.

## Consequences

- **The last five to fifteen seconds of a run are silent.** The timeline has been updating
  for the thirty seconds before that, and then the diagnosis appears at once.
- **The demo loses its most visually striking moment.** A diagnosis typing itself out reads
  as alive; one appearing whole reads as a page load. This is a real loss, traded for not
  writing a partial-JSON parser.
- Nothing outside the roadmap promised `answer.delta`. `CLAUDE.md` says "streaming its
  progress to the browser over SSE", which M26 satisfies.
- The event vocabulary stays at S8's three. Adding a fourth later is additive, as S8 says.

## Alternatives considered

**Stream the tool-call argument deltas.** The only option where what streams *is* what is
stored. Rejected on the parser above; revisit if a provider ever exposes structured output
incrementally by field.

**A second, non-tool call that restreams the prose.** Natural `content` streaming, at the
cost of an extra call per run and two sources for one diagnosis.

**Re-render the stored result slowly in the browser.** Looks like streaming and is a lie
about what is happening.
