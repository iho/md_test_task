// Package postgres implements the usecase.OrderRepository port against a
// real Postgres database. It has no knowledge of tracing/logging/metrics:
// the caller's context carries cancellation and trace context, while the
// TracingOrderRepository port decorator creates DB-operation child spans.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ordersvc/internal/domain"
)

var ErrNotFound = errors.New("order not found")

type OrderRepository struct {
	db *sql.DB
}

func NewOrderRepository(db *sql.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

func (r *OrderRepository) Save(ctx context.Context, order *domain.Order) error {
	items, err := json.Marshal(order.Items)
	if err != nil {
		return fmt.Errorf("marshal items: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO orders (id, customer_id, items, status, created_at, payment_reference)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, order.ID, order.CustomerID, items, string(order.Status), order.CreatedAt, nullableString(order.PaymentReference))
	if err != nil {
		return fmt.Errorf("save order %s: %w", order.ID, err)
	}
	return nil
}

// SaveIfStatus persists a recovery transition only if the row is still in the
// state the worker read. A competing worker can win without state regression.
func (r *OrderRepository) SaveIfStatus(ctx context.Context, order *domain.Order, expected domain.OrderStatus) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE orders SET status = $3, payment_reference = $4
		WHERE id = $1 AND status = $2
	`, order.ID, string(expected), string(order.Status), nullableString(order.PaymentReference))
	if err != nil {
		return false, fmt.Errorf("conditionally save order %s: %w", order.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("conditionally save order %s: %w", order.ID, err)
	}
	return rows == 1, nil
}

// ListRecoverableIDs pages by the primary key so unresolved old orders cannot
// permanently starve newer orders at the front of a fixed-size batch.
func (r *OrderRepository) ListRecoverableIDs(ctx context.Context, afterID string, olderThan time.Time, limit int) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM orders
		WHERE id > $1 AND created_at <= $2
		  AND status IN ('pending', 'payment_unknown', 'paid', 'fulfillment_pending')
		ORDER BY id LIMIT $3
	`, afterID, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("list recoverable orders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan recoverable order: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable orders: %w", err)
	}
	return ids, nil
}

// UpdateStatus updates only the status column - a targeted, "dirty field"
// update rather than a full-row overwrite, so it cannot clobber a
// concurrent write to another column (see testing/integration/order_repo_test.go).
func (r *OrderRepository) UpdateStatus(ctx context.Context, id string, status domain.OrderStatus) error {
	res, err := r.db.ExecContext(ctx, `UPDATE orders SET status = $2 WHERE id = $1`, id, string(status))
	if err != nil {
		return fmt.Errorf("update order %s status: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update order %s status: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	return nil
}

func (r *OrderRepository) FindByID(ctx context.Context, id string) (*domain.Order, error) {
	row := r.db.QueryRowContext(ctx, `
			SELECT id, customer_id, items, status, created_at, payment_reference FROM orders WHERE id = $1
	`, id)

	var (
		orderID, customerID, status string
		itemsJSON                   []byte
		createdAt                   time.Time
		paymentReference            sql.NullString
	)
	if err := row.Scan(&orderID, &customerID, &itemsJSON, &status, &createdAt, &paymentReference); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("order %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("find order %s: %w", id, err)
	}

	var items []domain.OrderItem
	if err := json.Unmarshal(itemsJSON, &items); err != nil {
		return nil, fmt.Errorf("unmarshal items for order %s: %w", id, err)
	}

	return &domain.Order{
		ID:               orderID,
		CustomerID:       customerID,
		Items:            items,
		Status:           domain.OrderStatus(status),
		CreatedAt:        createdAt,
		PaymentReference: paymentReference.String,
	}, nil
}

func (r *OrderRepository) CountPending(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM orders
		WHERE status IN ('pending', 'payment_unknown', 'paid', 'fulfillment_pending')
	`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count pending orders: %w", err)
	}
	return count, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
