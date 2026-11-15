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
ALTER TABLE review_admission_requests ADD COLUMN accepted_plan_revision_id TEXT;
-- Prior records have only their initial accepted binding: no earlier feature
-- could revise implementation-time commitments. Do not invent an unused grant.
UPDATE agent_turns t SET accepted_plan_revision_id=c.state->'planning'->'implementation'->>'accepted_revision_id'
FROM work_item_collaboration c WHERE c.workflow_id=t.workflow_id
AND COALESCE(c.state->'planning'->'implementation'->>'first_developer_turn_id','')<>'';
UPDATE change_proposals p SET ready_for_plan_revision_id=c.state->'planning'->'implementation'->>'accepted_revision_id'
FROM work_item_collaboration c WHERE c.workflow_id=p.workflow_id AND p.ready_for_sha IS NOT NULL
AND COALESCE(c.state->'planning'->'implementation'->>'first_developer_turn_id','')<>'';
UPDATE change_proposal_reviews r SET accepted_plan_revision_id=c.state->'planning'->'implementation'->>'accepted_revision_id'
FROM change_proposals p JOIN work_item_collaboration c ON c.workflow_id=p.workflow_id
WHERE r.change_proposal_id=p.id AND COALESCE(c.state->'planning'->'implementation'->>'first_developer_turn_id','')<>'';
UPDATE review_admission_requests r SET accepted_plan_revision_id=c.state->'planning'->'implementation'->>'accepted_revision_id'
FROM work_item_collaboration c WHERE c.workflow_id=r.workflow_id
AND COALESCE(c.state->'planning'->'implementation'->>'first_developer_turn_id','')<>'';
