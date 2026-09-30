ALTER TABLE webhook_deliveries
    ADD COLUMN retry_at TIMESTAMPTZ,
    ADD CONSTRAINT webhook_deliveries_retry_at_check CHECK (
        retry_at IS NULL OR status = 'PENDING'
    );

UPDATE webhook_deliveries
SET max_attempts = GREATEST(max_attempts, 8)
WHERE event_name IN ('installation', 'installation_repositories', 'repository')
  AND status IN ('PENDING', 'PROCESSING');
