-- Record the validated candidate before any external Git push. The old head
-- remains expected_sha; the format bit distinguishes legacy rows from new
-- attempts interrupted before a candidate could be recorded.
ALTER TABLE tool_invocations
    ADD COLUMN history_publication BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN proposed_sha TEXT,
    ADD CONSTRAINT tool_invocations_proposed_publication_sha_check CHECK (
        proposed_sha IS NULL OR
        (history_publication AND tool_name = 'publish_changes' AND
            (proposed_sha ~ '^[0-9a-f]{40}$' OR proposed_sha ~ '^[0-9a-f]{64}$'))
    ),
    ADD CONSTRAINT tool_invocations_history_publication_check CHECK (
        NOT history_publication OR (kind = 'MUTATION' AND tool_name = 'publish_changes')
    );
