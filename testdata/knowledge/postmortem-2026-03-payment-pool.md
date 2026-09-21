# Postmortem: payment-service pool exhaustion, 2026-03-14

## Impact

Card authorizations were degraded for 47 minutes. The p99 of `POST /charge` peaked at 6.2
seconds against a budget of 800ms, and checkout-service abandoned 11,400 orders after its
client timeout expired.

Roughly 900 of those abandoned calls had already been authorized by the card processor by
the time checkout gave up. Those customers were charged for orders that were never created,
and the refunds were issued manually over the following two days.

## Timeline

09:12 — A marketing campaign went live and traffic to `POST /charge` tripled within four
minutes. No pre-announcement had reached the payments team.

09:16 — `payment_pool_in_use` reached its maximum of 20 and stayed there.
`payment_pool_wait_seconds` began climbing.

09:23 — `PaymentServiceLatencyHigh` fired. CPU on every replica was under 30% and the card
processor's own latency was flat, which ruled out both of the other diagnoses in the
runbook within a few minutes.

09:31 — checkout-service's 504 rate crossed 5% and `storefront-oncall` was paged. Two teams
were now investigating the same incident from opposite ends.

09:41 — `PAYMENT_POOL_SIZE` was raised from 20 to 50 and the deployment restarted.

09:59 — The queue drained, the wait time returned to baseline, and latency recovered.

## Root cause

`PAYMENT_POOL_SIZE` had been set to 20 when payment-service ran on two replicas. The
service was scaled to four replicas in January and the pool size was never revisited, so
each replica still held a pool sized for half the traffic it was now serving.

The pool was not undersized for the campaign in particular. It had been undersized since
January and the campaign only supplied enough traffic to make it visible.

## Contributing factors

Nothing alerted on pool utilization. The in-use ratio had been above 0.8 during every
evening peak since January and nobody saw it, because the only dashboard showing it was one
the payments team opened during incidents.

The runbook at the time said to raise the pool size without saying to check the processor
first. That was the right action here and would have been the wrong one if the processor
had been the bottleneck.

## What we changed

Pool utilization is alerted on at 0.8, so this is now discovered before customers find it.

`PAYMENT_POOL_SIZE` is derived from the replica count in the deployment template rather
than being a constant someone has to remember to revisit after a scale-out.

The latency runbook now separates the pool diagnosis from the processor diagnosis and says
which remediation belongs to which, because raising the pool size against a slow processor
would have made this incident considerably worse.

Campaign launches are announced to the payments team, which is a process fix and therefore
the one least likely to still be working in a year.
