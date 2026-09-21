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

	"ordersvc/internal/paymentclient"
	"ordersvc/internal/usecase"
	"ordersvc/testing/setup"
)

// TestPaymentClient_ExternalAPI exercises the real paymentclient.Client
// HTTP code (headers, JSON encoding, status handling, client-side timeout)
// against a real WireMock server. A mock of PaymentClient.Charge would
// never catch a wrong header, a typo'd JSON field, or a timeout that isn't
// actually wired to the http.Client - see TESTING.md.
func TestPaymentClient_ExternalAPI(t *testing.T) {
	baseURL := setup.WireMock(t)
	client := paymentclient.New(baseURL, &http.Client{Timeout: 100 * time.Millisecond})
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
			Method: "POST", URLPath: "/v1/charges", Status: 200,
			Body:        `{"reference":"ref_123","status":"approved"}`,
			RequestBody: `{"order_id":"ord_1","customer_id":"cust_1","amount_cents":3000}`,
			RequestHeaders: map[string]string{
				"Content-Type": "application/json", "Idempotency-Key": "ord_1", "traceparent": traceparent,
			},
		})

		result, err := client.Charge(ctx, usecase.PaymentRequest{
			OrderID: "ord_1", CustomerID: "cust_1", AmountCents: 3000, IdempotencyKey: "ord_1",
		})
		require.NoError(t, err)
		require.Equal(t, "approved", result.Status)
		require.Equal(t, "ref_123", result.Reference)
	})

	t.Run("declined", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "POST", URLPath: "/v1/charges", Status: 200,
			Body:           `{"reference":"ref_124","status":"declined"}`,
			RequestBody:    `{"order_id":"ord_2","customer_id":"cust_2","amount_cents":3000}`,
			RequestHeaders: map[string]string{"Idempotency-Key": "ord_2"},
		})

		result, err := client.Charge(ctx, usecase.PaymentRequest{
			OrderID: "ord_2", CustomerID: "cust_2", AmountCents: 3000, IdempotencyKey: "ord_2",
		})
		require.NoError(t, err) // a decline is a valid business outcome, not a transport error
		require.Equal(t, "declined", result.Status)
	})

	t.Run("timeout", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "POST", URLPath: "/v1/charges", Status: 200,
			Body:           `{"reference":"ref_125","status":"approved"}`,
			RequestBody:    `{"order_id":"ord_3","customer_id":"cust_3","amount_cents":3000}`,
			RequestHeaders: map[string]string{"Idempotency-Key": "ord_3"},
			FixedDelayMs:   300,
		})

		_, err := client.Charge(ctx, usecase.PaymentRequest{
			OrderID: "ord_3", CustomerID: "cust_3", AmountCents: 3000, IdempotencyKey: "ord_3",
		})
		require.Error(t, err) // client's 100ms timeout fires before WireMock's 300ms response
	})

	t.Run("upstream 5xx surfaces as an error", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "POST", URLPath: "/v1/charges", Status: 502,
			Body:           `{"error":"upstream unavailable"}`,
			RequestBody:    `{"order_id":"ord_4","customer_id":"cust_4","amount_cents":3000}`,
			RequestHeaders: map[string]string{"Idempotency-Key": "ord_4"},
		})

		_, err := client.Charge(ctx, usecase.PaymentRequest{
			OrderID: "ord_4", CustomerID: "cust_4", AmountCents: 3000, IdempotencyKey: "ord_4",
		})
		require.Error(t, err)
		require.True(t, usecase.PaymentOutcomeMayHaveCommitted(err))
	})

	t.Run("upstream 4xx also surfaces as an error", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "POST", URLPath: "/v1/charges", Status: 401,
			Body:           `{"error":"unauthorized"}`,
			RequestBody:    `{"order_id":"ord_5","customer_id":"cust_5","amount_cents":3000}`,
			RequestHeaders: map[string]string{"Idempotency-Key": "ord_5"},
		})

		_, err := client.Charge(ctx, usecase.PaymentRequest{
			OrderID: "ord_5", CustomerID: "cust_5", AmountCents: 3000, IdempotencyKey: "ord_5",
		})
		require.Error(t, err)
		require.False(t, usecase.PaymentOutcomeMayHaveCommitted(err))
	})

	t.Run("read-only lookup by idempotency key", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "GET", URLPath: "/v1/charges/by-idempotency-key/ord_lookup_1", Status: 200,
			Body: `{"reference":"ref_lookup_1","status":"approved"}`,
			RequestHeaders: map[string]string{
				"Accept": "application/json", "traceparent": traceparent,
			},
		})

		result, found, err := client.LookupCharge(ctx, "ord_lookup_1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "approved", result.Status)
		require.Equal(t, "ref_lookup_1", result.Reference)
	})

	t.Run("missing lookup is not a decline", func(t *testing.T) {
		setup.StubWireMock(t, baseURL, setup.Stub{
			Method: "GET", URLPath: "/v1/charges/by-idempotency-key/ord_lookup_missing", Status: 404,
			Body: `{}`,
		})

		_, found, err := client.LookupCharge(ctx, "ord_lookup_missing")
		require.NoError(t, err)
		require.False(t, found)
	})
}
