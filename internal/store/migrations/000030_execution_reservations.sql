ALTER TABLE agent_turns RENAME COLUMN runtime_oom_container_id TO runtime_oom_resource_ref;

CREATE TABLE execution_reservations (
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL CHECK (execution_epoch > 0),
    identity jsonb NOT NULL,
    specification_sha256 text CHECK (specification_sha256 ~ '^[0-9a-f]{64}$'),
    dispatch_phase text NOT NULL DEFAULT 'reserved' CHECK (dispatch_phase IN ('reserved', 'setup', 'main')),
    setup_resource_ref text,
    setup_settled boolean NOT NULL DEFAULT false,
    resource_ref text,
    creation_settled boolean NOT NULL DEFAULT false,
    revoked boolean NOT NULL DEFAULT false,
    PRIMARY KEY (agent_turn_id, execution_epoch),
    CHECK (specification_sha256 IS NOT NULL OR revoked),
    CHECK (resource_ref IS NULL OR dispatch_phase = 'main'),
    CHECK (setup_resource_ref IS NULL OR dispatch_phase IN ('setup', 'main')),
    CHECK (dispatch_phase <> 'main' OR setup_resource_ref IS NULL OR setup_settled)
);
