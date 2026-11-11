//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

// The publication rows are fixture ledger evidence, not real GitHub effects.
// Refresh receipts have the same shape and exact execution identity as production.
func TestLateSteeringRejectedReviewYieldsAndDeliversToRetainedSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	for i, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejected=%t", reject), func(t *testing.T) {
			ctx := t.Context()
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 3980+i, workflow.RoleDeveloper, productionFixtureHead)
			execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			for j, tool := range []string{mcp.ToolRefreshDefaultBranch, mcp.ToolPublishChanges, mcp.ToolOpenPR, mcp.ToolRefreshDefaultBranch} {
				completeRecoveryFixture(t, database, lease, execution.Repository, j, tool)
			}
			receipt, err := database.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Cover historical publication."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "late-steering"}, Authorized: true, At: time.Now()})
			if err != nil || receipt.ResponseID == "" {
				t.Fatal(receipt, err)
			}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: &publicationReadGitHub{&replayGatewayGitHub{}}, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{}, Workflow: &replayGatewayWorkflow{}, Clarifications: database})
			if err != nil {
				t.Fatal(err)
			}
			scope := replayGatewayScope(lease, execution, productionFixtureHead)
			scope.PullRequest = &mcp.PullRequestScope{ID: int64((3980 + i) * 100), Number: int64(3980 + i)}
			gateway, registration := openReplayGateway(t, database, backend, scope)
			if reject {
				_, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: mcp.ToolRequestReview, OperationID: "direct-blocked", Request: json.RawMessage(`{"operation_id":"direct-blocked","summary":"Ready"}`)})
				var rejection *store.MutationReservationRejection
				if !errors.Is(err, store.ErrPendingHumanInput) || !errors.As(err, &rejection) {
					t.Fatalf("not an authoritative no-reservation rejection: %v", err)
				}
				for range 2 {
					body := callReplayGatewayTool(t, gateway, registration, mcp.ToolRequestReview, map[string]any{"operation_id": "review-blocked", "summary": "Ready"}, true)
					if !strings.Contains(body, store.ErrPendingHumanInput.Error()) {
						t.Fatal(body)
					}
				}
			}
			callReplayGatewayTool(t, gateway, registration, mcp.ToolRequestClarification, map[string]any{"operation_id": "yield", "questions": []any{}}, false)
			if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
				t.Fatal(err)
			}
			ledger, err := database.ListAgentTurnMutationInvocations(ctx, lease)
			if err != nil || len(ledger) != 5 {
				t.Fatal(ledger, err)
			}
			for j, m := range ledger {
				if m.State != store.MutationSucceeded || m.LastError != "" || m.InvocationNumber != int64(j+1) {
					t.Fatal(m)
				}
			}
			if err := gateway.CloseAndDrain(ctx, registration); err != nil {
				t.Fatalf("all-success yield drain: %v", err)
			}
			observation := store.AgentTurnSettlementObservation{ObservedAt: time.Now(), Outcome: workflow.TurnOutcomeClarificationWait, Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded}}
			settled, err := database.SettleAgentTurn(ctx, lease, observation)
			if err != nil || settled.SuccessorJobID == "" || settled.State == workflow.StateNeedsHuman {
				t.Fatal(settled, err)
			}
			replay, err := database.SettleAgentTurn(ctx, lease, observation)
			if err != nil || replay.SuccessorJobID != settled.SuccessorJobID {
				t.Fatal(replay, err)
			}
			job := claimSettlementSuccessorJob(t, database, pool, ctx, settled.SuccessorJobID, "steering-preparation")
			spec := preparationSpec("head", "provider/test")
			updateStagePreparation(&spec, workflow.StageImplementation, func(p *store.ParticipantPreparation) {
				p.Binding.RuntimeImageDigest = "sha256:test"
				original := fixture.turnSpec()
				p.Profile = store.AgentProfileSnapshot{CommitSHA: original.AgentProfileCommitSHA, ContentSHA256: original.AgentProfileContentSHA256, Config: original.AgentProfileConfig}
			})
			prepared, err := prepareWithFixtureAdmission(t, database, ctx, *job, spec)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Inputs []collaboration.Input `json:"human_input"`
			}
			if err := json.Unmarshal(prepared.Turn.TurnConfiguration, &config); err != nil {
				t.Fatal(err)
			}
			if prepared.Participant.ID != lease.AgentParticipantID || prepared.Session.ID != lease.AgentSessionID || prepared.Turn.WorkflowAttemptID != fixture.attemptID || len(config.Inputs) != 1 || config.Inputs[0].ID != receipt.ResponseID || config.Inputs[0].DeliveredTo != prepared.Turn.ID {
				t.Fatalf("continuation=%+v input=%+v", prepared, config)
			}
			var failures, reviews, jobs int
			if err := pool.QueryRow(ctx, `SELECT infrastructure_failures,COALESCE((review_usage->>'review')::int,0),(SELECT count(*) FROM jobs WHERE workflow_id=$2 AND kind='PREPARE_AGENT_TURN') FROM workflow_attempts WHERE id=$1`, fixture.attemptID, fixture.workflowID).Scan(&failures, &reviews, &jobs); err != nil {
				t.Fatal(err)
			}
			if failures != 0 || reviews != 0 || jobs != 2 {
				t.Fatalf("budgets/allocation changed: failures=%d reviews=%d jobs=%d", failures, reviews, jobs)
			}
		})
	}
}

