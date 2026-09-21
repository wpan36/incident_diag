# checkout-service latency runbook

## Symptoms

The alert `CheckoutServiceLatencyHigh` fires when the p99 of
`http_request_duration_seconds` for checkout-service stays above 3 seconds for five
minutes. It frequently fires together with a rising 504 rate on `POST /orders`, and the
combination is more informative than either alone.

A 504 from checkout-service means checkout gave up on something, not that something
returned an error to it. That distinction is the whole of this runbook: the same symptom
has two causes that need opposite responses.

## Diagnosis: downstream timeouts with a healthy payment-service

This is the most common cause and the most commonly misdiagnosed.

If checkout-service is returning 504s while payment-service's own p99 is normal, the
problem is on this side. The HTTP client timeout `CHECKOUT_PAYMENT_TIMEOUT` is shorter than
payment-service's actual response time, so checkout abandons calls that would have
succeeded.

Compare `checkout_payment_client_timeouts_total` against payment-service's request count
for the same window. A large gap means checkout is abandoning work that completed on the
other side — the authorization happened, the customer may have been charged, and no order
exists for it. Treat that as a correctness incident, not only a latency one.

The timeout must be derived from payment-service's published SLO of 800ms at p99, with
headroom. A value below that budget fails a share of entirely normal requests, and the
share is not small: the top decile of authorizations routinely takes 600 to 700ms.

## Diagnosis: checkout's own CPU

Check `container_cpu_usage_seconds_total` and `container_cpu_cfs_throttled_seconds_total`
for checkout-service. Order serialization is CPU-heavy and a basket with hundreds of lines
can saturate a replica with no downstream involvement at all.

The distinguishing signal is uniformity. CPU saturation raises latency on every endpoint at
once, including ones that never call payment-service. A downstream problem raises latency
only on the routes that call the thing that is slow.

## Diagnosis: is payment-service actually slow

Check payment-service's own p99 before anything else, because it changes which of the two
diagnoses above applies. If it is elevated, this is not a checkout incident: follow the
payment-service latency runbook and expect checkout to recover when it does.

## Remediation: the timeout

Raise `CHECKOUT_PAYMENT_TIMEOUT` only after confirming payment-service is healthy.

Raising it while payment-service is slow converts fast failures into slow ones, holds
checkout's own worker pool for longer, and propagates the saturation upstream. That is how
one slow dependency takes down two services instead of one.

## Remediation: CPU

Scale out rather than up. The serialization work parallelizes across requests but not
within a single one, so more replicas help and a larger CPU limit per replica does not.

If the rise followed a deployment, revert it. A regression in the serialization path is the
most common cause of a step change in checkout's CPU.

## Escalation

Page the storefront on-call through the `storefront-oncall` schedule. If the diagnosis
lands on payment-service, page `payments-oncall` as well rather than instead: the caller
usually notices first and the callee usually has to act.
