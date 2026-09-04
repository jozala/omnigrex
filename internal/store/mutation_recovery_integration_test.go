//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestAgentTurnMutationReconciliationWorkerSeams(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 21)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
UPDATE workflows
SET status = 'DEVELOPING', state_revision = 7,
    desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE'
WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("prepare Workflow aggregate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE workflow_attempts
SET infrastructure_failures = 0, infrastructure_failure_limit = 1
WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("prepare Workflow Attempt budget: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path,
    github_app_actor_id
)
VALUES ('40000000-0000-4000-8000-000000000219', $1, 'REVIEWER', 'ACTIVE',
        'reviewer', 'runtime', '1', 'sha256:test', '/state/reviewer-21', 7001)`, fixture.workflowID); err != nil {
		t.Fatalf("seed Reviewer Assignment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ('40000000-0000-4000-8000-000000000218', $1, 21, 'owner', 'repo',
        8121, 121, 'OPEN', 'main', 'base-sha', 'feature', 'head-sha')`, fixture.workflowID); err != nil {
		t.Fatalf("seed Turn-bound Change Proposal: %v", err)
	}

	turnSpec := fixture.turnSpec()
	turnSpec.ChangeProposalID = "40000000-0000-4000-8000-000000000218"
	turnSpec.ExpectedHeadSHA = "head-sha"
	turn, err := database.AllocateAgentTurn(ctx, turnSpec)
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	executionJob := claimAgentTurnJob(t, database, ctx, turn, 80*time.Millisecond)
	turnLease, err := database.AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "runtime", 80*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := database.OpenMutationAdmission(ctx, turnLease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	unknown, err := database.ReserveMutation(ctx, turnLease, store.MutationSpec{
		OperationID: "github:pull-request:create:recovery-context", ToolName: "create_pull_request",
		Request: json.RawMessage(`{"head":"feature"}`), ExternalService: "github",
	})
	if err != nil {
		t.Fatalf("ReserveMutation() UNKNOWN candidate error = %v", err)
	}
	if _, err := database.StartMutation(ctx, turnLease, unknown.ID); err != nil {
		t.Fatalf("StartMutation() UNKNOWN candidate error = %v", err)
	}
	reconciling, err := database.ReserveMutation(ctx, turnLease, store.MutationSpec{
		OperationID: "github:issue-comment:recovery-context", ToolName: "comment_on_issue",
		Request: json.RawMessage(`{"body":"status"}`), ExternalService: "github",
	})
	if err != nil {
		t.Fatalf("ReserveMutation() RECONCILING candidate error = %v", err)
	}
	if _, err := database.StartMutation(ctx, turnLease, reconciling.ID); err != nil {
		t.Fatalf("StartMutation() RECONCILING candidate error = %v", err)
	}
	if err := database.MarkMutationUnknown(ctx, turnLease, reconciling.ID, errors.New("request timed out")); err != nil {
		t.Fatalf("MarkMutationUnknown() error = %v", err)
	}
	if err := database.BeginMutationReconciliation(ctx, turnLease, reconciling.ID); err != nil {
		t.Fatalf("BeginMutationReconciliation() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE change_proposals
SET active = FALSE, head_sha = 'advanced-head-sha', updated_at = clock_timestamp()
WHERE id = '40000000-0000-4000-8000-000000000218'`); err != nil {
		t.Fatalf("advance Turn-bound Change Proposal: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ('40000000-0000-4000-8000-000000000217', $1, 21, 'owner', 'repo',
        8122, 122, 'OPEN', 'main', 'new-base-sha', 'new-feature', 'new-head-sha')`, fixture.workflowID); err != nil {
		t.Fatalf("seed newer active Change Proposal: %v", err)
	}

	time.Sleep(110 * time.Millisecond)
	recovery, err := database.RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("RecoverExpiredAgentTurn() error = %v", err)
	}
	stopLease, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "stop-worker", time.Second)
	if err != nil || stopLease == nil {
		t.Fatalf("ClaimJobKind() stop = (%#v, %v), want lease", stopLease, err)
	}
	reconcileLease, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.ReconcileAgentTurnMutationsJobKind, "reconcile-worker", time.Second)
	if err != nil || reconcileLease == nil {
		t.Fatalf("ClaimJobKind() reconciliation = (%#v, %v), want lease", reconcileLease, err)
	}

	if _, err := database.GetAgentTurnMutationReconciliationContext(ctx, *reconcileLease); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("GetAgentTurnMutationReconciliationContext() before stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	if _, err := database.ListAgentTurnMutationsForReconciliation(ctx, *reconcileLease); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("ListAgentTurnMutationsForReconciliation() before stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	stale := *reconcileLease
	stale.LeaseToken = "30000000-0000-4000-8000-000000000097"
	if _, err := database.GetAgentTurnMutationReconciliationContext(ctx, stale); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("GetAgentTurnMutationReconciliationContext() stale lease error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, *stopLease); err != nil {
		t.Fatalf("AcknowledgeRecoveredRuntimeStopped() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE change_proposals SET repository_id = 999 WHERE id = '40000000-0000-4000-8000-000000000218'`); err != nil {
		t.Fatalf("mismatch Turn-bound Change Proposal repository: %v", err)
	}
	if _, err := database.GetAgentTurnMutationReconciliationContext(ctx, *reconcileLease); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("GetAgentTurnMutationReconciliationContext() mismatched repository error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE change_proposals SET repository_id = 21 WHERE id = '40000000-0000-4000-8000-000000000218'`); err != nil {
		t.Fatalf("restore Turn-bound Change Proposal repository: %v", err)
	}

	reconciliationContext, err := database.GetAgentTurnMutationReconciliationContext(ctx, *reconcileLease)
	if err != nil {
		t.Fatalf("GetAgentTurnMutationReconciliationContext() error = %v", err)
	}
	if reconciliationContext.WorkflowID != fixture.workflowID ||
		reconciliationContext.Repository != (store.AgentTurnRepository{ID: 21, Owner: "owner", Name: "repo"}) ||
		reconciliationContext.Issue != (store.AgentTurnIssue{ID: 21, Number: 21}) ||
		reconciliationContext.Role != workflow.RoleDeveloper || reconciliationContext.ReviewerActorID != 7001 ||
		reconciliationContext.Turn.ID != turn.ID || reconciliationContext.Turn.Purpose != workflow.TurnPurposeInitialDevelopment ||
		reconciliationContext.Turn.ChangeProposalID != "40000000-0000-4000-8000-000000000218" ||
		reconciliationContext.Turn.ExpectedHeadSHA != "head-sha" ||
		reconciliationContext.Turn.CreatedAt.IsZero() {
		t.Errorf("reconciliation context = %#v", reconciliationContext)
	}
	if reconciliationContext.ChangeProposal == nil || reconciliationContext.ChangeProposal.ID != "40000000-0000-4000-8000-000000000218" ||
		reconciliationContext.ChangeProposal.PullRequestID != 8121 || reconciliationContext.ChangeProposal.PullRequestNumber != 121 ||
		reconciliationContext.ChangeProposal.BaseRef != "main" || reconciliationContext.ChangeProposal.BaseSHA != "base-sha" ||
		reconciliationContext.ChangeProposal.HeadRef != "feature" ||
		reconciliationContext.ChangeProposal.HeadSHA != "advanced-head-sha" {
		t.Errorf("Turn-bound Change Proposal context = %#v", reconciliationContext.ChangeProposal)
	}
	mutations, err := database.ListAgentTurnMutationsForReconciliation(ctx, *reconcileLease)
	if err != nil {
		t.Fatalf("ListAgentTurnMutationsForReconciliation() error = %v", err)
	}
	if len(mutations) != 2 || mutations[0].ID != unknown.ID || mutations[0].State != store.MutationUnknown ||
		mutations[1].ID != reconciling.ID || mutations[1].State != store.MutationReconciling {
		t.Fatalf("mutations for reconciliation = %#v, want ordered UNKNOWN and RECONCILING", mutations)
	}

	retryDelay := 15 * time.Millisecond
	firstFailureAt := time.Now()
	acknowledgement, err := database.AcknowledgeAgentTurnMutationReconciliationFailure(ctx, *reconcileLease, errors.New("GitHub search inconclusive"), retryDelay)
	if err != nil {
		t.Fatalf("first AcknowledgeAgentTurnMutationReconciliationFailure() error = %v", err)
	}
	if !acknowledgement.RetryScheduled || acknowledgement.Escalated || acknowledgement.Attempt != 1 || acknowledgement.UnresolvedMutationCount != 2 {
		t.Errorf("first acknowledgement = %#v, want delayed retry", acknowledgement)
	}
	retriedJob, err := database.GetJob(ctx, recovery.ReconcileMutationsJobID)
	if err != nil || retriedJob.Status != store.JobAvailable || retriedJob.AvailableAt.Before(firstFailureAt.Add(retryDelay-time.Millisecond)) {
		t.Errorf("first retried job = (%#v, %v), want delayed AVAILABLE", retriedJob, err)
	}
	if _, err := database.AcknowledgeAgentTurnMutationReconciliationFailure(ctx, *reconcileLease, errors.New("stale retry"), 0); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("failure acknowledgement with stale lease error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}

	time.Sleep(25 * time.Millisecond)
	second, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.ReconcileAgentTurnMutationsJobKind, "reconcile-worker", time.Second)
	if err != nil || second == nil {
		t.Fatalf("ClaimJobKind() second reconciliation = (%#v, %v), want lease", second, err)
	}
	acknowledgement, err = database.AcknowledgeAgentTurnMutationReconciliationFailure(ctx, *second, errors.New("GitHub search still inconclusive"), retryDelay)
	if err != nil || !acknowledgement.RetryScheduled || acknowledgement.Attempt != 2 {
		t.Fatalf("second acknowledgement = (%#v, %v), want retry", acknowledgement, err)
	}

	time.Sleep(25 * time.Millisecond)
	finalLease, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.ReconcileAgentTurnMutationsJobKind, "reconcile-worker", time.Second)
	if err != nil || finalLease == nil {
		t.Fatalf("ClaimJobKind() final reconciliation = (%#v, %v), want lease", finalLease, err)
	}
	finalCause := errors.New("GitHub search remained inconclusive")
	acknowledgement, err = database.AcknowledgeAgentTurnMutationReconciliationFailure(ctx, *finalLease, finalCause, retryDelay)
	if err != nil {
		t.Fatalf("final AcknowledgeAgentTurnMutationReconciliationFailure() error = %v", err)
	}
	if acknowledgement.RetryScheduled || !acknowledgement.Escalated || acknowledgement.Attempt != 3 ||
		acknowledgement.WorkflowRevision != 8 || acknowledgement.UnresolvedMutationCount != 2 ||
		!strings.Contains(acknowledgement.Diagnostic, "outcome unknowable; escalated") ||
		!strings.Contains(acknowledgement.Diagnostic, finalCause.Error()) {
		t.Errorf("final acknowledgement = %#v, want terminal escalation", acknowledgement)
	}
	if _, err := database.ListAgentTurnMutationsForReconciliation(ctx, *finalLease); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("list with completed reconciliation lease error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	settledAfterEscalation, err := database.GetAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("GetAgentTurnRecovery() after final escalation error = %v", err)
	}
	if !settledAfterEscalation.SuccessorAllowed || settledAfterEscalation.RecoverySettledAt == nil || settledAfterEscalation.MutationsUnsettled {
		t.Errorf("recovery after final escalation = %#v, want settled", settledAfterEscalation)
	}

	for _, mutationID := range []string{unknown.ID, reconciling.ID} {
		var state, diagnostic string
		if err := pool.QueryRow(ctx, `SELECT state, last_error FROM tool_invocations WHERE id = $1`, mutationID).Scan(&state, &diagnostic); err != nil {
			t.Fatalf("read escalated mutation %s: %v", mutationID, err)
		}
		if state != string(store.MutationFailed) || diagnostic != acknowledgement.Diagnostic {
			t.Errorf("escalated mutation %s = (%s, %q), want FAILED with %q", mutationID, state, diagnostic, acknowledgement.Diagnostic)
		}
	}

	var workflowStatus, desiredAssignmentStatus, resumeRole, workflowReason, attemptReason string
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.desired_assignment_status, workflow.resume_role,
       workflow.human_handoff_reason, attempt.human_handoff_reason
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.workflow_id = workflow.id AND attempt.active
WHERE workflow.id = $1`, fixture.workflowID).Scan(
		&workflowStatus, &desiredAssignmentStatus, &resumeRole, &workflowReason, &attemptReason,
	); err != nil {
		t.Fatalf("read escalated Workflow: %v", err)
	}
	wantReason := string(workflow.ReasonAgentTurnMutationReconciliationExhausted)
	if workflowStatus != string(workflow.StateNeedsHuman) || desiredAssignmentStatus != string(workflow.AssignmentWaitingForHuman) ||
		resumeRole != string(workflow.RoleDeveloper) || workflowReason != wantReason || attemptReason != wantReason {
		t.Errorf("escalated Workflow = (%s, %s, %s, %s, %s)", workflowStatus, desiredAssignmentStatus, resumeRole, workflowReason, attemptReason)
	}
	var waitingAssignments int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM agent_assignments
WHERE workflow_id = $1 AND status = 'WAITING_FOR_HUMAN' AND state_deleted_at IS NULL`, fixture.workflowID).Scan(&waitingAssignments); err != nil {
		t.Fatalf("count waiting Assignments: %v", err)
	}
	if waitingAssignments != 2 {
		t.Errorf("waiting Assignments = %d, want 2", waitingAssignments)
	}
	var handoffPayload, labelsPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF'`, fixture.workflowID).Scan(&handoffPayload); err != nil {
		t.Fatalf("read PUBLISH_HUMAN_HANDOFF: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT payload FROM jobs WHERE workflow_id = $1 AND kind = 'RECONCILE_GITHUB_LABELS'`, fixture.workflowID).Scan(&labelsPayload); err != nil {
		t.Fatalf("read RECONCILE_GITHUB_LABELS: %v", err)
	}
	if !strings.Contains(string(handoffPayload), wantReason) || !strings.Contains(string(handoffPayload), "outcome unknowable; escalated") ||
		!strings.Contains(string(labelsPayload), string(workflow.StateNeedsHuman)) {
		t.Errorf("handoff actions = handoff %s, labels %s", handoffPayload, labelsPayload)
	}

	reconciliationJob, err := database.GetJob(ctx, recovery.ReconcileMutationsJobID)
	if err != nil || reconciliationJob.Status != store.JobSucceeded ||
		!strings.Contains(string(reconciliationJob.Result), `"escalated": true`) {
		t.Errorf("completed reconciliation job = (%#v, %v), want escalated SUCCEEDED", reconciliationJob, err)
	}
	settled, err := database.CompleteAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("CompleteAgentTurnRecovery() after escalation error = %v", err)
	}
	if !settled.SuccessorAllowed || settled.RecoverySettledAt == nil || settled.MutationsUnsettled {
		t.Errorf("settled escalated recovery = %#v", settled)
	}
}

