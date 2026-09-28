//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestBeginTerminalCorroborationFencesStoppedTurnAndQueuesOneVerifier(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for index, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			_, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 951+index, role, "review-head")
			tool := "request_review"
			service := "omnigrex"
			resource := fmt.Sprintf("%d:%s", proposal.RepositoryID, proposal.HeadRef)
			request := json.RawMessage(`{"operation_id":"terminal-checkpoint","summary":"Ready"}`)
			result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
				proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
			if role == workflow.RoleReviewer {
				tool = "submit_review"
				service = "github"
				resource = fmt.Sprintf("%d:%d", proposal.RepositoryID, proposal.PullRequestID)
				request = json.RawMessage(`{"operation_id":"terminal-checkpoint","event":"APPROVE","body":"Ready","comments":[]}`)
				result = json.RawMessage(fmt.Sprintf(`{"review_id":%d,"node_id":"PRR_verifier","state":"APPROVED","commit_id":%q,"actor_id":%d,"html_url":"https://github.test/review"}`,
					int64(951+index)*1000+1, proposal.HeadSHA, (951+index)*100+2))
			}
			mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
				OperationID: "terminal-checkpoint", ToolName: tool,
				Request: request, ExternalService: service, ExternalResourceID: resource,
				ExpectedSHA: proposal.HeadSHA,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
				t.Fatal(err)
			}
			if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
				t.Fatal(err)
			}
			prompt := json.RawMessage(`{"stop_reason":"end_turn"}`)
			checkpoint, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID, prompt, "", "transient_transport")
			if err != nil {
				t.Fatalf("BeginTerminalCorroboration() error = %v", err)
			}
			if checkpoint.TurnID != lease.ID || checkpoint.SourceInvocationID != mutation.ID || checkpoint.VerificationJobID == "" || checkpoint.PendingSince.IsZero() {
				t.Fatalf("checkpoint = %#v", checkpoint)
			}
			again, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID, prompt, "", "transient_transport")
			if err != nil || again.VerificationJobID != checkpoint.VerificationJobID || !again.PendingSince.Equal(checkpoint.PendingSince) {
				t.Fatalf("idempotent checkpoint = (%#v, %v), original %#v", again, err, checkpoint)
			}
			if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID, nil, "FAILURE", "transient_transport"); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
				t.Fatalf("conflicting checkpoint error = %v", err)
			}
			if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID, prompt, "", "credentialshapedsecret"); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
				t.Fatalf("unrecognized failure code error = %v", err)
			}
			var turnStatus, executionJobStatus, verificationJobStatus, workflowStatus string
			var active bool
			var slots, settlements int
			err = pool.QueryRow(ctx, `
SELECT turn.status, turn.active, execution.status, verification.status,
       workflow.status,
       (SELECT count(*) FROM agent_turn_slots WHERE agent_turn_id = turn.id),
       (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = turn.id)
FROM agent_turns AS turn
JOIN jobs AS execution ON execution.id = $2
JOIN agent_turn_corroborations AS checkpoint ON checkpoint.agent_turn_id = turn.id
JOIN jobs AS verification ON verification.id = checkpoint.verification_job_id
JOIN workflows AS workflow ON workflow.id = checkpoint.workflow_id
WHERE turn.id = $1`, lease.ID, lease.JobLease.ID).Scan(
				&turnStatus, &active, &executionJobStatus, &verificationJobStatus,
				&workflowStatus, &slots, &settlements)
			if err != nil || turnStatus != string(store.AgentTurnCorroborating) || !active ||
				executionJobStatus != "SUCCEEDED" || verificationJobStatus != "AVAILABLE" ||
				slots != 0 || settlements != 0 ||
				role == workflow.RoleDeveloper && workflowStatus != string(workflow.StateDeveloping) ||
				role == workflow.RoleReviewer && workflowStatus != string(workflow.StateReviewing) {
				t.Fatalf("pending state = turn %s active %v execution %s verifier %s Workflow %s slots %d settlements %d; error %v",
					turnStatus, active, executionJobStatus, verificationJobStatus, workflowStatus, slots, settlements, err)
			}
			verificationLease, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind,
				"verify-terminal", time.Minute)
			if err != nil || verificationLease == nil || verificationLease.ID != checkpoint.VerificationJobID {
				t.Fatalf("claim verifier = (%#v, %v)", verificationLease, err)
			}
			substituted := *verificationLease
			substituted.AgentAssignmentID = "10000000-0000-4000-8000-000000000001"
			if _, err := database.GetTerminalCorroborationContext(ctx, substituted); !errors.Is(err, store.ErrJobLeaseLost) {
				t.Fatalf("substituted Assignment context error = %v", err)
			}
			context, err := database.GetTerminalCorroborationContext(ctx, *verificationLease)
			if err != nil || context.Checkpoint.TurnID != lease.ID || context.Execution.Turn.ID != lease.ID ||
				context.Execution.Assignment.Role != role || len(context.Mutations) != 1 || context.Mutations[0].ID != mutation.ID {
				t.Fatalf("read pending context = (%#v, %v)", context, err)
			}
			outcome := workflow.TurnOutcomeChangeProposalReady
			if role == workflow.RoleReviewer {
				outcome = workflow.TurnOutcomeApproved
			}
			observation := successfulSettlementObservation(outcome, proposal)
			observation.Completion.Outcome = prompt
			if role == workflow.RoleReviewer {
				actorID := int64((951+index)*100 + 2)
				observation.Review = &workflow.ReviewIdentity{
					ID: int64(951+index)*1000 + 1, NodeID: "PRR_verifier",
					ChangeProposalID: proposal.PullRequestID, ActorID: actorID, HeadSHA: proposal.HeadSHA,
				}
				observation.AuthorizedReviewerActorID = actorID
				unrelated := observation
				unrelatedReview := *observation.Review
				unrelatedReview.ID++
				unrelated.Review = &unrelatedReview
				if _, err := database.SettleTerminalCorroboration(ctx, *verificationLease, unrelated); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
					t.Fatalf("unrelated Reviewer evidence error = %v", err)
				}
			}
			wrongPrompt := observation
			wrongPrompt.Completion.Outcome = json.RawMessage(`{"stop_reason":"cancelled"}`)
			if _, err := database.SettleTerminalCorroboration(ctx, *verificationLease, wrongPrompt); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
				t.Fatalf("contradictory ACP completion error = %v", err)
			}
			staleLease := *verificationLease
			staleLease.LeaseToken = "10000000-0000-4000-8000-000000000001"
			if _, err := database.SettleTerminalCorroboration(ctx, staleLease, observation); !errors.Is(err, store.ErrJobLeaseLost) {
				t.Fatalf("stale verification lease error = %v", err)
			}
			settled, err := database.SettleTerminalCorroboration(ctx, *verificationLease, observation)
			if err != nil || settled.State == "" || settled.RecoveryJobID != verificationLease.ID {
				t.Fatalf("verified settlement = (%#v, %v)", settled, err)
			}
			if replayed, err := database.SettleTerminalCorroboration(ctx, *verificationLease, observation); err != nil || replayed.ID != settled.ID {
				t.Fatalf("replayed verifier settlement = (%#v, %v), original %#v", replayed, err, settled)
			}
		})
	}
}

func TestPromptEndingIsCheckpointedBeforeRuntimeCleanup(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 995, workflow.RoleDeveloper, "")
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatalf("repeat identical ACP ending: %v", err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "cancelled", ""); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
		t.Fatalf("contradictory ACP ending error = %v", err)
	}
	var stop, promptError string
	var recorded bool
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(prompt_stop_reason, ''), COALESCE(prompt_error_class, ''),
       prompt_recorded_at IS NOT NULL FROM agent_turns WHERE id = $1`, lease.ID).Scan(&stop, &promptError, &recorded); err != nil {
		t.Fatal(err)
	}
	if stop != "end_turn" || promptError != "" || !recorded {
		t.Fatalf("stored ACP classification = %q/%q recorded %t", stop, promptError, recorded)
	}
	if _, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "after-end-turn", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	}); !errors.Is(err, store.ErrMutationAdmissionClosed) {
		t.Fatalf("mutation admitted after recorded ACP ending: %v", err)
	}
}

func TestKnownACPCompletionSurvivesCleanupFailureAndCorroboratesAfterStop(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 996
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "ready-before-cleanup-failed", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"ready-before-cleanup-failed","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", number, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginAgentTurnRecovery(ctx, lease); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-after-prompt")
	mutationJob := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "reconcile-after-prompt")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if candidates, err := database.ListAgentTurnMutationsForReconciliation(ctx, mutationJob); err != nil || len(candidates) != 0 {
		t.Fatalf("eligible terminal intent must defer successful artifact reads to verifier: %d candidates, error %v", len(candidates), err)
	}
	if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, mutationJob); err != nil {
		t.Fatalf("complete stopped Turn's mutation barrier: %v", err)
	}
	recovery, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil || recovery.Status != store.AgentTurnCorroborating || recovery.SuccessorAllowed ||
		recovery.RecoverySettledAt == nil || recovery.SettlementID != "" {
		t.Fatalf("original Turn after stopped Runtime Process = (%#v, %v)", recovery, err)
	}
	var checkpoints, verifierJobs, successors int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_turn_corroborations WHERE agent_turn_id = $1 AND state = 'PENDING'),
       (SELECT count(*) FROM jobs WHERE agent_turn_id = $1 AND kind = 'VERIFY_TERMINAL_INTENT' AND status = 'AVAILABLE'),
       (SELECT count(*) FROM jobs WHERE workflow_id = $2 AND kind = 'PREPARE_AGENT_TURN' AND status = 'AVAILABLE')`,
		lease.ID, fixture.workflowID).Scan(&checkpoints, &verifierJobs, &successors); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 1 || verifierJobs != 1 || successors != 0 {
		t.Fatalf("stopped Turn queued %d checkpoints, %d verifier jobs, %d new Agent Turns", checkpoints, verifierJobs, successors)
	}
	api := &replayOutcomeGitHub{pullRequest: &githubapi.PullRequest{
		ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
		State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
	}}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{paths: cleanCorroborationPaths(t)}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "verify-stopped-original", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || api.calls != 1 {
		t.Fatalf("verify stopped original Turn = processed %t, error %v, fresh reads %d", processed, err, api.calls)
	}
	var workflowStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowStatus); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StateReviewing) {
		t.Fatalf("stopped original Turn advanced Workflow to %s, want REVIEWING", workflowStatus)
	}
}

