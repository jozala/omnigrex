CREATE TABLE webhook_deliveries (
    delivery_id UUID PRIMARY KEY,
    event_name TEXT NOT NULL CHECK (event_name <> ''),
    action TEXT,
    repository_id BIGINT NOT NULL CHECK (repository_id > 0),
    repository_owner TEXT NOT NULL CHECK (repository_owner <> ''),
    repository_name TEXT NOT NULL CHECK (repository_name <> ''),
    issue_id BIGINT CHECK (issue_id > 0),
    issue_number BIGINT CHECK (issue_number > 0),
    workflow_id UUID,
    headers JSONB NOT NULL DEFAULT '{}'::JSONB,
    payload BYTEA NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'PROCESSING', 'PROCESSED', 'IGNORED', 'FAILED')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    claim_owner TEXT,
    claim_token UUID,
    claimed_at TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    processed_at TIMESTAMPTZ,
    last_error TEXT,
    CONSTRAINT webhook_deliveries_issue_identity_check CHECK (
        (issue_id IS NULL AND issue_number IS NULL)
        OR (issue_id IS NOT NULL AND issue_number IS NOT NULL)
    ),
    CONSTRAINT webhook_deliveries_claim_check CHECK (
        (claim_owner IS NULL AND claim_token IS NULL AND claimed_at IS NULL AND lease_expires_at IS NULL)
        OR (claim_owner IS NOT NULL AND claim_token IS NOT NULL AND claimed_at IS NOT NULL AND lease_expires_at IS NOT NULL)
    )
);

CREATE INDEX webhook_deliveries_claimable_idx
    ON webhook_deliveries (status, lease_expires_at, received_at)
    WHERE status IN ('PENDING', 'PROCESSING');
CREATE INDEX webhook_deliveries_repository_issue_idx
    ON webhook_deliveries (repository_id, issue_id, received_at DESC)
    WHERE issue_id IS NOT NULL;

CREATE TABLE workflows (
    id UUID PRIMARY KEY,
    repository_id BIGINT NOT NULL CHECK (repository_id > 0),
    repository_owner TEXT NOT NULL CHECK (repository_owner <> ''),
    repository_name TEXT NOT NULL CHECK (repository_name <> ''),
    issue_id BIGINT NOT NULL CHECK (issue_id > 0),
    issue_number BIGINT NOT NULL CHECK (issue_number > 0),
    status TEXT NOT NULL CHECK (status <> ''),
    state_revision BIGINT NOT NULL DEFAULT 1 CHECK (state_revision > 0),
    ready_for_sha TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    closed_at TIMESTAMPTZ,
    human_handoff_reason TEXT,
    UNIQUE (repository_id, issue_id),
    UNIQUE (repository_id, issue_number)
);

ALTER TABLE webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_workflow_fk
    FOREIGN KEY (workflow_id) REFERENCES workflows (id);
CREATE INDEX webhook_deliveries_workflow_idx
    ON webhook_deliveries (workflow_id, received_at)
    WHERE workflow_id IS NOT NULL;

CREATE TABLE workflow_attempts (
    id UUID PRIMARY KEY,
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    trigger_delivery_id UUID REFERENCES webhook_deliveries (delivery_id),
    status TEXT NOT NULL CHECK (status <> ''),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    review_cycles_completed INTEGER NOT NULL DEFAULT 0 CHECK (review_cycles_completed >= 0),
    review_cycle_limit INTEGER NOT NULL DEFAULT 3 CHECK (review_cycle_limit > 0),
    infrastructure_failures INTEGER NOT NULL DEFAULT 0 CHECK (infrastructure_failures >= 0),
    infrastructure_failure_limit INTEGER NOT NULL DEFAULT 2 CHECK (infrastructure_failure_limit > 0),
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at TIMESTAMPTZ,
    human_handoff_reason TEXT,
    CONSTRAINT workflow_attempts_review_budget_check
        CHECK (review_cycles_completed <= review_cycle_limit),
    CONSTRAINT workflow_attempts_failure_budget_check
        CHECK (infrastructure_failures <= infrastructure_failure_limit),
    CONSTRAINT workflow_attempts_active_check CHECK (NOT active OR completed_at IS NULL),
    UNIQUE (workflow_id, attempt_number)
);

