package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"ordersvc/internal/domain"
)

type CustomerTier string

const (
	CustomerTierFree    CustomerTier = "free"
	CustomerTierPremium CustomerTier = "premium"
)

var (
	ErrNilRequest          = errors.New("create order: request is required")
	ErrInvalidCustomerTier = errors.New("create order: invalid customer tier")
	// ErrPaymentUncertain means the service cannot prove that its durable order
	// state agrees with the provider: either the provider response was ambiguous
	// or persisting a known approval failed. Callers must not present this as a
	// decline; reconciliation resolves it using the order ID/idempotency key.
	ErrPaymentUncertain = errors.New("create order: payment outcome is uncertain")
	// ErrPaymentRejected means the provider definitively rejected the request
	// before it could commit a charge (for example an authentication failure).
	ErrPaymentRejected = errors.New("create order: payment request was rejected")
	// ErrFulfillmentPending means payment is durable but inventory reservation
	// or final persistence must be retried with the order's idempotency key.
	ErrFulfillmentPending = errors.New("create order: fulfillment is pending")
	// ErrConcurrentStateChange means another request or recovery worker changed
	// the durable row before this request could persist its next state.
	ErrConcurrentStateChange = errors.New("create order: order state changed concurrently")
)

type CreateOrderRequest struct {
	CustomerID   string
	CustomerTier CustomerTier
	Items        []domain.OrderItem
}

// CreateOrderUsecase is implemented by the core interactor and its tracing,
// logging, and metrics decorators.
type CreateOrderUsecase interface {
	Execute(ctx context.Context, req *CreateOrderRequest) (*domain.Order, error)
}

type Interactor struct {
	repo      OrderRepository
	payment   PaymentClient
	inventory InventoryClient
	clock     Clock
	tracer    Tracer
	log       Logger
	metrics   MetricsRecorder
}

func NewInteractor(
	repo OrderRepository,
	payment PaymentClient,
	inventory InventoryClient,
	clock Clock,
	tracer Tracer,
	log Logger,
	metrics MetricsRecorder,
) *Interactor {
	return &Interactor{
		repo: repo, payment: payment, inventory: inventory, clock: clock, tracer: tracer, log: log, metrics: metrics,
	}
}