func TestStoppedTerminalRecoveryStillReconcilesSuccessfulIssueComment(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 1001
	_, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	terminal, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "ready-after-comment", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"ready-after-comment","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", number, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, terminal.ID, json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))); err != nil {
		t.Fatal(err)
	}
	comment, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "comment-before-cleanup", ToolName: "comment_on_issue",
		Request:         json.RawMessage(`{"operation_id":"comment-before-cleanup","body":"Done"}`),
		ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", number, number),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, comment.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, comment.ID, json.RawMessage(`{"comment_id":100101}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginAgentTurnRecovery(ctx, lease); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-after-comment")
	mutationJob := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "verify-comment")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	candidates, err := database.ListAgentTurnMutationsForReconciliation(ctx, mutationJob)
	if err != nil || len(candidates) != 1 || candidates[0].ID != comment.ID {
		t.Fatalf("ordinary successful side effect must remain in recovery barrier: %#v, error %v", candidates, err)
	}
}

func TestRecordedACPCompletionSurvivesExecutionLeaseExpiry(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 997
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "ready-before-process-death", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"ready-before-process-death","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", number, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	for _, expiration := range []struct {
		query string
		args  []any
	}{
		{`UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, []any{lease.JobLease.ID}},
		{`UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1 AND attempt_number = $2`, []any{lease.JobLease.ID, lease.JobLease.Attempt}},
		{`UPDATE agent_turns SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, []any{lease.ID}},
		{`UPDATE agent_turn_slots SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE agent_turn_id = $1`, []any{lease.ID}},
	} {
		if _, err := pool.Exec(ctx, expiration.query, expiration.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-expired-prompt")
	mutationJob := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "reconcile-expired-prompt")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if candidates, err := database.ListAgentTurnMutationsForReconciliation(ctx, mutationJob); err != nil || len(candidates) != 0 {
		t.Fatalf("eligible expired intent must defer artifact reads to verifier: %#v, error %v", candidates, err)
	}
	if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, mutationJob); err != nil {
		t.Fatal(err)
	}
	recovery, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil || recovery.Status != store.AgentTurnCorroborating || recovery.SuccessorAllowed {
		t.Fatalf("recover previously recorded ACP ending = (%#v, %v)", recovery, err)
	}
	var executionStatus, continuation string
	if err := pool.QueryRow(ctx, `
SELECT execution.status, turn.recovery_continuation
FROM agent_turns AS turn JOIN jobs AS execution ON execution.agent_turn_id = turn.id
  AND execution.kind = 'RUN_AGENT_TURN' WHERE turn.id = $1`, lease.ID).Scan(&executionStatus, &continuation); err != nil {
		t.Fatal(err)
	}
	if executionStatus != "FAILED" || continuation != "TERMINAL_CORROBORATION_PENDING" {
		t.Fatalf("expired execution = Job %s, continuation %s", executionStatus, continuation)
	}
	api := &replayOutcomeGitHub{pullRequest: &githubapi.PullRequest{
		ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
		State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
	}}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{paths: cleanCorroborationPaths(t)}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "verify-expired-original", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || api.calls != 1 {
		t.Fatalf("verify expired original Turn = processed %t, error %v, fresh reads %d", processed, err, api.calls)
	}
	var workflowStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT recovery_continuation FROM agent_turns WHERE id = $1`, lease.ID).Scan(&continuation); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StateReviewing) || continuation != "TERMINAL_CORROBORATION_APPLIED" {
		t.Fatalf("expired Turn result = Workflow %s, continuation %s", workflowStatus, continuation)
	}
}

func TestRecordedACPRefusalCannotBecomeTerminalRecoveryOrConfirmation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 998
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	spec := store.MutationSpec{
		OperationID: "review-before-refusal", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"review-before-refusal","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", number, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	}
	mutation, err := database.ReserveMutation(ctx, lease, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "refusal", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport"); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
		t.Fatalf("refused ACP ending was replaced by end_turn checkpoint: %v", err)
	}
	if _, err := database.BeginAgentTurnRecovery(ctx, lease); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-refused-prompt")
	mutationJob := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "reconcile-refused-prompt")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if candidates, err := database.ListAgentTurnMutationsForReconciliation(ctx, mutationJob); err != nil || len(candidates) != 1 || candidates[0].ID != mutation.ID {
		t.Fatalf("refused intent retained ordinary mutation recovery: %#v, error %v", candidates, err)
	}
	if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, mutationJob); err != nil {
		t.Fatal(err)
	}
	recovery, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil || recovery.Status != store.AgentTurnInterrupted || !recovery.SuccessorAllowed || recovery.SettlementID == "" {
		t.Fatalf("refused prompt recovery = (%#v, %v)", recovery, err)
	}
	var checkpoints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turn_corroborations WHERE agent_turn_id = $1`, lease.ID).Scan(&checkpoints); err != nil || checkpoints != 0 {
		t.Fatalf("refused prompt created %d pending checkpoints, error %v", checkpoints, err)
	}
	var successorJobID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE agent_turn_settlement_id = $1 AND kind = 'PREPARE_AGENT_TURN'`,
		recovery.SettlementID).Scan(&successorJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, successorJobID); err != nil {
		t.Fatal(err)
	}
	retrySpec := fixture.turnSpec()
	retrySpec.Purpose, retrySpec.RetryOfTurnID = workflow.TurnPurposeRetry, lease.ID
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, retrySpec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"refused-prompt-retry", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil || execution.PriorTerminalIntent != nil {
		t.Fatalf("refused prompt offered confirmation = (%#v, %v)", execution.PriorTerminalIntent, err)
	}
	if _, err := database.GetConfirmablePriorTerminalIntent(ctx, retryLease, mutation.ID); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("refused prompt confirmation error = %v", err)
	}
	if _, err := database.ReserveMutation(ctx, retryLease, spec); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("refused prompt exact replay error = %v", err)
	}
}

func TestIssueClosureCancelsRecoveredPendingTurnWithFailedExecutionJob(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 999
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "ready-before-closed-issue", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"ready-before-closed-issue","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", number, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	for _, expiration := range []struct {
		query string
		args  []any
	}{
		{`UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, []any{lease.JobLease.ID}},
		{`UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1 AND attempt_number = $2`, []any{lease.JobLease.ID, lease.JobLease.Attempt}},
		{`UPDATE agent_turns SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, []any{lease.ID}},
		{`UPDATE agent_turn_slots SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE agent_turn_id = $1`, []any{lease.ID}},
	} {
		if _, err := pool.Exec(ctx, expiration.query, expiration.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-before-closure")
	mutations := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "reconcile-before-closure")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	delivery := workflowDelivery("95000000-0000-4000-8000-000000000999")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = number, number, number
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: number, IssueID: number, IssueNumber: number},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.IssueClosedEvent{EventMetadata: eventContext.Metadata,
				ClosureID: "closure-999", RetainUntil: time.Now().UTC().Add(24 * time.Hour),
				RetentionToken: "retain-999"}, nil
		})
	if err != nil || application.State != workflow.StateClosing {
		t.Fatalf("close recovered pending Workflow = (%#v, %v)", application, err)
	}
	closureStop, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.StopAgentTurnJobKind,
		"close-recovered-pending", time.Minute)
	if err != nil || closureStop == nil {
		t.Fatalf("claim closure stop = (%#v, %v)", closureStop, err)
	}
	if _, err := database.AcknowledgeClosureTurnStopped(ctx, *closureStop); err != nil {
		t.Fatal(err)
	}
	closureSettlement, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.SettleClosureJobKind,
		"settle-recovered-pending", time.Minute)
	if err != nil || closureSettlement == nil {
		t.Fatalf("claim closure settlement = (%#v, %v)", closureSettlement, err)
	}
	if _, err := database.CompleteClosureSettlement(ctx, *closureSettlement); err != nil {
		t.Fatalf("settle recovered Turn closure: %v", err)
	}
	var checkpointStatus, verifierStatus, executionStatus, workflowStatus string
	if err := pool.QueryRow(ctx, `
SELECT checkpoint.state, verifier.status, execution.status, workflow.status
FROM agent_turn_corroborations AS checkpoint
JOIN jobs AS verifier ON verifier.id = checkpoint.verification_job_id
JOIN jobs AS execution ON execution.id = checkpoint.execution_job_id
JOIN workflows AS workflow ON workflow.id = checkpoint.workflow_id
WHERE checkpoint.agent_turn_id = $1`, lease.ID).Scan(
		&checkpointStatus, &verifierStatus, &executionStatus, &workflowStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != "CANCELLED" || verifierStatus != "CANCELLED" ||
		executionStatus != "FAILED" || workflowStatus != string(workflow.StateClosed) {
		t.Fatalf("closed recovered Turn = checkpoint %s, verifier %s, execution %s, Workflow %s (%s)",
			checkpointStatus, verifierStatus, executionStatus, workflowStatus, fixture.workflowID)
	}
}

func TestIssueClosureCancelsPendingTerminalCorroboration(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 963
	fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "review-before-closure", ToolName: "request_review",
		Request: json.RawMessage(`{"operation_id":"review-before-closure"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"outcome":"REVIEW_REQUESTED"}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport")
	if err != nil {
		t.Fatal(err)
	}
	delivery := workflowDelivery("95000000-0000-4000-8000-000000000963")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = number, number, number
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: number, IssueID: number, IssueNumber: number},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.IssueClosedEvent{
				EventMetadata: eventContext.Metadata, ClosureID: "closure-963",
				RetainUntil: time.Now().UTC().Add(24 * time.Hour), RetentionToken: "retention-963",
			}, nil
		})
	if err != nil || application.State != workflow.StateClosing {
		t.Fatalf("close pending Workflow = (%#v, %v)", application, err)
	}
	stop, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.StopAgentTurnJobKind, "stop-pending", time.Minute)
	if err != nil || stop == nil {
		t.Fatalf("claim closure stop = (%#v, %v)", stop, err)
	}
	if _, err := database.AcknowledgeClosureTurnStopped(ctx, *stop); err != nil {
		t.Fatalf("acknowledge closure stop: %v", err)
	}
	settlement, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.SettleClosureJobKind, "settle-pending", time.Minute)
	if err != nil || settlement == nil {
		t.Fatalf("claim closure settlement = (%#v, %v)", settlement, err)
	}
	if _, err := database.CompleteClosureSettlement(ctx, *settlement); err != nil {
		t.Fatalf("settle closure while corroborating: %v", err)
	}
	var checkpointState, verifierStatus, turnStatus, workflowStatus string
	if err := pool.QueryRow(ctx, `
SELECT checkpoint.state, verification.status, turn.status, workflow.status
FROM agent_turn_corroborations AS checkpoint
JOIN jobs AS verification ON verification.id = checkpoint.verification_job_id
JOIN agent_turns AS turn ON turn.id = checkpoint.agent_turn_id
JOIN workflows AS workflow ON workflow.id = checkpoint.workflow_id
WHERE checkpoint.agent_turn_id = $1 AND verification.id = $2`, lease.ID, checkpoint.VerificationJobID).Scan(
		&checkpointState, &verifierStatus, &turnStatus, &workflowStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointState != "CANCELLED" || verifierStatus != "CANCELLED" ||
		turnStatus != string(store.AgentTurnInterrupted) || workflowStatus != string(workflow.StateClosed) {
		t.Fatalf("closure = checkpoint %s verifier %s turn %s Workflow %s (source %s)",
			checkpointState, verifierStatus, turnStatus, workflowStatus, fixture.workflowID)
	}
}

