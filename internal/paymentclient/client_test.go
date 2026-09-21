package paymentclient_test

import (
	"context"
	"encoding/json"
	"errors"
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

	"ordersvc/internal/paymentclient"
	"ordersvc/internal/usecase"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestClient_Charge_SendsWireContractIdempotencyKeyAndTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	var received map[string]any
	var idempotencyKey, traceparent, contentType string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		idempotencyKey = request.Header.Get("Idempotency-Key")
		traceparent = request.Header.Get("traceparent")
		contentType = request.Header.Get("Content-Type")
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			return nil, err
		}
		return jsonResponse(http.StatusOK, `{"reference":"ref_123","status":"approved"}`), nil
	})
	client := paymentclient.New("http://payment.test", &http.Client{Transport: transport, Timeout: time.Second})
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	require.NoError(t, err)
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	result, err := client.Charge(ctx, usecase.PaymentRequest{
		OrderID: "ord_1", CustomerID: "cust_1", AmountCents: 3000, IdempotencyKey: "ord_1",
	})

	require.NoError(t, err)
	assert.Equal(t, "approved", result.Status)
	assert.Equal(t, "ord_1", idempotencyKey)
	assert.Equal(t, "application/json", contentType)
	assert.Equal(t, "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", traceparent)
	assert.Equal(t, map[string]any{
		"order_id": "ord_1", "customer_id": "cust_1", "amount_cents": float64(3000),
	}, received)
}

func TestClient_Charge_RejectsEveryNon2xxStatus(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"status":"declined"}`), nil
	})
	client := paymentclient.New("http://payment.test", &http.Client{Transport: transport, Timeout: time.Second})

	_, err := client.Charge(context.Background(), usecase.PaymentRequest{IdempotencyKey: "ord_1"})

	var statusErr *paymentclient.HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusUnauthorized, statusErr.StatusCode)
	assert.False(t, usecase.PaymentOutcomeMayHaveCommitted(err))
}

func TestClient_Charge_Classifies5xxAsAmbiguous(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadGateway, `{"error":"unavailable"}`), nil
	})
	client := paymentclient.New("http://payment.test", &http.Client{Transport: transport, Timeout: time.Second})

	_, err := client.Charge(context.Background(), usecase.PaymentRequest{IdempotencyKey: "ord_1"})

	require.Error(t, err)
	assert.True(t, usecase.PaymentOutcomeMayHaveCommitted(err))
}

func TestClient_Charge_RejectsUnknownBusinessStatus(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"reference":"ref_1","status":"private-provider-token-123"}`), nil
	})
	client := paymentclient.New("http://payment.test", &http.Client{Transport: transport, Timeout: time.Second})

	_, err := client.Charge(context.Background(), usecase.PaymentRequest{IdempotencyKey: "ord_1"})

	require.ErrorIs(t, err, paymentclient.ErrInvalidResponse)
	assert.NotContains(t, err.Error(), "private-provider-token-123")
}

func TestClient_LookupCharge_IsReadOnlyAndDistinguishesMissingRecord(t *testing.T) {
	var method, path, accept string
	status := http.StatusOK
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		method, path, accept = request.Method, request.URL.Path, request.Header.Get("Accept")
		if status == http.StatusNotFound {
			return jsonResponse(status, `{}`), nil
		}
		return jsonResponse(status, `{"reference":"ref_1","status":"approved"}`), nil
	})
	client := paymentclient.New("http://payment.test", &http.Client{Transport: transport})

	result, found, err := client.LookupCharge(context.Background(), "ord_1")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "approved", result.Status)
	assert.Equal(t, "ref_1", result.Reference)
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "/v1/charges/by-idempotency-key/ord_1", path)
	assert.Equal(t, "application/json", accept)

	status = http.StatusNotFound
	_, found, err = client.LookupCharge(context.Background(), "ord_2")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestClient_Charge_EnforcesConfiguredTimeout(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := paymentclient.New("http://payment.test", &http.Client{Transport: transport, Timeout: 20 * time.Millisecond})

	_, err := client.Charge(context.Background(), usecase.PaymentRequest{IdempotencyKey: "ord_1"})

	require.Error(t, err)
	assert.False(t, errors.Is(err, paymentclient.ErrInvalidResponse))
	assert.True(t, usecase.PaymentOutcomeMayHaveCommitted(err))
}
