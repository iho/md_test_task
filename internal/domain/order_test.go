package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ordersvc/internal/domain"
)

func validOrder(t *testing.T) *domain.Order {
	t.Helper()
	order, err := domain.NewOrder("ord_1", "cust_1", []domain.OrderItem{
		{ProductID: "sku_1", Quantity: 2, UnitPrice: 100},
	}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	return order
}

func TestOrder_ValidatesItemsAndDefensivelyCopiesThem(t *testing.T) {
	items := []domain.OrderItem{{ProductID: "sku_1", Quantity: 1, UnitPrice: 100}}
	order, err := domain.NewOrder("ord_1", "cust_1", items, time.Now())
	require.NoError(t, err)
	items[0].Quantity = 99
	assert.Equal(t, 1, order.Items[0].Quantity)

	_, err = domain.NewOrder("ord_2", "cust_1", []domain.OrderItem{{ProductID: "", Quantity: 1}}, time.Now())
	require.ErrorIs(t, err, domain.ErrInvalidOrderItem)
}

func TestOrder_PaymentUnknownCanBeReconciledToPaid(t *testing.T) {
	order := validOrder(t)
	require.NoError(t, order.MarkPaymentUnknown(time.Now(), "transport_error"))

	err := order.MarkPaid(time.Now(), "ref_1")

	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusPaid, order.Status)
	assert.Equal(t, "ref_1", order.PaymentReference)
}

func TestOrder_PaymentUnknownCanBeReconciledToFailed(t *testing.T) {
	order := validOrder(t)
	require.NoError(t, order.MarkPaymentUnknown(time.Now(), "transport_error"))

	err := order.MarkFailed(time.Now(), "provider_confirmed_decline")

	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusFailed, order.Status)
}

func TestOrder_FulfillmentPendingRequiresPaidState(t *testing.T) {
	order := validOrder(t)

	err := order.MarkFulfillmentPending(time.Now(), "inventory timeout")
	require.ErrorIs(t, err, domain.ErrOrderNotPaid)

	require.NoError(t, order.MarkPaid(time.Now(), "ref_1"))
	require.NoError(t, order.MarkFulfillmentPending(time.Now(), "inventory timeout"))
	assert.Equal(t, domain.OrderStatusFulfillmentPending, order.Status)

	require.NoError(t, order.Complete(time.Now()))
	assert.Equal(t, domain.OrderStatusCompleted, order.Status)
}
