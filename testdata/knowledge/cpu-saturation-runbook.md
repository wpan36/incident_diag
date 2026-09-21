# CPU saturation runbook

## Symptoms

`rate(process_cpu_seconds_total{job="<service>"}[5m])` approaches the replica's CPU
allowance. Latency rises across every endpoint at once, including endpoints that call
nothing downstream.

`container_cpu_usage_seconds_total` and `container_cpu_cfs_throttled_seconds_total` say the
same thing from the container's side and are better when they are available, but they come
from cAdvisor and are empty on some hosts. The per-process rate is exported by the service
itself and is always there, so start with it.

The uniformity is the signal. A slow dependency raises latency only on the routes that use
it, so a service whose health endpoint has also slowed down is not waiting on anything — it
cannot get scheduled.

Throttling matters more than raw usage. A container at 95% of its limit with no throttling
is busy; one at 80% with steady throttling is being stopped mid-request several times a
second, and the latency it reports is mostly time spent not running.

## Diagnosis: was it a deployment

Check whether the rise is a step change that lines up with a release. A regression in a hot
path is the most common cause and the easiest to act on, and a step change is what
distinguishes it from a traffic-driven rise, which is gradual and tracks request rate.

Per-request work that used to be done once — a key derivation, a regex compile, a config
parse, a JSON round trip — is the usual culprit. These are cheap enough to pass review and
expensive enough to matter at a thousand requests a second.

## Diagnosis: was it traffic

Compare CPU against request rate. If the ratio is unchanged and both rose, this is capacity
rather than a regression, and the remediation is scaling rather than reverting.

## Diagnosis: is it one replica or all of them

A single hot replica usually means uneven load balancing or a poisoned cache on that
instance, not a code problem. Restarting the one replica is diagnostic as well as
remedial: if it comes back hot, the load balancer is the problem.

## Remediation

Scale out if the work parallelizes across requests, which it does for anything
request-scoped. Revert if a deployment introduced it; a revert is faster than a fix and the
fix can be written calmly afterwards.

Raising the CPU limit is a last resort. It hides the regression, costs capacity that
another workload needed, and moves the bottleneck to whatever is next rather than removing
it. When it is the right call it is because the service was genuinely under-provisioned,
and that should be visible as a long-standing trend rather than as today's step change.