CREATE UNIQUE INDEX workflow_attempts_one_active_idx
    ON workflow_attempts (workflow_id)
    WHERE active;
CREATE INDEX workflow_attempts_trigger_delivery_idx
    ON workflow_attempts (trigger_delivery_id)
    WHERE trigger_delivery_id IS NOT NULL;

CREATE TABLE agent_assignments (
    id UUID PRIMARY KEY,
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    role TEXT NOT NULL CHECK (role IN ('DEVELOPER', 'REVIEWER')),
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0),
    status TEXT NOT NULL
        CHECK (status IN ('ACTIVE', 'WAITING_FOR_HUMAN', 'COMPLETED', 'SUPERSEDED')),
    agent_profile_name TEXT NOT NULL CHECK (agent_profile_name <> ''),
    runtime_profile_name TEXT NOT NULL CHECK (runtime_profile_name <> ''),
    runtime_profile_version TEXT NOT NULL CHECK (runtime_profile_version <> ''),
    runtime_image_digest TEXT NOT NULL CHECK (runtime_image_digest <> ''),
    github_app_installation_id BIGINT CHECK (github_app_installation_id > 0),
    github_app_actor_id BIGINT CHECK (github_app_actor_id > 0),
    runtime_state_path TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at TIMESTAMPTZ,
    retention_until TIMESTAMPTZ,
    state_deleted_at TIMESTAMPTZ,
    UNIQUE (workflow_id, role, generation)
);

CREATE UNIQUE INDEX agent_assignments_current_role_idx
    ON agent_assignments (workflow_id, role)
    WHERE status <> 'SUPERSEDED' AND state_deleted_at IS NULL;
CREATE INDEX agent_assignments_retention_idx
    ON agent_assignments (retention_until)
    WHERE retention_until IS NOT NULL AND state_deleted_at IS NULL;

