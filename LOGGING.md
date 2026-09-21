# Logging Strategy

## Where logging happens

Clean Architecture is maintained with two deliberately different placements:

- **Operational logs use Option C**, the `LoggingInteractor` decorator. It emits one request line and one response line with duration, outcome, order ID, durable status on success, last-known persisted status on error, and an error stack.
- **Business logs are emitted at the usecase boundary.** The pure domain records neutral events such as `order.created`, `order.paid`, `order.payment_unknown`, `order.fulfillment_pending`, and `order.completed`; after persistence succeeds, `Interactor.emitEvents` writes each event to the active span and the `Logger` port.

The usecase depends only on interfaces it owns. Concrete `slog`, OpenTelemetry, Prometheus, HTTP, and SQL implementations remain in outer packages.

Logging only in an HTTP service layer is insufficient because that layer knows the transport result but not which persisted domain transition occurred. Conversely, entry/exit boilerplate does not belong inside every usecase, which is why operational logging is a decorator.

## Business versus operational logs

| | Business | Operational |
|---|---|---|
| Meaning | Persisted state transition: `order.paid`, `order.failed`, `order.fulfillment_pending`, `order.completed` | Boundary mechanics: request, response, duration, top-level error |
| Audience | Support/product and reconciliation workflows | On-call/SRE |
| Placement | Usecase translates persisted domain events | Reusable decorator |
| Fields | Order ID, status-neutral event attributes | Outcome, duration, order ID/status when available |

Events are logged only after the corresponding save succeeds, so logs do not claim that an uncommitted state transition happened.

## Trace correlation

`TracingInteractor` is the outermost decorator:

```text
TracingInteractor → LoggingInteractor → MetricsInteractor → Interactor
```

Every log therefore receives the active usecase context. `observability.Logger` always emits `trace_id` and `span_id` fields, including empty strings for process-level logs that are genuinely outside a trace. It uses `InfoContext`/`ErrorContext`, so context-aware handlers also receive the context.

## Sensitive-data policy

Boundary code logs selected metadata, never complete request/response structs. For example, the request log includes item count and normalized tier but deliberately omits customer ID and item contents.

The logger then applies defense-in-depth redaction:

- normalized key-based redaction for card data, authentication data, customer identifiers, email, SSN, payment references, and idempotency keys;
- recursive redaction inside `map[string]any`/`[]any` values;
- email, payment-card, and bearer-token pattern removal from free-form strings and error messages.

This is covered by `observability/logging_test.go`. Production policy should still be backed by schema classification and a lint rule that prohibits logging raw request/domain structs.

Validation and provider-status errors also avoid embedding raw untrusted values before they reach logging or tracing. For example, an invalid `customer_tier` produces a stable sentinel error, not the submitted tier string.

## Error stacks

`Logger.Error` sanitizes the error text and attaches `debug.Stack()`. The stack represents the logging point; errors that need the original creation stack should use an error type that captures one when created.
