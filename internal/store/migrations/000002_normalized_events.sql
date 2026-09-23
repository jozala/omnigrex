ALTER TABLE webhook_deliveries
    ALTER COLUMN repository_id DROP NOT NULL,
    ALTER COLUMN repository_owner DROP NOT NULL,
    ALTER COLUMN repository_name DROP NOT NULL;

CREATE TABLE normalized_events (
    delivery_id UUID PRIMARY KEY REFERENCES webhook_deliveries (delivery_id),
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status = 'PENDING'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX normalized_events_pending_idx
    ON normalized_events (created_at, delivery_id)
    WHERE status = 'PENDING';
