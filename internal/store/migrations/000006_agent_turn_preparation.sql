ALTER TABLE agent_assignments
    ADD COLUMN created_by_preparation_job_id UUID REFERENCES jobs (id),
    ADD COLUMN reactivated_by_preparation_job_id UUID REFERENCES jobs (id),
    ADD COLUMN runtime_profile_content_sha256 TEXT,
    ADD CONSTRAINT agent_assignments_runtime_profile_content_sha256_check
        CHECK (
            runtime_profile_content_sha256 IS NULL
            OR runtime_profile_content_sha256 ~ '^[0-9a-f]{64}$'
        );

-- The legacy columns do not contain enough contract data to reconstruct this hash safely.

UPDATE agent_assignments AS assignment
SET runtime_state_path = COALESCE(
    NULLIF(assignment.runtime_state_path, ''),
    (
        SELECT session.runtime_state_path
        FROM agent_sessions AS session
        WHERE session.agent_assignment_id = assignment.id
        ORDER BY
            CASE session.status
                WHEN 'ACTIVE' THEN 1
                WHEN 'RETAINED' THEN 2
                WHEN 'CREATING' THEN 3
                ELSE 4
            END,
            session.session_number DESC
        LIMIT 1
    ),
    'assignment-' || assignment.id::text || '/runtime-state'
)
WHERE assignment.runtime_state_path IS NULL OR assignment.runtime_state_path = '';

ALTER TABLE agent_assignments
    ALTER COLUMN runtime_state_path SET NOT NULL,
    ADD CONSTRAINT agent_assignments_runtime_state_path_check
        CHECK (runtime_state_path <> '');

CREATE TEMPORARY TABLE phase_six_duplicate_runtime_state_paths ON COMMIT DROP AS
SELECT runtime_state_path
FROM agent_assignments
GROUP BY runtime_state_path
HAVING count(*) > 1;

CREATE TEMPORARY TABLE phase_six_path_conflict_workflows ON COMMIT DROP AS
SELECT assignment.workflow_id,
       string_agg(DISTINCT assignment.runtime_state_path, ', '
                  ORDER BY assignment.runtime_state_path) AS runtime_state_paths
FROM agent_assignments AS assignment
JOIN phase_six_duplicate_runtime_state_paths AS duplicate
  ON duplicate.runtime_state_path = assignment.runtime_state_path
GROUP BY assignment.workflow_id;

ALTER TABLE agent_sessions
    ADD COLUMN runtime_profile_content_sha256 TEXT,
    ADD COLUMN human_prompt_token UUID,
    ADD COLUMN human_prompt_leased_at TIMESTAMPTZ,
    ADD COLUMN human_prompt_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN human_prompt_heartbeat_at TIMESTAMPTZ,
    ADD CONSTRAINT agent_sessions_runtime_profile_content_sha256_check
        CHECK (
            runtime_profile_content_sha256 IS NULL
            OR runtime_profile_content_sha256 ~ '^[0-9a-f]{64}$'
        ),
    ADD CONSTRAINT agent_sessions_control_metadata_check CHECK (
        (controller_id IS NULL AND control_acquired_at IS NULL)
        OR (controller_id IS NOT NULL AND controller_id <> '' AND control_acquired_at IS NOT NULL)
    ),
    ADD CONSTRAINT agent_sessions_human_prompt_lease_check CHECK (
        (human_prompt_token IS NULL AND human_prompt_leased_at IS NULL
            AND human_prompt_lease_expires_at IS NULL AND human_prompt_heartbeat_at IS NULL)
        OR (human_prompt_token IS NOT NULL AND human_prompt_leased_at IS NOT NULL
            AND human_prompt_lease_expires_at IS NOT NULL AND human_prompt_heartbeat_at IS NOT NULL
            AND control_owner = 'HUMAN' AND status IN ('ACTIVE', 'RETAINED')
            AND human_prompt_lease_expires_at > human_prompt_leased_at
            AND human_prompt_heartbeat_at >= human_prompt_leased_at
            AND human_prompt_lease_expires_at > human_prompt_heartbeat_at)
    );

CREATE UNIQUE INDEX agent_sessions_human_prompt_token_idx
    ON agent_sessions (human_prompt_token)
    WHERE human_prompt_token IS NOT NULL;

ALTER TABLE agent_turns
    ADD COLUMN workflow_id UUID REFERENCES workflows (id),
    ADD COLUMN preparation_job_id UUID REFERENCES jobs (id);

UPDATE agent_turns AS turn
SET workflow_id = assignment.workflow_id
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE session.id = turn.agent_session_id;

ALTER TABLE agent_turns
    ALTER COLUMN workflow_id SET NOT NULL,
    ADD CONSTRAINT agent_turns_profile_config_object_check
        CHECK (jsonb_typeof(agent_profile_config) = 'object');

