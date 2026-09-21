package usecase_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ordersvc/internal/domain"
	"ordersvc/internal/usecase"
)

type reconcileRepo struct {
	*fakeRepo
	casCalls    int
	casConflict bool
}

func (r *reconcileRepo) FindByID(ctx context.Context, id string) (*domain.Order, error) {
	stored, err := r.fakeRepo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	copyOrder := *stored
	copyOrder.Items = append([]domain.OrderItem(nil), stored.Items...)
	return &copyOrder, nil
}

func (r *reconcileRepo) ListRecoverableIDs(_ context.Context, afterID string, olderThan time.Time, limit int) ([]string, error) {
	var ids []string
	for id, order := range r.orders {
		if id <= afterID || order.CreatedAt.After(olderThan) {
			continue
		}
		switch order.Status {
		case domain.OrderStatusPending, domain.OrderStatusPaymentUnknown,
			domain.OrderStatusPaid, domain.OrderStatusFulfillmentPending:
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (r *reconcileRepo) SaveIfStatus(ctx context.Context, order *domain.Order, expected domain.OrderStatus) (bool, error) {
	r.casCalls++
	if r.casConflict || r.orders[order.ID].Status != expected {
		return false, nil
	}
	return r.fakeRepo.SaveIfStatus(ctx, order, expected)
}

type fakeChargeLookup struct {
	result usecase.PaymentResult
	found  bool
	err    error
	calls  int
	key    string
}

func (f *fakeChargeLookup) LookupCharge(_ context.Context, key string) (usecase.PaymentResult, bool, error) {
	f.calls++
	f.key = key
	return f.result, f.found, f.err
}

func seededOrder(t *testing.T, status domain.OrderStatus) *domain.Order {
	t.Helper()
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	order, err := domain.NewOrder("ord_1", "cust_1", []domain.OrderItem{
		{ProductID: "sku_1", Quantity: 2, UnitPrice: 100},
	}, createdAt)
	require.NoError(t, err)
	order.PullEvents()
	switch status {
	case domain.OrderStatusPending:
	case domain.OrderStatusPaymentUnknown:
		require.NoError(t, order.MarkPaymentUnknown(createdAt, "timeout"))
	case domain.OrderStatusPaid:
		require.NoError(t, order.MarkPaid(createdAt, "ref_1"))
	case domain.OrderStatusFulfillmentPending:
		require.NoError(t, order.MarkPaid(createdAt, "ref_1"))
		require.NoError(t, order.MarkFulfillmentPending(createdAt, "inventory timeout"))
	default:
		t.Fatalf("unsupported seed status %q", status)
	}
	order.PullEvents()
	return order
}

func newTestReconciler(repo *reconcileRepo, lookup *fakeChargeLookup, inventory *fakeInventory) *usecase.Reconciler {
	return usecase.NewReconciler(
		repo, lookup, inventory,
		fixedClock{t: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		noopTracer{}, noopLogger{},
		usecase.ReconcilerConfig{MinAge: time.Minute, BatchSize: 1},
	)
}

func TestReconciler_ApprovedUnknownChargeCompletesWithoutChargingAgain(t *testing.T) {
	repo := &reconcileRepo{fakeRepo: newFakeRepo()}
	repo.orders["ord_1"] = seededOrder(t, domain.OrderStatusPaymentUnknown)
	lookup := &fakeChargeLookup{result: usecase.PaymentResult{Status: "approved", Reference: "ref_1"}, found: true}
	inventory := &fakeInventory{}
	reconciler := newTestReconciler(repo, lookup, inventory)

	require.NoError(t, reconciler.RunOnce(context.Background()))

	assert.Equal(t, "ord_1", lookup.key)
	assert.Equal(t, 1, lookup.calls)
	assert.Equal(t, 1, inventory.calls)
	assert.Equal(t, "ord_1", inventory.request.IdempotencyKey)
	assert.Equal(t, domain.OrderStatusCompleted, repo.orders["ord_1"].Status)
	assert.Equal(t, "ref_1", repo.orders["ord_1"].PaymentReference)
	assert.Equal(t, 2, repo.casCalls)
}

func TestReconciler_DeclineAndAbsentChargeDoNotReserveInventory(t *testing.T) {
	for _, test := range []struct {
		name   string
		lookup fakeChargeLookup
		want   domain.OrderStatus
	}{
		{name: "declined", lookup: fakeChargeLookup{result: usecase.PaymentResult{Status: "declined"}, found: true}, want: domain.OrderStatusFailed},
		{name: "not found", lookup: fakeChargeLookup{}, want: domain.OrderStatusPaymentUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &reconcileRepo{fakeRepo: newFakeRepo()}
			repo.orders["ord_1"] = seededOrder(t, domain.OrderStatusPaymentUnknown)
			inventory := &fakeInventory{}
			reconciler := newTestReconciler(repo, &test.lookup, inventory)

			require.NoError(t, reconciler.ReconcileOrder(context.Background(), "ord_1"))

			assert.Equal(t, test.want, repo.orders["ord_1"].Status)
			assert.Zero(t, inventory.calls)
		})
	}
}

func TestReconciler_RetriesFulfillmentWithSameIdempotencyKey(t *testing.T) {
	repo := &reconcileRepo{fakeRepo: newFakeRepo()}
	repo.orders["ord_1"] = seededOrder(t, domain.OrderStatusPaid)
	lookup := &fakeChargeLookup{}
	inventory := &fakeInventory{err: errInventoryTimeout}
	reconciler := newTestReconciler(repo, lookup, inventory)

	require.ErrorIs(t, reconciler.ReconcileOrder(context.Background(), "ord_1"), errInventoryTimeout)
	assert.Equal(t, domain.OrderStatusFulfillmentPending, repo.orders["ord_1"].Status)
	inventory.err = nil
	require.NoError(t, reconciler.ReconcileOrder(context.Background(), "ord_1"))

	assert.Equal(t, domain.OrderStatusCompleted, repo.orders["ord_1"].Status)
	assert.Equal(t, 2, inventory.calls)
	assert.Equal(t, "ord_1", inventory.request.IdempotencyKey)
	assert.Zero(t, lookup.calls)
}

func TestReconciler_CompareAndSwapConflictDoesNotStartFulfillment(t *testing.T) {
	repo := &reconcileRepo{fakeRepo: newFakeRepo(), casConflict: true}
	repo.orders["ord_1"] = seededOrder(t, domain.OrderStatusPaymentUnknown)
	lookup := &fakeChargeLookup{result: usecase.PaymentResult{Status: "approved", Reference: "ref_1"}, found: true}
	inventory := &fakeInventory{}
	reconciler := newTestReconciler(repo, lookup, inventory)

	require.NoError(t, reconciler.ReconcileOrder(context.Background(), "ord_1"))

	assert.Equal(t, domain.OrderStatusPaymentUnknown, repo.orders["ord_1"].Status)
	assert.Zero(t, inventory.calls)
}

func TestReconciler_RunOncePagesPastUnresolvedOrdersAndSkipsFreshOnes(t *testing.T) {
	repo := &reconcileRepo{fakeRepo: newFakeRepo()}
	repo.orders["ord_1"] = seededOrder(t, domain.OrderStatusPaymentUnknown)
	second := seededOrder(t, domain.OrderStatusPaymentUnknown)
	second.ID = "ord_2"
	repo.orders[second.ID] = second
	fresh := seededOrder(t, domain.OrderStatusPaymentUnknown)
	fresh.ID = "ord_3"
	fresh.CreatedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	repo.orders[fresh.ID] = fresh
	lookup := &fakeChargeLookup{}
	reconciler := newTestReconciler(repo, lookup, &fakeInventory{})

	require.NoError(t, reconciler.RunOnce(context.Background()))
	assert.Equal(t, "ord_1", lookup.key)
	require.NoError(t, reconciler.RunOnce(context.Background()))
	assert.Equal(t, "ord_2", lookup.key)
	require.NoError(t, reconciler.RunOnce(context.Background()))
	assert.Equal(t, "ord_1", lookup.key)
	assert.Equal(t, 3, lookup.calls)
}
