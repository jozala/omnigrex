ALTER TABLE agent_participants ADD COLUMN waiting_handoff_attempt_id uuid REFERENCES workflow_attempts(id);

-- Existing waiting Participants retain the most recent durable Handoff that
-- could have placed them under human responsibility. Do not authorize a
-- Participant created after that Handoff, or reactivated since it. A pending
-- Handoff still has an active Attempt and no completed_at; its updated_at
-- records the persisted Handoff decision before Participants were marked waiting.
UPDATE agent_participants AS participant
SET waiting_handoff_attempt_id = (
    SELECT handoff.id
    FROM workflow_attempts AS handoff
    WHERE handoff.workflow_id = participant.workflow_id
      AND handoff.human_handoff_reason IS NOT NULL
      AND COALESCE(handoff.completed_at, handoff.updated_at) >= participant.created_at
      AND NOT EXISTS (
          SELECT 1 FROM jobs AS reactivation
          JOIN workflow_attempts AS resumed ON resumed.id = reactivation.workflow_attempt_id
          WHERE reactivation.id = participant.reactivated_by_preparation_job_id
            AND resumed.workflow_id = handoff.workflow_id
            AND resumed.attempt_number > handoff.attempt_number
      )
    ORDER BY handoff.attempt_number DESC LIMIT 1
)
WHERE participant.status = 'WAITING_FOR_HUMAN' AND participant.state_deleted_at IS NULL;
