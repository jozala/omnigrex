-- Eligible means due and durably unleased, independently of execution capacity.
-- Keep domain predicates aligned with preparation, acquisition, recovery and
-- corroboration readers. All ages use this statement's single observation time.
WITH stages AS (
    SELECT * FROM jsonb_to_recordset($1::jsonb)
        AS stage(stage text, role text, state text, purposes jsonb, requires_proposal boolean)
), due_jobs AS MATERIALIZED (
    SELECT * FROM jobs
    WHERE status = 'AVAILABLE' AND available_at <= statement_timestamp()
      AND attempt_count < max_attempts
      AND ((queue = 'workflow' AND kind = 'PREPARE_AGENT_TURN')
        OR (queue = 'agent-turns' AND kind = 'RUN_AGENT_TURN')
        OR (queue = 'agent-turn-recovery' AND kind IN (
            'STOP_STALE_RUNTIME', 'RECONCILE_AGENT_TURN_MUTATIONS',
            'VERIFY_TERMINAL_INTENT', 'REVALIDATE_TERMINAL_INTENT'))
        OR (queue = 'label_provisioning' AND kind = 'PROVISION_MANAGED_LABELS'))
), pending AS (
    SELECT 'preparation' AS category, job.available_at AS available_at
    FROM due_jobs job
    JOIN workflows workflow ON workflow.id = job.workflow_id
    JOIN workflow_attempts attempt ON attempt.id = job.workflow_attempt_id AND attempt.workflow_id = workflow.id
    JOIN stages stage ON stage.stage = job.payload->>'stage' AND stage.role = job.payload->>'role'
    LEFT JOIN change_proposals proposal ON proposal.workflow_id = workflow.id AND proposal.active
    WHERE job.kind = 'PREPARE_AGENT_TURN'
      AND workflow.status = stage.state
      AND workflow.desired_assignment_status = 'ACTIVE' AND workflow.desired_runtime_state = 'ACTIVE'
      AND workflow.state_revision::text = job.payload->>'revision'
      AND attempt.active AND attempt.current_stage = stage.stage
      AND stage.purposes ? (job.payload->>'purpose')
      AND ((proposal.id IS NULL AND NOT stage.requires_proposal AND COALESCE(job.payload->>'expected_head_sha', '') = '')
        OR proposal.head_sha = job.payload->>'expected_head_sha')
      AND NOT EXISTS (SELECT 1 FROM agent_turns turn WHERE turn.workflow_id = workflow.id AND turn.active)
      AND NOT EXISTS (SELECT 1 FROM agent_turns turn WHERE turn.workflow_id = workflow.id
          AND turn.recovery_started_at IS NOT NULL AND turn.recovery_settled_at IS NULL)
    UNION ALL
    SELECT 'execution', job.available_at
    FROM due_jobs job
    JOIN agent_turns turn ON turn.id = job.agent_turn_id AND turn.execution_epoch = job.execution_epoch
    JOIN workflows workflow ON workflow.id = job.workflow_id
    JOIN workflow_attempts attempt ON attempt.id = job.workflow_attempt_id AND attempt.workflow_id = workflow.id
    JOIN agent_assignments participant ON participant.id = job.agent_assignment_id AND participant.workflow_id = workflow.id
    JOIN agent_sessions session ON session.id = job.agent_session_id AND session.agent_assignment_id = participant.id
    WHERE job.kind = 'RUN_AGENT_TURN'
      AND workflow.status IN ('DEVELOPING', 'REVIEWING') AND attempt.active
      AND participant.status = 'ACTIVE' AND session.control_owner = 'AUTOMATION'
      AND (session.status = 'ACTIVE' OR (session.status = 'CREATING' AND session.acp_session_id IS NULL))
      AND turn.active AND turn.status IN ('QUEUED', 'STARTING')
      AND turn.agent_session_id = session.id AND turn.workflow_attempt_id = attempt.id
      AND turn.control_revision = session.control_revision
      AND job.max_attempts = 1
      AND jsonb_typeof(job.payload) = 'object'
      AND job.payload - ARRAY['agent_turn_id', 'agent_session_id', 'execution_epoch', 'control_revision']::text[] = '{}'::jsonb
      AND job.payload->>'agent_turn_id' = turn.id::text
      AND job.payload->>'agent_session_id' = session.id::text
      AND job.payload->'execution_epoch' = to_jsonb(turn.execution_epoch)
      AND job.payload->'control_revision' = to_jsonb(session.control_revision)
      AND turn.owner_id IS NULL AND turn.owner_token IS NULL AND turn.lease_expires_at IS NULL
    UNION ALL
    SELECT CASE job.kind WHEN 'STOP_STALE_RUNTIME' THEN 'runtime_cleanup' ELSE 'mutation_recovery' END,
           job.available_at
    FROM due_jobs job
    JOIN agent_turns turn ON turn.id = job.agent_turn_id AND turn.execution_epoch = job.execution_epoch
    WHERE job.kind IN ('STOP_STALE_RUNTIME', 'RECONCILE_AGENT_TURN_MUTATIONS')
      AND NOT turn.active AND turn.status IN ('INTERRUPTED', 'RECONCILING')
      AND turn.recovery_started_at IS NOT NULL AND turn.recovery_settled_at IS NULL
      AND ((job.kind = 'STOP_STALE_RUNTIME' AND turn.stop_runtime_job_id = job.id)
        OR (job.kind = 'RECONCILE_AGENT_TURN_MUTATIONS' AND turn.reconcile_mutations_job_id = job.id
            AND turn.runtime_stopped_at IS NOT NULL
            AND EXISTS (SELECT 1 FROM jobs stop WHERE stop.id = turn.stop_runtime_job_id AND stop.status = 'SUCCEEDED')))
    UNION ALL
    SELECT 'corroboration', job.available_at
    FROM due_jobs job
    JOIN agent_turns turn ON turn.id = job.agent_turn_id AND turn.execution_epoch = job.execution_epoch
    JOIN agent_turn_corroborations checkpoint ON checkpoint.agent_turn_id = turn.id AND checkpoint.execution_epoch = turn.execution_epoch
    JOIN workflows workflow ON workflow.id = job.workflow_id AND workflow.id = checkpoint.workflow_id
    JOIN workflow_attempts attempt ON attempt.id = job.workflow_attempt_id AND attempt.workflow_id = workflow.id
    JOIN agent_sessions session ON session.id = job.agent_session_id
    WHERE job.kind IN ('VERIFY_TERMINAL_INTENT', 'REVALIDATE_TERMINAL_INTENT')
      AND workflow.status IN ('DEVELOPING', 'REVIEWING') AND attempt.active
      AND NOT turn.mutation_admission_open AND session.control_owner = 'AUTOMATION'
      AND turn.agent_session_id = session.id AND session.agent_assignment_id = job.agent_assignment_id
      AND ((job.kind = 'VERIFY_TERMINAL_INTENT' AND checkpoint.state = 'PENDING'
            AND checkpoint.verification_job_id = job.id AND turn.active AND turn.status = 'CORROBORATING'
            AND checkpoint.workflow_attempt_id = attempt.id)
        OR (job.kind = 'REVALIDATE_TERMINAL_INTENT' AND checkpoint.state = 'HANDED_OFF'
            AND NOT turn.active AND turn.status = 'FAILED' AND attempt.current_stage = turn.stage_id
            AND checkpoint.workflow_attempt_id <> attempt.id
            AND NOT EXISTS (SELECT 1 FROM agent_turns live WHERE live.workflow_id = workflow.id AND live.active)))
    UNION ALL
    SELECT 'label_provisioning', available_at FROM due_jobs WHERE kind = 'PROVISION_MANAGED_LABELS'
    UNION ALL
    SELECT 'webhooks', COALESCE(delivery.retry_at, delivery.received_at)
    FROM webhook_deliveries delivery
    WHERE delivery.status = 'PENDING' AND delivery.attempt_count < delivery.max_attempts
      AND (delivery.retry_at IS NULL OR delivery.retry_at <= statement_timestamp())
      AND NOT EXISTS (
          SELECT 1 FROM webhook_deliveries earlier
          LEFT JOIN normalized_events earlier_event ON earlier_event.delivery_id = earlier.delivery_id
          WHERE ((delivery.workflow_id IS NOT NULL AND earlier.workflow_id = delivery.workflow_id)
             OR (delivery.repository_id IS NOT NULL AND delivery.issue_id IS NOT NULL
                 AND earlier.repository_id = delivery.repository_id AND earlier.issue_id = delivery.issue_id
                 AND earlier.issue_number = delivery.issue_number))
            AND earlier.event_name = 'issues' AND earlier.action = 'reopened'
            AND (earlier.received_at, earlier.delivery_id) < (delivery.received_at, delivery.delivery_id)
            AND (earlier.status IN ('PENDING', 'PROCESSING')
                 OR (earlier.status = 'PROCESSED' AND earlier_event.status = 'PENDING'))
      )
    UNION ALL
    SELECT 'historical_events', event.created_at
    FROM normalized_events event JOIN webhook_deliveries delivery USING (delivery_id)
    WHERE event.status = 'PENDING' AND event.attempt_count < event.max_attempts
      AND NOT EXISTS (
          SELECT 1 FROM assignment_retention_generations generation
          JOIN workflows workflow ON workflow.id = generation.workflow_id
          WHERE generation.status = 'COLLECTING'
            AND (workflow.id = delivery.workflow_id OR (delivery.workflow_id IS NULL
                 AND workflow.repository_id = delivery.repository_id AND workflow.issue_id = delivery.issue_id
                 AND workflow.issue_number = delivery.issue_number))
      )
      AND NOT EXISTS (
          SELECT 1 FROM normalized_events earlier_event
          JOIN webhook_deliveries earlier_delivery USING (delivery_id)
          WHERE earlier_event.status = 'PENDING' AND earlier_event.attempt_count < earlier_event.max_attempts
            AND ((delivery.workflow_id IS NOT NULL AND earlier_delivery.workflow_id = delivery.workflow_id)
              OR (delivery.repository_id IS NOT NULL AND delivery.issue_id IS NOT NULL
                  AND earlier_delivery.repository_id = delivery.repository_id AND earlier_delivery.issue_id = delivery.issue_id
                  AND earlier_delivery.issue_number = delivery.issue_number))
            AND (earlier_delivery.received_at, earlier_delivery.delivery_id) < (delivery.received_at, delivery.delivery_id)
      )
)
SELECT 'workflow' AS family, status AS label, count(*) AS count, 0::double precision AS age
FROM workflows GROUP BY status
UNION ALL
SELECT 'active', '', count(*), 0::double precision FROM agent_turns
WHERE status IN ('STARTING', 'RUNNING', 'CANCELLING', 'SETTLING', 'RECONCILING', 'CORROBORATING')
UNION ALL
SELECT 'pending', category, count(*),
       GREATEST(0, extract(epoch FROM statement_timestamp() - min(available_at)))::double precision
FROM pending GROUP BY category
UNION ALL
-- No durable first-UNKNOWN timestamp exists. Use the stable start/admission time
-- as a conservative upper bound, not updated_at which changes during retries.
SELECT 'mutation', '', count(*),
       COALESCE(GREATEST(0, extract(epoch FROM statement_timestamp() - min(COALESCE(started_at, admitted_at)))), 0)::double precision
FROM tool_invocations WHERE kind = 'MUTATION' AND state IN ('UNKNOWN', 'RECONCILING');
