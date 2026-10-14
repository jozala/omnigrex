-- Pi identity is reserved before runtime launch. Existing ACP conversations are
-- neither converted nor rebound by this additive preparation-stage migration.
ALTER TABLE agent_sessions
    ADD COLUMN runtime_session_id TEXT,
    ADD COLUMN runtime_capabilities JSONB,
    ADD CONSTRAINT agent_sessions_runtime_identity_check CHECK (
        runtime_session_id IS NULL OR
        (runtime_profile_name = 'pi-rpc' AND runtime_profile_version = 'v1'
         AND runtime_session_id = id::text AND acp_session_id IS NULL)),
    ADD CONSTRAINT agent_sessions_runtime_capabilities_check CHECK (
        runtime_capabilities IS NULL OR
        (runtime_session_id IS NOT NULL AND jsonb_typeof(runtime_capabilities) = 'object'));

ALTER TABLE agent_sessions DROP CONSTRAINT agent_sessions_active_identifier_check;
ALTER TABLE agent_sessions ADD CONSTRAINT agent_sessions_active_identifier_check CHECK (
    status <> 'ACTIVE' OR acp_session_id IS NOT NULL
    OR (runtime_session_id IS NOT NULL AND runtime_capabilities IS NOT NULL));

CREATE UNIQUE INDEX agent_sessions_runtime_identifier_idx
    ON agent_sessions(runtime_session_id) WHERE runtime_session_id IS NOT NULL;

CREATE FUNCTION protect_runtime_session_identity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.runtime_session_id IS NOT NULL AND NEW.runtime_session_id IS DISTINCT FROM OLD.runtime_session_id)
       OR (OLD.runtime_capabilities IS NOT NULL AND NEW.runtime_capabilities IS DISTINCT FROM OLD.runtime_capabilities) THEN
        RAISE EXCEPTION 'Runtime Agent Session identity and capabilities are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER runtime_session_identity_immutable
BEFORE UPDATE ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION protect_runtime_session_identity();

CREATE OR REPLACE FUNCTION initialize_participant_state_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO retained_runtime_states (agent_participant_id, format, metadata)
    VALUES (NEW.id,
            CASE WHEN NEW.runtime_profile_name = 'pi-rpc' THEN 'pi-session/v3' ELSE 'opencode-data/v1' END,
            jsonb_build_object('version', 1, 'subpath', NEW.runtime_state_path));
    RETURN NEW;
END;
$$;
