CREATE UNIQUE INDEX tool_invocations_exact_identity_idx
    ON tool_invocations (id, agent_turn_id, execution_epoch, operation_lineage_id);

CREATE TABLE tool_invocation_replays (
    agent_turn_id UUID NOT NULL,
    execution_epoch BIGINT NOT NULL CHECK (execution_epoch > 0),
    operation_lineage_id UUID NOT NULL,
    invocation_number BIGINT NOT NULL CHECK (invocation_number > 0),
    source_tool_invocation_id UUID NOT NULL,
    source_agent_turn_id UUID NOT NULL,
    source_execution_epoch BIGINT NOT NULL CHECK (source_execution_epoch > 0),
    replayed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT tool_invocation_replays_pkey
        PRIMARY KEY (agent_turn_id, source_tool_invocation_id),
    CONSTRAINT tool_invocation_replays_turn_invocation_key
        UNIQUE (agent_turn_id, invocation_number),
    CONSTRAINT tool_invocation_replays_distinct_turn_check
        CHECK (source_agent_turn_id <> agent_turn_id),
    CONSTRAINT tool_invocation_replays_turn_epoch_lineage_fk
        FOREIGN KEY (agent_turn_id, execution_epoch, operation_lineage_id)
        REFERENCES agent_turns (id, execution_epoch, operation_lineage_id),
    CONSTRAINT tool_invocation_replays_source_exact_fk
        FOREIGN KEY (
            source_tool_invocation_id, source_agent_turn_id,
            source_execution_epoch, operation_lineage_id
        )
        REFERENCES tool_invocations (
            id, agent_turn_id, execution_epoch, operation_lineage_id
        )
);

ALTER TABLE agent_turns
    ADD COLUMN recovery_original_owner_id TEXT,
    ADD COLUMN recovery_original_owner_token_sha256 BYTEA,
    ADD COLUMN recovery_continuation TEXT CHECK (recovery_continuation IN (
        'PENDING_INFRASTRUCTURE_FAILURE',
        'INFRASTRUCTURE_FAILURE_APPLIED',
        'MUTATION_RECONCILIATION_HANDOFF_APPLIED',
        'MIGRATION_HANDOFF_APPLIED'
    ));

UPDATE agent_turns AS turn
SET recovery_original_owner_id = NULLIF(stop.payload->>'owner_id', ''),
    recovery_original_owner_token_sha256 = CASE
        WHEN COALESCE(stop.payload->>'owner_token_sha256', '') ~ '^[0-9a-f]{64}$'
            THEN decode(stop.payload->>'owner_token_sha256', 'hex')
        ELSE NULL
    END,
    recovery_continuation = CASE
        WHEN workflow.human_handoff_reason = 'agent_turn_mutation_reconciliation_exhausted'
            THEN 'MUTATION_RECONCILIATION_HANDOFF_APPLIED'
        WHEN NULLIF(stop.payload->>'owner_id', '') IS NOT NULL
            AND COALESCE(stop.payload->>'owner_token_sha256', '') ~ '^[0-9a-f]{64}$'
            THEN 'PENDING_INFRASTRUCTURE_FAILURE'
        ELSE 'MIGRATION_HANDOFF_APPLIED'
    END
FROM jobs AS stop, workflows AS workflow
WHERE turn.recovery_started_at IS NOT NULL
  AND stop.id = turn.stop_runtime_job_id
  AND workflow.id = turn.workflow_id;

-- Pre-Phase 8 normal recovery could have marked its barriers settled without
-- continuing the Workflow. Reopen its stop job under a fresh lease attempt so
-- final acknowledgement executes the new atomic settlement boundary.
UPDATE jobs AS stop
SET status = 'AVAILABLE', available_at = clock_timestamp(),
    max_attempts = GREATEST(stop.max_attempts, stop.attempt_count + 1),
    lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, completed_at = NULL,
    result = NULL, last_error = NULL, updated_at = clock_timestamp()
FROM agent_turns AS turn
WHERE turn.stop_runtime_job_id = stop.id
  AND turn.recovery_settled_at IS NOT NULL
  AND turn.recovery_continuation = 'PENDING_INFRASTRUCTURE_FAILURE'
  AND stop.status = 'SUCCEEDED';

