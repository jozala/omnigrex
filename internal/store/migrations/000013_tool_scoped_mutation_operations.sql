DROP INDEX tool_invocations_operation_id_idx;
DROP INDEX tool_invocations_idempotency_idx;

CREATE UNIQUE INDEX tool_invocations_operation_id_idx
    ON tool_invocations (operation_lineage_id, tool_name, operation_id)
    WHERE kind = 'MUTATION' AND operation_id IS NOT NULL;

CREATE UNIQUE INDEX tool_invocations_idempotency_idx
    ON tool_invocations (agent_turn_id, execution_epoch, tool_name, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
