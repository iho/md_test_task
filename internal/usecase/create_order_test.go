package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ordersvc/internal/domain"
	"ordersvc/internal/usecase"
)

var (
	errFakeNotFound          = errors.New("not found")
	errPaymentGatewayTimeout = errors.New("payment gateway timeout")
	errInventoryTimeout      = errors.New("inventory timeout")
	errDatabaseUnavailable   = errors.New("database unavailable")
	errSaveFailed            = errors.New("save failed")
	errFakeDuplicateOrder    = errors.New("duplicate order ID")
)

type fakeRepo struct {
	orders                map[string]*domain.Order
	saveCalls             int
	saveErrors            []error
	beforeConditionalSave func(*domain.Order)
}

func newFakeRepo() *fakeRepo { return &fakeRepo{orders: map[string]*domain.Order{}} }

func (f *fakeRepo) Save(_ context.Context, order *domain.Order) error {
	if err := f.nextSaveError(); err != nil {
		return err
	}
	if _, exists := f.orders[order.ID]; exists {
		return errFakeDuplicateOrder
	}
	f.store(order)
	return nil
}

func (f *fakeRepo) SaveIfStatus(_ context.Context, order *domain.Order, expected domain.OrderStatus) (bool, error) {
	if err := f.nextSaveError(); err != nil {
		return false, err
	}
	if f.beforeConditionalSave != nil {
		f.beforeConditionalSave(order)
	}
	stored, exists := f.orders[order.ID]
	if !exists || stored.Status != expected {
		return false, nil
	}
	f.store(order)
	return true, nil
}

func (f *fakeRepo) nextSaveError() error {
	f.saveCalls++
	if len(f.saveErrors) >= f.saveCalls {
		return f.saveErrors[f.saveCalls-1]
	}
	return nil
}

func (f *fakeRepo) store(order *domain.Order) {
	copyOrder := *order
	copyOrder.Items = append([]domain.OrderItem(nil), order.Items...)
	copyOrder.PullEvents() // database rows never contain in-memory events
	f.orders[order.ID] = &copyOrder
}

func (f *fakeRepo) FindByID(_ context.Context, id string) (*domain.Order, error) {
	order, ok := f.orders[id]
	if !ok {
		return nil, errFakeNotFound
	}
	return order, nil
}

type fakePayment struct {
	result   usecase.PaymentResult
	err      error
	calls    int
	request  usecase.PaymentRequest
	onCharge func(usecase.PaymentRequest)
}

type fakeInventory struct {
	err       error
	calls     int
	request   usecase.InventoryRequest
	onReserve func(usecase.InventoryRequest)
}

func (f *fakeInventory) Reserve(_ context.Context, request usecase.InventoryRequest) error {
	f.calls++
	f.request = request
	if f.onReserve != nil {
		f.onReserve(request)
	}
	return f.err
}

func (f *fakePayment) Charge(_ context.Context, request usecase.PaymentRequest) (usecase.PaymentResult, error) {
	f.calls++
	f.request = request
	if f.onCharge != nil {
		f.onCharge(request)
	}
	return f.result, f.err
}

type fixedClock struct{ t time.Time }

func (clock fixedClock) Now() time.Time { return clock.t }

type noopTracer struct{}

func (noopTracer) Start(ctx context.Context, _ string) (context.Context, usecase.Span) {
	return ctx, noopSpan{}
}

func (noopTracer) SpanFromContext(context.Context) usecase.Span { return noopSpan{} }

type noopSpan struct{}

func (noopSpan) AddEvent(string, map[string]any) {}
func (noopSpan) RecordError(error)               {}
func (noopSpan) End()                            {}

type noopLogger struct{}

func (noopLogger) Info(context.Context, string, ...any)         {}
func (noopLogger) Error(context.Context, string, error, ...any) {}

type recordingMetrics struct {
	counts map[string]int
	steps  []string
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{counts: map[string]int{}}
}

func (m *recordingMetrics) IncOrdersCreated(status, tier string) {
	m.counts[status+"/"+tier]++
}

func (m *recordingMetrics) ObserveStepDuration(_ context.Context, step string, _ time.Duration) {
	m.steps = append(m.steps, step)
}

