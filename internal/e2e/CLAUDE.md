# internal/e2e

## Purpose

The product, run whole: inject a fault into the Incident Lab, file an incident through the
API, let the agent investigate, and return what it did. It asserts nothing — M31's test
does that, and M32 aggregates the same outcomes over many runs.

## Contents

- `e2e.go` — `Scenario`, `Outcome`, `Step`, `Evidence`, `DefaultScenarios`, and the
  counters a report reads (`Retrievals`, `ToolCalls`, `ToolsUsed`, `EvidenceCount`).
- `harness.go` — `Config`, `ConfigFromEnv`, `New`, `Harness.Run`, `EnsureCorpus`,
  `loadManifest`, and the API client the sequence goes through.
- `score.go` — `Expectation`, `Score`, `Mark`, the negation heuristic `restsOn`, and
  `EvalScenarios`: M32's eight cases.
- `report.go` — `Result`, `Run` (a whole evaluation, saved as JSON) and `Report`, which
  renders `docs/agent-eval.md`.
- `e2e_integration_test.go` — M31: one scenario, with the assertions.
- `score_test.go` — the marking rules, the balance of the scenario set, and the report.
- `corpus_test.go` — the manifest describes the whole corpus, and the payment runbook
  still names the CPU metric.

## How it fits in

`Harness` assembles the API and both workers in one process over the real infrastructure,
so `make test-integration` runs an investigation rather than asking anyone to start three
binaries. The fault comes from `internal/lab/scenario`'s `Driver`; ops-mcp runs in its
container. The spec is `docs/plans/end-to-end-test.md`.

## Gotchas

**`cmd/ingestion-worker` and `cmd/agent-worker` must not be running.** The harness joins
their consumer groups, so a second member takes half the messages and fails them against a
storage root it does not share. This is the failure that looks like "no such file or
directory" on a document that was just uploaded.

**The budget is raised above the shipped default.** At `AGENT_MAX_TOOL_CALLS=6` the agent
stopped at a bound before it could separate "payment-service is slow" from "checkout's
timeout is too short", and named the wrong service about half the time. `e2eMaxToolCalls`
gives it room; how much it actually needs is M32's question.

**`affected_service` is free text.** The finish schema asks for a bare service name and a
model may still qualify it, so the test checks that the field *names* the service rather
than equals it.

**Only READY counts as an existing document.** A row left `FAILED` by an earlier run is
re-uploaded rather than treated as a permanent block.

**`payment-cpu` has its own incident wording.** It degrades rather than times out — payment
lands near its own budget rather than past checkout's two-second timeout — so the shared
504 symptom would send the agent hunting for a storm that is not there. That is what
derailed the runs that reported the metrics contradicting the incident.

**`manifest.json` decides a document's service and type, not its filename.** The filename
guess this replaced uploaded `cpu-saturation-runbook.md` as `payment-service` — it has no
"checkout" in its name — when the manifest declares it unattributed because it applies to
either. A search filtered to `checkout-service` could then not see it at all. A document
in the directory but not in the manifest is an error rather than a guess.

**A negative keyword cannot be a plain substring.** A correct diagnosis names the causes it
ruled out, so `restsOn` ignores a term whose preceding 90 characters carry a negation.
Without it, three runs that correctly said "checkout is shedding requests; payment-service
is healthy" were marked wrong.

**Scenarios contaminate each other unless the lab is reset.** The agent looks back over an
hour. `cmd/agent-eval` wipes Prometheus (`Driver.ResetMetrics`, which needs
`--web.enable-admin-api`) *and* restarts the two lab containers, because the services hold
their counters in memory and a wipe alone leaves the previous scenario's
`checkout_payment_client_timeouts_total` in place.

**Re-scoring is free; re-running is not.** Every run writes `docs/agent-eval.json` beside
the report; `agent-eval -rescore docs/agent-eval.json` re-marks it against the current
expectations and rewrites the report without calling a model.
