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

	"github.com/jackc/pgx/v5/pgxpool"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"

	"github.com/jozala/omnigrex/internal/workflow"
)

func TestClosureSettlementWithoutActiveTurnCompletesAndRetainsConcreteHierarchy(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 31)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prepareClosableFixture(t, pool, fixture)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	retainUntil := observedAt.Add(30 * 24 * time.Hour)
	settlement := closeAndSettleWithoutTurn(t, databases[0], pool, ctx, fixture, 31,
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
	closed, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"), workflowLocator(), func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.IssueClosedEvent{EventMetadata: context.Metadata,
				ClosureID: "closure-empty-preparation", RetainUntil: context.Metadata.ObservedAt.Add(24 * time.Hour), RetentionToken: "retention-empty-preparation"}, nil
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

func TestClosureRetainsAndCollectsPreparedCreatingSessionWithoutFabricatingRuntimeIdentity(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	contentSHA := strings.Repeat("a", 64)
	image := "registry.example/runtime@sha256:" + strings.Repeat("b", 64)
	binding := runtimeprofile.Binding{Name: "pi-rpc", Version: "v2", ContentSHA256: contentSHA, Image: image}
	fixture := seedAgentSessionWithRuntimeBindingsAndStatus(t, pool, 35, workflow.RoleDeveloper, binding, binding, store.AgentSessionCreating)
	prepareClosableFixture(t, pool, fixture)

	observedAt := time.Now().UTC().Add(-2 * time.Hour)
	closeAndSettleWithoutTurn(t, database, pool, ctx, fixture, 35,
		"51000000-0000-4000-8000-000000000351", "closure-creating", "retention-creating",
		observedAt, observedAt.Add(time.Hour))
	bindings, err := database.ListProtectedRuntimeBindings(ctx)
	if err != nil || len(bindings) != 1 || bindings[0].ContentSHA256 != contentSHA || bindings[0].Image != image {
		t.Fatalf("ListProtectedRuntimeBindings() before collection = (%#v, %v)", bindings, err)
	}
	var sessionStatus string
	var runtimeSessionID *string
	var retainedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, runtime_session_id, retained_at FROM agent_sessions WHERE id = $1`, fixture.sessionID).Scan(
		&sessionStatus, &runtimeSessionID, &retainedAt); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != "RETAINED" || runtimeSessionID != nil || retainedAt == nil {
		t.Fatalf("retained prepared Session = status %s, runtime identity %v, retained %v", sessionStatus, runtimeSessionID, retainedAt)
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
SELECT session.status, session.runtime_session_id, session.state_deleted_at,
       (SELECT count(*) FROM agent_sessions WHERE agent_assignment_id = $2),
       (SELECT count(*) FROM workflow_attempts WHERE workflow_id = $3)
FROM agent_sessions AS session WHERE session.id = $1`, fixture.sessionID, fixture.assignmentID, fixture.workflowID).Scan(
		&sessionStatus, &runtimeSessionID, &sessionDeletedAt, &sessions, &attempts); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != "DELETED" || runtimeSessionID != nil || sessionDeletedAt == nil || sessions != 1 || attempts != 1 {
		t.Fatalf("collected prepared Session history = status %s, runtime identity %v, deleted %v, rows %d/%d",
			sessionStatus, runtimeSessionID, sessionDeletedAt, sessions, attempts)
	}
}