CREATE TABLE agent_sessions (
    id UUID PRIMARY KEY,
    agent_assignment_id UUID NOT NULL REFERENCES agent_assignments (id),
    session_number INTEGER NOT NULL CHECK (session_number > 0),
    acp_session_id TEXT,
    runtime_profile_name TEXT NOT NULL CHECK (runtime_profile_name <> ''),
    runtime_profile_version TEXT NOT NULL CHECK (runtime_profile_version <> ''),
    runtime_image_digest TEXT NOT NULL CHECK (runtime_image_digest <> ''),
    runtime_state_path TEXT NOT NULL CHECK (runtime_state_path <> ''),
    capabilities JSONB NOT NULL DEFAULT '{}'::JSONB,
    status TEXT NOT NULL
        CHECK (status IN ('CREATING', 'ACTIVE', 'RETAINED', 'DELETED', 'FAILED')),
    control_owner TEXT NOT NULL DEFAULT 'AUTOMATION'
        CHECK (control_owner IN ('AUTOMATION', 'HUMAN')),
    control_revision BIGINT NOT NULL DEFAULT 1 CHECK (control_revision > 0),
    controller_id TEXT,
    control_acquired_at TIMESTAMPTZ,
    next_execution_epoch BIGINT NOT NULL DEFAULT 1 CHECK (next_execution_epoch > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    activated_at TIMESTAMPTZ,
    retained_at TIMESTAMPTZ,
    state_deleted_at TIMESTAMPTZ,
    CONSTRAINT agent_sessions_active_identifier_check
        CHECK (status NOT IN ('ACTIVE', 'RETAINED') OR acp_session_id IS NOT NULL),
    CONSTRAINT agent_sessions_deleted_check
        CHECK ((status = 'DELETED') = (state_deleted_at IS NOT NULL)),
    UNIQUE (agent_assignment_id, session_number)
);

CREATE UNIQUE INDEX agent_sessions_current_idx
    ON agent_sessions (agent_assignment_id)
    WHERE status IN ('CREATING', 'ACTIVE', 'RETAINED');
CREATE UNIQUE INDEX agent_sessions_acp_identifier_idx
    ON agent_sessions (agent_assignment_id, acp_session_id)
    WHERE acp_session_id IS NOT NULL;

CREATE TABLE agent_turns (
    id UUID PRIMARY KEY,
    agent_session_id UUID NOT NULL REFERENCES agent_sessions (id),
    workflow_attempt_id UUID NOT NULL REFERENCES workflow_attempts (id),
    turn_number BIGINT NOT NULL CHECK (turn_number > 0),
    execution_epoch BIGINT NOT NULL CHECK (execution_epoch > 0),
    retry_of_turn_id UUID REFERENCES agent_turns (id),
    status TEXT NOT NULL CHECK (status IN (
        'QUEUED', 'STARTING', 'RUNNING', 'CANCELLING',
        'SUCCEEDED', 'FAILED', 'INTERRUPTED', 'TIMED_OUT'
    )),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    control_revision BIGINT NOT NULL CHECK (control_revision > 0),
    agent_profile_commit_sha TEXT NOT NULL CHECK (agent_profile_commit_sha <> ''),
    agent_profile_content_sha256 BYTEA NOT NULL
        CHECK (octet_length(agent_profile_content_sha256) = 32),
    agent_profile_config JSONB NOT NULL DEFAULT '{}'::JSONB,
    owner_id TEXT,
    owner_token UUID,
    leased_at TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    mutation_admission_open BOOLEAN NOT NULL DEFAULT FALSE,
    mutation_admission_closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    outcome JSONB,
    last_error TEXT,
    CONSTRAINT agent_turns_active_check CHECK (NOT active OR completed_at IS NULL),
    CONSTRAINT agent_turns_owner_check CHECK (
        (owner_id IS NULL AND owner_token IS NULL AND leased_at IS NULL AND lease_expires_at IS NULL)
        OR (owner_id IS NOT NULL AND owner_token IS NOT NULL AND leased_at IS NOT NULL AND lease_expires_at IS NOT NULL)
    ),
    CONSTRAINT agent_turns_retry_check CHECK (retry_of_turn_id IS NULL OR retry_of_turn_id <> id),
    UNIQUE (agent_session_id, turn_number),
    UNIQUE (agent_session_id, execution_epoch),
    UNIQUE (id, execution_epoch)
);

CREATE UNIQUE INDEX agent_turns_one_active_idx
    ON agent_turns (agent_session_id)
    WHERE active;
CREATE INDEX agent_turns_attempt_idx
    ON agent_turns (workflow_attempt_id, created_at);
CREATE INDEX agent_turns_expired_lease_idx
    ON agent_turns (lease_expires_at)
    WHERE active AND lease_expires_at IS NOT NULL;

CREATE TABLE change_proposals (
    id UUID PRIMARY KEY,
    workflow_id UUID NOT NULL REFERENCES workflows (id),
    created_by_turn_id UUID REFERENCES agent_turns (id),
    repository_id BIGINT NOT NULL CHECK (repository_id > 0),
    repository_owner TEXT NOT NULL CHECK (repository_owner <> ''),
    repository_name TEXT NOT NULL CHECK (repository_name <> ''),
    pull_request_id BIGINT NOT NULL CHECK (pull_request_id > 0),
    pull_request_number BIGINT NOT NULL CHECK (pull_request_number > 0),
    pull_request_node_id TEXT,
    status TEXT NOT NULL CHECK (status <> ''),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    base_ref TEXT NOT NULL CHECK (base_ref <> ''),
    base_sha TEXT NOT NULL CHECK (base_sha <> ''),
    head_ref TEXT NOT NULL CHECK (head_ref <> ''),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    ready_for_sha TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    closed_at TIMESTAMPTZ,
    UNIQUE (repository_id, pull_request_id),
    UNIQUE (repository_id, pull_request_number)
);

CREATE UNIQUE INDEX change_proposals_one_active_idx
    ON change_proposals (workflow_id)
    WHERE active;
CREATE INDEX change_proposals_head_idx
    ON change_proposals (repository_id, head_sha);

CREATE TABLE tool_invocations (
    id UUID PRIMARY KEY,
    agent_turn_id UUID NOT NULL,
    execution_epoch BIGINT NOT NULL CHECK (execution_epoch > 0),
    invocation_number BIGINT NOT NULL CHECK (invocation_number > 0),
    tool_name TEXT NOT NULL CHECK (tool_name <> ''),
    kind TEXT NOT NULL CHECK (kind IN ('READ', 'MUTATION')),
    state TEXT NOT NULL CHECK (state IN (
        'RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'SUCCEEDED',
        'FAILED', 'RECONCILING'
    )),
    idempotency_key TEXT,
    request JSONB NOT NULL DEFAULT '{}'::JSONB,
    result JSONB,
    external_service TEXT,
    external_resource_id TEXT,
    expected_sha TEXT,
    admitted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    duration_ms BIGINT CHECK (duration_ms >= 0),
    last_error TEXT,
    CONSTRAINT tool_invocations_turn_epoch_fk
        FOREIGN KEY (agent_turn_id, execution_epoch)
        REFERENCES agent_turns (id, execution_epoch),
    UNIQUE (agent_turn_id, invocation_number)
);

CREATE UNIQUE INDEX tool_invocations_idempotency_idx
    ON tool_invocations (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX tool_invocations_unsettled_mutations_idx
    ON tool_invocations (agent_turn_id, execution_epoch, state)
    WHERE kind = 'MUTATION' AND state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING');

CREATE TABLE jobs (
    id UUID PRIMARY KEY,
    queue TEXT NOT NULL DEFAULT 'default' CHECK (queue <> ''),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload JSONB NOT NULL DEFAULT '{}'::JSONB,
    status TEXT NOT NULL DEFAULT 'AVAILABLE'
        CHECK (status IN ('AVAILABLE', 'LEASED', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
    priority INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts > 0),
    idempotency_key TEXT,
    workflow_id UUID REFERENCES workflows (id),
    workflow_attempt_id UUID REFERENCES workflow_attempts (id),
    agent_assignment_id UUID REFERENCES agent_assignments (id),
    agent_session_id UUID REFERENCES agent_sessions (id),
    agent_turn_id UUID,
    execution_epoch BIGINT,
    lease_owner TEXT,
    lease_token UUID,
    leased_at TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at TIMESTAMPTZ,
    last_error TEXT,
    CONSTRAINT jobs_attempt_limit_check CHECK (attempt_count <= max_attempts),
    CONSTRAINT jobs_lease_check CHECK (
        (status = 'LEASED' AND lease_owner IS NOT NULL AND lease_token IS NOT NULL
            AND leased_at IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'LEASED' AND lease_owner IS NULL AND lease_token IS NULL
            AND leased_at IS NULL AND lease_expires_at IS NULL)
    ),
    CONSTRAINT jobs_turn_epoch_fk
        FOREIGN KEY (agent_turn_id, execution_epoch)
        REFERENCES agent_turns (id, execution_epoch) MATCH FULL
);

CREATE UNIQUE INDEX jobs_idempotency_idx
    ON jobs (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX jobs_available_idx
    ON jobs (queue, priority DESC, available_at, id)
    WHERE status = 'AVAILABLE';
CREATE INDEX jobs_expired_lease_idx
    ON jobs (lease_expires_at)
    WHERE status = 'LEASED';
CREATE INDEX jobs_workflow_idx
    ON jobs (workflow_id, status, created_at)
    WHERE workflow_id IS NOT NULL;