func TestTerminalCorroborationExhaustionHandsOffWithoutAgentRetry(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 971, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "handoff-intent", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"handoff-intent","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", 971, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport"); err != nil {
		t.Fatal(err)
	}
	verifier, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind,
		"verify-exhausted", time.Minute)
	if err != nil || verifier == nil {
		t.Fatalf("claim verifier = (%#v, %v)", verifier, err)
	}
	settled, err := database.CompleteTerminalCorroborationHandoff(ctx, *verifier,
		workflow.ReasonTerminalCorroborationExhausted, "transient_transport")
	if err != nil || settled.Reason != workflow.ReasonTerminalCorroborationExhausted ||
		settled.State != workflow.StateNeedsHuman || settled.SuccessorJobID != "" {
		t.Fatalf("corroboration handoff = (%#v, %v)", settled, err)
	}
	if replayed, err := database.CompleteTerminalCorroborationHandoff(ctx, *verifier,
		workflow.ReasonTerminalCorroborationExhausted, "transient_transport"); err != nil || replayed.ID != settled.ID {
		t.Fatalf("replayed handoff = (%#v, %v)", replayed, err)
	}
	var failures int
	var checkpointState, workflowReason string
	if err := pool.QueryRow(ctx, `
SELECT attempt.infrastructure_failures, checkpoint.state, workflow.human_handoff_reason
FROM workflow_attempts AS attempt
JOIN agent_turn_corroborations AS checkpoint ON checkpoint.workflow_attempt_id = attempt.id
JOIN workflows AS workflow ON workflow.id = checkpoint.workflow_id
WHERE attempt.id = $1`, fixture.attemptID).Scan(&failures, &checkpointState, &workflowReason); err != nil {
		t.Fatal(err)
	}
	if failures != 0 || checkpointState != "HANDED_OFF" || workflowReason != string(workflow.ReasonTerminalCorroborationExhausted) {
		t.Fatalf("handoff durability = failures %d checkpoint %s reason %s", failures, checkpointState, workflowReason)
	}
	var controlRevision int64
	if err := pool.QueryRow(ctx, `SELECT control_revision FROM agent_sessions WHERE id = $1`, lease.AgentSessionID).Scan(&controlRevision); err != nil {
		t.Fatal(err)
	}
	humanSession, err := database.TransferAgentSessionControl(ctx, lease.AgentSessionID, controlRevision,
		store.SessionControlHuman, "human-before-revalidation")
	if err != nil {
		t.Fatal(err)
	}
	blockedDelivery := workflowDelivery("95000000-0000-4000-8000-000000000970")
	blockedDelivery.RepositoryID, blockedDelivery.IssueID, blockedDelivery.IssueNumber = 971, 971, 971
	blockedDelivery.RepositoryOwner, blockedDelivery.RepositoryName = "owner", "repo"
	blockedClaim := claimWorkflowDelivery(t, database, ctx, blockedDelivery)
	blockedApplication, err := database.CompleteWebhookTransition(ctx, blockedClaim.DeliveryID, blockedClaim.ClaimToken,
		normalizedPayload(blockedClaim.DeliveryID, "labeled"),
		store.WorkflowLocator{RepositoryID: 971, IssueID: 971, IssueNumber: 971},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: eventContext.Metadata,
				AttemptID:     "95000000-0000-4000-8000-000000000972",
				AttemptNumber: eventContext.Snapshot.LastAttemptNumber + 1}, nil
		})
	if err != nil || blockedApplication.State != workflow.StateNeedsHuman ||
		blockedApplication.Reason != workflow.ReasonTerminalRevalidationHumanControl {
		t.Fatalf("human-owned reactivation = (%#v, %v)", blockedApplication, err)
	}
	var attemptCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1`, fixture.workflowID).Scan(&attemptCount); err != nil || attemptCount != 1 {
		t.Fatalf("human-owned reactivation created %d Attempts, error %v", attemptCount, err)
	}
	if _, err := database.TransferAgentSessionControl(ctx, lease.AgentSessionID, humanSession.ControlRevision,
		store.SessionControlAutomation, "automation-after-revalidation"); err != nil {
		t.Fatal(err)
	}
	delivery := workflowDelivery("95000000-0000-4000-8000-000000000971")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 971, 971, 971
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "labeled"),
		store.WorkflowLocator{RepositoryID: 971, IssueID: 971, IssueNumber: 971},
		func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{
				EventMetadata: context.Metadata,
				AttemptID:     "95000000-0000-4000-8000-000000000972", AttemptNumber: context.Snapshot.LastAttemptNumber + 1,
			}, nil
		})
	if err != nil || application.State != workflow.StateDeveloping {
		t.Fatalf("reactivate eligible terminal intent = (%#v, %v)", application, err)
	}
	var verifierJobs, preparationJobs int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind = 'REVALIDATE_TERMINAL_INTENT'),
       count(*) FILTER (WHERE kind = 'PREPARE_AGENT_TURN')
FROM jobs WHERE workflow_attempt_id = $1`, "95000000-0000-4000-8000-000000000972").Scan(
		&verifierJobs, &preparationJobs); err != nil {
		t.Fatal(err)
	}
	if verifierJobs != 1 || preparationJobs != 0 {
		t.Fatalf("reactivation queued %d verifier Jobs and %d Agent Turn preparations", verifierJobs, preparationJobs)
	}
	firstRevalidation, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.RevalidateTerminalIntentJobKind,
		"first-human-revalidation", time.Minute)
	if err != nil || firstRevalidation == nil {
		t.Fatalf("claim first human revalidation = (%#v, %v)", firstRevalidation, err)
	}
	if err := database.RetryTerminalCorroboration(ctx, *firstRevalidation, "github_http_error", 0); err != nil {
		t.Fatalf("retry first revalidation with a new failure code: %v", err)
	}
	firstRevalidation, err = database.ClaimJobKind(ctx, "agent-turn-recovery", store.RevalidateTerminalIntentJobKind,
		"first-human-revalidation-retry", time.Minute)
	if err != nil || firstRevalidation == nil || firstRevalidation.Attempt != 2 {
		t.Fatalf("claim retried revalidation = (%#v, %v)", firstRevalidation, err)
	}
	readBack, err := database.GetTerminalCorroborationContext(ctx, *firstRevalidation)
	if err != nil || readBack.Checkpoint.LastFailureCode != "github_http_error" ||
		!readBack.Checkpoint.PendingSince.Equal(firstRevalidation.CreatedAt) {
		t.Fatalf("revalidation category/window = (%q, %s, %v), want github_http_error and %s",
			readBack.Checkpoint.LastFailureCode, readBack.Checkpoint.PendingSince, err, firstRevalidation.CreatedAt)
	}
	if err := database.CompleteTerminalRevalidationHandoff(ctx, *firstRevalidation,
		workflow.ReasonTerminalCorroborationExhausted, readBack.Checkpoint.LastFailureCode); err != nil {
		t.Fatalf("first human revalidation exhaustion: %v", err)
	}
	secondDelivery := workflowDelivery("95000000-0000-4000-8000-000000000973")
	secondDelivery.RepositoryID, secondDelivery.IssueID, secondDelivery.IssueNumber = 971, 971, 971
	secondDelivery.RepositoryOwner, secondDelivery.RepositoryName = "owner", "repo"
	secondClaim := claimWorkflowDelivery(t, database, ctx, secondDelivery)
	const secondAttemptID = "95000000-0000-4000-8000-000000000974"
	if _, err := database.CompleteWebhookTransition(ctx, secondClaim.DeliveryID, secondClaim.ClaimToken,
		normalizedPayload(secondClaim.DeliveryID, "labeled"),
		store.WorkflowLocator{RepositoryID: 971, IssueID: 971, IssueNumber: 971},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: eventContext.Metadata,
				AttemptID: secondAttemptID, AttemptNumber: eventContext.Snapshot.LastAttemptNumber + 1}, nil
		}); err != nil {
		t.Fatalf("second human reactivation: %v", err)
	}
	secondRevalidation, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.RevalidateTerminalIntentJobKind,
		"second-human-revalidation", time.Minute)
	if err != nil || secondRevalidation == nil {
		t.Fatalf("claim second human revalidation = (%#v, %v)", secondRevalidation, err)
	}
	if err := database.CompleteTerminalRevalidationHandoff(ctx, *secondRevalidation,
		workflow.ReasonTerminalCorroborationExhausted, "transient_transport"); err != nil {
		t.Fatalf("second human revalidation exhaustion: %v", err)
	}
	var handoffJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF'`,
		fixture.workflowID).Scan(&handoffJobs); err != nil || handoffJobs != 4 {
		t.Fatalf("four distinct handoff jobs = %d, error %v", handoffJobs, err)
	}
	thirdDelivery := workflowDelivery("95000000-0000-4000-8000-000000000975")
	thirdDelivery.RepositoryID, thirdDelivery.IssueID, thirdDelivery.IssueNumber = 971, 971, 971
	thirdDelivery.RepositoryOwner, thirdDelivery.RepositoryName = "owner", "repo"
	thirdClaim := claimWorkflowDelivery(t, database, ctx, thirdDelivery)
	const thirdAttemptID = "95000000-0000-4000-8000-000000000976"
	if _, err := database.CompleteWebhookTransition(ctx, thirdClaim.DeliveryID, thirdClaim.ClaimToken,
		normalizedPayload(thirdClaim.DeliveryID, "labeled"),
		store.WorkflowLocator{RepositoryID: 971, IssueID: 971, IssueNumber: 971},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: eventContext.Metadata,
				AttemptID: thirdAttemptID, AttemptNumber: eventContext.Snapshot.LastAttemptNumber + 1}, nil
		}); err != nil {
		t.Fatalf("third human reactivation: %v", err)
	}
	paths := cleanCorroborationPaths(t)
	api := &replayOutcomeGitHub{pullRequest: &githubapi.PullRequest{
		ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
		State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
	}}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "installation-token"}
	trace := &corroborationTracingStore{Store: database}
	worker, err := agentturn.NewTerminalCorroborationWorker(trace, outcomes, credential, credential,
		corroborationTestPaths{paths: paths}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "revalidate-after-handoff", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed {
		t.Fatalf("revalidate prior terminal intent = (%t, %v)", processed, err)
	}
	var newState, originalReason string
	var internalEvents, reviewerJobs int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, original.reason,
       (SELECT count(*) FROM workflow_internal_events WHERE workflow_id = workflow.id
           AND kind = 'TERMINAL_INTENT_REVALIDATED' AND applied_at IS NOT NULL),
       (SELECT count(*) FROM jobs WHERE workflow_attempt_id = $2 AND kind = 'PREPARE_AGENT_TURN')
FROM workflows AS workflow JOIN agent_turn_settlements AS original ON original.agent_turn_id = $3
WHERE workflow.id = $1`, fixture.workflowID, thirdAttemptID, lease.ID).Scan(
		&newState, &originalReason, &internalEvents, &reviewerJobs); err != nil {
		t.Fatal(err)
	}
	if newState != string(workflow.StateReviewing) || originalReason != string(workflow.ReasonTerminalCorroborationExhausted) ||
		internalEvents != 1 || reviewerJobs != 1 {
		var verifierStatus, verifierError string
		_ = pool.QueryRow(ctx, `SELECT status, COALESCE(last_error, '') FROM jobs WHERE workflow_attempt_id = $1 AND kind = 'REVALIDATE_TERMINAL_INTENT'`,
			thirdAttemptID).Scan(&verifierStatus, &verifierError)
		t.Fatalf("revalidated = Workflow %s, original settlement %s, internal events %d, Reviewer jobs %d, verifier %s/%s, settlement error %v",
			newState, originalReason, internalEvents, reviewerJobs, verifierStatus, verifierError, trace.settleErr)
	}
}

type corroborationTestPaths struct{ paths workspace.Paths }

func (paths corroborationTestPaths) Paths(string) (workspace.Paths, error) { return paths.paths, nil }

type corroborationTracingStore struct {
	*store.Store
	settleErr error
}

func (database *corroborationTracingStore) SettleTerminalRevalidation(ctx context.Context, lease store.JobLease, revision int64, observation store.AgentTurnSettlementObservation) error {
	database.settleErr = database.Store.SettleTerminalRevalidation(ctx, lease, revision, observation)
	return database.settleErr
}

func cleanCorroborationPaths(t *testing.T) workspace.Paths {
	t.Helper()
	root := t.TempDir()
	paths := workspace.Paths{Workspace: filepath.Join(root, "workspace"), Publication: filepath.Join(root, "publication")}
	for _, path := range []string{paths.Workspace, paths.Publication} {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("same\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

type corroborationReviewGitHub struct {
	pullRequest githubapi.PullRequest
	review      githubapi.Review
	reads       int
	getErr      error
}

func (api *corroborationReviewGitHub) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	api.reads++
	if api.getErr != nil {
		return githubapi.PullRequest{}, api.getErr
	}
	return api.pullRequest, nil
}

func (api *corroborationReviewGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	api.reads++
	return []githubapi.Review{api.review}, nil
}

func TestTerminalCorroborationWorkerSettlesOriginalDeveloperWithoutAnotherPrompt(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 973
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "ready-before-outage", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"ready-before-outage","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", number, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport"); err != nil {
		t.Fatal(err)
	}
	if _, recovered, err := databases[1].ClaimAndRecoverExpiredAgentTurn(ctx); err != nil || recovered {
		t.Fatalf("fresh Store must not treat pending corroboration as an expired prompt: recovered %t, error %v", recovered, err)
	}
	root := t.TempDir()
	paths := workspace.Paths{Workspace: filepath.Join(root, "workspace"), Publication: filepath.Join(root, "publication")}
	for _, path := range []string{paths.Workspace, paths.Publication} {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("same\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	github := &corroborationReviewGitHub{pullRequest: githubapi.PullRequest{
		ID: proposal.PullRequestID, NodeID: proposal.PullRequestNodeID, Number: int(proposal.PullRequestNumber),
		State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
	}, getErr: &githubapi.RateLimitError{APIError: &githubapi.APIError{StatusCode: 403}, RetryAfter: time.Minute}}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: databases[1], GitHub: github})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(databases[1], outcomes, credential, credential,
		corroborationTestPaths{paths: paths}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "verify-original", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || github.reads != 1 || credential.calls != 1 {
		t.Fatalf("rate-limited verification = (processed %t, error %v, GitHub calls %d, credentials %d)",
			processed, err, github.reads, credential.calls)
	}
	var scheduledAt time.Time
	if err := pool.QueryRow(ctx, `
SELECT available_at FROM jobs WHERE workflow_id = $1 AND kind = 'VERIFY_TERMINAL_INTENT' AND status = 'AVAILABLE'`,
		fixture.workflowID).Scan(&scheduledAt); err != nil || time.Until(scheduledAt) < 50*time.Second {
		t.Fatalf("rate-limit delay = %s, error %v", time.Until(scheduledAt), err)
	}
	github.getErr = nil
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = clock_timestamp()
WHERE workflow_id = $1 AND kind = 'VERIFY_TERMINAL_INTENT' AND status = 'AVAILABLE'`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	processed, err = worker.ProcessOne(ctx)
	if err != nil || !processed || github.reads != 2 || credential.calls != 2 {
		t.Fatalf("verified after rate limit = (processed %t, error %v, GitHub calls %d, credentials %d)",
			processed, err, github.reads, credential.calls)
	}
	var workflowStatus, turnStatus string
	var settlements, successors int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, turn.status,
       (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = turn.id),
       (SELECT count(*) FROM jobs AS successor JOIN agent_turn_settlements AS settlement
           ON successor.agent_turn_settlement_id = settlement.id
           WHERE settlement.agent_turn_id = turn.id AND successor.kind = 'PREPARE_AGENT_TURN')
FROM workflows AS workflow JOIN agent_turns AS turn ON turn.workflow_id = workflow.id
WHERE workflow.id = $1 AND turn.id = $2`, fixture.workflowID, lease.ID).Scan(
		&workflowStatus, &turnStatus, &settlements, &successors); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StateReviewing) || turnStatus != string(store.AgentTurnSucceeded) || settlements != 1 || successors != 1 {
		t.Fatalf("verified original = Workflow %s, Turn %s, settlements %d, Reviewer successors %d",
			workflowStatus, turnStatus, settlements, successors)
	}
}

func TestTerminalCorroborationWorkerAcceptsExistingReviewerReviewOnce(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 974
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleReviewer, "review-head")
	if alreadySubmitted, err := database.HasPriorSuccessfulReviewForTurn(ctx, lease); err != nil || alreadySubmitted {
		t.Fatalf("before first submit_review = (%t, %v)", alreadySubmitted, err)
	}
	const reviewID = 97401
	const actorID = 97402
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "submitted-before-outage", ToolName: "submit_review",
		Request:         json.RawMessage(`{"operation_id":"submitted-before-outage","event":"REQUEST_CHANGES","body":"Please adjust","comments":[]}`),
		ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", number, proposal.PullRequestID),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"review_id":%d,"node_id":"PRR_existing","state":"CHANGES_REQUESTED","commit_id":%q,"actor_id":%d,"html_url":"https://github.test/review"}`,
		reviewID, proposal.HeadSHA, actorID))
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if alreadySubmitted, err := database.HasPriorSuccessfulReviewForTurn(ctx, lease); err != nil || !alreadySubmitted {
		t.Fatalf("second submit_review in same Turn must be blocked = (%t, %v)", alreadySubmitted, err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "rate_limited"); err != nil {
		t.Fatal(err)
	}
	api := &corroborationReviewGitHub{
		pullRequest: githubapi.PullRequest{
			ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
			State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
			Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
		},
		review: githubapi.Review{ID: reviewID, NodeID: "PRR_existing", State: "CHANGES_REQUESTED",
			CommitID: proposal.HeadSHA, User: githubapi.User{ID: actorID}},
	}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "reviewer-installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "verify-review", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || api.reads != 2 || credential.calls != 1 {
		t.Fatalf("corroborate Reviewer review = processed %t, error %v, GitHub reads %d, credentials %d",
			processed, err, api.reads, credential.calls)
	}
	var state string
	var reviewCount, reviewUsage int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, (SELECT count(*) FROM change_proposal_reviews WHERE review_id = $2),
       COALESCE((attempt.review_usage->>'review')::int, 0)
