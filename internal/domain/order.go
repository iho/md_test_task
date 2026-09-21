// Package domain contains the Order aggregate. Nothing in this package
// imports context, tracing, logging or metrics: every fact it needs
// (including "now") is passed in by the caller, which keeps it
// deterministic and unit-testable without any infrastructure test doubles.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// OrderStatus is the lifecycle state of an Order aggregate.
type OrderStatus string

const (
	OrderStatusPending            OrderStatus = "pending"
	OrderStatusPaid               OrderStatus = "paid"
	OrderStatusFailed             OrderStatus = "failed"
	OrderStatusPaymentUnknown     OrderStatus = "payment_unknown"
	OrderStatusFulfillmentPending OrderStatus = "fulfillment_pending"
	OrderStatusCompleted          OrderStatus = "completed"
)

var (
	ErrEmptyOrder       = errors.New("order: must contain at least one item")
	ErrEmptyCustomer    = errors.New("order: customer ID is required")
	ErrInvalidOrderItem = errors.New("order: item must have a product ID, positive quantity, and non-negative price")
	ErrOrderNotPending  = errors.New("order: can only transition from pending or payment_unknown state")
	ErrOrderNotPaid     = errors.New("order: requires a paid fulfillment state")
)

// OrderItem is a line item on an order. Pure value object.
type OrderItem struct {
	ProductID string
	Quantity  int
	UnitPrice int64 // cents, avoids floating point rounding issues
}

// Event is a fact that happened inside the aggregate. The domain records
// events instead of emitting spans, logs or metrics directly, so it stays
// free of infrastructure concerns. See TRACING.md ("domain event
// recording") for how the usecase layer turns these into span events.
type Event struct {
	Name  string
	At    time.Time
	Attrs map[string]any
}

// Order is the order aggregate root.
type Order struct {
	ID         string
	CustomerID string
	Items      []OrderItem
	Status     OrderStatus
	CreatedAt  time.Time
	// PaymentReference is the provider's non-card payment identifier. It is
	// persisted for reconciliation but intentionally omitted from telemetry.
	PaymentReference string

	events []Event
}

// NewOrder creates a pending order. createdAt is supplied by the caller
// (the usecase layer, via an injected Clock) instead of the domain calling
// time.Now() itself - see ANSWERS.md Q5 for why that matters for
// deterministic tests.
func NewOrder(id, customerID string, items []OrderItem, createdAt time.Time) (*Order, error) {
	if customerID == "" {
		return nil, ErrEmptyCustomer
	}
	if len(items) == 0 {
		return nil, ErrEmptyOrder
	}
	for _, item := range items {
		if item.ProductID == "" || item.Quantity <= 0 || item.UnitPrice < 0 {
			return nil, ErrInvalidOrderItem
		}
	}
	itemsCopy := append([]OrderItem(nil), items...)
	o := &Order{
		ID:         id,
		CustomerID: customerID,
		Items:      itemsCopy,
		Status:     OrderStatusPending,
		CreatedAt:  createdAt,
	}
	o.record(Event{Name: "order.created", At: createdAt, Attrs: map[string]any{
		"order_id":   id,
		"item_count": len(items),
	}})
	return o, nil
}

// Total is the order value in cents.
func (o *Order) Total() int64 {
	var total int64
	for _, it := range o.Items {
		total += it.UnitPrice * int64(it.Quantity)
	}
	return total
}

// MarkPaid transitions pending/payment_unknown -> paid.
func (o *Order) MarkPaid(paidAt time.Time, paymentRef string) error {
	if o.Status != OrderStatusPending && o.Status != OrderStatusPaymentUnknown {
		return fmt.Errorf("%w: current status %q", ErrOrderNotPending, o.Status)
	}
	o.Status = OrderStatusPaid
	o.PaymentReference = paymentRef
	o.record(Event{Name: "order.paid", At: paidAt, Attrs: map[string]any{
		"order_id": o.ID,
	}})
	return nil
}

// MarkFailed transitions pending/payment_unknown -> failed.
func (o *Order) MarkFailed(failedAt time.Time, reason string) error {
	if o.Status != OrderStatusPending && o.Status != OrderStatusPaymentUnknown {
		return fmt.Errorf("%w: current status %q", ErrOrderNotPending, o.Status)
	}
	o.Status = OrderStatusFailed
	o.record(Event{Name: "order.failed", At: failedAt, Attrs: map[string]any{
		"order_id": o.ID,
		"reason":   reason,
	}})
	return nil
}

// MarkPaymentUnknown records an ambiguous transport outcome. A timeout does
// not prove that a charge failed: the provider may have committed it before
// the response was lost. Reconciliation may later transition this order to
// paid or failed.
func (o *Order) MarkPaymentUnknown(at time.Time, reason string) error {
	if o.Status != OrderStatusPending {
		return fmt.Errorf("%w: current status %q", ErrOrderNotPending, o.Status)
	}
	o.Status = OrderStatusPaymentUnknown
	o.record(Event{Name: "order.payment_unknown", At: at, Attrs: map[string]any{
		"order_id": o.ID,
		"reason":   reason,
	}})
	return nil
}

// MarkFulfillmentPending records that payment is durable but inventory
// reservation must be retried or reconciled.
func (o *Order) MarkFulfillmentPending(at time.Time, reason string) error {
	if o.Status != OrderStatusPaid {
		return fmt.Errorf("%w: current status %q", ErrOrderNotPaid, o.Status)
	}
	o.Status = OrderStatusFulfillmentPending
	o.record(Event{Name: "order.fulfillment_pending", At: at, Attrs: map[string]any{
		"order_id": o.ID,
		"reason":   reason,
	}})
	return nil
}

// Complete transitions paid/fulfillment_pending -> completed. Allowing the
// retryable state is what lets reconciliation finish an idempotent inventory
// reservation after the original request returned 202. This is the method the
// assessment shows importing "trace" directly - see TRACING.md for why
// that violates domain purity and what replaces it here.
func (o *Order) Complete(completedAt time.Time) error {
	if o.Status != OrderStatusPaid && o.Status != OrderStatusFulfillmentPending {
		return fmt.Errorf("%w: current status %q", ErrOrderNotPaid, o.Status)
	}
	o.Status = OrderStatusCompleted
	o.record(Event{Name: "order.completed", At: completedAt, Attrs: map[string]any{
		"order_id": o.ID,
	}})
	return nil
}

func (o *Order) record(e Event) {
	o.events = append(o.events, e)
}

// PullEvents drains and returns the events recorded since the last call.
// The usecase layer calls this after invoking domain methods and turns
// each Event into span events / log fields / metrics - the domain itself
// never touches those systems.
func (o *Order) PullEvents() []Event {
	events := o.events
	o.events = nil
	return events
}
