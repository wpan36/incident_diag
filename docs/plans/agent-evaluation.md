# Agent Evaluation

**Tier B spec (M32).** Written after M31 ran, which is what the roadmap asked for: which
scenarios discriminate was a question the harness had to answer first.

## Problem

The agent works. Nothing says how often it is right, or what being wrong looks like.

## Technical Plan

### Eight scenarios, one symptom

Every case presents as "checkout requests are bad". They differ only in why, which is the
point: a set where the symptom gives the answer away measures nothing.

| Scenario | Fault | Correct diagnosis |
| --- | --- | --- |
| `payment-latency` | the card processor slows, inside payment's pool | payment-service, processor latency; the pool fills as a *consequence* |
| `payment-cpu` | payment saturates its own CPU | payment-service, CPU; the processor is fine |
| `payment-errors` | payment sheds a share of requests | payment-service, 5xx |
| `checkout-latency` | checkout is slow in its own handler | checkout-service, not CPU |
| `checkout-cpu` | checkout saturates its own CPU | checkout-service, CPU |
| `checkout-errors` | checkout sheds its own requests | checkout-service, 5xx |
| `baseline` | none | nothing is wrong |
| `payment-latency-mild` | payment +300ms, under every threshold | nothing is wrong |

Four point at payment-service and three at checkout-service, so naming the service is a third
of the answer rather than all of it. Two negative cases, because an evaluation with no way to
be wrong by inventing something is not an evaluation.

### Scoring is deterministic

A run is **correct** when `affected_service` names the expected service *and* `root_cause`
matches the scenario's required terms and none of its forbidden ones. A negative case is
correct when the diagnosis says nothing is wrong and names none of the failure modes.

Keywords, not a second model. An LLM judge is more tolerant of paraphrase and makes the
*score* non-deterministic — the same output could get two different marks, and the report
would not be reproducible. The cost is real and stated in the report: a run that says
"the downstream card authorizer is slow" without the word `processor` is marked wrong.
Every trajectory is recorded so that a disputed mark can be checked by hand.

### Three runs per scenario

Twenty-four runs. One run cannot tell "correct" from "lucky", and three separates the three
outcomes that matter: stably right, stably wrong, and a coin flip. The fault is injected once
per scenario and held across its three runs, so the warm-up is paid eight times rather than
twenty-four.

Each scenario starts from a clean lab: Prometheus's history is wiped and the two services are
restarted, then the fault runs for two and a half minutes before the first incident is filed.
Both are necessary — see *What the first run showed*.

### What is reported

`docs/agent-eval.md`: accuracy per scenario, and across the set; how the budget was spent;
steps, tool calls, tokens and latency as a distribution rather than a mean; and for every run
the full trajectory — each step's action, its tool and its status.

`docs/agent-eval.json` beside it holds the same runs unrendered, so that a change to the
scoring can be re-applied with `agent-eval -rescore` instead of paying for twenty-four
investigations again.

The trajectory is the point. "Wrong" and "wrong, but it queried the right metric at step 3 and
misread it" are different problems, and only the second says what to change.

### Where it lives

`cmd/agent-eval`, reusing `internal/e2e`'s harness. A command rather than a test, for the
reason `cmd/lab-scenario` is one: it produces a report to read, and a test that fails because
a model had an off day is a test nobody keeps.

## Alternatives

**An LLM judge.** Rejected above.

**More scenarios.** The lab has three fault kinds on two services, so eight is most of what it
can express without inventing faults the corpus does not document.

**Running it in CI.** Twenty-four billed investigations per push, to measure something that
changes when the provider changes rather than when this code does.

## Verification

`make agent-eval` with the full stack, a key, and no worker running. The report is committed,
so the numbers are reviewable rather than claimed.

## What the first run showed

**71% (17/24).** The numbers and every trajectory are in [`docs/agent-eval.md`](../agent-eval.md).

**`payment-cpu` fails 3/3, and it is the interesting failure.** CPU saturation inside
payment-service looks, from checkout's side, exactly like the payment-latency scenario the
agent gets right 3/3 — checkout times out on a slow callee. The agent stops at checkout and
names it, instead of going one hop further to ask *why* payment is slow. Everything it
needed was one query away.

**No run ever chose to finish.** All twenty-four stopped at a bound, so every diagnosis in
the report is a forced one. The accuracy is therefore a lower bound on what a larger budget
would give, and the shipped default of 8 steps / 6 tool calls is tighter still.

**35 of 240 tool calls did not return OK**, nearly all of them the agent trying to learn
which metrics exist — `{job="checkout-service"}` and friends — and being refused by
ops-mcp's fifty-series cap. There is no metric-discovery tool, so that guessing costs about
a seventh of the tool budget. Adding one is the single change most likely to move the
number, and it is a tool-boundary change, so it belongs in a decision of its own rather
than here.

**Two methodology bugs, both found by disbelieving a bad number.** The first run scored
5/24, and the trajectories said why: the agent was reading the *previous* scenario out of
Prometheus, correctly reporting that the system had been broken an hour ago. Wiping the
metric history was not enough on its own, because the lab services keep their counters in
memory — so `cmd/agent-eval` restarts them too. A third bug was in the scoring: a plain
substring check marked three correct diagnoses wrong for naming the cause they had ruled
out.

## Known limitations, accepted

**The budget is the harness's, not the default.** `internal/e2e` raises it to 12 steps and 10
tool calls because the shipped 8 and 6 left the agent short — see
`docs/plans/end-to-end-test.md`. The report says which budget produced the numbers.

**One provider, one model.** Accuracy here is DeepSeek's as much as this project's.

**Keyword scoring understates paraphrase.** Stated above, and the trajectories are there to
check it against.

**The lab is not the estate it imitates.** A fault injected into two Go services is not a real
incident, and an agent that does well here has not been shown to do well anywhere else.