FROM workflows AS workflow JOIN workflow_attempts AS attempt ON attempt.workflow_id = workflow.id AND attempt.active
WHERE workflow.id = $1`, fixture.workflowID, reviewID).Scan(&state, &reviewCount, &reviewUsage); err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.StateDeveloping) || reviewCount != 1 || reviewUsage != 1 {
		t.Fatalf("accepted review = Workflow %s, review identities %d, review cycles %d", state, reviewCount, reviewUsage)
	}
}

func TestTerminalReviewerRevalidationAfterHumanTriggerCountsOneCycle(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number, reviewID, actorID = 975, 97501, 97502
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleReviewer, "review-head")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "review-before-access-loss", ToolName: "submit_review",
		Request:         json.RawMessage(`{"operation_id":"review-before-access-loss","event":"REQUEST_CHANGES","body":"Please adjust","comments":[]}`),
		ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", number, proposal.PullRequestID),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"review_id":%d,"node_id":"PRR_revalidated","state":"CHANGES_REQUESTED","commit_id":%q,"actor_id":%d,"html_url":"https://github.test/review"}`,
		reviewID, proposal.HeadSHA, actorID))
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "permission_denied"); err != nil {
		t.Fatal(err)
	}
	verifier, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind, "verify-review-prerequisite", time.Minute)
	if err != nil || verifier == nil {
		t.Fatalf("claim verifier = (%#v, %v)", verifier, err)
	}
	if _, err := database.CompleteTerminalCorroborationHandoff(ctx, *verifier,
		workflow.ReasonTerminalCorroborationPrerequisite, "permission_denied"); err != nil {
		t.Fatal(err)
	}
	delivery := workflowDelivery("95000000-0000-4000-8000-000000000975")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = number, number, number
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	const newAttemptID = "95000000-0000-4000-8000-000000000976"
	if _, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "labeled"),
		store.WorkflowLocator{RepositoryID: number, IssueID: number, IssueNumber: number},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{
				EventMetadata: eventContext.Metadata, AttemptID: newAttemptID,
				AttemptNumber: eventContext.Snapshot.LastAttemptNumber + 1,
			}, nil
		}); err != nil {
		t.Fatal(err)
	}
	var controlRevision int64
	if err := pool.QueryRow(ctx, `SELECT control_revision FROM agent_sessions WHERE id = $1`, lease.AgentSessionID).Scan(&controlRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := database.TransferAgentSessionControl(ctx, lease.AgentSessionID, controlRevision, store.SessionControlHuman,
		"human-while-revalidating"); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Fatalf("human control during revalidation error = %v", err)
	}
	api := &corroborationReviewGitHub{
		pullRequest: githubapi.PullRequest{
			ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
			State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
			Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
		},
		review: githubapi.Review{ID: reviewID, NodeID: "PRR_revalidated", State: "CHANGES_REQUESTED",
			CommitID: proposal.HeadSHA, User: githubapi.User{ID: actorID}},
	}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "reviewer-installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "revalidate-review", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || api.reads != 2 {
		t.Fatalf("revalidate existing review = processed %t, error %v, GitHub reads %d", processed, err, api.reads)
	}
	var state string
	var acceptedReviews, reviewUsage, internalEvents, developerJobs int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status,
       (SELECT count(*) FROM change_proposal_reviews WHERE review_id = $2 AND accepted),
       COALESCE((attempt.review_usage->>'review')::int, 0),
       (SELECT count(*) FROM workflow_internal_events WHERE workflow_id = workflow.id AND kind = 'TERMINAL_INTENT_REVALIDATED'),
       (SELECT count(*) FROM jobs WHERE workflow_attempt_id = $3 AND kind = 'PREPARE_AGENT_TURN')
