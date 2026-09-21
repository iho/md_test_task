CREATE TABLE IF NOT EXISTS orders (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    customer_id TEXT NOT NULL CHECK (customer_id <> ''),
    items JSONB NOT NULL CHECK (jsonb_typeof(items) = 'array'),
    status TEXT NOT NULL CHECK (
        status IN ('pending', 'paid', 'failed', 'payment_unknown', 'completed')
    ),
    created_at TIMESTAMPTZ NOT NULL,
    payment_reference TEXT NULL
);

CREATE INDEX IF NOT EXISTS orders_pending_status_idx
    ON orders (status)
    WHERE status IN ('pending', 'payment_unknown');
