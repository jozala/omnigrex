ALTER TABLE workflow_attempts ADD COLUMN autonomous_replanning_returns INTEGER NOT NULL DEFAULT 0 CHECK (autonomous_replanning_returns BETWEEN 0 AND 3);
ALTER TABLE agent_turns ADD COLUMN accepted_plan_revision_id TEXT;
ALTER TABLE change_proposals ADD COLUMN ready_for_plan_revision_id TEXT;
ALTER TABLE change_proposal_reviews ADD COLUMN accepted_plan_revision_id TEXT;
ALTER TABLE agent_turn_settlements DROP CONSTRAINT agent_turn_settlements_workflow_outcome_check;
ALTER TABLE agent_turn_settlements ADD CONSTRAINT agent_turn_settlements_workflow_outcome_check CHECK
 (workflow_outcome IN ('CHANGE_PROPOSAL_READY','CHANGES_REQUESTED','APPROVED','BLOCKED','INFRASTRUCTURE_FAILED','PREREQUISITE_FAILED','CLARIFICATION_WAIT','PLANNING_READY','REPLANNING_REQUESTED','REPLANNING_RESOLVED'));
ALTER TABLE agent_turns DROP CONSTRAINT agent_turns_purpose_check;
ALTER TABLE agent_turns ADD CONSTRAINT agent_turns_purpose_check CHECK
 (purpose IN ('INITIAL_DEVELOPMENT','INITIAL_PLANNING','REVIEW','REQUESTED_CHANGES','RETRY','SYNCHRONIZATION','REACTIVATION','REPLANNING','RESUMED_DEVELOPMENT'));