CREATE TEMPORARY TABLE phase_six_ranked_active_turns ON COMMIT DROP AS
WITH active_turn_compatibility AS (
    SELECT turn.id, turn.workflow_id, turn.workflow_attempt_id, assignment.role,
           turn.created_at,
           CASE workflow.status
               WHEN 'DEVELOPING' THEN
                   active_attempt.id IS NOT NULL
                   AND turn.workflow_attempt_id = active_attempt.id
                   AND assignment.role = 'DEVELOPER'
                   AND (
                       (active_proposal.id IS NULL
                           AND turn.change_proposal_id IS NULL
                           AND COALESCE(turn.expected_head_sha, '') = '')
                       OR (active_proposal.id IS NOT NULL
                           AND turn.change_proposal_id = active_proposal.id
                           AND turn.expected_head_sha = active_proposal.head_sha)
                   )
               WHEN 'REVIEWING' THEN
                   active_attempt.id IS NOT NULL
                   AND turn.workflow_attempt_id = active_attempt.id
                   AND assignment.role = 'REVIEWER'
                   AND active_proposal.id IS NOT NULL
                   AND turn.change_proposal_id = active_proposal.id
                   AND turn.expected_head_sha = active_proposal.head_sha
                 WHEN 'CLOSING' THEN
                     active_attempt.id IS NOT NULL
                     AND turn.workflow_attempt_id = active_attempt.id
                    AND closure_barrier.source_turn_id IS NOT NULL
                    AND closure_barrier.source_turn_id = turn.id
                    AND closure_barrier.source_session_id = turn.agent_session_id
                    AND closure_barrier.source_attempt_id = turn.workflow_attempt_id
                    AND closure_barrier.source_execution_epoch = turn.execution_epoch
                    AND closure_barrier.source_control_revision = turn.control_revision
                     AND session.control_revision = closure_barrier.source_control_revision
                     AND turn.status = 'CANCELLING'
                     AND NOT turn.mutation_admission_open
                     AND closure_stop_job.kind = 'STOP_AGENT_TURN'
                     AND closure_stop_job.queue = 'workflow'
                     AND (
                         (closure_stop_job.status = 'SUCCEEDED'
                             AND closure_barrier.runtime_stopped_at IS NOT NULL)
                         OR (closure_stop_job.status = 'AVAILABLE'
                             AND closure_stop_job.attempt_count < closure_stop_job.max_attempts)
                         OR (closure_stop_job.status = 'LEASED'
                             AND (
                                 closure_stop_job.lease_expires_at > clock_timestamp()
                                 OR closure_stop_job.attempt_count < closure_stop_job.max_attempts
                             ))
                     )
                     AND closure_settlement_job.kind = 'SETTLE_CLOSURE'
                     AND closure_settlement_job.queue = 'workflow'
                     AND (
                         (closure_settlement_job.status = 'AVAILABLE'
                             AND closure_settlement_job.attempt_count < closure_settlement_job.max_attempts)
                         OR (closure_settlement_job.status = 'LEASED'
                             AND (
                                 closure_settlement_job.lease_expires_at > clock_timestamp()
                                 OR closure_settlement_job.attempt_count < closure_settlement_job.max_attempts
                             ))
                     )
                 ELSE FALSE
             END AS aggregate_compatible
    FROM agent_turns AS turn
    JOIN agent_sessions AS session ON session.id = turn.agent_session_id
    JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
    JOIN workflows AS workflow ON workflow.id = turn.workflow_id
    LEFT JOIN workflow_attempts AS active_attempt
      ON active_attempt.workflow_id = workflow.id AND active_attempt.active
    LEFT JOIN change_proposals AS active_proposal
      ON active_proposal.workflow_id = workflow.id AND active_proposal.active
    LEFT JOIN workflow_closure_barriers AS closure_barrier
      ON closure_barrier.workflow_id = workflow.id
     AND closure_barrier.closure_id = workflow.closure_id
     AND closure_barrier.settled_at IS NULL
    LEFT JOIN jobs AS closure_stop_job
      ON closure_stop_job.id = closure_barrier.stop_job_id
    LEFT JOIN jobs AS closure_settlement_job
      ON closure_settlement_job.id = closure_barrier.settlement_job_id
    WHERE turn.active
)
SELECT turn.id, turn.workflow_id, turn.workflow_attempt_id, turn.role,
       turn.aggregate_compatible,
       row_number() OVER (
         PARTITION BY turn.workflow_id
         ORDER BY turn.aggregate_compatible DESC, turn.created_at DESC, turn.id DESC
     ) AS workflow_active_rank
