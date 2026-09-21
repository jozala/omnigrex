ALTER TABLE workflows
    ADD COLUMN continuation_stage TEXT,
    ADD CONSTRAINT workflows_continuation_stage_check CHECK (
        continuation_stage IS NULL OR continuation_stage ~ '^[a-z][a-z0-9_-]{0,63}$'
    );

UPDATE workflows
SET continuation_stage = 'review'
WHERE status = 'PR_READY'
   OR (status = 'CLOSING' AND resume_role = 'REVIEWER');

ALTER TABLE workflow_attempts
    ADD COLUMN current_stage TEXT NOT NULL DEFAULT 'implementation',
    ADD COLUMN review_usage JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT workflow_attempts_current_stage_check CHECK (
        current_stage ~ '^[a-z][a-z0-9_-]{0,63}$'
    ),
    ADD CONSTRAINT workflow_attempts_review_usage_check CHECK (
        jsonb_typeof(review_usage) = 'object'
    );

UPDATE workflow_attempts AS attempt
SET current_stage = CASE
        WHEN workflow.status IN ('REVIEWING', 'PR_READY') OR workflow.resume_role = 'REVIEWER'
          OR EXISTS (
              SELECT 1
              FROM agent_turns AS active_turn
              JOIN agent_sessions AS active_session ON active_session.id = active_turn.agent_session_id
              JOIN agent_assignments AS active_assignment ON active_assignment.id = active_session.agent_assignment_id
              WHERE active_turn.workflow_attempt_id = attempt.id
                AND active_turn.active
                AND active_assignment.role = 'REVIEWER'
          )
        THEN 'review'
        ELSE 'implementation'
    END
FROM workflows AS workflow
WHERE workflow.id = attempt.workflow_id;

ALTER TABLE agent_turns
    ADD COLUMN stage_id TEXT NOT NULL DEFAULT 'implementation',
    ADD CONSTRAINT agent_turns_stage_id_check CHECK (
        stage_id ~ '^[a-z][a-z0-9_-]{0,63}$'
    );

UPDATE agent_turns AS turn
SET stage_id = 'review'
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE session.id = turn.agent_session_id
  AND assignment.role = 'REVIEWER';