func TestRecoveredSubmitReviewBindsReviewerActor(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 34)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET role = 'REVIEWER', agent_profile_name = 'reviewer' WHERE id = $1`, fixture.assignmentID); err != nil {
		t.Fatalf("prepare Reviewer Assignment: %v", err)
	}
	turnSpec := fixture.turnSpec()
	turnSpec.Purpose = workflow.TurnPurposeReview
	turnSpec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10, "Review test instructions.", nil)
	turn, err := database.AllocateAgentTurn(ctx, turnSpec)
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	executionJob := claimAgentTurnJob(t, database, ctx, turn, 80*time.Millisecond)
	turnLease, err := database.AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "review-runtime", 80*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := database.OpenMutationAdmission(ctx, turnLease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	mutation, err := database.ReserveMutation(ctx, turnLease, store.MutationSpec{
		OperationID: "recovered-submit-review", ToolName: "submit_review", Request: json.RawMessage(`{"event":"REQUEST_CHANGES"}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() error = %v", err)
	}
	if _, err := database.StartMutation(ctx, turnLease, mutation.ID); err != nil {
		t.Fatalf("StartMutation() error = %v", err)
	}
	conflicting, err := database.ReserveMutation(ctx, turnLease, store.MutationSpec{
		OperationID: "conflicting-recovered-submit-review", ToolName: "submit_review", Request: json.RawMessage(`{"event":"REQUEST_CHANGES"}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() conflicting error = %v", err)
	}
	if _, err := database.StartMutation(ctx, turnLease, conflicting.ID); err != nil {
		t.Fatalf("StartMutation() conflicting error = %v", err)
	}

	time.Sleep(110 * time.Millisecond)
	if _, err := database.RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch); err != nil {
		t.Fatalf("RecoverExpiredAgentTurn() error = %v", err)
	}
	stopLease, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "stop-worker", time.Second)
	if err != nil || stopLease == nil {
		t.Fatalf("ClaimJobKind() stop = (%#v, %v), want lease", stopLease, err)
	}
	reconcileLease, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.ReconcileAgentTurnMutationsJobKind, "reconcile-worker", time.Second)
	if err != nil || reconcileLease == nil {
		t.Fatalf("ClaimJobKind() reconciliation = (%#v, %v), want lease", reconcileLease, err)
	}
	cleanup, err := database.GetAgentTurnRuntimeCleanupContext(ctx, *stopLease)
	if err != nil {
		t.Fatalf("GetAgentTurnRuntimeCleanupContext() error = %v", err)
	}
	if cleanup.AssignmentID != fixture.assignmentID || cleanup.Role != workflow.RoleReviewer {
		t.Errorf("runtime cleanup context = %#v, want recovered Reviewer Assignment", cleanup)
	}
	staleStop := *stopLease
	staleStop.LeaseToken = "30000000-0000-4000-8000-000000000097"
	if _, err := database.GetAgentTurnRuntimeCleanupContext(ctx, staleStop); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("GetAgentTurnRuntimeCleanupContext() with stale lease error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	if _, err := database.GetAgentTurnRuntimeCleanupContext(ctx, *reconcileLease); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("GetAgentTurnRuntimeCleanupContext() with reconciliation lease error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, *stopLease); err != nil {
		t.Fatalf("AcknowledgeRecoveredRuntimeStopped() error = %v", err)
	}
	reconciliationContext, err := database.GetAgentTurnMutationReconciliationContext(ctx, *reconcileLease)
	if err != nil {
		t.Fatalf("GetAgentTurnMutationReconciliationContext() error = %v", err)
	}
	if reconciliationContext.ChangeProposal != nil {
		t.Fatalf("Change Proposal context = %#v, want nil for unbound Turn", reconciliationContext.ChangeProposal)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, *reconcileLease, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"review_id":2,"actor_id":9201}`),
	}); err != nil {
		t.Fatalf("ReconcileRecoveredMutation() error = %v", err)
	}
	var actorID int64
	if err := pool.QueryRow(ctx, `SELECT github_app_actor_id FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&actorID); err != nil {
		t.Fatalf("read recovered Reviewer actor: %v", err)
	}
	if actorID != 9201 {
		t.Errorf("recovered Reviewer actor = %d, want 9201", actorID)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, *reconcileLease, conflicting.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"review_id":3,"actor_id":9202}`),
	}); !errors.Is(err, store.ErrReviewerActorConflict) {
		t.Fatalf("conflicting ReconcileRecoveredMutation() error = %v, want ErrReviewerActorConflict", err)
	}
	var mutationState string
	if err := pool.QueryRow(ctx, `SELECT github_app_actor_id FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&actorID); err != nil {
		t.Fatalf("read Reviewer actor after conflict: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id = $1`, conflicting.ID).Scan(&mutationState); err != nil {
		t.Fatalf("read conflicting recovered mutation: %v", err)
	}
	if actorID != 9201 || mutationState != string(store.MutationUnknown) {
		t.Errorf("conflicting recovery CAS = actor %d, mutation %s; want actor 9201 and UNKNOWN rollback", actorID, mutationState)
	}
}