func (uc *Interactor) Execute(ctx context.Context, req *CreateOrderRequest) (*domain.Order, error) {
	validationStarted := uc.clock.Now()
	if req == nil {
		uc.metrics.ObserveStepDuration(ctx, "validation", uc.clock.Now().Sub(validationStarted))
		return nil, ErrNilRequest
	}
	if _, ok := normalizedCustomerTier(req.CustomerTier); !ok {
		uc.metrics.ObserveStepDuration(ctx, "validation", uc.clock.Now().Sub(validationStarted))
		// The tier came from an untrusted request. Never echo its raw value into
		// an error: errors are also recorded in logs and trace exception events.
		return nil, ErrInvalidCustomerTier
	}

	orderID, err := newOrderID()
	if err != nil {
		uc.metrics.ObserveStepDuration(ctx, "validation", uc.clock.Now().Sub(validationStarted))
		return nil, fmt.Errorf("create order ID: %w", err)
	}
	order, err := domain.NewOrder(orderID, req.CustomerID, req.Items, uc.clock.Now())
	uc.metrics.ObserveStepDuration(ctx, "validation", uc.clock.Now().Sub(validationStarted))
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	// Establish a durable pending record before causing the external payment
	// side effect. If the later status update fails, reconciliation still has
	// an order and the idempotency key needed to recover it.
	if err := uc.repo.Save(ctx, order); err != nil {
		return nil, fmt.Errorf("save pending order: %w", err)
	}
	uc.emitEvents(ctx, order)

	paymentStarted := uc.clock.Now()
	result, chargeErr := uc.payment.Charge(ctx, PaymentRequest{
		OrderID:        order.ID,
		CustomerID:     order.CustomerID,
		AmountCents:    order.Total(),
		IdempotencyKey: order.ID,
	})
	uc.metrics.ObserveStepDuration(ctx, "payment", uc.clock.Now().Sub(paymentStarted))

	switch {
	case chargeErr != nil:
		var outcomeErr error
		outcomeOrder := cloneOrder(order)
		if PaymentOutcomeMayHaveCommitted(chargeErr) {
			if err := outcomeOrder.MarkPaymentUnknown(uc.clock.Now(), "ambiguous_provider_error"); err != nil {
				return nil, fmt.Errorf("mark payment outcome unknown: %w", err)
			}
			outcomeErr = fmt.Errorf("%w: charge: %w", ErrPaymentUncertain, chargeErr)
		} else {
			if err := outcomeOrder.MarkFailed(uc.clock.Now(), "payment_request_rejected"); err != nil {
				return nil, fmt.Errorf("mark payment request rejected: %w", err)
			}
			outcomeErr = fmt.Errorf("%w: charge: %w", ErrPaymentRejected, chargeErr)
		}
		saved, err := uc.saveTransition(ctx, order, outcomeOrder)
		if err != nil {
			if errors.Is(err, ErrConcurrentStateChange) {
				return saved, fmt.Errorf("%w: %w", ErrPaymentUncertain, err)
			}
			return saved, fmt.Errorf("%w; save payment outcome: %w", outcomeErr, err)
		}
		return saved, outcomeErr
	case result.Status == "declined":
		failedOrder := cloneOrder(order)
		if err := failedOrder.MarkFailed(uc.clock.Now(), "payment_declined"); err != nil {
			return nil, fmt.Errorf("mark order failed: %w", err)
		}
		saved, err := uc.saveTransition(ctx, order, failedOrder)
		if err != nil {
			if errors.Is(err, ErrConcurrentStateChange) {
				return saved, fmt.Errorf("%w: %w", ErrPaymentUncertain, err)
			}
			return saved, fmt.Errorf("save declined order: %w", err)
		}
		return saved, nil
	case result.Status == "approved":
		paidOrder := cloneOrder(order)
		if err := paidOrder.MarkPaid(uc.clock.Now(), result.Reference); err != nil {
			return nil, fmt.Errorf("mark order paid: %w", err)
		}
		// Persist the charge result before starting fulfillment. If any later
		// operation fails, reconciliation can safely retry inventory by order ID.
		saved, err := uc.saveTransition(ctx, order, paidOrder)
		if err != nil {
			return saved, fmt.Errorf("%w: save paid order: %w", ErrPaymentUncertain, err)
		}
		order = saved
	default:
		// A conforming PaymentClient rejects unknown statuses. Keep this guard
		// at the port boundary so another adapter cannot silently turn protocol
		// drift into a decline.
		unknownOrder := cloneOrder(order)
		if err := unknownOrder.MarkPaymentUnknown(uc.clock.Now(), "invalid_provider_status"); err != nil {
			return nil, fmt.Errorf("mark payment outcome unknown: %w", err)
		}
		saved, err := uc.saveTransition(ctx, order, unknownOrder)
		if err != nil {
			return saved, fmt.Errorf("%w: invalid payment status; save unknown outcome: %w", ErrPaymentUncertain, err)
		}
		return saved, fmt.Errorf("%w: invalid payment status", ErrPaymentUncertain)
	}

	fulfillmentStarted := uc.clock.Now()
	fulfillmentErr := uc.inventory.Reserve(ctx, InventoryRequest{
		OrderID:        order.ID,
		Items:          append([]domain.OrderItem(nil), order.Items...),
		IdempotencyKey: order.ID,
	})
	uc.metrics.ObserveStepDuration(ctx, "fulfillment", uc.clock.Now().Sub(fulfillmentStarted))
	if fulfillmentErr != nil {
		pendingOrder := cloneOrder(order)
		if err := pendingOrder.MarkFulfillmentPending(uc.clock.Now(), "inventory_reservation_error"); err != nil {
			return order, fmt.Errorf("mark fulfillment pending: %w", err)
		}
		saved, err := uc.saveTransition(ctx, order, pendingOrder)
		if err != nil {
			return saved, fmt.Errorf("%w: reserve inventory: %v; save fulfillment state: %w", ErrFulfillmentPending, fulfillmentErr, err)
		}
		return saved, fmt.Errorf("%w: reserve inventory: %w", ErrFulfillmentPending, fulfillmentErr)
	}

	completedOrder := cloneOrder(order)
	if err := completedOrder.Complete(uc.clock.Now()); err != nil {
		return order, fmt.Errorf("complete fulfilled order: %w", err)
	}
	saved, err := uc.saveTransition(ctx, order, completedOrder)
	if err != nil {
		return saved, fmt.Errorf("%w: save completed order: %w", ErrFulfillmentPending, err)
	}
	return saved, nil
}

// saveTransition never overwrites a state advanced by another request or the
// recovery worker. On conflict, return the actual durable row to the caller.
func (uc *Interactor) saveTransition(ctx context.Context, previous, next *domain.Order) (*domain.Order, error) {
	updated, err := uc.repo.SaveIfStatus(ctx, next, previous.Status)
	if err != nil {
		return previous, err
	}
	if !updated {
		current, err := uc.repo.FindByID(ctx, previous.ID)
		if err != nil {
			return previous, fmt.Errorf("%w: reload order: %w", ErrConcurrentStateChange, err)
		}
		return current, ErrConcurrentStateChange
	}
	uc.emitEvents(ctx, next)
	return next, nil
}

func normalizedCustomerTier(tier CustomerTier) (CustomerTier, bool) {
	switch tier {
	case "", CustomerTierFree:
		return CustomerTierFree, true
	case CustomerTierPremium:
		return CustomerTierPremium, true
	default:
		return "", false
	}
}

// emitEvents is the boundary where pure domain events become both span events
// and sparse business logs. Request/response mechanics remain in the logging
// decorator.
func (uc *Interactor) emitEvents(ctx context.Context, order *domain.Order) {
	emitDomainEvents(ctx, uc.tracer, uc.log, order)
}

func emitDomainEvents(ctx context.Context, tracer Tracer, log Logger, order *domain.Order) {
	span := tracer.SpanFromContext(ctx)
	for _, event := range order.PullEvents() {
		span.AddEvent(event.Name, event.Attrs)

		keys := make([]string, 0, len(event.Attrs))
		for key := range event.Attrs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fields := make([]any, 0, 2*len(keys)+2)
		fields = append(fields, "occurred_at", event.At)
		for _, key := range keys {
			fields = append(fields, key, event.Attrs[key])
		}
		log.Info(ctx, event.Name, fields...)
	}
}

func cloneOrder(order *domain.Order) *domain.Order {
	copyOrder := *order
	copyOrder.Items = append([]domain.OrderItem(nil), order.Items...)
	return &copyOrder
}

func newOrderID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ord_" + hex.EncodeToString(buf), nil
}
