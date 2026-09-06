-- Legacy bootstrap attempts used a retry limit of two, while the reducer contract
-- has always persisted new attempts with a limit of one.
UPDATE workflow_attempts
SET infrastructure_failures = LEAST(infrastructure_failures, 1),
    infrastructure_failure_limit = 1,
    updated_at = clock_timestamp()
WHERE active AND infrastructure_failure_limit <> 1;

CREATE TABLE workflow_internal_events (
    id UUID PRIMARY KEY,
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    kind TEXT NOT NULL CHECK (kind IN ('CLOSURE_SETTLED', 'ASSIGNMENTS_COLLECTED')),
    source_job_id UUID NOT NULL REFERENCES jobs (id),
    source_attempt_number INTEGER NOT NULL CHECK (source_attempt_number > 0),
    source_lease_token UUID NOT NULL,
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    disposition TEXT CHECK (disposition = 'APPLIED'),
    reason TEXT,
    workflow_revision BIGINT CHECK (workflow_revision > 0),
    observed_at TIMESTAMPTZ NOT NULL,
    applied_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (source_job_id),
    UNIQUE (source_lease_token),
    CONSTRAINT workflow_internal_events_source_attempt_fk
        FOREIGN KEY (source_job_id, source_attempt_number)
        REFERENCES job_attempts (job_id, attempt_number),
    CONSTRAINT workflow_internal_events_outcome_check CHECK (
        (applied_at IS NULL AND disposition IS NULL AND reason IS NULL
            AND workflow_revision IS NULL)
        OR (applied_at IS NOT NULL AND disposition = 'APPLIED' AND reason IS NOT NULL
            AND reason <> '' AND workflow_revision IS NOT NULL)
    )
);

ALTER TABLE jobs
    ADD COLUMN workflow_internal_event_id UUID REFERENCES workflow_internal_events (id),
    DROP CONSTRAINT jobs_action_provenance_check,
    ADD CONSTRAINT jobs_action_provenance_check CHECK (
        (normalized_event_id IS NULL AND agent_turn_settlement_id IS NULL
            AND workflow_internal_event_id IS NULL AND action_key IS NULL)
        OR (((normalized_event_id IS NOT NULL)::INTEGER
                + (agent_turn_settlement_id IS NOT NULL)::INTEGER
                + (workflow_internal_event_id IS NOT NULL)::INTEGER) = 1
            AND action_key IS NOT NULL AND action_key <> '')
    );

CREATE UNIQUE INDEX jobs_workflow_internal_event_action_idx
    ON jobs (workflow_internal_event_id, action_key)
    WHERE workflow_internal_event_id IS NOT NULL;

ALTER TABLE workflow_closure_barriers
    ADD COLUMN internal_event_id UUID UNIQUE REFERENCES workflow_internal_events (id),
    ADD COLUMN applied_revision BIGINT CHECK (applied_revision > 0);

-- A prepared Agent Session can be retained before an ACP runtime ever starts.
ALTER TABLE agent_sessions
    DROP CONSTRAINT agent_sessions_active_identifier_check,
    ADD CONSTRAINT agent_sessions_active_identifier_check
        CHECK (status <> 'ACTIVE' OR acp_session_id IS NOT NULL);

