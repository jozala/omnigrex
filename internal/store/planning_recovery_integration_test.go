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
	for i, mode := range []string{"confirmation-wait", "complete"} {
		t.Run(mode, func(t *testing.T) {
			database := databases[0]
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 1100+i, workflow.RolePlanner, "")
			state := collaboration.State{}
			if err := state.StartPlanning(collaboration.OriginalRequest{Title: "Onboarding", Body: "A rough idea.", Source: collaboration.Source{Channel: "fixture", ID: "original"}}); err != nil {
				t.Fatal(err)
			}
			state.Prepare(fixture.assignmentID, "prior-turn")
			content := collaboration.PlanningContent{Title: "Onboarding", Status: "proposed", Changes: "No persistence", RequirementsBrief: "Ephemeral local checklist.", TechnicalPlan: "Use local transient state. Test reset.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
			if _, err := state.ReserveCheckpoint("P1", fixture.assignmentID, "prior-turn", content); err != nil {
				t.Fatal(err)
			}
			if err := state.ConfirmCheckpoint("P1", collaboration.Source{Channel: "fixture", ID: "checkpoint"}); err != nil {
				t.Fatal(err)
			}
			tool := "request_clarification"
			result := json.RawMessage(`{"outcome":"CLARIFICATION_WAIT","round_id":""}`)
			if mode == "complete" {
				receipt := state.Accept("confirmation", collaboration.Command{Kind: "confirm-plan", Target: "P1"}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: fixture.assignmentID, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "confirm"}, At: time.Now()})
				if receipt.ResponseID == "" {
					t.Fatal(receipt)
				}
				tool = "complete_planning"
				result = json.RawMessage(`{"outcome":"PLANNING_READY","revision_id":"P1","repository_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
			}
			state.Prepare(fixture.assignmentID, lease.ID)
			request := json.RawMessage(`{"operation_id":"terminal"}`)
			if mode == "complete" {
				request = json.RawMessage(`{"operation_id":"terminal","revision_id":"P1","repository_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","evidence":"Current code still supports the approach.","material_decisions_resolved":true}`)
			}
			mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "terminal", ToolName: tool, Request: request, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
				t.Fatal(err)
			}
			if mode == "complete" {
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
			if status != expectedState || outcome != expectedOutcome || waiting != (mode == "confirmation-wait") || failures != 0 || turnStatus != "SUCCEEDED" || successors != 0 || api.calls != 0 {
				t.Fatalf("unexpected recovery %s/%s wait=%t failures=%d turn=%s successors=%d reads=%d", status, outcome, waiting, failures, turnStatus, successors, api.calls)
			}
			if mode == "complete" {
				var raw []byte
				if err = pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var final collaboration.State
				if err = json.Unmarshal(raw, &final); err != nil || final.Planning.ReadyRevision != "P1" {
					t.Fatal("confirmed pair not preserved", err)
				}
			}
			if processed, err := worker.ProcessOne(ctx); err != nil || processed {
				t.Fatal("duplicate corroboration", processed, err)
			}
		})
	}
}