FROM active_turn_compatibility AS turn;

CREATE TEMPORARY TABLE phase_six_superseded_active_turns ON COMMIT DROP AS
SELECT id, workflow_id
FROM phase_six_ranked_active_turns
WHERE workflow_active_rank > 1 OR NOT aggregate_compatible;

CREATE TEMPORARY TABLE phase_six_turn_handoff_reasons ON COMMIT DROP AS
SELECT workflow.id AS workflow_id,
       CASE
            WHEN mutation.has_uncertain_outcome
                THEN 'migration_duplicate_active_turn_uncertain_mutation'
            WHEN deferred.has_stranded_event
                THEN 'migration_interrupted_turn_deferred_event'
            ELSE 'migration_active_turn_incompatible_aggregate'
        END AS handoff_reason,
        CASE
            WHEN mutation.has_uncertain_outcome
                THEN 'Mutation outcome is UNKNOWN after Phase 6 reconciled duplicate active Agent Turns; human reconciliation is required'
            WHEN deferred.has_stranded_event
                THEN 'A normalized event was deferred to an Agent Turn interrupted by Phase 6 reconciliation; deterministic replay requires human reconciliation'
            ELSE 'No active Agent Turn is compatible with the current Workflow aggregate after Phase 6 reconciliation; human reconciliation is required'
        END AS diagnostic
FROM workflows AS workflow
JOIN phase_six_ranked_active_turns AS survivor
  ON survivor.workflow_id = workflow.id AND survivor.workflow_active_rank = 1
