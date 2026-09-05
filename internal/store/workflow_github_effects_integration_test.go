//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestWorkflowGitHubEffectContextAndAcknowledgementAreFencedByCurrentState(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workflowID := "7a000000-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, human_handoff_reason
)
VALUES ($1, 91, 'acme', 'widgets', 92, 17, 'NEEDS_HUMAN', 7, $2)`,
		workflowID, workflow.ReasonReviewBudgetExhausted); err != nil {
		t.Fatalf("seed Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha,
    head_ref, head_sha, ready_for_sha
)
VALUES ('7a000000-0000-4000-8000-000000000002', $1, 91, 'acme', 'widgets',
        93, 23, 'OPEN', 'main', repeat('b', 40), 'feature', repeat('a', 40), repeat('a', 40))`, workflowID); err != nil {
		t.Fatalf("seed Change Proposal: %v", err)
	}
	job, _, err := databases[0].EnqueueJob(ctx, store.JobSpec{
		Queue: store.WorkflowActionQueue, Kind: store.PublishHumanHandoffJobKind,
		Payload:     json.RawMessage(`{"reason":"review_budget_exhausted","diagnostic":"latest safe diagnostic","revision":7}`),
		MaxAttempts: 3, IdempotencyKey: "handoff-effect-fence", WorkflowID: workflowID,
	})
	if err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	lease, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.PublishHumanHandoffJobKind, "handoff-worker", time.Second)
	if err != nil || lease == nil || lease.ID != job.ID {
		t.Fatalf("ClaimWorkflowGitHubEffectJob() = (%#v, %v)", lease, err)
	}
	if err := databases[0].CompleteJob(ctx, *lease, json.RawMessage(`{}`)); !errors.Is(err, store.ErrWorkflowJobRequiresAcknowledgement) {
		t.Fatalf("generic CompleteJob() error = %v, want fenced acknowledgement", err)
	}

	effect, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *lease)
	if err != nil {
		t.Fatalf("GetWorkflowGitHubEffectContext() error = %v", err)
	}
	if effect.RepositoryID != 91 || effect.RepositoryOwner != "acme" || effect.RepositoryName != "widgets" ||
		effect.IssueNumber != 17 || effect.State != workflow.StateNeedsHuman || effect.Revision != 7 || effect.JobRevision != 7 ||
		effect.HandoffReason != string(workflow.ReasonReviewBudgetExhausted) || effect.HandoffDiagnostic != "latest safe diagnostic" ||
		effect.ChangeProposal == nil || effect.ChangeProposal.Number != 23 || effect.ChangeProposal.HeadSHA != strings.Repeat("a", 40) || effect.ReadyForSHA != strings.Repeat("a", 40) {
		t.Fatalf("effect context = %#v", effect)
	}

	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'REVIEWING', state_revision = 8, human_handoff_reason = NULL WHERE id = $1`, workflowID); err != nil {
		t.Fatalf("advance Workflow: %v", err)
	}
	acknowledgement, err := databases[0].AcknowledgeWorkflowGitHubEffect(ctx, *lease, effect, json.RawMessage(`{"comment_id":99}`))
	if err != nil {
		t.Fatalf("AcknowledgeWorkflowGitHubEffect() error = %v", err)
	}
	if !acknowledgement.Superseded || !acknowledgement.CleanupRequired || acknowledgement.Revision != 8 || acknowledgement.State != workflow.StateReviewing {
		t.Fatalf("acknowledgement = %#v, want superseded revision 8", acknowledgement)
	}
	stored, err := databases[0].GetJob(ctx, lease.ID)
	if err != nil || stored.Status != store.JobLeased || len(stored.Result) != 0 {
		t.Fatalf("stored cleanup-pending job = %#v, error %v", stored, err)
	}
	if _, err := databases[0].AcknowledgeWorkflowGitHubEffectCleanup(ctx, *lease); err != nil {
		t.Fatalf("AcknowledgeWorkflowGitHubEffectCleanup() error = %v", err)
	}
	stored, err = databases[0].GetJob(ctx, lease.ID)
	var cleanedResult struct {
		CleanupVerified bool `json:"cleanup_verified"`
	}
	decodeErr := json.Unmarshal(stored.Result, &cleanedResult)
	if err != nil || decodeErr != nil || stored.Status != store.JobSucceeded || !cleanedResult.CleanupVerified || strings.Contains(string(stored.Result), "comment_id") {
		t.Fatalf("stored cleaned job = %#v, error %v", stored, err)
	}
}

func TestWorkflowGitHubEffectFailureRetriesDurably(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workflowID := "7b000000-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status, state_revision)
VALUES ($1, 101, 'acme', 'widgets', 102, 17, 'DEVELOPING', 2)`, workflowID); err != nil {
		t.Fatalf("seed Workflow: %v", err)
	}
	_, _, err := databases[0].EnqueueJob(ctx, store.JobSpec{
		Queue: store.WorkflowActionQueue, Kind: store.ReconcileGitHubLabelsJobKind,
		Payload: json.RawMessage(`{"state":"DEVELOPING","revision":2}`), MaxAttempts: 3,
		IdempotencyKey: "label-effect-retry", WorkflowID: workflowID,
	})
	if err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	lease, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "label-worker", time.Second)
	if err != nil || lease == nil {
		t.Fatalf("ClaimWorkflowGitHubEffectJob() = (%#v, %v)", lease, err)
	}
	effect, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *lease)
	if err != nil {
		t.Fatalf("GetWorkflowGitHubEffectContext() error = %v", err)
	}
	acknowledgement, err := databases[0].AcknowledgeWorkflowGitHubEffectFailure(ctx, *lease, effect, errors.New("GitHub unavailable"), true, time.Millisecond)
	if err != nil || !acknowledgement.RetryScheduled || acknowledgement.Superseded {
		t.Fatalf("failure acknowledgement = (%#v, %v)", acknowledgement, err)
	}
	stored, err := databases[0].GetJob(ctx, lease.ID)
	if err != nil || stored.Status != store.JobAvailable || stored.LastError != "GitHub unavailable" || stored.AttemptCount != 1 {
		t.Fatalf("stored retry job = %#v, error %v", stored, err)
	}
}

