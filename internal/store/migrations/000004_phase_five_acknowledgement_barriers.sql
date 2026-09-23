CREATE TABLE workflow_closure_barriers (
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    closure_id TEXT NOT NULL CHECK (closure_id <> ''),
    workflow_revision BIGINT NOT NULL CHECK (workflow_revision > 0),
    source_turn_id UUID,
    source_session_id UUID,
    source_attempt_id UUID,
    source_execution_epoch BIGINT,
    source_control_revision BIGINT,
    mutation_admission_closed_at TIMESTAMPTZ NOT NULL,
    runtime_stopped_at TIMESTAMPTZ,
    settled_at TIMESTAMPTZ,
    stop_job_id UUID REFERENCES jobs (id),
    settlement_job_id UUID NOT NULL REFERENCES jobs (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (workflow_id, closure_id),
    UNIQUE (settlement_job_id),
    UNIQUE (stop_job_id),
    CONSTRAINT workflow_closure_barriers_source_check CHECK (
        (source_turn_id IS NULL AND source_session_id IS NULL AND source_attempt_id IS NULL
            AND source_execution_epoch IS NULL AND source_control_revision IS NULL
            AND stop_job_id IS NULL AND runtime_stopped_at IS NULL)
        OR (source_turn_id IS NOT NULL AND source_session_id IS NOT NULL AND source_attempt_id IS NOT NULL
            AND source_execution_epoch > 0 AND source_control_revision > 0 AND stop_job_id IS NOT NULL)
    ),
    CONSTRAINT workflow_closure_barriers_turn_fk
        FOREIGN KEY (source_turn_id, source_session_id, source_execution_epoch)
        REFERENCES agent_turns (id, agent_session_id, execution_epoch),
    CONSTRAINT workflow_closure_barriers_attempt_fk
        FOREIGN KEY (source_attempt_id, workflow_id)
        REFERENCES workflow_attempts (id, workflow_id)
);

CREATE INDEX workflow_closure_barriers_unsettled_idx
    ON workflow_closure_barriers (workflow_id, workflow_revision)
    WHERE settled_at IS NULL;