CROSS JOIN LATERAL (
    SELECT EXISTS (
        SELECT 1
        FROM phase_six_superseded_active_turns AS superseded
        JOIN tool_invocations AS invocation ON invocation.agent_turn_id = superseded.id
        WHERE superseded.workflow_id = workflow.id
          AND invocation.kind = 'MUTATION'
          AND (
              invocation.state IN ('IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
              OR invocation.state = 'RESERVED' AND invocation.started_at IS NOT NULL
          )
    ) AS has_uncertain_outcome
) AS mutation
 CROSS JOIN LATERAL (
     SELECT EXISTS (
         SELECT 1
         FROM normalized_events AS event
         JOIN phase_six_superseded_active_turns AS superseded
           ON superseded.id = event.deferred_for_turn_id
         WHERE superseded.workflow_id = workflow.id
           AND event.status = 'DEFERRED'
     ) AS has_stranded_event
 ) AS deferred
WHERE NOT survivor.aggregate_compatible
   OR mutation.has_uncertain_outcome
   OR deferred.has_stranded_event;

CREATE TEMPORARY TABLE phase_six_handoff_reasons ON COMMIT DROP AS
SELECT turn.workflow_id, turn.handoff_reason,
       turn.diagnostic || CASE
           WHEN conflict.workflow_id IS NULL THEN ''
           ELSE '; multiple legacy Agent Assignments reference the same Runtime State path(s): ' ||
               conflict.runtime_state_paths || '; durable state was preserved'
       END AS diagnostic
FROM phase_six_turn_handoff_reasons AS turn
LEFT JOIN phase_six_path_conflict_workflows AS conflict
  ON conflict.workflow_id = turn.workflow_id
UNION ALL
SELECT conflict.workflow_id,
       'migration_duplicate_runtime_state_path'::TEXT AS handoff_reason,
       'Multiple legacy Agent Assignments reference the same Runtime State path(s): ' ||
           conflict.runtime_state_paths ||
           '; durable state was preserved and human reconciliation is required' AS diagnostic
FROM phase_six_path_conflict_workflows AS conflict
WHERE NOT EXISTS (
    SELECT 1
    FROM phase_six_turn_handoff_reasons AS turn
    WHERE turn.workflow_id = conflict.workflow_id
);

CREATE TEMPORARY TABLE phase_six_missing_handoff_attempts ON COMMIT DROP AS
SELECT candidate.id, handoff.workflow_id,
       COALESCE((
           SELECT MAX(existing.attempt_number)
           FROM workflow_attempts AS existing
           WHERE existing.workflow_id = handoff.workflow_id
       ), 0) + 1 AS attempt_number
FROM phase_six_handoff_reasons AS handoff
CROSS JOIN LATERAL (
    SELECT (md5(
        'migration:000006:workflow:' || handoff.workflow_id::text ||
        ':active-attempt:' || candidate_number::text
    ))::UUID AS id
    FROM generate_series(0, 255) AS candidate_number
    WHERE NOT EXISTS (
        SELECT 1
        FROM workflow_attempts AS existing
        WHERE existing.id = (md5(
            'migration:000006:workflow:' || handoff.workflow_id::text ||
            ':active-attempt:' || candidate_number::text
        ))::UUID
    )
    ORDER BY candidate_number
    LIMIT 1
) AS candidate
WHERE NOT EXISTS (
    SELECT 1
    FROM workflow_attempts AS active_attempt
    WHERE active_attempt.workflow_id = handoff.workflow_id AND active_attempt.active
);

INSERT INTO workflow_attempts (
    id, workflow_id, attempt_number, status, active,
    review_cycles_completed, review_cycle_limit,
    infrastructure_failures, infrastructure_failure_limit
)
SELECT id, workflow_id, attempt_number, 'ACTIVE', TRUE, 0, 3, 0, 1
FROM phase_six_missing_handoff_attempts;

CREATE TEMPORARY TABLE phase_six_handoff_workflows ON COMMIT DROP AS
SELECT workflow.id AS workflow_id,
       active_attempt.id AS workflow_attempt_id,
       workflow.state_revision + 1 AS workflow_revision,
       CASE
           WHEN workflow.status = 'DEVELOPING' THEN 'DEVELOPER'
           WHEN workflow.status = 'REVIEWING' THEN 'REVIEWER'
           WHEN workflow.resume_role IN ('DEVELOPER', 'REVIEWER') THEN workflow.resume_role
            ELSE COALESCE(
                survivor.role,
                (
                    SELECT assignment.role
                    FROM agent_assignments AS assignment
                    WHERE assignment.workflow_id = workflow.id
                    ORDER BY
                        (assignment.state_deleted_at IS NULL) DESC,
                        CASE assignment.status
                            WHEN 'ACTIVE' THEN 1
                            WHEN 'WAITING_FOR_HUMAN' THEN 2
                            WHEN 'COMPLETED' THEN 3
                            ELSE 4
                        END,
                        assignment.generation DESC,
                        assignment.role,
                        assignment.id
                    LIMIT 1
                ),
                'DEVELOPER'
            )
        END AS resume_role,
       handoff.handoff_reason,
       handoff.diagnostic
FROM phase_six_handoff_reasons AS handoff
JOIN workflows AS workflow ON workflow.id = handoff.workflow_id
LEFT JOIN phase_six_ranked_active_turns AS survivor
  ON survivor.workflow_id = workflow.id AND survivor.workflow_active_rank = 1
JOIN workflow_attempts AS active_attempt
  ON active_attempt.workflow_id = workflow.id AND active_attempt.active;

CREATE TEMPORARY TABLE phase_six_abandoned_closure_barriers ON COMMIT DROP AS
SELECT barrier.workflow_id, barrier.closure_id, barrier.stop_job_id,
       barrier.settlement_job_id
FROM phase_six_handoff_workflows AS handoff
JOIN workflows AS workflow ON workflow.id = handoff.workflow_id
JOIN workflow_closure_barriers AS barrier
  ON barrier.workflow_id = workflow.id
WHERE workflow.status = 'CLOSING'
  AND barrier.settled_at IS NULL;

CREATE TEMPORARY TABLE phase_six_interrupted_active_turns ON COMMIT DROP AS
SELECT ranked.id, ranked.workflow_id
FROM phase_six_ranked_active_turns AS ranked
WHERE ranked.id IN (SELECT id FROM phase_six_superseded_active_turns)
   OR ranked.workflow_id IN (SELECT workflow_id FROM phase_six_handoff_workflows);

CREATE TEMPORARY TABLE phase_six_replayed_normalized_events ON COMMIT DROP AS
SELECT event.delivery_id
FROM normalized_events AS event
JOIN phase_six_interrupted_active_turns AS interrupted
  ON interrupted.id = event.deferred_for_turn_id
WHERE event.status = 'DEFERRED';

CREATE TEMPORARY TABLE phase_six_abandoned_event_reconciliation_jobs ON COMMIT DROP AS
SELECT DISTINCT link.job_id
FROM job_normalized_events AS link
JOIN phase_six_replayed_normalized_events AS replayed
  ON replayed.delivery_id = link.normalized_event_id;

UPDATE job_attempts AS attempt
SET status = 'FAILED',
    finished_at = clock_timestamp(),
    retryable = FALSE,
    last_error = concat_ws(
        '; ', NULLIF(attempt.last_error, ''),
        'Phase 6 migration returned deferred normalized events to deterministic replay'
    )
WHERE attempt.job_id IN (SELECT job_id FROM phase_six_abandoned_event_reconciliation_jobs)
  AND attempt.status = 'LEASED';

UPDATE jobs AS job
SET status = 'CANCELLED',
    lease_owner = NULL,
    lease_token = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    updated_at = clock_timestamp(),
    completed_at = clock_timestamp(),
    last_error = concat_ws(
        '; ', NULLIF(job.last_error, ''),
        'Phase 6 migration returned deferred normalized events to deterministic replay'
    )
WHERE job.id IN (SELECT job_id FROM phase_six_abandoned_event_reconciliation_jobs)
  AND job.status IN ('AVAILABLE', 'LEASED');

DELETE FROM job_normalized_events AS link
USING phase_six_replayed_normalized_events AS replayed
WHERE link.normalized_event_id = replayed.delivery_id;

UPDATE normalized_events AS event
SET status = 'PENDING',
    workflow_id = NULL,
    disposition = NULL,
    reason = NULL,
    applied_revision = NULL,
    deferred_for_turn_id = NULL,
    processed_at = NULL
WHERE event.delivery_id IN (SELECT delivery_id FROM phase_six_replayed_normalized_events);

CREATE TEMPORARY TABLE phase_six_missing_execution_jobs ON COMMIT DROP AS
SELECT candidate.id, turn.id AS agent_turn_id, turn.workflow_id,
       turn.workflow_attempt_id, assignment.id AS agent_assignment_id,
       session.id AS agent_session_id, turn.execution_epoch
FROM phase_six_interrupted_active_turns AS interrupted
JOIN agent_turns AS turn ON turn.id = interrupted.id
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
CROSS JOIN LATERAL (
    SELECT (md5(
        'migration:000006:agent-turn:' || turn.id::text ||
        ':execution-identity:' || candidate_number::text
    ))::UUID AS id
    FROM generate_series(0, 255) AS candidate_number
    WHERE NOT EXISTS (
        SELECT 1
        FROM jobs AS existing
        WHERE existing.id = (md5(
            'migration:000006:agent-turn:' || turn.id::text ||
            ':execution-identity:' || candidate_number::text
        ))::UUID
    )
    ORDER BY candidate_number
    LIMIT 1
) AS candidate
WHERE NOT EXISTS (
    SELECT 1
    FROM jobs AS execution_job
    WHERE execution_job.agent_turn_id = turn.id
      AND execution_job.execution_epoch = turn.execution_epoch
      AND execution_job.kind = 'RUN_AGENT_TURN'
);

INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, completed_at, last_error
)
SELECT id, 'agent-turns', 'RUN_AGENT_TURN', '{}'::JSONB, 'CANCELLED', 0,
       clock_timestamp(), 1, workflow_id, workflow_attempt_id,
       agent_assignment_id, agent_session_id, agent_turn_id, execution_epoch,
       clock_timestamp(),
       'Phase 6 migration reconstructed missing Agent Turn execution Job identity'
