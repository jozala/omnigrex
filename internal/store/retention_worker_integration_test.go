//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/retention"
	"github.com/jozala/omnigrex/internal/store"
)

func TestAssignmentRetentionWorkerWaitsForDeadlineAndLeavesNeighboringAssignmentUntouched(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 61)
	neighbor := seedAgentSession(t, pool, 62)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	makeFixtureRuntimePathCanonical(t, pool, neighbor)
	prepareClosableFixture(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	observedAt := time.Now().UTC()
	retainUntil := observedAt.Add(500 * time.Millisecond)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 61,
		"71000000-0000-4000-8000-000000000611", "worker-deadline", "worker-deadline-retention",
		observedAt, retainUntil)
	cleaner := &integrationRetentionCleaner{}
	worker := newIntegrationRetentionWorker(t, databases[0], cleaner, "deadline-worker")

	processed, err := worker.ProcessNext(ctx)
	if err != nil || processed {
		t.Fatalf("ProcessNext() before deadline = (%t, %v), want idle", processed, err)
	}
	if len(cleaner.callsSnapshot()) != 0 {
		t.Fatalf("cleanup before deadline = %#v", cleaner.callsSnapshot())
	}
	time.Sleep(time.Until(retainUntil) + 100*time.Millisecond)
	processed, err = worker.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessNext() after deadline = (%t, %v), want collection", processed, err)
	}
	want := []integrationCleanupCall{{assignmentID: fixture.assignmentID, path: canonicalRuntimePath(fixture.assignmentID)}}
	if calls := cleaner.callsSnapshot(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("cleanup calls = %#v, want %#v", calls, want)
	}

	var collectedRuntime, assignmentStatus, sessionStatus string
	var assignmentDeletedAt, sessionDeletedAt, neighborDeletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT desired_runtime_state FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&collectedRuntime); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, state_deleted_at FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&assignmentStatus, &assignmentDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, state_deleted_at FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(&sessionStatus, &sessionDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state_deleted_at FROM agent_assignments WHERE id = $1`, neighbor.assignmentID).Scan(&neighborDeletedAt); err != nil {
		t.Fatal(err)
	}
	if collectedRuntime != "COLLECTED" || assignmentStatus != "COMPLETED" || assignmentDeletedAt == nil ||
		sessionStatus != "DELETED" || sessionDeletedAt == nil || neighborDeletedAt != nil {
		t.Errorf("collection = runtime %s, Assignment %s/%v, Session %s/%v, neighbor deleted %v",
			collectedRuntime, assignmentStatus, assignmentDeletedAt, sessionStatus, sessionDeletedAt, neighborDeletedAt)
	}
}

func TestAssignmentRetentionWorkerRetriesPartialCleanupIdempotentlyAcrossLeaseAttempts(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 63)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	secondAssignmentID, secondSessionID := addRetainedFixtureAssignment(t, pool, fixture, 63)
	prepareClosableFixture(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	observedAt := time.Now().UTC().Add(-time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 63,
		"71000000-0000-4000-8000-000000000631", "worker-partial", "worker-partial-retention",
		observedAt, observedAt.Add(time.Minute))
	cleanupFailure := errors.New("runtime-state volume temporarily unavailable")
	cleaner := &integrationRetentionCleaner{results: []error{nil, cleanupFailure}}
	firstWorker := newIntegrationRetentionWorker(t, databases[0], cleaner, "partial-worker-one")

	processed, err := firstWorker.ProcessNext(ctx)
	if !processed || !errors.Is(err, cleanupFailure) {
		t.Fatalf("first ProcessNext() = (%t, %v), want partial cleanup failure", processed, err)
	}
	var jobStatus, generationStatus string
	var firstDeletedAt, secondDeletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE workflow_id = $1 AND kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM assignment_retention_generations WHERE workflow_id = $1`, fixture.workflowID).Scan(&generationStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state_deleted_at FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&firstDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state_deleted_at FROM agent_assignments WHERE id = $1`, secondAssignmentID).Scan(&secondDeletedAt); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "AVAILABLE" || generationStatus != "COLLECTING" || firstDeletedAt != nil || secondDeletedAt != nil {
		t.Fatalf("partial durable state = Job %s, generation %s, deleted (%v, %v)", jobStatus, generationStatus, firstDeletedAt, secondDeletedAt)
	}

	time.Sleep(5 * time.Millisecond)
	secondWorker := newIntegrationRetentionWorker(t, databases[1], cleaner, "partial-worker-two")
	processed, err = secondWorker.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("retry ProcessNext() = (%t, %v), want collection", processed, err)
	}
	wantCalls := []integrationCleanupCall{
		{assignmentID: fixture.assignmentID, path: canonicalRuntimePath(fixture.assignmentID)},
		{assignmentID: secondAssignmentID, path: canonicalRuntimePath(secondAssignmentID)},
		{assignmentID: fixture.assignmentID, path: canonicalRuntimePath(fixture.assignmentID)},
		{assignmentID: secondAssignmentID, path: canonicalRuntimePath(secondAssignmentID)},
	}
	if calls := cleaner.callsSnapshot(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Fatalf("cleanup calls across attempts = %#v, want %#v", calls, wantCalls)
	}
	var attempts, assignments, sessions int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM job_attempts AS attempt JOIN jobs AS job ON job.id = attempt.job_id
        WHERE job.workflow_id = $1 AND job.kind = 'COLLECT_ASSIGNMENTS'),
       (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_sessions WHERE agent_assignment_id IN ($2, $3))`,
		fixture.workflowID, fixture.assignmentID, secondAssignmentID).Scan(&attempts, &assignments, &sessions); err != nil {
		t.Fatal(err)
	}
	var firstSessionDeletedAt, secondSessionDeletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT state_deleted_at FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(&firstSessionDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state_deleted_at FROM agent_sessions WHERE id = $1`, secondSessionID).Scan(&secondSessionDeletedAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || assignments != 2 || sessions != 2 || firstSessionDeletedAt == nil || secondSessionDeletedAt == nil {
		t.Errorf("retained history = attempts %d, Assignments %d, Sessions %d, deleted (%v, %v)",
			attempts, assignments, sessions, firstSessionDeletedAt, secondSessionDeletedAt)
	}
}

func TestAssignmentRetentionWorkerExhaustionPublishesOnceAndContinuesUntilCollected(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 64)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	prepareClosableFixture(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	observedAt := time.Now().UTC().Add(-time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 64,
		"71000000-0000-4000-8000-000000000641", "worker-exhaustion", "worker-exhaustion-retention",
		observedAt, observedAt.Add(time.Minute))
	cleanupFailure := errors.New("runtime-state cleanup unavailable")
	cleaner := &integrationRetentionCleaner{results: []error{cleanupFailure, cleanupFailure, cleanupFailure}}
	worker := newIntegrationRetentionWorker(t, databases[0], cleaner, "exhaustion-worker")
	for attempt := 1; attempt <= 3; attempt++ {
		processed, err := worker.ProcessNext(ctx)
		if !processed || !errors.Is(err, cleanupFailure) {
			t.Fatalf("ProcessNext() attempt %d = (%t, %v)", attempt, processed, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	processed, err := worker.ProcessNext(ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessNext() after exhaustion = (%t, %v), want successful continuation", processed, err)
	}

	var jobStatus, generationStatus, assignmentStatus, sessionStatus string
	var assignmentDeletedAt, sessionDeletedAt *time.Time
	var attempts, handoffs int
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE workflow_id = $1 AND kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM assignment_retention_generations WHERE workflow_id = $1`, fixture.workflowID).Scan(&generationStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, state_deleted_at FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&assignmentStatus, &assignmentDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, state_deleted_at FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(&sessionStatus, &sessionDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts AS attempt JOIN jobs AS job ON job.id = attempt.job_id WHERE job.workflow_id = $1 AND job.kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF' AND payload->>'reason' = 'assignment_collection_exhausted'`, fixture.workflowID).Scan(&handoffs); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "SUCCEEDED" || generationStatus != "COLLECTED" || attempts != 4 || handoffs != 1 ||
		assignmentStatus != "COMPLETED" || sessionStatus != "DELETED" || assignmentDeletedAt == nil || sessionDeletedAt == nil {
		t.Errorf("continued collection = Job %s, generation %s, attempts %d, handoffs %d, Assignment %s/%v, Session %s/%v",
			jobStatus, generationStatus, attempts, handoffs, assignmentStatus, assignmentDeletedAt, sessionStatus, sessionDeletedAt)
	}
}

func TestAssignmentRetentionFinalAttemptCrashAfterDeletionContinuesToFinalize(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 65)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	prepareClosableFixture(t, pool, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	observedAt := time.Now().UTC().Add(-time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], ctx, fixture, 65,
		"71000000-0000-4000-8000-000000000651", "worker-final-crash", "worker-final-crash-retention",
		observedAt, observedAt.Add(time.Minute))
	cleanupFailure := errors.New("runtime-state cleanup temporarily unavailable")
	cleaner := &integrationRetentionCleaner{results: []error{cleanupFailure, cleanupFailure}}
	worker := newIntegrationRetentionWorker(t, databases[0], cleaner, "final-crash-worker")
	for attempt := 1; attempt <= 2; attempt++ {
		processed, err := worker.ProcessNext(ctx)
		if !processed || !errors.Is(err, cleanupFailure) {
			t.Fatalf("cleanup attempt %d = (%t, %v)", attempt, processed, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	crashedLease, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "crashed-final-attempt", 50*time.Millisecond)
	if err != nil || crashedLease == nil || crashedLease.Attempt != 3 {
		t.Fatalf("claim final attempt = (%#v, %v)", crashedLease, err)
	}
	authorization, err := databases[0].AuthorizeAssignmentCollection(ctx, *crashedLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleaner.EnsureAbsent(ctx, fixture.assignmentID, canonicalRuntimePath(fixture.assignmentID)); err != nil {
		t.Fatalf("simulate final-attempt physical deletion: %v", err)
	}
	if len(authorization.Targets) == 0 {
		t.Fatal("final attempt had no authorized cleanup targets")
	}
	time.Sleep(75 * time.Millisecond)
	replacement := newIntegrationRetentionWorker(t, databases[0], cleaner, "final-crash-replacement")
	if processed, err := replacement.ProcessNext(ctx); err != nil || processed {
		t.Fatalf("expired final-attempt reclaim = (%t, %v), want delayed continuation", processed, err)
	}
	time.Sleep(irrevocableRetryWait)
	if processed, err := replacement.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("post-crash continuation = (%t, %v)", processed, err)
	}

	var generationStatus, jobStatus string
	var attempts, handoffs int
	if err := pool.QueryRow(ctx, `
SELECT generation.status, job.status
FROM assignment_retention_generations AS generation
JOIN jobs AS job ON job.id = generation.collection_job_id
WHERE generation.workflow_id = $1`, fixture.workflowID).Scan(&generationStatus, &jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts AS attempt JOIN jobs AS job ON job.id = attempt.job_id WHERE job.workflow_id = $1 AND job.kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF' AND payload->>'reason' = 'assignment_collection_exhausted'`, fixture.workflowID).Scan(&handoffs); err != nil {
		t.Fatal(err)
	}
	if generationStatus != "COLLECTED" || jobStatus != string(store.JobSucceeded) || attempts != 4 || handoffs != 1 || len(cleaner.callsSnapshot()) != 4 {
		t.Fatalf("post-crash collection = generation %s, Job %s, attempts %d, handoffs %d, cleanup calls %d",
			generationStatus, jobStatus, attempts, handoffs, len(cleaner.callsSnapshot()))
	}
}

func newIntegrationRetentionWorker(t *testing.T, database *store.Store, cleaner *integrationRetentionCleaner, owner string) *retention.Worker {
	t.Helper()
	worker, err := retention.NewWorker(database, cleaner, retention.WorkerConfig{
		ClaimOwner: owner, LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Microsecond,
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return worker
}

func makeFixtureRuntimePathCanonical(t *testing.T, pool *pgxpool.Pool, fixture agentFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := canonicalRuntimePath(fixture.assignmentID)
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET runtime_state_path = $2 WHERE id = $1`, fixture.assignmentID, path); err != nil {
		t.Fatalf("make fixture Assignment runtime path canonical: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET runtime_state_path = $2 WHERE id = $1`, fixture.sessionID, path); err != nil {
		t.Fatalf("make fixture runtime path canonical: %v", err)
	}
}

func addRetainedFixtureAssignment(t *testing.T, pool *pgxpool.Pool, fixture agentFixture, number int) (string, string) {
	t.Helper()
	assignmentID := fmt.Sprintf("49000000-0000-4000-8000-%012d", number*10+5)
	sessionID := fmt.Sprintf("49000000-0000-4000-8000-%012d", number*10+6)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, 'REVIEWER', 'ACTIVE', 'reviewer', 'runtime', '1', 'sha256:test', $3)`,
		assignmentID, fixture.workflowID, canonicalRuntimePath(assignmentID)); err != nil {
		t.Fatalf("add retained fixture Assignment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ($1, $2, 1, $3, 'runtime', '1', 'sha256:test', $4, 'ACTIVE')`,
		sessionID, assignmentID, "session-"+sessionID, canonicalRuntimePath(assignmentID)); err != nil {
		t.Fatalf("add retained fixture Agent Session: %v", err)
	}
	return assignmentID, sessionID
}

func canonicalRuntimePath(assignmentID string) string {
	return "assignment-" + assignmentID + "/runtime-state"
}

type integrationCleanupCall struct {
	assignmentID string
	path         string
}

type integrationRetentionCleaner struct {
	mutex     sync.Mutex
	calls     []integrationCleanupCall
	results   []error
	alwaysErr error
}

const irrevocableRetryWait = 1100 * time.Millisecond

func (cleaner *integrationRetentionCleaner) EnsureAbsent(_ context.Context, assignmentID, path string) error {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	cleaner.calls = append(cleaner.calls, integrationCleanupCall{assignmentID: assignmentID, path: path})
	if len(cleaner.results) != 0 {
		result := cleaner.results[0]
		cleaner.results = cleaner.results[1:]
		return result
	}
	return cleaner.alwaysErr
}

func (cleaner *integrationRetentionCleaner) callsSnapshot() []integrationCleanupCall {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	return append([]integrationCleanupCall(nil), cleaner.calls...)
}
