# Postmortem: checkout 504s with a healthy payment-service, 2026-05-02

## Impact

checkout-service returned 504 on 8% of `POST /orders` for one hour and 44 minutes.
payment-service was healthy throughout: its own p99 never exceeded 700ms, its error rate was
flat, and no pool was saturated.

Approximately 3,200 orders failed from the customer's point of view. Because the failure was
a client-side timeout rather than a downstream error, most of those authorizations had
already succeeded, and the processor was charged for work no order ever used.

## Timeline

13:20 — A deployment of checkout-service lowered `CHECKOUT_PAYMENT_TIMEOUT` from 2s to
500ms. The change was part of an unrelated piece of work to tighten timeouts across the
service and was reviewed by two people.

13:21 — Authorizations taking between 500ms and 700ms — the top decile, and entirely normal
— began failing immediately.

13:34 — `CheckoutServiceLatencyHigh` fired on the 504 rate. The first fifteen minutes were
spent looking at payment-service, which was healthy, because a 504 mentioning a downstream
service reads like a downstream problem.

14:02 — Someone compared `checkout_payment_client_timeouts_total` against
payment-service's request count and found the gap: checkout was abandoning calls that the
other side had completed.

15:18 — The deployment was reverted and the 504 rate returned to zero within a minute.

## Root cause

The client timeout was set below payment-service's documented p99 budget of 800ms. Every
call slower than the new timeout failed even though the work completed successfully on the
other side.

The timeout was chosen by reasoning about what a user would tolerate rather than by reading
what the callee promised. Both are legitimate inputs; used alone, the first guarantees
failures at exactly the rate of the callee's tail.

## Contributing factors

Nothing in review connected the new value to payment-service's SLO. The two numbers lived
in different repositories owned by different teams, and no tooling related them.

The 504 rate alert names checkout-service, and the first instinct on seeing a 504 is to
look at whatever is downstream. Forty minutes were spent confirming that a healthy service
was healthy.

## What we changed

Client timeouts on the critical path are derived from the callee's published SLO, and a
test asserts the relationship, so lowering one below the other fails in CI rather than in
production.

The checkout latency runbook now leads with this diagnosis, because it is both the most
common cause of checkout 504s and the one that looks most like something else.

payment-service's published budget is treated as an interface. Changing it requires telling
the storefront team, in the same way a breaking API change would.
