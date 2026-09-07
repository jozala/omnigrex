//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	postgresImage    = "postgres:18-alpine@sha256:b40d931bd0e7ce6eecc59a5a6ac3b3c04a01e559750e73e7086b6dbd7f8bf545"
	postgresUser     = "omnigrex"
	postgresPassword = "integration-secret"
	postgresDatabase = "omnigrex"
)

func TestPhaseSevenMigrationPreservesRetryLineagesAndScopesCallerKeys(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
		"000006_agent_turn_preparation.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	fixture := seedAgentSession(t, pool, 35)
	if _, err := pool.Exec(ctx, `
	INSERT INTO agent_turns (
	    id, workflow_id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
	    retry_of_turn_id, status, active, control_revision, agent_profile_commit_sha,
	    agent_profile_content_sha256, agent_profile_config, purpose, completed_at
	)
	VALUES ('6e000000-0000-4000-8000-000000000001', $1, $2, $3, 1, 1, NULL,
	        'FAILED', FALSE, 1, 'legacy-profile', decode(repeat('11', 32), 'hex'),
	        '{}'::jsonb, 'INITIAL_DEVELOPMENT', clock_timestamp()),
	       ('6e000000-0000-4000-8000-000000000002', $1, $2, $3, 2, 2,
	        '6e000000-0000-4000-8000-000000000001', 'FAILED', FALSE, 1, 'legacy-profile',
	        decode(repeat('11', 32), 'hex'), '{}'::jsonb, 'RETRY', clock_timestamp()),
	       ('6e000000-0000-4000-8000-000000000003', $1, $2, $3, 3, 3,
	        '6e000000-0000-4000-8000-000000000002', 'SUCCEEDED', FALSE, 1, 'legacy-profile',
	        decode(repeat('11', 32), 'hex'), '{}'::jsonb, 'RETRY', clock_timestamp()),
	       ('6e000000-0000-4000-8000-000000000004', $1, $2, $3, 4, 4, NULL,
	        'SUCCEEDED', FALSE, 1, 'legacy-profile', decode(repeat('11', 32), 'hex'),
	        '{}'::jsonb, 'REQUESTED_CHANGES', clock_timestamp())`, fixture.workflowID, fixture.sessionID, fixture.attemptID); err != nil {
		t.Fatalf("seed version-6 Agent Turns: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO tool_invocations (
    id, agent_turn_id, execution_epoch, invocation_number, tool_name, kind, state,
    idempotency_key, operation_id, request, result
)
VALUES ('6e000000-0000-4000-8000-000000000011', '6e000000-0000-4000-8000-000000000001',
        1, 1, 'comment_on_issue', 'MUTATION', 'SUCCEEDED', 'legacy-key-1', 'legacy-key-1',
        '{"body":"first"}'::jsonb, '{"comment_id":1}'::jsonb),
	       ('6e000000-0000-4000-8000-000000000012', '6e000000-0000-4000-8000-000000000004',
	        4, 1, 'comment_on_issue', 'MUTATION', 'SUCCEEDED', 'legacy-key-2', 'legacy-key-2',
	        '{"body":"second"}'::jsonb, '{"comment_id":2}'::jsonb)`); err != nil {
		t.Fatalf("seed version-6 invocations: %v", err)
	}

	contents, err := migrations.Files.ReadFile("000007_scoped_mutation_operations.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000007: %v", err)
	}

	var preserved string
	if err := pool.QueryRow(ctx, `
SELECT string_agg(operation_id || ':' || request::text || ':' || result::text, '|' ORDER BY invocation_number, id)
FROM tool_invocations`).Scan(&preserved); err != nil {
		t.Fatalf("read migrated invocations: %v", err)
	}
	if preserved != `legacy-key-1:{"body": "first"}:{"comment_id": 1}|legacy-key-2:{"body": "second"}:{"comment_id": 2}` {
		t.Errorf("migrated invocations = %q, want legacy values unchanged", preserved)
	}

	for indexName, columns := range map[string]string{
		"tool_invocations_operation_id_idx": "(operation_lineage_id, operation_id)",
		"tool_invocations_idempotency_idx":  "(agent_turn_id, execution_epoch, idempotency_key)",
	} {
		var definition string
		if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, indexName).Scan(&definition); err != nil {
			t.Fatalf("read %s: %v", indexName, err)
		}
		if !strings.Contains(definition, "UNIQUE INDEX") || !strings.Contains(definition, columns) {
			t.Errorf("%s = %q, want scoped unique columns %s", indexName, definition, columns)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE tool_invocations SET operation_id = 'shared-caller-key', idempotency_key = 'shared-caller-key'`); err != nil {
		t.Fatalf("reuse caller key across migrated turn epochs: %v", err)
	}
	var lineages string
	if err := pool.QueryRow(ctx, `
SELECT string_agg(id::text || ':' || operation_lineage_id::text, '|' ORDER BY execution_epoch)
FROM agent_turns`).Scan(&lineages); err != nil {
		t.Fatal(err)
	}
	if lineages != "6e000000-0000-4000-8000-000000000001:6e000000-0000-4000-8000-000000000001|"+
		"6e000000-0000-4000-8000-000000000002:6e000000-0000-4000-8000-000000000001|"+
		"6e000000-0000-4000-8000-000000000003:6e000000-0000-4000-8000-000000000001|"+
		"6e000000-0000-4000-8000-000000000004:6e000000-0000-4000-8000-000000000004" {
		t.Errorf("migrated operation lineages = %q", lineages)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO tool_invocations (
    id, agent_turn_id, execution_epoch, operation_lineage_id, invocation_number,
    tool_name, kind, state, operation_id, request
)
VALUES ('6e000000-0000-4000-8000-000000000013',
        '6e000000-0000-4000-8000-000000000002', 2,
        '6e000000-0000-4000-8000-000000000001', 1,
        'comment_on_issue', 'MUTATION', 'RESERVED', 'shared-caller-key', '{}'::jsonb)`); err == nil {
		t.Fatal("duplicate operation ID in migrated retry lineage succeeded")
	}
}

func TestPhaseSixMigrationPreservesExistingRuntimeStatePaths(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ('66000000-0000-4000-8000-000000000001', 1, 'owner', 'repo', 1, 1, 'ACTIVE'),
       ('66000000-0000-4000-8000-000000000002', 2, 'owner', 'repo', 2, 2, 'ACTIVE')`); err != nil {
		t.Fatalf("seed legacy Workflows: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('66000000-0000-4000-8000-000000000011', '66000000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:one', 'legacy/custom-runtime-state'),
       ('66000000-0000-4000-8000-000000000012', '66000000-0000-4000-8000-000000000002',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:two', NULL)`); err != nil {
		t.Fatalf("seed legacy Assignments: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('66000000-0000-4000-8000-000000000021', '66000000-0000-4000-8000-000000000011',
        1, 'legacy-one', 'runtime', '1', 'sha256:one', 'legacy/custom-runtime-state', 'ACTIVE'),
       ('66000000-0000-4000-8000-000000000022', '66000000-0000-4000-8000-000000000012',
        1, 'legacy-two', 'runtime', '1', 'sha256:two', 'legacy/discovered-runtime-state', 'ACTIVE')`); err != nil {
		t.Fatalf("seed legacy Sessions: %v", err)
	}
	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006: %v", err)
	}

	for assignmentID, want := range map[string]string{
		"66000000-0000-4000-8000-000000000011": "legacy/custom-runtime-state",
		"66000000-0000-4000-8000-000000000012": "legacy/discovered-runtime-state",
	} {
		var assignmentPath, sessionPath string
		var assignmentHashMissing, sessionHashMissing bool
		if err := pool.QueryRow(ctx, `
SELECT assignment.runtime_state_path, session.runtime_state_path,
       assignment.runtime_profile_content_sha256 IS NULL,
       session.runtime_profile_content_sha256 IS NULL
FROM agent_assignments AS assignment
JOIN agent_sessions AS session ON session.agent_assignment_id = assignment.id
WHERE assignment.id = $1`, assignmentID).Scan(&assignmentPath, &sessionPath, &assignmentHashMissing, &sessionHashMissing); err != nil {
			t.Fatalf("read migrated runtime state path: %v", err)
		}
		if assignmentPath != want || sessionPath != want {
			t.Errorf("runtime state paths for %s = (%q, %q), want preserved %q", assignmentID, assignmentPath, sessionPath, want)
		}
		if !assignmentHashMissing || !sessionHashMissing {
			t.Errorf("legacy Runtime Profile hashes for %s were invented", assignmentID)
		}
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments
SET runtime_profile_content_sha256 = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
WHERE id = '66000000-0000-4000-8000-000000000011'`); err == nil {
		t.Fatal("uppercase Assignment Runtime Profile hash was accepted")
	}
	var purposeNullable bool
	if err := pool.QueryRow(ctx, `
SELECT is_nullable = 'YES'
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'agent_turns' AND column_name = 'purpose'`).Scan(&purposeNullable); err != nil {
		t.Fatalf("read Agent Turn purpose nullability: %v", err)
	}
	if !purposeNullable {
		t.Error("legacy Agent Turn purpose column is not nullable")
	}
	var indexDefinition string
	if err := pool.QueryRow(ctx, `
SELECT indexdef FROM pg_indexes
WHERE schemaname = 'public' AND indexname = 'jobs_available_kind_idx'`).Scan(&indexDefinition); err != nil {
		t.Fatalf("read kind-aware ClaimJobKind index: %v", err)
	}
	if !strings.Contains(indexDefinition, "(queue, kind, priority DESC, available_at, id)") ||
		!strings.Contains(indexDefinition, "WHERE (status = 'AVAILABLE'::text)") {
		t.Errorf("jobs_available_kind_idx = %q, want ClaimJobKind columns and AVAILABLE predicate", indexDefinition)
	}
}

func TestPhaseSixMigrationHandsOffDuplicateRuntimeStatePathsWithoutRewritingDurableState(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state
)
VALUES ('6c000000-0000-4000-8000-000000000001', 1, 'owner', 'repo', 1, 1,
        'ACTIVE', 2, 'ACTIVE', 'ACTIVE'),
       ('6c000000-0000-4000-8000-000000000002', 1, 'owner', 'repo', 2, 2,
        'REVIEWING', 4, 'ACTIVE', 'ACTIVE');

INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ('6c000000-0000-4000-8000-000000000010', '6c000000-0000-4000-8000-000000000002', 1, 'ACTIVE');

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, completed_at
)
VALUES ('6c000000-0000-4000-8000-000000000011', '6c000000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:developer',
        'legacy/shared-runtime-state', NULL),
       ('6c000000-0000-4000-8000-000000000012', '6c000000-0000-4000-8000-000000000002',
        'REVIEWER', 'COMPLETED', 'reviewer', 'runtime', '1', 'sha256:reviewer',
        'legacy/shared-runtime-state', clock_timestamp());
`); err != nil {
		t.Fatalf("seed duplicate legacy Runtime State paths: %v", err)
	}

	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006 with duplicate Runtime State paths: %v", err)
	}

	rows, err := pool.Query(ctx, `
SELECT workflow.id::text, workflow.status, workflow.state_revision,
       workflow.resume_role, workflow.human_handoff_reason,
       assignment.status, assignment.runtime_state_path,
       attempt.active, attempt.human_handoff_reason,
       (SELECT count(*) FROM jobs
        WHERE workflow_id = workflow.id
          AND workflow_attempt_id = attempt.id
          AND status = 'AVAILABLE'
          AND kind IN ('PUBLISH_HUMAN_HANDOFF', 'RECONCILE_GITHUB_LABELS')),
       (SELECT COALESCE(string_agg(payload::text, ' '), '') FROM jobs
        WHERE workflow_id = workflow.id AND kind = 'PUBLISH_HUMAN_HANDOFF')
FROM workflows AS workflow
JOIN agent_assignments AS assignment ON assignment.workflow_id = workflow.id
JOIN workflow_attempts AS attempt ON attempt.workflow_id = workflow.id AND attempt.active
WHERE workflow.id IN (
    '6c000000-0000-4000-8000-000000000001',
    '6c000000-0000-4000-8000-000000000002'
)
ORDER BY workflow.id`)
	if err != nil {
		t.Fatalf("read duplicate-path migration handoffs: %v", err)
	}
	defer rows.Close()
	wantRevision := map[string]int64{
		"6c000000-0000-4000-8000-000000000001": 3,
		"6c000000-0000-4000-8000-000000000002": 5,
	}
	wantRole := map[string]string{
		"6c000000-0000-4000-8000-000000000001": "DEVELOPER",
		"6c000000-0000-4000-8000-000000000002": "REVIEWER",
	}
	seen := 0
	for rows.Next() {
		var workflowID, status, resumeRole, workflowReason, assignmentStatus, path, attemptReason, outboxPayload string
		var revision int64
		var attemptActive bool
		var outboxCount int
		if err := rows.Scan(&workflowID, &status, &revision, &resumeRole, &workflowReason,
			&assignmentStatus, &path, &attemptActive, &attemptReason, &outboxCount, &outboxPayload); err != nil {
			t.Fatalf("scan duplicate-path migration handoff: %v", err)
		}
		seen++
		if status != "NEEDS_HUMAN" || revision != wantRevision[workflowID] || resumeRole != wantRole[workflowID] ||
			workflowReason != "migration_duplicate_runtime_state_path" || assignmentStatus != "WAITING_FOR_HUMAN" ||
			path != "legacy/shared-runtime-state" || !attemptActive || attemptReason != workflowReason || outboxCount != 2 ||
			!strings.Contains(outboxPayload, workflowReason) || !strings.Contains(outboxPayload, "legacy/shared-runtime-state") {
			t.Errorf("duplicate-path migration for %s = Workflow %s@%d resume %s reason %q, Assignment %s path %q, attempt %v/%q, outbox %d %q",
				workflowID, status, revision, resumeRole, workflowReason, assignmentStatus, path,
				attemptActive, attemptReason, outboxCount, outboxPayload)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("duplicate-path migration handoffs = %d, want 2", seen)
	}

	var indexDefinition string
	if err := pool.QueryRow(ctx, `
SELECT indexdef FROM pg_indexes
WHERE schemaname = 'public' AND indexname = 'agent_assignments_active_runtime_state_path_idx'`).Scan(&indexDefinition); err != nil {
		t.Fatalf("read active Runtime State path index: %v", err)
	}
	if !strings.Contains(indexDefinition, "UNIQUE") || !strings.Contains(indexDefinition, "runtime_state_path") ||
		!strings.Contains(indexDefinition, "status = 'ACTIVE'::text") || !strings.Contains(indexDefinition, "state_deleted_at IS NULL") {
		t.Errorf("active Runtime State path index = %q", indexDefinition)
	}

	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments SET status = 'ACTIVE'
WHERE id = '6c000000-0000-4000-8000-000000000011'`); err != nil {
		t.Fatalf("activate first isolated Assignment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments SET status = 'ACTIVE', completed_at = NULL
WHERE id = '6c000000-0000-4000-8000-000000000012'`); err == nil {
		t.Fatal("reactivating a second usable Assignment with the same Runtime State path succeeded")
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments
SET status = 'ACTIVE', completed_at = NULL, state_deleted_at = clock_timestamp()
WHERE id = '6c000000-0000-4000-8000-000000000012'`); err != nil {
		t.Fatalf("activate state-deleted Assignment outside usable-path uniqueness: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments SET state_deleted_at = NULL
WHERE id = '6c000000-0000-4000-8000-000000000012'`); err == nil {
		t.Fatal("making a colliding active Assignment usable succeeded")
	}
}

func TestPhaseSixMigrationRetainsNewestCompatibleActiveTurnWhenSupersededTurnsHaveNoUncertainMutations(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ('67000000-0000-4000-8000-000000000001', 1, 'owner', 'repo', 1, 1, 'REVIEWING');

INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ('67000000-0000-4000-8000-000000000010', '67000000-0000-4000-8000-000000000001', 1, 'ACTIVE');

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('67000000-0000-4000-8000-000000000011', '67000000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:developer', 'legacy/developer'),
       ('67000000-0000-4000-8000-000000000012', '67000000-0000-4000-8000-000000000001',
        'REVIEWER', 'ACTIVE', 'reviewer', 'runtime', '1', 'sha256:reviewer', 'legacy/reviewer');

INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('67000000-0000-4000-8000-000000000021', '67000000-0000-4000-8000-000000000011',
        1, 'legacy-developer', 'runtime', '1', 'sha256:developer', 'legacy/developer', 'ACTIVE'),
       ('67000000-0000-4000-8000-000000000022', '67000000-0000-4000-8000-000000000012',
        1, 'legacy-reviewer', 'runtime', '1', 'sha256:reviewer', 'legacy/reviewer', 'ACTIVE');

INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ('67000000-0000-4000-8000-000000000051', '67000000-0000-4000-8000-000000000001',
        1, 'owner', 'repo', 51, 51, 'OPEN', 'main', 'base-sha', 'feature', 'review-head');

INSERT INTO agent_turns (
    id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    status, active, control_revision, agent_profile_commit_sha,
    agent_profile_content_sha256, purpose, owner_id, owner_token, leased_at,
    lease_expires_at, mutation_admission_open, change_proposal_id, expected_head_sha,
    created_at, started_at
)
VALUES ('67000000-0000-4000-8000-000000000031', '67000000-0000-4000-8000-000000000021',
        '67000000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'legacy-developer',
        decode(repeat('11', 32), 'hex'), 'REVIEW', 'developer-runtime',
        '67000000-0000-4000-8000-000000000071', clock_timestamp(), clock_timestamp() + interval '1 hour',
        TRUE, NULL, NULL, '2026-01-01 00:00:01+00', '2026-01-01 00:00:01+00'),
       ('67000000-0000-4000-8000-000000000032', '67000000-0000-4000-8000-000000000022',
        '67000000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'legacy-reviewer',
        decode(repeat('22', 32), 'hex'), NULL, 'reviewer-runtime',
        '67000000-0000-4000-8000-000000000072', clock_timestamp(), clock_timestamp() + interval '1 hour',
        TRUE, '67000000-0000-4000-8000-000000000051', 'review-head',
        '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00');

INSERT INTO jobs (
    id, queue, kind, status, attempt_count, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, lease_owner, lease_token, leased_at, lease_expires_at
)
VALUES ('67000000-0000-4000-8000-000000000041', 'agent-turn', 'RUN_AGENT_TURN', 'LEASED', 1, 1,
        'legacy-run-developer', '67000000-0000-4000-8000-000000000001',
        '67000000-0000-4000-8000-000000000010', '67000000-0000-4000-8000-000000000011',
        '67000000-0000-4000-8000-000000000021', '67000000-0000-4000-8000-000000000031', 1,
        'developer-worker', '67000000-0000-4000-8000-000000000081', clock_timestamp(), clock_timestamp() + interval '1 hour'),
       ('67000000-0000-4000-8000-000000000042', 'agent-turn', 'RUN_AGENT_TURN', 'LEASED', 1, 1,
        'legacy-run-reviewer', '67000000-0000-4000-8000-000000000001',
        '67000000-0000-4000-8000-000000000010', '67000000-0000-4000-8000-000000000012',
        '67000000-0000-4000-8000-000000000022', '67000000-0000-4000-8000-000000000032', 1,
        'reviewer-worker', '67000000-0000-4000-8000-000000000082', clock_timestamp(), clock_timestamp() + interval '1 hour');

INSERT INTO job_attempts (job_id, attempt_number, lease_owner, lease_token, status, leased_at, lease_expires_at)
VALUES ('67000000-0000-4000-8000-000000000041', 1, 'developer-worker',
        '67000000-0000-4000-8000-000000000081', 'LEASED', clock_timestamp(), clock_timestamp() + interval '1 hour'),
       ('67000000-0000-4000-8000-000000000042', 1, 'reviewer-worker',
        '67000000-0000-4000-8000-000000000082', 'LEASED', clock_timestamp(), clock_timestamp() + interval '1 hour');

INSERT INTO agent_turn_slots (
    agent_turn_id, agent_session_id, execution_epoch, control_revision,
    owner_id, owner_token, lease_expires_at
)
VALUES ('67000000-0000-4000-8000-000000000031', '67000000-0000-4000-8000-000000000021',
        1, 1, 'developer-runtime', '67000000-0000-4000-8000-000000000071', clock_timestamp() + interval '1 hour'),
       ('67000000-0000-4000-8000-000000000032', '67000000-0000-4000-8000-000000000022',
        1, 1, 'reviewer-runtime', '67000000-0000-4000-8000-000000000072', clock_timestamp() + interval '1 hour');

INSERT INTO tool_invocations (
    id, agent_turn_id, execution_epoch, invocation_number, tool_name, kind, state,
    operation_id, request, started_at
)
VALUES ('67000000-0000-4000-8000-000000000061', '67000000-0000-4000-8000-000000000031',
         1, 1, 'create_issue_comment', 'MUTATION', 'RESERVED', 'legacy-reserved', '{}'::jsonb, NULL),
       ('67000000-0000-4000-8000-000000000062', '67000000-0000-4000-8000-000000000031',
         1, 2, 'update_pull_request', 'MUTATION', 'RESERVED', 'legacy-reserved-two', '{}'::jsonb, NULL);
`); err != nil {
		t.Fatalf("seed simultaneous legacy active Agent Turns: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ('67d00000-0000-4000-8000-000000000001', 2, 'owner', 'repo', 2, 2, 'DEVELOPING');

INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ('67d00000-0000-4000-8000-000000000010', '67d00000-0000-4000-8000-000000000001', 1, 'ACTIVE');

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('67d00000-0000-4000-8000-000000000011', '67d00000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:developer', 'deferred/developer'),
       ('67d00000-0000-4000-8000-000000000012', '67d00000-0000-4000-8000-000000000001',
        'REVIEWER', 'ACTIVE', 'reviewer', 'runtime', '1', 'sha256:reviewer', 'deferred/reviewer');

INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('67d00000-0000-4000-8000-000000000021', '67d00000-0000-4000-8000-000000000011',
        1, 'deferred-developer', 'runtime', '1', 'sha256:developer', 'deferred/developer', 'ACTIVE'),
       ('67d00000-0000-4000-8000-000000000022', '67d00000-0000-4000-8000-000000000012',
        1, 'deferred-reviewer', 'runtime', '1', 'sha256:reviewer', 'deferred/reviewer', 'ACTIVE');

INSERT INTO agent_turns (
    id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    status, active, control_revision, agent_profile_commit_sha,
    agent_profile_content_sha256, mutation_admission_open, created_at, started_at
)
VALUES ('67d00000-0000-4000-8000-000000000031', '67d00000-0000-4000-8000-000000000021',
        '67d00000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'deferred-developer',
        decode(repeat('11', 32), 'hex'), TRUE, '2026-01-01 00:00:01+00', '2026-01-01 00:00:01+00'),
       ('67d00000-0000-4000-8000-000000000032', '67d00000-0000-4000-8000-000000000022',
        '67d00000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'deferred-reviewer',
        decode(repeat('22', 32), 'hex'), TRUE, '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00');

INSERT INTO jobs (
    id, queue, kind, status, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch
)
VALUES ('67d00000-0000-4000-8000-000000000041', 'agent-turns', 'RUN_AGENT_TURN', 'AVAILABLE', 1,
        'deferred-run-developer', '67d00000-0000-4000-8000-000000000001',
        '67d00000-0000-4000-8000-000000000010', '67d00000-0000-4000-8000-000000000011',
        '67d00000-0000-4000-8000-000000000021', '67d00000-0000-4000-8000-000000000031', 1),
       ('67d00000-0000-4000-8000-000000000042', 'agent-turns', 'RUN_AGENT_TURN', 'AVAILABLE', 1,
        'deferred-run-reviewer', '67d00000-0000-4000-8000-000000000001',
        '67d00000-0000-4000-8000-000000000010', '67d00000-0000-4000-8000-000000000012',
        '67d00000-0000-4000-8000-000000000022', '67d00000-0000-4000-8000-000000000032', 1);

INSERT INTO webhook_deliveries (
    delivery_id, event_name, repository_id, repository_owner, repository_name,
    issue_id, issue_number, workflow_id, payload, status, processed_at
)
VALUES ('67d00000-0000-4000-8000-000000000091', 'issues', 2, 'owner', 'repo', 2, 2,
        '67d00000-0000-4000-8000-000000000001', '{}'::bytea, 'PROCESSED', clock_timestamp());

INSERT INTO normalized_events (
    delivery_id, payload, status, workflow_id, disposition, reason,
    applied_revision, deferred_for_turn_id, processed_at
)
VALUES ('67d00000-0000-4000-8000-000000000091',
        '{"delivery_id":"67d00000-0000-4000-8000-000000000091","kind":"trigger"}'::jsonb,
        'DEFERRED', '67d00000-0000-4000-8000-000000000001', 'DEFERRED',
        'active_turn', 1, '67d00000-0000-4000-8000-000000000032', clock_timestamp());
`); err != nil {
		t.Fatalf("seed deferred event fenced to superseded Agent Turn: %v", err)
	}

	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006: %v", err)
	}

	var activeCount int
	var activeTurnID string
	if err := pool.QueryRow(ctx, `
SELECT count(*), min(id::text) FROM agent_turns
WHERE workflow_id = '67000000-0000-4000-8000-000000000001' AND active`).Scan(&activeCount, &activeTurnID); err != nil {
		t.Fatalf("read migrated active Agent Turn: %v", err)
	}
	if activeCount != 1 || activeTurnID != "67000000-0000-4000-8000-000000000032" {
		t.Fatalf("migrated active Agent Turns = (%d, %s), want older aggregate-compatible Reviewer Turn", activeCount, activeTurnID)
	}
	var workflowStatus string
	var handoffJobs int
	if err := pool.QueryRow(ctx, `
SELECT status,
       (SELECT count(*) FROM jobs
        WHERE workflow_id = '67000000-0000-4000-8000-000000000001'
          AND kind = 'PUBLISH_HUMAN_HANDOFF')
FROM workflows
WHERE id = '67000000-0000-4000-8000-000000000001'`).Scan(&workflowStatus, &handoffJobs); err != nil {
		t.Fatalf("read migrated compatible Workflow: %v", err)
	}
	if workflowStatus != "REVIEWING" || handoffJobs != 0 {
		t.Errorf("migrated compatible Workflow = %s with %d Human Handoff Jobs, want REVIEWING with none", workflowStatus, handoffJobs)
	}
	var survivorRecoveryFields, survivorRecoveryJobs int
	if err := pool.QueryRow(ctx, `
SELECT
    (SELECT count(*) FROM agent_turns
     WHERE id = '67000000-0000-4000-8000-000000000032'
       AND (recovery_started_at IS NOT NULL OR runtime_stop_required
            OR stop_runtime_job_id IS NOT NULL OR reconcile_mutations_job_id IS NOT NULL)),
    (SELECT count(*) FROM jobs
     WHERE agent_turn_id = '67000000-0000-4000-8000-000000000032'
       AND kind IN ('STOP_STALE_RUNTIME', 'RECONCILE_AGENT_TURN_MUTATIONS'))`).Scan(
		&survivorRecoveryFields, &survivorRecoveryJobs,
	); err != nil {
		t.Fatalf("read compatible survivor recovery state: %v", err)
	}
	if survivorRecoveryFields != 0 || survivorRecoveryJobs != 0 {
		t.Errorf("compatible survivor recovery state = %d fields, %d Jobs; want unchanged", survivorRecoveryFields, survivorRecoveryJobs)
	}

	var deferredWorkflowStatus, deferredHandoffReason, deferredEventStatus string
	var deferredActiveTurns, deferredRecoveryBarriers int
	var deferredEventCleared bool
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.human_handoff_reason,
       (SELECT count(*) FROM agent_turns WHERE workflow_id = workflow.id AND active),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = workflow.id
        AND status = 'INTERRUPTED' AND recovery_started_at IS NOT NULL
        AND runtime_stop_required AND recovery_settled_at IS NULL),
       event.status,
       event.workflow_id IS NULL AND event.disposition IS NULL AND event.reason IS NULL
           AND event.applied_revision IS NULL AND event.deferred_for_turn_id IS NULL
           AND event.processed_at IS NULL
FROM workflows AS workflow
JOIN normalized_events AS event
  ON event.delivery_id = '67d00000-0000-4000-8000-000000000091'
WHERE workflow.id = '67d00000-0000-4000-8000-000000000001'`).Scan(
		&deferredWorkflowStatus, &deferredHandoffReason, &deferredActiveTurns,
		&deferredRecoveryBarriers, &deferredEventStatus, &deferredEventCleared,
	); err != nil {
		t.Fatalf("read deferred-event migration handoff: %v", err)
	}
	if deferredWorkflowStatus != "NEEDS_HUMAN" || deferredHandoffReason != "migration_interrupted_turn_deferred_event" ||
		deferredActiveTurns != 0 || deferredRecoveryBarriers != 2 || deferredEventStatus != "PENDING" || !deferredEventCleared {
		t.Errorf("deferred-event migration = Workflow %s reason %q, active %d, recoveries %d, event %s cleared %v",
			deferredWorkflowStatus, deferredHandoffReason, deferredActiveTurns,
			deferredRecoveryBarriers, deferredEventStatus, deferredEventCleared)
	}

	var status, lastError string
	var active, admissionOpen, ownerMissing, leaseMissing, admissionClosed, completed bool
	if err := pool.QueryRow(ctx, `
SELECT status, active, mutation_admission_open, owner_id IS NULL AND owner_token IS NULL,
       leased_at IS NULL AND lease_expires_at IS NULL AND heartbeat_at IS NULL,
       mutation_admission_closed_at IS NOT NULL, completed_at IS NOT NULL, COALESCE(last_error, '')
FROM agent_turns WHERE id = '67000000-0000-4000-8000-000000000031'`).Scan(
		&status, &active, &admissionOpen, &ownerMissing, &leaseMissing, &admissionClosed, &completed, &lastError,
	); err != nil {
		t.Fatalf("read superseded Agent Turn: %v", err)
	}
	if status != "INTERRUPTED" || active || admissionOpen || !ownerMissing || !leaseMissing || !admissionClosed || !completed || !strings.Contains(lastError, "Phase 6 migration") {
		t.Errorf("superseded Agent Turn = (%s, active %v, admission %v, owner missing %v, lease missing %v, admission closed %v, completed %v, error %q)",
			status, active, admissionOpen, ownerMissing, leaseMissing, admissionClosed, completed, lastError)
	}
	var recoveryStarted, runtimeStopRequired, runtimeUnstopped, recoveryUnsettled bool
	var stopJobID string
	if err := pool.QueryRow(ctx, `
SELECT recovery_started_at IS NOT NULL, runtime_stop_required,
       runtime_stopped_at IS NULL, recovery_settled_at IS NULL,
       stop_runtime_job_id::text
FROM agent_turns
WHERE id = '67000000-0000-4000-8000-000000000031'
  AND reconcile_mutations_job_id IS NULL`).Scan(
		&recoveryStarted, &runtimeStopRequired, &runtimeUnstopped, &recoveryUnsettled, &stopJobID,
	); err != nil {
		t.Fatalf("read superseded Agent Turn recovery barrier: %v", err)
	}
	if !recoveryStarted || !runtimeStopRequired || !runtimeUnstopped || !recoveryUnsettled || stopJobID == "" {
		t.Errorf("superseded Agent Turn recovery barrier = started %v stop required %v unstopped %v unsettled %v job %q",
			recoveryStarted, runtimeStopRequired, runtimeUnstopped, recoveryUnsettled, stopJobID)
	}
	var recoveryQueue, recoveryKind, recoveryStatus, recoveryKey, executionJobID string
	var recoveryPriority, recoveryMaxAttempts int
	var recoveryScopeMatches, recoveryPayloadMatches bool
	if err := pool.QueryRow(ctx, `
SELECT queue, kind, status, idempotency_key, priority, max_attempts,
       workflow_id = '67000000-0000-4000-8000-000000000001'
           AND workflow_attempt_id = '67000000-0000-4000-8000-000000000010'
           AND agent_assignment_id = '67000000-0000-4000-8000-000000000011'
           AND agent_session_id = '67000000-0000-4000-8000-000000000021'
           AND agent_turn_id = '67000000-0000-4000-8000-000000000031'
           AND execution_epoch = 1,
       payload ->> 'execution_job_id',
       payload = jsonb_build_object(
           'workflow_id', '67000000-0000-4000-8000-000000000001',
           'workflow_attempt_id', '67000000-0000-4000-8000-000000000010',
           'agent_assignment_id', '67000000-0000-4000-8000-000000000011',
           'agent_session_id', '67000000-0000-4000-8000-000000000021',
           'agent_turn_id', '67000000-0000-4000-8000-000000000031',
           'execution_epoch', 1,
           'control_revision', 1,
           'execution_job_id', '67000000-0000-4000-8000-000000000041'
       )
FROM jobs WHERE id = $1`, stopJobID).Scan(
		&recoveryQueue, &recoveryKind, &recoveryStatus, &recoveryKey,
		&recoveryPriority, &recoveryMaxAttempts, &recoveryScopeMatches,
		&executionJobID, &recoveryPayloadMatches,
	); err != nil {
		t.Fatalf("read superseded Agent Turn recovery Job: %v", err)
	}
	if recoveryQueue != store.AgentTurnRecoveryQueue || recoveryKind != store.StopStaleRuntimeJobKind ||
		recoveryStatus != "AVAILABLE" || recoveryKey != "agent-turn-recovery:67000000-0000-4000-8000-000000000031:epoch:1:stop_stale_runtime" ||
		recoveryPriority != 100 || recoveryMaxAttempts != 3 || !recoveryScopeMatches ||
		executionJobID != "67000000-0000-4000-8000-000000000041" || !recoveryPayloadMatches {
		t.Errorf("superseded recovery Job = %s/%s/%s key %q priority %d attempts %d scope %v execution %q payload %v",
			recoveryQueue, recoveryKind, recoveryStatus, recoveryKey, recoveryPriority,
			recoveryMaxAttempts, recoveryScopeMatches, executionJobID, recoveryPayloadMatches)
	}

	var jobStatus, attemptStatus, jobError, attemptError string
	var jobLeaseMissing, jobCompleted, attemptFinished, retryable bool
	if err := pool.QueryRow(ctx, `
SELECT job.status, job.lease_owner IS NULL AND job.lease_token IS NULL
           AND job.leased_at IS NULL AND job.lease_expires_at IS NULL,
       job.completed_at IS NOT NULL, COALESCE(job.last_error, ''),
       attempt.status, attempt.finished_at IS NOT NULL, COALESCE(attempt.retryable, TRUE),
       COALESCE(attempt.last_error, '')
FROM jobs AS job
JOIN job_attempts AS attempt ON attempt.job_id = job.id AND attempt.attempt_number = 1
WHERE job.id = '67000000-0000-4000-8000-000000000041'`).Scan(
		&jobStatus, &jobLeaseMissing, &jobCompleted, &jobError,
		&attemptStatus, &attemptFinished, &retryable, &attemptError,
	); err != nil {
		t.Fatalf("read superseded execution Job: %v", err)
	}
	if jobStatus != "CANCELLED" || !jobLeaseMissing || !jobCompleted || attemptStatus != "FAILED" || !attemptFinished || retryable ||
		!strings.Contains(jobError, "Phase 6 migration") || !strings.Contains(attemptError, "Phase 6 migration") {
		t.Errorf("superseded execution Job = (%s, lease missing %v, completed %v, error %q), attempt = (%s, finished %v, retryable %v, error %q)",
			jobStatus, jobLeaseMissing, jobCompleted, jobError, attemptStatus, attemptFinished, retryable, attemptError)
	}

	var supersededSlots, survivorSlots int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE agent_turn_id = '67000000-0000-4000-8000-000000000031'),
       count(*) FILTER (WHERE agent_turn_id = '67000000-0000-4000-8000-000000000032')
FROM agent_turn_slots`).Scan(&supersededSlots, &survivorSlots); err != nil {
		t.Fatalf("read migrated Agent Turn slots: %v", err)
	}
	if supersededSlots != 0 || survivorSlots != 1 {
		t.Errorf("migrated Agent Turn slots = (%d superseded, %d survivor), want (0, 1)", supersededSlots, survivorSlots)
	}

	rows, err := pool.Query(ctx, `
SELECT state, finished_at IS NOT NULL, COALESCE(last_error, '')
FROM tool_invocations
WHERE agent_turn_id = '67000000-0000-4000-8000-000000000031'
ORDER BY invocation_number`)
	if err != nil {
		t.Fatalf("read migrated mutations: %v", err)
	}
	defer rows.Close()
	type mutationState struct {
		state, lastError string
		finished         bool
	}
	var mutations []mutationState
	for rows.Next() {
		var mutation mutationState
		if err := rows.Scan(&mutation.state, &mutation.finished, &mutation.lastError); err != nil {
			t.Fatalf("scan migrated mutation: %v", err)
		}
		mutations = append(mutations, mutation)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 2 || mutations[0].state != "FAILED" || !mutations[0].finished ||
		mutations[1].state != "FAILED" || !mutations[1].finished ||
		!strings.Contains(mutations[0].lastError, "Phase 6 migration") || !strings.Contains(mutations[1].lastError, "Phase 6 migration") {
		t.Errorf("migrated superseded mutations = %#v, want both unstarted RESERVED mutations failed", mutations)
	}

	var reviewPurpose, unknownPurpose, purposeNullable bool
	if err := pool.QueryRow(ctx, `
SELECT
    (SELECT purpose = 'REVIEW' FROM agent_turns WHERE id = '67000000-0000-4000-8000-000000000031'),
    (SELECT purpose IS NULL FROM agent_turns WHERE id = '67000000-0000-4000-8000-000000000032'),
    (SELECT is_nullable = 'YES' FROM information_schema.columns
     WHERE table_schema = 'public' AND table_name = 'agent_turns' AND column_name = 'purpose')`).Scan(
		&reviewPurpose, &unknownPurpose, &purposeNullable,
	); err != nil {
		t.Fatalf("read migrated Agent Turn purposes: %v", err)
	}
	if !unknownPurpose || !reviewPurpose || !purposeNullable {
		t.Errorf("migrated purpose honesty = (unknown %v, review %v, nullable %v), want (true, true, true)", unknownPurpose, reviewPurpose, purposeNullable)
	}

	if _, err := pool.Exec(ctx, `
CREATE TABLE schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (name <> ''),
    checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`); err != nil {
		t.Fatalf("create migration history for upgraded Store: %v", err)
	}
	for version, migration := range []struct {
		filename string
		name     string
	}{
		{filename: "000001_bootstrap.sql", name: "bootstrap"},
		{filename: "000002_normalized_events.sql", name: "normalized_events"},
		{filename: "000003_durable_jobs_and_turn_fencing.sql", name: "durable_jobs_and_turn_fencing"},
		{filename: "000004_phase_five_acknowledgement_barriers.sql", name: "phase_five_acknowledgement_barriers"},
		{filename: "000005_single_live_workflow_successor.sql", name: "single_live_workflow_successor"},
		{filename: "000006_agent_turn_preparation.sql", name: "agent_turn_preparation"},
	} {
		contents, err := migrations.Files.ReadFile(migration.filename)
		if err != nil {
			t.Fatal(err)
		}
		checksum := sha256.Sum256(contents)
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, version+1, migration.name, checksum[:]); err != nil {
			t.Fatalf("record migration %s: %v", migration.filename, err)
		}
	}
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("open upgraded Store: %v", err)
	}
	defer database.Close()
	stopJob, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "migration-stop-worker", 5*time.Second)
	if err != nil || stopJob == nil || stopJob.ID != stopJobID {
		t.Fatalf("claim migrated stale-runtime stop Job = (%#v, %v), want %s", stopJob, err, stopJobID)
	}
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, *stopJob); err != nil {
		t.Fatalf("acknowledge migrated stale Runtime Process stopped: %v", err)
	}
	lease := store.AgentTurnLease{
		AgentTurn: store.AgentTurn{
			AgentTurnSpec: store.AgentTurnSpec{
				AgentSessionID: "67000000-0000-4000-8000-000000000022", WorkflowAttemptID: "67000000-0000-4000-8000-000000000010",
				ControlRevision: 1,
			},
			ID: "67000000-0000-4000-8000-000000000032", ExecutionEpoch: 1,
		},
		JobLease: store.JobLease{
			Job: store.Job{
				JobSpec:    store.JobSpec{AgentTurnID: "67000000-0000-4000-8000-000000000032", ExecutionEpoch: 1},
				ID:         "67000000-0000-4000-8000-000000000042",
				Status:     store.JobLeased,
				LeaseToken: "67000000-0000-4000-8000-000000000082",
			},
			Attempt: 1,
		},
		OwnerID: "reviewer-runtime", OwnerToken: "67000000-0000-4000-8000-000000000072",
	}
	if err := database.ValidateTurnFence(ctx, lease); err != nil {
		t.Errorf("ValidateTurnFence() for surviving unknown-purpose legacy Turn error = %v", err)
	}
}

func TestPhaseSixMigrationHandsOffClosingWorkflowsWithUnviableClosureJobs(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	type closureCase struct {
		name                    string
		workflowID, attemptID   string
		assignmentID, sessionID string
		turnID, runJobID        string
		stopJobID, settleJobID  string
		stopStatus              string
		stopAttempts            int
		settlementStatus        string
		settlementAttempts      int
		settlementExpired       bool
	}
	cases := []closureCase{
		{
			name:       "failed stop job",
			workflowID: "6d000000-0000-4000-8000-000000000001", attemptID: "6d000000-0000-4000-8000-000000000010",
			assignmentID: "6d000000-0000-4000-8000-000000000011", sessionID: "6d000000-0000-4000-8000-000000000021",
			turnID: "6d000000-0000-4000-8000-000000000031", runJobID: "6d000000-0000-4000-8000-000000000041",
			stopJobID: "6d000000-0000-4000-8000-000000000043", settleJobID: "6d000000-0000-4000-8000-000000000044",
			stopStatus: "FAILED", stopAttempts: 1, settlementStatus: "AVAILABLE",
		},
		{
			name:       "cancelled settlement job",
			workflowID: "6d000000-0000-4000-8000-000000000201", attemptID: "6d000000-0000-4000-8000-000000000210",
			assignmentID: "6d000000-0000-4000-8000-000000000211", sessionID: "6d000000-0000-4000-8000-000000000221",
			turnID: "6d000000-0000-4000-8000-000000000231", runJobID: "6d000000-0000-4000-8000-000000000241",
			stopJobID: "6d000000-0000-4000-8000-000000000243", settleJobID: "6d000000-0000-4000-8000-000000000244",
			stopStatus: "AVAILABLE", settlementStatus: "CANCELLED",
		},
		{
			name:       "exhausted available stop job",
			workflowID: "6d000000-0000-4000-8000-000000000301", attemptID: "6d000000-0000-4000-8000-000000000310",
			assignmentID: "6d000000-0000-4000-8000-000000000311", sessionID: "6d000000-0000-4000-8000-000000000321",
			turnID: "6d000000-0000-4000-8000-000000000331", runJobID: "6d000000-0000-4000-8000-000000000341",
			stopJobID: "6d000000-0000-4000-8000-000000000343", settleJobID: "6d000000-0000-4000-8000-000000000344",
			stopStatus: "AVAILABLE", stopAttempts: 3, settlementStatus: "AVAILABLE",
		},
		{
			name:       "expired exhausted settlement job",
			workflowID: "6d000000-0000-4000-8000-000000000101", attemptID: "6d000000-0000-4000-8000-000000000110",
			assignmentID: "6d000000-0000-4000-8000-000000000111", sessionID: "6d000000-0000-4000-8000-000000000121",
			turnID: "6d000000-0000-4000-8000-000000000131", runJobID: "6d000000-0000-4000-8000-000000000141",
			stopJobID: "6d000000-0000-4000-8000-000000000143", settleJobID: "6d000000-0000-4000-8000-000000000144",
			stopStatus: "AVAILABLE", settlementStatus: "LEASED", settlementAttempts: 3, settlementExpired: true,
		},
	}

	for index, testCase := range cases {
		closureID := fmt.Sprintf("closure-unviable-%d", index)
		if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state,
    closure_id, closure_deadline, closure_retention_token
)
VALUES ($1, 1, 'owner', 'repo', $7, $7, 'CLOSING', 7, 'ACTIVE', 'ACTIVE',
        $8, clock_timestamp() + interval '1 day', 'retention-unviable');

INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ($2, $1, 1, 'ACTIVE');

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ($3, $1, 'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1',
        'sha256:developer', $9);

INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status,
    control_revision
)
VALUES ($4, $3, 1, $4::text, 'runtime', '1', 'sha256:developer', $9, 'ACTIVE', 3);

INSERT INTO agent_turns (
    id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    status, active, control_revision, agent_profile_commit_sha,
    agent_profile_content_sha256, mutation_admission_open,
    mutation_admission_closed_at, created_at, started_at
)
VALUES ($5, $4, $2, 1, 2, 'CANCELLING', TRUE, 3, 'closing-unviable',
        decode(repeat('11', 32), 'hex'), FALSE, clock_timestamp(),
        clock_timestamp(), clock_timestamp());

INSERT INTO jobs (
    id, queue, kind, payload, status, attempt_count, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch
)
VALUES ($6, 'agent-turns', 'RUN_AGENT_TURN', '{}'::jsonb, 'AVAILABLE', 0, 1,
        $6::text, $1, $2, $3, $4, $5, 2);
`, pgx.QueryExecModeSimpleProtocol, testCase.workflowID, testCase.attemptID, testCase.assignmentID, testCase.sessionID,
			testCase.turnID, testCase.runJobID, index+1, closureID,
			"closing/unviable/"+fmt.Sprint(index)); err != nil {
			t.Fatalf("seed %s aggregate: %v", testCase.name, err)
		}

		insertClosureJob := func(id, kind, status string, attemptCount int, expired bool) {
			t.Helper()
			var leaseOwner, leaseToken any
			var leasedAt, leaseExpiresAt any
			if status == "LEASED" {
				leaseOwner = "legacy-worker"
				leaseToken = "6d000000-0000-4000-8000-000000000081"
				leasedAt = time.Now().Add(-2 * time.Hour)
				if expired {
					leaseExpiresAt = time.Now().Add(-time.Hour)
				} else {
					leaseExpiresAt = time.Now().Add(time.Hour)
				}
			}
			var completedAt any
			if status == "FAILED" || status == "CANCELLED" || status == "SUCCEEDED" {
				completedAt = time.Now()
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, attempt_count, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, lease_owner, lease_token, leased_at,
    lease_expires_at, completed_at
)
VALUES ($1::uuid, 'workflow', $2, '{}'::jsonb, $3, $4, 3, $1::text,
        $5, $6, $7, $8, $9, 2, $10, $11, $12, $13, $14)`,
				id, kind, status, attemptCount, testCase.workflowID, testCase.attemptID,
				testCase.assignmentID, testCase.sessionID, testCase.turnID,
				leaseOwner, leaseToken, leasedAt, leaseExpiresAt, completedAt); err != nil {
				t.Fatalf("seed %s %s: %v", testCase.name, kind, err)
			}
			if status == "LEASED" {
				if _, err := pool.Exec(ctx, `
INSERT INTO job_attempts (
    job_id, attempt_number, lease_owner, lease_token, status, leased_at, lease_expires_at
)
VALUES ($1, $2, 'legacy-worker', '6d000000-0000-4000-8000-000000000081',
        'LEASED', clock_timestamp() - interval '2 hours',
        clock_timestamp() - interval '1 hour')`, id, attemptCount); err != nil {
					t.Fatalf("seed %s leased attempt: %v", testCase.name, err)
				}
			}
		}
		insertClosureJob(testCase.stopJobID, store.StopAgentTurnJobKind, testCase.stopStatus, testCase.stopAttempts, false)
		insertClosureJob(testCase.settleJobID, store.SettleClosureJobKind, testCase.settlementStatus, testCase.settlementAttempts, testCase.settlementExpired)

		if _, err := pool.Exec(ctx, `
INSERT INTO workflow_closure_barriers (
    workflow_id, closure_id, workflow_revision, source_turn_id, source_session_id,
    source_attempt_id, source_execution_epoch, source_control_revision,
    mutation_admission_closed_at, stop_job_id, settlement_job_id
)
VALUES ($1, $2, 7, $3, $4, $5, 2, 3, clock_timestamp(), $6, $7)`,
			testCase.workflowID, closureID, testCase.turnID, testCase.sessionID,
			testCase.attemptID, testCase.stopJobID, testCase.settleJobID); err != nil {
			t.Fatalf("seed %s closure barrier: %v", testCase.name, err)
		}
	}

	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006 with unviable closure Jobs: %v", err)
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var workflowStatus, reason, turnStatus, stopStatus, settlementStatus, replacementKind, replacementStatus string
			var closureCleared, barrierSettled, turnActive bool
			var outboxCount int
			if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.human_handoff_reason, workflow.closure_id IS NULL,
       barrier.settled_at IS NOT NULL, turn.status, turn.active,
       stop_job.status, settlement_job.status, replacement.kind, replacement.status,
       (SELECT count(*) FROM jobs
        WHERE workflow_id = workflow.id
          AND kind IN ('PUBLISH_HUMAN_HANDOFF', 'RECONCILE_GITHUB_LABELS'))
FROM workflows AS workflow
JOIN workflow_closure_barriers AS barrier ON barrier.workflow_id = workflow.id
JOIN agent_turns AS turn ON turn.id = barrier.source_turn_id
JOIN jobs AS stop_job ON stop_job.id = barrier.stop_job_id
JOIN jobs AS settlement_job ON settlement_job.id = barrier.settlement_job_id
JOIN jobs AS replacement ON replacement.id = turn.stop_runtime_job_id
WHERE workflow.id = $1`, testCase.workflowID).Scan(
				&workflowStatus, &reason, &closureCleared, &barrierSettled, &turnStatus,
				&turnActive, &stopStatus, &settlementStatus, &replacementKind,
				&replacementStatus, &outboxCount,
			); err != nil {
				t.Fatalf("read migrated unviable closure: %v", err)
			}
			if workflowStatus != "NEEDS_HUMAN" || reason != "migration_active_turn_incompatible_aggregate" ||
				!closureCleared || !barrierSettled || turnStatus != "INTERRUPTED" || turnActive ||
				stopStatus == "AVAILABLE" || stopStatus == "LEASED" || settlementStatus == "AVAILABLE" || settlementStatus == "LEASED" ||
				replacementKind != store.StopStaleRuntimeJobKind || replacementStatus != "AVAILABLE" || outboxCount != 2 {
				t.Errorf("unviable closure migration = Workflow %s reason %q cleared %v, barrier %v, Turn %s/%v, Jobs %s/%s, replacement %s/%s, outbox %d",
					workflowStatus, reason, closureCleared, barrierSettled, turnStatus, turnActive,
					stopStatus, settlementStatus, replacementKind, replacementStatus, outboxCount)
			}
			if testCase.settlementExpired {
				var attemptStatus string
				var finished, retryable bool
				if err := pool.QueryRow(ctx, `
SELECT status, finished_at IS NOT NULL, retryable
FROM job_attempts WHERE job_id = $1 AND attempt_number = 3`, testCase.settleJobID).Scan(
					&attemptStatus, &finished, &retryable,
				); err != nil {
					t.Fatalf("read abandoned exhausted closure attempt: %v", err)
				}
				if attemptStatus != "FAILED" || !finished || retryable {
					t.Errorf("abandoned exhausted closure attempt = %s finished %v retryable %v", attemptStatus, finished, retryable)
				}
			}
		})
	}
}

