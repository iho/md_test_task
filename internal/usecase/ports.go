// Package usecase contains the CreateOrder interactor and the ports
// (interfaces) it depends on. The interactor only ever depends on these
// interfaces, never on concrete otel/slog/prometheus/sql types - those live
// in observability/ and internal/repository, internal/paymentclient.
package usecase

import (
	"context"
	"errors"
	"time"

	"ordersvc/internal/domain"
)

// Clock abstracts "now" so usecases (and, through the values they pass in,
// the domain) never call time.Now() directly. See ANSWERS.md Q5.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real, wall-clock implementation used outside tests.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// OrderRepository is the persistence port. It is implemented by an
// observability-free Postgres adapter and is tested for real with
// testcontainers rather than mocked - see TESTING.md.
type OrderRepository interface {
	Save(ctx context.Context, order *domain.Order) error
	SaveIfStatus(ctx context.Context, order *domain.Order, expected domain.OrderStatus) (bool, error)
	FindByID(ctx context.Context, id string) (*domain.Order, error)
}

// ReconciliationRepository adds bounded scanning and compare-and-swap writes.
// The latter prevents a recovery worker from overwriting a newer order state.
type ReconciliationRepository interface {
	OrderRepository
	ListRecoverableIDs(ctx context.Context, afterID string, olderThan time.Time, limit int) ([]string, error)
}

// PaymentRequest/PaymentResult are the usecase-level shapes exchanged with
// the Payment Service, independent of that client's wire format.
type PaymentRequest struct {
	OrderID        string
	CustomerID     string
	AmountCents    int64
	IdempotencyKey string
}

type PaymentResult struct {
	Reference string
	Status    string // "approved" | "declined"
}

// PaymentClient is the outbound port to the Payment Service.
type PaymentClient interface {
	Charge(ctx context.Context, req PaymentRequest) (PaymentResult, error)
}

// ChargeLookup is deliberately read-only: an absent provider record is not
// proof that a previously attempted charge can safely be retried.
type ChargeLookup interface {
	LookupCharge(ctx context.Context, idempotencyKey string) (PaymentResult, bool, error)
}

// PaymentFailure is implemented by adapter errors that can distinguish a
// definitive rejection from an ambiguous outcome. Unclassified errors default
// to ambiguous because falsely declaring an unknown charge failed is unsafe.
type PaymentFailure interface {
	error
	OutcomeMayHaveCommitted() bool
}

func PaymentOutcomeMayHaveCommitted(err error) bool {
	var failure PaymentFailure
	if errors.As(err, &failure) {
		return failure.OutcomeMayHaveCommitted()
	}
	return true
}

type InventoryRequest struct {
	OrderID        string
	Items          []domain.OrderItem
	IdempotencyKey string
}

// InventoryClient is the outbound port used by the fulfillment step.
type InventoryClient interface {
	Reserve(ctx context.Context, req InventoryRequest) error
}

// Span is the minimal tracing surface a usecase needs. It is satisfied by
// an adapter around go.opentelemetry.io/otel/trace.Span (see
// observability/tracing.go), but this package never imports otel directly -
// that keeps the usecase layer swappable and trivially fakeable in unit
// tests (see internal/usecase/create_order_test.go).
type Span interface {
	AddEvent(name string, attrs map[string]any)
	RecordError(err error)
	End()
}

// Tracer starts spans from a context. Trace context lives in
// context.Context (Option A) - see TRACING.md for why usecases use this
// option and how it differs from the domain and repository layers.
type Tracer interface {
	Start(ctx context.Context, spanName string) (context.Context, Span)
	SpanFromContext(ctx context.Context) Span
}

// Logger is a structured, leveled logger. Implementations attach the
// current trace ID from ctx automatically - see LOGGING.md.
type Logger interface {
	Info(ctx context.Context, msg string, kv ...any)
	Error(ctx context.Context, msg string, err error, kv ...any)
}

// MetricsRecorder records the bounded-cardinality metrics described in
// METRICS.md. Note there is no order_id/customer_id/product_id parameter
// anywhere on this interface - see METRICS.md Q2 for why that would cause
// cardinality explosion, and how per-observation correlation is instead
// done with exemplars in ObserveStepDuration (METRICS.md Q3).
type MetricsRecorder interface {
	IncOrdersCreated(status string, customerTier string)
	ObserveStepDuration(ctx context.Context, step string, d time.Duration)
}
