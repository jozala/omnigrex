-- Record only the bounded ACP stop classification, never prompt text or credentials.
-- A recorded ending survives cleanup failures and orchestrator restarts.
ALTER TABLE agent_turns
    ADD COLUMN prompt_stop_reason TEXT CHECK (prompt_stop_reason IN (
        'end_turn', 'max_tokens', 'max_turn_requests', 'refusal', 'cancelled'
    )),
    ADD COLUMN prompt_error_class TEXT CHECK (prompt_error_class IN (
        'FAILURE', 'DEADLINE', 'CANCELLATION', 'INVALID_RESPONSE'
    )),
    ADD COLUMN prompt_recorded_at TIMESTAMPTZ,
    ADD CONSTRAINT agent_turns_prompt_evidence_shape_check CHECK (
        (prompt_recorded_at IS NULL AND prompt_stop_reason IS NULL AND prompt_error_class IS NULL)
        OR (prompt_recorded_at IS NOT NULL AND
            ((prompt_stop_reason IS NOT NULL AND prompt_error_class IS NULL)
             OR (prompt_stop_reason IS NULL AND prompt_error_class IS NOT NULL)))
    );

ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_recovery_continuation_check,
    ADD CONSTRAINT agent_turns_recovery_continuation_check CHECK (recovery_continuation IN (
        'PENDING_INFRASTRUCTURE_FAILURE', 'INFRASTRUCTURE_FAILURE_APPLIED',
        'MUTATION_RECONCILIATION_HANDOFF_APPLIED', 'MIGRATION_HANDOFF_APPLIED',
        'WORKFLOW_DEFINITION_HANDOFF_APPLIED', 'TERMINAL_CORROBORATION_PENDING',
        'TERMINAL_CORROBORATION_APPLIED'
    ));