func TestPhaseSixMigrationRetainsExactClosingBarrierSourceAheadOfNewerDuplicate(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state,
    closure_id, closure_deadline, closure_retention_token
)
VALUES ('6a000000-0000-4000-8000-000000000001', 1, 'owner', 'repo', 1, 1,
        'CLOSING', 9, 'ACTIVE', 'ACTIVE', 'closure-6a',
        clock_timestamp() + interval '1 day', 'retention-6a');

INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ('6a000000-0000-4000-8000-000000000010', '6a000000-0000-4000-8000-000000000001', 1, 'ACTIVE');

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('6a000000-0000-4000-8000-000000000011', '6a000000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:developer', 'closing/source'),
       ('6a000000-0000-4000-8000-000000000012', '6a000000-0000-4000-8000-000000000001',
        'REVIEWER', 'ACTIVE', 'reviewer', 'runtime', '1', 'sha256:reviewer', 'closing/duplicate');

INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status,
    control_revision
)
VALUES ('6a000000-0000-4000-8000-000000000021', '6a000000-0000-4000-8000-000000000011',
        1, 'closing-source', 'runtime', '1', 'sha256:developer', 'closing/source', 'ACTIVE', 3),
       ('6a000000-0000-4000-8000-000000000022', '6a000000-0000-4000-8000-000000000012',
        1, 'closing-duplicate', 'runtime', '1', 'sha256:reviewer', 'closing/duplicate', 'ACTIVE', 3);

