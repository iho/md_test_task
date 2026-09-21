# Answers

## Q1 — Tracing a domain operation without passing context

```go
func (o *Order) Complete(ctx context.Context) error {
    span := trace.SpanFromContext(ctx)  // Infrastructure in domain!
    span.AddEvent("completing")
    // ...
}
```

This is wrong for two separate reasons: it imports `context`/`trace` into the domain package, and it makes `Order.Complete` fail or behave differently depending on what happens to be stashed in an opaque bag of values it didn't ask for.

**Approach 1 — domain event recording (implemented in this repo).** The domain method takes only the values it actually needs (`Complete(completedAt time.Time) error`, `internal/domain/order.go`) and records a plain `Event{Name, At, Attrs}` on itself instead of touching a span:

```go
func (o *Order) Complete(completedAt time.Time) error {
    if o.Status != OrderStatusPaid && o.Status != OrderStatusFulfillmentPending {
        return fmt.Errorf("%w: current status %q", ErrOrderNotPaid, o.Status)
    }
    o.Status = OrderStatusCompleted
    o.record(Event{Name: "order.completed", At: completedAt, Attrs: map[string]any{"order_id": o.ID}})
    return nil
}
```

The usecase — which does hold the real span, via `ctx` — drains these after calling into the domain and forwards them to the tracer (`Interactor.emitEvents`, `internal/usecase/create_order.go`):

```go
func (uc *Interactor) emitEvents(ctx context.Context, order *domain.Order) {
	span := uc.tracer.SpanFromContext(ctx)
	for _, e := range order.PullEvents() {
		span.AddEvent(e.Name, e.Attrs)
	}
}
```

**Approach 2 — a domain-owned recorder interface, passed as an explicit parameter.** Define the interface *in the domain package* (so the domain doesn't depend outward on otel), e.g. `type EventRecorder interface { Record(name string, attrs map[string]any) }`, and pass a concrete adapter as a normal method parameter: `func (o *Order) Complete(completedAt time.Time, rec EventRecorder) error`. The usecase constructs the adapter (backed by the real span) and passes it in. This still keeps the domain free of otel imports, but the domain actively pushes events during the call rather than the caller pulling them afterward.

This repo uses approach 1 because it keeps domain method signatures identical to what they'd be with zero observability — nothing in `Complete`'s signature hints that anything downstream cares about tracing, which is the stronger purity guarantee. Approach 2 is worth it when a domain operation is long/branchy enough that "just record everything and pull it all at the end" loses ordering or per-branch context that active push preserves better.

## Q2 — Mocking: right, wrong, and both

**1. When mocking is the right choice.** Testing `Interactor.Execute`'s own branching logic in isolation — e.g. "if `PaymentClient.Charge` returns `status: declined`, the order ends up `OrderStatusFailed` and is still saved." The thing under test is the *orchestration decision*, not whether Postgres or the Payment Service actually behaves that way; a fast, deterministic double that can be told "return declined" on demand is exactly right. See `TestInteractor_Execute_PaymentDeclined_IsDurablyFailed` in `internal/usecase/create_order_test.go` (using a hand-written fake rather than a mocking framework, per `TESTING.md`, but the principle — replace the dependency to isolate the usecase's own logic — is the same).

**2. When mocking hides bugs — concrete bug it would hide.** Given:

```go
func (r *OrderRepository) Save(ctx context.Context, order *domain.Order) error {
    _, err := r.db.ExecContext(ctx, `
        INSERT INTO orders (id, customer_id, items, status, created_at)
        VALUES ($1, $2, $3, $4)  -- BUG: 4 placeholders, 5 args passed below
    `, order.ID, order.CustomerID, items, string(order.Status), order.CreatedAt)
    return err
}
```

