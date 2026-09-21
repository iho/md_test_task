package usecase

import (
	"context"

	"ordersvc/internal/domain"
)

// TracingInteractor is the outermost usecase decorator. It creates the span
// before logging and metrics run, so every inner component receives the
// derived context and can attach logs, exemplars, and child spans to one trace.
type TracingInteractor struct {
	inner  CreateOrderUsecase
	tracer Tracer
}

func NewTracingInteractor(inner CreateOrderUsecase, tracer Tracer) *TracingInteractor {
	return &TracingInteractor{inner: inner, tracer: tracer}
}

func (t *TracingInteractor) Execute(ctx context.Context, req *CreateOrderRequest) (*domain.Order, error) {
	ctx, span := t.tracer.Start(ctx, "CreateOrder.Execute")
	defer span.End()

	order, err := t.inner.Execute(ctx, req)
	if err != nil {
		span.RecordError(err)
	}
	return order, err
}

// TracingOrderRepository adds child spans while keeping tracing out of the
// concrete SQL adapter. The same context still reaches database/sql for
// cancellation and deadlines.
type TracingOrderRepository struct {
	inner  OrderRepository
	tracer Tracer
}

func NewTracingOrderRepository(inner OrderRepository, tracer Tracer) *TracingOrderRepository {
	return &TracingOrderRepository{inner: inner, tracer: tracer}
}

func (r *TracingOrderRepository) Save(ctx context.Context, order *domain.Order) error {
	ctx, span := r.tracer.Start(ctx, "orders.repository.save")
	defer span.End()
	err := r.inner.Save(ctx, order)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

func (r *TracingOrderRepository) SaveIfStatus(ctx context.Context, order *domain.Order, expected domain.OrderStatus) (bool, error) {
	ctx, span := r.tracer.Start(ctx, "orders.repository.save_if_status")
	defer span.End()
	updated, err := r.inner.SaveIfStatus(ctx, order, expected)
	if err != nil {
		span.RecordError(err)
	}
	return updated, err
}

func (r *TracingOrderRepository) FindByID(ctx context.Context, id string) (*domain.Order, error) {
	ctx, span := r.tracer.Start(ctx, "orders.repository.find_by_id")
	defer span.End()
	order, err := r.inner.FindByID(ctx, id)
	if err != nil {
		span.RecordError(err)
	}
	return order, err
}
