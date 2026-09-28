-- An in-progress publication is a verified binding for one Developer Turn.
-- It does not create a Change Proposal or request review.
CREATE TABLE agent_turn_publications (
    agent_turn_id UUID PRIMARY KEY,
    execution_epoch BIGINT NOT NULL,
    head_ref TEXT NOT NULL CHECK (head_ref <> ''),
    head_sha TEXT NOT NULL CHECK (head_sha ~ '^[0-9a-f]+$' AND char_length(head_sha) IN (40, 64)),
    base_ref TEXT NOT NULL CHECK (base_ref <> ''),
    pull_request_id BIGINT,
    pull_request_number BIGINT,
    pull_request_node_id TEXT,
    source_publish_mutation_id UUID NOT NULL REFERENCES tool_invocations (id),
    source_open_pr_mutation_id UUID REFERENCES tool_invocations (id),
    bound_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES agent_turns (id, execution_epoch),
    CHECK (
        (pull_request_id IS NULL AND pull_request_number IS NULL AND pull_request_node_id IS NULL AND source_open_pr_mutation_id IS NULL)
        OR (pull_request_id IS NOT NULL AND pull_request_id > 0
            AND pull_request_number IS NOT NULL AND pull_request_number > 0
            AND pull_request_node_id IS NOT NULL AND pull_request_node_id <> ''
            AND source_open_pr_mutation_id IS NOT NULL)
    )
);
