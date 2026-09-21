package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ordersvc/internal/domain"
	"ordersvc/internal/httpapi"
	"ordersvc/internal/usecase"
)

var errUnexpectedCreateOrderCall = errors.New("must not be called")

type createOrderStub struct {
	order   *domain.Order
	err     error
	request *usecase.CreateOrderRequest
}

func (stub *createOrderStub) Execute(_ context.Context, request *usecase.CreateOrderRequest) (*domain.Order, error) {
	stub.request = request
	return stub.order, stub.err
}

func performRequest(t *testing.T, stub *createOrderStub, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := httpapi.New(stub)
	request := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.CreateOrder(recorder, request)
	return recorder
}

func TestCreateOrder_DecodesExplicitWireDTOAndReturnsCreated(t *testing.T) {
	stub := &createOrderStub{order: &domain.Order{ID: "ord_1", Status: domain.OrderStatusCompleted}}
	recorder := performRequest(t, stub, `{
		"customer_id":"cust_1",
		"customer_tier":"premium",
		"items":[{"product_id":"sku_1","quantity":2,"unit_price_cents":1500}]
	}`)

	require.Equal(t, http.StatusCreated, recorder.Code)
	require.NotNil(t, stub.request)
	assert.Equal(t, "cust_1", stub.request.CustomerID)
	assert.Equal(t, usecase.CustomerTierPremium, stub.request.CustomerTier)
	assert.EqualValues(t, 3000, stub.request.Items[0].UnitPrice*int64(stub.request.Items[0].Quantity))
	assert.JSONEq(t, `{"id":"ord_1","status":"completed"}`, recorder.Body.String())
}

func TestCreateOrder_FulfillmentPendingReturnsAccepted(t *testing.T) {
	stub := &createOrderStub{
		order: &domain.Order{ID: "ord_1", Status: domain.OrderStatusFulfillmentPending},
		err:   usecase.ErrFulfillmentPending,
	}
	recorder := performRequest(t, stub, `{"customer_id":"cust_1","items":[{"product_id":"sku_1","quantity":1,"unit_price_cents":100}]}`)

	assert.Equal(t, http.StatusAccepted, recorder.Code)
	assert.JSONEq(t, `{"id":"ord_1","status":"fulfillment_pending"}`, recorder.Body.String())
}

func TestCreateOrder_DefinitivePaymentRejectionReturnsBadGateway(t *testing.T) {
	stub := &createOrderStub{
		order: &domain.Order{ID: "ord_1", Status: domain.OrderStatusFailed},
		err:   usecase.ErrPaymentRejected,
	}
	recorder := performRequest(t, stub, `{"customer_id":"cust_1","items":[{"product_id":"sku_1","quantity":1,"unit_price_cents":100}]}`)

	assert.Equal(t, http.StatusBadGateway, recorder.Code)
	assert.JSONEq(t, `{"id":"ord_1","status":"failed"}`, recorder.Body.String())
}

func TestCreateOrder_AmbiguousPaymentReturnsAcceptedInsteadOfFailure(t *testing.T) {
	stub := &createOrderStub{
		order: &domain.Order{ID: "ord_1", Status: domain.OrderStatusPaymentUnknown},
		err:   usecase.ErrPaymentUncertain,
	}
	recorder := performRequest(t, stub, `{"customer_id":"cust_1","items":[{"product_id":"sku_1","quantity":1,"unit_price_cents":100}]}`)

	assert.Equal(t, http.StatusAccepted, recorder.Code)
	assert.JSONEq(t, `{"id":"ord_1","status":"payment_unknown"}`, recorder.Body.String())
}

func TestCreateOrder_DeclineReturnsPaymentRequired(t *testing.T) {
	stub := &createOrderStub{order: &domain.Order{ID: "ord_1", Status: domain.OrderStatusFailed}}
	recorder := performRequest(t, stub, `{"customer_id":"cust_1","items":[{"product_id":"sku_1","quantity":1,"unit_price_cents":100}]}`)

	assert.Equal(t, http.StatusPaymentRequired, recorder.Code)
}

func TestCreateOrder_RejectsMalformedAndUnknownFields(t *testing.T) {
	stub := &createOrderStub{err: errUnexpectedCreateOrderCall}
	recorder := performRequest(t, stub, `{"customer_id":"cust_1","card_number":"4111111111111111"}`)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Nil(t, stub.request)
}
