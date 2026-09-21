# checkout-service

## What it does

checkout-service turns a shopping basket into an order. It validates the basket, calls
payment-service to authorize the card, writes the order, and publishes an `order.placed`
event for fulfilment.

It exposes `POST /orders`, `GET /orders/:id`, and the usual `/health` and `/metrics`. It is
the only writer of the orders table.

## Dependencies

payment-service is the only synchronous dependency on the critical path, and the only one
whose latency is visible to the customer. The HTTP client timeout for that call is
`CHECKOUT_PAYMENT_TIMEOUT`.

That timeout is derived from payment-service's published p99 budget of 800ms with headroom,
not chosen independently. A value below the callee's budget fails a share of entirely
normal requests, and because the authorization has already happened on the other side, the
customer can be charged for an order that does not exist.

MySQL for the orders table, and Kafka for the `order.placed` event. The event is published
after the order is written and is not part of that transaction, so a lost event is a
fulfilment delay rather than a lost order.

## Service level objective

The p99 of `POST /orders` must stay under 1500ms: payment-service's 800ms budget plus this
service's own validation, persistence and serialization.

## Known cost centres

Order serialization is CPU-heavy and scales with basket size. A basket with hundreds of
lines can saturate a replica on its own, which is why checkout's latency alerts are as
often about its own CPU as about payment-service.

## Ownership

The storefront team owns it. The on-call rotation is `storefront-oncall`.
