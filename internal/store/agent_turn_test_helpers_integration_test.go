//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func prepareFixtureAgentTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, spec store.AgentTurnSpec) (store.AgentTurn, error) {
	t.Helper()
	preparationLease, preparation, err := claimFixtureAgentTurnPreparation(t, database, pool, ctx, spec)
	if err != nil {
		return store.AgentTurn{}, err
	}
	prepared, err := database.PrepareAgentTurn(ctx, preparationLease, preparation)
	if err != nil {
		if _, cleanupErr := pool.Exec(ctx, `DELETE FROM job_attempts WHERE job_id = $1`, preparationLease.ID); cleanupErr != nil {
			return store.AgentTurn{}, errors.Join(err, cleanupErr)
		}
		if _, cleanupErr := pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, preparationLease.ID); cleanupErr != nil {
			return store.AgentTurn{}, errors.Join(err, cleanupErr)
		}
		return store.AgentTurn{}, err
	}
	return prepared.Turn, nil
}

func claimFixtureAgentTurnPreparation(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, spec store.AgentTurnSpec) (store.JobLease, store.AgentTurnPreparationSpec, error) {
	t.Helper()
	var workflowID, role, profileName, runtimeName, runtimeVersion, runtimeHash, imageDigest string
	if err := pool.QueryRow(ctx, `
SELECT assignment.workflow_id::text, assignment.role,
       assignment.agent_profile_name, assignment.runtime_profile_name,
       assignment.runtime_profile_version, COALESCE(assignment.runtime_profile_content_sha256, ''),
       assignment.runtime_image_digest
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE session.id = $1`, spec.AgentSessionID).Scan(
		&workflowID, &role, &profileName, &runtimeName, &runtimeVersion, &runtimeHash, &imageDigest,
	); err != nil {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
	}
	if runtimeHash == "" {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, errors.New("prepare fixture Agent Turn: legacy Runtime Profile binding")
	}
	status := ""
	switch spec.Stage {
	case workflow.StageImplementation:
		status = string(workflow.StateDeveloping)
	case workflow.StageReview:
		status = string(workflow.StateReviewing)
	}
	if status != "" {
		if _, err := pool.Exec(ctx, `
UPDATE workflows
SET status = $2, state_revision = GREATEST(state_revision, 1),
    desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE'
WHERE id = $1 AND status = 'ACTIVE'`, workflowID, status); err != nil {
			return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET current_stage = $2 WHERE id = $1`, spec.WorkflowAttemptID, spec.Stage); err != nil {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
	}
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT state_revision FROM workflows WHERE id = $1`, workflowID).Scan(&revision); err != nil {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"mode": workflow.AssignmentGenerationCurrent, "stage": spec.Stage, "role": role,
		"purpose": spec.Purpose, "expected_head_sha": spec.ExpectedHeadSHA,
		"retry_of_turn_id": spec.RetryOfTurnID, "revision": revision,
	})
	if err != nil {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
	}
	var preparationJobID string
	if err := pool.QueryRow(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id
)
VALUES (gen_random_uuid(), $1, $2, $3, 'AVAILABLE', 1000, clock_timestamp(), 3,
        'test-preparation:' || gen_random_uuid()::text, $4, $5)
RETURNING id::text`, store.WorkflowActionQueue, store.PrepareAgentTurnJobKind, payload,
		workflowID, spec.WorkflowAttemptID).Scan(&preparationJobID); err != nil {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
	}
	if err := prioritizeFixtureJob(pool, ctx, preparationJobID, store.WorkflowActionQueue, store.PrepareAgentTurnJobKind); err != nil {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
	}
	preparationLease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.PrepareAgentTurnJobKind, "preparation-worker", time.Minute)
	if err != nil {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, err
	}
	if preparationLease == nil || preparationLease.ID != preparationJobID {
		return store.JobLease{}, store.AgentTurnPreparationSpec{}, errors.New("claim expected Agent Turn preparation job")
	}
	preparation := store.ParticipantPreparation{
		Binding: store.ParticipantRuntimeBinding{
			AgentProfileName: profileName, RuntimeProfileName: runtimeName,
			RuntimeProfileVersion: runtimeVersion, RuntimeProfileContentSHA256: runtimeHash,
			RuntimeImageDigest: imageDigest,
		},
		Profile: store.AgentProfileSnapshot{
			CommitSHA: spec.AgentProfileCommitSHA, ContentSHA256: spec.AgentProfileContentSHA256,
			Config: spec.AgentProfileConfig,
		},
		ProfilePath: ".omnigrex/team/" + profileName + ".md",
	}
	preparationSpec := store.AgentTurnPreparationSpec{
		Stages: map[workflow.StageID]store.ParticipantPreparation{spec.Stage: preparation},
	}
	return *preparationLease, preparationSpec, nil
}

func claimAndAcquireFixtureAgentTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, turn store.AgentTurn, owner string, leaseDuration time.Duration, concurrencyLimit int) (store.AgentTurnLease, error) {
	t.Helper()
	var jobID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE agent_turn_id = $1 AND kind = $2`, turn.ID, store.RunAgentTurnJobKind).Scan(&jobID); err != nil {
		return store.AgentTurnLease{}, err
	}
	if err := prioritizeFixtureJob(pool, ctx, jobID, store.AgentTurnQueue, store.RunAgentTurnJobKind); err != nil {
		return store.AgentTurnLease{}, err
	}
	lease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, owner, leaseDuration, concurrencyLimit)
	if err != nil {
		return store.AgentTurnLease{}, err
	}
	if !acquired || lease.ID != turn.ID || lease.ExecutionEpoch != turn.ExecutionEpoch {
		return store.AgentTurnLease{}, errors.New("claim expected Agent Turn execution job")
	}
	return lease, nil
}

func acquireFixtureAgentTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, job store.JobLease, controlRevision int64, owner string, leaseDuration time.Duration, concurrencyLimit int) (store.AgentTurnLease, error) {
	t.Helper()
	if err := prioritizeFixtureJob(pool, ctx, job.ID, store.AgentTurnQueue, store.RunAgentTurnJobKind); err != nil {
		return store.AgentTurnLease{}, err
	}
	lease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, owner, leaseDuration, concurrencyLimit)
	if err != nil {
		return store.AgentTurnLease{}, err
	}
	if !acquired || lease.JobLease.ID != job.ID || lease.ControlRevision != controlRevision {
		return store.AgentTurnLease{}, errors.New("claim expected Agent Turn execution job")
	}
	return lease, nil
}

func prioritizeFixtureJob(pool *pgxpool.Pool, ctx context.Context, jobID, queue, kind string) error {
	if _, err := pool.Exec(ctx, `UPDATE jobs SET priority = 2147483647 WHERE id = $1`, jobID); err != nil {
		return err
	}
	var nextJobID string
	if err := pool.QueryRow(ctx, `
SELECT id::text
FROM jobs
WHERE queue = $1 AND kind = $2 AND status = 'AVAILABLE'
  AND available_at <= clock_timestamp() AND attempt_count < max_attempts
ORDER BY priority DESC, available_at, id
LIMIT 1`, queue, kind).Scan(&nextJobID); err != nil {
		return err
	}
	if nextJobID != jobID {
		return errors.New("fixture job is not next in the production claim order")
	}
	return nil
}

func seedFixtureChangeProposal(t *testing.T, pool *pgxpool.Pool, ctx context.Context, workflowID, headSHA string) string {
	t.Helper()
	var proposalID string
	if err := pool.QueryRow(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, active,
    base_ref, base_sha, head_ref, head_sha
)
SELECT gen_random_uuid(), workflow.id, workflow.repository_id,
       workflow.repository_owner, workflow.repository_name,
       workflow.repository_id * 1000 + 1, workflow.issue_number,
       'OPEN', TRUE, 'main', 'base-sha', 'feature', $2
FROM workflows AS workflow
WHERE workflow.id = $1
RETURNING id::text`, workflowID, headSHA).Scan(&proposalID); err != nil {
		t.Fatal(err)
	}
	return proposalID
}
