//go:build integration

// Package integration holds tests that need real infrastructure
// (Postgres, WireMock) via testcontainers. They are excluded from the
// default `go test ./...` run by the "integration" build tag and are run
// separately with `go test -tags=integration ./testing/integration/...`,
// which requires a Docker daemon. See TESTING.md for the full test
// pyramid rationale.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ordersvc/internal/domain"
	"ordersvc/internal/repository/postgres"
	"ordersvc/testing/setup"
)

func TestOrderRepo_SaveAndFindByID_RoundTripsThroughRealDatabase(t *testing.T) {
	db := setup.Postgres(t)
	repo := postgres.NewOrderRepository(db)
	ctx := context.Background()

	order, err := domain.NewOrder("ord_1", "cust_1", []domain.OrderItem{
		{ProductID: "sku_1", Quantity: 2, UnitPrice: 1500},
	}, time.Now().UTC())
	require.NoError(t, err)

	require.NoError(t, repo.Save(ctx, order))

	got, err := repo.FindByID(ctx, order.ID)
	require.NoError(t, err)
	assert.Equal(t, order.CustomerID, got.CustomerID)
	assert.Equal(t, order.Items, got.Items)
	assert.Equal(t, domain.OrderStatusPending, got.Status)
	assert.WithinDuration(t, order.CreatedAt, got.CreatedAt, time.Microsecond)
	count, err := repo.CountPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// A raw round trip through the real driver and a real JSONB column
	// catches what a mock never would: JSON tag typos, type mismatches
	// between Go and the column type, and SQL syntax errors in the query
	// itself.
	var raw string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, order.ID).Scan(&raw))
	assert.Equal(t, "pending", raw)
}

func TestOrderRepo_UpdateStatus_OnlyTouchesTheDirtyField(t *testing.T) {
	db := setup.Postgres(t)
	repo := postgres.NewOrderRepository(db)
	ctx := context.Background()

	order, err := domain.NewOrder("ord_2", "cust_2", []domain.OrderItem{
		{ProductID: "sku_2", Quantity: 1, UnitPrice: 500},
	}, time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, repo.Save(ctx, order))

	require.NoError(t, repo.UpdateStatus(ctx, order.ID, domain.OrderStatusPaid))

	got, err := repo.FindByID(ctx, order.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusPaid, got.Status)
	// customer_id/items are untouched - proves UpdateStatus is a targeted
	// column update, not a full-row overwrite that could clobber a
	// concurrent write to another column.
	assert.Equal(t, order.CustomerID, got.CustomerID)
	assert.Equal(t, order.Items, got.Items)
}

func TestOrderRepo_UpdateStatus_UnknownID_ReturnsNotFound(t *testing.T) {
	db := setup.Postgres(t)
	repo := postgres.NewOrderRepository(db)

	err := repo.UpdateStatus(context.Background(), "does-not-exist", domain.OrderStatusPaid)
	require.ErrorIs(t, err, postgres.ErrNotFound)
}

func TestOrderRepo_FindByID_UnknownID_ReturnsNotFound(t *testing.T) {
	db := setup.Postgres(t)
	repo := postgres.NewOrderRepository(db)

	_, err := repo.FindByID(context.Background(), "does-not-exist")
	require.ErrorIs(t, err, postgres.ErrNotFound)
}

func TestOrderRepo_SaveRejectsDuplicateAndConditionalTransitions(t *testing.T) {
	db := setup.Postgres(t)
	repo := postgres.NewOrderRepository(db)
	ctx := context.Background()

	order, err := domain.NewOrder("ord_3", "cust_3", []domain.OrderItem{
		{ProductID: "sku_3", Quantity: 1, UnitPrice: 100},
	}, time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, repo.Save(ctx, order))
	require.Error(t, repo.Save(ctx, order), "a duplicate create must not overwrite an existing order")

	require.NoError(t, order.MarkPaid(time.Now().UTC(), "ref_1"))
	updated, err := repo.SaveIfStatus(ctx, order, domain.OrderStatusPending)
	require.NoError(t, err)
	require.True(t, updated)

	got, err := repo.FindByID(ctx, order.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusPaid, got.Status)
	assert.Equal(t, "ref_1", got.PaymentReference)
	count, err := repo.CountPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "paid orders still await fulfillment")
	require.NoError(t, order.Complete(time.Now().UTC()))
	updated, err = repo.SaveIfStatus(ctx, order, domain.OrderStatusPaid)
	require.NoError(t, err)
	require.True(t, updated)
	count, err = repo.CountPending(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestOrderRepo_ProductionMigrationEnforcesConstraints(t *testing.T) {
	db := setup.Postgres(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO orders (id, customer_id, items, status, created_at)
		VALUES ('ord_invalid_status', 'cust_1', '[]'::jsonb, 'invented', NOW())
	`)
	require.Error(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO orders (id, customer_id, items, status, created_at)
		VALUES ('ord_invalid_items', 'cust_1', '{}'::jsonb, 'pending', NOW())
	`)
	require.Error(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO orders (id, customer_id, items, status, created_at)
		VALUES ('ord_empty_customer', '', '[]'::jsonb, 'pending', NOW())
	`)
	require.Error(t, err)
}

func TestProductionMigrations_AreIdempotentAndChecksummed(t *testing.T) {
	db := setup.Postgres(t)
	ctx := context.Background()

	require.NoError(t, postgres.Migrate(ctx, db))

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM schema_migrations WHERE checksum <> ''
	`).Scan(&count))
	assert.Equal(t, 2, count)

	_, err := db.ExecContext(ctx, `
		UPDATE schema_migrations SET checksum = 'tampered' WHERE version = '001_orders.sql'
	`)
	require.NoError(t, err)
	err = postgres.Migrate(ctx, db)
	require.ErrorIs(t, err, postgres.ErrMigrationChecksumChanged)
}

func TestOrderRepo_ReconciliationScanAndCompareAndSwap(t *testing.T) {
	db := setup.Postgres(t)
	repo := postgres.NewOrderRepository(db)
	ctx := context.Background()
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old, err := domain.NewOrder("ord_reconcile_old", "cust_1", []domain.OrderItem{
		{ProductID: "sku_1", Quantity: 1, UnitPrice: 100},
	}, createdAt)
	require.NoError(t, err)
	require.NoError(t, repo.Save(ctx, old))
	fresh, err := domain.NewOrder("ord_reconcile_fresh", "cust_1", []domain.OrderItem{
		{ProductID: "sku_1", Quantity: 1, UnitPrice: 100},
	}, createdAt.Add(2*time.Hour))
	require.NoError(t, err)
	require.NoError(t, repo.Save(ctx, fresh))

	ids, err := repo.ListRecoverableIDs(ctx, "", createdAt.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, []string{"ord_reconcile_old"}, ids)
	ids, err = repo.ListRecoverableIDs(ctx, "ord_reconcile_old", createdAt.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, ids)

	require.NoError(t, old.MarkPaid(createdAt, "ref_1"))
	updated, err := repo.SaveIfStatus(ctx, old, domain.OrderStatusPending)
	require.NoError(t, err)
	require.True(t, updated)
	stale := *old
	stale.Status = domain.OrderStatusFailed
	updated, err = repo.SaveIfStatus(ctx, &stale, domain.OrderStatusPending)
	require.NoError(t, err)
	require.False(t, updated)
	stored, err := repo.FindByID(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, stored.Status)
	require.Equal(t, "ref_1", stored.PaymentReference)
}
