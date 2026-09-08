//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestPhaseNineMigrationMaterializesLegacyClosedRetention(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, filename := range []string{
		"000001_bootstrap.sql", "000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql", "000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql", "000006_agent_turn_preparation.sql",
		"000007_scoped_mutation_operations.sql", "000008_agent_turn_settlements.sql",
		"000009_workflow_action_exhaustion.sql", "000010_workflow_action_failure_barriers.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state,
    retention_deadline, retention_token
)
VALUES ('59000000-0000-4000-8000-000000000001', 59, 'owner', 'repo', 59, 59,
        'CLOSED', 4, 'COMPLETED', 'RETAINED',
        clock_timestamp() + interval '30 days', 'legacy-retention'),
       ('59000000-0000-4000-8000-000000000011', 591, 'owner', 'repo', 591, 591,
        'CLOSED', 4, 'COMPLETED', 'RETAINED',
        clock_timestamp() + interval '30 days', 'orphan-available'),
       ('59000000-0000-4000-8000-000000000021', 592, 'owner', 'repo', 592, 592,
        'CLOSED', 4, 'COMPLETED', 'RETAINED',
        clock_timestamp() + interval '30 days', 'orphan-leased');
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('59000000-0000-4000-8000-000000000002',
        '59000000-0000-4000-8000-000000000001', 'DEVELOPER', 'ACTIVE',
        'developer', 'runtime', '1', 'sha256:legacy',
        'assignment-59000000-0000-4000-8000-000000000002/runtime-state');
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('59000000-0000-4000-8000-000000000003',
        '59000000-0000-4000-8000-000000000002', 1, 'legacy-session',
        'runtime', '1', 'sha256:legacy',
        'assignment-59000000-0000-4000-8000-000000000002/runtime-state', 'ACTIVE');
INSERT INTO jobs (
    id, queue, kind, payload, status, max_attempts, idempotency_key, workflow_id
)
VALUES ('59000000-0000-4000-8000-000000000004', 'workflow', 'COMPLETE_ASSIGNMENTS',
        '{"revision":4}', 'AVAILABLE', 3, 'legacy-complete',
        '59000000-0000-4000-8000-000000000001'),
       ('59000000-0000-4000-8000-000000000005', 'workflow', 'COLLECT_ASSIGNMENTS',
         '{"retention_token":"legacy-retention","revision":4}', 'AVAILABLE', 3,
         'legacy-collect', '59000000-0000-4000-8000-000000000001'),
       ('59000000-0000-4000-8000-000000000012', 'workflow', 'COLLECT_ASSIGNMENTS',
         '{"retention_token":"orphan-available","revision":4}', 'AVAILABLE', 3,
         'orphan-available-collect', '59000000-0000-4000-8000-000000000011');
INSERT INTO jobs (
    id, queue, kind, payload, status, attempt_count, max_attempts,
    idempotency_key, workflow_id, lease_owner, lease_token, leased_at,
    lease_expires_at, heartbeat_at
)
VALUES ('59000000-0000-4000-8000-000000000022', 'workflow', 'COLLECT_ASSIGNMENTS',
        '{"retention_token":"orphan-leased","revision":4}', 'LEASED', 1, 3,
        'orphan-leased-collect', '59000000-0000-4000-8000-000000000021',
        'legacy-collector', '59000000-0000-4000-8000-000000000023',
        clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour',
        clock_timestamp());
