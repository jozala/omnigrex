//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestClarificationRecordedPromptRecovery(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	number := 970
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		for _, expired := range []bool{false, true} {
			for _, inputTiming := range []string{"none", "before-checkpoint", "during-corroboration", "empty-yield", "unpublished"} {
				t.Run(fmt.Sprintf("%s/expired=%t/%s", role, expired, inputTiming), func(t *testing.T) {
					number++
					database := databases[0]
					fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, number, role, "review-head")
					if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failures=1,review_usage='{"review":2}'::jsonb WHERE id=$1`, fixture.attemptID); err != nil {
						t.Fatal(err)
					}
					input := store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Keep compatibility."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "reply"}, Authorized: true, At: time.Now()}
					admit := func() {
						t.Helper()
						receipt, err := database.AdmitHumanInput(ctx, input)
						if err != nil || receipt.ResponseID == "" {
							t.Fatal(receipt, err)
						}
					}
					questions := []collaboration.Question{{ID: "Q1", Body: "Compatibility policy?"}}
					if inputTiming == "empty-yield" {
						questions = nil
						admit()
					}
					request, _ := json.Marshal(map[string]any{"operation_id": "question", "questions": questions})
					mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "question", ToolName: "request_clarification", Request: request, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID, ExpectedSHA: "review-head"})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
						t.Fatal(err)
					}
					round, err := database.RequestClarification(ctx, lease, mutation.ID, questions)
					if err != nil {
						t.Fatal(err)
					}
					if round.ID != "" && inputTiming != "unpublished" {
						if err = database.ConfirmClarificationPublication(ctx, fixture.workflowID, round.ID, collaboration.Source{Channel: "fixture", ID: "published"}); err != nil {
							t.Fatal(err)
						}
					}
					result, _ := json.Marshal(map[string]string{"outcome": "CLARIFICATION_WAIT", "round_id": round.ID})
					if err = database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
						t.Fatal(err)
					}
					if err = database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
						t.Fatal(err)
					}
					if inputTiming == "before-checkpoint" {
						admit()
					}
					if expired {
						expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
						if _, err = database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
							t.Fatal(err)
						}
						stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-clarification")
						mutations := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "reconcile-clarification")
						if _, err = database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
							t.Fatal(err)
						}
						if candidates, err := database.ListAgentTurnMutationsForReconciliation(ctx, mutations); err != nil || len(candidates) != 0 {
							t.Fatal(candidates, err)
						}
						if _, err = database.CompleteAgentTurnMutationReconciliation(ctx, mutations); err != nil {
							t.Fatal(err)
						}
						recovery, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
						if err != nil || recovery.Status != store.AgentTurnCorroborating || recovery.SuccessorAllowed {
							t.Fatalf("published clarification fell into infrastructure retry: %+v %v", recovery, err)
						}
					} else {
						if _, err = database.BeginTerminalCorroboration(ctx, lease, mutation.ID, json.RawMessage(`{"admitted":true,"status":"completed"}`), "", "transient_transport"); err != nil {
							t.Fatal(err)
						}
					}
					if inputTiming == "during-corroboration" {
						admit()
					}
					// A fresh application Store/worker observes only the recorded evidence.
					workspaces, _ := integrationTerminalWorkspace(t, pool, lease, committedPublicationPaths(t), "review-head", "installation-token")
					credential := &integrationCredentialProvider{credential: "installation-token"}
					api := &replayOutcomeGitHub{}
					outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: databases[1], GitHub: api, Workspace: workspaces, DeveloperCredentials: credential, ReviewerCredentials: credential})
					if err != nil {
						t.Fatal(err)
					}
					worker, err := agentturn.NewTerminalCorroborationWorker(databases[1], outcomes, credential, credential, workspaces, agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "clarification-recovery", Window: time.Minute, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
					if err != nil {
						t.Fatal(err)
					}
					if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
						t.Fatal(processed, err)
					}
					var waiting bool
					var retries, reviewUsage, active, jobs int
					var outcome, status, attempt, participant, session string
					if err = pool.QueryRow(ctx, `SELECT w.clarification_waiting,a.infrastructure_failures,COALESCE((a.review_usage->>'review')::int,0),s.workflow_outcome,t.status,t.workflow_attempt_id::text,s.agent_participant_id::text,t.agent_session_id::text,(SELECT count(*) FROM agent_turns WHERE workflow_id=w.id AND active),(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')) FROM workflows w JOIN workflow_attempts a ON a.id=$2 JOIN agent_turns t ON t.id=$3 JOIN agent_turn_settlements s ON s.agent_turn_id=t.id WHERE w.id=$1`, fixture.workflowID, fixture.attemptID, lease.ID).Scan(&waiting, &retries, &reviewUsage, &outcome, &status, &attempt, &participant, &session, &active, &jobs); err != nil {
						t.Fatal(err)
					}
					if inputTiming == "unpublished" {
						if waiting || retries != 1 || jobs != 0 || outcome != "INFRASTRUCTURE_FAILED" || status != "FAILED" {
							t.Fatalf("unpublished question was treated as delivered: wait=%t retries=%d jobs=%d outcome=%s status=%s", waiting, retries, jobs, outcome, status)
						}
						return
					}
					expectedJobs := 1
					if inputTiming == "none" {
						expectedJobs = 0
					}
					if !waiting || retries != 1 || reviewUsage != 2 || outcome != "CLARIFICATION_WAIT" || status != "SUCCEEDED" || attempt != fixture.attemptID || participant != fixture.assignmentID || session != fixture.sessionID || active != 0 || jobs != expectedJobs || api.calls != 0 {
						t.Fatalf("incorrect recovered wait: waiting=%t retries=%d reviews=%d outcome=%s status=%s identities=%s/%s/%s active=%d jobs=%d reads=%d", waiting, retries, reviewUsage, outcome, status, attempt, participant, session, active, jobs, api.calls)
					}
					if processed, err := worker.ProcessOne(ctx); err != nil || processed {
						t.Fatalf("duplicate verification: %t %v", processed, err)
					}
				})
			}
		}
	}
}

