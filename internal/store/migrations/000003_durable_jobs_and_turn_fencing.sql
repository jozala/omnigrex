ALTER TABLE jobs
    ADD COLUMN enqueue_delay_microseconds BIGINT NOT NULL DEFAULT 0
        CHECK (enqueue_delay_microseconds >= 0),
    ADD COLUMN result JSONB,
    ADD CONSTRAINT jobs_result_check CHECK (
        result IS NULL OR status = 'SUCCEEDED'
    ),
    ADD CONSTRAINT jobs_scope_hierarchy_check CHECK (
        (workflow_attempt_id IS NULL OR workflow_id IS NOT NULL)
        AND (agent_assignment_id IS NULL OR workflow_id IS NOT NULL)
        AND (agent_session_id IS NULL OR agent_assignment_id IS NOT NULL)
        AND (agent_turn_id IS NULL OR agent_session_id IS NOT NULL)
    ) NOT VALID;

ALTER TABLE normalized_events
    DROP CONSTRAINT normalized_events_status_check,
    ADD COLUMN workflow_id UUID REFERENCES workflows (id),
    ADD COLUMN disposition TEXT,
    ADD COLUMN reason TEXT,
    ADD COLUMN applied_revision BIGINT CHECK (applied_revision >= 0),
    ADD COLUMN deferred_for_turn_id UUID REFERENCES agent_turns (id),
    ADD COLUMN processed_at TIMESTAMPTZ,
    ADD CONSTRAINT normalized_events_status_check CHECK (
        status IN ('PENDING', 'DEFERRED', 'COMPLETED')
    ),
    ADD CONSTRAINT normalized_events_outcome_check CHECK (
        (status = 'PENDING' AND workflow_id IS NULL AND disposition IS NULL AND reason IS NULL
            AND applied_revision IS NULL AND deferred_for_turn_id IS NULL AND processed_at IS NULL)
        OR (status = 'DEFERRED' AND disposition = 'DEFERRED' AND reason IS NOT NULL
            AND applied_revision IS NOT NULL AND processed_at IS NOT NULL)
        OR (status = 'COMPLETED' AND disposition IN ('APPLIED', 'DUPLICATE', 'STALE', 'UNRELATED', 'ILLEGAL')
            AND reason IS NOT NULL AND applied_revision IS NOT NULL AND deferred_for_turn_id IS NULL AND processed_at IS NOT NULL)
    );

DROP INDEX normalized_events_pending_idx;
CREATE INDEX normalized_events_pending_idx
    ON normalized_events (created_at, delivery_id)
    WHERE status = 'PENDING';
CREATE INDEX normalized_events_deferred_workflow_idx
    ON normalized_events (workflow_id, deferred_for_turn_id, created_at)
    WHERE status = 'DEFERRED';

ALTER TABLE workflows
    ADD COLUMN resume_role TEXT CHECK (resume_role IN ('DEVELOPER', 'REVIEWER')),
    ADD COLUMN desired_assignment_status TEXT NOT NULL DEFAULT 'ACTIVE'
        CHECK (desired_assignment_status IN ('ACTIVE', 'WAITING_FOR_HUMAN', 'COMPLETED')),
    ADD COLUMN desired_runtime_state TEXT NOT NULL DEFAULT 'ACTIVE'
        CHECK (desired_runtime_state IN ('ACTIVE', 'RETAINED', 'COLLECTED')),
    ADD COLUMN closure_id TEXT,
    ADD COLUMN closure_deadline TIMESTAMPTZ,
    ADD COLUMN closure_retention_token TEXT,
    ADD COLUMN closure_reopen_requested BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN retention_deadline TIMESTAMPTZ,
    ADD COLUMN retention_token TEXT,
    ADD CONSTRAINT workflows_closure_identity_check CHECK (
        (closure_id IS NULL AND closure_deadline IS NULL AND closure_retention_token IS NULL AND NOT closure_reopen_requested)
        OR (closure_id IS NOT NULL AND closure_id <> '' AND closure_deadline IS NOT NULL
            AND closure_retention_token IS NOT NULL AND closure_retention_token <> '')
    ),
    ADD CONSTRAINT workflows_retention_identity_check CHECK (
        (retention_deadline IS NULL AND retention_token IS NULL)
        OR (retention_deadline IS NOT NULL AND retention_token IS NOT NULL AND retention_token <> '')
    );

