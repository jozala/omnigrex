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

func startClarificationResolutionMutation(t *testing.T, database *store.Store, ctx context.Context, lease store.AgentTurnLease, label, round string, resolutions []collaboration.Resolution) store.MutationReservation {
	t.Helper()
	request, err := json.Marshal(map[string]any{"operation_id": label, "round_id": round, "resolutions": resolutions})
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: "resolve_clarification", OperationID: label, Request: request, ExternalService: "omnigrex", ExternalResourceID: lease.JobLease.WorkflowID})
	if err != nil {
		t.Fatal(err)
	}
	mutation, err = database.StartMutation(ctx, lease, mutation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return mutation
}

func TestClarificationResolutionRecoveryRequiresThisOperationsEffect(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for n, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		for i, applied := range []bool{false, true} {
			name := "cancelled before effect"
			if applied {
				name = "lost response after effect"
			}
			t.Run(string(role)+"/"+name, func(t *testing.T) {
				fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 9940+n*2+i, role, strings.Repeat("a", 40))
				var state collaboration.State
				if _, err := state.Request("R1", lease.AgentParticipantID, lease.ID, "head", []collaboration.Question{{ID: "Q1", Body: "Compatibility policy?"}}); err != nil {
					t.Fatal(err)
				}
				if err := state.Publish("R1", collaboration.Source{Channel: "fixture", ID: "publication"}); err != nil {
					t.Fatal(err)
				}
				a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "answer"}, At: time.Now()}
				if receipt := state.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Keep"}, a); receipt.ResponseID != "A1" {
					t.Fatal(receipt)
				}
				a.Source.ID = "correction"
				if receipt := state.Accept("A2", collaboration.Command{Kind: "amend", Target: "A1", Body: "The explicit correction governs"}, a); receipt.ResponseID != "A2" {
					t.Fatal(receipt)
				}
				state.Prepare(lease.AgentParticipantID, lease.ID)
				raw, _ := json.Marshal(state)
				if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
					t.Fatal(err)
				}
				resolutions := []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A2"}, Explanation: "The explicit correction governs"}}
				earlier := startClarificationResolutionMutation(t, database, ctx, lease, "earlier-operation", "R1", resolutions)
				if err := database.ResolveClarification(ctx, lease, earlier.ID, "R1", resolutions); err != nil {
					t.Fatal(err)
				}
				if err := database.ResolveClarification(ctx, lease, earlier.ID, "R1", resolutions); err != nil {
					t.Fatal("own committed effect could not be replayed", err)
				}
				if err := database.CompleteMutation(ctx, lease, earlier.ID, json.RawMessage(`{"round_id":"R1","recorded":true}`)); err != nil {
					t.Fatal(err)
				}
				newer := startClarificationResolutionMutation(t, database, ctx, lease, "new-operation", "R1", resolutions)
				altered := append([]collaboration.Resolution(nil), resolutions...)
				altered[0].Explanation = "Not the reserved effect"
				if err := database.ResolveClarification(ctx, lease, newer.ID, "R1", altered); !errors.Is(err, store.ErrMutationOperationConflict) {
					t.Fatalf("different effect borrowed reservation: %v", err)
				}
				if err := database.ResolveClarification(ctx, lease, earlier.ID, "R1", resolutions); !errors.Is(err, store.ErrMutationStateConflict) {
					t.Fatalf("completed reservation was reapplied: %v", err)
				}
				// The new request's interpretation is identical to the earlier operation.
				// Cancellation before Store admission must not acquire its historical proof.
				if applied {
					if err := database.ResolveClarification(ctx, lease, newer.ID, "R1", resolutions); err != nil {
						t.Fatal(err)
					}
				} else {
					cancelled, stop := context.WithCancel(ctx)
					stop()
					if err := database.ResolveClarification(cancelled, lease, newer.ID, "R1", resolutions); !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled effect = %v", err)
					}
				}
				if err := database.MarkMutationUnknown(ctx, lease, newer.ID, context.Canceled); err != nil {
					t.Fatal(err)
				}
				round, err := database.GetClarificationRound(ctx, fixture.workflowID, "R1")
				if err != nil {
					t.Fatal(err)
				}
				expected := 1
				if applied {
					expected = 2
				}
				if len(round.Resolutions) != expected || round.Resolutions[0].OriginOperationID != earlier.ID {
					t.Fatal("origin evidence rewrote earlier history", round)
				}
				if applied && round.Resolutions[1].OriginOperationID != newer.ID {
					t.Fatal("applied effect was not bound to its reservation")
				}
				// Use the real stopped-Turn recovery worker and durable mutation barrier,
				// not a synthetic FOUND outcome fed directly into the Store.
				expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
				recovery, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch)
				if err != nil {
					t.Fatal(err)
				}
				stopLease, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "resolution-stop", time.Minute)
				if err != nil || stopLease == nil {
					t.Fatal(stopLease, err)
				}
				if _, err = database.AcknowledgeRecoveredRuntimeStopped(ctx, *stopLease); err != nil {
					t.Fatal(err)
				}
				reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{}, Clarifications: database})
				if err != nil {
					t.Fatal(err)
				}
				worker, err := mcp.NewRecoveryWorker(database, reconciler, mcp.RecoveryWorkerConfig{ClaimOwner: "resolution-recovery", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				processed, err := worker.ProcessNext(ctx)
				if !processed || (applied && err != nil) || (!applied && !errors.Is(err, mcp.ErrMutationReconciliationUnresolved)) {
					t.Fatalf("recovery processed=%t applied=%t error=%v", processed, applied, err)
				}
				var mutationState, barrierState string
				var result []byte
				if err = pool.QueryRow(ctx, `SELECT state,result FROM tool_invocations WHERE id=$1`, newer.ID).Scan(&mutationState, &result); err != nil {
					t.Fatal(err)
				}
				if err = pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, recovery.ReconcileMutationsJobID).Scan(&barrierState); err != nil {
					t.Fatal(err)
				}
				if applied {
					if mutationState != "SUCCEEDED" || barrierState != "SUCCEEDED" || !strings.Contains(string(result), `"recorded": true`) {
						t.Fatalf("own committed effect not recovered: %s %s %s", mutationState, barrierState, result)
					}
				} else {
					if mutationState != "UNKNOWN" || barrierState == "SUCCEEDED" || len(result) != 0 {
						t.Fatalf("cancelled new operation borrowed success: %s %s %s", mutationState, barrierState, result)
					}
					if _, err = database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
						t.Fatalf("cancelled operation released barrier: %v", err)
					}
				}
				// Keep this negative case's scheduled retry from being claimed by the
				// next independent fixture in this shared test database.
				if _, err = pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp()+interval '1 hour' WHERE id=$1 AND status='AVAILABLE'`, recovery.ReconcileMutationsJobID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
