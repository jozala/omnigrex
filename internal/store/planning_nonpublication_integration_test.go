//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestPlanningNonPublicationRecovery(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for n, mode := range []string{"rejection-commit-ack-lost", "crash-before-reservation", "crash-without-input", "published-control", "oversize-commit-ack-lost", "crash-after-reservation", "historical-rejection-commit-ack-lost"} {
		t.Run(mode, func(t *testing.T) {
			database := databases[0]
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 1130+n, workflow.RolePlanner, "")
			state := collaboration.State{}
			if err := state.StartPlanning(collaboration.OriginalRequest{Title: "CLI", Body: "Rough idea", Source: collaboration.Source{Channel: "fixture", ID: "original"}}); err != nil {
				t.Fatal(err)
			}
			c := collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "CLI", Status: "draft", Changes: "Audience proposal", RequirementsBrief: "Repository collaborators", TechnicalPlan: "Local CLI. Verify access.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
			state.Prepare(lease.AgentParticipantID, "prior-turn")
			if _, err := state.ReserveCheckpoint("Pprior", lease.AgentParticipantID, "prior-turn", c); err != nil {
				t.Fatal(err)
			}
			if err := state.ConfirmCheckpoint("Pprior", collaboration.Source{Channel: "fixture", ID: "prior"}); err != nil {
				t.Fatal(err)
			}
			a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "human", Name: "Maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "answer"}, At: time.Now()}
			if r := state.Accept("Afirst", collaboration.Command{Kind: "steer", Body: "Use a collaborator CLI."}, a); r.ResponseID == "" {
				t.Fatal(r)
			}
			if mode == "historical-rejection-commit-ack-lost" {
				round, err := state.Request("historical", lease.AgentParticipantID, "prior-turn", "", []collaboration.Question{{ID: "success", Body: "Success?"}, {ID: "selection", Body: "Selected checks?"}})
				if err != nil {
					t.Fatal(err)
				}
				if err = state.Publish(round.ID, collaboration.Source{Channel: "fixture", ID: "historical"}); err != nil {
					t.Fatal(err)
				}
				state.Prepare(lease.AgentParticipantID, "interpret-original")
				if err = state.Assess("original", lease.AgentParticipantID, "interpret-original", []collaboration.Assessment{{InputID: "Afirst", EvidenceIDs: []string{"Afirst"}, Disposition: "changed", Explanation: "Accept both"}}); err != nil {
					t.Fatal(err)
				}
				if err = state.Resolve(round.ID, "interpret-original", []collaboration.Resolution{{QuestionID: "success", ResponseIDs: []string{"Afirst"}, Explanation: "Success"}, {QuestionID: "selection", ResponseIDs: []string{"Afirst"}, Explanation: "Selection"}}); err != nil {
					t.Fatal(err)
				}
				a.Source.ID = "correction"
				if receipt := state.Accept("Acorrection", collaboration.Command{Kind: "steer", Body: "Change success, keep selection"}, a); receipt.ResponseID == "" {
					t.Fatal(receipt)
				}
			}
			state.Prepare(lease.AgentParticipantID, lease.ID)
			if mode == "historical-rejection-commit-ack-lost" {
				if err := state.Assess("correct", lease.AgentParticipantID, lease.ID, []collaboration.Assessment{{InputID: "Acorrection", EvidenceIDs: []string{"Afirst", "Acorrection"}, Corrects: []string{"Afirst"}, Disposition: "changed", Explanation: "Explicit correction"}}); err != nil {
					t.Fatal(err)
				}
				if err := state.Resolve("historical", lease.ID, []collaboration.Resolution{{QuestionID: "success", ResponseIDs: []string{"Afirst", "Acorrection"}, Explanation: "Reaffirm success"}}); err != nil {
					t.Fatal(err)
				}
				c.Status = "proposed"
			}
			raw, _ := json.Marshal(state)
			if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
				t.Fatal(err)
			}
			if mode == "oversize-commit-ack-lost" {
				c.RequirementsBrief = strings.Repeat("界", 10666)
				c.TechnicalPlan = strings.Repeat("p", 32768)
			}
			request, _ := json.Marshal(map[string]any{"operation_id": "checkpoint", "content": c, "signature": "fixture-signature"})
			spec := store.MutationSpec{OperationID: "checkpoint", ToolName: "publish_plan_checkpoint", Request: request, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID}
			mutation, err := database.ReserveMutation(ctx, lease, spec)
			if err != nil {
				t.Fatal(err)
			}
			mutation, err = database.StartMutation(ctx, lease, mutation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "rejection-commit-ack-lost" || mode == "crash-before-reservation" {
				// Sequential barriers: preparation completed, then both real admissions
				// commit, then reservation runs. No scheduler timing or sleeps involved.
				for i, command := range []collaboration.Command{{Kind: "amend", Target: "A1", Body: "Users have no repository access."}, {Kind: "steer", Body: "Use a standalone operator CLI."}} {
					r, err := database.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: command, Actor: a.Actor, Source: collaboration.Source{Channel: "fixture", ID: []string{"amend", "steer"}[i]}, Authorized: true, At: a.At.Add(time.Duration(i) * time.Second)})
					if err != nil || r.ResponseID == "" {
						t.Fatal(r, err)
					}
				}
			}
			if mode == "rejection-commit-ack-lost" || mode == "historical-rejection-commit-ack-lost" {
				_, err = database.ReservePlanningCheckpoint(ctx, lease, mutation.ID, c)
				var rejection *collaboration.CheckpointNotApplied
				wantReason := "late_input"
				if mode == "historical-rejection-commit-ack-lost" {
					wantReason = "historical_decisions_unresolved"
				}
				if !errors.As(err, &rejection) || rejection.Reason != wantReason {
					t.Fatal(err)
				}
				// The gateway has not yet durably completed the operation; restart reads
				// the committed pre-publication evidence, independent of result recording.
				if _, err = databases[1].GetPlanningCheckpoint(ctx, fixture.workflowID, "P"+mutation.ID); !errors.Is(err, collaboration.ErrInvalid) {
					t.Fatal("rejection reserved a revision", err)
				}
			}
			if mode == "oversize-commit-ack-lost" || mode == "crash-after-reservation" {
				r, err := database.ReservePlanningCheckpoint(ctx, lease, mutation.ID, c)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "oversize-commit-ack-lost" {
					if err = database.RejectPlanningPublication(ctx, lease, r.ID, "payload_too_large"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "published-control" {
				revision, err := database.ReservePlanningCheckpoint(ctx, lease, mutation.ID, c)
				if err != nil {
					t.Fatal(err)
				}
				if err = database.ConfirmPlanningPublication(ctx, fixture.workflowID, revision.ID, collaboration.Source{Channel: "fixture", ID: "published"}); err != nil {
					t.Fatal(err)
				}
			}
			// Bounds were frozen before any of these effects. The same failure says
			// nothing about either successful publication or definite non-publication.
			if _, err = database.GetCollaborationRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, "planning_history", 2, 0); err == nil {
				t.Fatal("history bounds widened")
			}
			if err = database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("acknowledgment lost")); err != nil {
				t.Fatal(err)
			}
			if mode == "crash-before-reservation" || mode == "crash-without-input" || mode == "crash-after-reservation" {
				if _, err = database.ReconcilePlanningNonPublication(ctx, fixture.workflowID, lease.ID, mutation.ID); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
					t.Fatal("absence was treated as proof while runtime live", err)
				}
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			if _, err = database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
				t.Fatal(err)
			}
			stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "stop-nonpublication")
			recovery := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "reconcile-nonpublication")
			if _, err = database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
				t.Fatal(err)
			}
			mutation.State = store.MutationUnknown
			scope, err := databases[1].GetAgentTurnMutationReconciliationContext(ctx, recovery)
			if err != nil {
				t.Fatal(err)
			}
			reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{}, Clarifications: databases[1]})
			if err != nil {
				t.Fatal(err)
			}
			var first json.RawMessage
			for range 2 {
				result, err := reconciler.Reconcile(ctx, scope, mutation)
				if mode == "published-control" {
					// This fixture has no external artifact. A reservation/confirmation alone
					// must not turn a failed exact-artifact read into proof of non-publication.
					if err == nil && result.Disposition == mcp.ReconciliationFound {
						t.Fatal("invented external evidence")
					}
					break
				}
				if err != nil || result.Disposition != mcp.ReconciliationFound {
					t.Fatal(result, err)
				}
				var got map[string]any
				if err = json.Unmarshal(result.Outcome.Result, &got); err != nil {
					t.Fatal(err)
				}
				want := "late_input"
				if mode == "historical-rejection-commit-ack-lost" {
					want = "historical_decisions_unresolved"
					if got["next_action"] != "reconcile_historical_decisions" || !strings.Contains(string(result.Outcome.Result), `"question_id":"selection"`) {
						t.Fatal(got)
					}
				}
				if mode == "crash-without-input" {
					want = "interrupted_before_reservation"
				}
				if mode == "oversize-commit-ack-lost" {
					want = "payload_too_large"
				}
				if mode == "crash-after-reservation" {
					want = "interrupted_before_publication"
				}
				if got["publication"] != "not_applied" || got["reason"] != want || got["revision_id"] != nil {
					t.Fatal(got)
				}
				if first != nil && string(first) != string(result.Outcome.Result) {
					t.Fatal("recovery result changed")
				}
				first = result.Outcome.Result
			}
			if mode != "published-control" {
				if _, err = database.ReconcileRecoveredMutation(ctx, recovery, mutation.ID, store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: first}); err != nil {
					t.Fatal(err)
				}
				if _, err = database.CompleteAgentTurnMutationReconciliation(ctx, recovery); err != nil {
					t.Fatal(err)
				}
			}
			if err = pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			expected := 1
			if mode == "published-control" || mode == "oversize-commit-ack-lost" || mode == "crash-after-reservation" {
				expected = 2
			}
			if len(state.Planning.Revisions) != expected || state.Planning.Revisions[0].Content.RequirementsBrief != "Repository collaborators" {
				t.Fatal("changed prior revision", state.Planning)
			}
			if mode == "rejection-commit-ack-lost" || mode == "crash-before-reservation" {
				if len(state.Inputs) != 3 || state.Inputs[1].Body != "Users have no repository access." || state.Inputs[1].Amends != "Afirst" || state.Inputs[1].Source.ID != "amend" || state.Inputs[1].Actor.ID != "human" || state.Inputs[1].DeliveredTo != "" {
					t.Fatal("lost late input or provenance", state.Inputs)
				}
			}
		})
	}
}