FROM phase_six_missing_execution_jobs;

CREATE TEMPORARY TABLE phase_six_interrupted_turn_recoveries ON COMMIT DROP AS
SELECT turn.id, turn.workflow_id, turn.workflow_attempt_id,
       assignment.id AS agent_assignment_id, session.id AS agent_session_id,
       turn.execution_epoch, turn.control_revision, execution_job.id AS execution_job_id
FROM phase_six_interrupted_active_turns AS interrupted
JOIN agent_turns AS turn ON turn.id = interrupted.id
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
JOIN jobs AS execution_job
  ON execution_job.agent_turn_id = turn.id
 AND execution_job.execution_epoch = turn.execution_epoch
 AND execution_job.kind = 'RUN_AGENT_TURN';

UPDATE tool_invocations AS invocation
SET state = CASE
        WHEN invocation.state = 'RESERVED' AND invocation.started_at IS NULL THEN 'FAILED'
        WHEN invocation.state IN ('RESERVED', 'IN_FLIGHT', 'RECONCILING') THEN 'UNKNOWN'
        ELSE invocation.state
    END,
    finished_at = CASE
        WHEN invocation.state = 'RESERVED' AND invocation.started_at IS NULL
            THEN COALESCE(invocation.finished_at, clock_timestamp())
        ELSE invocation.finished_at
    END,
    updated_at = clock_timestamp(),
    last_error = concat_ws(
        '; ', NULLIF(invocation.last_error, ''),
        CASE
            WHEN invocation.state = 'RESERVED' AND invocation.started_at IS NULL
                THEN 'Phase 6 migration interrupted active Agent Turn before mutation started during duplicate-active reconciliation'
            ELSE 'Phase 6 migration interrupted active Agent Turn with uncertain mutation during duplicate-active reconciliation'
        END
    )
WHERE invocation.kind = 'MUTATION'
   AND invocation.state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
   AND invocation.agent_turn_id IN (SELECT id FROM phase_six_interrupted_active_turns);

UPDATE job_attempts AS attempt
SET status = 'FAILED',
    finished_at = clock_timestamp(),
    retryable = FALSE,
    last_error = concat_ws(
        '; ', NULLIF(attempt.last_error, ''),
        'Phase 6 migration interrupted active Agent Turn execution during duplicate-active reconciliation'
    )
