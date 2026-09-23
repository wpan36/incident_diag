# Runbook: latency with no obvious cause

## When to use this

Use it when a service is measurably slower than its budget and the usual suspects have
already been ruled out: the connection pool is not saturated, the dependency it calls is
fast, its CPU is not near its limit, and no deployment went out.

That combination is where most investigations stall. The three causes below are the ones
that produce it, they are checked in the order given, and each is ruled in or out with
metrics every service here already exports.

## Diagnosis: are we slower, or are we busier

Check this first, because it is the cheapest and because the remediation is the opposite of
every other cause's. A service that is handling twice the traffic at the same cost per
request is not degraded; it is under-provisioned, and tuning it will not help.

Compare the request rate against the latency over the same window:
`sum(rate(http_request_duration_seconds_count{job="<service>"}[5m]))`. If the rate rose at
roughly the same time and in roughly the same proportion as the latency, this is a demand
problem. If the rate is flat and the latency rose anyway, the service really did get
slower, and one of the other causes applies.

**Look for retry amplification before concluding it is organic growth.** A caller that
retries a slow callee multiplies the load on exactly the service least able to absorb it,
and the multiplication compounds: one retry doubles the offered load, and a caller that
retries twice quadruples it. The signature is the callee's request rate rising while the
caller's rate is flat — the extra requests are being manufactured in between. Compare
`sum(rate(http_request_duration_seconds_count{job="payment-service"}[5m]))` against
checkout's own rate on the routes that call it. If they have diverged, the caller's retry
policy is the incident and raising capacity behind it makes the storm larger, not smaller.

## Diagnosis: is the runtime pausing

Garbage collection stops the service, so it produces latency that shows up in every
endpoint at once while CPU looks unremarkable and nothing downstream is slow. It is the
cause most often mistaken for a dependency problem.

Check `rate(go_gc_duration_seconds_sum{job="<service>"}[5m])` — the fraction of each second
spent collecting — alongside `go_memstats_heap_inuse_bytes` and
`rate(go_memstats_alloc_bytes_total{job="<service>"}[5m])`. Collection time rising with the
allocation rate means the service is producing garbage faster than before, usually because
a request path started allocating per item what it used to allocate once. Collection time
rising while the allocation rate is flat means the live heap grew, so each cycle has more
to scan; look at the leak section below.

The discriminator against CPU saturation is that GC pauses do not pin the CPU: a service
losing time to collection shows elevated latency at moderate
`rate(process_cpu_seconds_total{job="<service>"}[5m])`. The discriminator against a slow
dependency is uniformity — a collection pause delays endpoints that call nothing, so probe
`/health` and see whether it slowed too.

## Diagnosis: is something leaking

A leak is the cause that explains an incident with no trigger. Nothing was deployed and no
traffic changed because the change was three hours ago and has been accumulating since.

Check `go_goroutines{job="<service>"}` and `process_resident_memory_bytes{job="<service>"}`
over the longest window available. Both should be flat or should track the request rate. A
line that climbs monotonically and never returns to its earlier level after traffic drops
is a leak, and the time it started is the change that caused it.

Goroutines climbing in step with requests that never complete usually means a call with no
timeout: every abandoned request holds its goroutine, its connection and whatever the
handler allocated. That also drains a connection pool, so a leak and a pool exhaustion can
be the same incident seen from two ends — check whether `payment_pool_in_use` rose at the
same time and treat the leak as the cause rather than the pool.

Restarting the process clears a leak and destroys the evidence. Capture the metrics for the
window first, because after the restart the only thing that will be true is that it got
better.
