-- Supported release baseline. Subsequent schema changes use forward-only migrations.

CREATE FUNCTION public.enforce_repository_agent_profile_role() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    profile_repository_id BIGINT;
BEGIN
    SELECT repository_id INTO STRICT profile_repository_id
    FROM workflows
    WHERE id = NEW.workflow_id
    FOR SHARE;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        profile_repository_id::text || ':' || NEW.agent_profile_name, 0
    ));
    IF EXISTS (
        SELECT 1
        FROM agent_assignments AS participant
        JOIN workflows AS workflow ON workflow.id = participant.workflow_id
        WHERE workflow.repository_id = profile_repository_id
          AND participant.agent_profile_name = NEW.agent_profile_name
          AND participant.role <> NEW.role
          AND participant.id <> NEW.id
    ) THEN
        RAISE EXCEPTION 'Agent Profile % belongs to another Role in repository %',
            NEW.agent_profile_name, profile_repository_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.enforce_workflow_repository_agent_profile_roles() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    profile_name TEXT;
BEGIN
    IF NEW.repository_id = OLD.repository_id THEN
        RETURN NEW;
    END IF;

    FOR profile_name IN
        SELECT DISTINCT agent_profile_name
        FROM agent_assignments
        WHERE workflow_id = NEW.id
        ORDER BY agent_profile_name
    LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(
            NEW.repository_id::text || ':' || profile_name, 0
        ));
    END LOOP;

    IF EXISTS (
        SELECT 1
        FROM agent_assignments AS moved
        JOIN agent_assignments AS existing
          ON existing.agent_profile_name = moved.agent_profile_name
         AND existing.role <> moved.role
        JOIN workflows AS existing_workflow ON existing_workflow.id = existing.workflow_id
        WHERE moved.workflow_id = NEW.id
          AND existing.workflow_id <> NEW.id
          AND existing_workflow.repository_id = NEW.repository_id
    ) THEN
        RAISE EXCEPTION 'Workflow Agent Profiles conflict with repository %', NEW.repository_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.initialize_participant_state_binding() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO retained_runtime_states (agent_participant_id, format, metadata)
    VALUES (NEW.id, 'pi-session/v3', jsonb_build_object('version', 1, 'subpath', NEW.runtime_state_path));
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.protect_agent_participant_binding() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.workflow_id IS DISTINCT FROM OLD.workflow_id
       OR NEW.generation IS DISTINCT FROM OLD.generation
       OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.agent_profile_name IS DISTINCT FROM OLD.agent_profile_name
       OR NEW.runtime_profile_name IS DISTINCT FROM OLD.runtime_profile_name
       OR NEW.runtime_profile_version IS DISTINCT FROM OLD.runtime_profile_version
       OR NEW.runtime_profile_content_sha256 IS DISTINCT FROM OLD.runtime_profile_content_sha256
       OR NEW.runtime_image_digest IS DISTINCT FROM OLD.runtime_image_digest
       OR NEW.runtime_state_path IS DISTINCT FROM OLD.runtime_state_path
       OR NEW.created_by_preparation_job_id IS DISTINCT FROM OLD.created_by_preparation_job_id THEN
        RAISE EXCEPTION 'Agent Participant identity and Runtime binding are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.protect_execution_backend_binding() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'Execution Backend binding is immutable';
    END IF;
    IF NEW.family IS DISTINCT FROM OLD.family
       OR NEW.contract_version IS DISTINCT FROM OLD.contract_version
       OR NEW.namespace IS DISTINCT FROM OLD.namespace THEN
        RAISE EXCEPTION 'Execution Backend binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.protect_preparation_only_cleanup_proof() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.preparation_only_cleanup THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'Preparation-only cleanup proof is immutable';
        END IF;
        IF NOT NEW.preparation_only_cleanup
           OR NEW.agent_turn_id IS DISTINCT FROM OLD.agent_turn_id
           OR NEW.execution_epoch IS DISTINCT FROM OLD.execution_epoch
           OR NEW.identity IS DISTINCT FROM OLD.identity THEN
            RAISE EXCEPTION 'Preparation-only cleanup proof is immutable';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.protect_retained_state_binding() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.agent_participant_id IS DISTINCT FROM OLD.agent_participant_id
       OR NEW.format IS DISTINCT FROM OLD.format
       OR NEW.metadata IS DISTINCT FROM OLD.metadata
       OR (OLD.initialized AND NOT NEW.initialized) THEN
        RAISE EXCEPTION 'Retained Runtime State binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.protect_runtime_session_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (OLD.runtime_session_id IS NOT NULL AND NEW.runtime_session_id IS DISTINCT FROM OLD.runtime_session_id)
       OR (OLD.runtime_capabilities IS NOT NULL AND NEW.runtime_capabilities IS DISTINCT FROM OLD.runtime_capabilities) THEN
        RAISE EXCEPTION 'Runtime Agent Session identity and capabilities are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION public.reject_runtime_profile_compatibility_result_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'Runtime Profile compatibility results are immutable';
END;
$$;

CREATE FUNCTION public.reject_runtime_profile_contract_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'Runtime Profile contracts are immutable';
END;
$$;

CREATE FUNCTION public.reject_stage_assignment_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'Stage Assignments are immutable';
END;
$$;

CREATE FUNCTION public.reject_workspace_capture_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'Workspace Capture identity and digest are immutable';
END;
$$;

CREATE FUNCTION public.set_agent_session_workflow() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    participant_workflow_id UUID;
BEGIN
    SELECT workflow_id INTO participant_workflow_id
    FROM agent_assignments
    WHERE id = NEW.agent_assignment_id;

    IF participant_workflow_id IS NULL THEN
        RAISE EXCEPTION 'Agent Participant does not exist';
    END IF;
    IF NEW.workflow_id IS NOT NULL AND NEW.workflow_id <> participant_workflow_id THEN
        RAISE EXCEPTION 'Agent Session Workflow does not match Agent Participant';
    END IF;
    NEW.workflow_id = participant_workflow_id;
    RETURN NEW;
END;
$$;