FROM jobs AS job
WHERE job.id = attempt.job_id
  AND job.kind = 'RUN_AGENT_TURN'
   AND job.agent_turn_id IN (SELECT id FROM phase_six_interrupted_active_turns)
  AND attempt.status = 'LEASED';

UPDATE jobs AS job
SET status = 'CANCELLED',
    lease_owner = NULL,
    lease_token = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    updated_at = clock_timestamp(),
    completed_at = clock_timestamp(),
    last_error = concat_ws(
        '; ', NULLIF(job.last_error, ''),
        'Phase 6 migration interrupted active Agent Turn execution during duplicate-active reconciliation'
    )
WHERE job.kind = 'RUN_AGENT_TURN'
   AND job.agent_turn_id IN (SELECT id FROM phase_six_interrupted_active_turns)
  AND job.status IN ('AVAILABLE', 'LEASED');

CREATE TEMPORARY TABLE phase_six_stop_runtime_jobs ON COMMIT DROP AS
SELECT recovery.id AS agent_turn_id, candidate.id AS job_id
FROM phase_six_interrupted_turn_recoveries AS recovery
CROSS JOIN LATERAL (
    SELECT (md5(
        'migration:000006:agent-turn:' || recovery.id::text ||
        ':stop-stale-runtime:' || candidate_number::text
    ))::UUID AS id
    FROM generate_series(0, 255) AS candidate_number
    WHERE NOT EXISTS (
        SELECT 1
        FROM jobs AS existing
        WHERE existing.id = (md5(
            'migration:000006:agent-turn:' || recovery.id::text ||
            ':stop-stale-runtime:' || candidate_number::text
        ))::UUID
    )
    ORDER BY candidate_number
    LIMIT 1
) AS candidate;

INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch
)
SELECT stop.job_id, 'agent-turn-recovery', 'STOP_STALE_RUNTIME',
       jsonb_build_object(
           'workflow_id', recovery.workflow_id,
           'workflow_attempt_id', recovery.workflow_attempt_id,
           'agent_assignment_id', recovery.agent_assignment_id,
           'agent_session_id', recovery.agent_session_id,
           'agent_turn_id', recovery.id,
           'execution_epoch', recovery.execution_epoch,
           'control_revision', recovery.control_revision,
           'execution_job_id', recovery.execution_job_id
       ),
       'AVAILABLE', 100, clock_timestamp(), 3,
       'agent-turn-recovery:' || recovery.id::text || ':epoch:' ||
           recovery.execution_epoch::text || ':stop_stale_runtime',
       recovery.workflow_id, recovery.workflow_attempt_id,
       recovery.agent_assignment_id, recovery.agent_session_id,
       recovery.id, recovery.execution_epoch
FROM phase_six_interrupted_turn_recoveries AS recovery
JOIN phase_six_stop_runtime_jobs AS stop ON stop.agent_turn_id = recovery.id;

CREATE TEMPORARY TABLE phase_six_reconcile_mutation_jobs ON COMMIT DROP AS
SELECT recovery.id AS agent_turn_id, candidate.id AS job_id
FROM phase_six_interrupted_turn_recoveries AS recovery
CROSS JOIN LATERAL (
    SELECT (md5(
        'migration:000006:agent-turn:' || recovery.id::text ||
        ':reconcile-mutations:' || candidate_number::text
    ))::UUID AS id
    FROM generate_series(0, 255) AS candidate_number
    WHERE NOT EXISTS (
        SELECT 1
        FROM jobs AS existing
        WHERE existing.id = (md5(
            'migration:000006:agent-turn:' || recovery.id::text ||
            ':reconcile-mutations:' || candidate_number::text
        ))::UUID
    )
    ORDER BY candidate_number
    LIMIT 1
) AS candidate
WHERE EXISTS (
    SELECT 1
    FROM tool_invocations AS invocation
    WHERE invocation.agent_turn_id = recovery.id
      AND invocation.execution_epoch = recovery.execution_epoch
      AND invocation.kind = 'MUTATION'
      AND invocation.state IN ('UNKNOWN', 'RECONCILING')
);

INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch
)
SELECT reconcile.job_id, 'agent-turn-recovery', 'RECONCILE_AGENT_TURN_MUTATIONS',
       jsonb_build_object(
           'workflow_id', recovery.workflow_id,
           'workflow_attempt_id', recovery.workflow_attempt_id,
           'agent_assignment_id', recovery.agent_assignment_id,
           'agent_session_id', recovery.agent_session_id,
           'agent_turn_id', recovery.id,
           'execution_epoch', recovery.execution_epoch,
           'control_revision', recovery.control_revision,
           'execution_job_id', recovery.execution_job_id
       ),
       'AVAILABLE', 90, clock_timestamp(), 3,
       'agent-turn-recovery:' || recovery.id::text || ':epoch:' ||
           recovery.execution_epoch::text || ':reconcile_agent_turn_mutations',
       recovery.workflow_id, recovery.workflow_attempt_id,
       recovery.agent_assignment_id, recovery.agent_session_id,
       recovery.id, recovery.execution_epoch
FROM phase_six_interrupted_turn_recoveries AS recovery
JOIN phase_six_reconcile_mutation_jobs AS reconcile ON reconcile.agent_turn_id = recovery.id;

UPDATE job_attempts AS attempt
SET status = 'FAILED',
    finished_at = clock_timestamp(),
    retryable = FALSE,
    last_error = concat_ws(
        '; ', NULLIF(attempt.last_error, ''),
        'Phase 6 migration abandoned Workflow closure during Human Handoff'
    )
FROM jobs AS job
WHERE job.id = attempt.job_id
  AND job.kind IN ('STOP_AGENT_TURN', 'SETTLE_CLOSURE')
  AND attempt.status = 'LEASED'
  AND EXISTS (
      SELECT 1
      FROM phase_six_abandoned_closure_barriers AS barrier
      WHERE barrier.workflow_id = job.workflow_id
        AND job.id IN (barrier.stop_job_id, barrier.settlement_job_id)
  );

UPDATE jobs AS job
SET status = 'CANCELLED',
    lease_owner = NULL,
    lease_token = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    updated_at = clock_timestamp(),
    completed_at = clock_timestamp(),
    last_error = concat_ws(
        '; ', NULLIF(job.last_error, ''),
        'Phase 6 migration abandoned Workflow closure during Human Handoff'
    )
WHERE job.kind IN ('STOP_AGENT_TURN', 'SETTLE_CLOSURE')
  AND job.status IN ('AVAILABLE', 'LEASED')
  AND EXISTS (
      SELECT 1
      FROM phase_six_abandoned_closure_barriers AS barrier
      WHERE barrier.workflow_id = job.workflow_id
        AND job.id IN (barrier.stop_job_id, barrier.settlement_job_id)
  );

UPDATE workflow_closure_barriers AS barrier
SET settled_at = clock_timestamp()
FROM phase_six_abandoned_closure_barriers AS abandoned
WHERE barrier.workflow_id = abandoned.workflow_id
  AND barrier.closure_id = abandoned.closure_id
  AND barrier.settled_at IS NULL;

DELETE FROM agent_turn_slots
WHERE agent_turn_id IN (SELECT id FROM phase_six_interrupted_active_turns);

UPDATE agent_turns AS turn
SET status = CASE
        WHEN reconcile.job_id IS NOT NULL THEN 'RECONCILING'
        ELSE 'INTERRUPTED'
    END,
    active = FALSE,
    owner_id = NULL,
    owner_token = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    mutation_admission_open = FALSE,
    mutation_admission_closed_at = COALESCE(turn.mutation_admission_closed_at, clock_timestamp()),
    completed_at = COALESCE(turn.completed_at, clock_timestamp()),
    recovery_started_at = clock_timestamp(),
    runtime_stop_required = TRUE,
    runtime_stopped_at = NULL,
    recovery_settled_at = NULL,
    stop_runtime_job_id = stop.job_id,
    reconcile_mutations_job_id = reconcile.job_id,
    last_error = concat_ws(
        '; ', NULLIF(turn.last_error, ''),
        'Phase 6 migration interrupted active Agent Turn during duplicate-active reconciliation'
    )
FROM phase_six_stop_runtime_jobs AS stop
LEFT JOIN phase_six_reconcile_mutation_jobs AS reconcile
  ON reconcile.agent_turn_id = stop.agent_turn_id
WHERE turn.id = stop.agent_turn_id;

UPDATE agent_assignments AS assignment
SET status = 'WAITING_FOR_HUMAN',
    completed_at = NULL,
    retention_until = NULL,
    updated_at = clock_timestamp()
WHERE assignment.workflow_id IN (SELECT workflow_id FROM phase_six_handoff_workflows)
  AND assignment.status IN ('ACTIVE', 'WAITING_FOR_HUMAN', 'COMPLETED')
  AND assignment.state_deleted_at IS NULL;

CREATE UNIQUE INDEX agent_assignments_active_runtime_state_path_idx
    ON agent_assignments (runtime_state_path)
    WHERE status = 'ACTIVE' AND state_deleted_at IS NULL;

