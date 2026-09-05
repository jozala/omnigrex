ALTER TABLE normalized_events
    DROP CONSTRAINT normalized_events_outcome_check,
    ADD CONSTRAINT normalized_events_outcome_check CHECK (
        (status = 'PENDING' AND workflow_id IS NULL AND disposition IS NULL AND reason IS NULL
            AND applied_revision IS NULL AND deferred_for_turn_id IS NULL AND processed_at IS NULL)
        OR (status = 'DEFERRED' AND disposition = 'DEFERRED' AND reason IS NOT NULL
            AND applied_revision IS NOT NULL AND processed_at IS NOT NULL)
        OR (status = 'COMPLETED' AND disposition IN (
                'APPLIED', 'DUPLICATE', 'STALE', 'UNRELATED', 'ILLEGAL', 'RECONCILIATION_FAILED'
            ) AND reason IS NOT NULL AND applied_revision IS NOT NULL
            AND deferred_for_turn_id IS NULL AND processed_at IS NOT NULL)
    );

CREATE TABLE workflow_github_effect_cleanups (
    job_id UUID PRIMARY KEY REFERENCES jobs (id),
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    issue_number BIGINT NOT NULL CHECK (issue_number > 0),
    pull_request_number BIGINT CHECK (pull_request_number > 0),
    observed_revision BIGINT NOT NULL CHECK (observed_revision > 0),
    observed_state TEXT NOT NULL CHECK (observed_state <> ''),
    current_revision BIGINT NOT NULL CHECK (current_revision > 0),
    current_state TEXT NOT NULL CHECK (current_state <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    cleaned_at TIMESTAMPTZ
);

CREATE INDEX workflow_github_effect_cleanups_pending_idx
    ON workflow_github_effect_cleanups (workflow_id, created_at)
    WHERE cleaned_at IS NULL;
