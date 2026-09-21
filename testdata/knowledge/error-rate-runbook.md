# Error rate runbook

## Symptoms

The 5xx rate on any service exceeds 1% of requests for five minutes. Unlike a latency
alert this usually means something is failing outright rather than being slow, and it is
the more urgent of the two.

Check whether latency is also elevated. A service that is both slow and failing is
failing, and this runbook takes precedence over the latency one: the errors are the
informative signal and the latency is usually a consequence of retries.

## Diagnosis: separate 500 from 503

Do this before anything else. They are different incidents that happen to share an alert.

A 500 is an unhandled error inside the service. It belongs to whoever deployed last, and
the fastest diagnosis is almost always the diff. Look at whether the errors are on one
route or all of them: one route is a code path, all of them is usually a dependency the
service cannot start without.

A 503 is usually deliberate. Load shedding, a closed circuit breaker, a readiness probe
failing, or a pool refusing to hand out a connection all produce 503, and all of them mean
the service is protecting itself from something further down. The service emitting the 503
is rarely the service with the problem.

## Diagnosis: 503 from load shedding

If `payment_shed_low_value` is enabled, payment-service returns 503 for authorizations
below ten dollars by design. The 5xx rate rising after someone enables it during a latency
incident is the flag working, not a second incident, and checkout-service should be
surfacing those as declined payments rather than as errors.

Check the flag before investigating a 503 spike on payment-service. Several people have
spent the first ten minutes of an incident diagnosing a mitigation someone else turned on.

## Diagnosis: 504 is not in this runbook

A 504 means the service gave up waiting on something, which is a timeout problem and not
an error-rate one. checkout-service's 504s in particular are covered by its own latency
runbook, because the usual cause is a client timeout set below the callee's published
budget rather than anything failing.

## Remediation

For a 500, revert the most recent deployment. Diagnose afterwards; a revert is faster than
a fix and it restores the service while the cause is still being read.

For a 503, find what the service is protecting itself from. Fixing the dependency is the
only remediation that lasts. Disabling the protection — raising the breaker threshold,
turning off shedding — moves the failure into the dependency and usually makes the blast
radius larger.

## Escalation

Page the owning team's rotation. If the 503s trace to a shared dependency, page that team
as well: the service emitting the errors is the symptom and the shared dependency is where
the work is.