INSERT INTO job_attempts (
    job_id, attempt_number, lease_owner, lease_token, status, leased_at,
    lease_expires_at, heartbeat_at
)
VALUES ('59000000-0000-4000-8000-000000000022', 1, 'legacy-collector',
        '59000000-0000-4000-8000-000000000023', 'LEASED',
        clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour',
        clock_timestamp())`,
		pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("seed legacy CLOSED Workflow: %v", err)
	}
	contents, err := migrations.Files.ReadFile("000011_durable_closure_retention.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply Phase 9 migration: %v", err)
	}
	var assignmentStatus, sessionStatus, generationStatus, collectStatus, completeStatus string
	var retentionUntil *time.Time
	var targetCount int
	var generationID string
	if err := pool.QueryRow(ctx, `SELECT status, retention_until FROM agent_assignments WHERE id = '59000000-0000-4000-8000-000000000002'`).Scan(&assignmentStatus, &retentionUntil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id = '59000000-0000-4000-8000-000000000003'`).Scan(&sessionStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT generation.id::text, generation.status, collection.status,
       redundant.status,
       (SELECT count(*) FROM assignment_retention_targets WHERE retention_generation_id = generation.id)
FROM assignment_retention_generations AS generation
JOIN jobs AS collection ON collection.id = generation.collection_job_id
JOIN jobs AS redundant ON redundant.id = '59000000-0000-4000-8000-000000000004'
WHERE generation.workflow_id = '59000000-0000-4000-8000-000000000001'`).Scan(
		&generationID, &generationStatus, &collectStatus, &completeStatus, &targetCount); err != nil {
		t.Fatal(err)
	}
	if assignmentStatus != "COMPLETED" || retentionUntil == nil || sessionStatus != "RETAINED" ||
		generationID == "" || generationStatus != "SCHEDULED" || collectStatus != "AVAILABLE" ||
		completeStatus != "CANCELLED" || targetCount != 2 {
		t.Errorf("migrated retention = Assignment %s/%v, Session %s, generation %s/%s, Jobs %s/%s, targets %d",
			assignmentStatus, retentionUntil, sessionStatus, generationID, generationStatus,
			collectStatus, completeStatus, targetCount)
	}

	const orphanReason = "Phase 9 migration cancelled orphaned Assignment collection"
	for _, fixture := range []struct {
		name, workflowID, jobID string
		attempts                int
	}{
		{name: "available", workflowID: "59000000-0000-4000-8000-000000000011", jobID: "59000000-0000-4000-8000-000000000012"},
		{name: "leased", workflowID: "59000000-0000-4000-8000-000000000021", jobID: "59000000-0000-4000-8000-000000000022", attempts: 1},
	} {
		t.Run("cancels "+fixture.name+" orphan collection", func(t *testing.T) {
			var runtimeState, retentionToken, jobStatus, jobError string
			var retentionDeadline, completedAt *time.Time
			var leaseCleared bool
			var generations, attempts int
			if err := pool.QueryRow(ctx, `
SELECT workflow.desired_runtime_state, workflow.retention_deadline,
       COALESCE(workflow.retention_token, ''), job.status, job.completed_at,
       job.lease_owner IS NULL AND job.lease_token IS NULL AND job.leased_at IS NULL
           AND job.lease_expires_at IS NULL AND job.heartbeat_at IS NULL,
       COALESCE(job.last_error, ''),
       (SELECT count(*) FROM assignment_retention_generations
        WHERE workflow_id = workflow.id),
       (SELECT count(*) FROM job_attempts WHERE job_id = job.id)
FROM workflows AS workflow
JOIN jobs AS job ON job.id = $2
WHERE workflow.id = $1`, fixture.workflowID, fixture.jobID).Scan(
				&runtimeState, &retentionDeadline, &retentionToken, &jobStatus, &completedAt,
				&leaseCleared, &jobError, &generations, &attempts); err != nil {
				t.Fatal(err)
			}
			if runtimeState != "COLLECTED" || retentionDeadline != nil || retentionToken != "" ||
				jobStatus != "CANCELLED" || completedAt == nil || !leaseCleared || jobError != orphanReason ||
				generations != 0 || attempts != fixture.attempts {
				t.Errorf("orphan migration = Workflow %s/%v/%q, Job %s/%v lease cleared %t error %q, generations %d, attempts %d",
					runtimeState, retentionDeadline, retentionToken, jobStatus, completedAt,
					leaseCleared, jobError, generations, attempts)
			}
		})
	}

	var attemptStatus, attemptError string
	var attemptFinishedAt *time.Time
	var attemptRetryable bool
	if err := pool.QueryRow(ctx, `
SELECT status, finished_at, retryable, COALESCE(last_error, '')
FROM job_attempts
WHERE job_id = '59000000-0000-4000-8000-000000000022' AND attempt_number = 1`).Scan(
		&attemptStatus, &attemptFinishedAt, &attemptRetryable, &attemptError); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "FAILED" || attemptFinishedAt == nil || attemptRetryable || attemptError != orphanReason {
		t.Errorf("leased orphan attempt = %s/%v retryable %t error %q, want terminal migration failure",
			attemptStatus, attemptFinishedAt, attemptRetryable, attemptError)
	}
}

func TestPhaseNineMigrationReconcilesSettledPhaseEightClosureBarriers(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, filename := range []string{
		"000001_bootstrap.sql", "000002_normalized_events.sql",
		"000003_durable_jobs_and_turn_fencing.sql", "000004_phase_five_acknowledgement_barriers.sql",
		"000005_single_live_workflow_successor.sql", "000006_agent_turn_preparation.sql",
		"000007_scoped_mutation_operations.sql", "000008_agent_turn_settlements.sql",
		"000009_workflow_action_exhaustion.sql", "000010_workflow_action_failure_barriers.sql",
	} {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	type migrationClosureCase struct {
		name, workflowID, attemptID, assignmentID, sessionID string
		settlementJobID, settlementLeaseToken                string
		turnID, stopJobID, stopLeaseToken                    string
		settled, reopenRequested                             bool
	}
	cases := []migrationClosureCase{
		{
			name: "settled without source Turn", workflowID: "5a000000-0000-4000-8000-000000000001",
			attemptID: "5a000000-0000-4000-8000-000000000002", assignmentID: "5a000000-0000-4000-8000-000000000003",
			sessionID: "5a000000-0000-4000-8000-000000000004", settlementJobID: "5a000000-0000-4000-8000-000000000005",
			settlementLeaseToken: "5a000000-0000-4000-8000-000000000006", settled: true,
		},
		{
			name: "settled source Turn after reopen", workflowID: "5b000000-0000-4000-8000-000000000001",
			attemptID: "5b000000-0000-4000-8000-000000000002", assignmentID: "5b000000-0000-4000-8000-000000000003",
			sessionID: "5b000000-0000-4000-8000-000000000004", settlementJobID: "5b000000-0000-4000-8000-000000000005",
			settlementLeaseToken: "5b000000-0000-4000-8000-000000000006", turnID: "5b000000-0000-4000-8000-000000000007",
			stopJobID: "5b000000-0000-4000-8000-000000000008", stopLeaseToken: "5b000000-0000-4000-8000-000000000009",
			settled: true, reopenRequested: true,
		},
		{
			name: "unsettled barrier", workflowID: "5c000000-0000-4000-8000-000000000001",
			attemptID: "5c000000-0000-4000-8000-000000000002", assignmentID: "5c000000-0000-4000-8000-000000000003",
			sessionID: "5c000000-0000-4000-8000-000000000004", settlementJobID: "5c000000-0000-4000-8000-000000000005",
			settlementLeaseToken: "5c000000-0000-4000-8000-000000000006",
		},
	}
	deadline := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	for index, fixture := range cases {
		closureID := "migration-closure-" + fixture.name
		retentionToken := "migration-retention-" + fixture.name
		if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, desired_assignment_status, desired_runtime_state,
    closure_id, closure_deadline, closure_retention_token, closure_reopen_requested
)
VALUES ($1, 100, 'owner', 'repo', $5, $5, 'CLOSING', 7, 'ACTIVE', 'ACTIVE',
        $6, $7, $8, $9);
INSERT INTO workflow_attempts (
    id, workflow_id, attempt_number, status, infrastructure_failure_limit
)
VALUES ($2, $1, 1, 'ACTIVE', 1);
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ($3, $1, 'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1',
        'sha256:migration', 'migration/' || $3::text);
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status,
    control_revision
)
VALUES ($4, $3, 1, 'migration-session-' || $4::text, 'runtime', '1',
        'sha256:migration', 'migration/' || $3::text, 'ACTIVE', 2)`,
			pgx.QueryExecModeSimpleProtocol, fixture.workflowID, fixture.attemptID, fixture.assignmentID, fixture.sessionID,
			index+101, closureID, deadline, retentionToken, fixture.reopenRequested); err != nil {
			t.Fatalf("seed %s hierarchy: %v", fixture.name, err)
		}
		if fixture.turnID != "" {
			if _, err := pool.Exec(ctx, `
INSERT INTO agent_turns (
    id, workflow_id, agent_session_id, workflow_attempt_id, turn_number,
    execution_epoch, operation_lineage_id, status, active, control_revision,
    agent_profile_commit_sha, agent_profile_content_sha256, agent_profile_config,
    mutation_admission_open, mutation_admission_closed_at, completed_at
)
VALUES ($1, $2, $3, $4, 1, 1, $1, 'INTERRUPTED', FALSE, 2,
        'migration-profile', decode(repeat('11', 32), 'hex'), '{}'::jsonb,
        FALSE, clock_timestamp(), clock_timestamp());
INSERT INTO jobs (
    id, queue, kind, payload, status, attempt_count, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch, completed_at, result
)
VALUES ($5, 'workflow', 'STOP_AGENT_TURN', '{}'::jsonb, 'SUCCEEDED', 1, 3,
        $7, $2, $4, $6, $3, $1, 1, clock_timestamp(), '{}'::jsonb);
INSERT INTO job_attempts (
    job_id, attempt_number, lease_owner, lease_token, status,
    leased_at, lease_expires_at, finished_at, result
)
VALUES ($5, 1, 'phase-eight-stopper', $8, 'SUCCEEDED',
        clock_timestamp() - interval '2 minutes', clock_timestamp() + interval '3 minutes',
        clock_timestamp() - interval '1 minute', '{}'::jsonb)`,
				pgx.QueryExecModeSimpleProtocol, fixture.turnID, fixture.workflowID, fixture.sessionID, fixture.attemptID,
				fixture.stopJobID, fixture.assignmentID, "migration-stop-"+fixture.stopJobID,
				fixture.stopLeaseToken); err != nil {
				t.Fatalf("seed %s settled source Turn: %v", fixture.name, err)
			}
		}
		settlementStatus := "AVAILABLE"
		attemptCount := 0
		var completedAt any
		var result any
		var assignmentScope, sessionScope, turnScope any
		var executionEpoch any
		turnID, sessionID := "", ""
		controlRevision := int64(0)
		if fixture.turnID != "" {
			assignmentScope, sessionScope, turnScope = fixture.assignmentID, fixture.sessionID, fixture.turnID
			executionEpoch, turnID, sessionID, controlRevision = int64(1), fixture.turnID, fixture.sessionID, 2
		}
		if fixture.settled {
			settlementStatus, attemptCount, completedAt, result = "SUCCEEDED", 1, time.Now(), json.RawMessage(`{}`)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, attempt_count, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch, completed_at, result
)
VALUES ($1, 'workflow', 'SETTLE_CLOSURE', jsonb_build_object(
            'workflow_id', $4::uuid, 'workflow_attempt_id', $5::uuid,
            'closure_id', $12::text, 'turn_id', $13::text,
            'session_id', $14::text, 'execution_epoch', COALESCE($9::bigint, 0),
            'control_revision', $15::bigint, 'workflow_revision', 7
        ), $2, $3, 3, $16, $4, $5, $6, $7, $8, $9, $10, $11)`,
			fixture.settlementJobID, settlementStatus, attemptCount, fixture.workflowID,
			fixture.attemptID, assignmentScope, sessionScope, turnScope, executionEpoch,
			completedAt, result, closureID, turnID, sessionID, controlRevision,
			"migration-settlement-"+fixture.settlementJobID); err != nil {
			t.Fatalf("seed %s settlement Job: %v", fixture.name, err)
		}
		if fixture.settled {
			if _, err := pool.Exec(ctx, `