INSERT INTO agent_turns (
    id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    status, active, control_revision, agent_profile_commit_sha,
    agent_profile_content_sha256, mutation_admission_open,
    mutation_admission_closed_at, created_at, started_at
)
VALUES ('6a000000-0000-4000-8000-000000000031', '6a000000-0000-4000-8000-000000000021',
        '6a000000-0000-4000-8000-000000000010', 1, 7, 'CANCELLING', TRUE, 3, 'closing-source',
        decode(repeat('11', 32), 'hex'), FALSE, '2026-01-01 00:00:00+00',
        '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00'),
       ('6a000000-0000-4000-8000-000000000032', '6a000000-0000-4000-8000-000000000022',
        '6a000000-0000-4000-8000-000000000010', 1, 8, 'RUNNING', TRUE, 3, 'closing-duplicate',
        decode(repeat('22', 32), 'hex'), TRUE, NULL,
        '2026-01-01 00:00:01+00', '2026-01-01 00:00:01+00');

INSERT INTO jobs (
    id, queue, kind, payload, status, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch
)
VALUES ('6a000000-0000-4000-8000-000000000041', 'agent-turn', 'RUN_AGENT_TURN', '{}'::jsonb,
        'AVAILABLE', 1, 'closing-source-run', '6a000000-0000-4000-8000-000000000001',
        '6a000000-0000-4000-8000-000000000010', '6a000000-0000-4000-8000-000000000011',
        '6a000000-0000-4000-8000-000000000021', '6a000000-0000-4000-8000-000000000031', 7),
       ('6a000000-0000-4000-8000-000000000042', 'agent-turn', 'RUN_AGENT_TURN', '{}'::jsonb,
        'AVAILABLE', 1, 'closing-duplicate-run', '6a000000-0000-4000-8000-000000000001',
        '6a000000-0000-4000-8000-000000000010', '6a000000-0000-4000-8000-000000000012',
        '6a000000-0000-4000-8000-000000000022', '6a000000-0000-4000-8000-000000000032', 8),
       ('6a000000-0000-4000-8000-000000000043', 'workflow', 'STOP_AGENT_TURN',
        '{"workflow_id":"6a000000-0000-4000-8000-000000000001","workflow_attempt_id":"6a000000-0000-4000-8000-000000000010","closure_id":"closure-6a","turn_id":"6a000000-0000-4000-8000-000000000031","session_id":"6a000000-0000-4000-8000-000000000021","execution_epoch":7,"control_revision":3,"workflow_revision":9}'::jsonb,
        'AVAILABLE', 3, 'closing-stop', '6a000000-0000-4000-8000-000000000001',
        '6a000000-0000-4000-8000-000000000010', '6a000000-0000-4000-8000-000000000011',
        '6a000000-0000-4000-8000-000000000021', '6a000000-0000-4000-8000-000000000031', 7),
       ('6a000000-0000-4000-8000-000000000044', 'workflow', 'SETTLE_CLOSURE',
        '{"workflow_id":"6a000000-0000-4000-8000-000000000001","workflow_attempt_id":"6a000000-0000-4000-8000-000000000010","closure_id":"closure-6a","turn_id":"6a000000-0000-4000-8000-000000000031","session_id":"6a000000-0000-4000-8000-000000000021","execution_epoch":7,"control_revision":3,"workflow_revision":9}'::jsonb,
        'AVAILABLE', 3, 'closing-settlement', '6a000000-0000-4000-8000-000000000001',
        '6a000000-0000-4000-8000-000000000010', '6a000000-0000-4000-8000-000000000011',
        '6a000000-0000-4000-8000-000000000021', '6a000000-0000-4000-8000-000000000031', 7);

