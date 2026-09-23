# internal/lab

## Purpose

The Incident Lab: two small services that break on demand, so the agent is evaluated
against a system that actually fails rather than against fixtures. `checkout-service`
calls `payment-service`, which authorizes charges through a fixed-size pool of connections
to a simulated card processor.

Everything here is shaped by the knowledge corpus in `testdata/knowledge` — routes, metric
names, environment variables and status codes all come from those runbooks and
postmortems, not from this repository's conventions. See `docs/plans/incident-lab.md` and
`docs/plans/observability-and-fault-scenarios.md`.

## Contents

- `server.go` — `App` (registry, fault controller, routes), `Route` and `RouteKind`, the
  middleware that injects faults and records the duration metric and the request log, and
  `Run` with graceful shutdown.
- `fault.go` — the `Controller`: latency, error and cpu faults, their validation, and the
  `/fault` endpoint that dispatches on `kind`.
- `pool.go` — `Pool`, the card processor connections. The saturable resource.
- `checkout.go` — `POST /orders`, `GET /orders/:id`, the payment client and the in-memory
  order book.
- `payment.go` — `POST /charge`, `POST /refund` and the simulated processor call.
- `metrics.go` — every metric name as a constant, and the three registries' constructors.
- `logfile.go` — `LogWriter`, stdout plus `<service>.log`.
- `http.go` — JSON request and response helpers, deliberately not the platform's.
- `scenario/` — the M15 driver: an open-loop load generator, the fault calls per scenario,
  and the report. `cmd/lab-scenario` is its wiring.

## How it fits in

Nothing outside `cmd/checkout-service`, `cmd/payment-service` and `cmd/lab-scenario`
imports it, and it imports nothing from this project except `config`-free helpers
(`internal/id`, `internal/log`, `internal/shutdown`). The lab is meant to look like
somebody else's estate, and sharing types with the application that diagnoses it would be
a coincidence the agent could learn from.

## Gotchas

**payment-service is much harder to starve than checkout-service, and the scenarios say
so.** `checkout-cpu` uses `DefaultParams().Workers` (2) and `payment-cpu` uses
`lab.MaxCPUWorkers` (32). checkout's handler serializes orders, which is real CPU work, so
two spinners slow it. payment's `/charge` spends most of its time asleep inside the
simulated processor call, and the container is limited to one CPU from which Go derives
`GOMAXPROCS=2` — so two spinners leave it answering in 87 ms and there is no incident at
all. At 32 it reaches a p99 of about 900 ms against its 800 ms budget, which is
diagnosable but thin. An evaluation run measured the 87 ms version and reported, correctly,
that nothing was wrong.

**`scenario.Driver` is the second way to drive the lab.** `scenario.Run` has fixed
baseline, fault and recovery phases and ends in a metrics report; `Driver` starts a fault,
holds it with traffic flowing for as long as the caller needs, and stops. `internal/e2e`
uses the second, because an investigation lasts however long the model takes.


- **The metric names are a contract with the corpus**, not a naming choice.
  `TestRegisteredMetricNames` fails on a rename in either direction, because a runbook step
  that queries a metric nobody exports is worse than no runbook.
- **The processor call is not cancellable, and the pool wait is.** Once a connection is
  held the charge has been dispatched, so a caller that gives up does not stop it. That is
  what keeps the pool saturated for the full hold time and what makes checkout's 504 cover
  an authorization that actually succeeded — the case
  `checkout_payment_client_timeouts_total` counts.
- **The latency fault is applied in two different places**, on purpose.
  checkout-service applies it in the middleware (`LatencyFaultInMiddleware`);
  payment-service applies it inside the processor call, while a connection is held, which
  is the only reason the pool fills.
- **`RouteKind` decides more than it looks like.** `/metrics` is neither measured nor
  logged, or the scrape counts as traffic; `/fault` is exempt from the error fault, or a
  fault at ratio 1 could not be turned off; `/health` is measured, because the CPU
  runbook's signal is that saturation slows even the endpoints that call nothing.
- **Demand on the pool is the request rate times how long a charge takes.** A closed-loop
  load generator cannot express that, which is why `scenario` paces itself. Raising the
  request rate past `pool ÷ hold time` turns the latency scenario into an error-rate one.
- **The cpu fault's goroutines have two exit paths**: disabling the fault and `Close`,
  which `App.Run` registers with the shutdown group. Both wait for the goroutines, so a
  disabled fault is really gone.
- **Fault state is in memory and per process.** A restarted container is a silently healthy
  one.