ALTER TABLE agent_turns
    ADD COLUMN purpose TEXT CHECK (purpose IN (
        'INITIAL_DEVELOPMENT', 'REVIEW', 'REQUESTED_CHANGES', 'RETRY',
        'SYNCHRONIZATION', 'REACTIVATION'
    )),
    ADD COLUMN change_proposal_id UUID REFERENCES change_proposals (id),
    ADD COLUMN expected_head_sha TEXT,
    ADD CONSTRAINT agent_turns_expected_proposal_check CHECK (
        (change_proposal_id IS NULL AND COALESCE(expected_head_sha, '') = '')
        OR (change_proposal_id IS NOT NULL AND expected_head_sha IS NOT NULL AND expected_head_sha <> '')
    );

CREATE TABLE change_proposal_reviews (
    repository_id BIGINT NOT NULL CHECK (repository_id > 0),
    review_id BIGINT NOT NULL CHECK (review_id > 0),
    review_node_id TEXT NOT NULL CHECK (review_node_id <> ''),
    change_proposal_id UUID NOT NULL REFERENCES change_proposals (id),
    actor_id BIGINT NOT NULL CHECK (actor_id > 0),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    accepted BOOLEAN NOT NULL,
    normalized_event_id UUID NOT NULL REFERENCES normalized_events (delivery_id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (repository_id, review_id),
    UNIQUE (repository_id, review_node_id)
);

CREATE INDEX change_proposal_reviews_proposal_idx
    ON change_proposal_reviews (change_proposal_id, created_at);

ALTER TABLE jobs
    ADD COLUMN normalized_event_id UUID REFERENCES normalized_events (delivery_id),
    ADD COLUMN action_key TEXT,
    ADD CONSTRAINT jobs_action_provenance_check CHECK (
        (normalized_event_id IS NULL AND action_key IS NULL)
        OR (normalized_event_id IS NOT NULL AND action_key IS NOT NULL AND action_key <> '')
    );

CREATE UNIQUE INDEX jobs_normalized_event_action_idx
    ON jobs (normalized_event_id, action_key)
    WHERE normalized_event_id IS NOT NULL;

CREATE UNIQUE INDEX workflow_attempts_id_workflow_idx
    ON workflow_attempts (id, workflow_id);
CREATE UNIQUE INDEX agent_assignments_id_workflow_idx
    ON agent_assignments (id, workflow_id);
CREATE UNIQUE INDEX agent_sessions_id_assignment_idx
    ON agent_sessions (id, agent_assignment_id);
CREATE UNIQUE INDEX agent_turns_id_session_epoch_idx
    ON agent_turns (id, agent_session_id, execution_epoch);

ALTER TABLE jobs
    ADD CONSTRAINT jobs_workflow_attempt_scope_fk
        FOREIGN KEY (workflow_attempt_id, workflow_id)
        REFERENCES workflow_attempts (id, workflow_id) NOT VALID,
    ADD CONSTRAINT jobs_assignment_scope_fk
        FOREIGN KEY (agent_assignment_id, workflow_id)
        REFERENCES agent_assignments (id, workflow_id) NOT VALID,
    ADD CONSTRAINT jobs_session_scope_fk
        FOREIGN KEY (agent_session_id, agent_assignment_id)
        REFERENCES agent_sessions (id, agent_assignment_id) NOT VALID,
    ADD CONSTRAINT jobs_turn_scope_fk
        FOREIGN KEY (agent_turn_id, agent_session_id, execution_epoch)
        REFERENCES agent_turns (id, agent_session_id, execution_epoch) NOT VALID;

CREATE UNIQUE INDEX jobs_agent_turn_execution_idx
    ON jobs (agent_turn_id, execution_epoch)
    WHERE kind = 'RUN_AGENT_TURN';

CREATE UNIQUE INDEX jobs_agent_turn_recovery_kind_idx
    ON jobs (agent_turn_id, execution_epoch, kind)
    WHERE kind IN ('STOP_STALE_RUNTIME', 'RECONCILE_AGENT_TURN_MUTATIONS');

CREATE TABLE job_attempts (
    job_id UUID NOT NULL REFERENCES jobs (id),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    lease_owner TEXT NOT NULL CHECK (lease_owner <> ''),
    lease_token UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('LEASED', 'SUCCEEDED', 'FAILED', 'EXPIRED')),
    leased_at TIMESTAMPTZ NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    heartbeat_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    retryable BOOLEAN,
    result JSONB,
    last_error TEXT,
    PRIMARY KEY (job_id, attempt_number),
    UNIQUE (lease_token),
    CONSTRAINT job_attempts_finished_check CHECK (
        (status = 'LEASED' AND finished_at IS NULL)
        OR (status <> 'LEASED' AND finished_at IS NOT NULL)
    ),
    CONSTRAINT job_attempts_outcome_check CHECK (
        (status = 'LEASED' AND retryable IS NULL AND result IS NULL AND last_error IS NULL)
        OR (status = 'SUCCEEDED' AND retryable IS NULL AND result IS NOT NULL AND last_error IS NULL)
        OR (status IN ('FAILED', 'EXPIRED') AND retryable IS NOT NULL AND result IS NULL AND last_error IS NOT NULL)
    )
);

