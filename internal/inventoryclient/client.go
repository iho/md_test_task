// Package inventoryclient implements the fulfillment port over HTTP.
package inventoryclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"ordersvc/internal/usecase"
)

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
	clientCopy.Transport = otelhttp.NewTransport(transport)
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &clientCopy}
}

type reservationItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

type reservationRequest struct {
	OrderID string            `json:"order_id"`
	Items   []reservationItem `json:"items"`
}

type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("inventory service returned HTTP status %d", e.StatusCode)
}

func (c *Client) Reserve(ctx context.Context, req usecase.InventoryRequest) error {
	items := make([]reservationItem, len(req.Items))
	for i, item := range req.Items {
		items[i] = reservationItem{ProductID: item.ProductID, Quantity: item.Quantity}
	}
	body, err := json.Marshal(reservationRequest{OrderID: req.OrderID, Items: items})
	if err != nil {
		return fmt.Errorf("marshal inventory reservation: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/v1/reservations",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("build inventory reservation: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)

	response, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call inventory service: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &HTTPStatusError{StatusCode: response.StatusCode}
	}
	return nil
}