INSERT INTO job_attempts (
    job_id, attempt_number, lease_owner, lease_token, status,
    leased_at, lease_expires_at, finished_at, result
)
VALUES ($1, 1, 'phase-eight-settler', $2, 'SUCCEEDED',
        clock_timestamp() - interval '1 minute', clock_timestamp() + interval '4 minutes',
        clock_timestamp(), '{}'::jsonb)`, fixture.settlementJobID, fixture.settlementLeaseToken); err != nil {
				t.Fatalf("seed %s successful settlement attempt: %v", fixture.name, err)
			}
		}
		if fixture.turnID == "" {
			var barrierSettledAt any
			if fixture.settled {
				barrierSettledAt = time.Now()
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO workflow_closure_barriers (
    workflow_id, closure_id, workflow_revision, mutation_admission_closed_at,
    settled_at, settlement_job_id
)
VALUES ($1, $2, 7, clock_timestamp(), $3, $4)`, fixture.workflowID, closureID,
				barrierSettledAt, fixture.settlementJobID); err != nil {
				t.Fatalf("seed %s barrier: %v", fixture.name, err)
			}
		} else if _, err := pool.Exec(ctx, `
INSERT INTO workflow_closure_barriers (
    workflow_id, closure_id, workflow_revision, source_turn_id, source_session_id,
    source_attempt_id, source_execution_epoch, source_control_revision,
    mutation_admission_closed_at, runtime_stopped_at, settled_at, stop_job_id,
    settlement_job_id
)
VALUES ($1, $2, 7, $3, $4, $5, 1, 2, clock_timestamp(), clock_timestamp(),
        clock_timestamp(), $6, $7)`, fixture.workflowID, closureID, fixture.turnID,
			fixture.sessionID, fixture.attemptID, fixture.stopJobID, fixture.settlementJobID); err != nil {
			t.Fatalf("seed %s source barrier: %v", fixture.name, err)
		}
	}

	contents, err := migrations.Files.ReadFile("000011_durable_closure_retention.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply Phase 9 migration: %v", err)
	}

	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			var state, runtimeState, attemptStatus, assignmentStatus, sessionStatus string
			var revision int64
			var attemptActive, hasInternalEvent bool
			var retentionDeadline *time.Time
			var retentionToken string
			var generations, targets, closureEvents, labelJobs int
			if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.desired_runtime_state,
       workflow.retention_deadline,
       COALESCE(workflow.retention_token, ''), attempt.status, attempt.active,
       assignment.status, session.status, barrier.internal_event_id IS NOT NULL,
       (SELECT count(*) FROM assignment_retention_generations WHERE workflow_id = workflow.id),
       (SELECT count(*) FROM assignment_retention_targets AS target
        JOIN assignment_retention_generations AS generation ON generation.id = target.retention_generation_id
        WHERE generation.workflow_id = workflow.id),
       (SELECT count(*) FROM workflow_internal_events WHERE workflow_id = workflow.id),
       (SELECT count(*) FROM jobs WHERE workflow_id = workflow.id
        AND workflow_internal_event_id IS NOT NULL AND kind = 'RECONCILE_GITHUB_LABELS')
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.id = $2
JOIN agent_assignments AS assignment ON assignment.id = $3
JOIN agent_sessions AS session ON session.id = $4
JOIN workflow_closure_barriers AS barrier ON barrier.workflow_id = workflow.id
WHERE workflow.id = $1`, fixture.workflowID, fixture.attemptID, fixture.assignmentID, fixture.sessionID).Scan(
				&state, &revision, &runtimeState, &retentionDeadline, &retentionToken,
				&attemptStatus, &attemptActive, &assignmentStatus, &sessionStatus, &hasInternalEvent,
				&generations, &targets, &closureEvents, &labelJobs); err != nil {
				t.Fatal(err)
			}
			if !fixture.settled {
				if state != "CLOSING" || revision != 7 || runtimeState != "ACTIVE" || !attemptActive ||
					assignmentStatus != "ACTIVE" || sessionStatus != "ACTIVE" || hasInternalEvent ||
					generations != 0 || closureEvents != 0 || labelJobs != 0 {
					t.Fatalf("unsettled migration changed state: %s@%d runtime %s attempt %s/%t hierarchy %s/%s event %t generations/events/labels %d/%d/%d",
						state, revision, runtimeState, attemptStatus, attemptActive, assignmentStatus,
						sessionStatus, hasInternalEvent, generations, closureEvents, labelJobs)
				}
				return
			}
			wantState, wantGenerations, wantTargets := "CLOSED", 1, 2
			if fixture.reopenRequested {
				wantState, wantGenerations, wantTargets = "DORMANT", 0, 0
			}
			if state != wantState || revision != 8 || runtimeState != "RETAINED" || attemptActive || attemptStatus != "ISSUE_CLOSED" ||
				assignmentStatus != "COMPLETED" || sessionStatus != "RETAINED" || !hasInternalEvent ||
				generations != wantGenerations || targets != wantTargets || closureEvents != 1 || labelJobs != 1 {
				t.Fatalf("settled migration = %s@%d runtime %s attempt %s/%t hierarchy %s/%s event %t generations/targets/events/labels %d/%d/%d/%d",
					state, revision, runtimeState, attemptStatus, attemptActive, assignmentStatus,
					sessionStatus, hasInternalEvent, generations, targets, closureEvents, labelJobs)
			}
			if fixture.reopenRequested {
				if retentionDeadline != nil || retentionToken != "" {
					t.Fatalf("reopened migration retention = %v/%q, want none", retentionDeadline, retentionToken)
				}
			} else if retentionDeadline == nil || !retentionDeadline.Equal(deadline) || retentionToken != "migration-retention-"+fixture.name {
				t.Fatalf("closed migration retention = %v/%q, want %v/%q", retentionDeadline, retentionToken, deadline, "migration-retention-"+fixture.name)
			}
			if !fixture.reopenRequested {
				var generationDeadline, collectionAvailableAt time.Time
				var generationToken, payloadToken string
				if err := pool.QueryRow(ctx, `
