CREATE TABLE workflow_action_failures (
    source_job_id UUID PRIMARY KEY REFERENCES jobs (id),
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    escalation_job_id UUID UNIQUE REFERENCES jobs (id),
    source_kind TEXT NOT NULL CHECK (source_kind <> ''),
    diagnostic TEXT NOT NULL CHECK (diagnostic <> ''),
    resume_role TEXT CHECK (resume_role IN ('DEVELOPER', 'REVIEWER')),
    observed_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'APPLIED', 'TERMINAL')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at TIMESTAMPTZ,
    CONSTRAINT workflow_action_failures_resolution_check CHECK (
        (status = 'PENDING' AND escalation_job_id IS NOT NULL AND resolved_at IS NULL)
        OR (status IN ('APPLIED', 'TERMINAL') AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX workflow_action_failures_pending_idx
    ON workflow_action_failures (workflow_id, created_at)
    WHERE status = 'PENDING';

ALTER TABLE workflow_github_effect_cleanups
    ADD COLUMN status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'SUCCEEDED', 'EXHAUSTED')),
    ADD COLUMN last_error TEXT,
    ADD COLUMN exhausted_at TIMESTAMPTZ;

UPDATE workflow_github_effect_cleanups
SET status = 'SUCCEEDED'
WHERE cleaned_at IS NOT NULL;

ALTER TABLE workflow_github_effect_cleanups
    ADD CONSTRAINT workflow_github_effect_cleanups_outcome_check CHECK (
        (status = 'PENDING' AND cleaned_at IS NULL AND exhausted_at IS NULL)
        OR (status = 'SUCCEEDED' AND cleaned_at IS NOT NULL AND exhausted_at IS NULL
            AND last_error IS NULL)
        OR (status = 'EXHAUSTED' AND cleaned_at IS NULL AND exhausted_at IS NOT NULL
            AND last_error IS NOT NULL AND last_error <> '')
    );

DROP INDEX workflow_github_effect_cleanups_pending_idx;
CREATE INDEX workflow_github_effect_cleanups_pending_idx
    ON workflow_github_effect_cleanups (workflow_id, created_at)
    WHERE status = 'PENDING';