INSERT INTO workflow_closure_barriers (
    workflow_id, closure_id, workflow_revision, source_turn_id, source_session_id,
    source_attempt_id, source_execution_epoch, source_control_revision,
    mutation_admission_closed_at, stop_job_id, settlement_job_id
)
VALUES ('6a000000-0000-4000-8000-000000000001', 'closure-6a', 9,
        '6a000000-0000-4000-8000-000000000031', '6a000000-0000-4000-8000-000000000021',
        '6a000000-0000-4000-8000-000000000010', 7, 3, '2026-01-01 00:00:00+00',
        '6a000000-0000-4000-8000-000000000043', '6a000000-0000-4000-8000-000000000044');
`); err != nil {
		t.Fatalf("seed CLOSING Workflow with duplicate active Agent Turn: %v", err)
	}

	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006: %v", err)
	}

	var sourceStatus, duplicateStatus, workflowStatus string
	var sourceActive, duplicateActive, closureUnsettled bool
	if err := pool.QueryRow(ctx, `
SELECT source.status, source.active, duplicate.status, duplicate.active, workflow.status,
       barrier.settled_at IS NULL
FROM workflows AS workflow
JOIN workflow_closure_barriers AS barrier
  ON barrier.workflow_id = workflow.id AND barrier.closure_id = workflow.closure_id
