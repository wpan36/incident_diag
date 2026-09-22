# End-to-End Test

**Tier B spec (M31).** It is also M32's harness, which shapes it.

## Problem

Every layer is tested and the product is not. "The agent investigates an incident" is a claim
until something injects a fault, files an incident, runs the agent and checks what came back.

## Technical Plan

### One scenario, run whole

`internal/e2e`, behind the `integration` tag, skipping without the full stack and an LLM key:

```
inject          internal/lab/scenario, the injection M15 already built
upload          the corpus from testdata/knowledge, if not already READY
file            POST /api/incidents
run             POST /api/incidents/{id}/runs, then poll GET /api/runs/{id}
assert          on the terminal run
clear           the fault, on every exit path
```

Nothing here reimplements a step. The injection is M15's, the corpus is M12's, and the API is
the one a browser uses — a test that reached into MySQL would be testing something the
product does not do.

### What it asserts

- the run reaches a terminal status, `SUCCEEDED` with any `stop_reason` included, because a
  bounded run that produced a diagnosis is a success (S1)
- at least one `retrieve` step and at least one `tool_call` step: an agent that answered from
  the model's own knowledge has not investigated anything
- `final_result.affected_service` names the faulted service
- every evidence citation resolves to a step that exists

It does **not** assert on the wording of `root_cause`. A model is not deterministic and a test
that pinned prose would fail for reasons that are not this project's.

### Built for M32 to reuse

M32 runs eight to ten scenarios several times each and reports a distribution, so the shape
here is:

- a `Scenario` value — the fault, the incident text, the service the diagnosis should name
- a `Run(ctx, Scenario) (Outcome, error)` that does the sequence above and returns what
  happened, asserting nothing
- the assertions in the test, not in `Run`

M31 is that function called once with one scenario. Writing it any other way would mean M32
rewrites it.

**`Outcome` carries the whole trajectory** — every step, its action, its status, the tool it
called — not just the verdict. M32's roadmap note says why: "wrong" and "wrong, but it
queried the right metric at step 3 and misread it" are different problems, and only the
second says what to change.

### Cost

One real investigation: an LLM call per step and real tool calls. It is opt-in through the
same `TEST_LLM_API_KEY` the agent's own integration test uses, so `make test-integration`
without a key still runs everything else.

## Alternatives

**Asserting through the store rather than the API.** Fewer moving parts, and it stops being
end to end: the endpoints are what a user touches and what M28 depends on.

**A scripted LLM.** Deterministic and free, and it tests the plumbing this project already
tests elsewhere. The point of this milestone is the part that is not deterministic.

**Putting the assertions inside `Run`.** Convenient for M31 and useless for M32, which needs
outcomes to aggregate rather than failures to report.

## Verification

`make test-integration` with the full stack and a key. Without the key it skips, and says so.

## Known limitations, accepted

**One scenario proves the pipeline, not the agent.** Accuracy is M32's question; this
milestone proves the loop reaches an answer at all.

**A flaky provider fails the build.** The retry in `internal/llm` covers a hiccup, not an
outage.