func newInteractorWithInventory(
	repo *fakeRepo,
	payment *fakePayment,
	inventory *fakeInventory,
	metrics *recordingMetrics,
) *usecase.Interactor {
	return usecase.NewInteractor(
		repo,
		payment,
		inventory,
		fixedClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		noopTracer{},
		noopLogger{},
		metrics,
	)
}

func newInteractor(repo *fakeRepo, payment *fakePayment, metrics *recordingMetrics) *usecase.Interactor {
	return newInteractorWithInventory(repo, payment, &fakeInventory{}, metrics)
}

func validRequest() *usecase.CreateOrderRequest {
	return &usecase.CreateOrderRequest{
		CustomerID:   "cust_1",
		CustomerTier: usecase.CustomerTierPremium,
		Items: []domain.OrderItem{
			{ProductID: "sku_1", Quantity: 2, UnitPrice: 1500},
		},
	}
}

func TestInteractor_Execute_PaymentApproved_PersistsBeforeChargeAndSendsCorrectContract(t *testing.T) {
	repo := newFakeRepo()
	payment := &fakePayment{result: usecase.PaymentResult{Reference: "ref_1", Status: "approved"}}
	inventory := &fakeInventory{}
	payment.onCharge = func(request usecase.PaymentRequest) {
		saved, ok := repo.orders[request.OrderID]
		require.True(t, ok)
		assert.Equal(t, domain.OrderStatusPending, saved.Status)
	}
	inventory.onReserve = func(request usecase.InventoryRequest) {
		saved, ok := repo.orders[request.OrderID]
		require.True(t, ok)
		assert.Equal(t, domain.OrderStatusPaid, saved.Status)
	}
	metrics := newRecordingMetrics()
	interactor := newInteractorWithInventory(repo, payment, inventory, metrics)

	order, err := interactor.Execute(context.Background(), validRequest())

	require.NoError(t, err)
	require.NotNil(t, order)
	assert.Equal(t, domain.OrderStatusCompleted, order.Status)
	assert.Equal(t, "ref_1", order.PaymentReference)
	assert.Equal(t, 3, repo.saveCalls, "pending, paid, and completed states must be durable in order")
	assert.Equal(t, 1, payment.calls)
	assert.Equal(t, 1, inventory.calls)
	assert.Equal(t, order.ID, payment.request.OrderID)
	assert.Equal(t, order.ID, payment.request.IdempotencyKey)
	assert.Equal(t, "cust_1", payment.request.CustomerID)
	assert.EqualValues(t, 3000, payment.request.AmountCents)
	assert.Equal(t, order.ID, inventory.request.OrderID)
	assert.Equal(t, order.ID, inventory.request.IdempotencyKey)
	assert.Equal(t, validRequest().Items, inventory.request.Items)
	assert.Equal(t, []string{"validation", "payment", "fulfillment"}, metrics.steps)

	saved, err := repo.FindByID(context.Background(), order.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusCompleted, saved.Status)
}

func TestInteractor_Execute_PaymentDeclined_IsDurablyFailed(t *testing.T) {
	repo := newFakeRepo()
	payment := &fakePayment{result: usecase.PaymentResult{Status: "declined"}}
	interactor := newInteractor(repo, payment, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusFailed, order.Status)
	require.Equal(t, 2, repo.saveCalls)
	saved, err := repo.FindByID(context.Background(), order.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusFailed, saved.Status)
}

func TestInteractor_Execute_ChargeTransportError_RemainsReconcilable(t *testing.T) {
	repo := newFakeRepo()
	payment := &fakePayment{err: errPaymentGatewayTimeout}
	metrics := newRecordingMetrics()
	interactor := newInteractor(repo, payment, metrics)

	order, err := interactor.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, usecase.ErrPaymentUncertain)
	require.NotNil(t, order)
	assert.Equal(t, domain.OrderStatusPaymentUnknown, order.Status)
	saved, findErr := repo.FindByID(context.Background(), order.ID)
	require.NoError(t, findErr)
	assert.Equal(t, domain.OrderStatusPaymentUnknown, saved.Status)
}

type definitivePaymentError struct{ message string }

func (err definitivePaymentError) Error() string             { return err.message }
func (definitivePaymentError) OutcomeMayHaveCommitted() bool { return false }