JOIN agent_turns AS source ON source.id = barrier.source_turn_id
JOIN agent_turns AS duplicate ON duplicate.id = '6a000000-0000-4000-8000-000000000032'
WHERE workflow.id = '6a000000-0000-4000-8000-000000000001'`).Scan(
		&sourceStatus, &sourceActive, &duplicateStatus, &duplicateActive, &workflowStatus, &closureUnsettled,
	); err != nil {
		t.Fatalf("read migrated CLOSING reconciliation: %v", err)
	}
	if sourceStatus != "CANCELLING" || !sourceActive || duplicateStatus != "INTERRUPTED" || duplicateActive ||
		workflowStatus != "CLOSING" || !closureUnsettled {
		t.Fatalf("migrated CLOSING reconciliation = source %s/%v, duplicate %s/%v, Workflow %s, barrier unsettled %v",
			sourceStatus, sourceActive, duplicateStatus, duplicateActive, workflowStatus, closureUnsettled)
	}

	if _, err := pool.Exec(ctx, `
CREATE TABLE schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (name <> ''),
    checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`); err != nil {
		t.Fatalf("create migration history for upgraded Store: %v", err)
	}
	for version, migration := range []struct {
		filename string
		name     string
	}{
		{filename: "000001_bootstrap.sql", name: "bootstrap"},
		{filename: "000002_normalized_events.sql", name: "normalized_events"},
		{filename: "000003_durable_jobs_and_turn_fencing.sql", name: "durable_jobs_and_turn_fencing"},
		{filename: "000004_phase_five_acknowledgement_barriers.sql", name: "phase_five_acknowledgement_barriers"},
		{filename: "000005_single_live_workflow_successor.sql", name: "single_live_workflow_successor"},
		{filename: "000006_agent_turn_preparation.sql", name: "agent_turn_preparation"},
	} {
		contents, err := migrations.Files.ReadFile(migration.filename)
		if err != nil {
			t.Fatal(err)
		}
		checksum := sha256.Sum256(contents)
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, version+1, migration.name, checksum[:]); err != nil {
			t.Fatalf("record migration %s: %v", migration.filename, err)
		}
	}
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("open upgraded Store: %v", err)
	}
	defer database.Close()
	stopJob, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.StopAgentTurnJobKind, "stop-worker", 5*time.Second)
	if err != nil || stopJob == nil {
		t.Fatalf("claim preserved closure stop Job = (%#v, %v)", stopJob, err)
	}
	if _, err := database.AcknowledgeClosureTurnStopped(ctx, *stopJob); err != nil {
		t.Fatalf("acknowledge preserved closure stop Job: %v", err)
	}
	settlementJob, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.SettleClosureJobKind, "settlement-worker", 5*time.Second)
	if err != nil || settlementJob == nil {
		t.Fatalf("claim preserved closure settlement Job = (%#v, %v)", settlementJob, err)
	}
	settlement, err := database.CompleteClosureSettlement(ctx, *settlementJob)
	if err != nil {
		t.Fatalf("complete preserved closure settlement Job: %v", err)
	}
	if settlement.SourceTurnID != "6a000000-0000-4000-8000-000000000031" || settlement.SettledAt == nil {
		t.Errorf("preserved closure settlement = %#v", settlement)
	}
}

func TestPhaseSixMigrationTerminatesAbandonedClosureWorkOnHumanHandoff(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state,
    closure_id, closure_deadline, closure_retention_token
)
VALUES ('6b000000-0000-4000-8000-000000000001', 1, 'owner', 'repo', 1, 1,
        'CLOSING', 4, 'ACTIVE', 'ACTIVE', 'closure-6b',
        clock_timestamp() + interval '1 day', 'retention-6b');

INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ('6b000000-0000-4000-8000-000000000010', '6b000000-0000-4000-8000-000000000001', 1, 'ACTIVE');

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('6b000000-0000-4000-8000-000000000011', '6b000000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:developer', 'closing/handoff');

INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status,
    control_revision
)
VALUES ('6b000000-0000-4000-8000-000000000021', '6b000000-0000-4000-8000-000000000011',
        1, 'closing-handoff', 'runtime', '1', 'sha256:developer', 'closing/handoff', 'ACTIVE', 4);

INSERT INTO agent_turns (
    id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    status, active, control_revision, agent_profile_commit_sha,
    agent_profile_content_sha256, mutation_admission_open,
    mutation_admission_closed_at, created_at, started_at
)
VALUES ('6b000000-0000-4000-8000-000000000031', '6b000000-0000-4000-8000-000000000021',
        '6b000000-0000-4000-8000-000000000010', 1, 2, 'CANCELLING', TRUE, 3, 'closing-handoff',
        decode(repeat('11', 32), 'hex'), FALSE, clock_timestamp(),
        '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00');

INSERT INTO jobs (
    id, queue, kind, payload, status, attempt_count, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, lease_owner, lease_token, leased_at,
    lease_expires_at, heartbeat_at
)
VALUES ('6b000000-0000-4000-8000-000000000041', 'agent-turn', 'RUN_AGENT_TURN', '{}'::jsonb,
        'AVAILABLE', 0, 1, 'closing-handoff-run', '6b000000-0000-4000-8000-000000000001',
        '6b000000-0000-4000-8000-000000000010', '6b000000-0000-4000-8000-000000000011',
        '6b000000-0000-4000-8000-000000000021', '6b000000-0000-4000-8000-000000000031', 2,
        NULL, NULL, NULL, NULL, NULL),
       ('6b000000-0000-4000-8000-000000000043', 'workflow', 'STOP_AGENT_TURN',
        '{"workflow_id":"6b000000-0000-4000-8000-000000000001","workflow_attempt_id":"6b000000-0000-4000-8000-000000000010","closure_id":"closure-6b","turn_id":"6b000000-0000-4000-8000-000000000031","session_id":"6b000000-0000-4000-8000-000000000021","execution_epoch":2,"control_revision":3,"workflow_revision":4}'::jsonb,
        'LEASED', 1, 3, 'closing-handoff-stop', '6b000000-0000-4000-8000-000000000001',
        '6b000000-0000-4000-8000-000000000010', '6b000000-0000-4000-8000-000000000011',
        '6b000000-0000-4000-8000-000000000021', '6b000000-0000-4000-8000-000000000031', 2,
        'stop-worker', '6b000000-0000-4000-8000-000000000081', clock_timestamp(),
        clock_timestamp() + interval '1 hour', clock_timestamp()),
       ('6b000000-0000-4000-8000-000000000044', 'workflow', 'SETTLE_CLOSURE',
        '{"workflow_id":"6b000000-0000-4000-8000-000000000001","workflow_attempt_id":"6b000000-0000-4000-8000-000000000010","closure_id":"closure-6b","turn_id":"6b000000-0000-4000-8000-000000000031","session_id":"6b000000-0000-4000-8000-000000000021","execution_epoch":2,"control_revision":3,"workflow_revision":4}'::jsonb,
        'AVAILABLE', 0, 3, 'closing-handoff-settlement', '6b000000-0000-4000-8000-000000000001',
        '6b000000-0000-4000-8000-000000000010', '6b000000-0000-4000-8000-000000000011',
        '6b000000-0000-4000-8000-000000000021', '6b000000-0000-4000-8000-000000000031', 2,
        NULL, NULL, NULL, NULL, NULL);

INSERT INTO job_attempts (
    job_id, attempt_number, lease_owner, lease_token, status, leased_at,
    lease_expires_at, heartbeat_at
)
VALUES ('6b000000-0000-4000-8000-000000000043', 1, 'stop-worker',
        '6b000000-0000-4000-8000-000000000081', 'LEASED', clock_timestamp(),
        clock_timestamp() + interval '1 hour', clock_timestamp());

INSERT INTO workflow_closure_barriers (
    workflow_id, closure_id, workflow_revision, source_turn_id, source_session_id,
    source_attempt_id, source_execution_epoch, source_control_revision,
    mutation_admission_closed_at, stop_job_id, settlement_job_id
)
VALUES ('6b000000-0000-4000-8000-000000000001', 'closure-6b', 4,
        '6b000000-0000-4000-8000-000000000031', '6b000000-0000-4000-8000-000000000021',
        '6b000000-0000-4000-8000-000000000010', 2, 3, clock_timestamp(),
        '6b000000-0000-4000-8000-000000000043', '6b000000-0000-4000-8000-000000000044');
`); err != nil {
		t.Fatalf("seed incompatible CLOSING Workflow: %v", err)
	}

	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006: %v", err)
	}

	var workflowStatus, workflowReason, stopStatus, settlementStatus, stopAttemptStatus string
	var closureCleared, barrierSettled, stopAttemptFinished, stopAttemptRetryable bool
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.human_handoff_reason,
       workflow.closure_id IS NULL AND workflow.closure_deadline IS NULL
           AND workflow.closure_retention_token IS NULL,
       barrier.settled_at IS NOT NULL, stop_job.status, settlement_job.status,
       stop_attempt.status, stop_attempt.finished_at IS NOT NULL,
       COALESCE(stop_attempt.retryable, TRUE)
