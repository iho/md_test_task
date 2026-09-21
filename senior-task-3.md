# Backend Developer Assessment - Senior Level (Task 3)

## Instructions

1. Complete all tasks below
2. Push your solution to a **public GitHub repository**

---

## Task: Observability and Integration Testing Strategy

Design a comprehensive observability system and testing strategy for an order processing service.

### Context

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   API GW    │────>│   Order     │────>│  Inventory  │
│             │     │   Service   │     │   Service   │
└─────────────┘     └──────┬──────┘     └─────────────┘
                           │
                    ┌──────▼──────┐
                    │   Payment   │
                    │   Service   │
                    └─────────────┘
```

Current problems:
- No trace ID propagation
- Inconsistent logging
- Metrics only at infrastructure level
- Tests over-rely on mocks, missing real bugs

---

## Part 1: Tracing Design (TRACING.md)

Design trace context propagation through all layers.

### Question: Where should trace context live?

```go
// Option A: Context values
func (uc *Interactor) Execute(ctx context.Context, req *Request) (*Plan, error) {
    span := trace.SpanFromContext(ctx)
    // ...
}

// Option B: Explicit parameter
func (uc *Interactor) Execute(ctx context.Context, trace TraceContext, req *Request) error {
    // ...
}

// Option C: Middleware wrapper
type TracedInteractor struct {
    inner Interactor
    tracer Tracer
}
```

Answer these:
1. Which option for usecases? Why?
2. Which option for domain methods? Why? (Remember: domain must be pure)
3. Which option for repository methods?
4. How do you trace a domain method without passing context to it?

Provide implementation for option you chose.

---

## Part 2: Logging Strategy (LOGGING.md)

### Question: Where should logging happen?

```go
// Option A: Log in usecase
func (uc *Interactor) Execute(ctx context.Context, req *Request) (*Plan, error) {
    uc.log.Info("creating order", "customer_id", req.CustomerID)
    // ...
}

// Option B: Log in service layer only
func (s *Service) CreateOrder(ctx context.Context, req *Request) error {
    s.log.Info("CreateOrder", "request", req)
    result, err := s.usecase.Execute(ctx, req)
    s.log.Info("CreateOrder completed", "error", err)
    return err
}

// Option C: Decorator pattern
type LoggingInteractor struct {
    inner Interactor
    log   Logger
}
```

Answer:
1. Which option maintains Clean Architecture?
2. What's the difference between "business logs" and "operational logs"?
3. How do you avoid logging sensitive data?

Provide structured logging implementation with:
- Trace ID in every log
- Request/response logging at boundaries
- Error logging with stack traces
- PII redaction

---

## Part 3: Metrics Design (METRICS.md)

Design metrics for order processing:

```go
// Counters
orders_created_total{status="success|failure", customer_tier="free|premium"}

// Histograms
order_processing_duration_seconds{step="validation|payment|fulfillment"}

// Gauges
orders_pending_count{}
```

Answer:
1. Where do you instrument? (usecase, service, or repo?)
2. This causes metric explosion - why and how to fix?
```go
orderDuration.WithLabelValues(customerID, productID, orderID).Observe(duration)
```
3. How do you correlate metrics with traces?

---

## Part 4: Integration Testing (TESTING.md)

### The Over-Mocking Problem

```go
// This test passes but misses real bugs
func TestCreateOrder_WithMocks(t *testing.T) {
    mockRepo := &MockOrderRepo{}
    mockRepo.On("CreateMut", mock.Anything).Return(&Mutation{}, nil)

    uc := NewInteractor(mockRepo)
    _, err := uc.Execute(ctx, &Request{...})

    assert.NoError(t, err)
}
```

Problems:
1. Mock doesn't validate SQL
2. Serialization not tested
3. Database constraints not checked

### Your Task

Design testing strategy with clear boundaries:

| Test Level | What to Test | What to Mock |
|------------|--------------|--------------|
| Unit | ? | ? |
| Integration | ? | ? |
| E2E | ? | ? |

Implement:

**1. Repository Integration Test (with testcontainers)**
```go
func TestOrderRepo_Integration(t *testing.T) {
    // Setup real database in container
    // Test that CreateMut generates valid SQL
    // Test that UpdateMut only updates dirty fields
    // Verify data in database matches expected
}
```

**2. External Service Test (with WireMock)**
```go
func TestPaymentClient_ExternalAPI(t *testing.T) {
    // Setup WireMock
    // Stub payment API responses
    // Test success, failure, timeout scenarios
}
```

---

## Questions - Answer in ANSWERS.md

**Q1:** The domain layer must be pure (no infrastructure). But you want to trace domain operations. This code violates purity:

```go
func (o *Order) Complete(ctx context.Context) error {
    span := trace.SpanFromContext(ctx)  // Infrastructure in domain!
    span.AddEvent("completing")
    // ...
}
```

How do you trace domain operations without passing context? Describe two approaches.

**Q2:** Give specific examples:
1. When mocking is the RIGHT choice
2. When mocking HIDES bugs (give the bug it would hide)
3. When you need BOTH mock test AND integration test

**Q3:** Customer reports: "Order was charged but shows as failed."

Design debugging workflow:
- What logs would help?
- What metrics would indicate this?
- How would you set up alerts?

**Q4:** The outbox pattern:
1. Write aggregate + outbox entry
2. Background worker reads outbox
3. Publishes to message queue
4. Marks entry processed

How do you test this end-to-end WITHOUT flaky timing issues?

**Q5:** Your test passes locally but fails in CI. The test does:
```go
order.CreatedAt = time.Now()
// ... later
assert.Equal(t, time.Now(), order.CreatedAt)  // Fails sometimes
```

What's wrong? How do you fix time-dependent tests?

---

## Repository Structure

```
your-repo/
├── observability/
│   ├── tracing.go
│   ├── logging.go
│   └── metrics.go
├── testing/
│   ├── integration/
│   │   ├── order_repo_test.go
│   │   └── payment_client_test.go
│   └── setup/
│       └── testcontainers.go
├── TRACING.md
├── LOGGING.md
├── METRICS.md
├── TESTING.md
└── ANSWERS.md
```

---

## Evaluation

Your submission will be evaluated against our engineering standards document. Key areas:
- Trace context propagation that maintains domain purity
- Structured logging with proper boundaries
- Metrics without cardinality explosion
- Clear test pyramid with defined boundaries
- Integration tests using testcontainers
- Mock vs real service test decisions
- Time abstraction for deterministic tests