FROM workflows AS workflow JOIN workflow_attempts AS attempt ON attempt.workflow_id = workflow.id AND attempt.active
WHERE workflow.id = $1`, fixture.workflowID, reviewID, newAttemptID).Scan(
		&state, &acceptedReviews, &reviewUsage, &internalEvents, &developerJobs); err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.StateDeveloping) || acceptedReviews != 1 || reviewUsage != 1 || internalEvents != 1 || developerJobs != 1 {
		t.Fatalf("Reviewer revalidation = Workflow %s, accepted reviews %d, cycles %d, events %d, Developer jobs %d",
			state, acceptedReviews, reviewUsage, internalEvents, developerJobs)
	}
}

func TestRetryTurnExposesOnlyItsSuccessfulAncestorTerminalIntent(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, original, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 978, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, original, store.MutationSpec{
		OperationID: "prior-ready", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"prior-ready","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", 978, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, original, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, original, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, original); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, original, failedSettlementObservation("ACP response unavailable"))
	if err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("settle lost-response Turn = (%#v, %v)", settled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, settled.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	spec := fixture.turnSpec()
	spec.Purpose, spec.RetryOfTurnID = workflow.TurnPurposeRetry, original.ID
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"confirm-prior", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil || execution.PriorTerminalIntent == nil || execution.PriorTerminalIntent.SourceInvocationID != mutation.ID {
		t.Fatalf("prior intent notice = (%#v, %v)", execution.PriorTerminalIntent, err)
	}
	source, err := database.GetConfirmablePriorTerminalIntent(ctx, retryLease, mutation.ID)
	if err != nil || source.ID != mutation.ID || source.ToolName != "request_review" {
		t.Fatalf("confirmable source = (%#v, %v)", source, err)
	}
	if _, err := database.GetConfirmablePriorTerminalIntent(ctx, retryLease, "10000000-0000-4000-8000-000000000999"); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("unrelated source error = %v", err)
	}
	confirmRequest := json.RawMessage(fmt.Sprintf(`{"operation_id":"confirm-after-crash","source_invocation_id":%q}`, mutation.ID))
	confirmation, err := database.ReserveMutation(ctx, retryLease, store.MutationSpec{
		OperationID: "confirm-after-crash", ToolName: "confirm_prior_terminal_intent",
		Request: confirmRequest, ExternalService: "omnigrex",
		ExternalResourceID: fixture.workflowID, ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, retryLease, confirmation.ID); err != nil {
		t.Fatal(err)
	}
	confirmationResult, err := json.Marshal(map[string]any{
		"source_invocation_id": mutation.ID, "source_tool": mutation.ToolName,
		"source_operation_id": mutation.OperationID, "source_request": mutation.Request,
		"source_result": result, "external_service": mutation.ExternalService,
		"external_resource_id": mutation.ExternalResourceID, "expected_sha": mutation.ExpectedSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, retryLease, confirmation.ID, confirmationResult); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, retryLease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := database.BeginTerminalCorroboration(ctx, retryLease, confirmation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport")
	if err != nil || checkpoint.SourceInvocationID != confirmation.ID {
		t.Fatalf("checkpoint confirmed intent = (%#v, %v)", checkpoint, err)
	}
	api := &replayOutcomeGitHub{pullRequest: &githubapi.PullRequest{
		ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
		State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
	}}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{paths: cleanCorroborationPaths(t)}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "verify-confirmation", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || api.calls != 1 {
		t.Fatalf("verify confirmed prior effect = processed %t, error %v, GitHub reads %d", processed, err, api.calls)
	}
	var workflowStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowStatus); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StateReviewing) {
		t.Fatalf("confirmed prior intent Workflow = %s, want REVIEWING", workflowStatus)
	}
}

func TestReplayedTerminalMutationCanEnterDurableCorroboration(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, original, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 993, workflow.RoleDeveloper, "")
	spec := store.MutationSpec{
		OperationID: "replay-ready", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"replay-ready","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", 993, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	}
	source, err := database.ReserveMutation(ctx, original, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, original, source.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, original, source.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, original); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, original, failedSettlementObservation("fresh Pull Request unavailable"))
	if err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("original Turn = (%#v, %v)", settled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, settled.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	retrySpec := fixture.turnSpec()
	retrySpec.Purpose, retrySpec.RetryOfTurnID = workflow.TurnPurposeRetry, original.ID
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, retrySpec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"replay-and-verify", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	if cached, err := database.ReserveMutation(ctx, retryLease, spec); err != nil || cached.ID != source.ID {
		t.Fatalf("reserve successful ancestor replay = (%#v, %v)", cached, err)
	}
	if err := database.AcknowledgeMutationReplay(ctx, retryLease, source.ID, spec); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, retryLease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := database.BeginTerminalCorroboration(ctx, retryLease, source.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport")
	if err != nil || checkpoint.SourceInvocationID != source.ID || checkpoint.TurnID != retry.ID {
		t.Fatalf("replayed intent checkpoint = (%#v, %v)", checkpoint, err)
	}
	api := &replayOutcomeGitHub{pullRequest: &githubapi.PullRequest{
		ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
		State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
	}}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{paths: cleanCorroborationPaths(t)}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "verify-replayed-intent", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || api.calls != 1 {
		t.Fatalf("corroborate replayed intent = processed %t, error %v, GitHub reads %d", processed, err, api.calls)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&state); err != nil || state != string(workflow.StateReviewing) {
		t.Fatalf("replayed intent Workflow = %s, error %v", state, err)
	}
}

func TestReplayedReviewerReviewCorroboratesWithoutAnotherSubmission(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, original, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 994, workflow.RoleReviewer, "review-head")
	spec := store.MutationSpec{
		OperationID: "replay-review", ToolName: "submit_review",
		Request:         json.RawMessage(`{"operation_id":"replay-review","event":"APPROVE","body":"Ready","comments":[]}`),
		ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", 994, proposal.PullRequestID),
		ExpectedSHA: proposal.HeadSHA,
	}
	source, err := database.ReserveMutation(ctx, original, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, original, source.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"review_id":99401,"node_id":"PRR_replayed","state":"APPROVED","commit_id":%q,"actor_id":99402,"html_url":"https://github.test/review"}`,
		proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, original, source.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, original); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, original, failedSettlementObservation("fresh review observation unavailable"))
	if err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("original Reviewer Turn = (%#v, %v)", settled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, settled.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	retrySpec := fixture.turnSpec()
	retrySpec.Stage, retrySpec.Purpose, retrySpec.RetryOfTurnID = workflow.StageReview, workflow.TurnPurposeRetry, original.ID
	retrySpec.ChangeProposalID, retrySpec.ExpectedHeadSHA = original.ChangeProposalID, original.ExpectedHeadSHA
	retrySpec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10,
		"Review test instructions.", nil)
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, retrySpec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"replay-review-verifier", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	if cached, err := database.ReserveMutation(ctx, retryLease, spec); err != nil || cached.ID != source.ID {
		t.Fatalf("reserve cached review = (%#v, %v)", cached, err)
	}
	if err := database.AcknowledgeMutationReplay(ctx, retryLease, source.ID, spec); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, retryLease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, retryLease, source.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "review_not_visible_yet"); err != nil {
		t.Fatal(err)
	}
	api := &corroborationReviewGitHub{
		pullRequest: githubapi.PullRequest{
			ID: proposal.PullRequestID, Number: int(proposal.PullRequestNumber), NodeID: proposal.PullRequestNodeID,
			State: "open", Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: proposal.HeadSHA, Label: "owner:" + proposal.HeadRef},
			Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:" + proposal.BaseRef},
		},
		review: githubapi.Review{ID: 99401, NodeID: "PRR_replayed", State: "APPROVED", CommitID: proposal.HeadSHA,
			User: githubapi.User{ID: 99402}},
	}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "reviewer-installation-token"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "verify-replayed-review", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || api.reads != 2 {
		t.Fatalf("verify replayed review = processed %t, error %v, GitHub reads %d", processed, err, api.reads)
	}
	var workflowStatus string
	var reviews int
	if err := pool.QueryRow(ctx, `
SELECT status, (SELECT count(*) FROM change_proposal_reviews WHERE review_id = 99401 AND accepted)
FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowStatus, &reviews); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StatePRReady) || reviews != 1 {
		t.Fatalf("replayed review = Workflow %s, accepted review identities %d", workflowStatus, reviews)
	}
}

func TestConflictingAncestorTerminalIntentsCannotBeConfirmed(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, original, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 979, workflow.RoleDeveloper, "")
	var reviewID string
	for _, tool := range []string{"request_review", "report_blocked"} {
		mutation, err := database.ReserveMutation(ctx, original, store.MutationSpec{
			OperationID: tool + "-source", ToolName: tool,
			Request: json.RawMessage(fmt.Sprintf(`{"operation_id":%q}`, tool+"-source")),
		})
		if err != nil {
			t.Fatal(err)
		}
		if tool == "request_review" {
			reviewID = mutation.ID
		}
		if _, err := database.StartMutation(ctx, original, mutation.ID); err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteMutation(ctx, original, mutation.ID, json.RawMessage(`{"outcome":"recorded"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.CloseMutationAdmission(ctx, original); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, original, failedSettlementObservation("conflicting terminal evidence"))
	if err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("settle conflicting Turn = (%#v, %v)", settled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, settled.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	spec := fixture.turnSpec()
	spec.Purpose, spec.RetryOfTurnID = workflow.TurnPurposeRetry, original.ID
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"reject-conflicting-source", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil || execution.PriorTerminalIntent != nil {
		t.Fatalf("conflicting source must not appear in envelope = (%#v, %v)", execution.PriorTerminalIntent, err)
	}
	if _, err := database.GetConfirmablePriorTerminalIntent(ctx, retryLease, reviewID); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("confirm conflicting source error = %v", err)
	}
}

