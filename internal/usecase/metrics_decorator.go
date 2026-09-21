package usecase

import (
	"context"

	"ordersvc/internal/domain"
)

// MetricsInteractor decorates a CreateOrderUsecase with the counters and
// histograms defined in METRICS.md. Note it only ever passes bounded label
// values (status, customer_tier, step) to MetricsRecorder - never
// order.ID or req.CustomerID, which would blow up cardinality (see
// METRICS.md Q2).
type MetricsInteractor struct {
	inner   CreateOrderUsecase
	metrics MetricsRecorder
}

func NewMetricsInteractor(inner CreateOrderUsecase, metrics MetricsRecorder) *MetricsInteractor {
	return &MetricsInteractor{inner: inner, metrics: metrics}
}

func (m *MetricsInteractor) Execute(ctx context.Context, req *CreateOrderRequest) (*domain.Order, error) {
	order, err := m.inner.Execute(ctx, req)
	if order == nil {
		return nil, err
	}

	status := "failure"
	if err == nil && order.Status == domain.OrderStatusCompleted {
		status = "success"
	}
	tier := "unknown"
	if req != nil {
		if normalized, ok := normalizedCustomerTier(req.CustomerTier); ok {
			tier = string(normalized)
		}
	}
	m.metrics.IncOrdersCreated(status, tier)
	return order, err
}