SELECT generation.retention_deadline, generation.retention_token,
       collection.available_at, collection.payload->>'retention_token'
FROM assignment_retention_generations AS generation
JOIN jobs AS collection ON collection.id = generation.collection_job_id
WHERE generation.workflow_id = $1`, fixture.workflowID).Scan(
					&generationDeadline, &generationToken, &collectionAvailableAt, &payloadToken); err != nil {
					t.Fatal(err)
				}
				if !generationDeadline.Equal(deadline) || !collectionAvailableAt.Equal(deadline) ||
					generationToken != retentionToken || payloadToken != retentionToken {
					t.Fatalf("retention generation = deadline %v available %v tokens %q/%q, want %v/%q",
						generationDeadline, collectionAvailableAt, generationToken, payloadToken, deadline, retentionToken)
				}
			}
			if fixture.turnID != "" {
				var turnStatus string
				var turnActive bool
				if err := pool.QueryRow(ctx, `SELECT status, active FROM agent_turns WHERE id = $1`, fixture.turnID).Scan(&turnStatus, &turnActive); err != nil {
					t.Fatal(err)
				}
				if turnStatus != "INTERRUPTED" || turnActive {
					t.Fatalf("settled Turn history = %s/%t, want preserved INTERRUPTED/false", turnStatus, turnActive)
				}
			}
		})
	}
}

func TestClosureSettlementWithoutActiveTurnCompletesAndRetainsConcreteHierarchy(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 31)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prepareClosableFixture(t, pool, fixture)

	observedAt := time.Now().UTC()
	retainUntil := observedAt.Add(30 * 24 * time.Hour)
	settlement := closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 31,
		"51000000-0000-4000-8000-000000000311", "closure-no-turn", "retention-no-turn", observedAt, retainUntil)
	if settlement.SourceTurnID != "" || settlement.SettledAt == nil || settlement.CurrentWorkflowRevision != 3 {
		t.Fatalf("closure settlement = %#v", settlement)
	}

	var workflowState, desiredStatus, desiredRuntime, assignmentStatus, sessionStatus string
	var assignmentCompletedAt, assignmentRetention, sessionRetainedAt *time.Time
	var collectJobs, redundantJobs, internalEvents int
	if err := pool.QueryRow(ctx, `
SELECT status, desired_assignment_status, desired_runtime_state
FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowState, &desiredStatus, &desiredRuntime); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT status, completed_at, retention_until FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&assignmentStatus, &assignmentCompletedAt, &assignmentRetention); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, retained_at FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(&sessionStatus, &sessionRetainedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind = 'COLLECT_ASSIGNMENTS'),
       count(*) FILTER (WHERE kind IN ('COMPLETE_ASSIGNMENTS', 'CANCEL_ASSIGNMENT_RETENTION'))
FROM jobs WHERE workflow_id = $1`, fixture.workflowID).Scan(&collectJobs, &redundantJobs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_internal_events WHERE workflow_id = $1 AND kind = 'CLOSURE_SETTLED' AND applied_at IS NOT NULL`, fixture.workflowID).Scan(&internalEvents); err != nil {
		t.Fatal(err)
	}
	if workflowState != "CLOSED" || desiredStatus != "COMPLETED" || desiredRuntime != "RETAINED" ||
		assignmentStatus != "COMPLETED" || assignmentCompletedAt == nil || assignmentRetention == nil || !assignmentRetention.Equal(retainUntil) ||
		sessionStatus != "RETAINED" || sessionRetainedAt == nil || collectJobs != 1 || redundantJobs != 0 || internalEvents != 1 {
		t.Errorf("closure hierarchy = Workflow %s/%s/%s, Assignment %s/%v/%v, Session %s/%v, Jobs %d/%d, events %d",
			workflowState, desiredStatus, desiredRuntime, assignmentStatus, assignmentCompletedAt,
			assignmentRetention, sessionStatus, sessionRetainedAt, collectJobs, redundantJobs, internalEvents)
	}
	var payload struct {
		RetainUntil time.Time `json:"retain_until"`
	}
	var rawPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM jobs WHERE workflow_id = $1 AND kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID).Scan(&rawPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil || !payload.RetainUntil.Equal(retainUntil) {
		t.Errorf("authoritative retention payload = %s (%v), want %s", rawPayload, err, retainUntil)
	}
}

func TestClosureSettlementCancelsInitialPreparationWithoutCreatingEmptyRetention(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	application := triggerPreparationWorkflow(t, databases[0], ctx,
		"61000000-0000-4000-8000-000000000036", "62000000-0000-4000-8000-000000000036")
	preparation := claimPreparationJob(t, databases[0], ctx)

	delivery := workflowDelivery("51000000-0000-4000-8000-000000000361")
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	observedAt := time.Now().UTC()
	closed, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"), workflowLocator(), func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{EventMetadata: workflow.EventMetadata{
				ID: claim.DeliveryID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
			}, ClosureID: "closure-empty-preparation", RetainUntil: observedAt.Add(24 * time.Hour), RetentionToken: "retention-empty-preparation"})
		})
	if err != nil || closed.State != workflow.StateClosing {
		t.Fatalf("close unprepared Workflow = (%#v, %v)", closed, err)
	}
	settlement, err := databases[1].ClaimJobKind(ctx, store.WorkflowActionQueue, store.SettleClosureJobKind, "empty-closure-settler", time.Minute)
	if err != nil || settlement == nil {
		t.Fatalf("claim empty closure settlement = (%#v, %v)", settlement, err)
	}

	start := make(chan struct{})
	var preparationErr, settlementErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, preparationErr = databases[0].PrepareAgentTurn(ctx, preparation, preparationSpec("closure-race", "openai/closure-race"))
	}()
	go func() {
		defer wait.Done()
		<-start
		_, settlementErr = databases[1].CompleteClosureSettlement(ctx, *settlement)
	}()
	close(start)
	wait.Wait()
	if !errors.Is(preparationErr, store.ErrAgentTurnPreparationFenceLost) || settlementErr != nil {
		t.Fatalf("preparation/closure race = preparation %v, settlement %v", preparationErr, settlementErr)
	}

	var workflowState, runtimeState, preparationState, attemptState string
	var assignments, generations, collectionJobs int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.desired_runtime_state, preparation.status,
       attempt.status,
       (SELECT count(*) FROM agent_assignments WHERE workflow_id = workflow.id),
       (SELECT count(*) FROM assignment_retention_generations WHERE workflow_id = workflow.id),
       (SELECT count(*) FROM jobs WHERE workflow_id = workflow.id AND kind = 'COLLECT_ASSIGNMENTS')
