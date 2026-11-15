ALTER TABLE workflow_attempts ADD COLUMN ci_repair_usage integer NOT NULL DEFAULT 0 CHECK (ci_repair_usage BETWEEN 0 AND 3);
ALTER TABLE agent_turns DROP CONSTRAINT agent_turns_purpose_check;
ALTER TABLE agent_turns ADD CONSTRAINT agent_turns_purpose_check CHECK (purpose IN ('INITIAL_DEVELOPMENT','INITIAL_PLANNING','REVIEW','REQUESTED_CHANGES','RETRY','SYNCHRONIZATION','REACTIVATION','CI_REPAIR'));
-- At most one reset and one routing intent per logical request; committed under the Workflow lock.
CREATE TABLE review_ci_decisions (
 request_id uuid NOT NULL REFERENCES review_admission_requests(id),
 generation bigint NOT NULL,
 workflow_attempt_id uuid NOT NULL REFERENCES workflow_attempts(id),
 decision text NOT NULL CHECK (decision IN ('repair','handoff','reset')),
 workflow_revision bigint NOT NULL,
 evidence jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,decision)
);
-- Legacy completion-only release is not new-semantics admission authority.
UPDATE review_admission_requests SET status='WAITING', release_evidence=NULL WHERE status='RELEASED';
