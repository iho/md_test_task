//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"ordersvc/internal/domain"
	"ordersvc/internal/inventoryclient"
	"ordersvc/internal/paymentclient"
	"ordersvc/internal/repository/postgres"
	"ordersvc/internal/usecase"
	"ordersvc/observability"
	"ordersvc/testing/setup"
)

func TestReconciler_RealPostgresAndWireMock_ResolvesChargedOrder(t *testing.T) {
	db := setup.Postgres(t)
	baseURL := setup.WireMock(t)
	repo := postgres.NewOrderRepository(db)
	ctx := context.Background()
	createdAt := time.Now().Add(-time.Hour)
	order, err := domain.NewOrder("ord_recover_1", "cust_1", []domain.OrderItem{
		{ProductID: "sku_1", Quantity: 2, UnitPrice: 1500},
	}, createdAt)
	require.NoError(t, err)
	require.NoError(t, order.MarkPaymentUnknown(createdAt, "charge_timeout"))
	require.NoError(t, repo.Save(ctx, order))

	setup.StubWireMock(t, baseURL, setup.Stub{
		Method: "GET", URLPath: "/v1/charges/by-idempotency-key/ord_recover_1", Status: 200,
		Body: `{"reference":"ref_recovered","status":"approved"}`,
	})
	setup.StubWireMock(t, baseURL, setup.Stub{
		Method: "POST", URLPath: "/v1/reservations", Status: 204,
		Body:           `{}`,
		RequestBody:    `{"order_id":"ord_recover_1","items":[{"product_id":"sku_1","quantity":2}]}`,
		RequestHeaders: map[string]string{"Idempotency-Key": "ord_recover_1"},
	})
	client := &http.Client{Timeout: 3 * time.Second}
	logger := observability.NewLogger(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	reconciler := usecase.NewReconciler(
		repo, paymentclient.New(baseURL, client), inventoryclient.New(baseURL, client),
		usecase.SystemClock{}, observability.NewTracer(otel.Tracer("reconciliation-integration")), logger,
		usecase.ReconcilerConfig{},
	)

	require.NoError(t, reconciler.ReconcileOrder(ctx, order.ID))
	stored, err := repo.FindByID(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusCompleted, stored.Status)
	require.Equal(t, "ref_recovered", stored.PaymentReference)
}
