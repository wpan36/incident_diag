# Incident Lab

**Tier B spec (M13).** Two services that break on demand, so the agent is evaluated against
a system that actually fails rather than against fixtures. M14 scrapes them, M15 scripts the
scenarios.

## Problem

M12's corpus is not generic. Its runbooks name the metrics a responder is told to check —
`payment_pool_in_use`, `payment_pool_wait_seconds`, `payment_processor_latency_seconds`,
`checkout_payment_client_timeouts_total` — and M15 reproduces the payment-service latency
incident those runbooks describe. A lab exposing only request duration would leave the
agent retrieving a correct runbook whose first diagnostic step queries nothing.

M13 is also the first producer of the log format S6 defines, and `internal/log` does not
emit two things that format requires.

## Technical Plan

### Topology

`checkout-service` calls `payment-service` over HTTP. `payment-service` calls a **simulated
card processor** through a fixed-size connection pool. No MySQL, no Kafka: one saturable
pool is enough to produce the failure shape the corpus describes.

| Service | Routes |
| --- | --- |
| `checkout-service` | `POST /orders`, `GET /orders/:id`, `GET /health`, `GET /metrics`, `/fault` |
| `payment-service` | `POST /charge`, `POST /refund`, `GET /health`, `GET /metrics`, `/fault` |

Route names come from the corpus, including `/health` rather than the platform's
`/healthz`. `GET /orders/:id` earns its place by calling nothing downstream: the CPU
runbook's central distinction — saturation raises latency on every endpoint, a slow
dependency only on the routes that use it — is not observable without it.

`POST /orders` validates the basket, calls `POST /charge` under
`CHECKOUT_PAYMENT_TIMEOUT`, stores the order in memory and answers 201. A client timeout
increments `checkout_payment_client_timeouts_total` and answers **504**, which is the
correctness case the checkout runbook is about: the charge may have succeeded on the other
side.

### The pool is the model, not a fault switch

`POST /charge` acquires one of `PAYMENT_POOL_SIZE` tokens, spends
`PAYMENT_PROCESSOR_LATENCY_MS` (plus the latency fault, if enabled) in the simulated
processor, and releases it. Waiting for a token past the request deadline answers **503**,
per the error-rate runbook's reading of 503 as self-protection.

This is what makes M15's scenario a causal chain rather than a staged one: raising the
processor latency holds connections longer → the in-use ratio approaches 1 → the wait
histogram climbs → checkout's client latency rises → checkout times out and returns 504s.
Every step the payment latency runbook tells a responder to check is a real consequence of
the step before it.

### Metrics

Exactly the names the corpus uses. A table-driven test asserts each one is registered, so a
rename in either direction is a test failure rather than a silently dead runbook step.

| Metric | Type | Where |
| --- | --- | --- |
| `http_request_duration_seconds` | histogram, labels `method`, `route`, `status` | both |
| `payment_pool_size`, `payment_pool_in_use` | gauge | payment |
| `payment_pool_wait_seconds` | histogram | payment |
| `payment_processor_latency_seconds` | histogram | payment |
| `checkout_payment_client_latency_seconds` | histogram | checkout |
| `checkout_payment_client_timeouts_total` | counter | checkout |

`prometheus.DefBuckets` reaches 10s, past both alert thresholds (payment 2s, checkout 3s),
so the default buckets stand.

**M14 must set the scrape job label to the service name.** The runbooks say "the p99 of
`http_request_duration_seconds` for checkout-service"; without `job="checkout-service"`
there is nothing to filter on.

`container_cpu_usage_seconds_total` and `container_cpu_cfs_throttled_seconds_total` belong
to cAdvisor, so **M14 adds cAdvisor to Compose**. The services do not export those names
themselves: publishing invented numbers under another exporter's metric name is worse than
not having them.

### `/fault`

One endpoint, one body, dispatched on `kind`. M15 scripts a single URL rather than three.

```
POST   /fault   {"kind":"latency","enabled":true,"delay_ms":2500,"jitter_ms":500}
POST   /fault   {"kind":"error","enabled":true,"status":503,"ratio":0.2}
POST   /fault   {"kind":"cpu","enabled":true,"workers":2}
GET    /fault   → every kind's current state
DELETE /fault   → clears all of them
```

The payload's fields vary by kind, so decoding is two-step: `kind` first, then the rest.

- **latency** — on payment it is added to the *processor* call, inside the pool, so the
  chain above follows. On checkout it is added to the handler. The difference is the point.
- **error** — `ratio` of requests answer `status` before doing any work.
- **cpu** — `workers` goroutines spin until the fault is disabled or the process shuts
  down. The controller holds their cancel function; both paths call it.

