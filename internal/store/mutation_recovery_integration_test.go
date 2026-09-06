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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jozala/omnigrex/internal/mcp"
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
	executionJob := claimAgentTurnJob(t, database, ctx, turn, 5*time.Second)
	turnLease, err := database.AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "runtime", 5*time.Second, 1)
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

	expireAgentTurnExecution(t, pool, ctx, executionJob.ID, turn.ID)
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
	if _, err := database.ReconcileRecoveredMutation(ctx, *reconcileLease, unknown.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"pull_request_id":8121}`),
	}); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("ReconcileRecoveredMutation() before stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	if _, err := database.AcknowledgeAgentTurnMutationReconciliationFailure(ctx, *reconcileLease, errors.New("premature reconciliation"), 0); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("AcknowledgeAgentTurnMutationReconciliationFailure() before stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	if err := database.HeartbeatJob(ctx, *reconcileLease, 2*time.Second); err != nil {
		t.Fatalf("HeartbeatJob() while waiting for runtime stop error = %v", err)
	}
	stale := *reconcileLease
	stale.LeaseToken = "30000000-0000-4000-8000-000000000097"
	if _, err := database.GetAgentTurnMutationReconciliationContext(ctx, stale); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("GetAgentTurnMutationReconciliationContext() stale lease error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	staleStop := *stopLease
	staleStop.LeaseToken = "30000000-0000-4000-8000-000000000096"
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, staleStop); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("AcknowledgeRecoveredRuntimeStopped() stale lease error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	if _, err := database.GetAgentTurnMutationReconciliationContext(ctx, *reconcileLease); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("GetAgentTurnMutationReconciliationContext() after stale stop acknowledgement error = %v, want ErrAgentTurnRecoveryUnsettled", err)
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
	if settledAfterEscalation.Continuation != "MUTATION_RECONCILIATION_HANDOFF_APPLIED" || settledAfterEscalation.SettlementID != "" {
		t.Errorf("reconciliation exhaustion continuation = %#v", settledAfterEscalation)
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
	var infrastructureFailures, settlements, preparationJobs, handoffJobs int
	if err := pool.QueryRow(ctx, `
SELECT attempt.infrastructure_failures,
       (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = $2),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF')
FROM workflow_attempts AS attempt WHERE attempt.id = $3`, fixture.workflowID, turn.ID, fixture.attemptID).Scan(
		&infrastructureFailures, &settlements, &preparationJobs, &handoffJobs,
	); err != nil {
		t.Fatal(err)
	}
	if infrastructureFailures != 0 || settlements != 0 || preparationJobs != 0 || handoffJobs != 1 {
		t.Errorf("exhaustion transition = infrastructure budget %d, settlements %d, preparations %d, handoffs %d",
			infrastructureFailures, settlements, preparationJobs, handoffJobs)
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
	executionJob := claimAgentTurnJob(t, database, ctx, turn, 5*time.Second)
	turnLease, err := database.AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "review-runtime", 5*time.Second, 1)
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

	expireAgentTurnExecution(t, pool, ctx, executionJob.ID, turn.ID)
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

func TestSucceededMutationVerificationExhaustsIntoHumanHandoffWithoutRetry(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 341, workflow.RoleDeveloper, "")
	if _, err := pool.Exec(ctx, `
UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, lease.WorkflowAttemptID); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "verify-succeeded-missing", ToolName: mcp.ToolCommentOnIssue,
		Request:         json.RawMessage(`{"operation_id":"verify-succeeded-missing","body":"durable"}`),
		ExternalService: "github", ExternalResourceID: "341:341",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"comment_id":341}`)); err != nil {
		t.Fatal(err)
	}
	var originalResult string
	var originalUpdatedAt, originalFinishedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT result::text, updated_at, finished_at FROM tool_invocations WHERE id = $1`, mutation.ID).Scan(
		&originalResult, &originalUpdatedAt, &originalFinishedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	recovery, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil || recovery.ReconcileMutationsJobID == "" {
		t.Fatalf("RecoverExpiredAgentTurn() = (%#v, %v), want successful-mutation verification barrier", recovery, err)
	}
	stopLease := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "missing-verification-stop")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease); err != nil {
		t.Fatal(err)
	}
	worker, err := mcp.NewRecoveryWorker(database, unresolvedMutationReconciler{}, mcp.RecoveryWorkerConfig{
		ClaimOwner: "missing-verification-worker", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		processed, processErr := worker.ProcessNext(ctx)
		if !processed || !errors.Is(processErr, mcp.ErrMutationReconciliationUnresolved) {
			t.Fatalf("ProcessNext() attempt %d = (%t, %v)", attempt, processed, processErr)
		}
		if attempt < 3 {
			time.Sleep(3 * time.Millisecond)
		}
	}

	var workflowState, mutationState, verifiedResult string
	var verifiedUpdatedAt, verifiedFinishedAt time.Time
	var retries int
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, lease.JobLease.WorkflowID).Scan(&workflowState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state, result::text, updated_at, finished_at FROM tool_invocations WHERE id = $1`, mutation.ID).Scan(
		&mutationState, &verifiedResult, &verifiedUpdatedAt, &verifiedFinishedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM jobs
WHERE kind = 'PREPARE_AGENT_TURN' AND payload->>'retry_of_turn_id' = $1`, lease.ID).Scan(&retries); err != nil {
		t.Fatal(err)
	}
	if workflowState != string(workflow.StateNeedsHuman) || mutationState != string(store.MutationSucceeded) || retries != 0 {
		t.Fatalf("exhausted verification = workflow %s, mutation %s, retries %d", workflowState, mutationState, retries)
	}
	if verifiedResult != originalResult || !verifiedUpdatedAt.Equal(originalUpdatedAt) || !verifiedFinishedAt.Equal(originalFinishedAt) {
		t.Fatal("successful mutation ledger evidence changed during failed verification")
	}
}