FROM workflows AS workflow
JOIN workflow_closure_barriers AS barrier ON barrier.workflow_id = workflow.id
JOIN jobs AS stop_job ON stop_job.id = barrier.stop_job_id
JOIN jobs AS settlement_job ON settlement_job.id = barrier.settlement_job_id
JOIN job_attempts AS stop_attempt
  ON stop_attempt.job_id = stop_job.id AND stop_attempt.attempt_number = 1
WHERE workflow.id = '6b000000-0000-4000-8000-000000000001'`).Scan(
		&workflowStatus, &workflowReason, &closureCleared, &barrierSettled,
		&stopStatus, &settlementStatus, &stopAttemptStatus, &stopAttemptFinished,
		&stopAttemptRetryable,
	); err != nil {
		t.Fatalf("read abandoned closure reconciliation: %v", err)
	}
	if workflowStatus != "NEEDS_HUMAN" || workflowReason != "migration_active_turn_incompatible_aggregate" ||
		!closureCleared || !barrierSettled || stopStatus != "CANCELLED" || settlementStatus != "CANCELLED" ||
		stopAttemptStatus != "FAILED" || !stopAttemptFinished || stopAttemptRetryable {
		t.Errorf("abandoned closure reconciliation = Workflow %s reason %q cleared %v, barrier settled %v, Jobs %s/%s, attempt %s finished %v retryable %v",
			workflowStatus, workflowReason, closureCleared, barrierSettled, stopStatus, settlementStatus,
			stopAttemptStatus, stopAttemptFinished, stopAttemptRetryable)
	}
	var replacementStopStatus, replacementStopKind string
	var replacementRecoveryBarrier bool
	if err := pool.QueryRow(ctx, `
SELECT replacement.status, replacement.kind,
       turn.recovery_started_at IS NOT NULL AND turn.runtime_stop_required
           AND turn.runtime_stopped_at IS NULL AND turn.recovery_settled_at IS NULL
           AND turn.stop_runtime_job_id = replacement.id
FROM agent_turns AS turn
JOIN jobs AS replacement ON replacement.id = turn.stop_runtime_job_id
WHERE turn.id = '6b000000-0000-4000-8000-000000000031'`).Scan(
		&replacementStopStatus, &replacementStopKind, &replacementRecoveryBarrier,
	); err != nil {
		t.Fatalf("read replacement stale-runtime stop for abandoned closure: %v", err)
	}
	if replacementStopStatus != "AVAILABLE" || replacementStopKind != store.StopStaleRuntimeJobKind || !replacementRecoveryBarrier {
		t.Errorf("replacement abandoned-closure stop = %s/%s barrier %v, want AVAILABLE STOP_STALE_RUNTIME with barrier",
			replacementStopStatus, replacementStopKind, replacementRecoveryBarrier)
	}
}

func TestPhaseSixMigrationHandsOffWorkflowWhenNoActiveTurnIsAggregateCompatible(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state
)
VALUES ('69000000-0000-4000-8000-000000000001', 1, 'owner', 'repo', 1, 1,
        'REVIEWING', 5, 'ACTIVE', 'ACTIVE');

INSERT INTO workflow_attempts (
    id, workflow_id, attempt_number, status, review_cycles_completed,
    review_cycle_limit, infrastructure_failures, infrastructure_failure_limit
)
VALUES ('69000000-0000-4000-8000-000000000010', '69000000-0000-4000-8000-000000000001',
        1, 'ACTIVE', 5, 7, 3, 5);

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('69000000-0000-4000-8000-000000000011', '69000000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:developer', 'incompatible/developer'),
       ('69000000-0000-4000-8000-000000000012', '69000000-0000-4000-8000-000000000001',
        'REVIEWER', 'ACTIVE', 'reviewer', 'runtime', '1', 'sha256:reviewer', 'incompatible/reviewer');

INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('69000000-0000-4000-8000-000000000021', '69000000-0000-4000-8000-000000000011',
        1, 'incompatible-developer', 'runtime', '1', 'sha256:developer', 'incompatible/developer', 'ACTIVE'),
       ('69000000-0000-4000-8000-000000000022', '69000000-0000-4000-8000-000000000012',
        1, 'incompatible-reviewer', 'runtime', '1', 'sha256:reviewer', 'incompatible/reviewer', 'ACTIVE');

INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, active, base_ref, base_sha,
    head_ref, head_sha, ready_for_sha
)
VALUES ('69000000-0000-4000-8000-000000000051', '69000000-0000-4000-8000-000000000001',
        1, 'owner', 'repo', 51, 51, 'OPEN', TRUE, 'main', 'base-sha',
        'feature', 'current-head', 'current-head');

INSERT INTO agent_turns (
    id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    status, active, control_revision, agent_profile_commit_sha,
    agent_profile_content_sha256, owner_id, owner_token, leased_at,
    lease_expires_at, mutation_admission_open, change_proposal_id,
    expected_head_sha, created_at, started_at
)
VALUES ('69000000-0000-4000-8000-000000000031', '69000000-0000-4000-8000-000000000021',
        '69000000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'incompatible-developer',
        decode(repeat('11', 32), 'hex'), 'developer-runtime',
        '69000000-0000-4000-8000-000000000071', clock_timestamp(), clock_timestamp() + interval '1 hour',
        TRUE, NULL, NULL, '2026-01-01 00:00:01+00', '2026-01-01 00:00:01+00'),
       ('69000000-0000-4000-8000-000000000032', '69000000-0000-4000-8000-000000000022',
        '69000000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'incompatible-reviewer',
        decode(repeat('22', 32), 'hex'), 'reviewer-runtime',
        '69000000-0000-4000-8000-000000000072', clock_timestamp(), clock_timestamp() + interval '1 hour',
        TRUE, '69000000-0000-4000-8000-000000000051', 'stale-head',
        '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00');
`); err != nil {
		t.Fatalf("seed aggregate-incompatible active Agent Turns: %v", err)
	}

	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006: %v", err)
	}

	var status, resumeRole, workflowReason, attemptReason string
	var revision int64
	var reviewsUsed, reviewLimit, infrastructureFailures, infrastructureFailureLimit int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.resume_role,
       workflow.human_handoff_reason, attempt.human_handoff_reason,
       attempt.review_cycles_completed, attempt.review_cycle_limit,
       attempt.infrastructure_failures, attempt.infrastructure_failure_limit
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.workflow_id = workflow.id AND attempt.active
WHERE workflow.id = '69000000-0000-4000-8000-000000000001'`).Scan(
		&status, &revision, &resumeRole, &workflowReason, &attemptReason,
		&reviewsUsed, &reviewLimit, &infrastructureFailures, &infrastructureFailureLimit,
	); err != nil {
		t.Fatalf("read compatibility Human Handoff: %v", err)
	}
	if status != "NEEDS_HUMAN" || revision != 6 || resumeRole != "REVIEWER" ||
		workflowReason != "migration_active_turn_incompatible_aggregate" || attemptReason != workflowReason ||
		reviewsUsed != 3 || reviewLimit != 3 || infrastructureFailures != 1 || infrastructureFailureLimit != 1 {
		t.Errorf("compatibility Human Handoff = %s@%d resume %s reason %q/%q budgets %d/%d and %d/%d",
			status, revision, resumeRole, workflowReason, attemptReason, reviewsUsed, reviewLimit,
			infrastructureFailures, infrastructureFailureLimit)
	}

	var activeTurns, interruptedTurns, readyProposals int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE active),
       count(*) FILTER (WHERE status = 'INTERRUPTED' AND NOT active),
       (SELECT count(*) FROM change_proposals
        WHERE workflow_id = '69000000-0000-4000-8000-000000000001' AND active AND ready_for_sha IS NOT NULL)
FROM agent_turns
WHERE workflow_id = '69000000-0000-4000-8000-000000000001'`).Scan(
		&activeTurns, &interruptedTurns, &readyProposals,
	); err != nil {
		t.Fatalf("read compatibility reconciliation: %v", err)
	}
	if activeTurns != 0 || interruptedTurns != 2 || readyProposals != 0 {
		t.Errorf("compatibility reconciliation = %d active, %d interrupted, %d ready proposals; want 0, 2, 0",
			activeTurns, interruptedTurns, readyProposals)
	}

	rows, err := pool.Query(ctx, `
SELECT kind, payload::text
FROM jobs
WHERE workflow_id = '69000000-0000-4000-8000-000000000001'
  AND kind IN ('PUBLISH_HUMAN_HANDOFF', 'RECONCILE_GITHUB_LABELS')`)
	if err != nil {
		t.Fatalf("read compatibility outbox actions: %v", err)
	}
	defer rows.Close()
	actions := map[string]string{}
	for rows.Next() {
		var kind, payload string
		if err := rows.Scan(&kind, &payload); err != nil {
			t.Fatalf("scan compatibility outbox action: %v", err)
		}
		actions[kind] = payload
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || !strings.Contains(actions["PUBLISH_HUMAN_HANDOFF"], workflowReason) ||
		!strings.Contains(actions["PUBLISH_HUMAN_HANDOFF"], "compatible") ||
		strings.Contains(actions["PUBLISH_HUMAN_HANDOFF"], "UNKNOWN") ||
		!strings.Contains(actions["RECONCILE_GITHUB_LABELS"], "NEEDS_HUMAN") {
		t.Errorf("compatibility outbox actions = %#v", actions)
	}
}

