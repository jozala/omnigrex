ALTER TABLE agent_turns
    ADD COLUMN operation_lineage_id UUID;

WITH RECURSIVE turn_lineage AS (
    SELECT id, id AS operation_lineage_id
    FROM agent_turns
    WHERE retry_of_turn_id IS NULL

    UNION ALL

    SELECT retry.id, parent.operation_lineage_id
    FROM agent_turns AS retry
    JOIN turn_lineage AS parent ON parent.id = retry.retry_of_turn_id
)
UPDATE agent_turns AS turn
SET operation_lineage_id = lineage.operation_lineage_id
FROM turn_lineage AS lineage
WHERE lineage.id = turn.id;

ALTER TABLE agent_turns
    ALTER COLUMN operation_lineage_id SET NOT NULL,
    ADD CONSTRAINT agent_turns_operation_lineage_check CHECK (
        (retry_of_turn_id IS NULL AND operation_lineage_id = id)
        OR (retry_of_turn_id IS NOT NULL AND operation_lineage_id <> id)
    ),
    ADD CONSTRAINT agent_turns_operation_lineage_id_fkey
        FOREIGN KEY (operation_lineage_id) REFERENCES agent_turns (id);

CREATE UNIQUE INDEX agent_turns_id_session_lineage_idx
    ON agent_turns (id, agent_session_id, operation_lineage_id);

CREATE UNIQUE INDEX agent_turns_id_epoch_lineage_idx
    ON agent_turns (id, execution_epoch, operation_lineage_id);

ALTER TABLE agent_turns
    ADD CONSTRAINT agent_turns_retry_lineage_fkey
        FOREIGN KEY (retry_of_turn_id, agent_session_id, operation_lineage_id)
        REFERENCES agent_turns (id, agent_session_id, operation_lineage_id);

ALTER TABLE tool_invocations
    ADD COLUMN operation_lineage_id UUID;

UPDATE tool_invocations AS invocation
SET operation_lineage_id = turn.operation_lineage_id
FROM agent_turns AS turn
WHERE turn.id = invocation.agent_turn_id
  AND turn.execution_epoch = invocation.execution_epoch;

ALTER TABLE tool_invocations
    ALTER COLUMN operation_lineage_id SET NOT NULL,
    DROP CONSTRAINT tool_invocations_turn_epoch_fk,
    ADD CONSTRAINT tool_invocations_turn_epoch_lineage_fk
        FOREIGN KEY (agent_turn_id, execution_epoch, operation_lineage_id)
        REFERENCES agent_turns (id, execution_epoch, operation_lineage_id);

DROP INDEX tool_invocations_operation_id_idx;
DROP INDEX tool_invocations_idempotency_idx;

CREATE UNIQUE INDEX tool_invocations_operation_id_idx
    ON tool_invocations (operation_lineage_id, operation_id)
    WHERE kind = 'MUTATION' AND operation_id IS NOT NULL;

CREATE UNIQUE INDEX tool_invocations_idempotency_idx
    ON tool_invocations (agent_turn_id, execution_epoch, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