func TestClarificationRevalidationAfterExplicitHumanHandoffRecovery(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			number := 991 + i
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, number, role, "review-head")
			mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "question", ToolName: "request_clarification", Request: json.RawMessage(`{"operation_id":"question","questions":[{"id":"Q1","body":"Compatibility?"}]}`), ExternalService: "github", ExternalResourceID: fmt.Sprintf("%d:%d", number, number), ExpectedSHA: "review-head"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
				t.Fatal(err)
			}
			round, err := database.RequestClarification(ctx, lease, mutation.ID, []collaboration.Question{{ID: "Q1", Body: "Compatibility?"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = database.ConfirmClarificationPublication(ctx, fixture.workflowID, round.ID, collaboration.Source{Channel: "fixture", ID: "published"}); err != nil {
				t.Fatal(err)
			}
			result, _ := json.Marshal(map[string]string{"outcome": "CLARIFICATION_WAIT", "round_id": round.ID})
			if err = database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
				t.Fatal(err)
			}
			if err = database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
				t.Fatal(err)
			}
			if _, err = database.BeginTerminalCorroboration(ctx, lease, mutation.ID, json.RawMessage(`{"admitted":true,"status":"completed"}`), "", "transient_transport"); err != nil {
				t.Fatal(err)
			}
			verifier, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind, "verification", time.Minute)
			if err != nil || verifier == nil {
				t.Fatal(verifier, err)
			}
			if _, err = database.CompleteTerminalCorroborationHandoff(ctx, *verifier, workflow.ReasonTerminalCorroborationPrerequisite, "permission_denied"); err != nil {
				t.Fatal(err)
			}
			// Clarification itself cannot bypass this real Human Handoff.
			input := store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "answer", Target: round.ID, Body: "Keep compatibility."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "before-recovery"}, Authorized: true, At: time.Now()}
			receipt, err := database.AdmitHumanInput(ctx, input)
			if err != nil || receipt.ResponseID != "" {
				t.Fatal(receipt, err)
			}
			delivery := workflowDelivery(uuid.NewString())
			delivery.RepositoryID = int64(number)
			delivery.IssueID = int64(number)
			delivery.IssueNumber = int64(number)
			delivery.RepositoryOwner = "owner"
			delivery.RepositoryName = "repo"
			claim := claimWorkflowDelivery(t, database, ctx, delivery)
			newAttempt := uuid.NewString()
			if _, err = database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "labeled"), store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)}, func(c store.WorkflowEventContext) (workflow.Event, error) {
				return workflow.TriggerEvent{EventMetadata: c.Metadata, AttemptID: newAttempt, AttemptNumber: c.Snapshot.LastAttemptNumber + 1}, nil
			}); err != nil {
				t.Fatal(err)
			}
			workspaces, _ := integrationTerminalWorkspace(t, pool, lease, committedPublicationPaths(t), "review-head", "installation-token")
			credential := &integrationCredentialProvider{credential: "installation-token"}
			outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: &replayOutcomeGitHub{}, Workspace: workspaces, DeveloperCredentials: credential, ReviewerCredentials: credential})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := agentturn.NewTerminalCorroborationWorker(database, outcomes, credential, credential, workspaces, agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "revalidation", Window: time.Minute, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if done, err := worker.ProcessOne(ctx); err != nil || !done {
				t.Fatal(done, err)
			}
			var waiting bool
			var retries, jobs int
			var oldOutcome string
			if err = pool.QueryRow(ctx, `SELECT w.clarification_waiting,a.infrastructure_failures,(SELECT count(*) FROM jobs WHERE workflow_attempt_id=$2 AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')),(SELECT workflow_outcome FROM agent_turn_settlements WHERE agent_turn_id=$3) FROM workflows w JOIN workflow_attempts a ON a.id=$2 WHERE w.id=$1`, fixture.workflowID, newAttempt, lease.ID).Scan(&waiting, &retries, &jobs, &oldOutcome); err != nil {
				t.Fatal(err)
			}
			if !waiting || retries != 0 || jobs != 0 || oldOutcome != "INFRASTRUCTURE_FAILED" {
				t.Fatalf("revalidation rewrote history or lost wait: %t %d %d %s", waiting, retries, jobs, oldOutcome)
			}
			input.Source.ID = "after-recovery"
			receipt, err = database.AdmitHumanInput(ctx, input)
			if err != nil || receipt.ResponseID == "" {
				t.Fatal(receipt, err)
			}
			if err = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_attempt_id=$1 AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')`, newAttempt).Scan(&jobs); err != nil || jobs != 1 {
				t.Fatal(jobs, err)
			}
		})
	}
}
