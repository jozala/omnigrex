CREATE TABLE repository_label_provisioning (
    id UUID PRIMARY KEY,
    repository_id BIGINT NOT NULL CHECK (repository_id > 0),
    repository_owner TEXT NOT NULL CHECK (repository_owner <> ''),
    repository_name TEXT NOT NULL CHECK (repository_name <> ''),
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    source_delivery_id UUID NOT NULL REFERENCES webhook_deliveries (delivery_id),
    status TEXT NOT NULL DEFAULT 'AVAILABLE'
        CHECK (status IN ('AVAILABLE', 'LEASED', 'SUCCEEDED', 'FAILED')),
    priority INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    lease_owner TEXT,
    lease_token UUID,
    leased_at TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at TIMESTAMPTZ,
    result JSONB,
    last_error TEXT,
    CONSTRAINT repository_label_provisioning_attempt_limit_check CHECK (attempt_count <= max_attempts),
    CONSTRAINT repository_label_provisioning_lease_check CHECK (
        (status = 'LEASED' AND lease_owner IS NOT NULL AND lease_token IS NOT NULL
            AND leased_at IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'LEASED' AND lease_owner IS NULL AND lease_token IS NULL
            AND leased_at IS NULL AND lease_expires_at IS NULL)
    ),
    CONSTRAINT repository_label_provisioning_result_check CHECK (
        result IS NULL OR status = 'SUCCEEDED'
    )
);

CREATE UNIQUE INDEX repository_label_provisioning_delivery_repo_idx
    ON repository_label_provisioning (source_delivery_id, repository_id);
CREATE INDEX repository_label_provisioning_available_idx
    ON repository_label_provisioning (priority DESC, available_at, id)
    WHERE status = 'AVAILABLE';
CREATE INDEX repository_label_provisioning_expired_lease_idx
    ON repository_label_provisioning (lease_expires_at)
    WHERE status = 'LEASED';
CREATE INDEX repository_label_provisioning_repository_idx
    ON repository_label_provisioning (repository_id, status, created_at);