func TestReopenBeforeCollectionReusesPreparedCreatingSessionAndBindsRuntimeOnLaunch(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bindings := preparationBindings()
	developerBinding := bindings[workflow.RoleDeveloper]
	pinnedDeveloperBinding := runtimeprofile.Binding{
		Name: developerBinding.RuntimeProfileName, Version: developerBinding.RuntimeProfileVersion,
		ContentSHA256: developerBinding.RuntimeProfileContentSHA256, Image: developerBinding.RuntimeImageDigest,
	}
	fixture := seedAgentSessionWithRuntimeBindingsAndStatus(t, pool, 73, workflow.RoleDeveloper, pinnedDeveloperBinding, pinnedDeveloperBinding, store.AgentSessionCreating)
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
	prepareClosableFixture(t, pool, fixture)

	observedAt := time.Now().UTC()
	closeAndSettleWithoutTurn(t, database, pool, ctx, fixture, 73,
		"51000000-0000-4000-8000-000000000731", "closure-prepared", "retention-prepared",
		observedAt, observedAt.Add(time.Hour))
	applyReopen(t, database, ctx, 73, "51000000-0000-4000-8000-000000000732")
	applyTrigger(t, database, ctx, fixture, 73,
		"51000000-0000-4000-8000-000000000733", "52000000-0000-4000-8000-000000000733")

	prepared := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx),
		"prepared-reactivation", "openai/reactivated")
	if prepared.Assignment.ID != fixture.assignmentID || prepared.Session.ID != fixture.sessionID ||
		prepared.Assignment.ParticipantRuntimeBinding != developerBinding ||
		prepared.Session.Status != store.AgentSessionCreating || prepared.Session.RuntimeSessionID != "" ||
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

	lease := acquireAndBindTurn(t, database, pool, ctx, prepared, "created-after-reactivation")
	bound, err := database.GetAgentSession(ctx, prepared.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Status != store.AgentSessionActive || bound.RuntimeSessionID != prepared.Session.ID {
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
	closeAndSettleWithoutTurn(t, database, pool, ctx, fixture, 32,
		"51000000-0000-4000-8000-000000000321", "closure-cancel", "retention-cancel", observedAt, observedAt.Add(time.Hour))
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = clock_timestamp() WHERE workflow_id = $1 AND kind = 'COLLECT_ASSIGNMENTS'`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "collector-before-reopen", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}
	applyReopen(t, database, ctx, 32, "51000000-0000-4000-8000-000000000322")
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	preparationBinding := preparationBindings()[workflow.RoleDeveloper]
	pinnedBinding := runtimeprofile.Binding{
		Name: preparationBinding.RuntimeProfileName, Version: preparationBinding.RuntimeProfileVersion,
		ContentSHA256: preparationBinding.RuntimeProfileContentSHA256, Image: preparationBinding.RuntimeImageDigest,
	}
	fixture := seedAgentSessionWithRuntimeBindings(t, pool, 33, workflow.RoleDeveloper, pinnedBinding, pinnedBinding)
	prepareClosableFixture(t, pool, fixture)
	observedAt := time.Now().UTC().Add(-31 * 24 * time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], pool, ctx, fixture, 33,
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

	reopenDeliveryID := "51000000-0000-4000-8000-000000000332"
	reopenApplication, reopenErr := applyReopenApplication(databases[1], ctx, 33, reopenDeliveryID)
	if reopenErr != nil || reopenApplication.Status != store.NormalizedEventPending ||
		reopenApplication.State != workflow.StateClosed {
		t.Fatalf("reopen after authorization = (%#v, %v), want durable pending event on closed Workflow", reopenApplication, reopenErr)
	}
	var webhookStatus, normalizedStatus string
	var normalizedAttempts int
	if err := pool.QueryRow(ctx, `
SELECT delivery.status, event.status, event.attempt_count
FROM webhook_deliveries AS delivery
JOIN normalized_events AS event USING (delivery_id)
WHERE delivery.delivery_id = $1`, reopenDeliveryID).Scan(&webhookStatus, &normalizedStatus, &normalizedAttempts); err != nil {
		t.Fatal(err)
	}
	if webhookStatus != "PROCESSED" || normalizedStatus != "PENDING" || normalizedAttempts != 0 {
		t.Fatalf("deferred reopen durability = webhook %s, normalized event %s/%d", webhookStatus, normalizedStatus, normalizedAttempts)
	}
	reopenFactory := func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowEventFactory, error) {
		return store.WorkflowLocator{RepositoryID: 33, IssueID: 33, IssueNumber: 33}, func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.IssueReopenedEvent{EventMetadata: context.Metadata}, nil
		}, nil
	}
	if application, applied, err := databases[0].ApplyNextPendingNormalizedEvent(ctx, reopenFactory); err != nil || applied {
		t.Fatalf("drain reopen during collection = (%#v, %t, %v), want no eligible event", application, applied, err)
	}
	if err := pool.QueryRow(ctx, `SELECT attempt_count FROM normalized_events WHERE delivery_id = $1`, reopenDeliveryID).Scan(&normalizedAttempts); err != nil {
		t.Fatal(err)
	}
	if normalizedAttempts != 0 {
		t.Fatalf("deferred reopen attempts = %d, want zero", normalizedAttempts)
	}
	triggerDeliveryID := "51000000-0000-4000-8000-000000000334"
	triggerAttemptID := "52000000-0000-4000-8000-000000000334"
	triggerDelivery := workflowDelivery(triggerDeliveryID)
	triggerDelivery.RepositoryID, triggerDelivery.IssueID, triggerDelivery.IssueNumber = 33, 33, 33
	triggerDelivery.RepositoryOwner, triggerDelivery.RepositoryName = "owner", "repo"
	if inserted, err := databases[0].InsertWebhookDelivery(ctx, triggerDelivery); err != nil || !inserted {
		t.Fatalf("insert trigger behind deferred reopen = (%t, %v)", inserted, err)
	}
	if claim, err := databases[0].ClaimWebhookDelivery(ctx, "blocked-trigger-test", time.Minute); err != nil || claim != nil {
		t.Fatalf("claim trigger while reopen is unresolved = (%#v, %v), want no claim", claim, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1 AND attempt_number = $2`, first.ID, first.Attempt); err != nil {
		t.Fatal(err)
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

	reopened, applied, err := databases[0].ApplyNextPendingNormalizedEvent(ctx, reopenFactory)
	if err != nil || !applied || reopened.Status != store.NormalizedEventCompleted || reopened.State != workflow.StateDormant {
		t.Fatalf("apply deferred reopen = (%#v, %t, %v), want completed dormant Workflow", reopened, applied, err)
	}
	triggerClaim, err := databases[0].ClaimWebhookDelivery(ctx, "post-collection-trigger-test", time.Minute)
	if err != nil || triggerClaim == nil || triggerClaim.DeliveryID != triggerDeliveryID {
		t.Fatalf("claim trigger after deferred reopen = (%#v, %v)", triggerClaim, err)
	}
	triggerApplication, err := databases[0].CompleteWebhookTransition(ctx, triggerClaim.DeliveryID, triggerClaim.ClaimToken,
		normalizedPayload(triggerClaim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 33, IssueID: 33, IssueNumber: 33},
		func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: context.Metadata,
				AttemptID: triggerAttemptID, AttemptNumber: context.Snapshot.LastAttemptNumber + 1}, nil
		})
	if err != nil || triggerApplication.State != workflow.StateDeveloping {
		t.Fatalf("apply trigger after deferred reopen = (%#v, %v)", triggerApplication, err)
	}
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
	turnLease := acquireAndBindTurn(t, databases[0], pool, ctx, prepared, "post-collection-runtime")
	settleAcquiredTurn(t, databases[0], ctx, turnLease, store.AgentTurnSucceeded)
	secondObservedAt := time.Now().UTC()
	closeAndSettleWithoutTurn(t, databases[0], pool, ctx, fixture, 33,
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
	closeAndSettleWithoutTurn(t, databases[0], pool, ctx, fixture, 37,
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

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	retainUntil := observedAt.Add(time.Hour)
	closeAndSettleWithoutTurn(t, databases[0], pool, ctx, fixture, 36,
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
	closeAndSettleWithoutTurn(t, databases[0], pool, ctx, fixture, 34,
		"51000000-0000-4000-8000-000000000341", "closure-race", "retention-race", observedAt, observedAt.Add(30*24*time.Hour))
	lease, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "collector-race", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}

	start := make(chan struct{})
	var reopenApplication store.WorkflowApplication
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
		reopenApplication, reopenErr = applyReopenApplication(databases[1], ctx, 34, "51000000-0000-4000-8000-000000000342")
	}()
	close(start)
	wait.Wait()
	if reopenErr != nil {
		t.Fatalf("concurrent reopen error = %v", reopenErr)
	}
	if authorizationErr == nil && (reopenApplication.Status != store.NormalizedEventPending || reopenApplication.State != workflow.StateClosed) {
		t.Fatalf("authorization won but reopen application = %#v", reopenApplication)
	}
	if errors.Is(authorizationErr, store.ErrAssignmentCollectionFenceLost) &&
		(reopenApplication.Status != store.NormalizedEventCompleted || reopenApplication.State != workflow.StateDormant) {
		t.Fatalf("reopen won but application = %#v", reopenApplication)
	}
	if authorizationErr != nil && !errors.Is(authorizationErr, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("collection authorization error = %v", authorizationErr)
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

func closeAndSettleWithoutTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, fixture agentFixture, number int, deliveryID, closureID, retentionToken string, observedAt, retainUntil time.Time) store.ClosureSettlement {
	t.Helper()
	delivery := workflowDelivery(deliveryID)
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(number), int64(number), int64(number)
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET received_at = $2 WHERE delivery_id = $1`, claim.DeliveryID, observedAt); err != nil {
		t.Fatal(err)
	}
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)},
		func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.IssueClosedEvent{EventMetadata: context.Metadata,
				ClosureID: closureID, RetainUntil: retainUntil, RetentionToken: retentionToken}, nil
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

func applyReopen(t *testing.T, database *store.Store, ctx context.Context, number int, deliveryID string) {
	t.Helper()
	if err := applyReopenError(database, ctx, number, deliveryID); err != nil {
		t.Fatalf("reopen Workflow: %v", err)
	}
}

func applyReopenError(database *store.Store, ctx context.Context, number int, deliveryID string) error {
	_, err := applyReopenApplication(database, ctx, number, deliveryID)
	return err
}

func applyReopenApplication(database *store.Store, ctx context.Context, number int, deliveryID string) (store.WorkflowApplication, error) {
	delivery := workflowDelivery(deliveryID)
	delivery.EventName, delivery.Action = "issues", "reopened"
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(number), int64(number), int64(number)
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	if _, err := database.InsertWebhookDelivery(ctx, delivery); err != nil {
		return store.WorkflowApplication{}, err
	}
	claim, err := database.ClaimWebhookDelivery(ctx, "reopen-test", time.Minute)
	if err != nil || claim == nil {
		return store.WorkflowApplication{}, fmt.Errorf("claim reopen delivery: %w", err)
	}
	return database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "reopened"),
		store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)},
		func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.IssueReopenedEvent{EventMetadata: context.Metadata}, nil
		})
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
		func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: context.Metadata,
				AttemptID: attemptID, AttemptNumber: context.Snapshot.LastAttemptNumber + 1}, nil
		})
	if err != nil || application.State != workflow.StateDeveloping {
		t.Fatalf("trigger reopened Workflow = (%#v, %v)", application, err)
	}
}
