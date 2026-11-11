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

func TestResolutionRejectionDurableReceiptAndFreshCorrection(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for n, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 9960+n, role, strings.Repeat("a", 40))
			var state collaboration.State
			_, err := state.Request("R1", lease.AgentParticipantID, lease.ID, "head", []collaboration.Question{{ID: "Q1", Body: "First?"}, {ID: "Q2", Body: "Second?"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = state.Publish("R1", collaboration.Source{ID: "published", Channel: "fixture"}); err != nil {
				t.Fatal(err)
			}
			receipt := state.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "private-human-text"}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "answer"}, At: time.Now()})
			if receipt.ResponseID != "A1" {
				t.Fatal(receipt)
			}
			state.Prepare(lease.AgentParticipantID, lease.ID)
			raw, _ := json.Marshal(state)
			if _, err = pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
				t.Fatal(err)
			}
			resolutions := []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "First"}, {QuestionID: "Q2", ResponseIDs: []string{"missing"}, Explanation: "Second"}}
			bad := startClarificationResolutionMutation(t, database, ctx, lease, "invalid-evidence", "R1", resolutions)
			for range 2 {
				if err = database.ResolveClarification(ctx, lease, bad.ID, "R1", resolutions); !errors.Is(err, collaboration.ErrInvalid) {
					t.Fatal(err)
				}
			}
			rejection, err := database.GetResolutionRejection(ctx, fixture.workflowID, lease.AgentParticipantID, bad.ID)
			if err != nil || rejection == nil || rejection.ResolutionIndex != 2 {
				t.Fatal(rejection, err)
			}
			if other, err := database.GetResolutionRejection(ctx, fixture.workflowID, "other", bad.ID); err != nil || other != nil {
				t.Fatal(other, err)
			}
			round, err := database.GetClarificationRound(ctx, fixture.workflowID, "R1")
			if err != nil || len(round.Resolutions) != 0 || round.HasResolutionEffect(bad.ID, resolutions) {
				t.Fatal(round, err)
			}
			// A lost acknowledgment must leave an exact receipt even when the gateway
			// has not completed the mutation. Unknown is not proof of non-application.
			if err = database.MarkMutationUnknown(ctx, lease, bad.ID, context.DeadlineExceeded); err != nil {
				t.Fatal(err)
			}
			resolutions[1].ResponseIDs = []string{"A1"}
			// The original reservation is still bound to its invalid arguments, even
			// when callers know the validation receipt: only a fresh identity can repair it.
			// UNKNOWN state also cannot be reapplied through direct Store admission.
			if err = database.ResolveClarification(ctx, lease, bad.ID, "R1", resolutions); !errors.Is(err, store.ErrMutationStateConflict) {
				t.Fatal("unknown operation reapplied", err)
			}
			corrected := startClarificationResolutionMutation(t, database, ctx, lease, "corrected", "R1", resolutions)
			if err = database.ResolveClarification(ctx, lease, corrected.ID, "R1", resolutions); err != nil {
				t.Fatal(err)
			}
			round, err = database.GetClarificationRound(ctx, fixture.workflowID, "R1")
			if err != nil || len(round.Resolutions) != 2 || !round.HasResolutionEffect(corrected.ID, resolutions) {
				t.Fatal(round, err)
			}
			if err = database.CompleteMutation(ctx, lease, corrected.ID, json.RawMessage(`{"recorded":true}`)); err != nil {
				t.Fatal(err)
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			recovery, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch)
			if err != nil {
				t.Fatal(err)
			}
			stop, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "rejection-stop", time.Minute)
			if err != nil || stop == nil {
				t.Fatal(stop, err)
			}
			if _, err = database.AcknowledgeRecoveredRuntimeStopped(ctx, *stop); err != nil {
				t.Fatal(err)
			}
			reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{}, Clarifications: database})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := mcp.NewRecoveryWorker(database, reconciler, mcp.RecoveryWorkerConfig{ClaimOwner: "rejection-recovery", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if processed, err := worker.ProcessNext(ctx); err != nil || !processed {
				t.Fatal(processed, err)
			}
			var mutationState, barrierState string
			var recovered []byte
			if err = pool.QueryRow(ctx, `SELECT state,result FROM tool_invocations WHERE id=$1`, bad.ID).Scan(&mutationState, &recovered); err != nil {
				t.Fatal(err)
			}
			if err = pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, recovery.ReconcileMutationsJobID).Scan(&barrierState); err != nil {
				t.Fatal(err)
			}
			if mutationState != "SUCCEEDED" || barrierState != "SUCCEEDED" || !strings.Contains(string(recovered), `"recorded": false`) || !strings.Contains(string(recovered), `"application": "not_applied"`) {
				t.Fatal(mutationState, barrierState, string(recovered))
			}
		})
	}
}
