//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"strings"
	"testing"
	"time"
)

func TestPlanningRecordedTerminalRecovery(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i, mode := range []string{"confirmation-wait", "complete", "complete-late-input"} {
		t.Run(mode, func(t *testing.T) {
			database := databases[0]
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 1100+i, workflow.RolePlanner, "")
			state := collaboration.State{}
			if err := state.StartPlanning(collaboration.OriginalRequest{Title: "Onboarding", Body: "A rough idea.", Source: collaboration.Source{Channel: "fixture", ID: "original"}}); err != nil {
				t.Fatal(err)
			}
			state.Prepare(fixture.assignmentID, "prior-turn")
			content := collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "Onboarding", Status: "proposed", Changes: "No persistence", RequirementsBrief: "Ephemeral local checklist.", TechnicalPlan: "Use local transient state. Test reset.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
			if _, err := state.ReserveCheckpoint("P1", fixture.assignmentID, "prior-turn", content); err != nil {
				t.Fatal(err)
			}
			if err := state.ConfirmCheckpoint("P1", collaboration.Source{Channel: "fixture", ID: "checkpoint"}); err != nil {
				t.Fatal(err)
			}
			tool := "request_clarification"
			result := json.RawMessage(`{"outcome":"CLARIFICATION_WAIT","round_id":""}`)
			if mode != "confirmation-wait" {
				receipt := state.Accept("confirmation", collaboration.Command{Kind: "confirm-plan", Target: "P1"}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: fixture.assignmentID, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "confirm"}, At: time.Now()})
				if receipt.ResponseID == "" {
					t.Fatal(receipt)
				}
				tool = "complete_planning"
				result = json.RawMessage(`{"outcome":"PLANNING_READY","revision_id":"P1","repository_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
			}
			state.Prepare(fixture.assignmentID, lease.ID)
			if mode != "confirmation-wait" {
				if err := state.Assess("assess-confirmation", fixture.assignmentID, lease.ID, []collaboration.Assessment{{InputID: "confirmation", EvidenceIDs: []string{"confirmation"}, Disposition: "confirmed", Explanation: "Exact published pair accepted."}}); err != nil {
					t.Fatal(err)
				}
			}
			request := json.RawMessage(`{"operation_id":"terminal"}`)
			if mode != "confirmation-wait" {
				request = json.RawMessage(`{"operation_id":"terminal","revision_id":"P1","repository_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","evidence":"Current code still supports the approach.","material_decisions_resolved":true}`)
			}
			mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "terminal", ToolName: tool, Request: request, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
				t.Fatal(err)
			}
			if mode != "confirmation-wait" {
				if err = state.ValidatePlanning(mutation.ID, "P1", fixture.assignmentID, lease.ID, strings.Repeat("a", 40), "Current code still supports the approach.", true); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(state)
			if _, err = pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2) ON CONFLICT(workflow_id) DO UPDATE SET state=EXCLUDED.state`, fixture.workflowID, raw); err != nil {
				t.Fatal(err)
			}
			if err = database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
				t.Fatal(err)
			}
			if err = database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
				t.Fatal(err)
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			if _, err = database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
				t.Fatal(err)
			}
			stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-planner")
			mutations := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "reconcile-planner")
			if _, err = database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
				t.Fatal(err)
			}
			if _, err = database.CompleteAgentTurnMutationReconciliation(ctx, mutations); err != nil {
				t.Fatal(err)
			}
			recovered, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
			if err != nil || recovered.Status != store.AgentTurnCorroborating || recovered.SuccessorAllowed {
				t.Fatal("terminal intent lost to retry", recovered, err)
			}
			if mode == "complete-late-input" {
				receipt, err := database.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Withdraw my confirmation; we need to revise retention."}, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "late-steering"}, Authorized: true, At: time.Now()})
				if err != nil || receipt.ResponseID == "" {
					t.Fatal("late input not admitted", receipt, err)
				}
			}
			workspaces, _ := integrationTerminalWorkspace(t, pool, lease, committedPublicationPaths(t), "head", "installation-token")
			credentials := &integrationCredentialProvider{credential: "installation-token"}
			api := &replayOutcomeGitHub{}
			outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: databases[1], GitHub: api, Workspace: workspaces, DeveloperCredentials: credentials, ReviewerCredentials: credentials})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := agentturn.NewTerminalCorroborationWorker(databases[1], outcomes, credentials, credentials, workspaces, agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "planner-recovery", Window: time.Minute, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
				t.Fatal(processed, err)
			}
			var status, outcome, turnStatus string
			var waiting bool
			var failures, successors int
			if err = pool.QueryRow(ctx, `SELECT w.status,w.clarification_waiting,a.infrastructure_failures,s.workflow_outcome,t.status,(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')) FROM workflows w JOIN workflow_attempts a ON a.id=$2 JOIN agent_turns t ON t.id=$3 JOIN agent_turn_settlements s ON s.agent_turn_id=t.id WHERE w.id=$1`, fixture.workflowID, fixture.attemptID, lease.ID).Scan(&status, &waiting, &failures, &outcome, &turnStatus, &successors); err != nil {
				t.Fatal(err)
			}
			expectedState, expectedOutcome := "PLANNING", "CLARIFICATION_WAIT"
			if mode == "complete" {
				expectedState = "PLAN_READY"
				expectedOutcome = "PLANNING_READY"
			}
			expectedSuccessors := 0
			if mode == "complete-late-input" {
				expectedSuccessors = 1
			}
			if status != expectedState || outcome != expectedOutcome || waiting != (mode != "complete") || failures != 0 || turnStatus != "SUCCEEDED" || successors != expectedSuccessors || api.calls != 0 {
				t.Fatalf("unexpected recovery %s/%s wait=%t failures=%d turn=%s successors=%d reads=%d", status, outcome, waiting, failures, turnStatus, successors, api.calls)
			}
			if mode != "confirmation-wait" {
				var raw []byte
				if err = pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var final collaboration.State
				expectedReady := "P1"
				if mode == "complete-late-input" {
					expectedReady = ""
				}
				if err = json.Unmarshal(raw, &final); err != nil || final.Planning.ReadyRevision != expectedReady {
					t.Fatal("incorrect readiness after recovery", final.Planning, err)
				}
				if mode == "complete-late-input" && !final.Pending(fixture.assignmentID) {
					t.Fatal("late input was lost or marked delivered")
				}
			}
			if processed, err := worker.ProcessOne(ctx); err != nil || processed {
				t.Fatal("duplicate corroboration", processed, err)
			}
		})
	}
}