func TestReviewerSynchronizationCancelsLeasedPriorIntentVerifier(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 980
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleReviewer, "review-head")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "review-before-sync", ToolName: "submit_review",
		Request:         json.RawMessage(`{"operation_id":"review-before-sync","event":"APPROVE","body":"Ready","comments":[]}`),
		ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", number, proposal.PullRequestID),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID,
		json.RawMessage(fmt.Sprintf(`{"review_id":98001,"node_id":"PRR_sync","state":"APPROVED","commit_id":%q,"actor_id":98002,"html_url":"https://github.test/review"}`, proposal.HeadSHA))); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "rate_limited"); err != nil {
		t.Fatal(err)
	}
	originalVerifier, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind, "verify-before-sync", time.Minute)
	if err != nil || originalVerifier == nil {
		t.Fatalf("claim original verifier = (%#v, %v)", originalVerifier, err)
	}
	if _, err := database.CompleteTerminalCorroborationHandoff(ctx, *originalVerifier,
		workflow.ReasonTerminalCorroborationPrerequisite, "permission_denied"); err != nil {
		t.Fatal(err)
	}
	trigger := workflowDelivery("95000000-0000-4000-8000-000000000980")
	trigger.RepositoryID, trigger.IssueID, trigger.IssueNumber = number, number, number
	trigger.RepositoryOwner, trigger.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, trigger)
	const newAttemptID = "95000000-0000-4000-8000-000000000981"
	if _, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "labeled"),
		store.WorkflowLocator{RepositoryID: number, IssueID: number, IssueNumber: number},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: eventContext.Metadata, AttemptID: newAttemptID,
				AttemptNumber: eventContext.Snapshot.LastAttemptNumber + 1}, nil
		}); err != nil {
		t.Fatal(err)
	}
	revalidation, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.RevalidateTerminalIntentJobKind,
		"revalidate-before-sync", time.Minute)
	if err != nil || revalidation == nil {
		t.Fatalf("claim revalidation = (%#v, %v)", revalidation, err)
	}
	synchronize := workflowDelivery("95000000-0000-4000-8000-000000000982")
	synchronize.RepositoryID, synchronize.IssueID, synchronize.IssueNumber = number, number, number
	synchronize.RepositoryOwner, synchronize.RepositoryName = "owner", "repo"
	syncClaim := claimWorkflowDelivery(t, database, ctx, synchronize)
	application, err := database.CompleteWebhookTransition(ctx, syncClaim.DeliveryID, syncClaim.ClaimToken,
		normalizedPayload(syncClaim.DeliveryID, "synchronize"),
		store.WorkflowLocator{RepositoryID: number, IssueID: number, IssueNumber: number},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.SynchronizationEvent{EventMetadata: eventContext.Metadata,
				ChangeProposalID: proposal.PullRequestID, PreviousHeadSHA: proposal.HeadSHA,
				HeadSHA: "ffffffffffffffffffffffffffffffffffffffff"}, nil
		})
	if err != nil || application.Reason != workflow.ReasonReviewHeadReplaced {
		t.Fatalf("synchronize while revalidating = (%#v, %v)", application, err)
	}
	if _, err := database.GetTerminalCorroborationContext(ctx, *revalidation); !errors.Is(err, store.ErrJobLeaseLost) {
		t.Fatalf("superseded revalidation context error = %v", err)
	}
	var verifierStatus string
	var reviewerJobs int
	if err := pool.QueryRow(ctx, `
SELECT verifier.status,
       (SELECT count(*) FROM jobs WHERE workflow_attempt_id = $2 AND kind = 'PREPARE_AGENT_TURN' AND status = 'AVAILABLE')
FROM jobs AS verifier WHERE verifier.id = $1`, revalidation.ID, newAttemptID).Scan(&verifierStatus, &reviewerJobs); err != nil {
		t.Fatal(err)
	}
	if verifierStatus != "CANCELLED" || reviewerJobs != 1 {
		t.Fatalf("synchronization = verifier %s, new Reviewer jobs %d (Workflow %s)", verifierStatus, reviewerJobs, fixture.workflowID)
	}
}

