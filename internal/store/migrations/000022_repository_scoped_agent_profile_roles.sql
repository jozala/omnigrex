DROP TRIGGER agent_participants_profile_role ON agent_assignments;
DROP FUNCTION enforce_agent_profile_role();

CREATE FUNCTION enforce_repository_agent_profile_role() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    profile_repository_id BIGINT;
BEGIN
    SELECT repository_id INTO STRICT profile_repository_id
    FROM workflows
    WHERE id = NEW.workflow_id
    FOR SHARE;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        profile_repository_id::text || ':' || NEW.agent_profile_name, 0
    ));
    IF EXISTS (
        SELECT 1
        FROM agent_assignments AS participant
        JOIN workflows AS workflow ON workflow.id = participant.workflow_id
        WHERE workflow.repository_id = profile_repository_id
          AND participant.agent_profile_name = NEW.agent_profile_name
          AND participant.role <> NEW.role
          AND participant.id <> NEW.id
    ) THEN
        RAISE EXCEPTION 'Agent Profile % belongs to another Role in repository %',
            NEW.agent_profile_name, profile_repository_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER agent_participants_profile_role
BEFORE INSERT OR UPDATE OF agent_profile_name, role ON agent_assignments
FOR EACH ROW EXECUTE FUNCTION enforce_repository_agent_profile_role();

CREATE FUNCTION enforce_workflow_repository_agent_profile_roles() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    profile_name TEXT;
BEGIN
    IF NEW.repository_id = OLD.repository_id THEN
        RETURN NEW;
    END IF;

    FOR profile_name IN
        SELECT DISTINCT agent_profile_name
        FROM agent_assignments
        WHERE workflow_id = NEW.id
        ORDER BY agent_profile_name
    LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(
            NEW.repository_id::text || ':' || profile_name, 0
        ));
    END LOOP;

    IF EXISTS (
        SELECT 1
        FROM agent_assignments AS moved
        JOIN agent_assignments AS existing
          ON existing.agent_profile_name = moved.agent_profile_name
         AND existing.role <> moved.role
        JOIN workflows AS existing_workflow ON existing_workflow.id = existing.workflow_id
        WHERE moved.workflow_id = NEW.id
          AND existing.workflow_id <> NEW.id
          AND existing_workflow.repository_id = NEW.repository_id
    ) THEN
        RAISE EXCEPTION 'Workflow Agent Profiles conflict with repository %', NEW.repository_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workflows_repository_agent_profile_roles
BEFORE UPDATE OF repository_id ON workflows
FOR EACH ROW EXECUTE FUNCTION enforce_workflow_repository_agent_profile_roles();
