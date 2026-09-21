# Order Service Observability Assessment

[![CI](https://github.com/iho/md_test_task/actions/workflows/ci.yml/badge.svg)](https://github.com/iho/md_test_task/actions/workflows/ci.yml)

This repository contains the Task 3 design documents and a runnable Go implementation with:

- W3C trace extraction/injection and exported spans;
- trace-correlated structured logs with centralized PII redaction;
- bounded Prometheus business metrics and trace exemplars;
- a pure order domain with domain-event translation at the usecase boundary;
- pending-before-charge persistence, payment/inventory idempotency, and explicit `payment_unknown`/`fulfillment_pending` reconciliation states;
- a periodic reconciler that looks up uncertain charges and completes retryable fulfillment without charging again;
- real Payment and Inventory HTTP adapters with W3C propagation and bounded timeouts;
- checksummed, locked, transactional Postgres migrations;
- unit tests plus Postgres/WireMock integration tests using testcontainers.

## Run

```sh
export DATABASE_URL='postgres://orders:orders@localhost:5432/orders?sslmode=disable'
export PAYMENT_SERVICE_URL='http://localhost:8081'
export INVENTORY_SERVICE_URL='http://localhost:8082'
export PAYMENT_TIMEOUT='3s' # optional
export INVENTORY_TIMEOUT='3s' # optional
export RECONCILE_INTERVAL='30s' # optional
export RECONCILE_MIN_AGE='1m' # optional
export HTTP_ADDR=':8080'    # optional
export OTEL_TRACES_EXPORTER='stdout' # stdout, otlp, or none
go run ./cmd/orderservice
```

Endpoints:

- `POST /orders`
- `GET /metrics`
- `GET /healthz`

Application logs go to stdout. The default self-contained JSON trace exporter writes spans to stderr. Set `OTEL_TRACES_EXPORTER=otlp` in production; the official OTLP/HTTP exporter reads standard variables such as `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS`. `none` disables trace sampling/export.

## Verify

```sh
golangci-lint run ./...
go test ./...
go test -race ./...
go vet ./...
go test -tags=integration ./testing/integration/...
```

The integration suite requires Docker or a compatible container runtime. It applies the same embedded migrations used at application startup and verifies their ledger/checksums and repeatability.

## Recovery contract

The assessment does not specify a payment-status API. This implementation assumes the Payment Service supports `GET /v1/charges/by-idempotency-key/{key}` with the same approved/declined JSON response as the charge endpoint. A 404 means "not yet visible," not "definitely uncharged"; the worker leaves the order unresolved and **never submits another charge**. Adapt this lookup to the real provider contract before deployment.

On startup and every `RECONCILE_INTERVAL`, the worker scans a bounded page of orders older than `RECONCILE_MIN_AGE` in `pending`, `payment_unknown`, `paid`, or `fulfillment_pending`. Approved charges are persisted before idempotent inventory reservation; declines become `failed`; missing or unavailable provider results remain retryable. Creation inserts the pending row once; both foreground requests and recovery use conditional status updates, so neither can overwrite a newer state. `orders_pending_count` and the collection-error counter remain the operational signals for unresolved work.

The requested written answers are in `TRACING.md`, `LOGGING.md`, `METRICS.md`, `TESTING.md`, and `ANSWERS.md`.