A test using `mockRepo.On("Save", mock.Anything).Return(nil)` passes without ever executing this SQL string — the mismatched placeholder count is never noticed. `TestOrderRepo_SaveAndFindByID_RoundTripsThroughRealDatabase` (`testing/integration/order_repo_test.go`), which runs this exact method against a real Postgres via testcontainers, fails immediately with a driver error, because the query is actually parsed and executed. Mocking the repository interface can never catch a bug *inside* the repository implementation — only integration tests exercising the real implementation can.

**3. When you need BOTH a mock test and an integration test.** `Interactor.Execute`'s handling of `PaymentClient.Charge` returning an ambiguous error (`TestInteractor_Execute_ChargeTransportError_RemainsReconcilable`, using `fakePayment{err: ...}`) needs a fast unit test to pin down "declined, definitively rejected, and ambiguous are different outcomes" cheaply and deterministically. It *also* needs `TestPaymentClient_ExternalAPI/timeout` (`testing/integration/payment_client_test.go`, against real WireMock) to prove that `paymentclient.Client` actually turns a slow real HTTP response into that same kind of error in the first place — the unit test assumes `Charge` returns an error under some condition; the integration test proves that condition is reachable from real network behavior. Neither test alone covers both "does the usecase react correctly" and "does the client produce the right input for it to react to."

## Q3 — "Order was charged but shows as failed"

This is a **state-divergence bug between the Payment Service (source of truth for "was money moved") and the Order Service (source of truth for the customer-visible status)** — so the workflow is built around finding where the two disagree and why.

**Logs.** Start from the order ID and find its business log (`order.created`, `order.paid`, `order.failed`, or `order.payment_unknown`). That line carries the trace ID; use it to retrieve the corresponding `CreateOrder.request`/`CreateOrder.response` pair and full trace. Error responses include the order ID and label the returned state `last_known_status`; only persisted business-event logs claim that a transition happened. Compare the Payment Service client span and provider logs using the same order ID/idempotency key: a timeout after the provider committed the charge is an ambiguous outcome, not proof of failure. Sensitive customer/payment data is not needed for this workflow and is redacted.

**Metrics.** Inspect `order_processing_duration_seconds{step="payment"}` and use an exemplar to open a representative slow trace. Correlate a payment-latency/error spike with `orders_created_total{status="failure"}` and `orders_pending_count`; a rise in pending/unknown orders after payment latency increases is the expected signature of ambiguous responses awaiting reconciliation.