func TestReplayedAndDirectConflictingAncestorIntentsCannotBeConfirmed(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	reducer, err := workflow.NewReducer(definition, 3)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile, storeConfigForReducer(t, reducer))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	fixture, original, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 982, workflow.RoleDeveloper, "")
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 3 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	spec := store.MutationSpec{
		OperationID: "original-ready", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"original-ready","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", 982, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	}
	source, err := database.ReserveMutation(ctx, original, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, original, source.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))
	if err := database.CompleteMutation(ctx, original, source.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, original); err != nil {
		t.Fatal(err)
	}
	first, err := database.SettleAgentTurn(ctx, original, failedSettlementObservation("lost prompt response"))
	if err != nil || first.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("first settlement = (%#v, %v)", first, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, first.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	retrySpec := fixture.turnSpec()
	retrySpec.Purpose, retrySpec.RetryOfTurnID = workflow.TurnPurposeRetry, original.ID
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, retrySpec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"replay-then-block", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	if replay, err := database.ReserveMutation(ctx, retryLease, spec); err != nil || replay.ID != source.ID {
		t.Fatalf("reserve prior replay = (%#v, %v)", replay, err)
	}
	if err := database.AcknowledgeMutationReplay(ctx, retryLease, source.ID, spec); err != nil {
		t.Fatal(err)
	}
	blocked, err := database.ReserveMutation(ctx, retryLease, store.MutationSpec{
		OperationID: "new-blocker", ToolName: "report_blocked",
		Request: json.RawMessage(`{"operation_id":"new-blocker","reason":"conflicting intent"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, retryLease, blocked.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, retryLease, blocked.ID, json.RawMessage(`{"outcome":"BLOCKED"}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	second, err := database.SettleAgentTurn(ctx, retryLease, failedSettlementObservation("duplicate terminal evidence"))
	if err != nil || second.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("second settlement = (%#v, %v)", second, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, second.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	retrySpec.RetryOfTurnID = retry.ID
	last, err := prepareFixtureAgentTurn(t, database, pool, ctx, retrySpec)
	if err != nil {
		t.Fatal(err)
	}
	lastLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, last), last.ControlRevision,
		"reject-replayed-conflict", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, lastLease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, lastLease)
	if err != nil || execution.PriorTerminalIntent != nil {
		t.Fatalf("conflicting lineage notice = (%#v, %v)", execution.PriorTerminalIntent, err)
	}
	if _, err := database.GetConfirmablePriorTerminalIntent(ctx, lastLease, source.ID); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("conflicting replay lineage error = %v", err)
	}
}

func TestConfirmationOfSameAncestorEffectRemainsConfirmableAfterInterruptedRetry(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	reducer, err := workflow.NewReducer(definition, 3)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile, storeConfigForReducer(t, reducer))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	fixture, original, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 983, workflow.RoleDeveloper, "")
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 3 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	sourceSpec := store.MutationSpec{
		OperationID: "original-ready", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"original-ready","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", 983, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	}
	source, err := database.ReserveMutation(ctx, original, sourceSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, original, source.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, original, source.ID, json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, original); err != nil {
		t.Fatal(err)
	}
	first, err := database.SettleAgentTurn(ctx, original, failedSettlementObservation("lost ACP response"))
	if err != nil || first.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("original settlement = (%#v, %v)", first, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, first.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	retrySpec := fixture.turnSpec()
	retrySpec.Purpose, retrySpec.RetryOfTurnID = workflow.TurnPurposeRetry, original.ID
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, retrySpec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"confirm-same-effect", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	confirmation, err := database.ReserveMutation(ctx, retryLease, store.MutationSpec{
		OperationID: "confirm-original", ToolName: "confirm_prior_terminal_intent",
		Request:         json.RawMessage(fmt.Sprintf(`{"operation_id":"confirm-original","source_invocation_id":%q}`, source.ID)),
		ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID, ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, retryLease, confirmation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, retryLease, confirmation.ID,
		json.RawMessage(fmt.Sprintf(`{"source_invocation_id":%q,"source_tool":"request_review"}`, source.ID))); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	second, err := database.SettleAgentTurn(ctx, retryLease, failedSettlementObservation("confirmation prompt cancelled"))
	if err != nil || second.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("confirmation settlement = (%#v, %v)", second, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, second.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	retrySpec.RetryOfTurnID = retry.ID
	last, err := prepareFixtureAgentTurn(t, database, pool, ctx, retrySpec)
	if err != nil {
		t.Fatal(err)
	}
	lastLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, last), last.ControlRevision,
		"confirm-after-cancel", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, lastLease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, lastLease)
	if err != nil || execution.PriorTerminalIntent == nil || execution.PriorTerminalIntent.SourceInvocationID != source.ID {
		t.Fatalf("same-effect prior notice = (%#v, %v)", execution.PriorTerminalIntent, err)
	}
	if proven, err := database.GetConfirmablePriorTerminalIntent(ctx, lastLease, source.ID); err != nil || proven.ID != source.ID {
		t.Fatalf("same-effect confirmation = (%#v, %v)", proven, err)
	}
}