CREATE TABLE assignment_retention_generations (
    id UUID PRIMARY KEY,
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    retention_token TEXT NOT NULL CHECK (retention_token <> ''),
    retention_deadline TIMESTAMPTZ NOT NULL,
    workflow_revision BIGINT NOT NULL CHECK (workflow_revision > 0),
    status TEXT NOT NULL CHECK (status IN ('SCHEDULED', 'COLLECTING', 'COLLECTED', 'CANCELLED')),
    collection_job_id UUID NOT NULL UNIQUE REFERENCES jobs (id) DEFERRABLE INITIALLY DEFERRED,
    authorized_attempt_number INTEGER CHECK (authorized_attempt_number > 0),
    authorized_lease_owner TEXT CHECK (authorized_lease_owner <> ''),
    authorized_lease_token UUID,
    authorized_at TIMESTAMPTZ,
    collected_event_id UUID UNIQUE REFERENCES workflow_internal_events (id),
    collected_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (workflow_id, retention_token),
    CONSTRAINT assignment_retention_generations_authorization_check CHECK (
        (status IN ('SCHEDULED', 'CANCELLED') AND authorized_attempt_number IS NULL
            AND authorized_lease_owner IS NULL AND authorized_lease_token IS NULL
            AND authorized_at IS NULL)
        OR (status IN ('COLLECTING', 'COLLECTED') AND authorized_attempt_number IS NOT NULL
            AND authorized_lease_owner IS NOT NULL AND authorized_lease_token IS NOT NULL
            AND authorized_at IS NOT NULL)
    ),
    CONSTRAINT assignment_retention_generations_outcome_check CHECK (
        (status = 'SCHEDULED' AND collected_event_id IS NULL AND collected_at IS NULL
            AND cancelled_at IS NULL)
        OR (status = 'COLLECTING' AND collected_event_id IS NULL AND collected_at IS NULL
            AND cancelled_at IS NULL)
        OR (status = 'COLLECTED' AND collected_event_id IS NOT NULL
            AND collected_at IS NOT NULL AND cancelled_at IS NULL)
        OR (status = 'CANCELLED' AND collected_event_id IS NULL
            AND collected_at IS NULL AND cancelled_at IS NOT NULL)
    )
);

ALTER TABLE jobs
    ADD COLUMN retention_generation_id UUID
        REFERENCES assignment_retention_generations (id) DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX jobs_retention_generation_idx
    ON jobs (retention_generation_id)
    WHERE retention_generation_id IS NOT NULL;

CREATE INDEX assignment_retention_generations_due_idx
    ON assignment_retention_generations (retention_deadline, workflow_id)
    WHERE status = 'SCHEDULED';

