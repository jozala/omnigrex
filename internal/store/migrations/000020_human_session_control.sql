ALTER TABLE agent_sessions
    ADD COLUMN workflow_id UUID REFERENCES workflows (id);

UPDATE agent_sessions AS session
SET workflow_id = participant.workflow_id
FROM agent_assignments AS participant
WHERE participant.id = session.agent_assignment_id;

ALTER TABLE agent_sessions
    ALTER COLUMN workflow_id SET NOT NULL;

CREATE FUNCTION set_agent_session_workflow() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    participant_workflow_id UUID;
BEGIN
    SELECT workflow_id INTO participant_workflow_id
    FROM agent_assignments
    WHERE id = NEW.agent_assignment_id;

    IF participant_workflow_id IS NULL THEN
        RAISE EXCEPTION 'Agent Participant does not exist';
    END IF;
    IF NEW.workflow_id IS NOT NULL AND NEW.workflow_id <> participant_workflow_id THEN
        RAISE EXCEPTION 'Agent Session Workflow does not match Agent Participant';
    END IF;
    NEW.workflow_id = participant_workflow_id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER agent_sessions_workflow_identity
BEFORE INSERT OR UPDATE OF agent_assignment_id, workflow_id ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION set_agent_session_workflow();

WITH duplicate_human_control AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY workflow_id
               ORDER BY control_acquired_at NULLS LAST, created_at, id
           ) AS control_rank
    FROM agent_sessions
    WHERE control_owner = 'HUMAN' AND status IN ('ACTIVE', 'RETAINED')
)
UPDATE agent_sessions AS session
SET control_owner = 'AUTOMATION',
    control_revision = control_revision + 1,
    controller_id = 'migration:000020',
    control_acquired_at = clock_timestamp(),
    human_prompt_token = NULL,
    human_prompt_leased_at = NULL,
    human_prompt_lease_expires_at = NULL,
    human_prompt_heartbeat_at = NULL,
    updated_at = clock_timestamp()
FROM duplicate_human_control AS duplicate
WHERE duplicate.id = session.id AND duplicate.control_rank > 1;

CREATE UNIQUE INDEX agent_sessions_one_human_controlled_per_workflow_idx
    ON agent_sessions (workflow_id)
    WHERE control_owner = 'HUMAN' AND status IN ('ACTIVE', 'RETAINED');
