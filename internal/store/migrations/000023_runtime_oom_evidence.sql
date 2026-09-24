ALTER TABLE agent_turns
    ADD COLUMN runtime_oom_container_id TEXT,
    ADD COLUMN runtime_oom_exit_code INTEGER,
    ADD COLUMN runtime_oom_observed_at TIMESTAMPTZ,
    ADD CONSTRAINT agent_turns_runtime_oom_evidence_check CHECK (
        (runtime_oom_container_id IS NULL AND runtime_oom_exit_code IS NULL AND runtime_oom_observed_at IS NULL)
        OR (runtime_oom_container_id IS NOT NULL AND length(runtime_oom_container_id) BETWEEN 1 AND 128
            AND runtime_oom_exit_code IS NOT NULL AND runtime_oom_exit_code >= 0
            AND runtime_oom_observed_at IS NOT NULL)
    );
