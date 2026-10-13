-- Preserve historical classifications without converting conversations or outcomes.
-- Current application writes use only the neutral completed/limited/aborted contract.
ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_prompt_stop_reason_check,
    ADD CONSTRAINT agent_turns_prompt_stop_reason_check CHECK (prompt_stop_reason IN (
        'completed', 'limit_reached', 'aborted',
        'end_turn', 'max_tokens', 'max_turn_requests', 'refusal', 'cancelled'
    ));
