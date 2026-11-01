-- Interaction identities and source references are independent of GitHub artifacts.
-- The Workflow row is the admission/settlement/preparation serialization boundary.
CREATE TABLE work_item_collaboration (
    workflow_id UUID PRIMARY KEY REFERENCES workflows(id),
    state JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(state) = 'object'),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE workflows ADD COLUMN clarification_waiting BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE agent_turn_settlements DROP CONSTRAINT agent_turn_settlements_workflow_outcome_check;
ALTER TABLE agent_turn_settlements ADD CONSTRAINT agent_turn_settlements_workflow_outcome_check CHECK
    (workflow_outcome IN ('CHANGE_PROPOSAL_READY', 'CHANGES_REQUESTED', 'APPROVED', 'BLOCKED',
                         'INFRASTRUCTURE_FAILED', 'PREREQUISITE_FAILED', 'CLARIFICATION_WAIT'));
