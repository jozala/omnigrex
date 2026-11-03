ALTER TABLE workflows ADD COLUMN planning_only BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE agent_turn_settlements DROP CONSTRAINT agent_turn_settlements_workflow_outcome_check;
ALTER TABLE agent_turn_settlements ADD CONSTRAINT agent_turn_settlements_workflow_outcome_check CHECK
    (workflow_outcome IN ('CHANGE_PROPOSAL_READY','CHANGES_REQUESTED','APPROVED','BLOCKED',
                         'INFRASTRUCTURE_FAILED','PREREQUISITE_FAILED','CLARIFICATION_WAIT','PLANNING_READY'));
ALTER TABLE agent_turns DROP CONSTRAINT agent_turns_purpose_check;
ALTER TABLE agent_turns ADD CONSTRAINT agent_turns_purpose_check CHECK
    (purpose IN ('INITIAL_DEVELOPMENT','INITIAL_PLANNING','REVIEW','REQUESTED_CHANGES',
                 'RETRY','SYNCHRONIZATION','REACTIVATION'));