FROM workflows AS workflow
JOIN jobs AS preparation ON preparation.id = $2
JOIN job_attempts AS attempt ON attempt.job_id = preparation.id AND attempt.attempt_number = $3
WHERE workflow.id = $1`, application.WorkflowID, preparation.ID, preparation.Attempt).Scan(
		&workflowState, &runtimeState, &preparationState, &attemptState,
		&assignments, &generations, &collectionJobs); err != nil {
		t.Fatal(err)
	}
	if workflowState != string(workflow.StateClosed) || runtimeState != string(workflow.RuntimeStateCollected) ||
		preparationState != string(store.JobCancelled) || attemptState != "FAILED" ||
		assignments != 0 || generations != 0 || collectionJobs != 0 {
		t.Fatalf("empty closure = Workflow %s/%s, preparation %s/%s, Assignments %d, generations %d, collection Jobs %d",
			workflowState, runtimeState, preparationState, attemptState, assignments, generations, collectionJobs)
	}
}

func TestClosureRetainsAndCollectsPreparedCreatingSessionWithoutFabricatingACPIdentity(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 35)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prepareClosableFixture(t, pool, fixture)
	contentSHA := strings.Repeat("a", 64)
	image := "registry.example/runtime@sha256:" + strings.Repeat("b", 64)
	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments
SET runtime_profile_content_sha256 = $2, runtime_image_digest = $3
WHERE id = $1`, fixture.assignmentID, contentSHA, image); err != nil {
		t.Fatalf("prepare immutable Assignment binding: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_sessions
SET status = 'CREATING', acp_session_id = NULL,
    runtime_profile_content_sha256 = $2, runtime_image_digest = $3
WHERE id = $1`, fixture.sessionID, contentSHA, image); err != nil {
		t.Fatalf("prepare CREATING Agent Session: %v", err)
	}

	observedAt := time.Now().UTC().Add(-2 * time.Hour)
	closeAndSettleWithoutTurn(t, database, ctx, fixture, 35,
		"51000000-0000-4000-8000-000000000351", "closure-creating", "retention-creating",
		observedAt, observedAt.Add(time.Hour))
	bindings, err := database.ListProtectedRuntimeBindings(ctx)
	if err != nil || len(bindings) != 1 || bindings[0].ContentSHA256 != contentSHA || bindings[0].Image != image {
		t.Fatalf("ListProtectedRuntimeBindings() before collection = (%#v, %v)", bindings, err)
	}
	var sessionStatus string
	var acpSessionID *string
	var retainedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, acp_session_id, retained_at FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(
		&sessionStatus, &acpSessionID, &retainedAt); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != "RETAINED" || acpSessionID != nil || retainedAt == nil {
		t.Fatalf("retained prepared Session = status %s, ACP %v, retained %v", sessionStatus, acpSessionID, retainedAt)
	}

	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "creating-session-collector", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim creating-session collection = (%#v, %v)", lease, err)
	}
	authorization, err := database.AuthorizeAssignmentCollection(ctx, *lease)
	if err != nil || len(authorization.Targets) != 2 || authorization.Targets[1].SessionID != fixture.sessionID {
		t.Fatalf("AuthorizeAssignmentCollection() = (%#v, %v), want Assignment and CREATING Session targets", authorization, err)
	}
	if _, err := database.FinalizeAssignmentCollection(ctx, *lease, authorization.Targets); err != nil {
		t.Fatalf("FinalizeAssignmentCollection() error = %v", err)
	}
	bindings, err = database.ListProtectedRuntimeBindings(ctx)
	if err != nil || len(bindings) != 0 {
		t.Fatalf("ListProtectedRuntimeBindings() after collection = (%#v, %v), want none", bindings, err)
	}
	var sessionDeletedAt *time.Time
	var sessions, attempts int
	if err := pool.QueryRow(ctx, `
SELECT session.status, session.acp_session_id, session.state_deleted_at,
       (SELECT count(*) FROM agent_sessions WHERE agent_assignment_id = $2),
       (SELECT count(*) FROM workflow_attempts WHERE workflow_id = $3)
FROM agent_sessions AS session WHERE session.id = $1`, fixture.sessionID, fixture.assignmentID, fixture.workflowID).Scan(
		&sessionStatus, &acpSessionID, &sessionDeletedAt, &sessions, &attempts); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != "DELETED" || acpSessionID != nil || sessionDeletedAt == nil || sessions != 1 || attempts != 1 {
		t.Fatalf("collected prepared Session history = status %s, ACP %v, deleted %v, rows %d/%d",
			sessionStatus, acpSessionID, sessionDeletedAt, sessions, attempts)
	}
}

func TestReopenBeforeCollectionReusesPreparedCreatingSessionAndBindsACPOnLaunch(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 73)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bindings := preparationBindings()
	developerBinding := bindings[workflow.RoleDeveloper]
	setFixtureRuntimeBinding(t, pool, fixture, runtimeprofile.Binding{
		Name: developerBinding.RuntimeProfileName, Version: developerBinding.RuntimeProfileVersion,
		ContentSHA256: developerBinding.RuntimeProfileContentSHA256, Image: developerBinding.RuntimeImageDigest,
	}, runtimeprofile.Binding{
		Name: developerBinding.RuntimeProfileName, Version: developerBinding.RuntimeProfileVersion,
		ContentSHA256: developerBinding.RuntimeProfileContentSHA256, Image: developerBinding.RuntimeImageDigest,
	})
	reviewerBinding := bindings[workflow.RoleReviewer]
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_profile_content_sha256, runtime_image_digest, runtime_state_path
)
VALUES ('40000000-0000-4000-8000-000000000735', $1, 'REVIEWER', 'ACTIVE',
        $2, $3, $4, $5, $6, 'prepared-never-started/reviewer')`, fixture.workflowID,
		reviewerBinding.AgentProfileName, reviewerBinding.RuntimeProfileName,
		reviewerBinding.RuntimeProfileVersion, reviewerBinding.RuntimeProfileContentSHA256,
		reviewerBinding.RuntimeImageDigest); err != nil {
		t.Fatalf("seed Reviewer Assignment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_sessions
SET status = 'CREATING', acp_session_id = NULL, capabilities = '{}'::jsonb,
    activated_at = NULL
WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatalf("prepare never-started Agent Session: %v", err)
	}
	prepareClosableFixture(t, pool, fixture)

	observedAt := time.Now().UTC()
	closeAndSettleWithoutTurn(t, database, ctx, fixture, 73,
		"51000000-0000-4000-8000-000000000731", "closure-prepared", "retention-prepared",
		observedAt, observedAt.Add(time.Hour))
	applyReopen(t, database, ctx, fixture, 73, "51000000-0000-4000-8000-000000000732")
	applyTrigger(t, database, ctx, fixture, 73,
		"51000000-0000-4000-8000-000000000733", "52000000-0000-4000-8000-000000000733")

	prepared := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx),
		"prepared-reactivation", "openai/reactivated")
	if prepared.Assignment.ID != fixture.assignmentID || prepared.Session.ID != fixture.sessionID ||
		prepared.Assignment.AssignmentRuntimeBinding != developerBinding ||
		prepared.Session.Status != store.AgentSessionCreating || prepared.Session.ACPSessionID != "" ||
		prepared.Session.RetainedAt != nil {
		t.Fatalf("reactivated prepared Session = %#v / %#v", prepared.Assignment, prepared.Session)
	}
	var sessions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_sessions WHERE agent_assignment_id = $1`, fixture.assignmentID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("Agent Sessions after retained preparation = %d, want exact retained identity", sessions)
	}

	lease := acquireAndBindTurn(t, database, ctx, prepared, "created-after-reactivation")
	bound, err := database.GetAgentSession(ctx, prepared.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Status != store.AgentSessionActive || bound.ACPSessionID != "created-after-reactivation" {
		t.Fatalf("Agent Session after launch binding = %#v", bound)
	}
	settleAcquiredTurn(t, database, ctx, lease, store.AgentTurnSucceeded)
}

func TestReopenBeforeCollectionAuthorizationCancelsLeasedGenerationAndRetainedTrigger(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prepareClosableFixture(t, pool, fixture)

	observedAt := time.Now().UTC()
	closeAndSettleWithoutTurn(t, database, ctx, fixture, 32,
		"51000000-0000-4000-8000-000000000321", "closure-cancel", "retention-cancel", observedAt, observedAt.Add(time.Hour))
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = clock_timestamp() WHERE workflow_id = $1 AND kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "collector-before-reopen", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}
	applyReopen(t, database, ctx, fixture, 32, "51000000-0000-4000-8000-000000000322")
	if _, err := database.AuthorizeAssignmentCollection(ctx, *lease); !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Errorf("authorize cancelled lease error = %v, want fence lost", err)
	}
	var state, generationStatus, jobStatus, assignmentStatus, sessionStatus string
	var retentionUntil *time.Time
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM assignment_retention_generations WHERE workflow_id = $1`, fixture.workflowID).Scan(&generationStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, lease.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, retention_until FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&assignmentStatus, &retentionUntil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(&sessionStatus); err != nil {
		t.Fatal(err)
	}
	if state != "DORMANT" || generationStatus != "CANCELLED" || jobStatus != "CANCELLED" ||
		assignmentStatus != "COMPLETED" || retentionUntil != nil || sessionStatus != "RETAINED" {
		t.Errorf("cancelled retention = Workflow %s, generation %s, Job %s, Assignment %s/%v, Session %s",
			state, generationStatus, jobStatus, assignmentStatus, retentionUntil, sessionStatus)
	}

	applyTrigger(t, database, ctx, fixture, 32, "51000000-0000-4000-8000-000000000323", "52000000-0000-4000-8000-000000000323")
	var mode string
	if err := pool.QueryRow(ctx, `SELECT payload->>'mode' FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN' ORDER BY created_at DESC LIMIT 1`, fixture.workflowID).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != string(workflow.AssignmentGenerationRetained) {
		t.Errorf("reopened preparation mode = %q, want RETAINED", mode)
	}
}