func TestInteractor_Execute_DefinitivePaymentError_IsDurablyFailed(t *testing.T) {
	repo := newFakeRepo()
	payment := &fakePayment{err: definitivePaymentError{message: "unauthorized"}}
	inventory := &fakeInventory{}
	interactor := newInteractorWithInventory(repo, payment, inventory, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, usecase.ErrPaymentRejected)
	require.NotNil(t, order)
	assert.Equal(t, domain.OrderStatusFailed, order.Status)
	assert.Zero(t, inventory.calls)
	saved, findErr := repo.FindByID(context.Background(), order.ID)
	require.NoError(t, findErr)
	assert.Equal(t, domain.OrderStatusFailed, saved.Status)
}

func TestInteractor_Execute_InventoryError_IsDurablyRetryable(t *testing.T) {
	repo := newFakeRepo()
	payment := &fakePayment{result: usecase.PaymentResult{Reference: "ref_1", Status: "approved"}}
	inventory := &fakeInventory{err: errInventoryTimeout}
	interactor := newInteractorWithInventory(repo, payment, inventory, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, usecase.ErrFulfillmentPending)
	assert.Equal(t, domain.OrderStatusFulfillmentPending, order.Status)
	assert.Equal(t, 3, repo.saveCalls)
	saved, findErr := repo.FindByID(context.Background(), order.ID)
	require.NoError(t, findErr)
	assert.Equal(t, domain.OrderStatusFulfillmentPending, saved.Status)
}

func TestInteractor_Execute_ValidationFailureCallsNeitherPaymentNorRepository(t *testing.T) {
	tests := []struct {
		name    string
		request *usecase.CreateOrderRequest
		target  error
	}{
		{name: "nil request", request: nil, target: usecase.ErrNilRequest},
		{name: "empty items", request: &usecase.CreateOrderRequest{CustomerID: "cust_1"}, target: domain.ErrEmptyOrder},
		{name: "invalid tier", request: &usecase.CreateOrderRequest{
			CustomerID: "cust_1", CustomerTier: "enterprise",
			Items: []domain.OrderItem{{ProductID: "sku_1", Quantity: 1, UnitPrice: 10}},
		}, target: usecase.ErrInvalidCustomerTier},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newFakeRepo()
			payment := &fakePayment{}
			inventory := &fakeInventory{}
			interactor := newInteractorWithInventory(repo, payment, inventory, newRecordingMetrics())

			_, err := interactor.Execute(context.Background(), test.request)

			require.ErrorIs(t, err, test.target)
			assert.Zero(t, payment.calls)
			assert.Zero(t, inventory.calls)
			assert.Zero(t, repo.saveCalls)
		})
	}
}

func TestInteractor_Execute_InvalidTierDoesNotLeakIntoTelemetryError(t *testing.T) {
	repo := newFakeRepo()
	interactor := newInteractor(repo, &fakePayment{}, newRecordingMetrics())
	request := validRequest()
	request.CustomerTier = "private-customer-token-123"

	_, err := interactor.Execute(context.Background(), request)

	require.ErrorIs(t, err, usecase.ErrInvalidCustomerTier)
	assert.NotContains(t, err.Error(), string(request.CustomerTier))
}