func TestWorkflowGitHubEffectExhaustionWaitsForTurnMutationAndSuccessorBeforeOneHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 3)
	fixture := seedAgentSession(t, pool, 49)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', state_revision = 3,
    desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE'
WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	activeTurn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	executionJob := claimAgentTurnJob(t, databases[0], ctx, activeTurn, 10*time.Second)
	turnLease, err := databases[0].AcquireAgentTurn(ctx, executionJob, activeTurn.ControlRevision, "runtime", 10*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, turnLease); err != nil {
		t.Fatal(err)
	}
	mutation, err := databases[0].ReserveMutation(ctx, turnLease, store.MutationSpec{
		OperationID: "effect-exhaustion-in-flight", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"in flight"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := databases[0].StartMutation(ctx, turnLease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	originID := "49000000-0000-4000-8000-000000000001"
	delivery := workflowDelivery(originID)
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 49, 49, 49
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	if inserted, err := databases[0].InsertWebhookDelivery(ctx, delivery); err != nil || !inserted {
		t.Fatalf("InsertWebhookDelivery() = (%t, %v)", inserted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'PROCESSED', workflow_id = $2, processed_at = clock_timestamp() WHERE delivery_id = $1`, originID, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO normalized_events (
    delivery_id, payload, status, workflow_id, disposition, reason, applied_revision, processed_at
) VALUES ($1, $3, 'COMPLETED', $2, 'APPLIED', 'workflow_triggered', 3, clock_timestamp())`,
		originID, fixture.workflowID, normalizedPayload(originID, "triggered")); err != nil {
		t.Fatal(err)
	}
	jobID := "49000000-0000-4000-8000-000000000002"
	if _, err := pool.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, max_attempts, idempotency_key, workflow_id,
    workflow_attempt_id, normalized_event_id, action_key
) VALUES ($1, 'workflow', 'RECONCILE_GITHUB_LABELS', '{"state":"DEVELOPING","revision":3}', 1,
    'label-exhaustion-49', $2, $3, $4, 'reconcile-github-labels')`,
		jobID, fixture.workflowID, fixture.attemptID, originID); err != nil {
		t.Fatal(err)
	}
	lease, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "label-worker", 5*time.Second)
	if err != nil || lease == nil {
		t.Fatalf("ClaimWorkflowGitHubEffectJob() = (%#v, %v)", lease, err)
	}
	effect, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *lease)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgement, err := databases[0].AcknowledgeWorkflowGitHubEffectFailure(ctx, *lease, effect,
		errors.New("GitHub App installation is missing"), false, 0)
	if err != nil || acknowledgement.HandoffApplied || acknowledgement.Revision != 3 || acknowledgement.State != workflow.StateDeveloping {
		t.Fatalf("terminal visible-effect acknowledgement = (%#v, %v)", acknowledgement, err)
	}
	if escalated, err := databases[0].ApplyNextWorkflowActionFailureEscalation(ctx, "escalation-worker", 5*time.Second); err != nil || escalated != nil {
		t.Fatalf("escalation with active turn = (%#v, %v), want deferred", escalated, err)
	}
	var state, assignmentStatus, sourceStatus, turnStatus, executionStatus, mutationState string
	var revision, derived, slotCount int
	var turnActive, admissionOpen bool
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.desired_assignment_status, source.status,
	   (SELECT count(*) FROM jobs WHERE normalized_event_id = $2
           AND action_key IN (
               'workflow-action:reconcile_github_labels:' || $3 || ':publish-human-handoff',
               'workflow-action:reconcile_github_labels:' || $3 || ':reconcile-github-labels'
           ))
	   , turn.status, turn.active, turn.mutation_admission_open,
	   execution.status, invocation.state,
	   (SELECT count(*) FROM agent_turn_slots WHERE agent_turn_id = turn.id)
FROM workflows AS workflow JOIN jobs AS source ON source.id = $3
JOIN agent_turns AS turn ON turn.id = $4
JOIN jobs AS execution ON execution.agent_turn_id = turn.id AND execution.kind = 'RUN_AGENT_TURN'
JOIN tool_invocations AS invocation ON invocation.id = $5
WHERE workflow.id = $1`, fixture.workflowID, originID, jobID, activeTurn.ID, mutation.ID).Scan(
		&state, &revision, &assignmentStatus, &sourceStatus, &derived,
		&turnStatus, &turnActive, &admissionOpen, &executionStatus, &mutationState, &slotCount,
	); err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.StateDeveloping) || revision != 3 || assignmentStatus != string(workflow.AssignmentActive) ||
		sourceStatus != string(store.JobFailed) || derived != 0 || turnStatus != "RUNNING" || !turnActive || !admissionOpen ||
		executionStatus != string(store.JobLeased) || mutationState != string(store.MutationInFlight) || slotCount != 1 {
		t.Errorf("deferred exhaustion altered authority = %s@%d assignment %s source %s derived %d turn %s/%t/%t execution %s mutation %s slots %d", state, revision, assignmentStatus, sourceStatus, derived, turnStatus, turnActive, admissionOpen, executionStatus, mutationState, slotCount)
	}
	if err := databases[0].CompleteMutation(ctx, turnLease, mutation.ID, json.RawMessage(`{"comment_id":42}`)); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].CloseMutationAdmission(ctx, turnLease); err != nil {
		t.Fatal(err)
	}
	settlement, err := databases[0].SettleAgentTurn(ctx, turnLease, failedSettlementObservation("runtime failed after mutation completion"))
	if err != nil || settlement.SuccessorJobID == "" || settlement.Revision != 4 {
		t.Fatalf("SettleAgentTurn() = (%#v, %v)", settlement, err)
	}
	if escalated, err := databases[0].ApplyNextWorkflowActionFailureEscalation(ctx, "escalation-worker", 5*time.Second); err != nil || escalated != nil {
		t.Fatalf("escalation with successor authority = (%#v, %v), want deferred", escalated, err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'test resolved successor authority'
WHERE id = $1 AND status = 'AVAILABLE'`, settlement.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	type escalationResult struct {
		result *store.WorkflowActionFailureEscalation
		err    error
	}
	start := make(chan struct{})
	results := make(chan escalationResult, 2)
	for index := range 2 {
		go func(database *store.Store) {
			<-start
			result, err := database.ApplyNextWorkflowActionFailureEscalation(ctx, "escalation-worker", 5*time.Second)
			results <- escalationResult{result: result, err: err}
		}(databases[index])
	}
	close(start)
	var applied, empty int
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.result == nil {
			empty++
		} else if result.result.HandoffApplied && result.result.WorkflowRevision == 5 {
			applied++
		}
	}
	if applied != 1 || empty != 1 {
		t.Fatalf("concurrent escalation outcomes = applied %d empty %d", applied, empty)
	}

	var generatedLabelID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE workflow_id = $1 AND action_key = $2`, fixture.workflowID,
		"workflow-action:reconcile_github_labels:"+jobID+":reconcile-github-labels").Scan(&generatedLabelID); err != nil {
		t.Fatal(err)
	}
	generated, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "label-worker", 5*time.Second)
	if err != nil || generated == nil || generated.ID != generatedLabelID {
		t.Fatalf("claim generated NEEDS_HUMAN label = (%#v, %v)", generated, err)
	}
	generatedEffect, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *generated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := databases[0].AcknowledgeWorkflowGitHubEffectFailure(ctx, *generated, generatedEffect,
		errors.New("GitHub still denies writes"), false, 0)
	if err != nil || second.HandoffApplied || second.Revision != 5 {
		t.Fatalf("nonrecursive visible-effect exhaustion = (%#v, %v)", second, err)
	}
	handoff, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.PublishHumanHandoffJobKind, "handoff-worker", 5*time.Second)
	if err != nil || handoff == nil {
		t.Fatalf("claim generated handoff = (%#v, %v)", handoff, err)
	}
	handoffEffect, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *handoff)
	if err != nil {
		t.Fatal(err)
	}
	publicationFailure, err := databases[0].AcknowledgeWorkflowGitHubEffectFailure(ctx, *handoff, handoffEffect,
		errors.New("GitHub App lacks Issues write permission"), false, 0)
	if err != nil || publicationFailure.HandoffApplied || publicationFailure.Revision != 5 {
		t.Fatalf("terminal handoff publication failure = (%#v, %v)", publicationFailure, err)
	}
	var escalationJobs, terminalFailures int
	if err := pool.QueryRow(ctx, `
SELECT workflow.state_revision,
       (SELECT count(*) FROM jobs WHERE workflow_id = workflow.id AND kind = 'ESCALATE_WORKFLOW_ACTION_FAILURE'),
       (SELECT count(*) FROM workflow_action_failures WHERE workflow_id = workflow.id AND status = 'TERMINAL')
FROM workflows AS workflow WHERE workflow.id = $1`, fixture.workflowID).Scan(&revision, &escalationJobs, &terminalFailures); err != nil || revision != 5 {
		t.Fatalf("Workflow revision after recursive denial = (%d, %v)", revision, err)
	}
	if escalationJobs != 1 || terminalFailures != 2 {
		t.Errorf("nonrecursive failures = %d escalation jobs, %d terminal records", escalationJobs, terminalFailures)
	}
}