func TestAssignmentCollectionAuthorizationIsIrrevocableAndRecoversAfterCollectorCrash(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 33)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	preparationBinding := preparationBindings()[workflow.RoleDeveloper]
	pinnedBinding := runtimeprofile.Binding{
		Name: preparationBinding.RuntimeProfileName, Version: preparationBinding.RuntimeProfileVersion,
		ContentSHA256: preparationBinding.RuntimeProfileContentSHA256, Image: preparationBinding.RuntimeImageDigest,
	}
	setFixtureRuntimeBinding(t, pool, fixture, pinnedBinding, pinnedBinding)
	prepareClosableFixture(t, pool, fixture)
	observedAt := time.Now().UTC().Add(-31 * 24 * time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 33,
		"51000000-0000-4000-8000-000000000331", "closure-crash", "retention-crash", observedAt, observedAt.Add(30*24*time.Hour))
	first, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "collector-crash", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", first, err)
	}
	authorized, err := databases[0].AuthorizeAssignmentCollection(ctx, *first)
	if err != nil || len(authorized.Targets) != 2 {
		t.Fatalf("AuthorizeAssignmentCollection() = (%#v, %v)", authorized, err)
	}
	var firstAuthorizedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT authorized_at FROM assignment_retention_generations WHERE id = $1`, authorized.GenerationID).Scan(&firstAuthorizedAt); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].CompleteJob(ctx, *first, json.RawMessage(`{}`)); !errors.Is(err, store.ErrWorkflowJobRequiresAcknowledgement) {
		t.Errorf("generic collection completion error = %v", err)
	}

	reopenErr := applyReopenError(databases[1], ctx, fixture, 33, "51000000-0000-4000-8000-000000000332")
	if !errors.Is(reopenErr, store.ErrAssignmentCollectionIrrevocable) {
		t.Errorf("reopen after authorization error = %v, want irrevocable", reopenErr)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1 AND attempt_number = $2`, first.ID, first.Attempt); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := databases[0].ReclaimExpiredJobs(ctx, 10); err != nil || reclaimed != 1 {
		t.Fatalf("reclaim crashed collector = (%d, %v)", reclaimed, err)
	}
	second, err := databases[1].ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "collector-retry", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("claim replacement collection Job = (%#v, %v)", second, err)
	}
	reauthorized, err := databases[1].AuthorizeAssignmentCollection(ctx, *second)
	if err != nil || fmt.Sprint(reauthorized.Targets) != fmt.Sprint(authorized.Targets) {
		t.Fatalf("replacement authorization = (%#v, %v), want immutable targets %#v", reauthorized, err, authorized.Targets)
	}
	var reauthorizedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT authorized_at FROM assignment_retention_generations WHERE id = $1`, authorized.GenerationID).Scan(&reauthorizedAt); err != nil {
		t.Fatal(err)
	}
	if !reauthorizedAt.Equal(firstAuthorizedAt) {
		t.Fatalf("replacement authorization time = %v, want original due proof %v", reauthorizedAt, firstAuthorizedAt)
	}
	stale := *second
	stale.LeaseToken = "53000000-0000-4000-8000-000000000001"
	if _, err := databases[1].AuthorizeAssignmentCollection(ctx, stale); !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Errorf("stale lease authorization error = %v", err)
	}
	if _, err := databases[1].FinalizeAssignmentCollection(ctx, *second, reauthorized.Targets[:1]); !errors.Is(err, store.ErrAssignmentCollectionIncomplete) {
		t.Errorf("partial cleanup finalization error = %v", err)
	}
	collected, err := databases[1].FinalizeAssignmentCollection(ctx, *second, reauthorized.Targets)
	if err != nil {
		t.Fatalf("FinalizeAssignmentCollection() error = %v", err)
	}
	resolved, err := databases[1].FinalizeAssignmentCollection(ctx, *second, nil)
	if err != nil || resolved.GenerationID != collected.GenerationID || !resolved.CollectedAt.Equal(collected.CollectedAt) {
		t.Errorf("resolved collection = (%#v, %v), want %#v", resolved, err, collected)
	}
	var workflowRuntime, assignmentStatus, sessionStatus, generationStatus string
	var assignmentDeletedAt, sessionDeletedAt *time.Time
	var assignments, sessions, turns, attempts, events int
	if err := pool.QueryRow(ctx, `SELECT desired_runtime_state FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowRuntime); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, state_deleted_at FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&assignmentStatus, &assignmentDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, state_deleted_at FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(&sessionStatus, &sessionDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM assignment_retention_generations WHERE id = $1`, collected.GenerationID).Scan(&generationStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_sessions WHERE agent_assignment_id = $2),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1),
       (SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1),
       (SELECT count(*) FROM workflow_internal_events WHERE workflow_id = $1 AND applied_at IS NOT NULL)`, fixture.workflowID, fixture.assignmentID).Scan(&assignments, &sessions, &turns, &attempts, &events); err != nil {
		t.Fatal(err)
	}
	if workflowRuntime != "COLLECTED" || assignmentStatus != "COMPLETED" || assignmentDeletedAt == nil ||
		sessionStatus != "DELETED" || sessionDeletedAt == nil || generationStatus != "COLLECTED" ||
		assignments != 1 || sessions != 1 || turns != 0 || attempts != 1 || events != 2 {
		t.Errorf("collected history = runtime %s, Assignment %s/%v (%d), Session %s/%v (%d), generation %s, turns %d, attempts %d, events %d",
			workflowRuntime, assignmentStatus, assignmentDeletedAt, assignments, sessionStatus,
			sessionDeletedAt, sessions, generationStatus, turns, attempts, events)
	}

	applyReopen(t, databases[0], ctx, fixture, 33, "51000000-0000-4000-8000-000000000333")
	applyTrigger(t, databases[0], ctx, fixture, 33, "51000000-0000-4000-8000-000000000334", "52000000-0000-4000-8000-000000000334")
	preparationLease := claimPreparationJob(t, databases[0], ctx)
	var preparationPayload struct {
		Mode workflow.AssignmentGeneration `json:"mode"`
	}
	if err := json.Unmarshal(preparationLease.Payload, &preparationPayload); err != nil || preparationPayload.Mode != workflow.AssignmentGenerationNew {
		t.Fatalf("post-collection preparation payload = %s (%v), want NEW", preparationLease.Payload, err)
	}
	prepared := prepareTurn(t, databases[0], ctx, preparationLease, "post-collection", "openai/new")
	if prepared.Assignment.ID == fixture.assignmentID || prepared.Assignment.Generation != 2 {
		t.Fatalf("post-collection Assignment = %#v, want new generation", prepared.Assignment)
	}
	turnLease := acquireAndBindTurn(t, databases[0], ctx, prepared, "post-collection-acp")
	settleAcquiredTurn(t, databases[0], ctx, turnLease, store.AgentTurnSucceeded)
	secondObservedAt := time.Now().UTC()
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 33,
		"51000000-0000-4000-8000-000000000335", "closure-second-generation", "retention-second-generation",
		secondObservedAt, secondObservedAt.Add(30*24*time.Hour))
	var generations, scheduled, collectedGenerations, oldTargets int
	if err := pool.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE status = 'SCHEDULED'),
       count(*) FILTER (WHERE status = 'COLLECTED')