func TestPhaseSixMigrationHandsOffWorkflowWhenSupersededTurnHasUncertainMutation(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, filename := range []string{
		"000001_bootstrap.sql",
		"000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql",
		"000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state
)
VALUES ('68000000-0000-4000-8000-000000000001', 1, 'owner', 'repo', 1, 1,
        'REVIEWING', 7, 'ACTIVE', 'ACTIVE');

INSERT INTO workflow_attempts (
    id, workflow_id, attempt_number, status, review_cycles_completed,
    review_cycle_limit, infrastructure_failures, infrastructure_failure_limit
)
VALUES ('68000000-0000-4000-8000-000000000010', '68000000-0000-4000-8000-000000000001',
        1, 'ACTIVE', 2, 7, 3, 5);

INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('68000000-0000-4000-8000-000000000011', '68000000-0000-4000-8000-000000000001',
        'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:developer', 'uncertain/developer'),
       ('68000000-0000-4000-8000-000000000012', '68000000-0000-4000-8000-000000000001',
        'REVIEWER', 'ACTIVE', 'reviewer', 'runtime', '1', 'sha256:reviewer', 'uncertain/reviewer');

INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('68000000-0000-4000-8000-000000000021', '68000000-0000-4000-8000-000000000011',
        1, 'uncertain-developer', 'runtime', '1', 'sha256:developer', 'uncertain/developer', 'ACTIVE'),
       ('68000000-0000-4000-8000-000000000022', '68000000-0000-4000-8000-000000000012',
        1, 'uncertain-reviewer', 'runtime', '1', 'sha256:reviewer', 'uncertain/reviewer', 'ACTIVE');

INSERT INTO agent_turns (
    id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    status, active, control_revision, agent_profile_commit_sha,
    agent_profile_content_sha256, owner_id, owner_token, leased_at,
    lease_expires_at, heartbeat_at, mutation_admission_open, created_at, started_at
)
VALUES ('68000000-0000-4000-8000-000000000031', '68000000-0000-4000-8000-000000000021',
        '68000000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'uncertain-developer',
        decode(repeat('11', 32), 'hex'), 'developer-runtime',
        '68000000-0000-4000-8000-000000000071', clock_timestamp(), clock_timestamp() + interval '1 hour',
        clock_timestamp(), TRUE, '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00'),
       ('68000000-0000-4000-8000-000000000032', '68000000-0000-4000-8000-000000000022',
        '68000000-0000-4000-8000-000000000010', 1, 1, 'RUNNING', TRUE, 1, 'uncertain-reviewer',
        decode(repeat('22', 32), 'hex'), 'reviewer-runtime',
        '68000000-0000-4000-8000-000000000072', clock_timestamp(), clock_timestamp() + interval '1 hour',
        clock_timestamp(), TRUE, '2026-01-01 00:00:01+00', '2026-01-01 00:00:01+00');

INSERT INTO jobs (
    id, queue, kind, status, attempt_count, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, lease_owner, lease_token, leased_at,
    lease_expires_at, heartbeat_at
)
VALUES ('68000000-0000-4000-8000-000000000041', 'agent-turn', 'RUN_AGENT_TURN', 'LEASED', 1, 1,
        'uncertain-run-developer', '68000000-0000-4000-8000-000000000001',
        '68000000-0000-4000-8000-000000000010', '68000000-0000-4000-8000-000000000011',
        '68000000-0000-4000-8000-000000000021', '68000000-0000-4000-8000-000000000031', 1,
        'developer-worker', '68000000-0000-4000-8000-000000000081', clock_timestamp(),
        clock_timestamp() + interval '1 hour', clock_timestamp()),
       ('68000000-0000-4000-8000-000000000042', 'agent-turn', 'RUN_AGENT_TURN', 'LEASED', 1, 1,
        'uncertain-run-reviewer', '68000000-0000-4000-8000-000000000001',
        '68000000-0000-4000-8000-000000000010', '68000000-0000-4000-8000-000000000012',
        '68000000-0000-4000-8000-000000000022', '68000000-0000-4000-8000-000000000032', 1,
        'reviewer-worker', '68000000-0000-4000-8000-000000000082', clock_timestamp(),
        clock_timestamp() + interval '1 hour', clock_timestamp());

INSERT INTO job_attempts (job_id, attempt_number, lease_owner, lease_token, status, leased_at, lease_expires_at, heartbeat_at)
VALUES ('68000000-0000-4000-8000-000000000041', 1, 'developer-worker',
        '68000000-0000-4000-8000-000000000081', 'LEASED', clock_timestamp(), clock_timestamp() + interval '1 hour', clock_timestamp()),
       ('68000000-0000-4000-8000-000000000042', 1, 'reviewer-worker',
        '68000000-0000-4000-8000-000000000082', 'LEASED', clock_timestamp(), clock_timestamp() + interval '1 hour', clock_timestamp());

INSERT INTO agent_turn_slots (
    agent_turn_id, agent_session_id, execution_epoch, control_revision,
    owner_id, owner_token, lease_expires_at
)
VALUES ('68000000-0000-4000-8000-000000000031', '68000000-0000-4000-8000-000000000021',
        1, 1, 'developer-runtime', '68000000-0000-4000-8000-000000000071', clock_timestamp() + interval '1 hour'),
       ('68000000-0000-4000-8000-000000000032', '68000000-0000-4000-8000-000000000022',
        1, 1, 'reviewer-runtime', '68000000-0000-4000-8000-000000000072', clock_timestamp() + interval '1 hour');

INSERT INTO tool_invocations (
    id, agent_turn_id, execution_epoch, invocation_number, tool_name, kind, state,
    operation_id, request, started_at
)
VALUES ('68000000-0000-4000-8000-000000000061', '68000000-0000-4000-8000-000000000031',
        1, 1, 'update_pull_request', 'MUTATION', 'IN_FLIGHT', 'uncertain-in-flight', '{}'::jsonb, clock_timestamp()),
       ('68000000-0000-4000-8000-000000000062', '68000000-0000-4000-8000-000000000032',
         1, 1, 'create_issue_comment', 'MUTATION', 'RESERVED', 'uncertain-reserved', '{}'::jsonb, NULL);

INSERT INTO webhook_deliveries (
    delivery_id, event_name, action, repository_id, repository_owner,
    repository_name, issue_id, issue_number, workflow_id, payload, status, processed_at
)
VALUES ('68000000-0000-4000-8000-000000000091', 'issues', 'labeled', 1, 'owner',
        'repo', 1, 1, '68000000-0000-4000-8000-000000000001', '{}'::bytea,
        'PROCESSED', clock_timestamp());

INSERT INTO normalized_events (
    delivery_id, payload, status, workflow_id, disposition, reason,
    applied_revision, deferred_for_turn_id, processed_at
)
VALUES ('68000000-0000-4000-8000-000000000091',
        '{"delivery_id":"68000000-0000-4000-8000-000000000091","kind":"trigger"}'::jsonb,
        'DEFERRED', '68000000-0000-4000-8000-000000000001', 'DEFERRED',
        'active_turn', 7, '68000000-0000-4000-8000-000000000031', clock_timestamp());

INSERT INTO jobs (
    id, queue, kind, payload, status, max_attempts, idempotency_key,
    workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, normalized_event_id, action_key
)
VALUES ('68000000-0000-4000-8000-000000000045', 'workflow', 'RECONCILE_PENDING_EVENTS',
        '{}'::jsonb, 'AVAILABLE', 3, 'legacy-reconcile-pending',
        '68000000-0000-4000-8000-000000000001', '68000000-0000-4000-8000-000000000010',
        '68000000-0000-4000-8000-000000000011', '68000000-0000-4000-8000-000000000021',
        '68000000-0000-4000-8000-000000000031', 1,
        '68000000-0000-4000-8000-000000000091', 'reconcile-pending-events');

INSERT INTO job_normalized_events (job_id, normalized_event_id)
VALUES ('68000000-0000-4000-8000-000000000045', '68000000-0000-4000-8000-000000000091');

INSERT INTO jobs (id, queue, kind, max_attempts, idempotency_key)
VALUES ((md5('migration:000006:workflow:68000000-0000-4000-8000-000000000001:publish-human-handoff:0'))::uuid,
        'collision', 'EXISTING_JOB', 1, 'migration-id-collision-seed'),
       ((md5('migration:000006:agent-turn:68000000-0000-4000-8000-000000000031:stop-stale-runtime:0'))::uuid,
        'collision', 'EXISTING_JOB', 1, 'migration-recovery-id-collision-seed');
`); err != nil {
		t.Fatalf("seed uncertain duplicate active Agent Turns: %v", err)
	}

	contents, err := migrations.Files.ReadFile("000006_agent_turn_preparation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply migration 000006: %v", err)
	}

	var status, resumeRole, desiredAssignmentStatus, desiredRuntimeState, workflowReason, attemptReason string
	var revision int64
	var attemptActive bool
	var reviewsUsed, reviewLimit, infrastructureFailures, infrastructureFailureLimit int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.resume_role,
       workflow.desired_assignment_status, workflow.desired_runtime_state,
       workflow.human_handoff_reason, attempt.active, attempt.human_handoff_reason,
       attempt.review_cycles_completed, attempt.review_cycle_limit,
       attempt.infrastructure_failures, attempt.infrastructure_failure_limit
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.workflow_id = workflow.id AND attempt.active
WHERE workflow.id = '68000000-0000-4000-8000-000000000001'`).Scan(
		&status, &revision, &resumeRole, &desiredAssignmentStatus, &desiredRuntimeState,
		&workflowReason, &attemptActive, &attemptReason, &reviewsUsed, &reviewLimit,
		&infrastructureFailures, &infrastructureFailureLimit,
	); err != nil {
		t.Fatalf("read migration Human Handoff: %v", err)
	}
	if status != "NEEDS_HUMAN" || revision != 8 || resumeRole != "REVIEWER" ||
		desiredAssignmentStatus != "WAITING_FOR_HUMAN" || desiredRuntimeState != "ACTIVE" ||
		workflowReason == "" || !strings.Contains(workflowReason, "migration") ||
		!attemptActive || attemptReason != workflowReason || reviewsUsed != 2 || reviewLimit != 3 ||
		infrastructureFailures != 1 || infrastructureFailureLimit != 1 {
		t.Errorf("migration Human Handoff = %s@%d resume %s desired %s/%s reason %q attempt active %v reason %q budgets %d/%d and %d/%d",
			status, revision, resumeRole, desiredAssignmentStatus, desiredRuntimeState,
			workflowReason, attemptActive, attemptReason, reviewsUsed, reviewLimit,
			infrastructureFailures, infrastructureFailureLimit)
	}

	var activeTurns, openAdmissions, ownedTurns, turnLeases, turnSlots, liveExecutionJobs, leasedAttempts int
	if err := pool.QueryRow(ctx, `
SELECT
    (SELECT count(*) FROM agent_turns WHERE workflow_id = $1 AND active),
    (SELECT count(*) FROM agent_turns WHERE workflow_id = $1 AND mutation_admission_open),
    (SELECT count(*) FROM agent_turns WHERE workflow_id = $1 AND (owner_id IS NOT NULL OR owner_token IS NOT NULL)),
    (SELECT count(*) FROM agent_turns WHERE workflow_id = $1 AND (leased_at IS NOT NULL OR lease_expires_at IS NOT NULL OR heartbeat_at IS NOT NULL)),
    (SELECT count(*) FROM agent_turn_slots AS slot JOIN agent_turns AS turn ON turn.id = slot.agent_turn_id WHERE turn.workflow_id = $1),
    (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN' AND status IN ('AVAILABLE', 'LEASED')),
    (SELECT count(*) FROM job_attempts AS attempt JOIN jobs AS job ON job.id = attempt.job_id WHERE job.workflow_id = $1 AND job.kind = 'RUN_AGENT_TURN' AND attempt.status = 'LEASED')`,
		"68000000-0000-4000-8000-000000000001").Scan(
		&activeTurns, &openAdmissions, &ownedTurns, &turnLeases, &turnSlots, &liveExecutionJobs, &leasedAttempts,
	); err != nil {
		t.Fatalf("read migrated execution fences: %v", err)
	}
	if activeTurns != 0 || openAdmissions != 0 || ownedTurns != 0 || turnLeases != 0 ||
		turnSlots != 0 || liveExecutionJobs != 0 || leasedAttempts != 0 {
		t.Errorf("migration fences = active turns %d, open admissions %d, owned turns %d, turn leases %d, slots %d, live jobs %d, leased attempts %d; want all zero",
			activeTurns, openAdmissions, ownedTurns, turnLeases, turnSlots, liveExecutionJobs, leasedAttempts)
	}

	var recoveringTurns, cancelledJobs, failedAttempts, waitingAssignments int
	if err := pool.QueryRow(ctx, `
SELECT
    (SELECT count(*) FROM agent_turns WHERE workflow_id = $1 AND status IN ('INTERRUPTED', 'RECONCILING') AND NOT active
        AND mutation_admission_closed_at IS NOT NULL AND completed_at IS NOT NULL),
    (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN' AND status = 'CANCELLED'
        AND lease_owner IS NULL AND lease_token IS NULL AND leased_at IS NULL AND lease_expires_at IS NULL AND heartbeat_at IS NULL),
    (SELECT count(*) FROM job_attempts AS attempt JOIN jobs AS job ON job.id = attempt.job_id
        WHERE job.workflow_id = $1 AND job.kind = 'RUN_AGENT_TURN' AND attempt.status = 'FAILED'
          AND attempt.finished_at IS NOT NULL AND NOT attempt.retryable),
    (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1 AND status = 'WAITING_FOR_HUMAN')`,
		"68000000-0000-4000-8000-000000000001").Scan(
		&recoveringTurns, &cancelledJobs, &failedAttempts, &waitingAssignments,
	); err != nil {
		t.Fatalf("read migrated terminal state: %v", err)
	}
	if recoveringTurns != 2 || cancelledJobs != 2 || failedAttempts != 2 || waitingAssignments != 2 {
		t.Errorf("migration terminal state = %d recovering turns, %d cancelled jobs, %d failed attempts, %d waiting Assignments; want 2 each",
			recoveringTurns, cancelledJobs, failedAttempts, waitingAssignments)
	}

	var unknownMutations, failedReservations int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE state = 'UNKNOWN' AND finished_at IS NULL),
       count(*) FILTER (WHERE state = 'FAILED' AND finished_at IS NOT NULL)
FROM tool_invocations
WHERE agent_turn_id IN (
    '68000000-0000-4000-8000-000000000031',
    '68000000-0000-4000-8000-000000000032'
)`).Scan(&unknownMutations, &failedReservations); err != nil {
		t.Fatalf("read migrated uncertain mutations: %v", err)
	}
	if unknownMutations != 1 || failedReservations != 1 {
		t.Errorf("migrated mutations = %d UNKNOWN and %d FAILED, want one of each", unknownMutations, failedReservations)
	}

	var eventStatus, staleReconciliationStatus string
	var eventFenceCleared bool
	var eventLinks int
	if err := pool.QueryRow(ctx, `
SELECT event.status,
       event.workflow_id IS NULL AND event.disposition IS NULL AND event.reason IS NULL
           AND event.applied_revision IS NULL AND event.deferred_for_turn_id IS NULL
           AND event.processed_at IS NULL,
       reconciliation.status,
       (SELECT count(*) FROM job_normalized_events
        WHERE normalized_event_id = event.delivery_id)
FROM normalized_events AS event
JOIN jobs AS reconciliation ON reconciliation.id = '68000000-0000-4000-8000-000000000045'
WHERE event.delivery_id = '68000000-0000-4000-8000-000000000091'`).Scan(
		&eventStatus, &eventFenceCleared, &staleReconciliationStatus, &eventLinks,
	); err != nil {
		t.Fatalf("read migrated deferred normalized event: %v", err)
	}
	if eventStatus != "PENDING" || !eventFenceCleared || staleReconciliationStatus != "CANCELLED" || eventLinks != 0 {
		t.Errorf("migrated deferred event = %s fence cleared %v, stale reconciliation %s, links %d; want PENDING/true/CANCELLED/0",
			eventStatus, eventFenceCleared, staleReconciliationStatus, eventLinks)
	}

	var recoveryTurns, reconcilingTurns, availableStops, availableReconciliations int
	var collisionFallback bool
	if err := pool.QueryRow(ctx, `
