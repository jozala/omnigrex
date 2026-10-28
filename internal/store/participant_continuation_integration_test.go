//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestWaitingDeveloperContinuesAfterReviewerOnlyAttempts(t *testing.T) {
	for _, scenario := range []struct {
		approvals int
		upgrade   bool
	}{{0, false}, {1, false}, {3, false}, {0, true}, {1, true}} {
		approvals := scenario.approvals
		t.Run(fmt.Sprintf("%d approvals/upgrade=%t", approvals, scenario.upgrade), func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			app := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000090", "62000000-0000-4000-8000-000000000090")
			prepare := func() store.AgentTurnPreparationCommit {
				return prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "profile", "openai/test")
			}
			settle := func(prepared store.AgentTurnPreparationCommit, outcome workflow.TurnOutcome, proposal *store.AgentTurnSettlementChangeProposal, reviewID int64) store.AgentTurnSettlement {
				lease := acquireAndBindTurn(t, database, pool, ctx, prepared, "runtime")
				if err := database.OpenMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				if err := database.CloseMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				observation := successfulSettlementObservation(outcome, proposal)
				if reviewID != 0 {
					if _, err := pool.Exec(ctx, `UPDATE agent_participants SET github_app_actor_id = 9001 WHERE id = $1`, prepared.Participant.ID); err != nil {
						t.Fatal(err)
					}
					observation.Review = &workflow.ReviewIdentity{ID: reviewID, NodeID: fmt.Sprintf("PRR_%d", reviewID), ChangeProposalID: proposal.PullRequestID, ActorID: 9001, HeadSHA: proposal.HeadSHA}
					observation.AuthorizedReviewerActorID = 9001
				}
				result, err := database.SettleAgentTurn(ctx, lease, observation)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			developer := prepare()
			proposal := &store.AgentTurnSettlementChangeProposal{WorkflowID: app.WorkflowID, RepositoryID: 9123, RepositoryOwner: "jozala", RepositoryName: "omnigrex", PullRequestID: 10, PullRequestNumber: 10, PullRequestNodeID: "PR_10", Status: "OPEN", Active: true, BaseRef: "main", BaseSHA: "base", HeadRef: "feature", HeadSHA: "head"}
			published := settle(developer, workflow.TurnOutcomeChangeProposalReady, proposal, 0)
			reviewer := prepare()
			handoff := settle(reviewer, workflow.TurnOutcomeBlocked, nil, 0)
			if handoff.State != workflow.StateNeedsHuman {
				t.Fatalf("handoff = %#v", handoff)
			}
			if scenario.upgrade {
				// Recreate the pre-upgrade ledger shape at an actual pending Handoff.
				// Execute the production backfill unchanged over the real schema.
				var pending bool
				if err := pool.QueryRow(ctx, `SELECT active AND completed_at IS NULL AND human_handoff_reason IS NOT NULL FROM workflow_attempts WHERE id = $1`, reviewer.Turn.WorkflowAttemptID).Scan(&pending); err != nil || !pending {
					t.Fatalf("pending Handoff = %t, %v", pending, err)
				}
				migration, err := migrations.Files.ReadFile("000003_participant_handoff_provenance.sql")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `ALTER TABLE agent_participants DROP COLUMN waiting_handoff_attempt_id`); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, string(migration)); err != nil {
					t.Fatal(err)
				}
				var backfilled int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_participants WHERE workflow_id = $1 AND status = 'WAITING_FOR_HUMAN' AND waiting_handoff_attempt_id = $2`, app.WorkflowID, reviewer.Turn.WorkflowAttemptID).Scan(&backfilled); err != nil || backfilled != 2 {
					t.Fatalf("backfilled waiting Participants = %d, %v; want 2", backfilled, err)
				}
			}
			for i := 0; i <= approvals; i++ {
				activation := triggerPreparationWorkflow(t, database, ctx, fmt.Sprintf("61000000-0000-4000-8000-%012d", 100+i), fmt.Sprintf("62000000-0000-4000-8000-%012d", 100+i))
				if activation.State != workflow.StateReviewing {
					t.Fatalf("activation = %#v", activation)
				}
				currentReviewer := prepare()
				if currentReviewer.Participant.ID != reviewer.Participant.ID || currentReviewer.Session.ID != reviewer.Session.ID {
					t.Fatal("Reviewer identity changed")
				}
				var status string
				if err := pool.QueryRow(ctx, `SELECT status FROM agent_participants WHERE id = $1`, developer.Participant.ID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "WAITING_FOR_HUMAN" {
					t.Fatalf("unused Developer status = %s", status)
				}
				outcome := workflow.TurnOutcomeApproved
				if i == approvals {
					outcome = workflow.TurnOutcomeChangesRequested
				}
				result := settle(currentReviewer, outcome, proposal, int64(100+i))
				if i < approvals && result.State != workflow.StatePRReady {
					t.Fatalf("approval = %#v", result)
				}
			}
			job := claimPreparationJob(t, database, ctx)
			var handoffAttemptID, triggerDeliveryID string
			if err := pool.QueryRow(ctx, `SELECT waiting_handoff_attempt_id::text FROM agent_participants WHERE id = $1`, developer.Participant.ID).Scan(&handoffAttemptID); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT trigger_delivery_id::text FROM workflow_attempts WHERE id = $1`, job.WorkflowAttemptID).Scan(&triggerDeliveryID); err != nil {
				t.Fatal(err)
			}
			// Invalid authority must not partially reactivate the waiting Participant.
			for _, fence := range []struct {
				name, invalidate, restore string
				id                        string
			}{
				{"missing Participant Handoff", "UPDATE agent_participants SET waiting_handoff_attempt_id = NULL WHERE id = $1", fmt.Sprintf("UPDATE agent_participants SET waiting_handoff_attempt_id = '%s' WHERE id = $1", handoffAttemptID), developer.Participant.ID},
				{"Attempt without human activation", "UPDATE workflow_attempts SET trigger_delivery_id = NULL WHERE id = $1", fmt.Sprintf("UPDATE workflow_attempts SET trigger_delivery_id = '%s' WHERE id = $1", triggerDeliveryID), job.WorkflowAttemptID},
				{"Workflow revision", "UPDATE workflows SET state_revision = state_revision + 1 WHERE id = $1", "UPDATE workflows SET state_revision = state_revision - 1 WHERE id = $1", app.WorkflowID},
				{"Workflow still needs human", "UPDATE workflows SET status = 'NEEDS_HUMAN' WHERE id = $1", "UPDATE workflows SET status = 'DEVELOPING' WHERE id = $1", app.WorkflowID},
				{"inactive Attempt", "UPDATE workflow_attempts SET active = FALSE WHERE id = $1", "UPDATE workflow_attempts SET active = TRUE WHERE id = $1", job.WorkflowAttemptID},
				{"human Session", "UPDATE agent_sessions SET control_owner = 'HUMAN' WHERE id = $1", "UPDATE agent_sessions SET control_owner = 'AUTOMATION' WHERE id = $1", developer.Session.ID},
				{"deleted Participant", "UPDATE agent_participants SET state_deleted_at = clock_timestamp() WHERE id = $1", "UPDATE agent_participants SET state_deleted_at = NULL WHERE id = $1", developer.Participant.ID},
				{"superseded Participant", "UPDATE agent_participants SET status = 'SUPERSEDED' WHERE id = $1", "UPDATE agent_participants SET status = 'WAITING_FOR_HUMAN' WHERE id = $1", developer.Participant.ID},
			} {
				if _, err := pool.Exec(ctx, fence.invalidate, fence.id); err != nil {
					t.Fatal(err)
				}
				if _, err := database.PrepareAgentTurn(ctx, job, preparationSpec("profile", "openai/test")); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
					t.Fatalf("%s: error = %v", fence.name, err)
				}
				if _, err := pool.Exec(ctx, fence.restore, fence.id); err != nil {
					t.Fatal(err)
				}
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT status = 'WAITING_FOR_HUMAN' AND reactivated_by_preparation_job_id IS NULL FROM agent_participants WHERE id = $1`, developer.Participant.ID).Scan(&waiting); err != nil || !waiting {
					t.Fatalf("%s partially reactivated Participant: %v", fence.name, err)
				}
			}
			continued := prepareTurn(t, database, ctx, job, "updated-profile", "openai/updated")
			if continued.Participant.ID != developer.Participant.ID || continued.Session.ID != developer.Session.ID || continued.Participant.ParticipantRuntimeBinding != developer.Participant.ParticipantRuntimeBinding || continued.Participant.RuntimeStatePath != developer.Participant.RuntimeStatePath {
				t.Fatal("Developer continuation changed identity or immutable runtime binding")
			}
			if continued.Turn.Stage != workflow.StageImplementation || continued.Turn.Purpose != workflow.TurnPurposeRequestedChanges || continued.Turn.ExpectedHeadSHA != "head" || continued.Turn.ChangeProposalID != published.ChangeProposalID || continued.Turn.TurnNumber != 2 || continued.Session.RuntimeSessionID != developer.Session.ID {
				t.Fatalf("continued Turn = %#v", continued.Turn)
			}
			if _, err := database.PrepareAgentTurn(ctx, job, preparationSpec("profile", "openai/test")); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
				t.Fatalf("duplicate error = %v", err)
			}
			var revision int64
			if err := pool.QueryRow(ctx, `SELECT state_revision FROM workflows WHERE id = $1`, app.WorkflowID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			insertPreparationJob(t, pool, app.WorkflowID, job.WorkflowAttemptID, revision, workflow.AssignmentGenerationCurrent, workflow.RoleDeveloper, workflow.TurnPurposeRequestedChanges, "head", "")
			competing := claimPreparationJob(t, database, ctx)
			if _, err := database.PrepareAgentTurn(ctx, competing, preparationSpec("profile", "openai/test")); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
				t.Fatalf("active-Turn fencing error = %v", err)
			}
			var turns int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turns WHERE agent_session_id = $1`, developer.Session.ID).Scan(&turns); err != nil || turns != 2 {
				t.Fatalf("Developer history count = %d, %v", turns, err)
			}
		})
	}
}
