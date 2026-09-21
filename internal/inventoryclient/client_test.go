package inventoryclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"ordersvc/internal/domain"
	"ordersvc/internal/inventoryclient"
	"ordersvc/internal/usecase"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func response(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}
}

func TestClient_Reserve_SendsWireContractIdempotencyKeyAndTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	var received map[string]any
	var idempotencyKey, traceparent, contentType string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		idempotencyKey = request.Header.Get("Idempotency-Key")
		traceparent = request.Header.Get("traceparent")
		contentType = request.Header.Get("Content-Type")
		require.NoError(t, json.NewDecoder(request.Body).Decode(&received))
		return response(http.StatusNoContent), nil
	})
	client := inventoryclient.New("http://inventory.test", &http.Client{Transport: transport, Timeout: time.Second})
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	require.NoError(t, err)
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	err = client.Reserve(ctx, usecase.InventoryRequest{
		OrderID: "ord_1", IdempotencyKey: "ord_1",
		Items: []domain.OrderItem{{ProductID: "sku_1", Quantity: 2, UnitPrice: 1500}},
	})

	require.NoError(t, err)
	assert.Equal(t, "ord_1", idempotencyKey)
	assert.Equal(t, "application/json", contentType)
	assert.Equal(t, "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", traceparent)
	assert.Equal(t, map[string]any{
		"order_id": "ord_1",
		"items":    []any{map[string]any{"product_id": "sku_1", "quantity": float64(2)}},
	}, received)
}

func TestClient_Reserve_RejectsEveryNon2xxStatus(t *testing.T) {
	client := inventoryclient.New("http://inventory.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusConflict), nil
	})})

	err := client.Reserve(context.Background(), usecase.InventoryRequest{OrderID: "ord_1", IdempotencyKey: "ord_1"})

	var statusErr *inventoryclient.HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusConflict, statusErr.StatusCode)
}

func TestClient_Reserve_EnforcesConfiguredTimeout(t *testing.T) {
	client := inventoryclient.New("http://inventory.test", &http.Client{
		Timeout: 20 * time.Millisecond,
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
	})

	err := client.Reserve(context.Background(), usecase.InventoryRequest{OrderID: "ord_1", IdempotencyKey: "ord_1"})

	require.Error(t, err)
}