const productionFixtureHead = "0123456789abcdef0123456789abcdef01234567"

func completeRecoveryFixture(t *testing.T, database *store.Store, lease store.AgentTurnLease, repo store.AgentTurnRepository, index int, tool string) store.MutationReservation {
	t.Helper()
	op := fmt.Sprintf("fixture-success-%d", index)
	request, _ := json.Marshal(map[string]any{"operation_id": op})
	spec := store.MutationSpec{OperationID: op, ToolName: tool, Request: request}
	if tool == mcp.ToolRefreshDefaultBranch {
		spec.ExternalService = "workspace"
		spec.ExternalResourceID = fmt.Sprintf("%d:%s:%s:%s:%d", repo.ID, lease.AgentParticipantID, lease.AgentSessionID, lease.ID, lease.ExecutionEpoch)
	}
	mutation, err := database.ReserveMutation(t.Context(), lease, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(t.Context(), lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"fixture_success":true}`)
	if tool == mcp.ToolRefreshDefaultBranch {
		digest := sha256.Sum256([]byte(spec.ExternalResourceID + ":" + mutation.ID))
		now := time.Now().UTC()
		result, _ = json.Marshal(map[string]any{"status": "current", "repository": map[string]any{"id": repo.ID, "owner": repo.Owner, "name": repo.Name}, "branch": "main", "sha": productionFixtureHead, "ref": "refs/omnigrex/default/" + hex.EncodeToString(digest[:]), "observed_at": now, "reobserved_at": now, "observed_branch": "main", "observed_sha": productionFixtureHead})
	}
	if err := database.CompleteMutation(t.Context(), lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	return mutation
}

// Both selector branches must accept completed local receipts through the real
// recovery worker, without consulting GitHub, publications, or the live workspace.
func TestSucceededRefreshRecoveryCompletesBothSelectorBranches(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	for i, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded=%t", recorded), func(t *testing.T) {
			ctx := t.Context()
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 3985+i, workflow.RoleDeveloper, productionFixtureHead)
			execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			for j := range 2 {
				completeRecoveryFixture(t, database, lease, execution.Repository, j, mcp.ToolRefreshDefaultBranch)
			}
			if _, err := database.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Continue."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "pending"}, Authorized: true, At: time.Now()}); err != nil {
				t.Fatal(err)
			}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: &publicationReadGitHub{&replayGatewayGitHub{}}, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{}, Workflow: &replayGatewayWorkflow{}, Clarifications: database})
			if err != nil {
				t.Fatal(err)
			}
			scope := replayGatewayScope(lease, execution, productionFixtureHead)
			scope.PullRequest = &mcp.PullRequestScope{ID: int64((3985 + i) * 100), Number: int64(3985 + i)}
			gateway, registration := openReplayGateway(t, database, backend, scope)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolRequestClarification, map[string]any{"operation_id": "yield", "questions": []any{}}, false)
			if err := gateway.CloseAndDrain(ctx, registration); err != nil {
				t.Fatal(err)
			}
			if recorded {
				err = database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", "")
			} else {
				err = database.CloseMutationAdmission(ctx, lease)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.BeginAgentTurnRecovery(ctx, lease); err != nil {
				t.Fatal(err)
			}
			stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "refresh-stop")
			if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
				t.Fatal(err)
			}
			reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &noRecoveryGitHub{}, Credentials: noRecoveryCredentials{}, Publications: noRecoveryPublications{}, Clarifications: database})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := mcp.NewRecoveryWorker(database, reconciler, mcp.RecoveryWorkerConfig{ClaimOwner: "refresh-recovery", LeaseDuration: 5 * time.Second, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if processed, err := worker.ProcessNext(ctx); !processed || err != nil {
				t.Fatalf("recovery=%t %v", processed, err)
			}
			var status string
			var unknown int
			if err := pool.QueryRow(ctx, `SELECT w.status,(SELECT count(*) FROM tool_invocations WHERE agent_turn_id=$2 AND state IN ('UNKNOWN','RECONCILING')) FROM workflows w WHERE id=$1`, fixture.workflowID, lease.ID).Scan(&status, &unknown); err != nil {
				t.Fatal(err)
			}
			if status == "NEEDS_HUMAN" || unknown != 0 {
				t.Fatalf("status=%s unknown=%d", status, unknown)
			}
		})
	}
}

type noRecoveryGitHub struct{ mcp.GitHubAPI }
type noRecoveryCredentials struct{}

func (noRecoveryCredentials) DeveloperCredential(context.Context, mcp.RepositoryScope) (string, error) {
	panic("unexpected credentials")
}
func (noRecoveryCredentials) ReviewerCredential(context.Context, mcp.RepositoryScope) (string, error) {
	panic("unexpected credentials")
}

type noRecoveryPublications struct{}

func (noRecoveryPublications) ReconcilePublication(context.Context, workspace.PublicationReconciliation) (workspace.PublicationReconciliationResult, error) {
	panic("unexpected publication read")
}

func TestRecoveryDiagnosticsCountSelectedSuccessfulEffectsSeparately(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	for i, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded=%t", recorded), func(t *testing.T) {
			ctx := t.Context()
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 3990+i, workflow.RoleDeveloper, productionFixtureHead)
			execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			for j, tool := range []string{mcp.ToolRefreshDefaultBranch, mcp.ToolPublishChanges, mcp.ToolOpenPR, mcp.ToolRefreshDefaultBranch} {
				completeRecoveryFixture(t, database, lease, execution.Repository, j, tool)
			}
			if _, err := database.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Continue."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "pending"}, Authorized: true, At: time.Now()}); err != nil {
				t.Fatal(err)
			}
			mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "yield", ToolName: mcp.ToolRequestClarification, Request: json.RawMessage(`{"operation_id":"yield","questions":[]}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.RequestClarification(ctx, lease, mutation.ID, nil); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"outcome":"CLARIFICATION_WAIT","round_id":""}`)); err != nil {
				t.Fatal(err)
			}
			if recorded {
				err = database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", "")
			} else {
				err = database.CloseMutationAdmission(ctx, lease)
			}
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := database.BeginAgentTurnRecovery(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "diagnostic-stop")
			if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
				t.Fatal(err)
			}
			want := 5
			if recorded {
				want = 2
			}
			for attempt := 1; attempt <= 3; attempt++ {
				job := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "diagnostic-reconcile")
				candidates, err := database.ListAgentTurnMutationsForReconciliation(ctx, job)
				if err != nil || len(candidates) != want {
					t.Fatal(candidates, err)
				}
				ack, err := database.AcknowledgeAgentTurnMutationReconciliationFailure(ctx, job, errors.New("fixture verification unavailable"), 0)
				if err != nil || ack.UnresolvedMutationCount != 0 || ack.SuccessfulVerificationCount != want || ack.VerificationCandidateCount != want || ack.Escalated != (attempt == 3) {
					t.Fatal(ack, err)
				}
				if attempt == 3 {
					if !strings.Contains(ack.Diagnostic, fmt.Sprintf("0 unknown/reconciling mutations, %d successful effects awaiting verification (%d selected candidates)", want, want)) {
						t.Fatal(ack.Diagnostic)
					}
					stored := readStoredJob(t, pool, ctx, recovery.ReconcileMutationsJobID)
					var result struct {
						Unknown    int `json:"unresolved_mutation_count"`
						Succeeded  int `json:"successful_verification_count"`
						Candidates int `json:"verification_candidate_count"`
					}
					if err := json.Unmarshal(stored.Result, &result); err != nil || result.Unknown != 0 || result.Succeeded != want || result.Candidates != want {
						t.Fatalf("result=%s err=%v", stored.Result, err)
					}
				}
			}
			var succeeded, unknown int
			if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='SUCCEEDED'),count(*) FILTER (WHERE state IN ('UNKNOWN','RECONCILING')) FROM tool_invocations WHERE agent_turn_id=$1`, lease.ID).Scan(&succeeded, &unknown); err != nil {
				t.Fatal(err)
			}
			if succeeded != 5 || unknown != 0 {
				t.Fatalf("ledger rewritten: succeeded=%d unknown=%d", succeeded, unknown)
			}
		})
	}
}