CREATE INDEX job_attempts_live_lease_idx
    ON job_attempts (lease_expires_at, job_id)
    WHERE status = 'LEASED';

ALTER TABLE agent_sessions
    ADD COLUMN next_turn_number BIGINT NOT NULL DEFAULT 1 CHECK (next_turn_number > 0);

UPDATE agent_sessions AS session
SET next_turn_number = existing.next_turn_number,
    next_execution_epoch = GREATEST(session.next_execution_epoch, existing.next_execution_epoch)
FROM (
    SELECT agent_session_id,
           COALESCE(MAX(turn_number), 0) + 1 AS next_turn_number,
           COALESCE(MAX(execution_epoch), 0) + 1 AS next_execution_epoch
    FROM agent_turns
    GROUP BY agent_session_id
) AS existing
WHERE session.id = existing.agent_session_id;

ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_status_check,
    ADD CONSTRAINT agent_turns_status_check CHECK (status IN (
        'QUEUED', 'STARTING', 'RUNNING', 'CANCELLING', 'SETTLING', 'RECONCILING',
        'SUCCEEDED', 'FAILED', 'INTERRUPTED', 'TIMED_OUT'
    )),
    ADD COLUMN next_invocation_number BIGINT NOT NULL DEFAULT 1
        CHECK (next_invocation_number > 0);

UPDATE agent_turns AS turn
SET next_invocation_number = existing.next_invocation_number
FROM (
    SELECT agent_turn_id, COALESCE(MAX(invocation_number), 0) + 1 AS next_invocation_number
    FROM tool_invocations
    GROUP BY agent_turn_id
) AS existing
WHERE turn.id = existing.agent_turn_id;

ALTER TABLE tool_invocations
    ADD COLUMN operation_id TEXT;

CREATE UNIQUE INDEX tool_invocations_operation_id_idx
    ON tool_invocations (operation_id)
    WHERE operation_id IS NOT NULL;

CREATE TABLE agent_turn_slots (
    agent_turn_id UUID PRIMARY KEY,
    agent_session_id UUID NOT NULL REFERENCES agent_sessions (id),
    execution_epoch BIGINT NOT NULL CHECK (execution_epoch > 0),
    control_revision BIGINT NOT NULL CHECK (control_revision > 0),
    owner_id TEXT NOT NULL CHECK (owner_id <> ''),
    owner_token UUID NOT NULL UNIQUE,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_turn_slots_turn_epoch_fk
        FOREIGN KEY (agent_turn_id, execution_epoch)
        REFERENCES agent_turns (id, execution_epoch)
);

CREATE INDEX agent_turn_slots_live_idx
    ON agent_turn_slots (lease_expires_at, agent_turn_id);

ALTER TABLE agent_turns
    ADD COLUMN recovery_started_at TIMESTAMPTZ,
    ADD COLUMN runtime_stop_required BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN runtime_stopped_at TIMESTAMPTZ,
    ADD COLUMN recovery_settled_at TIMESTAMPTZ,
    ADD COLUMN stop_runtime_job_id UUID REFERENCES jobs (id),
    ADD COLUMN reconcile_mutations_job_id UUID REFERENCES jobs (id),
    ADD CONSTRAINT agent_turns_recovery_barrier_check CHECK (
        (recovery_started_at IS NULL AND NOT runtime_stop_required
            AND runtime_stopped_at IS NULL AND recovery_settled_at IS NULL
            AND stop_runtime_job_id IS NULL AND reconcile_mutations_job_id IS NULL)
        OR (recovery_started_at IS NOT NULL AND runtime_stop_required
            AND stop_runtime_job_id IS NOT NULL
            AND (recovery_settled_at IS NULL OR runtime_stopped_at IS NOT NULL))
    );

CREATE INDEX agent_turns_unsettled_recovery_idx
    ON agent_turns (agent_session_id, recovery_started_at)
    WHERE recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL;

CREATE TABLE job_normalized_events (
    job_id UUID NOT NULL REFERENCES jobs (id),
    normalized_event_id UUID NOT NULL UNIQUE REFERENCES normalized_events (delivery_id),
    PRIMARY KEY (job_id, normalized_event_id)
);
