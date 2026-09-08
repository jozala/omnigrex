UPDATE webhook_deliveries
SET max_attempts = attempt_count + CASE WHEN status IN ('PENDING', 'PROCESSING') THEN 1 ELSE 0 END
WHERE attempt_count >= 3 AND (
    attempt_count > max_attempts
    OR (attempt_count = max_attempts AND status IN ('PENDING', 'PROCESSING'))
);

ALTER TABLE webhook_deliveries
    VALIDATE CONSTRAINT webhook_deliveries_attempt_limit_check;

ALTER TABLE webhook_deliveries
    ALTER COLUMN max_attempts SET NOT NULL;
