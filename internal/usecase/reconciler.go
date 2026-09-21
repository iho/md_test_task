package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ordersvc/internal/domain"
)

var (
	ErrUnknownChargeStatus    = errors.New("reconcile order: unknown provider charge status")
	ErrMissingChargeReference = errors.New("reconcile order: approved charge has no reference")
)

type ReconcilerConfig struct {
	// MinAge keeps recovery away from requests that are still in flight.
	MinAge    time.Duration
	BatchSize int
}

// Reconciler resolves durable intermediate states without re-issuing charges.
// RunOnce is synchronous so the recovery policy can be tested without timers.
type Reconciler struct {
	repo      ReconciliationRepository
	lookup    ChargeLookup
	inventory InventoryClient
	clock     Clock
	tracer    Tracer
	log       Logger
	config    ReconcilerConfig
	afterID   string
}

func NewReconciler(
	repo ReconciliationRepository,
	lookup ChargeLookup,
	inventory InventoryClient,
	clock Clock,
	tracer Tracer,
	log Logger,
	config ReconcilerConfig,
) *Reconciler {
	if config.MinAge <= 0 {
		config.MinAge = time.Minute
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	return &Reconciler{
		repo: repo, lookup: lookup, inventory: inventory, clock: clock,
		tracer: tracer, log: log, config: config,
	}
}

// Run processes a batch immediately and then on each tick. Only one Run or
// RunOnce call should be active per Reconciler instance; a second process may
// run independently because status writes use compare-and-swap.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.Error(ctx, "reconciliation.batch_failed", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce scans one bounded page. The cursor wraps after the last ID, so an
// unresolved old order cannot starve newer orders indefinitely.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cutoff := r.clock.Now().Add(-r.config.MinAge)
	ids, err := r.repo.ListRecoverableIDs(ctx, r.afterID, cutoff, r.config.BatchSize)
	if err != nil {
		return err
	}
	if len(ids) == 0 && r.afterID != "" {
		r.afterID = ""
		ids, err = r.repo.ListRecoverableIDs(ctx, "", cutoff, r.config.BatchSize)
		if err != nil {
			return err
		}
	}
	var failures []error
	for _, id := range ids {
		r.afterID = id
		if err := r.ReconcileOrder(ctx, id); err != nil {
			failures = append(failures, fmt.Errorf("reconcile order %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

// ReconcileOrder is also useful for an operator-driven retry. A missing
// provider record leaves the order unresolved: absence is not proof that a
// charge did not commit, so recovery never submits a second charge.
func (r *Reconciler) ReconcileOrder(ctx context.Context, id string) error {
	ctx, span := r.tracer.Start(ctx, "ReconcileOrder.Execute")
	defer span.End()
	err := r.reconcileOrder(ctx, id)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

func (r *Reconciler) reconcileOrder(ctx context.Context, id string) error {
	order, err := r.repo.FindByID(ctx, id)
	if err != nil {
		return fmt.Errorf("find order: %w", err)
	}
	switch order.Status {
	case domain.OrderStatusPending, domain.OrderStatusPaymentUnknown:
		result, found, err := r.lookup.LookupCharge(ctx, order.ID)
		if err != nil {
			return fmt.Errorf("look up charge: %w", err)
		}
		if !found {
			return nil
		}
		expected := order.Status
		switch result.Status {
		case "approved":
			if result.Reference == "" {
				return ErrMissingChargeReference
			}
			if err := order.MarkPaid(r.clock.Now(), result.Reference); err != nil {
				return err
			}
		case "declined":
			if err := order.MarkFailed(r.clock.Now(), "payment_reconciled_declined"); err != nil {
				return err
			}
		default:
			return ErrUnknownChargeStatus
		}
		updated, err := r.repo.SaveIfStatus(ctx, order, expected)
		if err != nil {
			return fmt.Errorf("save payment outcome: %w", err)
		}
		if !updated {
			return nil // another worker advanced the row
		}
		emitDomainEvents(ctx, r.tracer, r.log, order)
		if order.Status == domain.OrderStatusFailed {
			return nil
		}
		return r.fulfill(ctx, order)
	case domain.OrderStatusPaid, domain.OrderStatusFulfillmentPending:
		return r.fulfill(ctx, order)
	default:
		return nil
	}
}

func (r *Reconciler) fulfill(ctx context.Context, order *domain.Order) error {
	err := r.inventory.Reserve(ctx, InventoryRequest{
		OrderID:        order.ID,
		Items:          append([]domain.OrderItem(nil), order.Items...),
		IdempotencyKey: order.ID,
	})
	if err != nil {
		if order.Status == domain.OrderStatusPaid {
			expected := order.Status
			if transitionErr := order.MarkFulfillmentPending(r.clock.Now(), "inventory_reconciliation_error"); transitionErr != nil {
				return transitionErr
			}
			updated, saveErr := r.repo.SaveIfStatus(ctx, order, expected)
			if saveErr != nil {
				return fmt.Errorf("reserve inventory: %v; save fulfillment state: %w", err, saveErr)
			}
			if updated {
				emitDomainEvents(ctx, r.tracer, r.log, order)
			}
		}
		return fmt.Errorf("reserve inventory: %w", err)
	}
	expected := order.Status
	if err := order.Complete(r.clock.Now()); err != nil {
		return err
	}
	updated, err := r.repo.SaveIfStatus(ctx, order, expected)
	if err != nil {
		return fmt.Errorf("save completed order: %w", err)
	}
	if updated {
		emitDomainEvents(ctx, r.tracer, r.log, order)
	}
	return nil
}
