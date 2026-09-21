# Postmortem: payment-service CPU regression, 2026-07-19

## Impact

Latency on every payment-service endpoint roughly doubled for 80 minutes. The p99 of
`POST /charge` reached 1.6 seconds against a budget of 800ms. No requests failed, no pool
was saturated, and the card processor was unaffected throughout.

checkout-service stayed within its own SLO because its client timeout has headroom above
payment-service's budget, so no orders were lost. The incident was invisible to customers
and visible only as a breached objective.

## Timeline

02:10 — A deployment added per-request signature verification to the authorization path.

02:14 — `rate(process_cpu_seconds_total{job="payment-service"}[5m])` rose from 0.4 to 0.94
as a step change, and `container_cpu_cfs_throttled_seconds_total` began climbing.

02:31 — `PaymentServiceLatencyHigh` fired. The on-call engineer checked the pool first,
found the in-use ratio at its usual 0.4, and moved on.

02:40 — Latency was observed to have risen on `/health` as well as on `POST /charge`. A
health endpoint that calls nothing and has become slow does not have a dependency problem,
and that observation pointed at CPU within a couple of minutes.

03:30 — The deployment was reverted. CPU returned to 40% and latency to baseline within
three minutes.

## Root cause

The signature verification recomputed a key derivation on every request rather than caching
it. The derivation is deliberately expensive — that is what makes it useful — and it was
placed on the hot path.

## Contributing factors

Load tests ran on an unconstrained developer machine with far more CPU than a production
replica, so the regression was invisible in testing. The change looked cheap because the
machine it was measured on had headroom production does not.

The uniform rise across endpoints is the signal that separates CPU saturation from a slow
dependency, and it was not written down anywhere at the time. The engineer who noticed
`/health` had also slowed down had seen the pattern before rather than read about it.

## What we changed

The key derivation is cached.

Load tests run against a CPU limit matching production, so a regression of this shape fails
before it ships.

The CPU saturation runbook now states the uniformity signal explicitly, including the
`/health` check, so the next person does not have to have seen it before.