func TestInteractor_Execute_InitialSaveFailureDoesNotCharge(t *testing.T) {
	repo := newFakeRepo()
	repo.saveErrors = []error{errDatabaseUnavailable}
	payment := &fakePayment{result: usecase.PaymentResult{Reference: "ref_1", Status: "approved"}}
	interactor := newInteractor(repo, payment, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.Error(t, err)
	assert.Nil(t, order)
	assert.Zero(t, payment.calls)
}

func TestInteractor_Execute_PaidSaveFailureLeavesDurablePendingOrder(t *testing.T) {
	repo := newFakeRepo()
	repo.saveErrors = []error{nil, errDatabaseUnavailable}
	payment := &fakePayment{result: usecase.PaymentResult{Reference: "ref_1", Status: "approved"}}
	inventory := &fakeInventory{}
	interactor := newInteractorWithInventory(repo, payment, inventory, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, usecase.ErrPaymentUncertain)
	require.NotNil(t, order)
	assert.Equal(t, domain.OrderStatusPending, order.Status)
	assert.Zero(t, inventory.calls)
	saved, findErr := repo.FindByID(context.Background(), order.ID)
	require.NoError(t, findErr)
	assert.Equal(t, domain.OrderStatusPending, saved.Status)
}

func TestInteractor_Execute_CompletedSaveFailureReturnsDurablePaidState(t *testing.T) {
	repo := newFakeRepo()
	repo.saveErrors = []error{nil, nil, errDatabaseUnavailable}
	payment := &fakePayment{result: usecase.PaymentResult{Reference: "ref_1", Status: "approved"}}
	inventory := &fakeInventory{}
	interactor := newInteractorWithInventory(repo, payment, inventory, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, usecase.ErrFulfillmentPending)
	assert.Equal(t, domain.OrderStatusPaid, order.Status)
	assert.Equal(t, 1, inventory.calls)
	saved, findErr := repo.FindByID(context.Background(), order.ID)
	require.NoError(t, findErr)
	assert.Equal(t, domain.OrderStatusPaid, saved.Status)
}

func TestInteractor_Execute_ConcurrentRecoveryCannotRegressCompletedOrder(t *testing.T) {
	repo := newFakeRepo()
	transitionAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo.beforeConditionalSave = func(next *domain.Order) {
		if next.Status != domain.OrderStatusPaid {
			return
		}
		stored := repo.orders[next.ID]
		require.NoError(t, stored.MarkPaid(transitionAt, "ref_recovered"))
		require.NoError(t, stored.Complete(transitionAt))
		stored.PullEvents()
	}
	payment := &fakePayment{result: usecase.PaymentResult{Reference: "ref_1", Status: "approved"}}
	inventory := &fakeInventory{}
	interactor := newInteractorWithInventory(repo, payment, inventory, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, usecase.ErrPaymentUncertain)
	require.ErrorIs(t, err, usecase.ErrConcurrentStateChange)
	require.NotNil(t, order)
	assert.Equal(t, domain.OrderStatusCompleted, order.Status)
	assert.Equal(t, "ref_recovered", order.PaymentReference)
	assert.Equal(t, domain.OrderStatusCompleted, repo.orders[order.ID].Status)
	assert.Zero(t, inventory.calls, "foreground request must not fulfill after recovery won")
}

func TestInteractor_Execute_ConcurrentCompletionCannotBeOverwritten(t *testing.T) {
	repo := newFakeRepo()
	transitionAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo.beforeConditionalSave = func(next *domain.Order) {
		if next.Status != domain.OrderStatusCompleted {
			return
		}
		stored := repo.orders[next.ID]
		require.NoError(t, stored.Complete(transitionAt))
		stored.PullEvents()
	}
	payment := &fakePayment{result: usecase.PaymentResult{Reference: "ref_1", Status: "approved"}}
	inventory := &fakeInventory{}
	interactor := newInteractorWithInventory(repo, payment, inventory, newRecordingMetrics())

	order, err := interactor.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, usecase.ErrFulfillmentPending)
	require.ErrorIs(t, err, usecase.ErrConcurrentStateChange)
	require.NotNil(t, order)
	assert.Equal(t, domain.OrderStatusCompleted, order.Status)
	assert.Equal(t, domain.OrderStatusCompleted, repo.orders[order.ID].Status)
	assert.Equal(t, 1, inventory.calls)
}

type staticUsecase struct {
	order *domain.Order
	err   error
}

func (usecaseStub staticUsecase) Execute(context.Context, *usecase.CreateOrderRequest) (*domain.Order, error) {
	return usecaseStub.order, usecaseStub.err
}

func TestMetricsInteractor_CountsDeclinedOrderAsFailureAndBoundsUnknownTier(t *testing.T) {
	metrics := newRecordingMetrics()
	decorator := usecase.NewMetricsInteractor(staticUsecase{order: &domain.Order{Status: domain.OrderStatusFailed}}, metrics)

	_, err := decorator.Execute(context.Background(), &usecase.CreateOrderRequest{CustomerTier: "attacker-controlled"})

	require.NoError(t, err)
	assert.Equal(t, 1, metrics.counts["failure/unknown"])
}

func TestMetricsInteractor_DoesNotCountRequestThatCreatedNoOrder(t *testing.T) {
	metrics := newRecordingMetrics()
	decorator := usecase.NewMetricsInteractor(staticUsecase{err: domain.ErrEmptyOrder}, metrics)

	_, err := decorator.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, domain.ErrEmptyOrder)
	assert.Empty(t, metrics.counts)
}

