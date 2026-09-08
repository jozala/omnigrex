ALTER TABLE webhook_deliveries
    ADD COLUMN max_attempts INTEGER DEFAULT 3,
    ADD CONSTRAINT webhook_deliveries_attempt_limit_check CHECK (
        max_attempts IS NOT NULL AND max_attempts > 0 AND attempt_count <= max_attempts
    ) NOT VALID;