**Alerts.** Three alerts specifically aimed at *this* failure mode, not just generic error-rate:
- A rate-based alert on `orders_created_total{status="failure"}` divided by total, alerting on a relative spike (e.g. failure rate doubling over a short window) rather than an absolute count, since "some failures" is normal (declines happen).
- A sustained-growth alert on `orders_pending_count`, with a short grace period so ordinary in-flight orders do not page.
- A reconciliation-based alert (not derivable from the Order Service's own metrics alone): a periodic job compares failed/pending/unknown orders against successful provider charges by idempotency key and alerts when mismatches remain after the reconciliation SLA. This directly detects cross-service disagreement that neither service's local error rate can prove.

The implementation now inserts `pending` before charging, uses the order ID as the provider idempotency key, and stores a timeout/5xx as `payment_unknown`; definitive 4xx rejection becomes `failed`. After an approved charge, `paid` is persisted before the idempotent Inventory request. Inventory or completion-persistence trouble remains recoverable as `fulfillment_pending` or `paid`. A periodic reconciler looks up the provider outcome by idempotency key and retries inventory idempotently. Both request and recovery transitions use compare-and-swap status writes; if recovery wins, the request reloads the actual persisted row rather than overwriting it. A missing lookup does not trigger another charge. The lookup endpoint is an explicit assessment assumption documented in `README.md`; a real provider contract must be confirmed before deployment.

## Q4 — Testing the outbox pattern without flaky timing

The flakiness in a naive outbox test comes from asserting on **wall-clock timing** ("sleep 500ms, then check the queue") against a background worker running on its own schedule. Fix it by removing wall-clock waits from the test entirely, at each of the four steps:

1. **Write aggregate + outbox entry** — test this step alone, synchronously: call the usecase, then assert directly against the database that both the aggregate row and the outbox row exist in the same transaction (or that a fault-injected failure between the two writes is impossible because they share one `INSERT`/transaction). No timing involved at all — this is the same shape as `TestOrderRepo_SaveAndFindByID_RoundTripsThroughRealDatabase`.
2. **Background worker reads outbox** — don't run the worker's own ticker/scheduler in the test. Instantiate the worker's "process one batch" method directly and call it once, synchronously, against a real database with a known outbox row pre-inserted. This turns "wait for the poller to eventually pick it up" into a deterministic function call.
3. **Publishes to message queue** — use a real broker in a container (e.g. a Kafka/RabbitMQ testcontainers module) or, for the message-queue *client* specifically, a stub broker; either way, after step 2's single synchronous call, assert the message is present by **actively consuming with a bounded timeout** (`context.WithTimeout` + blocking receive), not by sleeping and then checking — a consume-with-timeout returns as soon as the message arrives instead of always waiting the full sleep duration, and still fails deterministically if it never arrives.
4. **Marks entry processed** — after the synchronous "process one batch" call in step 2, assert directly against the database that the outbox row's `processed_at`/status column is set. Again no timing: the call in step 2 either completed and updated the row, or it errored, both observable immediately.

The general principle: **never test a background poller by racing its schedule.** Either drive its "do one unit of work" method directly and synchronously (steps 2 and 4), or, if the end-to-end poller loop itself must be tested, use consume-with-timeout / poll-with-backoff-and-deadline assertions (`require.Eventually` in testify, or an explicit bounded retry loop) rather than a fixed `time.Sleep`, so the test passes as fast as the system actually is and only fails after a real deadline, instead of being tuned to "however long CI happens to be slow today."

## Q5 — Time-dependent test flakiness

```go
order.CreatedAt = time.Now()
// ... later
assert.Equal(t, time.Now(), order.CreatedAt)  // Fails sometimes
```

**What's wrong:** the two `time.Now()` calls happen at genuinely different instants — any code between them (even a single allocation or scheduler preemption) takes non-zero wall-clock time, so the second `time.Now()` is later than the first by some number of nanoseconds. `assert.Equal` on `time.Time` does a full equality check (wall clock reading, monotonic reading, and location), so it fails unless the two calls happen to land in the same nanosecond — reliably true only by luck, which is worse on a loaded CI runner than on a fast local machine, matching the reported symptom exactly.

**The fix** is the `Clock` abstraction used throughout this repo (`internal/usecase/ports.go`), not a comparison tweak:

```go
type Clock interface{ Now() time.Time }
type SystemClock struct{}
func (SystemClock) Now() time.Time { return time.Now() }
```

Production code takes a `Clock` and never calls `time.Now()` itself (`Interactor.Execute` calls `uc.clock.Now()`; `domain.NewOrder` takes `createdAt time.Time` as a parameter rather than reading the clock internally — see `TRACING.md`/`ANSWERS.md` Q1 for the same "domain takes values, not clocks or contexts" principle applied to time instead of tracing). Tests inject a `fixedClock{t: time.Date(2026, 1, 1, ...)}` (`internal/usecase/create_order_test.go`) and assert against that *exact*, known value — `assert.Equal(t, clock.t, order.CreatedAt)` — which is deterministic because both sides of the comparison are the same fixed value, not two independent calls to `time.Now()`.

Where a real, non-fixed clock genuinely must be used (e.g. asserting a duration measured against the real `SystemClock`), the fix is a tolerance-based assertion instead of exact equality — `assert.WithinDuration(t, expected, actual, 50*time.Millisecond)` (testify) — rather than either exact `Equal` or re-adding a `time.Sleep` to "make it match."