CREATE TABLE public.agent_assignments (
    id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    role text NOT NULL,
    generation integer DEFAULT 1 NOT NULL,
    status text NOT NULL,
    agent_profile_name text NOT NULL,
    runtime_profile_name text NOT NULL,
    runtime_profile_version text NOT NULL,
    runtime_image_digest text NOT NULL,
    github_app_installation_id bigint,
    github_app_actor_id bigint,
    runtime_state_path text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    completed_at timestamp with time zone,
    retention_until timestamp with time zone,
    state_deleted_at timestamp with time zone,
    created_by_preparation_job_id uuid,
    reactivated_by_preparation_job_id uuid,
    runtime_profile_content_sha256 text NOT NULL,
    CONSTRAINT agent_assignments_agent_profile_name_check CHECK ((agent_profile_name <> ''::text)),
    CONSTRAINT agent_assignments_generation_check CHECK ((generation > 0)),
    CONSTRAINT agent_assignments_github_app_actor_id_check CHECK ((github_app_actor_id > 0)),
    CONSTRAINT agent_assignments_github_app_installation_id_check CHECK ((github_app_installation_id > 0)),
    CONSTRAINT agent_assignments_runtime_image_digest_check CHECK ((runtime_image_digest <> ''::text)),
    CONSTRAINT agent_assignments_runtime_profile_content_sha256_check CHECK ((runtime_profile_content_sha256 ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT agent_assignments_runtime_profile_name_check CHECK ((runtime_profile_name = 'pi-rpc'::text)),
    CONSTRAINT agent_assignments_runtime_profile_version_check CHECK ((runtime_profile_version = 'v2'::text)),
    CONSTRAINT agent_assignments_runtime_state_path_check CHECK ((runtime_state_path <> ''::text)),
    CONSTRAINT agent_assignments_status_check CHECK ((status = ANY (ARRAY['ACTIVE'::text, 'WAITING_FOR_HUMAN'::text, 'COMPLETED'::text, 'SUPERSEDED'::text]))),
    CONSTRAINT agent_participants_role_syntax_check CHECK ((role ~ '^[A-Z][A-Z0-9_]{0,63}$'::text))
);

CREATE VIEW public.agent_participants AS
 SELECT id,
    workflow_id,
    generation AS assignment_generation,
    role,
    agent_profile_name,
    runtime_profile_name,
    runtime_profile_version,
    runtime_profile_content_sha256,
    runtime_image_digest,
    github_app_installation_id,
    github_app_actor_id,
    runtime_state_path,
    status,
    created_by_preparation_job_id,
    reactivated_by_preparation_job_id,
    created_at,
    updated_at,
    completed_at,
    retention_until,
    state_deleted_at
   FROM public.agent_assignments;

CREATE TABLE public.agent_sessions (
    id uuid NOT NULL,
    agent_assignment_id uuid NOT NULL,
    session_number integer NOT NULL,
    runtime_profile_name text NOT NULL,
    runtime_profile_version text NOT NULL,
    runtime_image_digest text NOT NULL,
    runtime_state_path text NOT NULL,
    status text NOT NULL,
    control_owner text DEFAULT 'AUTOMATION'::text NOT NULL,
    control_revision bigint DEFAULT 1 NOT NULL,
    controller_id text,
    control_acquired_at timestamp with time zone,
    next_execution_epoch bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    activated_at timestamp with time zone,
    retained_at timestamp with time zone,
    state_deleted_at timestamp with time zone,
    next_turn_number bigint DEFAULT 1 NOT NULL,
    runtime_profile_content_sha256 text NOT NULL,
    human_prompt_token uuid,
    human_prompt_leased_at timestamp with time zone,
    human_prompt_lease_expires_at timestamp with time zone,
    human_prompt_heartbeat_at timestamp with time zone,
    workflow_id uuid NOT NULL,
    runtime_session_id text,
    runtime_capabilities jsonb,
    CONSTRAINT agent_sessions_active_identifier_check CHECK (((status <> 'ACTIVE'::text) OR ((runtime_session_id IS NOT NULL) AND (runtime_capabilities IS NOT NULL)))),
    CONSTRAINT agent_sessions_control_metadata_check CHECK ((((controller_id IS NULL) AND (control_acquired_at IS NULL)) OR ((controller_id IS NOT NULL) AND (controller_id <> ''::text) AND (control_acquired_at IS NOT NULL)))),
    CONSTRAINT agent_sessions_control_owner_check CHECK ((control_owner = ANY (ARRAY['AUTOMATION'::text, 'HUMAN'::text]))),
    CONSTRAINT agent_sessions_control_revision_check CHECK ((control_revision > 0)),
    CONSTRAINT agent_sessions_deleted_check CHECK (((status = 'DELETED'::text) = (state_deleted_at IS NOT NULL))),
    CONSTRAINT agent_sessions_human_prompt_lease_check CHECK ((((human_prompt_token IS NULL) AND (human_prompt_leased_at IS NULL) AND (human_prompt_lease_expires_at IS NULL) AND (human_prompt_heartbeat_at IS NULL)) OR ((human_prompt_token IS NOT NULL) AND (human_prompt_leased_at IS NOT NULL) AND (human_prompt_lease_expires_at IS NOT NULL) AND (human_prompt_heartbeat_at IS NOT NULL) AND (control_owner = 'HUMAN'::text) AND (status = ANY (ARRAY['ACTIVE'::text, 'RETAINED'::text])) AND (human_prompt_lease_expires_at > human_prompt_leased_at) AND (human_prompt_heartbeat_at >= human_prompt_leased_at) AND (human_prompt_lease_expires_at > human_prompt_heartbeat_at)))),
    CONSTRAINT agent_sessions_next_execution_epoch_check CHECK ((next_execution_epoch > 0)),
    CONSTRAINT agent_sessions_next_turn_number_check CHECK ((next_turn_number > 0)),
    CONSTRAINT agent_sessions_runtime_capabilities_check CHECK (((runtime_capabilities IS NULL) OR ((runtime_session_id IS NOT NULL) AND (jsonb_typeof(runtime_capabilities) = 'object'::text)))),
    CONSTRAINT agent_sessions_runtime_identity_check CHECK (((runtime_session_id IS NULL) OR ((runtime_profile_name = 'pi-rpc'::text) AND (runtime_profile_version = 'v2'::text) AND (runtime_session_id = (id)::text)))),
    CONSTRAINT agent_sessions_runtime_image_digest_check CHECK ((runtime_image_digest <> ''::text)),
    CONSTRAINT agent_sessions_runtime_profile_content_sha256_check CHECK ((runtime_profile_content_sha256 ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT agent_sessions_runtime_profile_name_check CHECK ((runtime_profile_name = 'pi-rpc'::text)),
    CONSTRAINT agent_sessions_runtime_profile_version_check CHECK ((runtime_profile_version = 'v2'::text)),
    CONSTRAINT agent_sessions_runtime_state_path_check CHECK ((runtime_state_path <> ''::text)),
    CONSTRAINT agent_sessions_session_number_check CHECK ((session_number > 0)),
    CONSTRAINT agent_sessions_status_check CHECK ((status = ANY (ARRAY['CREATING'::text, 'ACTIVE'::text, 'RETAINED'::text, 'DELETED'::text, 'FAILED'::text])))
);

CREATE TABLE public.agent_turn_corroborations (
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    workflow_id uuid NOT NULL,
    workflow_attempt_id uuid NOT NULL,
    source_invocation_id uuid NOT NULL,
    execution_job_id uuid NOT NULL,
    execution_owner_id text NOT NULL,
    execution_owner_token_sha256 bytea NOT NULL,
    prompt_outcome jsonb,
    prompt_error text,
    pending_since timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    state text NOT NULL,
    last_failure_code text,
    verification_job_id uuid NOT NULL,
    resolved_at timestamp with time zone,
    CONSTRAINT agent_turn_corroborations_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT agent_turn_corroborations_execution_owner_id_check CHECK ((execution_owner_id <> ''::text)),
    CONSTRAINT agent_turn_corroborations_execution_owner_token_sha256_check CHECK ((octet_length(execution_owner_token_sha256) = 32)),
    CONSTRAINT agent_turn_corroborations_prompt_check CHECK ((((prompt_outcome IS NOT NULL) AND (prompt_error IS NULL) AND (jsonb_typeof(prompt_outcome) = 'object'::text)) OR ((prompt_outcome IS NULL) AND (prompt_error IS NOT NULL)))),
    CONSTRAINT agent_turn_corroborations_prompt_error_check CHECK ((prompt_error = ANY (ARRAY['FAILURE'::text, 'DEADLINE'::text]))),
    CONSTRAINT agent_turn_corroborations_resolution_check CHECK ((((state = 'PENDING'::text) AND (resolved_at IS NULL)) OR ((state <> 'PENDING'::text) AND (resolved_at IS NOT NULL)))),
    CONSTRAINT agent_turn_corroborations_state_check CHECK ((state = ANY (ARRAY['PENDING'::text, 'VERIFIED'::text, 'HANDED_OFF'::text, 'CANCELLED'::text])))
);

CREATE TABLE public.agent_turn_publications (
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    head_ref text NOT NULL,
    head_sha text NOT NULL,
    base_ref text NOT NULL,
    pull_request_id bigint,
    pull_request_number bigint,
    pull_request_node_id text,
    source_publish_mutation_id uuid NOT NULL,
    source_open_pr_mutation_id uuid,
    bound_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT agent_turn_publications_base_ref_check CHECK ((base_ref <> ''::text)),
    CONSTRAINT agent_turn_publications_check CHECK ((((pull_request_id IS NULL) AND (pull_request_number IS NULL) AND (pull_request_node_id IS NULL) AND (source_open_pr_mutation_id IS NULL)) OR ((pull_request_id IS NOT NULL) AND (pull_request_id > 0) AND (pull_request_number IS NOT NULL) AND (pull_request_number > 0) AND (pull_request_node_id IS NOT NULL) AND (pull_request_node_id <> ''::text) AND (source_open_pr_mutation_id IS NOT NULL)))),
    CONSTRAINT agent_turn_publications_head_ref_check CHECK ((head_ref <> ''::text)),
    CONSTRAINT agent_turn_publications_head_sha_check CHECK (((head_sha ~ '^[0-9a-f]+$'::text) AND (char_length(head_sha) = ANY (ARRAY[40, 64]))))
);

CREATE TABLE public.agent_turn_settlements (
    id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    workflow_attempt_id uuid NOT NULL,
    agent_assignment_id uuid NOT NULL,
    agent_session_id uuid NOT NULL,
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    control_revision bigint NOT NULL,
    authority text NOT NULL,
    execution_job_id uuid NOT NULL,
    job_attempt_number integer NOT NULL,
    job_lease_owner text NOT NULL,
    job_lease_token uuid NOT NULL,
    owner_id text NOT NULL,
    owner_token_sha256 bytea NOT NULL,
    recovery_job_id uuid,
    recovery_job_kind text,
    recovery_job_attempt_number integer,
    recovery_job_lease_owner text,
    recovery_job_lease_token uuid,
    observation jsonb NOT NULL,
    observation_sha256 bytea NOT NULL,
    workflow_outcome text NOT NULL,
    terminal_turn_status text NOT NULL,
    terminal_turn_outcome jsonb,
    terminal_last_error text,
    pending_event_count integer NOT NULL,
    latest_observed_head_sha text,
    disposition text NOT NULL,
    reason text NOT NULL,
    workflow_state text NOT NULL,
    workflow_revision bigint NOT NULL,
    change_proposal_id uuid,
    successor_job_id uuid,
    reconciliation_job_id uuid,
    result jsonb,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    settled_at timestamp with time zone,
    CONSTRAINT agent_turn_settlements_authority_check CHECK ((authority = ANY (ARRAY['LIVE'::text, 'RECOVERY'::text]))),
    CONSTRAINT agent_turn_settlements_authority_shape_check CHECK ((((authority = 'LIVE'::text) AND (recovery_job_id IS NULL) AND (recovery_job_kind IS NULL) AND (recovery_job_attempt_number IS NULL) AND (recovery_job_lease_owner IS NULL) AND (recovery_job_lease_token IS NULL)) OR ((authority = 'RECOVERY'::text) AND (recovery_job_id IS NOT NULL) AND (recovery_job_kind = ANY (ARRAY['STOP_STALE_RUNTIME'::text, 'RECONCILE_AGENT_TURN_MUTATIONS'::text, 'VERIFY_TERMINAL_INTENT'::text])) AND (recovery_job_attempt_number IS NOT NULL) AND (recovery_job_lease_owner IS NOT NULL) AND (recovery_job_lease_token IS NOT NULL)))),
    CONSTRAINT agent_turn_settlements_completion_check CHECK ((((settled_at IS NULL) AND (result IS NULL) AND (successor_job_id IS NULL) AND (reconciliation_job_id IS NULL)) OR ((settled_at IS NOT NULL) AND (result IS NOT NULL) AND (NOT ((successor_job_id IS NOT NULL) AND (reconciliation_job_id IS NOT NULL)))))),
    CONSTRAINT agent_turn_settlements_control_revision_check CHECK ((control_revision > 0)),
    CONSTRAINT agent_turn_settlements_disposition_check CHECK ((disposition = 'APPLIED'::text)),
    CONSTRAINT agent_turn_settlements_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT agent_turn_settlements_job_attempt_number_check CHECK ((job_attempt_number > 0)),
    CONSTRAINT agent_turn_settlements_job_lease_owner_check CHECK ((job_lease_owner <> ''::text)),
    CONSTRAINT agent_turn_settlements_observation_check CHECK ((jsonb_typeof(observation) = 'object'::text)),
    CONSTRAINT agent_turn_settlements_observation_sha256_check CHECK ((octet_length(observation_sha256) = 32)),
    CONSTRAINT agent_turn_settlements_owner_id_check CHECK ((owner_id <> ''::text)),
    CONSTRAINT agent_turn_settlements_owner_token_sha256_check CHECK ((octet_length(owner_token_sha256) = 32)),
    CONSTRAINT agent_turn_settlements_pending_event_count_check CHECK ((pending_event_count >= 0)),
    CONSTRAINT agent_turn_settlements_reason_check CHECK ((reason <> ''::text)),
    CONSTRAINT agent_turn_settlements_recovery_job_attempt_number_check CHECK ((recovery_job_attempt_number > 0)),
    CONSTRAINT agent_turn_settlements_recovery_job_lease_owner_check CHECK ((recovery_job_lease_owner <> ''::text)),
    CONSTRAINT agent_turn_settlements_terminal_turn_status_check CHECK ((terminal_turn_status = ANY (ARRAY['SUCCEEDED'::text, 'FAILED'::text, 'INTERRUPTED'::text, 'TIMED_OUT'::text]))),
    CONSTRAINT agent_turn_settlements_workflow_outcome_check CHECK ((workflow_outcome = ANY (ARRAY['CHANGE_PROPOSAL_READY'::text, 'CHANGES_REQUESTED'::text, 'APPROVED'::text, 'BLOCKED'::text, 'INFRASTRUCTURE_FAILED'::text]))),
    CONSTRAINT agent_turn_settlements_workflow_revision_check CHECK ((workflow_revision > 0)),
    CONSTRAINT agent_turn_settlements_workflow_state_check CHECK ((workflow_state <> ''::text))
);

CREATE TABLE public.agent_turn_slots (
    agent_turn_id uuid NOT NULL,
    agent_session_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    control_revision bigint NOT NULL,
    owner_id text NOT NULL,
    owner_token uuid NOT NULL,
    lease_expires_at timestamp with time zone NOT NULL,
    acquired_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT agent_turn_slots_control_revision_check CHECK ((control_revision > 0)),
    CONSTRAINT agent_turn_slots_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT agent_turn_slots_owner_id_check CHECK ((owner_id <> ''::text))
);

CREATE TABLE public.agent_turns (
    id uuid NOT NULL,
    agent_session_id uuid NOT NULL,
    workflow_attempt_id uuid NOT NULL,
    turn_number bigint NOT NULL,
    execution_epoch bigint NOT NULL,
    retry_of_turn_id uuid,
    status text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    control_revision bigint NOT NULL,
    agent_profile_commit_sha text NOT NULL,
    agent_profile_content_sha256 bytea NOT NULL,
    agent_profile_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    owner_id text,
    owner_token uuid,
    leased_at timestamp with time zone,
    lease_expires_at timestamp with time zone,
    heartbeat_at timestamp with time zone,
    mutation_admission_open boolean DEFAULT false NOT NULL,
    mutation_admission_closed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    outcome jsonb,
    last_error text,
    purpose text NOT NULL,
    change_proposal_id uuid,
    expected_head_sha text,
    next_invocation_number bigint DEFAULT 1 NOT NULL,
    recovery_started_at timestamp with time zone,
    runtime_stop_required boolean DEFAULT false NOT NULL,
    runtime_stopped_at timestamp with time zone,
    recovery_settled_at timestamp with time zone,
    stop_runtime_job_id uuid,
    reconcile_mutations_job_id uuid,
    workflow_id uuid NOT NULL,
    preparation_job_id uuid,
    operation_lineage_id uuid NOT NULL,
    recovery_original_owner_id text,
    recovery_original_owner_token_sha256 bytea,
    recovery_continuation text,
    recovery_settlement_id uuid,
    stage_id text DEFAULT 'implementation'::text NOT NULL,
    runtime_oom_resource_ref text,
    runtime_oom_exit_code integer,
    runtime_oom_observed_at timestamp with time zone,
    turn_configuration jsonb DEFAULT '{}'::jsonb NOT NULL,
    prompt_stop_reason text,
    prompt_error_class text,
    prompt_recorded_at timestamp with time zone,
    CONSTRAINT agent_turns_active_check CHECK (((NOT active) OR (completed_at IS NULL))),
    CONSTRAINT agent_turns_agent_profile_commit_sha_check CHECK ((agent_profile_commit_sha <> ''::text)),
    CONSTRAINT agent_turns_agent_profile_content_sha256_check CHECK ((octet_length(agent_profile_content_sha256) = 32)),
    CONSTRAINT agent_turns_control_revision_check CHECK ((control_revision > 0)),
    CONSTRAINT agent_turns_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT agent_turns_expected_proposal_check CHECK ((((change_proposal_id IS NULL) AND (COALESCE(expected_head_sha, ''::text) = ''::text)) OR ((change_proposal_id IS NOT NULL) AND (expected_head_sha IS NOT NULL) AND (expected_head_sha <> ''::text)))),
    CONSTRAINT agent_turns_next_invocation_number_check CHECK ((next_invocation_number > 0)),
    CONSTRAINT agent_turns_operation_lineage_check CHECK ((((retry_of_turn_id IS NULL) AND (operation_lineage_id = id)) OR ((retry_of_turn_id IS NOT NULL) AND (operation_lineage_id <> id)))),
    CONSTRAINT agent_turns_owner_check CHECK ((((owner_id IS NULL) AND (owner_token IS NULL) AND (leased_at IS NULL) AND (lease_expires_at IS NULL)) OR ((owner_id IS NOT NULL) AND (owner_token IS NOT NULL) AND (leased_at IS NOT NULL) AND (lease_expires_at IS NOT NULL)))),
    CONSTRAINT agent_turns_profile_config_object_check CHECK ((jsonb_typeof(agent_profile_config) = 'object'::text)),
    CONSTRAINT agent_turns_prompt_error_class_check CHECK ((prompt_error_class = ANY (ARRAY['FAILURE'::text, 'DEADLINE'::text, 'CANCELLATION'::text, 'INVALID_RESPONSE'::text]))),
    CONSTRAINT agent_turns_prompt_evidence_shape_check CHECK ((((prompt_recorded_at IS NULL) AND (prompt_stop_reason IS NULL) AND (prompt_error_class IS NULL)) OR ((prompt_recorded_at IS NOT NULL) AND (((prompt_stop_reason IS NOT NULL) AND (prompt_error_class IS NULL)) OR ((prompt_stop_reason IS NULL) AND (prompt_error_class IS NOT NULL)))))),
    CONSTRAINT agent_turns_prompt_stop_reason_check CHECK ((prompt_stop_reason = ANY (ARRAY['completed'::text, 'limit_reached'::text, 'aborted'::text]))),
    CONSTRAINT agent_turns_purpose_check CHECK ((purpose = ANY (ARRAY['INITIAL_DEVELOPMENT'::text, 'REVIEW'::text, 'REQUESTED_CHANGES'::text, 'RETRY'::text, 'SYNCHRONIZATION'::text, 'REACTIVATION'::text]))),
    CONSTRAINT agent_turns_recovery_barrier_check CHECK (
        (recovery_started_at IS NULL AND NOT runtime_stop_required AND runtime_stopped_at IS NULL
         AND recovery_settled_at IS NULL AND stop_runtime_job_id IS NULL AND reconcile_mutations_job_id IS NULL
         AND recovery_original_owner_id IS NULL AND recovery_original_owner_token_sha256 IS NULL AND recovery_continuation IS NULL)
        OR (recovery_started_at IS NOT NULL AND runtime_stop_required AND stop_runtime_job_id IS NOT NULL
            AND recovery_continuation IS NOT NULL AND (recovery_settled_at IS NULL OR runtime_stopped_at IS NOT NULL)
            AND recovery_original_owner_id IS NOT NULL AND recovery_original_owner_id <> ''
            AND octet_length(recovery_original_owner_token_sha256) = 32)),
    CONSTRAINT agent_turns_recovery_continuation_check CHECK ((recovery_continuation = ANY (ARRAY['PENDING_INFRASTRUCTURE_FAILURE'::text, 'INFRASTRUCTURE_FAILURE_APPLIED'::text, 'MUTATION_RECONCILIATION_HANDOFF_APPLIED'::text, 'WORKFLOW_DEFINITION_HANDOFF_APPLIED'::text, 'TERMINAL_CORROBORATION_PENDING'::text, 'TERMINAL_CORROBORATION_APPLIED'::text]))),
    CONSTRAINT agent_turns_recovery_continuation_shape_check CHECK ((((recovery_continuation = 'INFRASTRUCTURE_FAILURE_APPLIED'::text) AND (recovery_settlement_id IS NOT NULL)) OR ((recovery_continuation <> 'INFRASTRUCTURE_FAILURE_APPLIED'::text) AND (recovery_settlement_id IS NULL)) OR (recovery_continuation IS NULL))),
    CONSTRAINT agent_turns_retry_check CHECK (((retry_of_turn_id IS NULL) OR (retry_of_turn_id <> id))),
    CONSTRAINT agent_turns_runtime_oom_evidence_check CHECK ((((runtime_oom_resource_ref IS NULL) AND (runtime_oom_exit_code IS NULL) AND (runtime_oom_observed_at IS NULL)) OR ((runtime_oom_resource_ref IS NOT NULL) AND ((length(runtime_oom_resource_ref) >= 1) AND (length(runtime_oom_resource_ref) <= 128)) AND (runtime_oom_exit_code IS NOT NULL) AND (runtime_oom_exit_code >= 0) AND (runtime_oom_observed_at IS NOT NULL)))),
    CONSTRAINT agent_turns_stage_id_check CHECK ((stage_id ~ '^[a-z][a-z0-9_-]{0,63}$'::text)),
    CONSTRAINT agent_turns_status_check CHECK ((status = ANY (ARRAY['QUEUED'::text, 'STARTING'::text, 'RUNNING'::text, 'CANCELLING'::text, 'SETTLING'::text, 'RECONCILING'::text, 'CORROBORATING'::text, 'SUCCEEDED'::text, 'FAILED'::text, 'INTERRUPTED'::text, 'TIMED_OUT'::text]))),
    CONSTRAINT agent_turns_turn_configuration_object CHECK ((jsonb_typeof(turn_configuration) = 'object'::text)),
    CONSTRAINT agent_turns_turn_number_check CHECK ((turn_number > 0))
);

CREATE TABLE public.assignment_retention_generations (
    id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    retention_token text NOT NULL,
    retention_deadline timestamp with time zone NOT NULL,
    workflow_revision bigint NOT NULL,
    status text NOT NULL,
    collection_job_id uuid NOT NULL,
    authorized_attempt_number integer,
    authorized_lease_owner text,
    authorized_lease_token uuid,
    authorized_at timestamp with time zone,
    collected_event_id uuid,
    collected_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT assignment_retention_generation_authorized_attempt_number_check CHECK ((authorized_attempt_number > 0)),
    CONSTRAINT assignment_retention_generations_authorization_check CHECK ((((status = ANY (ARRAY['SCHEDULED'::text, 'CANCELLED'::text])) AND (authorized_attempt_number IS NULL) AND (authorized_lease_owner IS NULL) AND (authorized_lease_token IS NULL) AND (authorized_at IS NULL)) OR ((status = ANY (ARRAY['COLLECTING'::text, 'COLLECTED'::text])) AND (authorized_attempt_number IS NOT NULL) AND (authorized_lease_owner IS NOT NULL) AND (authorized_lease_token IS NOT NULL) AND (authorized_at IS NOT NULL)))),
    CONSTRAINT assignment_retention_generations_authorized_lease_owner_check CHECK ((authorized_lease_owner <> ''::text)),
    CONSTRAINT assignment_retention_generations_outcome_check CHECK ((((status = 'SCHEDULED'::text) AND (collected_event_id IS NULL) AND (collected_at IS NULL) AND (cancelled_at IS NULL)) OR ((status = 'COLLECTING'::text) AND (collected_event_id IS NULL) AND (collected_at IS NULL) AND (cancelled_at IS NULL)) OR ((status = 'COLLECTED'::text) AND (collected_event_id IS NOT NULL) AND (collected_at IS NOT NULL) AND (cancelled_at IS NULL)) OR ((status = 'CANCELLED'::text) AND (collected_event_id IS NULL) AND (collected_at IS NULL) AND (cancelled_at IS NOT NULL)))),
    CONSTRAINT assignment_retention_generations_retention_token_check CHECK ((retention_token <> ''::text)),
    CONSTRAINT assignment_retention_generations_status_check CHECK ((status = ANY (ARRAY['SCHEDULED'::text, 'COLLECTING'::text, 'COLLECTED'::text, 'CANCELLED'::text]))),
    CONSTRAINT assignment_retention_generations_workflow_revision_check CHECK ((workflow_revision > 0))
);

CREATE TABLE public.assignment_retention_targets (
    retention_generation_id uuid NOT NULL,
    assignment_id uuid NOT NULL,
    session_id uuid,
    runtime_state_path text NOT NULL,
    runtime_image_digest text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT assignment_retention_targets_runtime_image_digest_check CHECK ((runtime_image_digest <> ''::text)),
    CONSTRAINT assignment_retention_targets_runtime_state_path_check CHECK ((runtime_state_path <> ''::text))
);

CREATE TABLE public.change_proposal_reviews (
    repository_id bigint NOT NULL,
    review_id bigint NOT NULL,
    review_node_id text NOT NULL,
    change_proposal_id uuid NOT NULL,
    actor_id bigint NOT NULL,
    head_sha text NOT NULL,
    accepted boolean NOT NULL,
    normalized_event_id uuid,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    agent_turn_settlement_id uuid,
    workflow_internal_event_id uuid,
    CONSTRAINT change_proposal_reviews_actor_id_check CHECK ((actor_id > 0)),
    CONSTRAINT change_proposal_reviews_head_sha_check CHECK ((head_sha <> ''::text)),
    CONSTRAINT change_proposal_reviews_provenance_check CHECK ((((((normalized_event_id IS NOT NULL))::integer + ((agent_turn_settlement_id IS NOT NULL))::integer) + ((workflow_internal_event_id IS NOT NULL))::integer) = 1)),
    CONSTRAINT change_proposal_reviews_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT change_proposal_reviews_review_id_check CHECK ((review_id > 0)),
    CONSTRAINT change_proposal_reviews_review_node_id_check CHECK ((review_node_id <> ''::text))
);

CREATE TABLE public.change_proposals (
    id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    created_by_turn_id uuid,
    repository_id bigint NOT NULL,
    repository_owner text NOT NULL,
    repository_name text NOT NULL,
    pull_request_id bigint NOT NULL,
    pull_request_number bigint NOT NULL,
    pull_request_node_id text,
    status text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    base_ref text NOT NULL,
    base_sha text NOT NULL,
    head_ref text NOT NULL,
    head_sha text NOT NULL,
    ready_for_sha text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    closed_at timestamp with time zone,
    CONSTRAINT change_proposals_base_ref_check CHECK ((base_ref <> ''::text)),
    CONSTRAINT change_proposals_base_sha_check CHECK ((base_sha <> ''::text)),
    CONSTRAINT change_proposals_head_ref_check CHECK ((head_ref <> ''::text)),
    CONSTRAINT change_proposals_head_sha_check CHECK ((head_sha <> ''::text)),
    CONSTRAINT change_proposals_pull_request_id_check CHECK ((pull_request_id > 0)),
    CONSTRAINT change_proposals_pull_request_number_check CHECK ((pull_request_number > 0)),
    CONSTRAINT change_proposals_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT change_proposals_repository_name_check CHECK ((repository_name <> ''::text)),
    CONSTRAINT change_proposals_repository_owner_check CHECK ((repository_owner <> ''::text)),
    CONSTRAINT change_proposals_status_check CHECK ((status <> ''::text))
);

CREATE TABLE public.execution_backend_binding (
    singleton boolean DEFAULT true NOT NULL,
    family text NOT NULL,
    contract_version integer NOT NULL,
    namespace text NOT NULL CHECK (namespace <> '' AND namespace = btrim(namespace)),
    CONSTRAINT execution_backend_binding_family_check CHECK (family <> '' AND family = btrim(family)),
    CONSTRAINT execution_backend_binding_contract_version_check CHECK ((contract_version > 0)),
    CONSTRAINT execution_backend_binding_singleton_check CHECK (singleton)
);

CREATE TABLE public.execution_reservations (
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    identity jsonb NOT NULL,
    specification_sha256 text,
    dispatch_phase text DEFAULT 'reserved'::text NOT NULL,
    setup_resource_ref text,
    setup_settled boolean DEFAULT false NOT NULL,
    resource_ref text,
    creation_settled boolean DEFAULT false NOT NULL,
    revoked boolean DEFAULT false NOT NULL,
    preparation_only_cleanup boolean DEFAULT false NOT NULL,
    CONSTRAINT execution_reservations_check CHECK (((specification_sha256 IS NOT NULL) OR revoked)),
    CONSTRAINT execution_reservations_check1 CHECK (((resource_ref IS NULL) OR (dispatch_phase = 'main'::text))),
    CONSTRAINT execution_reservations_check2 CHECK (((setup_resource_ref IS NULL) OR (dispatch_phase = ANY (ARRAY['setup'::text, 'main'::text])))),
    CONSTRAINT execution_reservations_check3 CHECK (((dispatch_phase <> 'main'::text) OR (setup_resource_ref IS NULL) OR setup_settled)),
    CONSTRAINT execution_reservations_dispatch_phase_check CHECK ((dispatch_phase = ANY (ARRAY['reserved'::text, 'setup'::text, 'main'::text]))),
    CONSTRAINT execution_reservations_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT execution_reservations_specification_sha256_check CHECK ((specification_sha256 ~ '^[0-9a-f]{64}$'::text))
);

CREATE TABLE public.job_attempts (
    job_id uuid NOT NULL,
    attempt_number integer NOT NULL,
    lease_owner text NOT NULL,
    lease_token uuid NOT NULL,
    status text NOT NULL,
    leased_at timestamp with time zone NOT NULL,
    lease_expires_at timestamp with time zone NOT NULL,
    heartbeat_at timestamp with time zone,
    finished_at timestamp with time zone,
    retryable boolean,
    result jsonb,
    last_error text,
    CONSTRAINT job_attempts_attempt_number_check CHECK ((attempt_number > 0)),
    CONSTRAINT job_attempts_finished_check CHECK ((((status = 'LEASED'::text) AND (finished_at IS NULL)) OR ((status <> 'LEASED'::text) AND (finished_at IS NOT NULL)))),
    CONSTRAINT job_attempts_lease_owner_check CHECK ((lease_owner <> ''::text)),
    CONSTRAINT job_attempts_outcome_check CHECK ((((status = 'LEASED'::text) AND (retryable IS NULL) AND (result IS NULL) AND (last_error IS NULL)) OR ((status = 'SUCCEEDED'::text) AND (retryable IS NULL) AND (result IS NOT NULL) AND (last_error IS NULL)) OR ((status = ANY (ARRAY['FAILED'::text, 'EXPIRED'::text])) AND (retryable IS NOT NULL) AND (result IS NULL) AND (last_error IS NOT NULL)))),
    CONSTRAINT job_attempts_status_check CHECK ((status = ANY (ARRAY['LEASED'::text, 'SUCCEEDED'::text, 'FAILED'::text, 'EXPIRED'::text])))
);

CREATE TABLE public.job_normalized_events (
    job_id uuid NOT NULL,
    normalized_event_id uuid NOT NULL
);

CREATE TABLE public.jobs (
    id uuid NOT NULL,
    queue text DEFAULT 'default'::text NOT NULL,
    kind text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'AVAILABLE'::text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    available_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    max_attempts integer NOT NULL,
    idempotency_key text,
    workflow_id uuid,
    workflow_attempt_id uuid,
    agent_assignment_id uuid,
    agent_session_id uuid,
    agent_turn_id uuid,
    execution_epoch bigint,
    lease_owner text,
    lease_token uuid,
    leased_at timestamp with time zone,
    lease_expires_at timestamp with time zone,
    heartbeat_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    completed_at timestamp with time zone,
    last_error text,
    enqueue_delay_microseconds bigint DEFAULT 0 NOT NULL,
    result jsonb,
    normalized_event_id uuid,
    action_key text,
    agent_turn_settlement_id uuid,
    workflow_internal_event_id uuid,
    retention_generation_id uuid,
    CONSTRAINT jobs_action_provenance_check CHECK ((((normalized_event_id IS NULL) AND (agent_turn_settlement_id IS NULL) AND (workflow_internal_event_id IS NULL) AND (action_key IS NULL)) OR ((((((normalized_event_id IS NOT NULL))::integer + ((agent_turn_settlement_id IS NOT NULL))::integer) + ((workflow_internal_event_id IS NOT NULL))::integer) = 1) AND (action_key IS NOT NULL) AND (action_key <> ''::text)))),
    CONSTRAINT jobs_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT jobs_attempt_limit_check CHECK ((attempt_count <= max_attempts)),
    CONSTRAINT jobs_enqueue_delay_microseconds_check CHECK ((enqueue_delay_microseconds >= 0)),
    CONSTRAINT jobs_kind_check CHECK ((kind <> ''::text)),
    CONSTRAINT jobs_lease_check CHECK ((((status = 'LEASED'::text) AND (lease_owner IS NOT NULL) AND (lease_token IS NOT NULL) AND (leased_at IS NOT NULL) AND (lease_expires_at IS NOT NULL)) OR ((status <> 'LEASED'::text) AND (lease_owner IS NULL) AND (lease_token IS NULL) AND (leased_at IS NULL) AND (lease_expires_at IS NULL)))),
    CONSTRAINT jobs_max_attempts_check CHECK ((max_attempts > 0)),
    CONSTRAINT jobs_queue_check CHECK ((queue <> ''::text)),
    CONSTRAINT jobs_result_check CHECK (((result IS NULL) OR (status = 'SUCCEEDED'::text))),
    CONSTRAINT jobs_status_check CHECK ((status = ANY (ARRAY['AVAILABLE'::text, 'LEASED'::text, 'SUCCEEDED'::text, 'FAILED'::text, 'CANCELLED'::text])))
);

CREATE TABLE public.normalized_events (
    delivery_id uuid NOT NULL,
    payload jsonb NOT NULL,
    status text DEFAULT 'PENDING'::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    workflow_id uuid,
    disposition text,
    reason text,
    applied_revision bigint,
    deferred_for_turn_id uuid,
    processed_at timestamp with time zone,
    attempt_count integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 3 NOT NULL,
    last_error text,
    CONSTRAINT normalized_events_applied_revision_check CHECK ((applied_revision >= 0)),
    CONSTRAINT normalized_events_attempt_limit_check CHECK (((attempt_count >= 0) AND (max_attempts > 0) AND (attempt_count <= max_attempts))),
    CONSTRAINT normalized_events_outcome_check CHECK ((((status = 'PENDING'::text) AND (workflow_id IS NULL) AND (disposition IS NULL) AND (reason IS NULL) AND (applied_revision IS NULL) AND (deferred_for_turn_id IS NULL) AND (processed_at IS NULL)) OR ((status = 'DEFERRED'::text) AND (disposition = 'DEFERRED'::text) AND (reason IS NOT NULL) AND (applied_revision IS NOT NULL) AND (processed_at IS NOT NULL) AND (last_error IS NULL)) OR ((status = 'COMPLETED'::text) AND (disposition = ANY (ARRAY['APPLIED'::text, 'DUPLICATE'::text, 'STALE'::text, 'UNRELATED'::text, 'ILLEGAL'::text, 'RECONCILIATION_FAILED'::text])) AND (reason IS NOT NULL) AND (applied_revision IS NOT NULL) AND (deferred_for_turn_id IS NULL) AND (processed_at IS NOT NULL) AND (last_error IS NULL)) OR ((status = 'FAILED'::text) AND (workflow_id IS NULL) AND (disposition IS NULL) AND (reason IS NOT NULL) AND (reason <> ''::text) AND (applied_revision IS NULL) AND (deferred_for_turn_id IS NULL) AND (processed_at IS NOT NULL) AND (last_error = reason)))),
    CONSTRAINT normalized_events_status_check CHECK ((status = ANY (ARRAY['PENDING'::text, 'DEFERRED'::text, 'COMPLETED'::text, 'FAILED'::text])))
);

CREATE TABLE public.pi_runtime_profile_compatibility_results (
    qualification_key text NOT NULL,
    result jsonb NOT NULL,
    result_sha256 bytea NOT NULL,
    recorded_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT pi_runtime_profile_compatibility_result_qualification_key_check CHECK ((qualification_key ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT pi_runtime_profile_compatibility_results_result_check CHECK ((jsonb_typeof(result) = 'object'::text)),
    CONSTRAINT pi_runtime_profile_compatibility_results_result_sha256_check CHECK ((octet_length(result_sha256) = 32))
);

CREATE TABLE public.retained_runtime_states (
    agent_participant_id uuid NOT NULL,
    format text NOT NULL,
    metadata jsonb NOT NULL,
    initialized boolean DEFAULT false NOT NULL,
    CONSTRAINT retained_runtime_states_metadata_check CHECK ((jsonb_typeof(metadata) = 'object'::text))
);

CREATE TABLE public.runtime_profile_contracts (
    content_sha256 text NOT NULL,
    contract jsonb NOT NULL,
    CONSTRAINT runtime_profile_contracts_content_sha256_check CHECK ((content_sha256 ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT runtime_profile_contracts_contract_check CHECK ((jsonb_typeof(contract) = 'object'::text))
);

CREATE TABLE public.stage_assignments (
    workflow_id uuid NOT NULL,
    assignment_generation integer NOT NULL,
    stage_id text NOT NULL,
    role text NOT NULL,
    agent_participant_id uuid NOT NULL,
    created_by_preparation_job_id uuid,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT stage_assignments_assignment_generation_check CHECK ((assignment_generation > 0)),
    CONSTRAINT stage_assignments_role_syntax_check CHECK ((role ~ '^[A-Z][A-Z0-9_]{0,63}$'::text)),
    CONSTRAINT stage_assignments_stage_id_check CHECK ((stage_id ~ '^[a-z][a-z0-9_-]{0,63}$'::text))
);

CREATE TABLE public.tool_invocation_replays (
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    operation_lineage_id uuid NOT NULL,
    invocation_number bigint NOT NULL,
    source_tool_invocation_id uuid NOT NULL,
    source_agent_turn_id uuid NOT NULL,
    source_execution_epoch bigint NOT NULL,
    replayed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT tool_invocation_replays_distinct_turn_check CHECK ((source_agent_turn_id <> agent_turn_id)),
    CONSTRAINT tool_invocation_replays_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT tool_invocation_replays_invocation_number_check CHECK ((invocation_number > 0)),
    CONSTRAINT tool_invocation_replays_source_execution_epoch_check CHECK ((source_execution_epoch > 0))
);

CREATE TABLE public.tool_invocations (
    id uuid NOT NULL,
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    invocation_number bigint NOT NULL,
    tool_name text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL,
    idempotency_key text,
    request jsonb DEFAULT '{}'::jsonb NOT NULL,
    result jsonb,
    external_service text,
    external_resource_id text,
    expected_sha text,
    admitted_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    duration_ms bigint,
    last_error text,
    operation_id text,
    operation_lineage_id uuid NOT NULL,
    history_publication boolean DEFAULT false NOT NULL,
    proposed_sha text,
    CONSTRAINT tool_invocations_duration_ms_check CHECK ((duration_ms >= 0)),
    CONSTRAINT tool_invocations_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT tool_invocations_history_publication_check CHECK (((NOT history_publication) OR ((kind = 'MUTATION'::text) AND (tool_name = 'publish_changes'::text)))),
    CONSTRAINT tool_invocations_invocation_number_check CHECK ((invocation_number > 0)),
    CONSTRAINT tool_invocations_kind_check CHECK ((kind = ANY (ARRAY['READ'::text, 'MUTATION'::text]))),
    CONSTRAINT tool_invocations_proposed_publication_sha_check CHECK (((proposed_sha IS NULL) OR (history_publication AND (tool_name = 'publish_changes'::text) AND ((proposed_sha ~ '^[0-9a-f]{40}$'::text) OR (proposed_sha ~ '^[0-9a-f]{64}$'::text))))),
    CONSTRAINT tool_invocations_state_check CHECK ((state = ANY (ARRAY['RESERVED'::text, 'IN_FLIGHT'::text, 'UNKNOWN'::text, 'SUCCEEDED'::text, 'FAILED'::text, 'RECONCILING'::text]))),
    CONSTRAINT tool_invocations_tool_name_check CHECK ((tool_name <> ''::text))
);

CREATE TABLE public.webhook_deliveries (
    delivery_id uuid NOT NULL,
    event_name text NOT NULL,
    action text,
    repository_id bigint,
    repository_owner text,
    repository_name text,
    issue_id bigint,
    issue_number bigint,
    workflow_id uuid,
    headers jsonb DEFAULT '{}'::jsonb NOT NULL,
    payload bytea NOT NULL,
    status text DEFAULT 'PENDING'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    claim_owner text,
    claim_token uuid,
    claimed_at timestamp with time zone,
    lease_expires_at timestamp with time zone,
    received_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    processed_at timestamp with time zone,
    last_error text,
    max_attempts integer DEFAULT 3 NOT NULL,
    retry_at timestamp with time zone,
    CONSTRAINT webhook_deliveries_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT webhook_deliveries_attempt_limit_check CHECK (((max_attempts IS NOT NULL) AND (max_attempts > 0) AND (attempt_count <= max_attempts))),
    CONSTRAINT webhook_deliveries_claim_check CHECK ((((claim_owner IS NULL) AND (claim_token IS NULL) AND (claimed_at IS NULL) AND (lease_expires_at IS NULL)) OR ((claim_owner IS NOT NULL) AND (claim_token IS NOT NULL) AND (claimed_at IS NOT NULL) AND (lease_expires_at IS NOT NULL)))),
    CONSTRAINT webhook_deliveries_event_name_check CHECK ((event_name <> ''::text)),
    CONSTRAINT webhook_deliveries_issue_id_check CHECK ((issue_id > 0)),
    CONSTRAINT webhook_deliveries_issue_identity_check CHECK ((((issue_id IS NULL) AND (issue_number IS NULL)) OR ((issue_id IS NOT NULL) AND (issue_number IS NOT NULL)))),
    CONSTRAINT webhook_deliveries_issue_number_check CHECK ((issue_number > 0)),
    CONSTRAINT webhook_deliveries_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT webhook_deliveries_repository_name_check CHECK ((repository_name <> ''::text)),
    CONSTRAINT webhook_deliveries_repository_owner_check CHECK ((repository_owner <> ''::text)),
    CONSTRAINT webhook_deliveries_retry_at_check CHECK (((retry_at IS NULL) OR (status = 'PENDING'::text))),
    CONSTRAINT webhook_deliveries_status_check CHECK ((status = ANY (ARRAY['PENDING'::text, 'PROCESSING'::text, 'PROCESSED'::text, 'IGNORED'::text, 'FAILED'::text])))
);

CREATE TABLE public.workflow_action_failures (
    source_job_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    escalation_job_id uuid,
    source_kind text NOT NULL,
    diagnostic text NOT NULL,
    resume_role text,
    observed_at timestamp with time zone NOT NULL,
    status text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    resolved_at timestamp with time zone,
    CONSTRAINT workflow_action_failures_diagnostic_check CHECK ((diagnostic <> ''::text)),
    CONSTRAINT workflow_action_failures_resolution_check CHECK ((((status = 'PENDING'::text) AND (escalation_job_id IS NOT NULL) AND (resolved_at IS NULL)) OR ((status = ANY (ARRAY['APPLIED'::text, 'TERMINAL'::text])) AND (resolved_at IS NOT NULL)))),
    CONSTRAINT workflow_action_failures_resume_role_syntax_check CHECK (((resume_role IS NULL) OR (resume_role ~ '^[A-Z][A-Z0-9_]{0,63}$'::text))),
    CONSTRAINT workflow_action_failures_source_kind_check CHECK ((source_kind <> ''::text)),
    CONSTRAINT workflow_action_failures_status_check CHECK ((status = ANY (ARRAY['PENDING'::text, 'APPLIED'::text, 'TERMINAL'::text])))
);

CREATE TABLE public.workflow_attempts (
    id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    attempt_number integer NOT NULL,
    trigger_delivery_id uuid,
    status text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    infrastructure_failures integer DEFAULT 0 NOT NULL,
    infrastructure_failure_limit integer DEFAULT 2 NOT NULL,
    started_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    completed_at timestamp with time zone,
    human_handoff_reason text,
    current_stage text DEFAULT 'implementation'::text NOT NULL,
    review_usage jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT workflow_attempts_active_check CHECK (((NOT active) OR (completed_at IS NULL))),
    CONSTRAINT workflow_attempts_attempt_number_check CHECK ((attempt_number > 0)),
    CONSTRAINT workflow_attempts_current_stage_check CHECK ((current_stage ~ '^[a-z][a-z0-9_-]{0,63}$'::text)),
    CONSTRAINT workflow_attempts_failure_budget_check CHECK ((infrastructure_failures <= infrastructure_failure_limit)),
    CONSTRAINT workflow_attempts_infrastructure_failure_limit_check CHECK ((infrastructure_failure_limit > 0)),
    CONSTRAINT workflow_attempts_infrastructure_failures_check CHECK ((infrastructure_failures >= 0)),
    CONSTRAINT workflow_attempts_review_usage_check CHECK ((jsonb_typeof(review_usage) = 'object'::text)),
    CONSTRAINT workflow_attempts_status_check CHECK ((status <> ''::text))
);

CREATE TABLE public.workflow_closure_barriers (
    workflow_id uuid NOT NULL,
    closure_id text NOT NULL,
    workflow_revision bigint NOT NULL,
    source_turn_id uuid,
    source_session_id uuid,
    source_attempt_id uuid,
    source_execution_epoch bigint,
    source_control_revision bigint,
    mutation_admission_closed_at timestamp with time zone NOT NULL,
    runtime_stopped_at timestamp with time zone,
    settled_at timestamp with time zone,
    stop_job_id uuid,
    settlement_job_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    internal_event_id uuid,
    applied_revision bigint,
    CONSTRAINT workflow_closure_barriers_applied_revision_check CHECK ((applied_revision > 0)),
    CONSTRAINT workflow_closure_barriers_closure_id_check CHECK ((closure_id <> ''::text)),
    CONSTRAINT workflow_closure_barriers_source_check CHECK ((((source_turn_id IS NULL) AND (source_session_id IS NULL) AND (source_attempt_id IS NULL) AND (source_execution_epoch IS NULL) AND (source_control_revision IS NULL) AND (stop_job_id IS NULL) AND (runtime_stopped_at IS NULL)) OR ((source_turn_id IS NOT NULL) AND (source_session_id IS NOT NULL) AND (source_attempt_id IS NOT NULL) AND (source_execution_epoch > 0) AND (source_control_revision > 0) AND (stop_job_id IS NOT NULL)))),
    CONSTRAINT workflow_closure_barriers_workflow_revision_check CHECK ((workflow_revision > 0))
);

CREATE TABLE public.workflow_github_effect_cleanups (
    job_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    issue_number bigint NOT NULL,
    pull_request_number bigint,
    observed_revision bigint NOT NULL,
    observed_state text NOT NULL,
    current_revision bigint NOT NULL,
    current_state text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    cleaned_at timestamp with time zone,
    status text DEFAULT 'PENDING'::text NOT NULL,
    last_error text,
    exhausted_at timestamp with time zone,
    CONSTRAINT workflow_github_effect_cleanups_current_revision_check CHECK ((current_revision > 0)),
    CONSTRAINT workflow_github_effect_cleanups_current_state_check CHECK ((current_state <> ''::text)),
    CONSTRAINT workflow_github_effect_cleanups_issue_number_check CHECK ((issue_number > 0)),
    CONSTRAINT workflow_github_effect_cleanups_observed_revision_check CHECK ((observed_revision > 0)),
    CONSTRAINT workflow_github_effect_cleanups_observed_state_check CHECK ((observed_state <> ''::text)),
    CONSTRAINT workflow_github_effect_cleanups_outcome_check CHECK ((((status = 'PENDING'::text) AND (cleaned_at IS NULL) AND (exhausted_at IS NULL)) OR ((status = 'SUCCEEDED'::text) AND (cleaned_at IS NOT NULL) AND (exhausted_at IS NULL) AND (last_error IS NULL)) OR ((status = 'EXHAUSTED'::text) AND (cleaned_at IS NULL) AND (exhausted_at IS NOT NULL) AND (last_error IS NOT NULL) AND (last_error <> ''::text)))),
    CONSTRAINT workflow_github_effect_cleanups_pull_request_number_check CHECK ((pull_request_number > 0)),
    CONSTRAINT workflow_github_effect_cleanups_status_check CHECK ((status = ANY (ARRAY['PENDING'::text, 'SUCCEEDED'::text, 'EXHAUSTED'::text])))
);

CREATE TABLE public.workflow_internal_events (
    id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    kind text NOT NULL,
    source_job_id uuid NOT NULL,
    source_attempt_number integer NOT NULL,
    source_lease_token uuid NOT NULL,
    payload jsonb NOT NULL,
    disposition text,
    reason text,
    workflow_revision bigint,
    observed_at timestamp with time zone NOT NULL,
    applied_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT workflow_internal_events_disposition_check CHECK ((disposition = 'APPLIED'::text)),
    CONSTRAINT workflow_internal_events_kind_check CHECK ((kind = ANY (ARRAY['CLOSURE_SETTLED'::text, 'ASSIGNMENTS_COLLECTED'::text, 'TERMINAL_INTENT_REVALIDATED'::text, 'TERMINAL_REVALIDATION_FAILED'::text]))),
    CONSTRAINT workflow_internal_events_outcome_check CHECK ((((applied_at IS NULL) AND (disposition IS NULL) AND (reason IS NULL) AND (workflow_revision IS NULL)) OR ((applied_at IS NOT NULL) AND (disposition = 'APPLIED'::text) AND (reason IS NOT NULL) AND (reason <> ''::text) AND (workflow_revision IS NOT NULL)))),
    CONSTRAINT workflow_internal_events_payload_check CHECK ((jsonb_typeof(payload) = 'object'::text)),
    CONSTRAINT workflow_internal_events_source_attempt_number_check CHECK ((source_attempt_number > 0)),
    CONSTRAINT workflow_internal_events_workflow_revision_check CHECK ((workflow_revision > 0))
);

CREATE TABLE public.workflows (
    id uuid NOT NULL,
    repository_id bigint NOT NULL,
    repository_owner text NOT NULL,
    repository_name text NOT NULL,
    issue_id bigint NOT NULL,
    issue_number bigint NOT NULL,
    status text NOT NULL,
    state_revision bigint DEFAULT 1 NOT NULL,
    ready_for_sha text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    closed_at timestamp with time zone,
    human_handoff_reason text,
    resume_role text,
    desired_assignment_status text DEFAULT 'ACTIVE'::text NOT NULL,
    desired_runtime_state text DEFAULT 'ACTIVE'::text NOT NULL,
    closure_id text,
    closure_deadline timestamp with time zone,
    closure_retention_token text,
    closure_reopen_requested boolean DEFAULT false NOT NULL,
    retention_deadline timestamp with time zone,
    retention_token text,
    continuation_stage text,
    CONSTRAINT workflows_closure_identity_check CHECK ((((closure_id IS NULL) AND (closure_deadline IS NULL) AND (closure_retention_token IS NULL) AND (NOT closure_reopen_requested)) OR ((closure_id IS NOT NULL) AND (closure_id <> ''::text) AND (closure_deadline IS NOT NULL) AND (closure_retention_token IS NOT NULL) AND (closure_retention_token <> ''::text)))),
    CONSTRAINT workflows_continuation_stage_check CHECK (((continuation_stage IS NULL) OR (continuation_stage ~ '^[a-z][a-z0-9_-]{0,63}$'::text))),
    CONSTRAINT workflows_desired_assignment_status_check CHECK ((desired_assignment_status = ANY (ARRAY['ACTIVE'::text, 'WAITING_FOR_HUMAN'::text, 'COMPLETED'::text]))),
    CONSTRAINT workflows_desired_runtime_state_check CHECK ((desired_runtime_state = ANY (ARRAY['ACTIVE'::text, 'RETAINED'::text, 'COLLECTED'::text]))),
    CONSTRAINT workflows_issue_id_check CHECK ((issue_id > 0)),
    CONSTRAINT workflows_issue_number_check CHECK ((issue_number > 0)),
    CONSTRAINT workflows_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT workflows_repository_name_check CHECK ((repository_name <> ''::text)),
    CONSTRAINT workflows_repository_owner_check CHECK ((repository_owner <> ''::text)),
    CONSTRAINT workflows_resume_role_syntax_check CHECK (((resume_role IS NULL) OR (resume_role ~ '^[A-Z][A-Z0-9_]{0,63}$'::text))),
    CONSTRAINT workflows_retention_identity_check CHECK ((((retention_deadline IS NULL) AND (retention_token IS NULL)) OR ((retention_deadline IS NOT NULL) AND (retention_token IS NOT NULL) AND (retention_token <> ''::text)))),
    CONSTRAINT workflows_state_revision_check CHECK ((state_revision > 0)),
    CONSTRAINT workflows_status_check CHECK ((status <> ''::text))
);

CREATE TABLE public.workspace_captures (
    agent_turn_id uuid NOT NULL,
    execution_epoch bigint NOT NULL,
    purpose text NOT NULL,
    operation_id text NOT NULL,
    request jsonb NOT NULL,
    sha256 text NOT NULL,
    sealed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT workspace_captures_execution_epoch_check CHECK ((execution_epoch > 0)),
    CONSTRAINT workspace_captures_operation_id_check CHECK (((octet_length(operation_id) >= 1) AND (octet_length(operation_id) <= 256))),
    CONSTRAINT workspace_captures_purpose_check CHECK ((purpose = ANY (ARRAY['publication'::text, 'terminal'::text]))),
    CONSTRAINT workspace_captures_request_check CHECK ((jsonb_typeof(request) = 'object'::text)),
    CONSTRAINT workspace_captures_sha256_check CHECK ((sha256 ~ '^[0-9a-f]{64}$'::text))
);

ALTER TABLE ONLY public.agent_assignments
    ADD CONSTRAINT agent_assignments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_assignments
    ADD CONSTRAINT agent_participants_identity_key UNIQUE (workflow_id, generation, agent_profile_name);

ALTER TABLE ONLY public.agent_assignments
    ADD CONSTRAINT agent_participants_identity_role_key UNIQUE (id, workflow_id, generation, role);

ALTER TABLE ONLY public.agent_sessions
    ADD CONSTRAINT agent_sessions_agent_assignment_id_session_number_key UNIQUE (agent_assignment_id, session_number);

ALTER TABLE ONLY public.agent_sessions
    ADD CONSTRAINT agent_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_turn_corroborations
    ADD CONSTRAINT agent_turn_corroborations_pkey PRIMARY KEY (agent_turn_id);

ALTER TABLE ONLY public.agent_turn_publications
    ADD CONSTRAINT agent_turn_publications_pkey PRIMARY KEY (agent_turn_id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_execution_attempt_key UNIQUE (execution_job_id, job_attempt_number);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_turn_epoch_key UNIQUE (agent_turn_id, execution_epoch);

ALTER TABLE ONLY public.agent_turn_slots
    ADD CONSTRAINT agent_turn_slots_owner_token_key UNIQUE (owner_token);

ALTER TABLE ONLY public.agent_turn_slots
    ADD CONSTRAINT agent_turn_slots_pkey PRIMARY KEY (agent_turn_id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_agent_session_id_execution_epoch_key UNIQUE (agent_session_id, execution_epoch);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_agent_session_id_turn_number_key UNIQUE (agent_session_id, turn_number);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_id_execution_epoch_key UNIQUE (id, execution_epoch);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.assignment_retention_generations
    ADD CONSTRAINT assignment_retention_generation_workflow_id_retention_token_key UNIQUE (workflow_id, retention_token);

ALTER TABLE ONLY public.assignment_retention_generations
    ADD CONSTRAINT assignment_retention_generations_collected_event_id_key UNIQUE (collected_event_id);

ALTER TABLE ONLY public.assignment_retention_generations
    ADD CONSTRAINT assignment_retention_generations_collection_job_id_key UNIQUE (collection_job_id);

ALTER TABLE ONLY public.assignment_retention_generations
    ADD CONSTRAINT assignment_retention_generations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.assignment_retention_targets
    ADD CONSTRAINT assignment_retention_targets_identity_key UNIQUE NULLS NOT DISTINCT (retention_generation_id, assignment_id, session_id);

ALTER TABLE ONLY public.change_proposal_reviews
    ADD CONSTRAINT change_proposal_reviews_pkey PRIMARY KEY (repository_id, review_id);

ALTER TABLE ONLY public.change_proposal_reviews
    ADD CONSTRAINT change_proposal_reviews_repository_id_review_node_id_key UNIQUE (repository_id, review_node_id);

ALTER TABLE ONLY public.change_proposals
    ADD CONSTRAINT change_proposals_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.change_proposals
    ADD CONSTRAINT change_proposals_repository_id_pull_request_id_key UNIQUE (repository_id, pull_request_id);

ALTER TABLE ONLY public.change_proposals
    ADD CONSTRAINT change_proposals_repository_id_pull_request_number_key UNIQUE (repository_id, pull_request_number);

ALTER TABLE ONLY public.execution_backend_binding
    ADD CONSTRAINT execution_backend_binding_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY public.execution_reservations
    ADD CONSTRAINT execution_reservations_pkey PRIMARY KEY (agent_turn_id, execution_epoch);

ALTER TABLE ONLY public.job_attempts
    ADD CONSTRAINT job_attempts_lease_token_key UNIQUE (lease_token);

ALTER TABLE ONLY public.job_attempts
    ADD CONSTRAINT job_attempts_pkey PRIMARY KEY (job_id, attempt_number);

ALTER TABLE ONLY public.job_normalized_events
    ADD CONSTRAINT job_normalized_events_normalized_event_id_key UNIQUE (normalized_event_id);

ALTER TABLE ONLY public.job_normalized_events
    ADD CONSTRAINT job_normalized_events_pkey PRIMARY KEY (job_id, normalized_event_id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_pkey PRIMARY KEY (id);

ALTER TABLE public.jobs
    ADD CONSTRAINT jobs_scope_hierarchy_check CHECK ((((workflow_attempt_id IS NULL) OR (workflow_id IS NOT NULL)) AND ((agent_assignment_id IS NULL) OR (workflow_id IS NOT NULL)) AND ((agent_session_id IS NULL) OR (agent_assignment_id IS NOT NULL)) AND ((agent_turn_id IS NULL) OR (agent_session_id IS NOT NULL))));

ALTER TABLE ONLY public.normalized_events
    ADD CONSTRAINT normalized_events_pkey PRIMARY KEY (delivery_id);

ALTER TABLE ONLY public.pi_runtime_profile_compatibility_results
    ADD CONSTRAINT pi_runtime_profile_compatibility_results_pkey PRIMARY KEY (qualification_key);

ALTER TABLE ONLY public.retained_runtime_states
    ADD CONSTRAINT retained_runtime_states_pkey PRIMARY KEY (agent_participant_id);

ALTER TABLE ONLY public.runtime_profile_contracts
    ADD CONSTRAINT runtime_profile_contracts_pkey PRIMARY KEY (content_sha256);

ALTER TABLE ONLY public.stage_assignments
    ADD CONSTRAINT stage_assignments_pkey PRIMARY KEY (workflow_id, assignment_generation, stage_id);

ALTER TABLE ONLY public.tool_invocation_replays
    ADD CONSTRAINT tool_invocation_replays_pkey PRIMARY KEY (agent_turn_id, source_tool_invocation_id);

ALTER TABLE ONLY public.tool_invocation_replays
    ADD CONSTRAINT tool_invocation_replays_turn_invocation_key UNIQUE (agent_turn_id, invocation_number);

ALTER TABLE ONLY public.tool_invocations
    ADD CONSTRAINT tool_invocations_agent_turn_id_invocation_number_key UNIQUE (agent_turn_id, invocation_number);

ALTER TABLE ONLY public.tool_invocations
    ADD CONSTRAINT tool_invocations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_pkey PRIMARY KEY (delivery_id);

ALTER TABLE ONLY public.workflow_action_failures
    ADD CONSTRAINT workflow_action_failures_escalation_job_id_key UNIQUE (escalation_job_id);

ALTER TABLE ONLY public.workflow_action_failures
    ADD CONSTRAINT workflow_action_failures_pkey PRIMARY KEY (source_job_id);

ALTER TABLE ONLY public.workflow_attempts
    ADD CONSTRAINT workflow_attempts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.workflow_attempts
    ADD CONSTRAINT workflow_attempts_workflow_id_attempt_number_key UNIQUE (workflow_id, attempt_number);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_internal_event_id_key UNIQUE (internal_event_id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_pkey PRIMARY KEY (workflow_id, closure_id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_settlement_job_id_key UNIQUE (settlement_job_id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_stop_job_id_key UNIQUE (stop_job_id);

ALTER TABLE ONLY public.workflow_github_effect_cleanups
    ADD CONSTRAINT workflow_github_effect_cleanups_pkey PRIMARY KEY (job_id);

ALTER TABLE ONLY public.workflow_internal_events
    ADD CONSTRAINT workflow_internal_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.workflow_internal_events
    ADD CONSTRAINT workflow_internal_events_source_job_id_key UNIQUE (source_job_id);

ALTER TABLE ONLY public.workflow_internal_events
    ADD CONSTRAINT workflow_internal_events_source_lease_token_key UNIQUE (source_lease_token);

ALTER TABLE ONLY public.workflows
    ADD CONSTRAINT workflows_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.workflows
    ADD CONSTRAINT workflows_repository_id_issue_id_key UNIQUE (repository_id, issue_id);

ALTER TABLE ONLY public.workflows
    ADD CONSTRAINT workflows_repository_id_issue_number_key UNIQUE (repository_id, issue_number);

ALTER TABLE ONLY public.workspace_captures
    ADD CONSTRAINT workspace_captures_pkey PRIMARY KEY (agent_turn_id, execution_epoch, purpose, operation_id);

CREATE UNIQUE INDEX agent_assignments_id_workflow_idx ON public.agent_assignments USING btree (id, workflow_id);

CREATE INDEX agent_assignments_retention_idx ON public.agent_assignments USING btree (retention_until) WHERE ((retention_until IS NOT NULL) AND (state_deleted_at IS NULL));

CREATE UNIQUE INDEX agent_participants_live_runtime_state_path_idx ON public.agent_assignments USING btree (runtime_state_path) WHERE ((status = 'ACTIVE'::text) AND (state_deleted_at IS NULL));

CREATE UNIQUE INDEX agent_sessions_current_idx ON public.agent_sessions USING btree (agent_assignment_id) WHERE (status = ANY (ARRAY['CREATING'::text, 'ACTIVE'::text, 'RETAINED'::text]));

CREATE UNIQUE INDEX agent_sessions_human_prompt_token_idx ON public.agent_sessions USING btree (human_prompt_token) WHERE (human_prompt_token IS NOT NULL);

CREATE UNIQUE INDEX agent_sessions_id_assignment_idx ON public.agent_sessions USING btree (id, agent_assignment_id);

CREATE UNIQUE INDEX agent_sessions_one_human_controlled_per_workflow_idx ON public.agent_sessions USING btree (workflow_id) WHERE ((control_owner = 'HUMAN'::text) AND (status = ANY (ARRAY['ACTIVE'::text, 'RETAINED'::text])));

CREATE UNIQUE INDEX agent_sessions_runtime_identifier_idx ON public.agent_sessions USING btree (runtime_session_id) WHERE (runtime_session_id IS NOT NULL);

CREATE UNIQUE INDEX agent_turn_corroborations_pending_workflow_idx ON public.agent_turn_corroborations USING btree (workflow_id) WHERE (state = 'PENDING'::text);

CREATE UNIQUE INDEX agent_turn_settlements_job_lease_token_idx ON public.agent_turn_settlements USING btree (job_lease_token);

CREATE UNIQUE INDEX agent_turn_settlements_recovery_job_attempt_idx ON public.agent_turn_settlements USING btree (recovery_job_id, recovery_job_attempt_number) WHERE (recovery_job_id IS NOT NULL);

CREATE UNIQUE INDEX agent_turn_settlements_recovery_job_lease_token_idx ON public.agent_turn_settlements USING btree (recovery_job_lease_token) WHERE (recovery_job_lease_token IS NOT NULL);

CREATE INDEX agent_turn_slots_live_idx ON public.agent_turn_slots USING btree (lease_expires_at, agent_turn_id);

CREATE INDEX agent_turns_attempt_idx ON public.agent_turns USING btree (workflow_attempt_id, created_at);

CREATE INDEX agent_turns_expired_lease_idx ON public.agent_turns USING btree (lease_expires_at) WHERE (active AND (lease_expires_at IS NOT NULL));

CREATE UNIQUE INDEX agent_turns_id_epoch_lineage_idx ON public.agent_turns USING btree (id, execution_epoch, operation_lineage_id);

CREATE UNIQUE INDEX agent_turns_id_session_epoch_idx ON public.agent_turns USING btree (id, agent_session_id, execution_epoch);

CREATE UNIQUE INDEX agent_turns_id_session_lineage_idx ON public.agent_turns USING btree (id, agent_session_id, operation_lineage_id);

CREATE UNIQUE INDEX agent_turns_one_active_idx ON public.agent_turns USING btree (agent_session_id) WHERE active;

CREATE UNIQUE INDEX agent_turns_one_active_workflow_idx ON public.agent_turns USING btree (workflow_id) WHERE active;

CREATE UNIQUE INDEX agent_turns_preparation_job_idx ON public.agent_turns USING btree (preparation_job_id) WHERE (preparation_job_id IS NOT NULL);

CREATE INDEX agent_turns_unsettled_recovery_idx ON public.agent_turns USING btree (agent_session_id, recovery_started_at) WHERE ((recovery_started_at IS NOT NULL) AND (recovery_settled_at IS NULL));

CREATE INDEX assignment_retention_generations_due_idx ON public.assignment_retention_generations USING btree (retention_deadline, workflow_id) WHERE (status = 'SCHEDULED'::text);

CREATE INDEX assignment_retention_targets_generation_idx ON public.assignment_retention_targets USING btree (retention_generation_id, runtime_state_path, session_id);

CREATE INDEX change_proposal_reviews_proposal_idx ON public.change_proposal_reviews USING btree (change_proposal_id, created_at);

CREATE INDEX change_proposal_reviews_settlement_idx ON public.change_proposal_reviews USING btree (agent_turn_settlement_id) WHERE (agent_turn_settlement_id IS NOT NULL);

CREATE INDEX change_proposals_head_idx ON public.change_proposals USING btree (repository_id, head_sha);

CREATE UNIQUE INDEX change_proposals_one_active_idx ON public.change_proposals USING btree (workflow_id) WHERE active;

CREATE INDEX job_attempts_live_lease_idx ON public.job_attempts USING btree (lease_expires_at, job_id) WHERE (status = 'LEASED'::text);

CREATE UNIQUE INDEX jobs_agent_turn_execution_idx ON public.jobs USING btree (agent_turn_id, execution_epoch) WHERE (kind = 'RUN_AGENT_TURN'::text);

CREATE UNIQUE INDEX jobs_agent_turn_recovery_kind_idx ON public.jobs USING btree (agent_turn_id, execution_epoch, kind) WHERE (kind = ANY (ARRAY['STOP_STALE_RUNTIME'::text, 'RECONCILE_AGENT_TURN_MUTATIONS'::text]));

CREATE UNIQUE INDEX jobs_agent_turn_settlement_action_idx ON public.jobs USING btree (agent_turn_settlement_id, action_key) WHERE (agent_turn_settlement_id IS NOT NULL);

CREATE INDEX jobs_available_idx ON public.jobs USING btree (queue, priority DESC, available_at, id) WHERE (status = 'AVAILABLE'::text);

CREATE INDEX jobs_available_kind_idx ON public.jobs USING btree (queue, kind, priority DESC, available_at, id) WHERE (status = 'AVAILABLE'::text);

CREATE INDEX jobs_expired_lease_idx ON public.jobs USING btree (lease_expires_at) WHERE (status = 'LEASED'::text);

CREATE UNIQUE INDEX jobs_idempotency_idx ON public.jobs USING btree (idempotency_key) WHERE (idempotency_key IS NOT NULL);

CREATE UNIQUE INDEX jobs_normalized_event_action_idx ON public.jobs USING btree (normalized_event_id, action_key) WHERE (normalized_event_id IS NOT NULL);

CREATE UNIQUE INDEX jobs_one_live_workflow_successor_idx ON public.jobs USING btree (workflow_id) WHERE ((workflow_id IS NOT NULL) AND (kind = 'PREPARE_AGENT_TURN'::text) AND (status = ANY (ARRAY['AVAILABLE'::text, 'LEASED'::text])));

CREATE UNIQUE INDEX jobs_retention_generation_idx ON public.jobs USING btree (retention_generation_id) WHERE (retention_generation_id IS NOT NULL);

CREATE INDEX jobs_workflow_idx ON public.jobs USING btree (workflow_id, status, created_at) WHERE (workflow_id IS NOT NULL);

CREATE UNIQUE INDEX jobs_workflow_internal_event_action_idx ON public.jobs USING btree (workflow_internal_event_id, action_key) WHERE (workflow_internal_event_id IS NOT NULL);

CREATE INDEX normalized_events_deferred_workflow_idx ON public.normalized_events USING btree (workflow_id, deferred_for_turn_id, created_at) WHERE (status = 'DEFERRED'::text);

CREATE INDEX normalized_events_pending_idx ON public.normalized_events USING btree (created_at, delivery_id) WHERE (status = 'PENDING'::text);

CREATE INDEX stage_assignments_participant_idx ON public.stage_assignments USING btree (agent_participant_id, workflow_id, assignment_generation);

CREATE UNIQUE INDEX tool_invocations_exact_identity_idx ON public.tool_invocations USING btree (id, agent_turn_id, execution_epoch, operation_lineage_id);

CREATE UNIQUE INDEX tool_invocations_idempotency_idx ON public.tool_invocations USING btree (agent_turn_id, execution_epoch, tool_name, idempotency_key) WHERE (idempotency_key IS NOT NULL);

CREATE UNIQUE INDEX tool_invocations_operation_id_idx ON public.tool_invocations USING btree (operation_lineage_id, tool_name, operation_id) WHERE ((kind = 'MUTATION'::text) AND (operation_id IS NOT NULL));

CREATE INDEX tool_invocations_unsettled_mutations_idx ON public.tool_invocations USING btree (agent_turn_id, execution_epoch, state) WHERE ((kind = 'MUTATION'::text) AND (state = ANY (ARRAY['RESERVED'::text, 'IN_FLIGHT'::text, 'UNKNOWN'::text, 'RECONCILING'::text])));

CREATE INDEX webhook_deliveries_claimable_idx ON public.webhook_deliveries USING btree (status, lease_expires_at, received_at) WHERE (status = ANY (ARRAY['PENDING'::text, 'PROCESSING'::text]));

CREATE INDEX webhook_deliveries_repository_issue_idx ON public.webhook_deliveries USING btree (repository_id, issue_id, received_at DESC) WHERE (issue_id IS NOT NULL);

CREATE INDEX webhook_deliveries_workflow_idx ON public.webhook_deliveries USING btree (workflow_id, received_at) WHERE (workflow_id IS NOT NULL);

CREATE INDEX workflow_action_failures_pending_idx ON public.workflow_action_failures USING btree (workflow_id, created_at) WHERE (status = 'PENDING'::text);

CREATE UNIQUE INDEX workflow_attempts_id_workflow_idx ON public.workflow_attempts USING btree (id, workflow_id);

CREATE UNIQUE INDEX workflow_attempts_one_active_idx ON public.workflow_attempts USING btree (workflow_id) WHERE active;

CREATE INDEX workflow_attempts_trigger_delivery_idx ON public.workflow_attempts USING btree (trigger_delivery_id) WHERE (trigger_delivery_id IS NOT NULL);

CREATE INDEX workflow_closure_barriers_unsettled_idx ON public.workflow_closure_barriers USING btree (workflow_id, workflow_revision) WHERE (settled_at IS NULL);

CREATE INDEX workflow_github_effect_cleanups_pending_idx ON public.workflow_github_effect_cleanups USING btree (workflow_id, created_at) WHERE (status = 'PENDING'::text);

CREATE TRIGGER agent_participants_binding_immutable BEFORE UPDATE ON public.agent_assignments FOR EACH ROW EXECUTE FUNCTION public.protect_agent_participant_binding();

CREATE TRIGGER agent_participants_profile_role BEFORE INSERT OR UPDATE OF agent_profile_name, role ON public.agent_assignments FOR EACH ROW EXECUTE FUNCTION public.enforce_repository_agent_profile_role();

CREATE TRIGGER agent_sessions_workflow_identity BEFORE INSERT OR UPDATE OF agent_assignment_id, workflow_id ON public.agent_sessions FOR EACH ROW EXECUTE FUNCTION public.set_agent_session_workflow();

CREATE TRIGGER execution_backend_binding_immutable BEFORE DELETE OR UPDATE ON public.execution_backend_binding FOR EACH ROW EXECUTE FUNCTION public.protect_execution_backend_binding();

CREATE TRIGGER execution_preparation_only_cleanup_immutable BEFORE DELETE OR UPDATE ON public.execution_reservations FOR EACH ROW EXECUTE FUNCTION public.protect_preparation_only_cleanup_proof();

CREATE TRIGGER participant_state_binding AFTER INSERT ON public.agent_assignments FOR EACH ROW EXECUTE FUNCTION public.initialize_participant_state_binding();

CREATE TRIGGER pi_runtime_profile_compatibility_results_immutable BEFORE DELETE OR UPDATE ON public.pi_runtime_profile_compatibility_results FOR EACH ROW EXECUTE FUNCTION public.reject_runtime_profile_compatibility_result_mutation();

CREATE TRIGGER retained_state_binding_immutable BEFORE UPDATE ON public.retained_runtime_states FOR EACH ROW EXECUTE FUNCTION public.protect_retained_state_binding();

CREATE TRIGGER runtime_profile_contracts_immutable BEFORE DELETE OR UPDATE ON public.runtime_profile_contracts FOR EACH ROW EXECUTE FUNCTION public.reject_runtime_profile_contract_mutation();

CREATE TRIGGER runtime_session_identity_immutable BEFORE UPDATE ON public.agent_sessions FOR EACH ROW EXECUTE FUNCTION public.protect_runtime_session_identity();

CREATE TRIGGER stage_assignments_immutable BEFORE DELETE OR UPDATE ON public.stage_assignments FOR EACH ROW EXECUTE FUNCTION public.reject_stage_assignment_mutation();

CREATE TRIGGER workflows_repository_agent_profile_roles BEFORE UPDATE OF repository_id ON public.workflows FOR EACH ROW EXECUTE FUNCTION public.enforce_workflow_repository_agent_profile_roles();

CREATE TRIGGER workspace_captures_immutable BEFORE DELETE OR UPDATE ON public.workspace_captures FOR EACH ROW EXECUTE FUNCTION public.reject_workspace_capture_mutation();

ALTER TABLE ONLY public.agent_assignments
    ADD CONSTRAINT agent_assignments_created_by_preparation_job_id_fkey FOREIGN KEY (created_by_preparation_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_assignments
    ADD CONSTRAINT agent_assignments_reactivated_by_preparation_job_id_fkey FOREIGN KEY (reactivated_by_preparation_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_assignments
    ADD CONSTRAINT agent_assignments_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.agent_sessions
    ADD CONSTRAINT agent_sessions_agent_assignment_id_fkey FOREIGN KEY (agent_assignment_id) REFERENCES public.agent_assignments(id);

ALTER TABLE ONLY public.agent_sessions
    ADD CONSTRAINT agent_sessions_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.agent_turn_corroborations
    ADD CONSTRAINT agent_turn_corroborations_attempt_fk FOREIGN KEY (workflow_attempt_id, workflow_id) REFERENCES public.workflow_attempts(id, workflow_id);

ALTER TABLE ONLY public.agent_turn_corroborations
    ADD CONSTRAINT agent_turn_corroborations_execution_job_id_fkey FOREIGN KEY (execution_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turn_corroborations
    ADD CONSTRAINT agent_turn_corroborations_source_invocation_id_fkey FOREIGN KEY (source_invocation_id) REFERENCES public.tool_invocations(id);

ALTER TABLE ONLY public.agent_turn_corroborations
    ADD CONSTRAINT agent_turn_corroborations_turn_fk FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES public.agent_turns(id, execution_epoch);

ALTER TABLE ONLY public.agent_turn_corroborations
    ADD CONSTRAINT agent_turn_corroborations_verification_job_id_fkey FOREIGN KEY (verification_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turn_corroborations
    ADD CONSTRAINT agent_turn_corroborations_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.agent_turn_publications
    ADD CONSTRAINT agent_turn_publications_agent_turn_id_execution_epoch_fkey FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES public.agent_turns(id, execution_epoch);

ALTER TABLE ONLY public.agent_turn_publications
    ADD CONSTRAINT agent_turn_publications_source_open_pr_mutation_id_fkey FOREIGN KEY (source_open_pr_mutation_id) REFERENCES public.tool_invocations(id);

ALTER TABLE ONLY public.agent_turn_publications
    ADD CONSTRAINT agent_turn_publications_source_publish_mutation_id_fkey FOREIGN KEY (source_publish_mutation_id) REFERENCES public.tool_invocations(id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_assignment_scope_fk FOREIGN KEY (agent_assignment_id, workflow_id) REFERENCES public.agent_assignments(id, workflow_id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_attempt_scope_fk FOREIGN KEY (workflow_attempt_id, workflow_id) REFERENCES public.workflow_attempts(id, workflow_id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_change_proposal_id_fkey FOREIGN KEY (change_proposal_id) REFERENCES public.change_proposals(id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_execution_job_id_fkey FOREIGN KEY (execution_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_job_attempt_fk FOREIGN KEY (execution_job_id, job_attempt_number) REFERENCES public.job_attempts(job_id, attempt_number);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_reconciliation_job_fk FOREIGN KEY (reconciliation_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_recovery_job_attempt_fk FOREIGN KEY (recovery_job_id, recovery_job_attempt_number) REFERENCES public.job_attempts(job_id, attempt_number);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_recovery_job_id_fkey FOREIGN KEY (recovery_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_session_scope_fk FOREIGN KEY (agent_session_id, agent_assignment_id) REFERENCES public.agent_sessions(id, agent_assignment_id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_successor_job_fk FOREIGN KEY (successor_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_turn_scope_fk FOREIGN KEY (agent_turn_id, agent_session_id, execution_epoch) REFERENCES public.agent_turns(id, agent_session_id, execution_epoch);

ALTER TABLE ONLY public.agent_turn_settlements
    ADD CONSTRAINT agent_turn_settlements_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.agent_turn_slots
    ADD CONSTRAINT agent_turn_slots_agent_session_id_fkey FOREIGN KEY (agent_session_id) REFERENCES public.agent_sessions(id);

ALTER TABLE ONLY public.agent_turn_slots
    ADD CONSTRAINT agent_turn_slots_turn_epoch_fk FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES public.agent_turns(id, execution_epoch);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_agent_session_id_fkey FOREIGN KEY (agent_session_id) REFERENCES public.agent_sessions(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_change_proposal_id_fkey FOREIGN KEY (change_proposal_id) REFERENCES public.change_proposals(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_operation_lineage_id_fkey FOREIGN KEY (operation_lineage_id) REFERENCES public.agent_turns(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_preparation_job_id_fkey FOREIGN KEY (preparation_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_reconcile_mutations_job_id_fkey FOREIGN KEY (reconcile_mutations_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_recovery_settlement_id_fkey FOREIGN KEY (recovery_settlement_id) REFERENCES public.agent_turn_settlements(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_retry_lineage_fkey FOREIGN KEY (retry_of_turn_id, agent_session_id, operation_lineage_id) REFERENCES public.agent_turns(id, agent_session_id, operation_lineage_id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_retry_of_turn_id_fkey FOREIGN KEY (retry_of_turn_id) REFERENCES public.agent_turns(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_stop_runtime_job_id_fkey FOREIGN KEY (stop_runtime_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_workflow_attempt_id_fkey FOREIGN KEY (workflow_attempt_id) REFERENCES public.workflow_attempts(id);

ALTER TABLE ONLY public.agent_turns
    ADD CONSTRAINT agent_turns_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.assignment_retention_generations
    ADD CONSTRAINT assignment_retention_generations_collected_event_id_fkey FOREIGN KEY (collected_event_id) REFERENCES public.workflow_internal_events(id);

ALTER TABLE ONLY public.assignment_retention_generations
    ADD CONSTRAINT assignment_retention_generations_collection_job_id_fkey FOREIGN KEY (collection_job_id) REFERENCES public.jobs(id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY public.assignment_retention_generations
    ADD CONSTRAINT assignment_retention_generations_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.assignment_retention_targets
    ADD CONSTRAINT assignment_retention_targets_assignment_fk FOREIGN KEY (assignment_id) REFERENCES public.agent_assignments(id);

ALTER TABLE ONLY public.assignment_retention_targets
    ADD CONSTRAINT assignment_retention_targets_retention_generation_id_fkey FOREIGN KEY (retention_generation_id) REFERENCES public.assignment_retention_generations(id);

ALTER TABLE ONLY public.assignment_retention_targets
    ADD CONSTRAINT assignment_retention_targets_session_fk FOREIGN KEY (session_id) REFERENCES public.agent_sessions(id);

ALTER TABLE ONLY public.change_proposal_reviews
    ADD CONSTRAINT change_proposal_reviews_agent_turn_settlement_id_fkey FOREIGN KEY (agent_turn_settlement_id) REFERENCES public.agent_turn_settlements(id);

ALTER TABLE ONLY public.change_proposal_reviews
    ADD CONSTRAINT change_proposal_reviews_change_proposal_id_fkey FOREIGN KEY (change_proposal_id) REFERENCES public.change_proposals(id);

ALTER TABLE ONLY public.change_proposal_reviews
    ADD CONSTRAINT change_proposal_reviews_normalized_event_id_fkey FOREIGN KEY (normalized_event_id) REFERENCES public.normalized_events(delivery_id);

ALTER TABLE ONLY public.change_proposal_reviews
    ADD CONSTRAINT change_proposal_reviews_workflow_internal_event_id_fkey FOREIGN KEY (workflow_internal_event_id) REFERENCES public.workflow_internal_events(id);

ALTER TABLE ONLY public.change_proposals
    ADD CONSTRAINT change_proposals_created_by_turn_id_fkey FOREIGN KEY (created_by_turn_id) REFERENCES public.agent_turns(id);

ALTER TABLE ONLY public.change_proposals
    ADD CONSTRAINT change_proposals_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.job_attempts
    ADD CONSTRAINT job_attempts_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.job_normalized_events
    ADD CONSTRAINT job_normalized_events_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.job_normalized_events
    ADD CONSTRAINT job_normalized_events_normalized_event_id_fkey FOREIGN KEY (normalized_event_id) REFERENCES public.normalized_events(delivery_id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_agent_assignment_id_fkey FOREIGN KEY (agent_assignment_id) REFERENCES public.agent_assignments(id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_agent_session_id_fkey FOREIGN KEY (agent_session_id) REFERENCES public.agent_sessions(id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_agent_turn_settlement_id_fkey FOREIGN KEY (agent_turn_settlement_id) REFERENCES public.agent_turn_settlements(id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_assignment_scope_fk FOREIGN KEY (agent_assignment_id, workflow_id) REFERENCES public.agent_assignments(id, workflow_id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_normalized_event_id_fkey FOREIGN KEY (normalized_event_id) REFERENCES public.normalized_events(delivery_id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_retention_generation_id_fkey FOREIGN KEY (retention_generation_id) REFERENCES public.assignment_retention_generations(id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_session_scope_fk FOREIGN KEY (agent_session_id, agent_assignment_id) REFERENCES public.agent_sessions(id, agent_assignment_id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_turn_epoch_fk FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES public.agent_turns(id, execution_epoch) MATCH FULL;

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_turn_scope_fk FOREIGN KEY (agent_turn_id, agent_session_id, execution_epoch) REFERENCES public.agent_turns(id, agent_session_id, execution_epoch);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_workflow_attempt_id_fkey FOREIGN KEY (workflow_attempt_id) REFERENCES public.workflow_attempts(id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_workflow_attempt_scope_fk FOREIGN KEY (workflow_attempt_id, workflow_id) REFERENCES public.workflow_attempts(id, workflow_id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_workflow_internal_event_id_fkey FOREIGN KEY (workflow_internal_event_id) REFERENCES public.workflow_internal_events(id);

ALTER TABLE ONLY public.normalized_events
    ADD CONSTRAINT normalized_events_deferred_for_turn_id_fkey FOREIGN KEY (deferred_for_turn_id) REFERENCES public.agent_turns(id);

ALTER TABLE ONLY public.normalized_events
    ADD CONSTRAINT normalized_events_delivery_id_fkey FOREIGN KEY (delivery_id) REFERENCES public.webhook_deliveries(delivery_id);

ALTER TABLE ONLY public.normalized_events
    ADD CONSTRAINT normalized_events_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.retained_runtime_states
    ADD CONSTRAINT retained_runtime_states_agent_participant_id_fkey FOREIGN KEY (agent_participant_id) REFERENCES public.agent_assignments(id);

ALTER TABLE ONLY public.stage_assignments
    ADD CONSTRAINT stage_assignments_created_by_preparation_job_id_fkey FOREIGN KEY (created_by_preparation_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.stage_assignments
    ADD CONSTRAINT stage_assignments_participant_fk FOREIGN KEY (agent_participant_id, workflow_id, assignment_generation, role) REFERENCES public.agent_assignments(id, workflow_id, generation, role);

ALTER TABLE ONLY public.stage_assignments
    ADD CONSTRAINT stage_assignments_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.tool_invocation_replays
    ADD CONSTRAINT tool_invocation_replays_source_exact_fk FOREIGN KEY (source_tool_invocation_id, source_agent_turn_id, source_execution_epoch, operation_lineage_id) REFERENCES public.tool_invocations(id, agent_turn_id, execution_epoch, operation_lineage_id);

ALTER TABLE ONLY public.tool_invocation_replays
    ADD CONSTRAINT tool_invocation_replays_turn_epoch_lineage_fk FOREIGN KEY (agent_turn_id, execution_epoch, operation_lineage_id) REFERENCES public.agent_turns(id, execution_epoch, operation_lineage_id);

ALTER TABLE ONLY public.tool_invocations
    ADD CONSTRAINT tool_invocations_turn_epoch_lineage_fk FOREIGN KEY (agent_turn_id, execution_epoch, operation_lineage_id) REFERENCES public.agent_turns(id, execution_epoch, operation_lineage_id);

ALTER TABLE ONLY public.webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_workflow_fk FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.workflow_action_failures
    ADD CONSTRAINT workflow_action_failures_escalation_job_id_fkey FOREIGN KEY (escalation_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.workflow_action_failures
    ADD CONSTRAINT workflow_action_failures_source_job_id_fkey FOREIGN KEY (source_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.workflow_action_failures
    ADD CONSTRAINT workflow_action_failures_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.workflow_attempts
    ADD CONSTRAINT workflow_attempts_trigger_delivery_id_fkey FOREIGN KEY (trigger_delivery_id) REFERENCES public.webhook_deliveries(delivery_id);

ALTER TABLE ONLY public.workflow_attempts
    ADD CONSTRAINT workflow_attempts_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_attempt_fk FOREIGN KEY (source_attempt_id, workflow_id) REFERENCES public.workflow_attempts(id, workflow_id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_internal_event_id_fkey FOREIGN KEY (internal_event_id) REFERENCES public.workflow_internal_events(id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_settlement_job_id_fkey FOREIGN KEY (settlement_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_stop_job_id_fkey FOREIGN KEY (stop_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_turn_fk FOREIGN KEY (source_turn_id, source_session_id, source_execution_epoch) REFERENCES public.agent_turns(id, agent_session_id, execution_epoch);

ALTER TABLE ONLY public.workflow_closure_barriers
    ADD CONSTRAINT workflow_closure_barriers_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.workflow_github_effect_cleanups
    ADD CONSTRAINT workflow_github_effect_cleanups_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.workflow_github_effect_cleanups
    ADD CONSTRAINT workflow_github_effect_cleanups_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.workflow_internal_events
    ADD CONSTRAINT workflow_internal_events_source_attempt_fk FOREIGN KEY (source_job_id, source_attempt_number) REFERENCES public.job_attempts(job_id, attempt_number);

ALTER TABLE ONLY public.workflow_internal_events
    ADD CONSTRAINT workflow_internal_events_source_job_id_fkey FOREIGN KEY (source_job_id) REFERENCES public.jobs(id);

ALTER TABLE ONLY public.workflow_internal_events
    ADD CONSTRAINT workflow_internal_events_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id);

ALTER TABLE ONLY public.workspace_captures
    ADD CONSTRAINT workspace_captures_agent_turn_id_execution_epoch_fkey FOREIGN KEY (agent_turn_id, execution_epoch) REFERENCES public.agent_turns(id, execution_epoch);
