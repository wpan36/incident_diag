# Observability Base and Fault Scenarios

**Tier B spec (M14, M15).** M14 scrapes the Incident Lab; M15 drives it into the failures
the M12 corpus describes. They are one spec because M15's only verification is the metrics
M14 collects.

`docs/plans/incident-lab.md` (M13) is the prerequisite and is not repeated here.

## Problem

The corpus does not ask general questions. Its runbooks name PromQL a responder is told to
run — `payment_pool_in_use / payment_pool_size`, the p99 of `http_request_duration_seconds`
"for checkout-service", `container_cpu_cfs_throttled_seconds_total` — and each of those
only resolves if M14 labels the data the way the runbook assumes.

M15 then has to make the numbers move. That is harder than calling `/fault`: a load
generator that pushes as hard as it can saturates the pool whether or not a fault is
enabled, which erases the difference between a healthy baseline and an incident.

## Technical Plan

### Scrape labels are a contract with the corpus

| Runbook text | What M14 must produce |
| --- | --- |
| "p99 of `http_request_duration_seconds` for checkout-service" | `job="checkout-service"` |
| `container_cpu_usage_seconds_total` "for checkout-service" | cAdvisor's `name="checkout-service"` |

So the Prometheus job name **is** the service name, and the two lab containers get
`container_name: checkout-service` and `container_name: payment-service` — cAdvisor labels
by container name, and the Compose default (`incident_diag-checkout-service-1`) would not
match anything a runbook says. One replica each, so pinning the container name costs
nothing.

`scrape_interval` is 15s. The runbooks reason over five-minute windows, and 15s gives
`rate()` and `histogram_quantile()` enough points inside one.

### The CPU fault needs a CPU limit to be visible

`container_cpu_cfs_throttled_seconds_total` is zero without a quota, and two spinning
goroutines on an idle multi-core host slow nothing measurably. Both lab services therefore
get `cpus: "1.0"` under `deploy.resources.limits`. Without it the CPU scenario produces a
graph that looks like the CPU runbook's "busy, not throttled" case, which is the opposite
of what it is supposed to demonstrate.

### Grafana gets a datasource and nothing else

A provisioned Prometheus datasource, anonymous viewer access, no dashboards: the two
dashboards are M30, and a placeholder now would be rewritten then.

### Compose

Everything in this spec plus M13's two services sits in `deploy/docker-compose.yml` under
`profiles: [lab]`, reached with `make up-lab`. One Compose project means one network, which
is what lets M16's `ops-mcp` address `prometheus:9090` and `checkout-service:8080` by name.
`make up` keeps starting only MySQL, Kafka and Elasticsearch, so `make test-integration`
does not build two Go images first.

### The load generator is open-loop

A closed-loop generator (N workers, each sending the next request as soon as the last
returns) offers concurrency N no matter how slow the system is. With N above the pool size
the pool is saturated at baseline; with N below it the pool can never saturate, however
slow the processor becomes. Neither reproduces the corpus.

M15 therefore drives a **target request rate**: a ticker at `1/rps` dispatches each request
to its own goroutine, bounded by a maximum in-flight count. Concurrency then follows from
latency, which is what makes the chain in `incident-lab.md` observable —

```
rps × hold_time = concurrent demand      demand > pool ⇒ queueing
```

— and what makes the difference between phases meaningful. Reaching the in-flight bound is
reported as a load-generator limit, never silently dropped: it means the numbers under it
understate the real demand.

Each iteration sends `POST /orders` and, on a 201, `GET /orders/{id}`. The second call
touches nothing downstream and is the control the CPU runbook's central distinction needs.

### The numbers

Chosen so that the payment-latency scenario is a latency incident with a flat error rate,
which is what its runbook says distinguishes it from an outage.

| | Value | Why |
| --- | --- | --- |
| `PAYMENT_POOL_SIZE` | 20 | the March postmortem's value |
| `PAYMENT_PROCESSOR_LATENCY_MS` | 50 | baseline in-use ratio ≈ 0.02 |
| `CHECKOUT_PAYMENT_TIMEOUT` | 2s | the May postmortem's value before it was lowered |
| `-rps` | 7 | demand under fault ≈ 17.9 of 20 connections |
| latency fault | 2500ms ± 250 | payment p99 ≈ 2.6s, past its 2s alert; past checkout's 2s timeout |

Demand under fault sits just under the pool size on purpose. Above it, waits reach the
request deadline and payment answers 503 for most of its traffic, which is the error-rate
runbook's incident rather than this one.

A consequence worth naming: with demand below capacity and arrivals paced by a ticker,
`payment_pool_wait_seconds` stays near zero even at a 0.85 in-use ratio. That is the
runbook's own discriminator rather than a gap — a high ratio with a flat wait and an
elevated `payment_processor_latency_seconds` means the pool is saturated *as a consequence*
of a slow dependency, so raising the pool size is the remediation that makes it worse.

### What M13 must do for the chain to close

