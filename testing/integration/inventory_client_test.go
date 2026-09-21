//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"ordersvc/internal/domain"
	"ordersvc/internal/inventoryclient"
	"ordersvc/internal/usecase"
	"ordersvc/testing/setup"
)

func TestInventoryClient_ExternalAPI(t *testing.T) {
	baseURL := setup.WireMock(t)
	client := inventoryclient.New(baseURL, &http.Client{Timeout: 100 * time.Millisecond})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	require.NoError(t, err)
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	traceparent := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

	t.Run("success", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "POST", URLPath: "/v1/reservations", Status: http.StatusNoContent,
			Body:        `{}`,
			RequestBody: `{"order_id":"ord_1","items":[{"product_id":"sku_1","quantity":2}]}`,
			RequestHeaders: map[string]string{
				"Content-Type": "application/json", "Idempotency-Key": "ord_1", "traceparent": traceparent,
			},
		})

		err := client.Reserve(ctx, usecase.InventoryRequest{
			OrderID: "ord_1", IdempotencyKey: "ord_1",
			Items: []domain.OrderItem{{ProductID: "sku_1", Quantity: 2, UnitPrice: 1500}},
		})
		require.NoError(t, err)
	})

	t.Run("upstream failure", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "POST", URLPath: "/v1/reservations", Status: http.StatusConflict,
			Body:           `{"error":"out of stock"}`,
			RequestBody:    `{"order_id":"ord_2","items":[{"product_id":"sku_2","quantity":1}]}`,
			RequestHeaders: map[string]string{"Idempotency-Key": "ord_2"},
		})

		err := client.Reserve(ctx, usecase.InventoryRequest{
			OrderID: "ord_2", IdempotencyKey: "ord_2",
			Items: []domain.OrderItem{{ProductID: "sku_2", Quantity: 1}},
		})
		var statusErr *inventoryclient.HTTPStatusError
		require.ErrorAs(t, err, &statusErr)
		require.Equal(t, http.StatusConflict, statusErr.StatusCode)
	})

	t.Run("timeout", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "POST", URLPath: "/v1/reservations", Status: http.StatusNoContent,
			Body:           `{}`,
			RequestBody:    `{"order_id":"ord_3","items":[]}`,
			RequestHeaders: map[string]string{"Idempotency-Key": "ord_3"},
			FixedDelayMs:   300,
		})

		err := client.Reserve(ctx, usecase.InventoryRequest{OrderID: "ord_3", IdempotencyKey: "ord_3"})
		require.Error(t, err)
	})
}
