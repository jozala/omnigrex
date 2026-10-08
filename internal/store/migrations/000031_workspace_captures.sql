CREATE TABLE workspace_captures (
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL CHECK (execution_epoch > 0),
    purpose text NOT NULL CHECK (purpose IN ('publication', 'terminal')),
    operation_id text NOT NULL CHECK (octet_length(operation_id) BETWEEN 1 AND 256),
    request jsonb NOT NULL CHECK (jsonb_typeof(request) = 'object'),
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    sealed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (agent_turn_id, execution_epoch, purpose, operation_id),
    FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES agent_turns(id, execution_epoch)
);

CREATE FUNCTION reject_workspace_capture_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Workspace Capture identity and digest are immutable';
END;
$$;

CREATE TRIGGER workspace_captures_immutable
BEFORE UPDATE OR DELETE ON workspace_captures
FOR EACH ROW EXECUTE FUNCTION reject_workspace_capture_mutation();

ALTER TABLE execution_reservations
    ADD COLUMN preparation_only_cleanup boolean NOT NULL DEFAULT false;

CREATE FUNCTION protect_preparation_only_cleanup_proof() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.preparation_only_cleanup THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'Preparation-only cleanup proof is immutable';
        END IF;
        IF NOT NEW.preparation_only_cleanup
           OR NEW.agent_turn_id IS DISTINCT FROM OLD.agent_turn_id
           OR NEW.execution_epoch IS DISTINCT FROM OLD.execution_epoch
           OR NEW.identity IS DISTINCT FROM OLD.identity THEN
            RAISE EXCEPTION 'Preparation-only cleanup proof is immutable';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER execution_preparation_only_cleanup_immutable
BEFORE UPDATE OR DELETE ON execution_reservations
FOR EACH ROW EXECUTE FUNCTION protect_preparation_only_cleanup_proof();
