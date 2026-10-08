-- This is a storage binding migration, not an OpenCode conversation migration.
-- The deployment namespace is claimed once by composition because SQL cannot
-- safely infer a configured Docker volume name.
CREATE TABLE execution_backend_binding (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    family TEXT,
    contract_version INTEGER CHECK (contract_version > 0),
    namespace TEXT,
    CHECK ((family IS NULL AND contract_version IS NULL AND namespace IS NULL)
        OR (family IS NOT NULL AND contract_version IS NOT NULL))
);
INSERT INTO execution_backend_binding (family, contract_version)
SELECT CASE WHEN EXISTS (SELECT 1 FROM agent_assignments) THEN 'docker' END,
       CASE WHEN EXISTS (SELECT 1 FROM agent_assignments) THEN 1 END;

CREATE TABLE retained_runtime_states (
    agent_participant_id UUID PRIMARY KEY REFERENCES agent_assignments(id),
    format TEXT NOT NULL,
    metadata JSONB NOT NULL CHECK (jsonb_typeof(metadata) = 'object'),
    initialized BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO retained_runtime_states (agent_participant_id, format, metadata, initialized)
SELECT id, 'opencode-data/v1',
       jsonb_build_object('version', 1, 'subpath', runtime_state_path),
       EXISTS (SELECT 1 FROM agent_sessions s WHERE s.agent_assignment_id = a.id AND s.acp_session_id IS NOT NULL)
FROM agent_assignments a;

CREATE FUNCTION initialize_participant_state_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO retained_runtime_states (agent_participant_id, format, metadata)
    VALUES (NEW.id, 'opencode-data/v1', jsonb_build_object('version', 1, 'subpath', NEW.runtime_state_path));
    RETURN NEW;
END;
$$;
CREATE TRIGGER participant_state_binding
AFTER INSERT ON agent_assignments
FOR EACH ROW EXECUTE FUNCTION initialize_participant_state_binding();

CREATE FUNCTION protect_retained_state_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.agent_participant_id IS DISTINCT FROM OLD.agent_participant_id
       OR NEW.format IS DISTINCT FROM OLD.format
       OR NEW.metadata IS DISTINCT FROM OLD.metadata
       OR (OLD.initialized AND NOT NEW.initialized) THEN
        RAISE EXCEPTION 'Retained Runtime State binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER retained_state_binding_immutable
BEFORE UPDATE ON retained_runtime_states
FOR EACH ROW EXECUTE FUNCTION protect_retained_state_binding();

CREATE FUNCTION protect_execution_backend_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.family IS NOT NULL AND NEW.family IS DISTINCT FROM OLD.family)
       OR (OLD.contract_version IS NOT NULL AND NEW.contract_version IS DISTINCT FROM OLD.contract_version)
       OR (OLD.namespace IS NOT NULL AND NEW.namespace IS DISTINCT FROM OLD.namespace) THEN
        RAISE EXCEPTION 'Execution Backend binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER execution_backend_binding_immutable
BEFORE UPDATE ON execution_backend_binding
FOR EACH ROW EXECUTE FUNCTION protect_execution_backend_binding();