FROM assignment_retention_generations WHERE workflow_id = $1`, fixture.workflowID).Scan(&generations, &scheduled, &collectedGenerations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM assignment_retention_targets AS target
JOIN assignment_retention_generations AS generation ON generation.id = target.retention_generation_id
WHERE generation.workflow_id = $1 AND generation.retention_token = 'retention-second-generation'
  AND target.assignment_id = $2`, fixture.workflowID, fixture.assignmentID).Scan(&oldTargets); err != nil {
		t.Fatal(err)
	}
	if generations != 2 || scheduled != 1 || collectedGenerations != 1 || oldTargets != 0 {
		t.Errorf("retention generations = total %d, scheduled %d, collected %d, old targets in new %d",
			generations, scheduled, collectedGenerations, oldTargets)
	}
}

func TestAssignmentCollectionFailureDoesNotTrustCallerClaimOfInvalidTargets(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 37)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	prepareClosableFixture(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	observedAt := time.Now().UTC().Add(-time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 37,
		"51000000-0000-4000-8000-000000000371", "closure-false-invalid", "retention-false-invalid",
		observedAt, observedAt.Add(time.Minute))
	lease, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "false-invalid-collector", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}
	claimedInvalid := fmt.Errorf("caller classified target: %w", store.ErrInvalidAssignmentCleanupTargets)
	if _, err := databases[0].AcknowledgeAssignmentCollectionFailure(ctx, *lease, claimedInvalid, false, time.Second); !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("AcknowledgeAssignmentCollectionFailure() error = %v, want durable revalidation failure", err)
	}

	var generationStatus, jobStatus string
	var handoffs int
	if err := pool.QueryRow(ctx, `
SELECT generation.status, job.status,
       (SELECT count(*) FROM jobs AS handoff
        WHERE handoff.workflow_id = generation.workflow_id
          AND handoff.kind = 'PUBLISH_HUMAN_HANDOFF'
          AND handoff.payload->>'reason' = 'assignment_collection_invalid_targets')
FROM assignment_retention_generations AS generation
JOIN jobs AS job ON job.id = generation.collection_job_id
WHERE generation.workflow_id = $1`, fixture.workflowID).Scan(&generationStatus, &jobStatus, &handoffs); err != nil {
		t.Fatal(err)
	}
	if generationStatus != "SCHEDULED" || jobStatus != string(store.JobLeased) || handoffs != 0 {
		t.Fatalf("caller-claimed invalid target outcome = generation %s, Job %s, handoffs %d",
			generationStatus, jobStatus, handoffs)
	}
}