func TestMetricsInteractor_CountsOnlyCompletedOrderAsSuccess(t *testing.T) {
	metrics := newRecordingMetrics()
	decorator := usecase.NewMetricsInteractor(staticUsecase{order: &domain.Order{Status: domain.OrderStatusCompleted}}, metrics)

	_, err := decorator.Execute(context.Background(), validRequest())

	require.NoError(t, err)
	assert.Equal(t, 1, metrics.counts["success/premium"])
}

type contextKey struct{}

type injectingTracer struct{}

func (injectingTracer) Start(ctx context.Context, _ string) (context.Context, usecase.Span) {
	return context.WithValue(ctx, contextKey{}, true), noopSpan{}
}

func (injectingTracer) SpanFromContext(context.Context) usecase.Span { return noopSpan{} }

type contextCheckingUsecase struct {
	sawDerivedContext bool
}

func (inner *contextCheckingUsecase) Execute(ctx context.Context, _ *usecase.CreateOrderRequest) (*domain.Order, error) {
	inner.sawDerivedContext, _ = ctx.Value(contextKey{}).(bool)
	return &domain.Order{Status: domain.OrderStatusCompleted}, nil
}

func TestTracingInteractor_PassesDerivedContextToAllInnerDecorators(t *testing.T) {
	inner := &contextCheckingUsecase{}
	decorator := usecase.NewTracingInteractor(inner, injectingTracer{})

	_, err := decorator.Execute(context.Background(), validRequest())

	require.NoError(t, err)
	assert.True(t, inner.sawDerivedContext)
}

type logEntry struct {
	message string
	err     error
	fields  map[string]any
}

type recordingLogger struct {
	entries []logEntry
}

func (logger *recordingLogger) Info(_ context.Context, message string, values ...any) {
	logger.entries = append(logger.entries, logEntry{message: message, fields: fieldsMap(values)})
}

func (logger *recordingLogger) Error(_ context.Context, message string, err error, values ...any) {
	logger.entries = append(logger.entries, logEntry{message: message, err: err, fields: fieldsMap(values)})
}

func fieldsMap(values []any) map[string]any {
	fields := make(map[string]any, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		key, _ := values[i].(string)
		fields[key] = values[i+1]
	}
	return fields
}

func TestLoggingInteractor_LogsSafeBoundaryFieldsAndBusinessFailureOutcome(t *testing.T) {
	logger := &recordingLogger{}
	inner := staticUsecase{order: &domain.Order{ID: "ord_1", Status: domain.OrderStatusFailed}}
	decorator := usecase.NewLoggingInteractor(inner, logger, fixedClock{t: time.Now()})

	_, err := decorator.Execute(context.Background(), validRequest())

	require.NoError(t, err)
	require.Len(t, logger.entries, 2)
	requestEntry := logger.entries[0]
	assert.Equal(t, "CreateOrder.request", requestEntry.message)
	assert.NotContains(t, requestEntry.fields, "customer_id")
	assert.Equal(t, "premium", requestEntry.fields["customer_tier"])
	responseEntry := logger.entries[1]
	assert.Equal(t, "failure", responseEntry.fields["outcome"])
	assert.Equal(t, "ord_1", responseEntry.fields["order_id"])
}

func TestLoggingInteractor_ErrorIncludesOrderIDAndLabelsStateAsAttempted(t *testing.T) {
	logger := &recordingLogger{}
	innerErr := errSaveFailed
	inner := staticUsecase{
		order: &domain.Order{ID: "ord_1", Status: domain.OrderStatusPaid},
		err:   innerErr,
	}
	decorator := usecase.NewLoggingInteractor(inner, logger, fixedClock{t: time.Now()})

	_, err := decorator.Execute(context.Background(), validRequest())

	require.ErrorIs(t, err, innerErr)
	require.Len(t, logger.entries, 2)
	responseEntry := logger.entries[1]
	assert.ErrorIs(t, responseEntry.err, innerErr)
	assert.Equal(t, "ord_1", responseEntry.fields["order_id"])
	assert.Equal(t, "paid", responseEntry.fields["last_known_status"])
	assert.NotContains(t, responseEntry.fields, "status")
}
