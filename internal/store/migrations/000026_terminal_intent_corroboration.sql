-- A successful terminal MCP mutation is not by itself a Workflow outcome.
-- This checkpoint pins the exact Turn and mutation whose ACP completion is
-- eligible for later, read-only corroboration after its Runtime Process stops.
ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_status_check,
    ADD CONSTRAINT agent_turns_status_check CHECK (status IN (
        'QUEUED', 'STARTING', 'RUNNING', 'CANCELLING', 'SETTLING', 'RECONCILING',
        'CORROBORATING', 'SUCCEEDED', 'FAILED', 'INTERRUPTED', 'TIMED_OUT'
    ));

CREATE TABLE agent_turn_corroborations (
    agent_turn_id UUID PRIMARY KEY,
    execution_epoch BIGINT NOT NULL CHECK (execution_epoch > 0),
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    workflow_attempt_id UUID NOT NULL,
    source_invocation_id UUID NOT NULL REFERENCES tool_invocations (id),
    execution_job_id UUID NOT NULL REFERENCES jobs (id),
    execution_owner_id TEXT NOT NULL CHECK (execution_owner_id <> ''),
    execution_owner_token_sha256 BYTEA NOT NULL CHECK (octet_length(execution_owner_token_sha256) = 32),
    prompt_outcome JSONB,
    prompt_error TEXT CHECK (prompt_error IN ('FAILURE', 'DEADLINE')),
    pending_since TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'VERIFIED', 'HANDED_OFF', 'CANCELLED')),
    last_failure_code TEXT,
    verification_job_id UUID NOT NULL REFERENCES jobs (id),
    resolved_at TIMESTAMPTZ,
    CONSTRAINT agent_turn_corroborations_prompt_check CHECK (
        (prompt_outcome IS NOT NULL AND prompt_error IS NULL AND jsonb_typeof(prompt_outcome) = 'object')
        OR (prompt_outcome IS NULL AND prompt_error IS NOT NULL)
    ),
    CONSTRAINT agent_turn_corroborations_resolution_check CHECK (
        (state = 'PENDING' AND resolved_at IS NULL) OR (state <> 'PENDING' AND resolved_at IS NOT NULL)
    ),
    CONSTRAINT agent_turn_corroborations_turn_fk
        FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES agent_turns (id, execution_epoch),
    CONSTRAINT agent_turn_corroborations_attempt_fk
        FOREIGN KEY (workflow_attempt_id, workflow_id) REFERENCES workflow_attempts (id, workflow_id)
);

ALTER TABLE agent_turn_settlements
    DROP CONSTRAINT agent_turn_settlements_authority_shape_check,
    ADD CONSTRAINT agent_turn_settlements_authority_shape_check CHECK (
        (authority = 'LIVE' AND recovery_job_id IS NULL
            AND recovery_job_kind IS NULL AND recovery_job_attempt_number IS NULL
            AND recovery_job_lease_owner IS NULL AND recovery_job_lease_token IS NULL)
        OR (authority = 'RECOVERY' AND recovery_job_id IS NOT NULL
            AND recovery_job_kind IN ('STOP_STALE_RUNTIME', 'RECONCILE_AGENT_TURN_MUTATIONS', 'VERIFY_TERMINAL_INTENT')
            AND recovery_job_attempt_number IS NOT NULL
            AND recovery_job_lease_owner IS NOT NULL AND recovery_job_lease_token IS NOT NULL)
    );

ALTER TABLE change_proposal_reviews
    ADD COLUMN workflow_internal_event_id UUID REFERENCES workflow_internal_events (id),
    DROP CONSTRAINT change_proposal_reviews_provenance_check,
    ADD CONSTRAINT change_proposal_reviews_provenance_check CHECK (
        ((normalized_event_id IS NOT NULL)::INTEGER
          + (agent_turn_settlement_id IS NOT NULL)::INTEGER
          + (workflow_internal_event_id IS NOT NULL)::INTEGER) = 1
    );

ALTER TABLE workflow_internal_events
    DROP CONSTRAINT workflow_internal_events_kind_check,
    ADD CONSTRAINT workflow_internal_events_kind_check CHECK (kind IN (
        'CLOSURE_SETTLED', 'ASSIGNMENTS_COLLECTED',
        'TERMINAL_INTENT_REVALIDATED', 'TERMINAL_REVALIDATION_FAILED'
    ));

CREATE UNIQUE INDEX agent_turn_corroborations_pending_workflow_idx
    ON agent_turn_corroborations (workflow_id) WHERE state = 'PENDING';
