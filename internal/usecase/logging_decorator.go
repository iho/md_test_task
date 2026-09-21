package usecase

import (
	"context"

	"ordersvc/internal/domain"
)

// LoggingInteractor decorates a CreateOrderUsecase with operational
// logging at the service boundary: request in, response/duration/error
// out. It knows nothing about order semantics - that "business logging"
// (order.created, order.paid, order.failed) happens inside
// Interactor.Execute via domain events, so it is not duplicated here.
// See LOGGING.md for the operational-vs-business distinction.
type LoggingInteractor struct {
	inner CreateOrderUsecase
	log   Logger
	clock Clock
}

func NewLoggingInteractor(inner CreateOrderUsecase, log Logger, clock Clock) *LoggingInteractor {
	return &LoggingInteractor{inner: inner, log: log, clock: clock}
}

func (l *LoggingInteractor) Execute(ctx context.Context, req *CreateOrderRequest) (*domain.Order, error) {
	start := l.clock.Now()
	itemCount := 0
	tier := "unknown"
	if req != nil {
		itemCount = len(req.Items)
		if normalized, ok := normalizedCustomerTier(req.CustomerTier); ok {
			tier = string(normalized)
		}
	}
	l.log.Info(ctx, "CreateOrder.request",
		"item_count", itemCount,
		"customer_tier", tier,
	)

	order, err := l.inner.Execute(ctx, req)
	durationMS := l.clock.Now().Sub(start).Milliseconds()

	if err != nil {
		fields := []any{"duration_ms", durationMS, "outcome", "failure"}
		if order != nil {
			fields = append(fields, "order_id", order.ID, "last_known_status", string(order.Status))
		}
		l.log.Error(ctx, "CreateOrder.response", err, fields...)
		return order, err
	}
	outcome := "failure"
	if order.Status == domain.OrderStatusCompleted {
		outcome = "success"
	}
	l.log.Info(ctx, "CreateOrder.response",
		"duration_ms", durationMS,
		"outcome", outcome,
		"order_id", order.ID,
		"status", string(order.Status),
	)
	return order, nil
}