State is in memory and per process. A restart is a reset, which is the expected way to
clean up after a scenario.

### Logging

`internal/log` gains the two things S6's format requires:

- **`New(w io.Writer, level slog.Level, service string)`.** The attribute is attached once,
  through the existing `With` path, rather than at call sites — the same argument the
  package already makes about correlation ids. `cmd/api` and `cmd/ingestion-worker` change.
- **UTC timestamps**, forced in `HandlerOptions.ReplaceAttr`. Containers do not agree on a
  time zone and a time-window filter across services that disagree is silently wrong.

The lab services log to `io.MultiWriter(os.Stdout, file)`, where the file is
`<service>.log` under `LAB_LOG_DIR`, so `docker compose logs` still works. Each request
logs at INFO with `method`, `route`, `status` and `duration_ms`; a 5xx logs at ERROR and a
pool wait over one second at WARN. `read_service_logs`' `min_level` filter needs levels
that actually vary.

### Packaging

First Dockerfiles in the repository: one multi-stage `Dockerfile` at the root with a build
argument naming the `cmd/` package, so the two lab services and M16's `ops-mcp` share it.
Both run in Compose, writing to the bind mount `./data/lab-logs`, which `ops-mcp` mounts
read-only later. `/data/` is already the gitignored bind-mount root.

### Configuration

`config.LoadCheckout` and `config.LoadPayment`, beside the existing loaders, using the
corpus's variable names where it has them:

```
CHECKOUT_PAYMENT_URL, CHECKOUT_PAYMENT_TIMEOUT
PAYMENT_POOL_SIZE, PAYMENT_PROCESSOR_LATENCY_MS
LAB_LOG_DIR
```

`config.Load` supplies `Env`, `LogLevel` and `HTTP_ADDR` as it does for every binary.

## Alternatives

**Emit only `http_request_duration_seconds`.** Half the code. Rejected: the runbooks M12
already wrote name the other metrics, and a diagnostic step that queries nothing is worse
than no runbook.

**Export `container_cpu_*` from the services.** Removes a Compose dependency. Rejected:
those names mean cAdvisor's measurement, and reusing them for a self-reported number makes
the metric a lie.

**Three fault endpoints instead of one.** Simpler decoding, no kind dispatch. Rejected:
M15's script would carry three URLs and their combinations.

**gin, already a dependency.** Rejected: six routes between two services need routing, not
a framework, and the lab is better off independent of the main application's stack.

**Real MySQL and Kafka in the lab.** Matches the corpus's description of checkout. Rejected:
the ledger and `order.placed` add operational surface without adding a failure mode the
corpus asks the agent to diagnose.

## Detailed Implementation

- `internal/lab/` — the HTTP server skeleton (routes, the duration middleware, `/health`,
  `/metrics`, graceful shutdown through `internal/shutdown`), the fault controller, and the
  pool. Shared because both services need all of it.
- `cmd/checkout-service/`, `cmd/payment-service/` — wiring only: load config, build the
  logger and the log file, register that service's metrics and routes, run.
- `internal/log/` — the `service` parameter and the UTC `ReplaceAttr`; two call sites and
  the package's own tests.
- `internal/config/` — `LoadCheckout`, `LoadPayment`, and their variables added to
  `isolate` in `config_test.go`, or the tests inherit a developer's shell.
- `Dockerfile`, `deploy/docker-compose.yml`, `.env.example`.
- New dependency: `github.com/prometheus/client_golang`.

## Verification

- `make check` — fault controller state and the cpu fault's goroutine exit (leak test),
  error-fault ratio, pool acquire and its deadline path, the registered-metric-names table,
  and a log record parsed back to assert the S6 fields and UTC.
- Smoke: `docker compose up`, `POST /orders` answers 201, `/metrics` contains every name in
  the table above, `./data/lab-logs/payment-service.log` holds JSON lines; then the latency
  fault on payment, and the in-use ratio, wait histogram and checkout 504s all move.

## Known limitations, accepted

**One replica each.** The corpus's replica-count reasoning — pool size multiplied across
replicas, one hot instance behind a load balancer — cannot be reproduced, only read about.

**Basket size does not drive CPU**, though the corpus says it does. The cpu fault is the
switch instead. Modelling it would mean a serialization cost curve tuned to look real.

**`payment_shed_low_value` is not implemented.** It is a remediation, and the agent is
read-only (ADR 0005), so nothing in the system would ever turn it on.

**Fault state is in memory**, so a restarted container is a silently healthy one.
