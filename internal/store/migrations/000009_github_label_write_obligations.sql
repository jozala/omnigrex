-- Register uncertainty before sending label writes, not after losing the lease.
-- A process crash or failed return-path transaction leaves restart-visible work.
CREATE TABLE workflow_github_label_writes (
    lease_token uuid PRIMARY KEY REFERENCES job_attempts(lease_token) ON DELETE CASCADE,
    workflow_id uuid NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    next_probe_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    probe_generation bigint NOT NULL DEFAULT 0 CHECK (probe_generation >= 0),
    probe_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL
);
CREATE INDEX workflow_github_label_writes_probe ON workflow_github_label_writes(next_probe_at);