func TestPlanningRetrySurvivesDeferredReopenedEvent(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 1120, workflow.RolePlanner, "")
	delivery := workflowDelivery("11200000-0000-4000-8000-000000000001")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 1120, 1120, 1120
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	locator := store.WorkflowLocator{RepositoryID: 1120, IssueID: 1120, IssueNumber: 1120}
	factory := func(c store.WorkflowEventContext) (workflow.Event, error) {
		return workflow.IssueReopenedEvent{EventMetadata: c.Metadata}, nil
	}
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "reopened"), locator, factory)
	if err != nil || application.Status != store.NormalizedEventDeferred {
		t.Fatal(application, err)
	}
	if err = database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, lease, failedSettlementObservation("runtime failed"))
	if err != nil || settled.ReconciliationJobID == "" {
		t.Fatal(settled, err)
	}
	job, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "planner-retry", time.Minute)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	ack, err := database.AcknowledgePendingEventReconciliation(ctx, *job, func(store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowEventFactory, error) {
		return locator, factory, nil
	})
	if err != nil || ack.SuccessorJobID == "" {
		t.Fatal("Planner retry dropped", ack, err)
	}
	var state, role, purpose string
	var waiting bool
	if err = pool.QueryRow(ctx, `SELECT w.status,w.clarification_waiting,j.payload->>'role',j.payload->>'purpose' FROM workflows w JOIN jobs j ON j.id=$2 WHERE w.id=$1`, fixture.workflowID, ack.SuccessorJobID).Scan(&state, &waiting, &role, &purpose); err != nil {
		t.Fatal(err)
	}
	if state != "PLANNING" || waiting || role != "PLANNER" || purpose != "RETRY" {
		t.Fatal(state, waiting, role, purpose)
	}
}

func TestInitialPlanningPreparationSupportsOriginalReadAndCheckpoint(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture := seedAgentSessionForRole(t, pool, 1121, workflow.RolePlanner)
	state := collaboration.State{}
	original := collaboration.OriginalRequest{Title: "Checklist", Body: "A rough idea before any questions", Source: collaboration.Source{Channel: "fixture", ID: "original"}}
	if err := state.StartPlanning(original); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status='PLANNING',planning_only=true WHERE id=$1;`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET current_stage='planning' WHERE id=$1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	spec := fixture.turnSpec()
	spec.Stage, spec.Purpose = workflow.StagePlanning, workflow.TurnPurposeInitialPlanning
	spec.AgentProfileConfig = agentProfileConfig("planner", workflow.RolePlanner, "pi-rpc/v2", "provider/test", "", 0, "Plan only", nil)
	turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	job := agentTurnExecutionJob(t, pool, ctx, turn)
	lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, job, turn.ControlRevision, "initial-planner", time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	page, err := database.GetCollaborationRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, "planning_request", 0, 0)
	if err != nil {
		t.Fatal("original request unavailable", err)
	}
	var got collaboration.OriginalRequest
	if err = json.Unmarshal([]byte(page.Content), &got); err != nil || got != original {
		t.Fatal(got, err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "initial-pair", ToolName: "publish_plan_checkpoint", Request: json.RawMessage(`{"operation_id":"initial-pair"}`), ExternalService: "github", ExternalResourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	revision, err := database.ReservePlanningCheckpoint(ctx, lease, mutation.ID, collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "Checklist", Status: "draft", Changes: "Initial investigation", RequirementsBrief: "Scope proposed, not agreed", TechnicalPlan: "Inspect local checklist approach", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}})
	if err != nil || revision.ID == "" {
		t.Fatal("initial checkpoint rejected", revision, err)
	}
	if err = database.ConfirmPlanningPublication(ctx, fixture.workflowID, revision.ID, collaboration.Source{Channel: "fixture", ID: "initial-checkpoint"}); err != nil {
		t.Fatal(err)
	}
}
