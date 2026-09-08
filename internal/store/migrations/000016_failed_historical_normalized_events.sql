ALTER TABLE normalized_events
    DROP CONSTRAINT normalized_events_status_check,
    DROP CONSTRAINT normalized_events_outcome_check,
    ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 3,
    ADD COLUMN last_error TEXT,
    ADD CONSTRAINT normalized_events_attempt_limit_check CHECK (
        attempt_count >= 0 AND max_attempts > 0 AND attempt_count <= max_attempts
    ) NOT VALID,
    ADD CONSTRAINT normalized_events_status_check CHECK (
        status IN ('PENDING', 'DEFERRED', 'COMPLETED', 'FAILED')
    ) NOT VALID,
    ADD CONSTRAINT normalized_events_outcome_check CHECK (
        (status = 'PENDING' AND workflow_id IS NULL AND disposition IS NULL AND reason IS NULL
            AND applied_revision IS NULL AND deferred_for_turn_id IS NULL AND processed_at IS NULL)
        OR (status = 'DEFERRED' AND disposition = 'DEFERRED' AND reason IS NOT NULL
            AND applied_revision IS NOT NULL AND processed_at IS NOT NULL AND last_error IS NULL)
        OR (status = 'COMPLETED' AND disposition IN (
                'APPLIED', 'DUPLICATE', 'STALE', 'UNRELATED', 'ILLEGAL', 'RECONCILIATION_FAILED'
            ) AND reason IS NOT NULL AND applied_revision IS NOT NULL
            AND deferred_for_turn_id IS NULL AND processed_at IS NOT NULL AND last_error IS NULL)
        OR (status = 'FAILED' AND workflow_id IS NULL AND disposition IS NULL
            AND reason IS NOT NULL AND reason <> '' AND applied_revision IS NULL
            AND deferred_for_turn_id IS NULL AND processed_at IS NOT NULL
            AND last_error = reason)
    ) NOT VALID;
