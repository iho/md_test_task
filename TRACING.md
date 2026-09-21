# Tracing Design

## Where trace context lives

| Layer | Choice | Reason |
|---|---|---|
| Usecase | Option A: `context.Context` | Cancellation, deadlines, and W3C trace context follow the same call chain without a second plumbing parameter. `TracingInteractor` creates the usecase span before logging and metrics execute. |
| Domain | No trace context | Domain methods accept domain values only and record plain domain events. They import neither `context` nor OpenTelemetry. |
| Repository | Option A: `context.Context` | The context reaches `database/sql` calls. `TracingOrderRepository` adds child spans around repository operations without coupling the SQL adapter to OpenTelemetry. |

Option B duplicates information already carried by `context.Context`. Option C is useful for creating the usecase span, but it complements rather than replaces context propagation: the decorator must pass its derived context inward.

## End-to-end propagation

The runnable composition in `cmd/orderservice/main.go` installs:

1. An SDK tracer provider with a real exporter and a `TraceContext`/`Baggage` propagator.
2. `otelhttp.NewHandler` on the inbound server, which extracts an incoming `traceparent` and creates a server span.
3. `TracingInteractor` as the outermost usecase decorator. Logging, metrics exemplars, the core interactor, and repository calls therefore receive the derived context.
4. `otelhttp.NewTransport` in both `paymentclient.Client` and `inventoryclient.Client`, which creates client spans and injects `traceparent` into outbound requests.
5. `TracingOrderRepository`, which creates child spans around request-path `Save`, conditional `SaveIfStatus`, and `FindByID` operations while forwarding the context to `ExecContext`/`QueryRowContext`. The pending gauge queries its raw read adapter only at Prometheus scrape time, outside a customer request trace.

The JSON span exporter makes the assessment runnable without a collector. `OTEL_TRACES_EXPORTER=otlp` selects the official OTLP/HTTP exporter in production, configured through standard `OTEL_EXPORTER_OTLP_*` variables, without changing domain or usecase code.

## Tracing pure domain operations

The implemented approach is domain-event recording:

```go
func (o *Order) Complete(completedAt time.Time) error {
    if o.Status != OrderStatusPaid && o.Status != OrderStatusFulfillmentPending {
        return fmt.Errorf("%w: current status %q", ErrOrderNotPaid, o.Status)
    }
    o.Status = OrderStatusCompleted
    o.record(Event{
        Name: "order.completed",
        At: completedAt,
        Attrs: map[string]any{"order_id": o.ID},
    })
    return nil
}
```

The usecase drains those values after the state has been persisted and translates each event into a span event and a business log. The domain never sees a span, tracer, logger, or context.

A second valid approach is a domain-owned recorder interface such as `EventRecorder`. The caller passes an adapter that records neutral domain events. This preserves dependency direction but adds an observability-shaped parameter to every domain call, so the pull-based event approach is preferable here.

## Error semantics

`otelSpan.RecordError` both records the exception event and sets the span status to `Error`; `RecordError` alone would not set OpenTelemetry status. Payment timeouts/5xx/protocol failures are recorded as errors and move the order to `payment_unknown`, because they cannot prove that a charge did not commit. Definitive provider 4xx responses move it to `failed`. Inventory failures leave a durable paid order in `fulfillment_pending` (or `paid` if that state write itself fails), safe to retry with the order ID as idempotency key.