func TestAssignmentCollectionFinalizationSurvivesWallClockRollbackAfterAuthorization(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 36)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	prepareClosableFixture(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	observedAt := time.Now().UTC()
	retainUntil := observedAt.Add(time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 36,
		"51000000-0000-4000-8000-000000000361", "closure-clock-rollback", "retention-clock-rollback",
		observedAt, retainUntil)
	if _, err := pool.Exec(ctx, `
UPDATE jobs
SET available_at = clock_timestamp()
WHERE workflow_id = $1 AND kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	lease, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "clock-rollback-collector", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}
	provedDueAt := retainUntil.Add(time.Minute)
	if _, err := pool.Exec(ctx, `
UPDATE assignment_retention_generations
SET status = 'COLLECTING', authorized_attempt_number = $2,
    authorized_lease_owner = $3, authorized_lease_token = $4,
    authorized_at = $5
WHERE collection_job_id = $1`, lease.ID, lease.Attempt, lease.LeaseOwner, lease.LeaseToken, provedDueAt); err != nil {
		t.Fatalf("represent pre-rollback authorization: %v", err)
	}
	targets := []store.AssignmentCleanupTarget{
		{AssignmentID: fixture.assignmentID, RuntimeStatePath: canonicalRuntimePath(fixture.assignmentID), RuntimeImageDigest: "sha256:test"},
		{AssignmentID: fixture.assignmentID, SessionID: fixture.sessionID, RuntimeStatePath: canonicalRuntimePath(fixture.assignmentID), RuntimeImageDigest: "sha256:test"},
	}

	collected, err := databases[0].FinalizeAssignmentCollection(ctx, *lease, targets)
	if err != nil {
		t.Fatalf("FinalizeAssignmentCollection() after clock rollback error = %v", err)
	}
	if collected.CollectedAt.Before(retainUntil) || collected.CollectedAt.Before(provedDueAt) {
		t.Fatalf("CollectedAt = %v, want no earlier than deadline %v and proven due time %v",
			collected.CollectedAt, retainUntil, provedDueAt)
	}
}

func TestConcurrentAssignmentCollectionAuthorizationAndReopenHasOneWinner(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 34)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	prepareClosableFixture(t, pool, fixture)
	observedAt := time.Now().UTC().Add(-31 * 24 * time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 34,
		"51000000-0000-4000-8000-000000000341", "closure-race", "retention-race", observedAt, observedAt.Add(30*24*time.Hour))
	lease, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "collector-race", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}

	start := make(chan struct{})
	var authorizationErr, reopenErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, authorizationErr = databases[0].AuthorizeAssignmentCollection(ctx, *lease)
	}()
	go func() {
		defer wait.Done()
		<-start
		reopenErr = applyReopenError(databases[1], ctx, fixture, 34, "51000000-0000-4000-8000-000000000342")
	}()
	close(start)
	wait.Wait()
	if authorizationErr == nil && !errors.Is(reopenErr, store.ErrAssignmentCollectionIrrevocable) {
		t.Fatalf("authorization won but reopen error = %v", reopenErr)
	}
	if reopenErr == nil && !errors.Is(authorizationErr, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("reopen won but authorization error = %v", authorizationErr)
	}
	if (authorizationErr == nil) == (reopenErr == nil) {
		t.Fatalf("race outcomes = authorize %v, reopen %v; want exactly one winner", authorizationErr, reopenErr)
	}
}

func prepareClosableFixture(t *testing.T, pool *pgxpool.Pool, fixture agentFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', desired_assignment_status = 'ACTIVE',
    desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("prepare closable fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("prepare closable Workflow Attempt: %v", err)
	}
}

func closeAndSettleWithoutTurn(t *testing.T, database *store.Store, ctx context.Context, fixture agentFixture, number int, deliveryID, closureID, retentionToken string, observedAt, retainUntil time.Time) store.ClosureSettlement {
	t.Helper()
	delivery := workflowDelivery(deliveryID)
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(number), int64(number), int64(number)
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{EventMetadata: workflow.EventMetadata{
				ID: claim.DeliveryID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem,
				ExpectedRevision: snapshot.Revision,
			}, ClosureID: closureID, RetainUntil: retainUntil, RetentionToken: retentionToken})
		})
	if err != nil || application.State != workflow.StateClosing {
		t.Fatalf("close Workflow = (%#v, %v)", application, err)
	}
	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.SettleClosureJobKind, "closure-settler", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim closure settlement = (%#v, %v)", lease, err)
	}
	settlement, err := database.CompleteClosureSettlement(ctx, *lease)
	if err != nil {
		t.Fatalf("CompleteClosureSettlement() error = %v", err)
	}
	return settlement
}

func applyReopen(t *testing.T, database *store.Store, ctx context.Context, fixture agentFixture, number int, deliveryID string) {
	t.Helper()
	if err := applyReopenError(database, ctx, fixture, number, deliveryID); err != nil {
		t.Fatalf("reopen Workflow: %v", err)
	}
}

func applyReopenError(database *store.Store, ctx context.Context, fixture agentFixture, number int, deliveryID string) error {
	delivery := workflowDelivery(deliveryID)
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(number), int64(number), int64(number)
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	if _, err := database.InsertWebhookDelivery(ctx, delivery); err != nil {
		return err
	}
	claim, err := database.ClaimWebhookDelivery(ctx, "reopen-test", time.Minute)
	if err != nil || claim == nil {
		return fmt.Errorf("claim reopen delivery: %w", err)
	}
	_, err = database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "reopened"),
		store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueReopenedEvent{EventMetadata: workflow.EventMetadata{
				ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt, WorkItem: snapshot.WorkItem,
				ExpectedRevision: snapshot.Revision,
			}})
		})
	return err
}

func applyTrigger(t *testing.T, database *store.Store, ctx context.Context, fixture agentFixture, number int, deliveryID, attemptID string) {
	t.Helper()
	delivery := workflowDelivery(deliveryID)
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(number), int64(number), int64(number)
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.TriggerEvent{EventMetadata: workflow.EventMetadata{
				ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt, WorkItem: snapshot.WorkItem,
				ExpectedRevision: snapshot.Revision,
			}, AttemptID: attemptID, AttemptNumber: snapshot.LastAttemptNumber + 1})
		})
	if err != nil || application.State != workflow.StateDeveloping {
		t.Fatalf("trigger reopened Workflow = (%#v, %v)", application, err)
	}
}
