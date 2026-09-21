// Package paymentclient implements the usecase.PaymentClient port over the
// Payment Service's HTTP API. It is exercised in
// testing/integration/payment_client_test.go against a real WireMock
// instance, not a mock of this package - that catches wire-format bugs
// (wrong field name, wrong status handling) that an in-process mock can't.
package paymentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"ordersvc/internal/usecase"
)

var ErrInvalidResponse = errors.New("payment service returned an invalid response")

type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("payment service returned HTTP status %d", e.StatusCode)
}

// OutcomeMayHaveCommitted distinguishes definitive request rejection from a
// response that may have arrived after the provider committed the charge.
func (e *HTTPStatusError) OutcomeMayHaveCommitted() bool {
	if e.StatusCode >= http.StatusInternalServerError {
		return true
	}
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	clientCopy := *httpClient
	transport := clientCopy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	// otelhttp injects W3C trace context and records a child client span.
	clientCopy.Transport = otelhttp.NewTransport(transport)
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &clientCopy}
}

type chargeRequest struct {
	OrderID     string `json:"order_id"`
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"`
}

type chargeResponse struct {
	Reference string `json:"reference"`
	Status    string `json:"status"`
}

func (c *Client) Charge(ctx context.Context, req usecase.PaymentRequest) (usecase.PaymentResult, error) {
	body, err := json.Marshal(chargeRequest{
		OrderID:     req.OrderID,
		CustomerID:  req.CustomerID,
		AmountCents: req.AmountCents,
	})
	if err != nil {
		return usecase.PaymentResult{}, fmt.Errorf("marshal charge request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/charges", bytes.NewReader(body))
	if err != nil {
		return usecase.PaymentResult{}, fmt.Errorf("build charge request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return usecase.PaymentResult{}, fmt.Errorf("call payment service: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return usecase.PaymentResult{}, &HTTPStatusError{StatusCode: resp.StatusCode}
	}

	return decodeChargeResponse(resp.Body)
}

// LookupCharge is a read-only reconciliation request keyed by the same value
// used for the original charge. A 404 is deliberately not treated as a
// decline: the provider may be eventually consistent.
func (c *Client) LookupCharge(ctx context.Context, idempotencyKey string) (usecase.PaymentResult, bool, error) {
	endpoint := c.baseURL + "/v1/charges/by-idempotency-key/" + url.PathEscape(idempotencyKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return usecase.PaymentResult{}, false, fmt.Errorf("build charge lookup request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return usecase.PaymentResult{}, false, fmt.Errorf("look up payment charge: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusNotFound {
		return usecase.PaymentResult{}, false, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return usecase.PaymentResult{}, false, &HTTPStatusError{StatusCode: resp.StatusCode}
	}
	result, err := decodeChargeResponse(resp.Body)
	if err != nil {
		return usecase.PaymentResult{}, false, err
	}
	return result, true, nil
}

func decodeChargeResponse(body io.Reader) (usecase.PaymentResult, error) {
	var out chargeResponse
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&out); err != nil {
		return usecase.PaymentResult{}, fmt.Errorf("decode charge response: %w", err)
	}
	switch out.Status {
	case "approved":
		if out.Reference == "" {
			return usecase.PaymentResult{}, fmt.Errorf("%w: approved response has no reference", ErrInvalidResponse)
		}
	case "declined":
	default:
		return usecase.PaymentResult{}, fmt.Errorf("%w: unknown status", ErrInvalidResponse)
	}

	return usecase.PaymentResult{Reference: out.Reference, Status: out.Status}, nil
}
