CREATE TABLE review_admission_requests (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 workflow_id uuid NOT NULL REFERENCES workflows(id),
 workflow_attempt_id uuid NOT NULL REFERENCES workflow_attempts(id),
 stage_id text NOT NULL,
 pull_request_id bigint NOT NULL,
 pull_request_number bigint NOT NULL,
 repository_id bigint NOT NULL,
 source_job_id uuid NOT NULL REFERENCES jobs(id),
 created_at timestamptz NOT NULL,
 deadline timestamptz NOT NULL,
 grace_anchor timestamptz NOT NULL,
 next_observation_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
 head_sha text NOT NULL,
 base_sha text NOT NULL DEFAULT '',
 test_merge_sha text NOT NULL DEFAULT '',
 policy_source_sha text NOT NULL DEFAULT '',
 policy jsonb,
 inventory jsonb,
 last_observation jsonb,
 release_evidence jsonb,
 status text NOT NULL DEFAULT 'WAITING' CHECK (status IN ('WAITING','RELEASED','ADMITTED','CANCELLED')),
 admitted_turn_id uuid REFERENCES agent_turns(id),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (deadline >= created_at)
);
CREATE UNIQUE INDEX review_admission_one_pending ON review_admission_requests(workflow_id) WHERE status IN ('WAITING','RELEASED');
CREATE TABLE review_admission_jobs (
 job_id uuid PRIMARY KEY REFERENCES jobs(id),
 request_id uuid NOT NULL REFERENCES review_admission_requests(id)
);
CREATE INDEX review_admission_jobs_request ON review_admission_jobs(request_id);

ALTER TABLE workflows ADD COLUMN review_presentation_revision bigint NOT NULL DEFAULT 0;