CREATE TABLE assignment_retention_targets (
    retention_generation_id UUID NOT NULL REFERENCES assignment_retention_generations (id),
    assignment_id UUID NOT NULL,
    session_id UUID,
    runtime_state_path TEXT NOT NULL CHECK (runtime_state_path <> ''),
    runtime_image_digest TEXT NOT NULL CHECK (runtime_image_digest <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT assignment_retention_targets_identity_key
        UNIQUE NULLS NOT DISTINCT (retention_generation_id, assignment_id, session_id),
    CONSTRAINT assignment_retention_targets_assignment_fk
        FOREIGN KEY (assignment_id)
        REFERENCES agent_assignments (id),
    CONSTRAINT assignment_retention_targets_session_fk
        FOREIGN KEY (session_id) REFERENCES agent_sessions (id)
);

CREATE INDEX assignment_retention_targets_generation_idx
    ON assignment_retention_targets (retention_generation_id, runtime_state_path, session_id);

-- Phase 8 could durably settle the closure barrier and its Job before applying
-- the Workflow transition. Those Jobs cannot be reclaimed, so materialize the
-- reducer-owned transition with deterministic internal-event provenance.
CREATE TEMPORARY TABLE phase_nine_settled_closure_reconciliation ON COMMIT DROP AS
SELECT barrier.workflow_id,
       barrier.closure_id,
       barrier.workflow_revision,
       workflow.state_revision + 1 AS applied_revision,
       workflow.closure_deadline,
       workflow.closure_retention_token,
       workflow.closure_reopen_requested,
       barrier.source_turn_id,
       barrier.source_execution_epoch,
       barrier.source_control_revision,
       barrier.settlement_job_id,
       successful_attempt.attempt_number,
       successful_attempt.lease_token,
       barrier.settled_at,
       EXISTS (
           SELECT 1
           FROM agent_assignments AS assignment
           WHERE assignment.workflow_id = workflow.id
             AND assignment.status <> 'SUPERSEDED'
             AND assignment.state_deleted_at IS NULL
       ) AS assignments_exist,
       (md5('migration:000011:closure-event:' || workflow.id::text || ':' || barrier.closure_id))::UUID AS internal_event_id,
       (md5('migration:000011:closure-labels:' || workflow.id::text || ':' || barrier.closure_id))::UUID AS label_job_id
FROM workflow_closure_barriers AS barrier
JOIN workflows AS workflow
  ON workflow.id = barrier.workflow_id
 AND workflow.status = 'CLOSING'
 AND workflow.closure_id = barrier.closure_id
JOIN jobs AS settlement_job
  ON settlement_job.id = barrier.settlement_job_id
 AND settlement_job.kind = 'SETTLE_CLOSURE'
 AND settlement_job.status = 'SUCCEEDED'
JOIN LATERAL (
    SELECT attempt.attempt_number, attempt.lease_token
    FROM job_attempts AS attempt
    WHERE attempt.job_id = settlement_job.id
      AND attempt.attempt_number = settlement_job.attempt_count
      AND attempt.status = 'SUCCEEDED'
    ORDER BY attempt.attempt_number DESC
    LIMIT 1
) AS successful_attempt ON TRUE
WHERE barrier.settled_at IS NOT NULL
  AND barrier.internal_event_id IS NULL
  AND barrier.applied_revision IS NULL;

UPDATE job_attempts AS attempt
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = FALSE,
    last_error = 'Issue closure cancelled Agent Turn preparation'
FROM jobs AS preparation
WHERE preparation.workflow_id IN (
        SELECT workflow_id FROM phase_nine_settled_closure_reconciliation
    )
  AND preparation.kind = 'PREPARE_AGENT_TURN'
  AND preparation.status = 'LEASED'
  AND attempt.job_id = preparation.id
  AND attempt.attempt_number = preparation.attempt_count
  AND attempt.lease_token = preparation.lease_token
  AND attempt.status = 'LEASED';

UPDATE jobs
SET status = 'CANCELLED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    completed_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'Issue closure cancelled Agent Turn preparation'
WHERE workflow_id IN (
        SELECT workflow_id FROM phase_nine_settled_closure_reconciliation
    )
  AND kind = 'PREPARE_AGENT_TURN'
  AND status IN ('AVAILABLE', 'LEASED');

UPDATE workflow_attempts AS attempt
SET active = FALSE, status = 'ISSUE_CLOSED',
    completed_at = COALESCE(attempt.completed_at, reconciliation.settled_at),
    updated_at = clock_timestamp()
FROM phase_nine_settled_closure_reconciliation AS reconciliation
WHERE attempt.workflow_id = reconciliation.workflow_id
  AND attempt.active;

UPDATE change_proposals AS proposal
SET ready_for_sha = NULL, updated_at = clock_timestamp()
FROM phase_nine_settled_closure_reconciliation AS reconciliation
WHERE proposal.workflow_id = reconciliation.workflow_id
  AND proposal.active;

UPDATE agent_assignments AS assignment
SET status = 'COMPLETED',
    completed_at = COALESCE(assignment.completed_at, reconciliation.settled_at),
    retention_until = CASE
        WHEN NOT reconciliation.closure_reopen_requested
            THEN reconciliation.closure_deadline
        ELSE NULL
    END,
    updated_at = clock_timestamp()
FROM phase_nine_settled_closure_reconciliation AS reconciliation
WHERE assignment.workflow_id = reconciliation.workflow_id
  AND assignment.status <> 'SUPERSEDED'
  AND assignment.state_deleted_at IS NULL;

UPDATE agent_sessions AS session
SET status = 'RETAINED',
    retained_at = COALESCE(session.retained_at, reconciliation.settled_at),
    updated_at = clock_timestamp()
FROM agent_assignments AS assignment,
     phase_nine_settled_closure_reconciliation AS reconciliation
WHERE assignment.workflow_id = reconciliation.workflow_id
  AND assignment.id = session.agent_assignment_id
  AND assignment.status = 'COMPLETED'
  AND assignment.state_deleted_at IS NULL
  AND session.state_deleted_at IS NULL
  AND session.status IN ('CREATING', 'ACTIVE', 'RETAINED');

UPDATE workflows AS workflow
SET status = CASE
        WHEN reconciliation.closure_reopen_requested THEN 'DORMANT'
        ELSE 'CLOSED'
    END,
    state_revision = reconciliation.applied_revision,
    desired_assignment_status = 'COMPLETED',
    desired_runtime_state = CASE
        WHEN reconciliation.assignments_exist THEN 'RETAINED'
        ELSE 'COLLECTED'
    END,
    ready_for_sha = NULL,
    closure_id = NULL,
    closure_deadline = NULL,
    closure_retention_token = NULL,
    closure_reopen_requested = FALSE,
    retention_deadline = CASE
        WHEN NOT reconciliation.closure_reopen_requested
             AND reconciliation.assignments_exist
            THEN reconciliation.closure_deadline
        ELSE NULL
    END,
    retention_token = CASE
        WHEN NOT reconciliation.closure_reopen_requested
             AND reconciliation.assignments_exist
            THEN reconciliation.closure_retention_token
        ELSE NULL
    END,
    closed_at = CASE
        WHEN reconciliation.closure_reopen_requested THEN NULL
        ELSE COALESCE(workflow.closed_at, reconciliation.settled_at)
    END,
    human_handoff_reason = NULL,
    updated_at = clock_timestamp()
FROM phase_nine_settled_closure_reconciliation AS reconciliation
WHERE workflow.id = reconciliation.workflow_id;

INSERT INTO workflow_internal_events (
    id, workflow_id, kind, source_job_id, source_attempt_number,
    source_lease_token, payload, disposition, reason, workflow_revision,
    observed_at, applied_at
)
SELECT reconciliation.internal_event_id, reconciliation.workflow_id,
       'CLOSURE_SETTLED', reconciliation.settlement_job_id,
       reconciliation.attempt_number, reconciliation.lease_token,
       jsonb_build_object(
           'closure_id', reconciliation.closure_id,
           'source_turn_id', COALESCE(reconciliation.source_turn_id::text, ''),
           'source_execution_epoch', COALESCE(reconciliation.source_execution_epoch, 0),
           'source_control_revision', COALESCE(reconciliation.source_control_revision, 0),
           'assignments_exist', reconciliation.assignments_exist,
           'migration', '000011'
       ),
       'APPLIED', 'closure_settled', reconciliation.applied_revision,
       reconciliation.settled_at, reconciliation.settled_at
FROM phase_nine_settled_closure_reconciliation AS reconciliation;

UPDATE workflow_closure_barriers AS barrier
SET internal_event_id = reconciliation.internal_event_id,
    applied_revision = reconciliation.applied_revision
FROM phase_nine_settled_closure_reconciliation AS reconciliation
WHERE barrier.workflow_id = reconciliation.workflow_id
  AND barrier.closure_id = reconciliation.closure_id;

INSERT INTO jobs (
    id, queue, kind, payload, status, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_internal_event_id, action_key
)
SELECT reconciliation.label_job_id, 'workflow', 'RECONCILE_GITHUB_LABELS',
       jsonb_build_object(
           'state', CASE
               WHEN reconciliation.closure_reopen_requested THEN 'DORMANT'
               ELSE 'CLOSED'
           END,
           'ready_for_sha', '', 'consume_run', FALSE,
           'revision', reconciliation.applied_revision
       ),
       'AVAILABLE', clock_timestamp(), 3,
       'workflow:' || reconciliation.workflow_id::text ||
           ':internal-event:' || reconciliation.internal_event_id::text ||
           ':action:reconcile-github-labels',
       reconciliation.workflow_id, reconciliation.internal_event_id,
       'reconcile-github-labels'
FROM phase_nine_settled_closure_reconciliation AS reconciliation;

-- A desired retained state without concrete Assignments has nothing to collect.
CREATE TEMPORARY TABLE phase_nine_empty_closed_workflows ON COMMIT DROP AS
SELECT workflow.id AS workflow_id
FROM workflows AS workflow
WHERE workflow.status = 'CLOSED'
  AND NOT EXISTS (
      SELECT 1
      FROM agent_assignments AS assignment
      WHERE assignment.workflow_id = workflow.id
        AND assignment.status <> 'SUPERSEDED'
        AND assignment.state_deleted_at IS NULL
  );

UPDATE workflows AS workflow
SET desired_runtime_state = 'COLLECTED', retention_deadline = NULL,
    retention_token = NULL, updated_at = clock_timestamp()
FROM phase_nine_empty_closed_workflows AS empty
WHERE workflow.id = empty.workflow_id;

UPDATE job_attempts AS attempt
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = FALSE,
    last_error = 'Phase 9 migration cancelled orphaned Assignment collection'
FROM jobs AS job, phase_nine_empty_closed_workflows AS empty
WHERE job.workflow_id = empty.workflow_id
  AND job.kind = 'COLLECT_ASSIGNMENTS' AND job.status = 'LEASED'
  AND attempt.job_id = job.id AND attempt.attempt_number = job.attempt_count
  AND attempt.lease_token = job.lease_token AND attempt.status = 'LEASED';

UPDATE jobs AS job
SET status = 'CANCELLED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    completed_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'Phase 9 migration cancelled orphaned Assignment collection'
FROM phase_nine_empty_closed_workflows AS empty
WHERE job.workflow_id = empty.workflow_id
  AND job.kind = 'COLLECT_ASSIGNMENTS'
  AND job.status IN ('AVAILABLE', 'LEASED');

-- Phase 8 could leave CLOSED Workflows with desired-only completion and an
-- unfenced collection Job. Materialize those concrete rows and replace the Job
-- payload in place so its original action provenance remains intact.
UPDATE agent_assignments AS assignment
SET status = 'COMPLETED',
    completed_at = COALESCE(assignment.completed_at, clock_timestamp()),
    retention_until = workflow.retention_deadline,
    updated_at = clock_timestamp()
FROM workflows AS workflow
WHERE workflow.id = assignment.workflow_id
  AND workflow.status = 'CLOSED'
  AND workflow.retention_token IS NOT NULL
  AND assignment.status <> 'SUPERSEDED'
  AND assignment.state_deleted_at IS NULL;

UPDATE agent_sessions AS session
SET status = 'RETAINED',
    retained_at = COALESCE(session.retained_at, clock_timestamp()),
    updated_at = clock_timestamp()
FROM agent_assignments AS assignment, workflows AS workflow
WHERE assignment.id = session.agent_assignment_id
  AND workflow.id = assignment.workflow_id
  AND workflow.status = 'CLOSED'
  AND workflow.retention_token IS NOT NULL
  AND assignment.status = 'COMPLETED'
  AND assignment.state_deleted_at IS NULL
  AND session.state_deleted_at IS NULL
  AND session.status IN ('CREATING', 'ACTIVE', 'RETAINED');

CREATE TEMPORARY TABLE phase_nine_retention_backfill ON COMMIT DROP AS
SELECT workflow.id AS workflow_id,
       workflow.retention_token,
       workflow.retention_deadline,
       workflow.state_revision AS workflow_revision,
       reconciliation.internal_event_id,
       (md5('migration:000011:retention-generation:' || workflow.id::text || ':' || workflow.retention_token))::UUID AS generation_id,
       COALESCE(existing.id,
           (md5('migration:000011:collection-job:' || workflow.id::text || ':' || workflow.retention_token))::UUID) AS collection_job_id,
       existing.id IS NULL AS create_collection_job,
       ARRAY(
           SELECT assignment.id
           FROM agent_assignments AS assignment
           WHERE assignment.workflow_id = workflow.id
             AND assignment.status = 'COMPLETED'
             AND assignment.state_deleted_at IS NULL
           ORDER BY assignment.id
       ) AS assignment_ids
FROM workflows AS workflow
LEFT JOIN phase_nine_settled_closure_reconciliation AS reconciliation
  ON reconciliation.workflow_id = workflow.id
LEFT JOIN LATERAL (
    SELECT job.id
    FROM jobs AS job
    WHERE job.workflow_id = workflow.id
      AND job.kind = 'COLLECT_ASSIGNMENTS'
      AND job.status IN ('AVAILABLE', 'LEASED')
      AND job.payload->>'retention_token' = workflow.retention_token
    ORDER BY job.created_at, job.id
    LIMIT 1
) AS existing ON TRUE
WHERE workflow.status = 'CLOSED'
  AND workflow.retention_token IS NOT NULL
  AND workflow.retention_deadline IS NOT NULL
  AND EXISTS (
      SELECT 1
      FROM agent_assignments AS assignment
      WHERE assignment.workflow_id = workflow.id
        AND assignment.status = 'COMPLETED'
        AND assignment.state_deleted_at IS NULL
  );

UPDATE job_attempts AS attempt
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = TRUE,
    last_error = 'Replaced by Phase 9 fenced Assignment collection'
FROM phase_nine_retention_backfill AS backfill
WHERE attempt.job_id = backfill.collection_job_id
  AND attempt.status = 'LEASED';

UPDATE jobs AS job
SET payload = jsonb_build_object(
        'generation_id', backfill.generation_id,
        'workflow_id', backfill.workflow_id,
        'retention_token', backfill.retention_token,
        'retain_until', backfill.retention_deadline,
        'revision', backfill.workflow_revision,
        'assignment_ids', to_jsonb(backfill.assignment_ids)
    ),
    status = 'AVAILABLE', available_at = backfill.retention_deadline,
    max_attempts = GREATEST(job.max_attempts, job.attempt_count + 1),
    lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, completed_at = NULL,
    result = NULL, last_error = NULL, updated_at = clock_timestamp(),
    retention_generation_id = backfill.generation_id
FROM phase_nine_retention_backfill AS backfill
WHERE job.id = backfill.collection_job_id
  AND NOT backfill.create_collection_job;

INSERT INTO jobs (
    id, queue, kind, payload, status, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_internal_event_id, action_key,
    retention_generation_id
)
SELECT backfill.collection_job_id, 'workflow', 'COLLECT_ASSIGNMENTS',
       jsonb_build_object(
           'generation_id', backfill.generation_id,
           'workflow_id', backfill.workflow_id,
           'retention_token', backfill.retention_token,
           'retain_until', backfill.retention_deadline,
           'revision', backfill.workflow_revision,
           'assignment_ids', to_jsonb(backfill.assignment_ids)
       ),
       'AVAILABLE', backfill.retention_deadline, 3,
       CASE
           WHEN backfill.internal_event_id IS NOT NULL
               THEN 'workflow:' || backfill.workflow_id::text ||
                   ':internal-event:' || backfill.internal_event_id::text ||
                   ':action:collect-retention'
           ELSE 'workflow:' || backfill.workflow_id::text ||
               ':migration:000011:collect-retention'
       END,
       backfill.workflow_id, backfill.internal_event_id,
       CASE WHEN backfill.internal_event_id IS NOT NULL
           THEN 'collect-retention' ELSE NULL END,
       backfill.generation_id
FROM phase_nine_retention_backfill AS backfill
WHERE backfill.create_collection_job;

INSERT INTO assignment_retention_generations (
    id, workflow_id, retention_token, retention_deadline,
    workflow_revision, status, collection_job_id
)
SELECT generation_id, workflow_id, retention_token, retention_deadline,
       workflow_revision, 'SCHEDULED', collection_job_id
FROM phase_nine_retention_backfill;

INSERT INTO assignment_retention_targets (
    retention_generation_id, assignment_id, session_id,
    runtime_state_path, runtime_image_digest
)
SELECT backfill.generation_id, assignment.id, NULL,
       assignment.runtime_state_path, assignment.runtime_image_digest
FROM phase_nine_retention_backfill AS backfill
JOIN agent_assignments AS assignment
  ON assignment.id = ANY(backfill.assignment_ids)
UNION ALL
SELECT backfill.generation_id, assignment.id, session.id,
       session.runtime_state_path, session.runtime_image_digest
FROM phase_nine_retention_backfill AS backfill
JOIN agent_assignments AS assignment
  ON assignment.id = ANY(backfill.assignment_ids)
JOIN agent_sessions AS session ON session.agent_assignment_id = assignment.id
WHERE session.status = 'RETAINED' AND session.state_deleted_at IS NULL;

UPDATE job_attempts AS attempt
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = FALSE,
    last_error = 'Lifecycle Job made redundant by Phase 9 atomic Store transition'
FROM jobs AS job
WHERE job.id = attempt.job_id
  AND job.kind IN ('COMPLETE_ASSIGNMENTS', 'CANCEL_ASSIGNMENT_RETENTION')
  AND job.status = 'LEASED' AND attempt.status = 'LEASED';

UPDATE jobs
SET status = 'CANCELLED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    completed_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'Lifecycle Job made redundant by Phase 9 atomic Store transition'
WHERE kind IN ('COMPLETE_ASSIGNMENTS', 'CANCEL_ASSIGNMENT_RETENTION')
  AND status IN ('AVAILABLE', 'LEASED');