func TestPlanningUnreadInputRequiresDurableCoverage(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	database := databases[0]
	fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 1139, workflow.RolePlanner, "")
	state := collaboration.State{}
	if err := state.StartPlanning(collaboration.OriginalRequest{Title: "CLI", Source: collaboration.Source{Channel: "fixture", ID: "original"}}); err != nil {
		t.Fatal(err)
	}
	if r := state.Accept("A1", collaboration.Command{Kind: "steer", Body: strings.Repeat("\\\"界", 40000)}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "input"}, At: time.Now()}); r.ResponseID == "" {
		t.Fatal(r)
	}
	state.Prepare(lease.AgentParticipantID, lease.ID)
	config, _ := json.Marshal(state.Project(lease.AgentParticipantID, lease.ID))
	raw, _ := json.Marshal(state)
	if !state.Inputs[0].DeliveryPending {
		t.Fatal("fixture did not require retrieval")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET turn_configuration=$2 WHERE id=$1`, lease.ID, config); err != nil {
		t.Fatal(err)
	}
	c := collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "CLI", Status: "draft", Changes: "Scope", RequirementsBrief: "Standalone", TechnicalPlan: "Local. Test reset.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}, DecisionResponseIDs: []string{"A1"}}
	reserve := func(label string) (collaboration.PlanningRevision, error) {
		request, _ := json.Marshal(map[string]any{"operation_id": label, "content": c})
		m, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: "publish_plan_checkpoint", OperationID: label, Request: request, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = database.StartMutation(ctx, lease, m.ID); err != nil {
			t.Fatal(err)
		}
		return database.ReservePlanningCheckpoint(ctx, lease, m.ID, c)
	}
	assertUnread := func(label string) {
		t.Helper()
		_, err := reserve(label)
		var r *collaboration.CheckpointNotApplied
		if !errors.As(err, &r) || r.Reason != "unread_input" {
			t.Fatal(err)
		}
	}
	assertUnread("unread")
	for offset := 0; ; {
		page, err := database.GetCollaborationRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, "human_input", 0, offset)
		if err != nil {
			t.Fatal(err)
		}
		if page.NextOffset == nil {
			assertUnread("final-chunk-not-recorded")
		}
		request, _ := json.Marshal(map[string]any{"collection": "human_input", "index": 0, "offset": offset})
		result, _ := json.Marshal(page)
		if _, err = databases[1].RecordReadInvocation(ctx, lease, store.ReadInvocation{ToolName: "get_collaboration_record", Request: request, Result: result, StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if r, err := reserve("after-coverage"); err != nil || r.ID == "" {
		t.Fatal(r, err)
	}
}
