//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestPendingEventReconciliationConsumesExactlyLinkedRowsOnce(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', desired_assignment_status = 'ACTIVE',
    desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("prepare reconciliation Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("prepare reconciliation Workflow Attempt: %v", err)
	}
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	executionJob := claimAgentTurnJob(t, databases[0], ctx, turn, 10*time.Second)
	turnLease, err := databases[0].AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "runtime", 10*time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}

	deferredIDs := []string{
		"41000000-0000-4000-8000-000000000002",
		"41000000-0000-4000-8000-000000000001",
	}
	for _, deliveryID := range deferredIDs {
		delivery := workflowDelivery(deliveryID)
		delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 20, 20, 20
		delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
		claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
		application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
			normalizedPayload(claim.DeliveryID, "corroboration"),
			store.WorkflowLocator{RepositoryID: 20, IssueID: 20, IssueNumber: 20},
			func(snapshot workflow.Snapshot) workflow.Decision {
				return workflow.Reduce(snapshot, workflow.ChangeProposalObservedEvent{
					EventMetadata: workflow.EventMetadata{
						ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt,
						WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
					},
					ChangeProposal: workflow.ChangeProposal{ID: 200, Number: 20, HeadSHA: "same-head", Open: true},
				})
			})
		if err != nil || application.Status != store.NormalizedEventDeferred {
			t.Fatalf("defer corroborating event = (%#v, %v)", application, err)
		}
	}

	settlementID := "41000000-0000-4000-8000-000000000003"
	delivery := workflowDelivery(settlementID)
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 20, 20, 20
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "turn-settled"),
		store.WorkflowLocator{RepositoryID: 20, IssueID: 20, IssueNumber: 20},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.TurnSettledEvent{
				EventMetadata: workflow.EventMetadata{
					ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt,
					WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
				},
				Turn: workflow.TurnGuard{
					TurnID: turn.ID, SessionID: turn.AgentSessionID, AttemptID: turn.WorkflowAttemptID,
					Role: workflow.RoleDeveloper, Epoch: uint64(turn.ExecutionEpoch), ControlRevision: uint64(turn.ControlRevision),
				},
				Outcome: workflow.TurnOutcomeInfrastructureFailed,
				PendingEvents: workflow.PendingEventsObservation{
					Count: 2, LatestObservedHeadSHA: "same-head",
				},
			})
		})
	if err != nil || application.Disposition != workflow.DispositionApplied {
		t.Fatalf("settle turn with pending events = (%#v, %v)", application, err)
	}
	if err := databases[0].CloseMutationAdmission(ctx, turnLease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	if err := databases[0].FinalizeAgentTurn(ctx, turnLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed}); err != nil {
		t.Fatalf("FinalizeAgentTurn() error = %v", err)
	}

	job, err := databases[0].ClaimJob(ctx, store.WorkflowActionQueue, "reconciliation-worker", 10*time.Second)
	if err != nil || job == nil || job.Kind != store.ReconcilePendingEventsJobKind {
		t.Fatalf("ClaimJob() reconciliation = (%#v, %v)", job, err)
	}
	if err := databases[0].CompleteJob(ctx, *job, json.RawMessage(`{}`)); !errors.Is(err, store.ErrWorkflowJobRequiresAcknowledgement) {
		t.Errorf("generic CompleteJob() error = %v, want ErrWorkflowJobRequiresAcknowledgement", err)
	}
	factory := func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		return store.WorkflowLocator{RepositoryID: 20, IssueID: 20, IssueNumber: 20}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Decision{
				Snapshot: snapshot, Disposition: workflow.DispositionUnrelated,
				Reason: workflow.ReasonCorroborationWithoutActiveTurn,
			}
		}, nil
	}
	stale := *job
	stale.LeaseOwner = "stale-worker"
	if _, err := databases[0].AcknowledgePendingEventReconciliation(ctx, stale, factory); !errors.Is(err, store.ErrPendingEventReconciliationFenceLost) {
		t.Errorf("stale reconciliation acknowledgement error = %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM job_normalized_events WHERE normalized_event_id = $1`, deferredIDs[0]); err != nil {
		t.Fatalf("remove reconciliation link: %v", err)
	}
	if _, err := databases[0].AcknowledgePendingEventReconciliation(ctx, *job, factory); !errors.Is(err, store.ErrPendingEventReconciliationFenceLost) {
		t.Errorf("acknowledgement with missing exact link error = %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO job_normalized_events (job_id, normalized_event_id) VALUES ($1, $2)`, job.ID, deferredIDs[0]); err != nil {
		t.Fatalf("restore reconciliation link: %v", err)
	}

	acknowledged, err := databases[0].AcknowledgePendingEventReconciliation(ctx, *job, factory)
	if err != nil {
		t.Fatalf("AcknowledgePendingEventReconciliation() error = %v", err)
	}
	if acknowledged.CompletedCount != 2 || acknowledged.SourceTurnID != turn.ID ||
		acknowledged.Successor.Role != workflow.RoleDeveloper ||
		acknowledged.Successor.Purpose != workflow.TurnPurposeRetry ||
		acknowledged.Successor.RetryOfTurnID != turn.ID || acknowledged.SuccessorJobID == "" {
		t.Errorf("reconciliation acknowledgement = %#v", acknowledged)
	}
	if _, err := databases[0].AcknowledgePendingEventReconciliation(ctx, *job, factory); !errors.Is(err, store.ErrPendingEventReconciliationFenceLost) {
		t.Errorf("duplicate reconciliation acknowledgement error = %v", err)
	}
	var completed, successorJobs int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'COMPLETED' AND disposition = 'UNRELATED'
		AND reason = 'corroboration_without_active_turn'),
       (SELECT count(*) FROM jobs WHERE workflow_id = $2 AND kind = 'PREPARE_AGENT_TURN')
FROM normalized_events WHERE delivery_id = ANY($1)`, deferredIDs, fixture.workflowID).Scan(&completed, &successorJobs); err != nil {
		t.Fatalf("query reconciliation outcome: %v", err)
	}
	if completed != 2 || successorJobs != 1 {
		t.Errorf("reconciliation outcome = %d completed rows, %d successor jobs; want 2 and 1", completed, successorJobs)
	}
}

func TestPendingEventReconciliationReplaysSynchronizationAndSupersedesFallback(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 22)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proposalRowID := "62000000-0000-4000-8000-000000000022"
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'REVIEWING', desired_assignment_status = 'ACTIVE',
    desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("prepare reviewing Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("prepare reviewing Workflow Attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET role = 'REVIEWER', agent_profile_name = 'reviewer' WHERE id = $1`, fixture.assignmentID); err != nil {
		t.Fatalf("prepare Reviewer Assignment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ($1, $2, 22, 'owner', 'repo', 220, 22, 'OPEN', 'main', 'base', 'feature', 'head-old')`,
		proposalRowID, fixture.workflowID); err != nil {
		t.Fatalf("seed synchronized Change Proposal: %v", err)
	}
	turnSpec := fixture.turnSpec()
	turnSpec.Purpose = workflow.TurnPurposeReview
	turnSpec.ChangeProposalID = proposalRowID
	turnSpec.ExpectedHeadSHA = "head-old"
	turnSpec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10, "Review test instructions.", nil)
	turn, err := databases[0].AllocateAgentTurn(ctx, turnSpec)
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	executionJob := claimAgentTurnJob(t, databases[0], ctx, turn, 10*time.Second)
	turnLease, err := databases[0].AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "review-runtime", 10*time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}

	deferredID := "43000000-0000-4000-8000-000000000001"
	syncPayload, err := json.Marshal(map[string]string{
		"delivery_id": deferredID, "before": "head-old", "head": "head-new",
	})
	if err != nil {
		t.Fatalf("encode synchronization payload: %v", err)
	}
	delivery := workflowDelivery(deferredID)
	delivery.EventName, delivery.Action = "pull_request", "synchronize"
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 22, "owner", "repo"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		syncPayload, store.WorkflowLocator{RepositoryID: 22, PullRequestID: 220},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.SynchronizationEvent{
				EventMetadata: workflow.EventMetadata{
					ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt,
					WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
				},
				ChangeProposalID: 220, PreviousHeadSHA: "head-old", HeadSHA: "head-new",
			})
		})
	if err != nil || application.Status != store.NormalizedEventDeferred {
		t.Fatalf("defer synchronization = (%#v, %v)", application, err)
	}

	settlementID := "43000000-0000-4000-8000-000000000002"
	delivery = workflowDelivery(settlementID)
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 22, "owner", "repo"
	delivery.IssueID, delivery.IssueNumber = 22, 22
	claim = claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err = databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "turn-settled"),
		store.WorkflowLocator{RepositoryID: 22, IssueID: 22, IssueNumber: 22},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.TurnSettledEvent{
				EventMetadata: workflow.EventMetadata{
					ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt,
					WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
				},
				Turn: workflow.TurnGuard{
					TurnID: turn.ID, SessionID: turn.AgentSessionID, AttemptID: turn.WorkflowAttemptID,
					Role: workflow.RoleReviewer, Epoch: uint64(turn.ExecutionEpoch), ControlRevision: uint64(turn.ControlRevision),
					ChangeProposalID: 220, ExpectedHeadSHA: "head-old",
				},
				Outcome: workflow.TurnOutcomeInfrastructureFailed,
				PendingEvents: workflow.PendingEventsObservation{
					Count: 1, RequiresReconciliation: true, LatestObservedHeadSHA: "head-new",
				},
			})
		})
	if err != nil || application.Disposition != workflow.DispositionApplied {
		t.Fatalf("settle Reviewer turn = (%#v, %v)", application, err)
	}
	if err := databases[0].CloseMutationAdmission(ctx, turnLease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	if err := databases[0].FinalizeAgentTurn(ctx, turnLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed}); err != nil {
		t.Fatalf("FinalizeAgentTurn() error = %v", err)
	}

	job, err := databases[0].ClaimJob(ctx, store.WorkflowActionQueue, "synchronization-reconciler", 10*time.Second)
	if err != nil || job == nil || job.Kind != store.ReconcilePendingEventsJobKind {
		t.Fatalf("ClaimJob() reconciliation = (%#v, %v)", job, err)
	}
	factory := func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		var payload struct {
			DeliveryID string `json:"delivery_id"`
			Before     string `json:"before"`
			Head       string `json:"head"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return store.WorkflowLocator{}, nil, err
		}
		return store.WorkflowLocator{RepositoryID: 22, PullRequestID: 220}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.SynchronizationEvent{
				EventMetadata: workflow.EventMetadata{
					ID: payload.DeliveryID, ObservedAt: record.CreatedAt,
					WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
				},
				ChangeProposalID: 220, PreviousHeadSHA: payload.Before, HeadSHA: payload.Head,
			})
		}, nil
	}
	acknowledged, err := databases[0].AcknowledgePendingEventReconciliation(ctx, *job, factory)
	if err != nil {
		t.Fatalf("AcknowledgePendingEventReconciliation() error = %v", err)
	}
	if acknowledged.CompletedCount != 1 || acknowledged.SuccessorJobID == "" ||
		acknowledged.Successor.Role != workflow.RoleReviewer ||
		acknowledged.Successor.Purpose != workflow.TurnPurposeSynchronization ||
		acknowledged.Successor.ExpectedHeadSHA != "head-new" || acknowledged.Successor.RetryOfTurnID != "" {
		t.Errorf("authoritative reconciliation = %#v", acknowledged)
	}
	var head, eventStatus, disposition, reason, purpose, expectedHead, retryOf string
	var workflowRevision, eventRevision, successorCount int64
	if err := pool.QueryRow(ctx, `SELECT head_sha FROM change_proposals WHERE id = $1`, proposalRowID).Scan(&head); err != nil {
		t.Fatalf("query reconciled Change Proposal: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, disposition, reason, applied_revision FROM normalized_events WHERE delivery_id = $1`, deferredID).Scan(&eventStatus, &disposition, &reason, &eventRevision); err != nil {
		t.Fatalf("query reconciled synchronization: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state_revision FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowRevision); err != nil {
		t.Fatalf("query reconciled Workflow revision: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*), min(payload->>'purpose'), min(payload->>'expected_head_sha'),
       min(payload->>'retry_of_turn_id')
FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'
  AND status IN ('AVAILABLE', 'LEASED')`, fixture.workflowID).Scan(&successorCount, &purpose, &expectedHead, &retryOf); err != nil {
		t.Fatalf("query coalesced successor: %v", err)
	}
	if head != "head-new" || eventStatus != "COMPLETED" || disposition != "APPLIED" ||
		reason != string(workflow.ReasonReviewHeadReplaced) || eventRevision != workflowRevision ||
		successorCount != 1 || purpose != string(workflow.TurnPurposeSynchronization) ||
		expectedHead != "head-new" || retryOf != "" {
		t.Errorf("replayed synchronization = head %q, event %s/%s/%s@%d, Workflow @%d, successor %d %s/%q retry %q",
			head, eventStatus, disposition, reason, eventRevision, workflowRevision,
			successorCount, purpose, expectedHead, retryOf)
	}
}

func TestPendingEventReconciliationRejectsInterveningRevisionAndSuccessor(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 23)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', desired_assignment_status = 'ACTIVE',
    desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("prepare delayed reconciliation Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("prepare delayed reconciliation Workflow Attempt: %v", err)
	}
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_turns
SET active = FALSE, status = 'FAILED', mutation_admission_open = FALSE,
    mutation_admission_closed_at = clock_timestamp(), completed_at = clock_timestamp()
WHERE id = $1`, turn.ID); err != nil {
		t.Fatalf("settle delayed reconciliation source turn: %v", err)
	}

	deferredID := "44000000-0000-4000-8000-000000000001"
	originID := "44000000-0000-4000-8000-000000000002"
	for _, deliveryID := range []string{deferredID, originID} {
		delivery := workflowDelivery(deliveryID)
		delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 23, 23, 23
		delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
		if inserted, err := databases[0].InsertWebhookDelivery(ctx, delivery); err != nil || !inserted {
			t.Fatalf("InsertWebhookDelivery(%s) = (%t, %v)", deliveryID, inserted, err)
		}
		if _, err := pool.Exec(ctx, `
UPDATE webhook_deliveries SET status = 'PROCESSED', processed_at = clock_timestamp()
WHERE delivery_id = $1`, deliveryID); err != nil {
			t.Fatalf("complete seeded delivery %s: %v", deliveryID, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO normalized_events (
    delivery_id, payload, status, workflow_id, disposition, reason,
    applied_revision, deferred_for_turn_id, processed_at
)
VALUES ($1, $2, 'DEFERRED', $3, 'DEFERRED', 'active_turn', 1, $4, clock_timestamp()),
       ($5, $6, 'COMPLETED', $3, 'APPLIED', 'infrastructure_retry', 1, NULL, clock_timestamp())`,
		deferredID, normalizedPayload(deferredID, "corroboration"), fixture.workflowID, turn.ID,
		originID, normalizedPayload(originID, "turn-settled")); err != nil {
		t.Fatalf("seed delayed normalized events: %v", err)
	}
	reconciliationJobID := "44000000-0000-4000-8000-000000000003"
	payload, err := json.Marshal(map[string]any{
		"workflow_id": fixture.workflowID, "workflow_attempt_id": fixture.attemptID,
		"count": 1, "latest_observed_head_sha": "", "fallback_role": workflow.RoleDeveloper,
		"fallback_purpose": workflow.TurnPurposeRetry, "fallback_expected_head_sha": "",
		"retry_of_turn_id": turn.ID, "revision": 1, "source_turn_id": turn.ID,
		"source_execution_epoch": turn.ExecutionEpoch, "source_control_revision": turn.ControlRevision,
		"deferred_normalized_event_ids": []string{deferredID},
	})
	if err != nil {
		t.Fatalf("encode delayed reconciliation job: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, max_attempts, idempotency_key, workflow_id,
    workflow_attempt_id, agent_assignment_id, agent_session_id, agent_turn_id,
    execution_epoch, normalized_event_id, action_key
)
VALUES ($1, 'workflow', 'RECONCILE_PENDING_EVENTS', $2, 3, $3, $4,
		$5, $6, $7, $8, $9, $10, 'reconcile-pending-events')`,
		reconciliationJobID, payload, "delayed-reconciliation", fixture.workflowID,
		fixture.attemptID, fixture.assignmentID, fixture.sessionID, turn.ID,
		turn.ExecutionEpoch, originID); err != nil {
		t.Fatalf("seed delayed reconciliation job: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO job_normalized_events (job_id, normalized_event_id) VALUES ($1, $2)`, reconciliationJobID, deferredID); err != nil {
		t.Fatalf("link delayed reconciliation event: %v", err)
	}
	job, err := databases[0].ClaimJob(ctx, store.WorkflowActionQueue, "delayed-reconciler", 10*time.Second)
	if err != nil || job == nil || job.ID != reconciliationJobID {
		t.Fatalf("ClaimJob() delayed reconciliation = (%#v, %v)", job, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflows SET state_revision = 2 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("advance Workflow revision: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, max_attempts, idempotency_key, workflow_id, workflow_attempt_id
)
VALUES ('44000000-0000-4000-8000-000000000004', 'workflow', 'PREPARE_AGENT_TURN',
        '{"mode":"CURRENT","role":"DEVELOPER","purpose":"RETRY","revision":2}'::jsonb,
        3, 'intervening-successor', $1, $2)`, fixture.workflowID, fixture.attemptID); err != nil {
		t.Fatalf("seed intervening successor: %v", err)
	}
	factoryCalled := false
	factory := func(store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		factoryCalled = true
		return store.WorkflowLocator{}, nil, errors.New("must not replay across revision fence")
	}
	if _, err := databases[0].AcknowledgePendingEventReconciliation(ctx, *job, factory); !errors.Is(err, store.ErrPendingEventReconciliationFenceLost) {
		t.Fatalf("delayed reconciliation error = %v, want ErrPendingEventReconciliationFenceLost", err)
	}
	if factoryCalled {
		t.Error("transition factory called after revision fence was lost")
	}
	var deferredStatus, reconciliationStatus string
	var successorCount int
	if err := pool.QueryRow(ctx, `SELECT status FROM normalized_events WHERE delivery_id = $1`, deferredID).Scan(&deferredStatus); err != nil {
		t.Fatalf("query delayed deferred event: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, reconciliationJobID).Scan(&reconciliationStatus); err != nil {
		t.Fatalf("query delayed reconciliation job: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM jobs
WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN' AND status IN ('AVAILABLE', 'LEASED')`, fixture.workflowID).Scan(&successorCount); err != nil {
		t.Fatalf("count successors after delayed reconciliation: %v", err)
	}
	if deferredStatus != "DEFERRED" || reconciliationStatus != "LEASED" || successorCount != 1 {
		t.Errorf("delayed reconciliation durability = event %s, job %s, successors %d; want DEFERRED, LEASED, 1",
			deferredStatus, reconciliationStatus, successorCount)
	}
}

func TestPendingEventReconciliationTreatsUnsettledTurnRecoveryAsLiveSuccessor(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 45)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', state_revision = 1,
    desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE'
WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	executionJob := claimAgentTurnJob(t, database, ctx, turn, 5*time.Second)
	lease, err := database.AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "runtime-pending-recovery", 5*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "pending-event-recovery", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("unknown")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginAgentTurnMutationRecovery(ctx, lease); err != nil {
		t.Fatal(err)
	}

	reconciliationJobID := "44000000-0000-4000-8000-000000000045"
	payload, err := json.Marshal(map[string]any{
		"workflow_id": fixture.workflowID, "workflow_attempt_id": fixture.attemptID,
		"count": 1, "latest_observed_head_sha": "", "fallback_role": workflow.RoleDeveloper,
		"fallback_purpose": workflow.TurnPurposeRetry, "fallback_expected_head_sha": "",
		"retry_of_turn_id": turn.ID, "revision": 1, "source_turn_id": turn.ID,
		"source_execution_epoch": turn.ExecutionEpoch, "source_control_revision": turn.ControlRevision,
		"deferred_normalized_event_ids": []string{"44000000-0000-4000-8000-000000000046"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, max_attempts, idempotency_key, workflow_id,
    workflow_attempt_id, agent_assignment_id, agent_session_id, agent_turn_id, execution_epoch
)
VALUES ($1, $2, $3, $4, 3, 'pending-event-unsettled-recovery', $5, $6, $7, $8, $9, $10)`,
		reconciliationJobID, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, payload,
		fixture.workflowID, fixture.attemptID, fixture.assignmentID, fixture.sessionID, turn.ID, turn.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	reconciliationJob, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "pending-reconciler", 5*time.Second)
	if err != nil || reconciliationJob == nil {
		t.Fatalf("ClaimJobKind() = (%#v, %v)", reconciliationJob, err)
	}
	factoryCalled := false
	if _, err := database.AcknowledgePendingEventReconciliation(ctx, *reconciliationJob, func(store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		factoryCalled = true
		return store.WorkflowLocator{}, nil, errors.New("must not replay while recovery is unsettled")
	}); !errors.Is(err, store.ErrWorkflowSuccessorConflict) {
		t.Fatalf("AcknowledgePendingEventReconciliation() error = %v, want ErrWorkflowSuccessorConflict", err)
	}
	if factoryCalled {
		t.Error("transition factory called while recovery barrier was unsettled")
	}
	var status string
	var successors int
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, reconciliationJobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, fixture.workflowID).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if status != string(store.JobLeased) || successors != 0 {
		t.Errorf("blocked reconciliation left job=%s successors=%d; want LEASED and 0", status, successors)
	}
}

func TestClosureSettlementWaitsForStopAndMutationAcknowledgements(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 21)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', desired_assignment_status = 'ACTIVE',
    desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("prepare closure Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("prepare closure Workflow Attempt: %v", err)
	}
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	executionJob := claimAgentTurnJob(t, databases[0], ctx, turn, 10*time.Second)
	turnLease, err := databases[0].AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "runtime", 10*time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, turnLease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	mutation, err := databases[0].ReserveMutation(ctx, turnLease, store.MutationSpec{
		OperationID: "github:closure:ambiguous", ToolName: "close_sensitive_mutation", Request: json.RawMessage(`{"value":1}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() error = %v", err)
	}
	if _, err := databases[0].StartMutation(ctx, turnLease, mutation.ID); err != nil {
		t.Fatalf("StartMutation() error = %v", err)
	}

	delivery := workflowDelivery("42000000-0000-4000-8000-000000000001")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 21, 21, 21
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	closedAt := time.Now().UTC()
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: 21, IssueID: 21, IssueNumber: 21},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{
				EventMetadata: workflow.EventMetadata{
					ID: claim.DeliveryID, ObservedAt: closedAt,
					WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
				},
				ClosureID: "closure-21", RetainUntil: closedAt.Add(24 * time.Hour), RetentionToken: "retention-21",
			})
		})
	if err != nil || application.State != workflow.StateClosing {
		t.Fatalf("close Workflow = (%#v, %v)", application, err)
	}
	prematureDelivery := workflowDelivery("42000000-0000-4000-8000-000000000002")
	prematureDelivery.RepositoryID, prematureDelivery.IssueID, prematureDelivery.IssueNumber = 21, 21, 21
	prematureDelivery.RepositoryOwner, prematureDelivery.RepositoryName = "owner", "repo"
	prematureClaim := claimWorkflowDelivery(t, databases[0], ctx, prematureDelivery)
	_, err = databases[0].CompleteWebhookTransition(ctx, prematureClaim.DeliveryID, prematureClaim.ClaimToken,
		normalizedPayload(prematureClaim.DeliveryID, "closure-settled"),
		store.WorkflowLocator{RepositoryID: 21, IssueID: 21, IssueNumber: 21},
		func(snapshot workflow.Snapshot) workflow.Decision {
			guard := workflow.TurnGuard{
				TurnID: turn.ID, SessionID: turn.AgentSessionID, AttemptID: turn.WorkflowAttemptID,
				Role: workflow.RoleDeveloper, Epoch: uint64(turn.ExecutionEpoch), ControlRevision: uint64(turn.ControlRevision),
			}
			return workflow.Reduce(snapshot, workflow.ClosureSettledEvent{
				EventMetadata: workflow.EventMetadata{
					ID: prematureClaim.DeliveryID, ObservedAt: prematureClaim.ReceivedAt,
					WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
				},
				ClosureID: "closure-21", Turn: &guard,
			})
		})
	if !errors.Is(err, store.ErrClosureSettlementUnsettled) {
		t.Fatalf("premature ClosureSettledEvent error = %v, want ErrClosureSettlementUnsettled", err)
	}
	stopJob, err := databases[0].ClaimJob(ctx, store.WorkflowActionQueue, "stop-worker", 10*time.Second)
	if err != nil || stopJob == nil || stopJob.Kind != store.StopAgentTurnJobKind {
		t.Fatalf("ClaimJob() stop = (%#v, %v)", stopJob, err)
	}
	settlementJob, err := databases[0].ClaimJob(ctx, store.WorkflowActionQueue, "settlement-worker", 10*time.Second)
	if err != nil || settlementJob == nil || settlementJob.Kind != store.SettleClosureJobKind {
		t.Fatalf("ClaimJob() settlement = (%#v, %v)", settlementJob, err)
	}
	if err := databases[0].CompleteJob(ctx, *stopJob, json.RawMessage(`{}`)); !errors.Is(err, store.ErrWorkflowJobRequiresAcknowledgement) {
		t.Errorf("generic stop completion error = %v", err)
	}
	if err := databases[0].CompleteJob(ctx, *settlementJob, json.RawMessage(`{}`)); !errors.Is(err, store.ErrWorkflowJobRequiresAcknowledgement) {
		t.Errorf("generic settlement completion error = %v", err)
	}
	if _, err := databases[0].CompleteClosureSettlement(ctx, *settlementJob); !errors.Is(err, store.ErrClosureSettlementUnsettled) {
		t.Errorf("settlement before stop acknowledgement error = %v", err)
	}
	if _, err := databases[0].ReconcileClosureMutation(ctx, *settlementJob, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"ok":true}`),
	}); !errors.Is(err, store.ErrClosureSettlementUnsettled) {
		t.Errorf("mutation reconciliation before stop acknowledgement error = %v", err)
	}
	staleStop := *stopJob
	staleStop.ExecutionEpoch++
	if _, err := databases[0].AcknowledgeClosureTurnStopped(ctx, staleStop); !errors.Is(err, store.ErrClosureSettlementFenceLost) {
		t.Errorf("stale stop acknowledgement error = %v", err)
	}
	if _, err := databases[0].AcknowledgeClosureTurnStopped(ctx, *stopJob); err != nil {
		t.Fatalf("AcknowledgeClosureTurnStopped() error = %v", err)
	}
	if _, err := databases[0].CompleteClosureSettlement(ctx, *settlementJob); !errors.Is(err, store.ErrClosureSettlementUnsettled) {
		t.Errorf("settlement with ambiguous mutation error = %v", err)
	}
	staleSettlement := *settlementJob
	staleSettlement.LeaseOwner = "stale-settlement-worker"
	if _, err := databases[0].ReconcileClosureMutation(ctx, staleSettlement, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationFailed, LastError: "escalated",
	}); !errors.Is(err, store.ErrClosureSettlementFenceLost) {
		t.Errorf("stale mutation reconciliation error = %v", err)
	}
	if _, err := databases[0].ReconcileClosureMutation(ctx, *settlementJob, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationFailed, LastError: "external outcome could not be confirmed; escalated",
	}); err != nil {
		t.Fatalf("ReconcileClosureMutation() error = %v", err)
	}
	settled, err := databases[0].CompleteClosureSettlement(ctx, *settlementJob)
	if err != nil {
		t.Fatalf("CompleteClosureSettlement() error = %v", err)
	}
	if settled.SettledAt == nil || settled.RuntimeStoppedAt == nil || settled.SourceTurnID != turn.ID {
		t.Errorf("closure settlement = %#v", settled)
	}
	if _, err := databases[0].CompleteClosureSettlement(ctx, *settlementJob); !errors.Is(err, store.ErrClosureSettlementFenceLost) {
		t.Errorf("duplicate closure settlement error = %v", err)
	}
	var turnStatus, executionStatus, settlementStatus, assignmentStatus string
	var active bool
	var completionJobs int
	if err := pool.QueryRow(ctx, `SELECT status, active FROM agent_turns WHERE id = $1`, turn.ID).Scan(&turnStatus, &active); err != nil {
		t.Fatalf("query settled closure turn: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, executionJob.ID).Scan(&executionStatus); err != nil {
		t.Fatalf("query cancelled execution job: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, settlementJob.ID).Scan(&settlementStatus); err != nil {
		t.Fatalf("query completed settlement job: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&assignmentStatus); err != nil {
		t.Fatalf("query assignment before ClosureSettledEvent: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind IN ('COMPLETE_ASSIGNMENTS', 'COLLECT_ASSIGNMENTS')`, fixture.workflowID).Scan(&completionJobs); err != nil {
		t.Fatalf("count premature closure jobs: %v", err)
	}
	if turnStatus != "INTERRUPTED" || active || executionStatus != "CANCELLED" || settlementStatus != "SUCCEEDED" || assignmentStatus != "ACTIVE" || completionJobs != 0 {
		t.Errorf("closure durability = turn %s/%t, execution %s, settlement %s, assignment %s, completion jobs %d",
			turnStatus, active, executionStatus, settlementStatus, assignmentStatus, completionJobs)
	}

	settledDelivery := workflowDelivery("42000000-0000-4000-8000-000000000003")
	settledDelivery.RepositoryID, settledDelivery.IssueID, settledDelivery.IssueNumber = 21, 21, 21
	settledDelivery.RepositoryOwner, settledDelivery.RepositoryName = "owner", "repo"
	settledClaim := claimWorkflowDelivery(t, databases[0], ctx, settledDelivery)
	application, err = databases[0].CompleteWebhookTransition(ctx, settledClaim.DeliveryID, settledClaim.ClaimToken,
		normalizedPayload(settledClaim.DeliveryID, "closure-settled"),
		store.WorkflowLocator{RepositoryID: 21, IssueID: 21, IssueNumber: 21},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.ClosureSettledEvent{
				EventMetadata: workflow.EventMetadata{
					ID: settledClaim.DeliveryID, ObservedAt: settledClaim.ReceivedAt,
					WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
				},
				ClosureID: "closure-21",
			})
		})
	if err != nil || application.State != workflow.StateClosed {
		t.Fatalf("ClosureSettledEvent after barrier = (%#v, %v)", application, err)
	}
	var desiredAssignmentStatus string
	if err := pool.QueryRow(ctx, `SELECT desired_assignment_status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&desiredAssignmentStatus); err != nil {
		t.Fatalf("query settled closure Workflow: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind IN ('COMPLETE_ASSIGNMENTS', 'COLLECT_ASSIGNMENTS')`, fixture.workflowID).Scan(&completionJobs); err != nil {
		t.Fatalf("count permitted closure jobs: %v", err)
	}
	if desiredAssignmentStatus != "COMPLETED" || completionJobs != 2 {
		t.Errorf("settled closure work = desired assignment %s, jobs %d; want COMPLETED and 2", desiredAssignmentStatus, completionJobs)
	}
}
