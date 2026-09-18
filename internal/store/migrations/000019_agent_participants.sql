-- Agent Assignments represented both durable participants and their changing Stage use.
-- Keep the existing table and foreign keys as the participant storage so deployed data
-- migrates without rebuilding the execution hierarchy.
DROP INDEX agent_assignments_current_role_idx;
DROP INDEX agent_assignments_active_runtime_state_path_idx;

ALTER TABLE agent_assignments
    DROP CONSTRAINT agent_assignments_workflow_id_role_generation_key,
    ADD CONSTRAINT agent_participants_identity_key
        UNIQUE (workflow_id, generation, agent_profile_name),
    ADD CONSTRAINT agent_participants_identity_role_key
        UNIQUE (id, workflow_id, generation, role);

CREATE UNIQUE INDEX agent_participants_live_runtime_state_path_idx
    ON agent_assignments (runtime_state_path)
    WHERE status = 'ACTIVE' AND state_deleted_at IS NULL;

CREATE FUNCTION enforce_agent_profile_role() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.agent_profile_name, 0));
    IF EXISTS (
        SELECT 1 FROM agent_assignments
        WHERE agent_profile_name = NEW.agent_profile_name AND role <> NEW.role
          AND id <> NEW.id
    ) THEN
        RAISE EXCEPTION 'Agent Profile % belongs to another Role', NEW.agent_profile_name;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER agent_participants_profile_role
BEFORE INSERT OR UPDATE OF agent_profile_name, role ON agent_assignments
FOR EACH ROW EXECUTE FUNCTION enforce_agent_profile_role();

CREATE VIEW agent_participants AS
SELECT id, workflow_id, generation AS assignment_generation, role,
       agent_profile_name, runtime_profile_name, runtime_profile_version,
       runtime_profile_content_sha256, runtime_image_digest,
       github_app_installation_id, github_app_actor_id, runtime_state_path,
       status, created_by_preparation_job_id, reactivated_by_preparation_job_id,
       created_at, updated_at, completed_at, retention_until, state_deleted_at
FROM agent_assignments;

CREATE TABLE stage_assignments (
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    assignment_generation INTEGER NOT NULL CHECK (assignment_generation > 0),
    stage_id TEXT NOT NULL CHECK (stage_id ~ '^[a-z][a-z0-9_-]{0,63}$'),
    role TEXT NOT NULL CHECK (role IN ('DEVELOPER', 'REVIEWER')),
    agent_participant_id UUID NOT NULL,
    created_by_preparation_job_id UUID REFERENCES jobs (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (workflow_id, assignment_generation, stage_id),
    CONSTRAINT stage_assignments_participant_fk
        FOREIGN KEY (agent_participant_id, workflow_id, assignment_generation, role)
        REFERENCES agent_assignments (id, workflow_id, generation, role)
);

CREATE INDEX stage_assignments_participant_idx
    ON stage_assignments (agent_participant_id, workflow_id, assignment_generation);

-- Historical rows predate durable Stage selection. Materialize only bindings proven by
-- persisted Turns; unvisited Stages remain unassigned and will be provisioned lazily.
INSERT INTO stage_assignments (
    workflow_id, assignment_generation, stage_id, role,
    agent_participant_id, created_by_preparation_job_id, created_at
)
SELECT DISTINCT ON (turn.workflow_id, participant.generation, turn.stage_id)
       turn.workflow_id, participant.generation, turn.stage_id, participant.role,
       participant.id,
       COALESCE(turn.preparation_job_id, participant.created_by_preparation_job_id),
       turn.created_at
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS participant ON participant.id = session.agent_assignment_id
ORDER BY turn.workflow_id, participant.generation, turn.stage_id,
         turn.created_at, turn.id;

CREATE FUNCTION reject_stage_assignment_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Stage Assignments are immutable';
END;
$$;

CREATE TRIGGER stage_assignments_immutable
BEFORE UPDATE OR DELETE ON stage_assignments
FOR EACH ROW EXECUTE FUNCTION reject_stage_assignment_mutation();

CREATE FUNCTION protect_agent_participant_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.workflow_id IS DISTINCT FROM OLD.workflow_id
       OR NEW.generation IS DISTINCT FROM OLD.generation
       OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.agent_profile_name IS DISTINCT FROM OLD.agent_profile_name
       OR NEW.runtime_profile_name IS DISTINCT FROM OLD.runtime_profile_name
       OR NEW.runtime_profile_version IS DISTINCT FROM OLD.runtime_profile_version
       OR NEW.runtime_profile_content_sha256 IS DISTINCT FROM OLD.runtime_profile_content_sha256
       OR NEW.runtime_image_digest IS DISTINCT FROM OLD.runtime_image_digest
       OR NEW.runtime_state_path IS DISTINCT FROM OLD.runtime_state_path
       OR NEW.created_by_preparation_job_id IS DISTINCT FROM OLD.created_by_preparation_job_id THEN
        RAISE EXCEPTION 'Agent Participant identity and Runtime binding are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER agent_participants_binding_immutable
BEFORE UPDATE ON agent_assignments
FOR EACH ROW EXECUTE FUNCTION protect_agent_participant_binding();