UPDATE agent_turns
SET recovery_settled_at = NULL
WHERE recovery_settled_at IS NOT NULL
  AND recovery_continuation = 'PENDING_INFRASTRUCTURE_FAILURE';

ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_recovery_barrier_check,
    ADD CONSTRAINT agent_turns_recovery_barrier_check CHECK (
        (recovery_started_at IS NULL AND NOT runtime_stop_required
            AND runtime_stopped_at IS NULL AND recovery_settled_at IS NULL
            AND stop_runtime_job_id IS NULL AND reconcile_mutations_job_id IS NULL
            AND recovery_original_owner_id IS NULL
            AND recovery_original_owner_token_sha256 IS NULL
            AND recovery_continuation IS NULL)
        OR (recovery_started_at IS NOT NULL AND runtime_stop_required
            AND stop_runtime_job_id IS NOT NULL
            AND recovery_continuation IS NOT NULL
            AND (recovery_settled_at IS NULL OR runtime_stopped_at IS NOT NULL)
            AND (
                recovery_continuation = 'MIGRATION_HANDOFF_APPLIED'
                OR (recovery_original_owner_id IS NOT NULL
                    AND recovery_original_owner_id <> ''
                    AND octet_length(recovery_original_owner_token_sha256) = 32)
            ))
    );

CREATE TABLE agent_turn_settlements (
    id UUID PRIMARY KEY,
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    workflow_attempt_id UUID NOT NULL,
    agent_assignment_id UUID NOT NULL,
    agent_session_id UUID NOT NULL,
    agent_turn_id UUID NOT NULL,
    execution_epoch BIGINT NOT NULL CHECK (execution_epoch > 0),
    control_revision BIGINT NOT NULL CHECK (control_revision > 0),
    authority TEXT NOT NULL CHECK (authority IN ('LIVE', 'RECOVERY')),
    execution_job_id UUID NOT NULL REFERENCES jobs (id),
    job_attempt_number INTEGER NOT NULL CHECK (job_attempt_number > 0),
    job_lease_owner TEXT NOT NULL CHECK (job_lease_owner <> ''),
    job_lease_token UUID NOT NULL,
    owner_id TEXT NOT NULL CHECK (owner_id <> ''),
    owner_token_sha256 BYTEA NOT NULL CHECK (octet_length(owner_token_sha256) = 32),
    recovery_job_id UUID REFERENCES jobs (id),
    recovery_job_kind TEXT,
    recovery_job_attempt_number INTEGER CHECK (recovery_job_attempt_number > 0),
    recovery_job_lease_owner TEXT CHECK (recovery_job_lease_owner <> ''),
    recovery_job_lease_token UUID,
    observation JSONB NOT NULL CHECK (jsonb_typeof(observation) = 'object'),
    observation_sha256 BYTEA NOT NULL CHECK (octet_length(observation_sha256) = 32),
    workflow_outcome TEXT NOT NULL CHECK (workflow_outcome IN (
        'CHANGE_PROPOSAL_READY', 'CHANGES_REQUESTED', 'APPROVED', 'BLOCKED',
        'INFRASTRUCTURE_FAILED'
    )),
    terminal_turn_status TEXT NOT NULL CHECK (terminal_turn_status IN (
        'SUCCEEDED', 'FAILED', 'INTERRUPTED', 'TIMED_OUT'
    )),
    terminal_turn_outcome JSONB,
    terminal_last_error TEXT,
    pending_event_count INTEGER NOT NULL CHECK (pending_event_count >= 0),
    latest_observed_head_sha TEXT,
    disposition TEXT NOT NULL CHECK (disposition = 'APPLIED'),
    reason TEXT NOT NULL CHECK (reason <> ''),
    workflow_state TEXT NOT NULL CHECK (workflow_state <> ''),
    workflow_revision BIGINT NOT NULL CHECK (workflow_revision > 0),
    change_proposal_id UUID REFERENCES change_proposals (id),
    successor_job_id UUID,
    reconciliation_job_id UUID,
    result JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    settled_at TIMESTAMPTZ,
    CONSTRAINT agent_turn_settlements_turn_epoch_key UNIQUE (agent_turn_id, execution_epoch),
    CONSTRAINT agent_turn_settlements_execution_attempt_key
        UNIQUE (execution_job_id, job_attempt_number),
    CONSTRAINT agent_turn_settlements_attempt_scope_fk
        FOREIGN KEY (workflow_attempt_id, workflow_id)
        REFERENCES workflow_attempts (id, workflow_id),
    CONSTRAINT agent_turn_settlements_assignment_scope_fk
        FOREIGN KEY (agent_assignment_id, workflow_id)
        REFERENCES agent_assignments (id, workflow_id),
    CONSTRAINT agent_turn_settlements_session_scope_fk
        FOREIGN KEY (agent_session_id, agent_assignment_id)
        REFERENCES agent_sessions (id, agent_assignment_id),
    CONSTRAINT agent_turn_settlements_turn_scope_fk
        FOREIGN KEY (agent_turn_id, agent_session_id, execution_epoch)
        REFERENCES agent_turns (id, agent_session_id, execution_epoch),
    CONSTRAINT agent_turn_settlements_job_attempt_fk
        FOREIGN KEY (execution_job_id, job_attempt_number)
        REFERENCES job_attempts (job_id, attempt_number),
    CONSTRAINT agent_turn_settlements_recovery_job_attempt_fk
        FOREIGN KEY (recovery_job_id, recovery_job_attempt_number)
        REFERENCES job_attempts (job_id, attempt_number),
    CONSTRAINT agent_turn_settlements_authority_shape_check CHECK (
        (authority = 'LIVE' AND recovery_job_id IS NULL
            AND recovery_job_kind IS NULL
            AND recovery_job_attempt_number IS NULL
            AND recovery_job_lease_owner IS NULL
            AND recovery_job_lease_token IS NULL)
        OR (authority = 'RECOVERY' AND recovery_job_id IS NOT NULL
            AND recovery_job_kind IN ('STOP_STALE_RUNTIME', 'RECONCILE_AGENT_TURN_MUTATIONS')
            AND recovery_job_attempt_number IS NOT NULL
            AND recovery_job_lease_owner IS NOT NULL
            AND recovery_job_lease_token IS NOT NULL)
    ),
    CONSTRAINT agent_turn_settlements_completion_check CHECK (
        (settled_at IS NULL AND result IS NULL
            AND successor_job_id IS NULL AND reconciliation_job_id IS NULL)
        OR (settled_at IS NOT NULL AND result IS NOT NULL
            AND NOT (successor_job_id IS NOT NULL AND reconciliation_job_id IS NOT NULL))
    )
);

