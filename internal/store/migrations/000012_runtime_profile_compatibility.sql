CREATE TABLE runtime_profile_compatibility_results (
    source_runtime_profile_name TEXT NOT NULL CHECK (btrim(source_runtime_profile_name) = source_runtime_profile_name AND source_runtime_profile_name <> ''),
    source_runtime_profile_version TEXT NOT NULL CHECK (btrim(source_runtime_profile_version) = source_runtime_profile_version AND source_runtime_profile_version <> ''),
    source_runtime_profile_content_sha256 TEXT NOT NULL CHECK (source_runtime_profile_content_sha256 ~ '^[0-9a-f]{64}$'),
    source_runtime_image_digest TEXT NOT NULL CHECK (source_runtime_image_digest ~ '^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$'),
    target_runtime_profile_name TEXT NOT NULL CHECK (btrim(target_runtime_profile_name) = target_runtime_profile_name AND target_runtime_profile_name <> ''),
    target_runtime_profile_version TEXT NOT NULL CHECK (btrim(target_runtime_profile_version) = target_runtime_profile_version AND target_runtime_profile_version <> ''),
    target_runtime_profile_content_sha256 TEXT NOT NULL CHECK (target_runtime_profile_content_sha256 ~ '^[0-9a-f]{64}$'),
    target_runtime_image_digest TEXT NOT NULL CHECK (target_runtime_image_digest ~ '^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$'),
    platform_os TEXT NOT NULL CHECK (platform_os = 'linux'),
    platform_arch TEXT NOT NULL CHECK (platform_arch IN ('amd64', 'arm64')),
    state_contract_version TEXT NOT NULL CHECK (state_contract_version = 'opencode-acp-state/v1'),
    workspace_path TEXT NOT NULL CHECK (workspace_path = '/workspace'),
    qualification_suite TEXT NOT NULL CHECK (qualification_suite = 'opencode-controlled-state-upgrade'),
    qualification_version TEXT NOT NULL CHECK (qualification_version = '1'),
    qualified_at TIMESTAMPTZ NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome = 'success'),
    result_sha256 BYTEA NOT NULL CHECK (octet_length(result_sha256) = 32),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (
        source_runtime_profile_name, source_runtime_profile_version,
        source_runtime_profile_content_sha256, source_runtime_image_digest,
        target_runtime_profile_name, target_runtime_profile_version,
        target_runtime_profile_content_sha256, target_runtime_image_digest,
        platform_os, platform_arch, state_contract_version, workspace_path,
        qualification_suite, qualification_version, qualified_at
    ),
    UNIQUE (
        source_runtime_profile_name, source_runtime_profile_version,
        source_runtime_profile_content_sha256, source_runtime_image_digest,
        target_runtime_profile_name, target_runtime_profile_version,
        target_runtime_profile_content_sha256, target_runtime_image_digest,
        platform_os, platform_arch, state_contract_version, workspace_path,
        qualification_suite, qualification_version
    ),
    CHECK (
        source_runtime_profile_name <> target_runtime_profile_name
        OR source_runtime_profile_version <> target_runtime_profile_version
        OR source_runtime_profile_content_sha256 <> target_runtime_profile_content_sha256
        OR source_runtime_image_digest <> target_runtime_image_digest
    )
);

CREATE FUNCTION reject_runtime_profile_compatibility_result_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'Runtime Profile compatibility results are immutable';
END;
$$;

CREATE TRIGGER runtime_profile_compatibility_results_immutable
BEFORE UPDATE OR DELETE ON runtime_profile_compatibility_results
FOR EACH ROW EXECUTE FUNCTION reject_runtime_profile_compatibility_result_mutation();