**The simulated processor call is not cancellable.** The pool wait is — a caller that gives
up while queueing frees the queue and gets 503 — but once a connection is held, the charge
has been dispatched and completes even if the caller has gone. Two things follow, and both
are in the corpus: the pool stays saturated for the full hold time rather than draining as
callers time out, and checkout's 504 covers an authorization that actually succeeded, which
is the correctness case `checkout_payment_client_timeouts_total` exists to measure.

### The scenarios

`lab-scenario <name>` runs three phases — baseline, fault, recovery — and prints per-phase
request counts by status, p50/p95/p99 per route, and the relevant gauges and histogram
sums scraped from both `/metrics` endpoints. Scraping the services directly means a
scenario verifies itself without Prometheus running.

| Scenario | Fault | Expected shape | Runbook |
| --- | --- | --- | --- |
| `baseline` | none | everything healthy | none — the negative case M32 needs |
| `payment-latency` | latency 2500ms on payment | processor latency rises, in-use ratio follows it to ~0.85 with the **wait time flat**, payment p99 > 2s, checkout 504s and `checkout_payment_client_timeouts_total` rising, payment 5xx flat | payment-latency, then connection-pool |
| `checkout-cpu` | cpu 2 workers on checkout | latency rises on `POST /orders` **and** `GET /orders/:id`, payment unaffected, and throttling in cAdvisor where cAdvisor works — see the limitation below | cpu-saturation |
| `payment-errors` | error 503 at 0.2 on payment | payment 5xx ≈ 20%, checkout 502s, latency flat | error-rate |

Faults are cleared on exit, including on SIGINT: a scenario that leaves the lab broken
makes the next one meaningless. `lab-scenario list` prints the table above.

Both services' `/health` is polled before the baseline phase starts, so the tool waits for
a stack that is still coming up instead of measuring it.

## Alternatives

**Alerting rules in Prometheus for the two alerts the runbooks name.** Tempting, because
the agent could then read `ALERTS`. Rejected for now: without Alertmanager they only
change a page in the Prometheus UI, and giving the agent the answer as a metric is the
opposite of what the tool layer exists to test.

**A closed-loop generator with a worker count per scenario.** Simpler code. Rejected
above: the concurrency would be a constant chosen per scenario rather than a consequence of
the fault, so the scenario would demonstrate its own configuration.

**Scraping Prometheus for the verification instead of the services.** More faithful to how
the agent will read the data. Rejected: it makes M15 depend on M14 being up, and a
scenario that cannot self-verify is harder to trust when the agent later disagrees with it.

**Dashboards now rather than in M30.** Rejected: M30 has the metrics list this project
actually cares about, and a dashboard built against the lab alone would be rebuilt.

## Detailed Implementation

- `deploy/prometheus/prometheus.yml` — three jobs: `checkout-service`, `payment-service`,
  `cadvisor`.
- `deploy/grafana/provisioning/datasources/prometheus.yml` — one datasource, default.
- `deploy/docker-compose.yml` — `prometheus`, `grafana`, `cadvisor` in the `lab` profile;
  container names and CPU limits on the two lab services.
- `internal/lab/scenario/` — the rate-limited generator, the latency summary, the metrics
  snapshot (through `expfmt`, the parser the client library already ships) and the scenario
  table.
- `cmd/lab-scenario/` — flags and wiring.
- `Makefile` — `up-lab`, `logs-lab`, `scenario`. No `down-lab`: `make down` already
  removes the whole compose project, profiles included.

## Verification

- `make check` — the generator's pacing and in-flight bound (fake clock or a short run
  against `httptest`), the percentile computation on a known sample, the metrics reader on
  a recorded `/metrics` body, and that every scenario clears its fault on both the normal
  and the cancelled path.
- Smoke: `make up-lab`, then `lab-scenario payment-latency`, and check the printed report
  shows the chain; then the same query in Prometheus with `job="payment-service"`, and
  `container_cpu_usage_seconds_total{name="checkout-service"}` after `checkout-cpu`.

## Known limitations, accepted

**The default phase is 2 minutes and the alerts the runbooks quote need 5.** `-hold 5m`
reproduces them; the default keeps a smoke test short.

**cAdvisor produces nothing on a Docker daemon using the containerd image store.** Verified
on Docker 29 (`docker info` reports `driver=overlayfs`): every container is dropped with
"failed to identify the read-write layer ID", because cAdvisor still looks for the legacy
`/var/lib/docker/image/overlayfs/layerdb` layout, so only the root cgroup is exported and
`container_cpu_usage_seconds_total{name="checkout-service"}` is empty. It works on a daemon
using the classic `overlay2` store. Until then the CPU runbook's container metrics are
unavailable and the `checkout-cpu` scenario rests on its other, primary signal — latency
rising uniformly across routes, including `GET /orders/:id`, which calls nothing — plus
`process_cpu_seconds_total`, which both services export through the Go process collector.
It also needs privileged access to the host's cgroups, and nothing else depends on it, so
removing it costs only those metrics.

**The numbers above are tuned against one machine.** They are flags for that reason, and
the printed report is what says whether a run reproduced the shape.

**No Alertmanager, so nothing fires.** The incident is filed by hand, which is what the
rest of the system already assumes.
