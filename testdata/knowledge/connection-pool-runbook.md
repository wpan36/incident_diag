# Connection pool exhaustion runbook

## Symptoms

Requests queue while CPU and memory look idle. Latency rises with no corresponding rise in
downstream latency, and the time spent waiting for a resource grows faster than the time
spent using it.

This shape is distinctive and worth recognising quickly, because it is the one case where
every obvious resource metric looks healthy while the service is plainly not. Nothing is
saturated except a number in a configuration file.

## Diagnosis

Every pooled client in this estate exports three metrics: `*_pool_in_use`, `*_pool_size`
and `*_pool_wait_seconds`. A saturated pool is an in-use ratio near 1 with a rising wait
time. An idle pool with rising latency means the bottleneck is somewhere else and this
runbook does not apply.

Check the wait time before the ratio. A pool can sit at full utilization indefinitely
without anyone suffering, as long as connections are returned as fast as they are
requested; it is the queue in front of it that hurts. A ratio of 1.0 with a flat wait time
is a pool that is exactly the right size.

## Sizing

A pool should be large enough that the in-use ratio sits below 0.7 at peak, and small
enough that all replicas together stay within whatever the far side allows.

The second constraint is the one people forget. A pool size is per replica, so scaling out
multiplies it. Exceeding what the far side permits does not produce a clear error: it
produces refused connections that look like an intermittent network fault, and it moves the
failure out of the service that caused it and into a shared dependency where several teams
will investigate it at once.

Write the ceiling down next to the pool size. `PAYMENT_POOL_SIZE` is capped by the card
processor's 200 connections per tenant; the ledger pool is capped by MySQL's `max_connections`
shared across every service that talks to it.

## Remediation: raise the size

Raise it within the downstream ceiling and watch the wait time rather than the ratio: the
ratio will fall immediately and mean nothing, while the wait time falling is the evidence
that the queue drained.

Derive the value from the replica count rather than setting a constant. A pool sized for
two replicas and then left alone through a scale-out is the single most common way this
incident recurs.

## Remediation: reduce concurrency

Fewer concurrent requests need fewer connections. Load shedding, a smaller worker pool
upstream, or a queue in front of the service all work, and all of them are preferable to
raising a pool size that is already at the downstream ceiling.

## What not to do

Never raise a pool size to work around a slow dependency. The pool is full because
connections are being held longer, and more connections held just as long means more
concurrent load on something that is already failing to keep up. This converts a latency
incident into an outage, and it does so several minutes after the change, which makes the
cause hard to see.