UPDATE workflow_attempts AS attempt
SET review_cycles_completed = LEAST(attempt.review_cycles_completed, 3),
    review_cycle_limit = 3,
    infrastructure_failures = LEAST(attempt.infrastructure_failures, 1),
    infrastructure_failure_limit = 1,
    human_handoff_reason = handoff.handoff_reason,
    updated_at = clock_timestamp()
FROM phase_six_handoff_workflows AS handoff
WHERE attempt.id = handoff.workflow_attempt_id
  AND attempt.active;

UPDATE change_proposals AS proposal
SET ready_for_sha = NULL,
    updated_at = clock_timestamp()
WHERE proposal.workflow_id IN (SELECT workflow_id FROM phase_six_handoff_workflows)
  AND proposal.active;

UPDATE workflows AS workflow
SET status = 'NEEDS_HUMAN',
    state_revision = handoff.workflow_revision,
    resume_role = handoff.resume_role,
    desired_assignment_status = 'WAITING_FOR_HUMAN',
    desired_runtime_state = 'ACTIVE',
    ready_for_sha = NULL,
    closure_id = NULL,
    closure_deadline = NULL,
    closure_retention_token = NULL,
    closure_reopen_requested = FALSE,
    retention_deadline = NULL,
    retention_token = NULL,
    closed_at = NULL,
    human_handoff_reason = handoff.handoff_reason,
    updated_at = clock_timestamp()
FROM phase_six_handoff_workflows AS handoff
WHERE workflow.id = handoff.workflow_id;

INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id
)
SELECT candidate.id, 'workflow', 'PUBLISH_HUMAN_HANDOFF',
       jsonb_build_object(
           'reason', handoff.handoff_reason,
           'diagnostic', handoff.diagnostic,
           'revision', handoff.workflow_revision
       ),
       'AVAILABLE', 0, clock_timestamp(), 3,
       'migration:000006:workflow:' || handoff.workflow_id::text || ':publish-human-handoff',
       handoff.workflow_id, handoff.workflow_attempt_id
FROM phase_six_handoff_workflows AS handoff
CROSS JOIN LATERAL (
    SELECT (md5(
        'migration:000006:workflow:' || handoff.workflow_id::text ||
        ':publish-human-handoff:' || candidate_number::text
    ))::UUID AS id
    FROM generate_series(0, 255) AS candidate_number
    WHERE NOT EXISTS (
        SELECT 1
        FROM jobs AS existing
        WHERE existing.id = (md5(
            'migration:000006:workflow:' || handoff.workflow_id::text ||
            ':publish-human-handoff:' || candidate_number::text
        ))::UUID
          AND existing.idempotency_key IS DISTINCT FROM
              'migration:000006:workflow:' || handoff.workflow_id::text || ':publish-human-handoff'
    )
    ORDER BY candidate_number
    LIMIT 1
) AS candidate
ON CONFLICT DO NOTHING;

INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id
)
SELECT candidate.id, 'workflow', 'RECONCILE_GITHUB_LABELS',
       jsonb_build_object(
           'state', 'NEEDS_HUMAN',
           'ready_for_sha', '',
           'consume_run', FALSE,
           'revision', handoff.workflow_revision
       ),
       'AVAILABLE', 0, clock_timestamp(), 3,
       'migration:000006:workflow:' || handoff.workflow_id::text || ':reconcile-github-labels',
       handoff.workflow_id, handoff.workflow_attempt_id
FROM phase_six_handoff_workflows AS handoff
CROSS JOIN LATERAL (
    SELECT (md5(
        'migration:000006:workflow:' || handoff.workflow_id::text ||
        ':reconcile-github-labels:' || candidate_number::text
    ))::UUID AS id
    FROM generate_series(0, 255) AS candidate_number
    WHERE NOT EXISTS (
        SELECT 1
        FROM jobs AS existing
        WHERE existing.id = (md5(
            'migration:000006:workflow:' || handoff.workflow_id::text ||
            ':reconcile-github-labels:' || candidate_number::text
        ))::UUID
          AND existing.idempotency_key IS DISTINCT FROM
              'migration:000006:workflow:' || handoff.workflow_id::text || ':reconcile-github-labels'
    )
    ORDER BY candidate_number
    LIMIT 1
) AS candidate
ON CONFLICT DO NOTHING;

CREATE UNIQUE INDEX agent_turns_one_active_workflow_idx
    ON agent_turns (workflow_id)
    WHERE active;

CREATE UNIQUE INDEX agent_turns_preparation_job_idx
    ON agent_turns (preparation_job_id)
    WHERE preparation_job_id IS NOT NULL;

CREATE INDEX jobs_available_kind_idx
    ON jobs (queue, kind, priority DESC, available_at, id)
    WHERE status = 'AVAILABLE';
