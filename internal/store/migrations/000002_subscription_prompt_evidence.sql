ALTER TABLE agent_turns DROP CONSTRAINT agent_turns_prompt_error_class_check;
ALTER TABLE agent_turns ADD CONSTRAINT agent_turns_prompt_error_class_check CHECK
    (prompt_error_class IN ('FAILURE', 'DEADLINE', 'CANCELLATION', 'INVALID_RESPONSE',
                           'SUBSCRIPTION_QUOTA', 'SUBSCRIPTION_AUTH', 'SUBSCRIPTION_CONFIGURATION'));

ALTER TABLE agent_turn_corroborations DROP CONSTRAINT agent_turn_corroborations_prompt_error_check;
ALTER TABLE agent_turn_corroborations ADD CONSTRAINT agent_turn_corroborations_prompt_error_check CHECK
    (prompt_error IN ('FAILURE', 'DEADLINE', 'SUBSCRIPTION_QUOTA', 'SUBSCRIPTION_AUTH', 'SUBSCRIPTION_CONFIGURATION'));

ALTER TABLE agent_turn_settlements DROP CONSTRAINT agent_turn_settlements_workflow_outcome_check;
ALTER TABLE agent_turn_settlements ADD CONSTRAINT agent_turn_settlements_workflow_outcome_check CHECK
    (workflow_outcome IN ('CHANGE_PROPOSAL_READY', 'CHANGES_REQUESTED', 'APPROVED', 'BLOCKED',
                         'INFRASTRUCTURE_FAILED', 'PREREQUISITE_FAILED'));