func TestExplicitlyCancelledTerminalIntentCannotBeConfirmed(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for index, promptOutcome := range []json.RawMessage{json.RawMessage(`{"stop_reason":"cancelled"}`), nil} {
		t.Run(fmt.Sprintf("cancelled-%d", index), func(t *testing.T) {
			fixture, original, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 985+index, workflow.RoleDeveloper, "")
			mutation, err := database.ReserveMutation(ctx, original, store.MutationSpec{
				OperationID: "cancelled-ready", ToolName: "request_review",
				Request:         json.RawMessage(`{"operation_id":"cancelled-ready","summary":"Ready"}`),
				ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", 985+index, proposal.HeadRef),
				ExpectedSHA: proposal.HeadSHA,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.StartMutation(ctx, original, mutation.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteMutation(ctx, original, mutation.ID,
				json.RawMessage(fmt.Sprintf(`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
					proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))); err != nil {
				t.Fatal(err)
			}
			if err := database.CloseMutationAdmission(ctx, original); err != nil {
				t.Fatal(err)
			}
			const diagnostic = "ACP prompt was cancelled"
			observation := store.AgentTurnSettlementObservation{
				ObservedAt: time.Now().UTC(), Outcome: workflow.TurnOutcomeInfrastructureFailed,
				Diagnostic: diagnostic, Completion: store.AgentTurnCompletion{
					Status: store.AgentTurnInterrupted, Outcome: promptOutcome, LastError: diagnostic,
				},
			}
			settled, err := database.SettleAgentTurn(ctx, original, observation)
			if err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
				t.Fatalf("cancelled settlement = (%#v, %v)", settled, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, settled.SuccessorJobID); err != nil {
				t.Fatal(err)
			}
			spec := fixture.turnSpec()
			spec.Purpose, spec.RetryOfTurnID = workflow.TurnPurposeRetry, original.ID
			retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
				"cancelled-ancestor", time.Minute, 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
				t.Fatal(err)
			}
			execution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
			if err != nil || execution.PriorTerminalIntent != nil {
				t.Fatalf("cancelled Turn offered prior confirmation = (%#v, %v)", execution.PriorTerminalIntent, err)
			}
			if _, err := database.GetConfirmablePriorTerminalIntent(ctx, retryLease, mutation.ID); !errors.Is(err, store.ErrMutationOperationConflict) {
				t.Fatalf("cancelled Turn confirmation error = %v", err)
			}
		})
	}
}

func TestRevalidationIncompatibleDefinitionCreatesHumanHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const number = 989
	fixture, original, _ := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, original, store.MutationSpec{
		OperationID: "before-incompatible-upgrade", ToolName: "request_review",
		Request: json.RawMessage(`{"operation_id":"before-incompatible-upgrade","summary":"Ready"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, original, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, original, mutation.ID, json.RawMessage(`{"outcome":"REVIEW_REQUESTED"}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, original, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTerminalCorroboration(ctx, original, mutation.ID,
		json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport"); err != nil {
		t.Fatal(err)
	}
	verifier, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind,
		"before-incompatible-upgrade", time.Minute)
	if err != nil || verifier == nil {
		t.Fatalf("claim original verifier = (%#v, %v)", verifier, err)
	}
	if _, err := database.CompleteTerminalCorroborationHandoff(ctx, *verifier,
		workflow.ReasonTerminalCorroborationExhausted, "transient_transport"); err != nil {
		t.Fatal(err)
	}
	delivery := workflowDelivery("95000000-0000-4000-8000-000000000989")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = number, number, number
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	const newAttemptID = "95000000-0000-4000-8000-000000000990"
	if _, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "labeled"),
		store.WorkflowLocator{RepositoryID: number, IssueID: number, IssueNumber: number},
		func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: eventContext.Metadata,
				AttemptID: newAttemptID, AttemptNumber: eventContext.Snapshot.LastAttemptNumber + 1}, nil
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET current_stage = 'removed-stage' WHERE id = $1`, newAttemptID); err != nil {
		t.Fatal(err)
	}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: &replayOutcomeGitHub{}})
	if err != nil {
		t.Fatal(err)
	}
	credential := &integrationCredentialProvider{credential: "unused"}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential,
		corroborationTestPaths{}, agentturn.TerminalCorroborationWorkerConfig{
			ClaimOwner: "incompatible-revalidation", Window: 30 * time.Minute,
			PollInterval: time.Second, LeaseDuration: 30 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || credential.calls != 0 {
		t.Fatalf("incompatible revalidation = processed %t, error %v, credential calls %d", processed, err, credential.calls)
	}
	var state, reason string
	var handoffEvents int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.human_handoff_reason,
       (SELECT count(*) FROM workflow_internal_events WHERE workflow_id = workflow.id
           AND kind = 'TERMINAL_REVALIDATION_FAILED' AND applied_at IS NOT NULL)
FROM workflows AS workflow WHERE workflow.id = $1`, fixture.workflowID).Scan(&state, &reason, &handoffEvents); err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.StateNeedsHuman) || reason != string(workflow.ReasonWorkflowDefinitionIncompatible) || handoffEvents != 1 {
		t.Fatalf("incompatible outcome = Workflow %s, reason %s, internal events %d", state, reason, handoffEvents)
	}
}

func TestCancelledReviewerReviewInRetryRequiresHumanNotDuplicateSubmission(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, original, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 992, workflow.RoleReviewer, "review-head")
	mutation, err := database.ReserveMutation(ctx, original, store.MutationSpec{
		OperationID: "cancelled-review", ToolName: "submit_review",
		Request:         json.RawMessage(`{"operation_id":"cancelled-review","event":"APPROVE","body":"Ready","comments":[]}`),
		ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", 992, proposal.PullRequestID),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, original, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, original, mutation.ID,
		json.RawMessage(fmt.Sprintf(`{"review_id":99201,"node_id":"PRR_cancelled","state":"APPROVED","commit_id":%q,"actor_id":99202,"html_url":"https://github.test/review"}`, proposal.HeadSHA))); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, original); err != nil {
		t.Fatal(err)
	}
	const diagnostic = "ACP prompt was cancelled"
	settled, err := database.SettleAgentTurn(ctx, original, store.AgentTurnSettlementObservation{
		ObservedAt: time.Now().UTC(), Outcome: workflow.TurnOutcomeInfrastructureFailed,
		Diagnostic: diagnostic, Completion: store.AgentTurnCompletion{
			Status: store.AgentTurnInterrupted, Outcome: json.RawMessage(`{"stop_reason":"cancelled"}`), LastError: diagnostic,
		},
	})
	if err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("cancelled Reviewer settlement = (%#v, %v)", settled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp()
WHERE id = $1 AND status = 'AVAILABLE'`, settled.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	spec := fixture.turnSpec()
	spec.Stage, spec.Purpose, spec.RetryOfTurnID = workflow.StageReview, workflow.TurnPurposeRetry, original.ID
	spec.ChangeProposalID, spec.ExpectedHeadSHA = original.ChangeProposalID, original.ExpectedHeadSHA
	spec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10,
		"Review test instructions.", nil)
	retry, err := prepareFixtureAgentTurn(t, database, pool, ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	retryLease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, retry), retry.ControlRevision,
		"cancelled-review-retry", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil || execution.PriorTerminalIntent != nil || execution.PriorReviewRequiresHuman == nil ||
		execution.PriorReviewRequiresHuman.SourceInvocationID != mutation.ID {
		t.Fatalf("cancelled Reviewer retry notice = (confirmable %#v, requires human %#v, error %v)",
			execution.PriorTerminalIntent, execution.PriorReviewRequiresHuman, err)
	}
	if previous, err := database.HasPriorSuccessfulReviewForTurn(ctx, retryLease); err != nil || !previous {
		t.Fatalf("duplicate review guard = (%t, %v)", previous, err)
	}
	if _, err := database.GetConfirmablePriorTerminalIntent(ctx, retryLease, mutation.ID); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("cancelled review confirmation error = %v", err)
	}
	priorSpec := store.MutationSpec{
		OperationID: "cancelled-review", ToolName: "submit_review",
		Request:         json.RawMessage(`{"operation_id":"cancelled-review","event":"APPROVE","body":"Ready","comments":[]}`),
		ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", 992, proposal.PullRequestID),
		ExpectedSHA: proposal.HeadSHA,
	}
	if _, err := database.ReserveMutation(ctx, retryLease, priorSpec); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("cancelled review exact replay reservation error = %v", err)
	}
	if err := database.AcknowledgeMutationReplay(ctx, retryLease, mutation.ID, priorSpec); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("cancelled review exact replay acknowledgement error = %v", err)
	}
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	content, err := agentturn.BuildEventEnvelope(execution, proposal.HeadSHA, definition, role.BuiltinPolicyCatalog())
	if err != nil || len(content) != 1 || !strings.Contains(content[0].Text, `"prior_unconfirmable_review"`) ||
		!strings.Contains(content[0].Text, "Do not submit another review") {
		t.Fatalf("cancelled review envelope = (%#v, %v)", content, err)
	}
}