CREATE UNIQUE INDEX agent_turn_settlements_job_lease_token_idx
    ON agent_turn_settlements (job_lease_token);

CREATE UNIQUE INDEX agent_turn_settlements_recovery_job_attempt_idx
    ON agent_turn_settlements (recovery_job_id, recovery_job_attempt_number)
    WHERE recovery_job_id IS NOT NULL;

CREATE UNIQUE INDEX agent_turn_settlements_recovery_job_lease_token_idx
    ON agent_turn_settlements (recovery_job_lease_token)
    WHERE recovery_job_lease_token IS NOT NULL;

ALTER TABLE agent_turns
    ADD COLUMN recovery_settlement_id UUID REFERENCES agent_turn_settlements (id),
    ADD CONSTRAINT agent_turns_recovery_continuation_shape_check CHECK (
        (recovery_continuation = 'INFRASTRUCTURE_FAILURE_APPLIED'
            AND recovery_settlement_id IS NOT NULL)
        OR (recovery_continuation <> 'INFRASTRUCTURE_FAILURE_APPLIED'
            AND recovery_settlement_id IS NULL)
        OR recovery_continuation IS NULL
    );

ALTER TABLE jobs
    ADD COLUMN agent_turn_settlement_id UUID REFERENCES agent_turn_settlements (id),
    DROP CONSTRAINT jobs_action_provenance_check,
    ADD CONSTRAINT jobs_action_provenance_check CHECK (
        (normalized_event_id IS NULL AND agent_turn_settlement_id IS NULL
            AND action_key IS NULL)
        OR (((normalized_event_id IS NOT NULL)::INTEGER
                + (agent_turn_settlement_id IS NOT NULL)::INTEGER) = 1
            AND action_key IS NOT NULL AND action_key <> '')
    );

CREATE UNIQUE INDEX jobs_agent_turn_settlement_action_idx
    ON jobs (agent_turn_settlement_id, action_key)
    WHERE agent_turn_settlement_id IS NOT NULL;

ALTER TABLE agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_successor_job_fk
        FOREIGN KEY (successor_job_id) REFERENCES jobs (id),
    ADD CONSTRAINT agent_turn_settlements_reconciliation_job_fk
        FOREIGN KEY (reconciliation_job_id) REFERENCES jobs (id);

ALTER TABLE change_proposal_reviews
    ALTER COLUMN normalized_event_id DROP NOT NULL,
    ADD COLUMN agent_turn_settlement_id UUID REFERENCES agent_turn_settlements (id),
    ADD CONSTRAINT change_proposal_reviews_provenance_check CHECK (
        ((normalized_event_id IS NOT NULL)::INTEGER
            + (agent_turn_settlement_id IS NOT NULL)::INTEGER) = 1
    );

CREATE INDEX change_proposal_reviews_settlement_idx
    ON change_proposal_reviews (agent_turn_settlement_id)
    WHERE agent_turn_settlement_id IS NOT NULL;
