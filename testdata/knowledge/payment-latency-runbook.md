# payment-service latency runbook

## Symptoms

The alert `PaymentServiceLatencyHigh` fires when the p99 of `http_request_duration_seconds`
for payment-service stays above 2 seconds for five minutes. It is a latency alert, not an
availability one: the error rate usually stays flat while this happens, and that is the
first thing that distinguishes it from an outage.

Callers see it before the alert does. checkout-service will report a rising
`checkout_payment_client_latency_seconds` and, if the condition persists, will begin
abandoning calls once they exceed its own client timeout. A page from checkout that names
payment-service is usually this alert arriving by a slower route.

What this alert does not mean is that payment-service is returning errors. If the 5xx rate
is also elevated, stop here and use the error rate runbook instead: a service that is both
slow and failing is failing, and the failure is the more informative signal.

There are three causes below and they are checked in order: the connection pool, the card
processor, then payment-service's own CPU. Do not stop after the first two. If the pool is
idle and the processor is fast and callers are still timing out on us, the answer is
almost always the third, and it is the one whose symptoms hide from our own latency
metric.

If all three are clean and the latency is real, continue with the unexplained latency
runbook: demand and retry amplification, garbage collection, and leaks. Those are what is
left once the pool, the processor and the CPU have been excluded.

## Diagnosis: is the processor connection pool saturated

Check `payment_pool_in_use / payment_pool_size` in Prometheus. Sustained above 0.9 means
requests are queuing for a connection to the card processor rather than doing work.
Confirm with `payment_pool_wait_seconds`: if the time spent waiting for a connection is
growing faster than the time spent using one, the pool is the bottleneck.

A saturated pool with idle CPU is the classic shape. If CPU is also near its limit, the
pool may be saturated as a consequence rather than a cause — connections are held longer
because the process cannot get scheduled to release them — and the CPU saturation runbook
applies first.

The pool is sized by `PAYMENT_POOL_SIZE` per replica. Multiply it by the replica count
before concluding anything: the same value is fine at two replicas and reckless at eight.

## Diagnosis: is the processor itself slow

Check `payment_processor_latency_seconds`. If it is elevated at the same time, the pool
saturation is a symptom and not the cause. Connections are being held longer because the
far side is slow, so the pool fills whatever its size.

This distinction decides the remediation, and getting it backwards makes the incident
worse. Raising the pool size when the processor is the bottleneck sends more concurrent
work to something already failing to keep up, which is how a slow dependency becomes a
failed one.

The processor publishes its own status page. Check it before assuming the problem is
local, particularly if the latency rose without any deployment on our side.

## Diagnosis: is payment-service CPU-bound

Check this whenever the pool is idle and the processor is fast and callers are still
timing out on us. It is the case that looks like nothing is wrong here, and it is the one
most often blamed on the caller.

Run `rate(process_cpu_seconds_total{job="payment-service"}[5m])`. A replica is limited to
one CPU, so a rate approaching 1.0 means the process is saturated and everything it does
is waiting for the scheduler. `container_cpu_cfs_throttled_seconds_total` says the same
thing from the container's side and is the better signal where cAdvisor has data;
throttling matters more than raw usage, because a throttled process is one being stopped
rather than one that is merely busy.

**Our own latency metric will not show this, and that is the trap.** The p99 of
`http_request_duration_seconds` is measured inside the handler. When the CPU is saturated
the delay is in getting scheduled to run the handler at all, so our histogram can sit
under 250ms while checkout-service is abandoning calls at its two-second timeout. If the
caller's numbers and ours disagree that badly, do not conclude the caller is at fault:
that gap is the signature of this cause. `checkout_payment_client_latency_seconds` is
measured around the whole call and will show the truth.

Confirm with the uniformity check from the CPU saturation runbook: probe `/health`, which
touches no pool and no processor. If that has slowed down too, the process cannot get
scheduled and nothing downstream is to blame.

Do not raise `PAYMENT_POOL_SIZE` here. More concurrent work on a process that cannot get
scheduled makes it slower, not faster.

## Diagnosis: was there a deployment

Check whether the rise followed a release of payment-service. A regression in the
authorization path shows up as latency that rises uniformly across every endpoint rather
than on `POST /charge` alone, because the hot path is shared. If that is the shape, revert
first and diagnose afterwards.

## Remediation: raise the pool size

Only when the processor is healthy and CPU is not the constraint.

Set `PAYMENT_POOL_SIZE` to 50 and restart the deployment. Watch the in-use ratio: it should
fall below 0.7 within two minutes. The processor allows 200 concurrent connections per
tenant across all replicas, so four replicas at 50 is exactly the ceiling and there is no
headroom above it. If four replicas at 50 is not enough, the answer is fewer concurrent
requests, not more connections.

## Remediation: shed load

Enable the `payment_shed_low_value` flag. It rejects authorizations below ten dollars with
a 503 so that high-value traffic keeps its connections. Expect the 5xx rate to rise — that
is the flag working, not a new problem — and expect checkout-service to surface those as
declined payments rather than as errors.

This is the correct action when the processor is the bottleneck, because it reduces
concurrent load on the far side rather than increasing it.

## Escalation

Page the payments on-call through the `payments-oncall` schedule. If the processor is
implicated, the vendor's escalation path is in the payments team's handbook and needs the
tenant identifier from `PAYMENT_TENANT_ID`. The platform on-call is the fallback and cannot
help with processor issues.