func TestSupersededHumanHandoffCleanupSurvivesLeaseExpiry(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	workflowID := "4a000000-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, human_handoff_reason
) VALUES ($1, 501, 'acme', 'widgets', 502, 17, 'NEEDS_HUMAN', 7, 'agent_blocked')`, workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
) VALUES ('4a000000-0000-4000-8000-000000000002', $1, 501, 'acme', 'widgets',
    503, 23, 'OPEN', 'main', 'base', 'feature', 'head')`, workflowID); err != nil {
		t.Fatal(err)
	}
	job, _, err := databases[0].EnqueueJob(ctx, store.JobSpec{
		Queue: store.WorkflowActionQueue, Kind: store.PublishHumanHandoffJobKind,
		Payload:     json.RawMessage(`{"reason":"agent_blocked","revision":7,"pull_request_number":23}`),
		MaxAttempts: 3, IdempotencyKey: "handoff-cleanup-expiry", WorkflowID: workflowID,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.PublishHumanHandoffJobKind, "handoff-1", 30*time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first claim = (%#v, %v)", first, err)
	}
	effect, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'REVIEWING', state_revision = 8, human_handoff_reason = NULL WHERE id = $1`, workflowID); err != nil {
		t.Fatal(err)
	}
	acknowledgement, err := databases[0].AcknowledgeWorkflowGitHubEffect(ctx, *first, effect, json.RawMessage(`{"issue":{"id":101},"pull_request":{"id":102}}`))
	if err != nil || !acknowledgement.CleanupRequired {
		t.Fatalf("superseded acknowledgement = (%#v, %v)", acknowledgement, err)
	}
	time.Sleep(40 * time.Millisecond)
	second, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.PublishHumanHandoffJobKind, "handoff-2", 5*time.Second)
	if err != nil || second == nil || second.ID != job.ID || second.Attempt != 2 {
		t.Fatalf("reclaimed cleanup claim = (%#v, %v)", second, err)
	}
	cleanup, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *second)
	if err != nil || !cleanup.CleanupRequired || cleanup.CleanupIssueNumber != 17 || cleanup.CleanupPullRequestNumber != 23 {
		t.Fatalf("reclaimed cleanup context = (%#v, %v)", cleanup, err)
	}
	failure, err := databases[0].AcknowledgeWorkflowGitHubEffectFailure(ctx, *second, cleanup,
		errors.New("GitHub cleanup temporarily unavailable"), true, time.Millisecond)
	if err != nil || !failure.RetryScheduled {
		t.Fatalf("cleanup failure acknowledgement = (%#v, %v)", failure, err)
	}
	time.Sleep(2 * time.Millisecond)
	third, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.PublishHumanHandoffJobKind, "handoff-3", 5*time.Second)
	if err != nil || third == nil || third.ID != job.ID || third.Attempt != 3 {
		t.Fatalf("retried cleanup claim = (%#v, %v)", third, err)
	}
	if retriedCleanup, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *third); err != nil || !retriedCleanup.CleanupRequired {
		t.Fatalf("retried cleanup context = (%#v, %v)", retriedCleanup, err)
	}
	if _, err := databases[0].AcknowledgeWorkflowGitHubEffectCleanup(ctx, *third); err != nil {
		t.Fatal(err)
	}
	if _, err := databases[0].AcknowledgeWorkflowGitHubEffectCleanup(ctx, *first); !errors.Is(err, store.ErrWorkflowGitHubEffectFenceLost) {
		t.Fatalf("stale cleanup acknowledgement error = %v", err)
	}
	if _, err := databases[0].AcknowledgeWorkflowGitHubEffectCleanup(ctx, *second); !errors.Is(err, store.ErrWorkflowGitHubEffectFenceLost) {
		t.Fatalf("stale retried cleanup acknowledgement error = %v", err)
	}
}

func TestSupersededHumanHandoffCleanupFinalFailureIsTerminalAndEscalatesWithProvenance(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 51)
	workflowID := fixture.workflowID
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'NEEDS_HUMAN', state_revision = 7,
    human_handoff_reason = 'agent_blocked', resume_role = 'DEVELOPER',
    desired_assignment_status = 'WAITING_FOR_HUMAN', desired_runtime_state = 'ACTIVE'
WHERE id = $1`, workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	job, _, err := databases[0].EnqueueJob(ctx, store.JobSpec{
		Queue: store.WorkflowActionQueue, Kind: store.PublishHumanHandoffJobKind,
		Payload: json.RawMessage(`{"reason":"agent_blocked","revision":7}`), MaxAttempts: 1,
		IdempotencyKey: "handoff-cleanup-final", WorkflowID: workflowID,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.PublishHumanHandoffJobKind, "handoff-worker", 5*time.Second)
	if err != nil || lease == nil {
		t.Fatalf("claim handoff = (%#v, %v)", lease, err)
	}
	effect, err := databases[0].GetWorkflowGitHubEffectContext(ctx, *lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', state_revision = 8, human_handoff_reason = NULL,
    resume_role = NULL, desired_assignment_status = 'ACTIVE'
WHERE id = $1`, workflowID); err != nil {
		t.Fatal(err)
	}
	acknowledgement, err := databases[0].AcknowledgeWorkflowGitHubEffect(ctx, *lease, effect, json.RawMessage(`{"issue":{"id":101}}`))
	if err != nil || !acknowledgement.CleanupRequired {
		t.Fatalf("record superseded cleanup = (%#v, %v)", acknowledgement, err)
	}
	diagnostic := "GitHub denied exact marker deletion"
	failure, err := databases[0].AcknowledgeWorkflowGitHubEffectFailure(ctx, *lease, effect, errors.New(diagnostic), false, 0)
	if err != nil || failure.RetryScheduled || failure.CleanupRequired || !failure.Superseded {
		t.Fatalf("terminal cleanup failure = (%#v, %v)", failure, err)
	}
	var cleanupStatus, cleanupDiagnostic, sourceStatus, failureStatus, sourceKind string
	var observedRevision, currentRevision int
	if err := pool.QueryRow(ctx, `
SELECT cleanup.status, cleanup.last_error, cleanup.observed_revision, cleanup.current_revision,
       source.status, action_failure.status, action_failure.source_kind
FROM workflow_github_effect_cleanups AS cleanup
JOIN jobs AS source ON source.id = cleanup.job_id
JOIN workflow_action_failures AS action_failure ON action_failure.source_job_id = source.id
WHERE cleanup.job_id = $1`, job.ID).Scan(&cleanupStatus, &cleanupDiagnostic, &observedRevision,
		&currentRevision, &sourceStatus, &failureStatus, &sourceKind); err != nil {
		t.Fatal(err)
	}
	if cleanupStatus != "EXHAUSTED" || cleanupDiagnostic != diagnostic || observedRevision != 7 || currentRevision != 8 ||
		sourceStatus != string(store.JobFailed) || failureStatus != "PENDING" || sourceKind != store.PublishHumanHandoffJobKind {
		t.Errorf("terminal cleanup provenance = %s/%q %d->%d source %s failure %s/%s", cleanupStatus, cleanupDiagnostic, observedRevision, currentRevision, sourceStatus, failureStatus, sourceKind)
	}
	escalation, err := databases[0].ApplyNextWorkflowActionFailureEscalation(ctx, "failure-escalation", 5*time.Second)
	if err != nil || escalation == nil || !escalation.HandoffApplied || escalation.WorkflowRevision != 9 {
		t.Fatalf("cleanup failure escalation = (%#v, %v)", escalation, err)
	}
	var generated int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1
    AND kind IN ('PUBLISH_HUMAN_HANDOFF', 'RECONCILE_GITHUB_LABELS') AND id <> $2`, workflowID, job.ID).Scan(&generated); err != nil {
		t.Fatal(err)
	}
	if generated != 2 {
		t.Errorf("cleanup failure generated effects = %d, want 2", generated)
	}
}

func TestWorkflowGitHubEffectFinalLeaseExpiryCreatesHumanHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = 'DEVELOPING', state_revision = 2,
    desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE'
WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	job, _, err := databases[0].EnqueueJob(ctx, store.JobSpec{
		Queue: store.WorkflowActionQueue, Kind: store.ReconcileGitHubLabelsJobKind,
		Payload: json.RawMessage(`{"state":"DEVELOPING","revision":2}`), MaxAttempts: 1,
		IdempotencyKey: "label-final-expiry-50", WorkflowID: fixture.workflowID,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := databases[0].ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "crashing-label-worker", 25*time.Millisecond)
	if err != nil || lease == nil || lease.ID != job.ID {
		t.Fatalf("ClaimWorkflowGitHubEffectJob() = (%#v, %v)", lease, err)
	}
	time.Sleep(35 * time.Millisecond)
	if reclaimed, err := databases[0].ReclaimExpiredJobs(ctx, 10); err != nil || reclaimed != 1 {
		t.Fatalf("ReclaimExpiredJobs() = (%d, %v)", reclaimed, err)
	}
	escalation, err := databases[0].ApplyNextWorkflowActionFailureEscalation(ctx, "failure-escalation", 5*time.Second)
	if err != nil || escalation == nil || !escalation.HandoffApplied || escalation.WorkflowRevision != 3 {
		t.Fatalf("ApplyNextWorkflowActionFailureEscalation() = (%#v, %v)", escalation, err)
	}
	var state, assignmentStatus, sourceStatus string
	var revision, derived int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.desired_assignment_status, source.status,
       (SELECT count(*) FROM jobs WHERE workflow_id = workflow.id
           AND kind IN ('PUBLISH_HUMAN_HANDOFF', 'RECONCILE_GITHUB_LABELS') AND id <> source.id)
FROM workflows AS workflow JOIN jobs AS source ON source.id = $2
WHERE workflow.id = $1`, fixture.workflowID, job.ID).Scan(&state, &revision, &assignmentStatus, &sourceStatus, &derived); err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.StateNeedsHuman) || revision != 3 || assignmentStatus != string(workflow.AssignmentWaitingForHuman) || sourceStatus != string(store.JobFailed) || derived != 2 {
		t.Errorf("final expiry = Workflow %s@%d assignment %s source %s derived %d", state, revision, assignmentStatus, sourceStatus, derived)
	}
}

func TestWorkflowGitHubEffectClaimsSerializeSameKindPerWorkflow(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workflowA := "7c000000-0000-4000-8000-000000000001"
	workflowB := "7c000000-0000-4000-8000-000000000002"
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision
)
VALUES ($1, 201, 'acme', 'widgets-a', 202, 17, 'REVIEWING', 3),
       ($2, 211, 'acme', 'widgets-b', 212, 18, 'DEVELOPING', 1)`, workflowA, workflowB); err != nil {
		t.Fatalf("seed Workflows: %v", err)
	}
	enqueue := func(key, workflowID string, revision, priority int) store.Job {
		t.Helper()
		job, _, err := databases[0].EnqueueJob(ctx, store.JobSpec{
			Queue: store.WorkflowActionQueue, Kind: store.ReconcileGitHubLabelsJobKind,
			Payload:  json.RawMessage(fmt.Sprintf(`{"state":"DEVELOPING","revision":%d}`, revision)),
			Priority: priority, MaxAttempts: 3, IdempotencyKey: key, WorkflowID: workflowID,
		})
		if err != nil {
			t.Fatalf("EnqueueJob(%s) error = %v", key, err)
		}
		return job
	}
	oldA := enqueue("serialized-label-a-old", workflowA, 1, 20)
	newA := enqueue("serialized-label-a-new", workflowA, 2, 10)
	jobB := enqueue("serialized-label-b", workflowB, 1, 0)

	type claimResult struct {
		lease *store.JobLease
		err   error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	for index := range 2 {
		go func(database *store.Store, owner string) {
			<-start
			lease, err := database.ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, owner, 5*time.Second)
			results <- claimResult{lease: lease, err: err}
		}(databases[index], fmt.Sprintf("label-worker-%d", index))
	}
	close(start)
	claimed := make(map[string]*store.JobLease, 2)
	for range 2 {
		result := <-results
		if result.err != nil || result.lease == nil {
			t.Fatalf("concurrent ClaimWorkflowGitHubEffectJob() = (%#v, %v)", result.lease, result.err)
		}
		if previous := claimed[result.lease.WorkflowID]; previous != nil {
			t.Fatalf("same Workflow leased concurrently: %#v and %#v", previous, result.lease)
		}
		claimed[result.lease.WorkflowID] = result.lease
	}
	if claimed[workflowA] == nil || claimed[workflowA].ID != oldA.ID || claimed[workflowB] == nil || claimed[workflowB].ID != jobB.ID {
		t.Fatalf("concurrent claims = %#v, want oldest A %s and unrelated B %s", claimed, oldA.ID, jobB.ID)
	}
	if blocked, err := databases[2].ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "label-worker-2", 5*time.Second); err != nil || blocked != nil {
		t.Fatalf("claim while both Workflows have live label effects = (%#v, %v), want nil", blocked, err)
	}

	for workflowID, lease := range claimed {
		effect, err := databases[2].GetWorkflowGitHubEffectContext(ctx, *lease)
		if err != nil {
			t.Fatalf("GetWorkflowGitHubEffectContext(%s) error = %v", workflowID, err)
		}
		if workflowID == workflowA && (effect.JobRevision != 1 || effect.Revision != 3 || effect.State != workflow.StateReviewing) {
			t.Fatalf("older A job context = %#v, want latest durable revision 3", effect)
		}
		if _, err := databases[2].AcknowledgeWorkflowGitHubEffect(ctx, *lease, effect, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("acknowledge %s: %v", workflowID, err)
		}
	}
	next, err := databases[2].ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "label-worker-2", 5*time.Second)
	if err != nil || next == nil || next.ID != newA.ID {
		t.Fatalf("next serialized A claim = (%#v, %v), want %s", next, err, newA.ID)
	}
	effect, err := databases[2].GetWorkflowGitHubEffectContext(ctx, *next)
	if err != nil || effect.Revision != 3 || effect.State != workflow.StateReviewing {
		t.Fatalf("newer queued A job latest context = (%#v, %v)", effect, err)
	}
}
