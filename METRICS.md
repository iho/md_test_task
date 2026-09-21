# Metrics Design

## Instrumentation boundary

Instrumentation is placed where the relevant semantic information exists:

- `MetricsInteractor` increments the created-order counter only when a durable order exists. Validation and initial persistence failures return no order and are not mislabeled as “created.” Only `completed` is success; failed or recoverable intermediate states are failures for this request outcome.
- `Interactor` records the three actual pipeline durations around validation, the Payment Service call, and the Inventory Service reservation (`fulfillment`). A boundary-only decorator cannot measure these internal steps separately.
- Repository query latency is represented by repository child spans; low-level pool/driver metrics belong in the SQL adapter/runtime, not in business metrics.
- `orders_pending_count` is a scrape-time `GaugeFunc`. Prometheus collection runs one authoritative database count over `pending`, `payment_unknown`, `paid`, and `fulfillment_pending`; create-order requests never execute that aggregate query. Query failures yield `NaN` and increment `orders_pending_collection_errors_total` rather than publishing a stale value.

This keeps business decisions in the usecase while all Prometheus-specific code remains in `observability` behind `MetricsRecorder`.

## Metrics

```text
orders_created_total{status="success|failure",customer_tier="free|premium|unknown"}
order_processing_duration_seconds{step="validation|payment|fulfillment|unknown"}
orders_pending_count
orders_pending_collection_errors_total
```

`unknown` is an intentional bounded fallback. The usecase rejects unsupported customer tiers, and the Prometheus adapter independently normalizes every label again as defense in depth.

## Why identifier labels explode

```go
orderDuration.WithLabelValues(customerID, productID, orderID).Observe(duration)
```

Prometheus creates one time series per unique label combination. These identifiers are effectively unbounded and often unique per request, so series count, memory, storage, query cost, and managed-service cost all grow with the number of orders.

Metrics retain only small, predefined dimensions. Per-order/customer detail belongs in logs and traces. This repository enforces the rule twice:

1. `MetricsRecorder` has no identifier parameters.
2. `PrometheusMetrics` maps every unexpected status, tier, or step to a fixed fallback rather than creating a new series.

`observability/metrics_test.go` proves that attacker-controlled label input becomes `failure/unknown`.

## Correlating metrics and traces

Histogram observations use Prometheus exemplars:

```go
observer.ObserveWithExemplar(
    duration.Seconds(),
    prometheus.Labels{"trace_id": TraceIDFromContext(ctx)},
)
```

The trace ID is attached to an observation, not a metric label, so it does not create a new series. Grafana-compatible backends can link a slow bucket directly to a representative trace. Because tracing is the outermost usecase decorator, all three step observations receive the active trace context.