func TestConcurrentSucceededMutationVerificationRunsOnce(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, lease, _ := prepareOpenSettlementTurn(t, databases[0], pool, ctx, 342, workflow.RoleDeveloper, "")
	mutation, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "verify-succeeded-concurrently", ToolName: mcp.ToolCommentOnIssue,
		Request:         json.RawMessage(`{"operation_id":"verify-succeeded-concurrently","body":"durable"}`),
		ExternalService: "github", ExternalResourceID: "342:342",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := databases[0].StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"comment_id":342}`)); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	if _, err := databases[0].RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	stopLease := claimRecoveryJob(t, databases[0], ctx, store.StopStaleRuntimeJobKind, "concurrent-verification-stop")
	if _, err := databases[0].AcknowledgeRecoveredRuntimeStopped(ctx, stopLease); err != nil {
		t.Fatal(err)
	}

	reconciler := &concurrentFoundMutationReconciler{}
	start := make(chan struct{})
	results := make(chan bool, len(databases))
	errorsFound := make(chan error, len(databases))
	var wait sync.WaitGroup
	for index, database := range databases {
		worker, err := mcp.NewRecoveryWorker(database, reconciler, mcp.RecoveryWorkerConfig{
			ClaimOwner: fmt.Sprintf("concurrent-verifier-%d", index), LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
			IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			processed, err := worker.ProcessNext(ctx)
			results <- processed
			errorsFound <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	processed := 0
	for result := range results {
		if result {
			processed++
		}
	}
	if processed != 1 || reconciler.callCount() != 1 {
		t.Fatalf("concurrent verification = %d workers, %d reconciler calls; want one each", processed, reconciler.callCount())
	}
}

func expireAgentTurnExecution(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, ctx context.Context, jobID, turnID string) {
	t.Helper()
	for _, update := range []struct {
		query string
		id    string
	}{
		{`UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, jobID},
		{`UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1 AND status = 'LEASED'`, jobID},
		{`UPDATE agent_turns SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, turnID},
		{`UPDATE agent_turn_slots SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE agent_turn_id = $1`, turnID},
	} {
		if _, err := pool.Exec(ctx, update.query, update.id); err != nil {
			t.Fatalf("expire Agent Turn execution: %v", err)
		}
	}
}

type unresolvedMutationReconciler struct{}

func (unresolvedMutationReconciler) Reconcile(context.Context, store.AgentTurnMutationReconciliationContext, store.MutationReservation) (mcp.MutationReconciliationResult, error) {
	return mcp.MutationReconciliationResult{Disposition: mcp.ReconciliationUnresolved}, nil
}

type concurrentFoundMutationReconciler struct {
	mutex sync.Mutex
	calls int
}

func (reconciler *concurrentFoundMutationReconciler) Reconcile(_ context.Context, _ store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (mcp.MutationReconciliationResult, error) {
	reconciler.mutex.Lock()
	reconciler.calls++
	reconciler.mutex.Unlock()
	return mcp.MutationReconciliationResult{
		Disposition: mcp.ReconciliationFound,
		Outcome:     store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: append(json.RawMessage(nil), mutation.Result...)},
	}, nil
}

func (reconciler *concurrentFoundMutationReconciler) callCount() int {
	reconciler.mutex.Lock()
	defer reconciler.mutex.Unlock()
	return reconciler.calls
}
