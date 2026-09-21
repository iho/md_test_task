ALTER TABLE orders DROP CONSTRAINT orders_status_check;

ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (
    status IN ('pending', 'paid', 'failed', 'payment_unknown', 'fulfillment_pending', 'completed')
);

DROP INDEX orders_pending_status_idx;

CREATE INDEX orders_pending_status_idx
    ON orders (status)
    WHERE status IN ('pending', 'payment_unknown', 'paid', 'fulfillment_pending');
