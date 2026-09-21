# payment-service

## What it does

payment-service authorizes card payments on behalf of checkout-service. It is the only
service permitted to hold credentials for the card processor, and every authorization in
the system passes through it.

It exposes `POST /charge` for authorization, `POST /refund`, and the usual `/health` and
`/metrics`. Nothing else in the estate is allowed to call the processor directly, so a
feature that needs payment data needs an endpoint here rather than a second integration.

## Dependencies

The card processor, reached over HTTPS through a connection pool sized by
`PAYMENT_POOL_SIZE` per replica. The processor allows 200 concurrent connections per tenant
across all replicas, which is the hard ceiling on that pool.

MySQL, for the authorization ledger, through a second pool. The ledger is the record of
what was authorized and is the reason a lost response is worse than a slow one: an
authorization the caller never learns about has still charged the customer.

No asynchronous dependencies. Everything on the request path is synchronous, which is why
latency here shows up immediately in checkout-service.

## Service level objective

The p99 of `POST /charge` must stay under 800ms. Availability is 99.9% over a rolling 30
days. Breaching either for two consecutive hours is an incident.

The 800ms figure is not arbitrary and is not ours alone: checkout-service derives its own
client timeout from it, so lowering the published budget without telling the storefront
team breaks their timeout in a way that looks like our outage.

## Capacity

Four replicas at the time of writing. `PAYMENT_POOL_SIZE` is derived from the replica count
in the deployment template rather than set as a constant, following the March 2026 incident
in which a pool sized for two replicas was left in place through a scale-out.

## Ownership

The payments team owns it. The on-call rotation is `payments-oncall`, and the processor
vendor's escalation path needs the tenant identifier from `PAYMENT_TENANT_ID`.