SELECT
    (SELECT count(*) FROM agent_turns WHERE workflow_id = $1
        AND recovery_started_at IS NOT NULL AND runtime_stop_required
        AND runtime_stopped_at IS NULL AND recovery_settled_at IS NULL
        AND stop_runtime_job_id IS NOT NULL),
    (SELECT count(*) FROM agent_turns WHERE workflow_id = $1
        AND status = 'RECONCILING' AND reconcile_mutations_job_id IS NOT NULL),
    (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND queue = 'agent-turn-recovery'
        AND kind = 'STOP_STALE_RUNTIME' AND status = 'AVAILABLE'),
    (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND queue = 'agent-turn-recovery'
        AND kind = 'RECONCILE_AGENT_TURN_MUTATIONS' AND status = 'AVAILABLE'),
    EXISTS (
        SELECT 1 FROM jobs
        WHERE id = (md5('migration:000006:agent-turn:68000000-0000-4000-8000-000000000031:stop-stale-runtime:1'))::uuid
          AND idempotency_key = 'agent-turn-recovery:68000000-0000-4000-8000-000000000031:epoch:1:stop_stale_runtime'
    )`, "68000000-0000-4000-8000-000000000001").Scan(
		&recoveryTurns, &reconcilingTurns, &availableStops, &availableReconciliations,
		&collisionFallback,
	); err != nil {
		t.Fatalf("read migration Agent Turn recoveries: %v", err)
	}
	if recoveryTurns != 2 || reconcilingTurns != 1 || availableStops != 2 || availableReconciliations != 1 || !collisionFallback {
		t.Errorf("migration recoveries = %d barriers, %d reconciling turns, %d stops, %d reconciliations, collision fallback %v; want 2/1/2/1/true",
			recoveryTurns, reconcilingTurns, availableStops, availableReconciliations, collisionFallback)
	}

	rows, err := pool.Query(ctx, `
SELECT id::text, kind, status, idempotency_key, payload::text,
       id = (md5(
           'migration:000006:workflow:68000000-0000-4000-8000-000000000001:' ||
           CASE kind
               WHEN 'PUBLISH_HUMAN_HANDOFF' THEN 'publish-human-handoff:1'
               ELSE 'reconcile-github-labels:0'
           END
       ))::uuid,
       idempotency_key =
           'migration:000006:workflow:68000000-0000-4000-8000-000000000001:' ||
           CASE kind
               WHEN 'PUBLISH_HUMAN_HANDOFF' THEN 'publish-human-handoff'
               ELSE 'reconcile-github-labels'
           END
FROM jobs
WHERE workflow_id = $1 AND kind IN ('PUBLISH_HUMAN_HANDOFF', 'RECONCILE_GITHUB_LABELS')
ORDER BY kind`, "68000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("read migration outbox actions: %v", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	keys := map[string]bool{}
	actions := map[string]string{}
	deterministicIDs := true
	deterministicKeys := true
	for rows.Next() {
		var id, kind, jobStatus, key, payload string
		var deterministicID, deterministicKey bool
		if err := rows.Scan(&id, &kind, &jobStatus, &key, &payload, &deterministicID, &deterministicKey); err != nil {
			t.Fatalf("scan migration outbox action: %v", err)
		}
		if jobStatus != "AVAILABLE" || id == "" || key == "" {
			t.Errorf("migration outbox %s = status %s id %q key %q", kind, jobStatus, id, key)
		}
		ids[id] = true
		keys[key] = true
		actions[kind] = payload
		deterministicIDs = deterministicIDs && deterministicID
		deterministicKeys = deterministicKeys && deterministicKey
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || len(ids) != 2 || len(keys) != 2 || !deterministicIDs || !deterministicKeys ||
		actions["PUBLISH_HUMAN_HANDOFF"] == "" || actions["RECONCILE_GITHUB_LABELS"] == "" ||
		!strings.Contains(actions["PUBLISH_HUMAN_HANDOFF"], workflowReason) ||
		!strings.Contains(actions["PUBLISH_HUMAN_HANDOFF"], "UNKNOWN") ||
		!strings.Contains(actions["RECONCILE_GITHUB_LABELS"], "NEEDS_HUMAN") {
		t.Errorf("migration outbox actions = %#v, ids %d deterministic %v, keys %d deterministic %v",
			actions, len(ids), deterministicIDs, len(keys), deterministicKeys)
	}

	if _, err := pool.Exec(ctx, `
CREATE TABLE schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (name <> ''),
    checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`); err != nil {
		t.Fatalf("create migration history for recovery consumers: %v", err)
	}
	for version, migration := range []struct {
		filename string
		name     string
	}{
		{filename: "000001_bootstrap.sql", name: "bootstrap"},
		{filename: "000002_normalized_events.sql", name: "normalized_events"},
		{filename: "000003_durable_jobs_and_turn_fencing.sql", name: "durable_jobs_and_turn_fencing"},
		{filename: "000004_phase_five_acknowledgement_barriers.sql", name: "phase_five_acknowledgement_barriers"},
		{filename: "000005_single_live_workflow_successor.sql", name: "single_live_workflow_successor"},
		{filename: "000006_agent_turn_preparation.sql", name: "agent_turn_preparation"},
	} {
		contents, err := migrations.Files.ReadFile(migration.filename)
		if err != nil {
			t.Fatal(err)
		}
		checksum := sha256.Sum256(contents)
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, version+1, migration.name, checksum[:]); err != nil {
			t.Fatalf("record migration %s: %v", migration.filename, err)
		}
	}
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("open migrated Store for recovery consumers: %v", err)
	}
	defer database.Close()
	stoppedTurns := map[string]bool{}
	for range 2 {
		stopJob, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "migration-stop-worker", 5*time.Second)
		if err != nil || stopJob == nil {
			t.Fatalf("claim migrated stale-runtime stop Job = (%#v, %v)", stopJob, err)
		}
		var identity struct {
			ExecutionJobID string `json:"execution_job_id"`
		}
		if err := json.Unmarshal(stopJob.Payload, &identity); err != nil || identity.ExecutionJobID == "" {
			t.Fatalf("decode migrated stale-runtime stop payload = (%#v, %v)", identity, err)
		}
		stoppedTurns[stopJob.AgentTurnID] = true
		if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, *stopJob); err != nil {
			t.Fatalf("acknowledge migrated stale Runtime Process %s stopped under Human Handoff: %v", stopJob.AgentTurnID, err)
		}
	}
	if len(stoppedTurns) != 2 {
		t.Errorf("acknowledged migrated stale Runtime Processes = %#v, want both interrupted turns", stoppedTurns)
	}
	reconcileJob, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.ReconcileAgentTurnMutationsJobKind, "migration-reconcile-worker", 5*time.Second)
	if err != nil || reconcileJob == nil || reconcileJob.AgentTurnID != "68000000-0000-4000-8000-000000000031" {
		t.Fatalf("claim migrated mutation reconciliation Job = (%#v, %v)", reconcileJob, err)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, *reconcileJob, "68000000-0000-4000-8000-000000000061", store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"reconciled":true}`),
	}); err != nil {
		t.Fatalf("consume migrated mutation reconciliation Job under Human Handoff: %v", err)
	}
	if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, *reconcileJob); err != nil {
		t.Fatalf("complete migrated mutation reconciliation Job under Human Handoff: %v", err)
	}
	application, applied, err := database.ApplyNextPendingNormalizedEvent(ctx, func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		if record.DeliveryID != "68000000-0000-4000-8000-000000000091" || record.Status != store.NormalizedEventPending {
			t.Errorf("claimed migrated normalized event = %#v", record)
		}
		return store.WorkflowLocator{RepositoryID: 1, IssueID: 1, IssueNumber: 1}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Decision{Snapshot: snapshot, Disposition: workflow.DispositionDuplicate, Reason: workflow.ReasonEventDuplicate}
		}, nil
	})
	if err != nil || !applied || application.DeliveryID != "68000000-0000-4000-8000-000000000091" || application.Status != store.NormalizedEventCompleted {
		t.Fatalf("replay migrated deferred normalized event = (%#v, %v, %v), want completed", application, applied, err)
	}
}

func TestRunExecutesMigrationsExactlyOnceUnderConcurrentCalls(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))

	const callers = 8
	start := make(chan struct{})
	errors := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			errors <- migrations.Run(ctx, pool)
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 13 {
		t.Errorf("schema_migrations rows = %d, want 13", count)
	}
	for version, filename := range map[int]string{
		1:  "000001_bootstrap.sql",
		2:  "000002_normalized_events.sql",
		3:  "000003_durable_jobs_and_turn_fencing.sql",
		4:  "000004_phase_five_acknowledgement_barriers.sql",
		5:  "000005_single_live_workflow_successor.sql",
		6:  "000006_agent_turn_preparation.sql",
		7:  "000007_scoped_mutation_operations.sql",
		8:  "000008_agent_turn_settlements.sql",
		9:  "000009_workflow_action_exhaustion.sql",
		10: "000010_workflow_action_failure_barriers.sql",
		11: "000011_durable_closure_retention.sql",
		12: "000012_runtime_profile_compatibility.sql",
		13: "000013_tool_scoped_mutation_operations.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read embedded migration %q: %v", filename, err)
		}
		wantChecksum := sha256.Sum256(contents)
		var checksum []byte
		if err := pool.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, version).Scan(&checksum); err != nil {
			t.Fatalf("query migration %d checksum: %v", version, err)
		}
		if string(checksum) != string(wantChecksum[:]) {
			t.Errorf("migration %d checksum = %x, want SHA-256 %x", version, checksum, wantChecksum)
		}
	}

	for _, table := range []string{
		"webhook_deliveries",
		"workflows",
		"workflow_attempts",
		"agent_assignments",
		"agent_sessions",
		"agent_turns",
		"change_proposals",
		"tool_invocations",
		"tool_invocation_replays",
		"jobs",
		"job_attempts",
		"agent_turn_slots",
		"job_normalized_events",
		"workflow_closure_barriers",
		"agent_turn_settlements",
		"workflow_github_effect_cleanups",
		"workflow_action_failures",
		"workflow_internal_events",
		"assignment_retention_generations",
		"assignment_retention_targets",
		"runtime_profile_compatibility_results",
	} {
		var exists bool
		err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("look up table %q: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q does not exist", table)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum = decode(repeat('00', 32), 'hex')`); err != nil {
		t.Fatalf("tamper with migration checksum: %v", err)
	}
	if err := migrations.Run(ctx, pool); err == nil || !strings.Contains(err.Error(), "differs from recorded history") {
		t.Errorf("Run() with changed migration history error = %v, want checksum mismatch error", err)
	}
}

func TestOpenUsesPasswordSecretAndReturnsReadyStore(t *testing.T) {
	postgres := startPostgres(t)
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword+"\n"), 0o600); err != nil {
		t.Fatalf("write password secret: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := database.Ready(ctx); err != nil {
		t.Errorf("Ready() error = %v", err)
	}
	pool := openPool(t, postgres.databaseURL(true))
	var migrationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("query migrations applied by Open(): %v", err)
	}
	if migrationCount != 13 {
		t.Errorf("migrations applied by Open() = %d, want 13", migrationCount)
	}
	database.Close()
	if err := database.Ready(ctx); err == nil {
		t.Error("Ready() after Close() error = nil, want closed-pool error")
	}
}

type postgresContainer struct {
	hostPort string
}

func (postgres postgresContainer) databaseURL(withPassword bool) string {
	user := url.User(postgresUser)
	if withPassword {
		user = url.UserPassword(postgresUser, postgresPassword)
	}
	return (&url.URL{
		Scheme:   "postgres",
		User:     user,
		Host:     net.JoinHostPort("127.0.0.1", postgres.hostPort),
		Path:     "/" + postgresDatabase,
		RawQuery: "sslmode=disable",
	}).String()
}

func startPostgres(t *testing.T) postgresContainer {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("find Docker CLI: %v", err)
	}

	name := fmt.Sprintf("omnigrex-postgres-test-%d", time.Now().UnixNano())
	args := []string{
		"run", "--detach", "--rm",
		"--name", name,
		"--user", "70:70",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/var/lib/postgresql:rw,noexec,nosuid,nodev,size=256m,uid=70,gid=70,mode=0700",
		"--tmpfs", "/var/run/postgresql:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0750",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0700",
		"--env", "POSTGRES_USER=" + postgresUser,
		"--env", "POSTGRES_PASSWORD=" + postgresPassword,
		"--env", "POSTGRES_DB=" + postgresDatabase,
		"--publish", "127.0.0.1::5432",
		postgresImage,
	}
	if output, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start %s: %v\n%s", postgresImage, err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("docker", "rm", "--force", name).CombinedOutput(); err != nil && !strings.Contains(string(output), "No such container") {
			t.Errorf("remove PostgreSQL container: %v\n%s", err, output)
		}
	})

	portOutput, err := exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("get PostgreSQL port: %v\n%s", err, portOutput)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(portOutput)))
	if err != nil {
		t.Fatalf("parse PostgreSQL port %q: %v", strings.TrimSpace(string(portOutput)), err)
	}
	postgres := postgresContainer{hostPort: port}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		config, configErr := pgxpool.ParseConfig(postgres.databaseURL(true))
		if configErr != nil {
			cancel()
			t.Fatalf("parse test database URL: %v", configErr)
		}
		pool, poolErr := pgxpool.NewWithConfig(ctx, config)
		if poolErr == nil {
			poolErr = pool.Ping(ctx)
			pool.Close()
		}
		cancel()
		if poolErr == nil {
			return postgres
		}
		time.Sleep(100 * time.Millisecond)
	}

	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	t.Fatalf("PostgreSQL did not become ready\n%s", logs)
	return postgresContainer{}
}

func openPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	return pool
}
