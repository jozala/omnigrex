ALTER TABLE agent_assignments
    DROP CONSTRAINT agent_assignments_role_check,
    ADD CONSTRAINT agent_participants_role_syntax_check
        CHECK (role ~ '^[A-Z][A-Z0-9_]{0,63}$');

ALTER TABLE stage_assignments
    DROP CONSTRAINT stage_assignments_role_check,
    ADD CONSTRAINT stage_assignments_role_syntax_check
        CHECK (role ~ '^[A-Z][A-Z0-9_]{0,63}$');

ALTER TABLE workflows
    DROP CONSTRAINT workflows_resume_role_check,
    ADD CONSTRAINT workflows_resume_role_syntax_check
        CHECK (resume_role IS NULL OR resume_role ~ '^[A-Z][A-Z0-9_]{0,63}$');

ALTER TABLE workflow_action_failures
    DROP CONSTRAINT workflow_action_failures_resume_role_check,
    ADD CONSTRAINT workflow_action_failures_resume_role_syntax_check
        CHECK (resume_role IS NULL OR resume_role ~ '^[A-Z][A-Z0-9_]{0,63}$');

ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_recovery_continuation_check,
    ADD CONSTRAINT agent_turns_recovery_continuation_check CHECK (recovery_continuation IN (
        'PENDING_INFRASTRUCTURE_FAILURE',
        'INFRASTRUCTURE_FAILURE_APPLIED',
        'MUTATION_RECONCILIATION_HANDOFF_APPLIED',
        'MIGRATION_HANDOFF_APPLIED',
        'WORKFLOW_DEFINITION_HANDOFF_APPLIED'
    ));
