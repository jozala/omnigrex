//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestSubscriptionFailureAfterInterruptedCleanupStillHandsOffWithoutRetry(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for index, class := range []string{"SUBSCRIPTION_QUOTA", "SUBSCRIPTION_AUTH", "SUBSCRIPTION_CONFIGURATION"} {
		t.Run(class, func(t *testing.T) {
			_, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 3100+index, workflow.RoleDeveloper, "")
			if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "", class); err != nil {
				t.Fatal(err)
			}
			if _, err := database.BeginAgentTurnRecovery(ctx, lease); err != nil {
				t.Fatal(err)
			}
			stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "subscription-stop")
			if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
				t.Fatal(err)
			}
			var outcome, terminalStatus, diagnostic, state string
			var noSuccessor bool
			if err := pool.QueryRow(ctx, `SELECT workflow_outcome, terminal_turn_status, terminal_last_error, workflow_state, successor_job_id IS NULL FROM agent_turn_settlements WHERE agent_turn_id = $1`, lease.ID).Scan(&outcome, &terminalStatus, &diagnostic, &state, &noSuccessor); err != nil {
				t.Fatal(err)
			}
			if outcome != string(workflow.TurnOutcomePrerequisiteFailed) || terminalStatus != string(store.AgentTurnFailed) || state != string(workflow.StateNeedsHuman) || !strings.HasPrefix(diagnostic, "subscription_") || !noSuccessor {
				t.Fatalf("recovered subscription failure = %s/%s/%s, no successor %t", outcome, terminalStatus, state, noSuccessor)
			}
		})
	}
}

func TestSubscriptionAdmissionFailureCanBeRecordedBeforeMutationAdmission(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	database := databases[0]
	_, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 3110, workflow.RoleDeveloper, "")
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET mutation_admission_open = FALSE, mutation_admission_closed_at = NULL WHERE id = $1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "", "SUBSCRIPTION_QUOTA"); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, lease, store.AgentTurnSettlementObservation{ObservedAt: time.Now(), Outcome: workflow.TurnOutcomePrerequisiteFailed, Diagnostic: "subscription_quota: operator recovery required", Completion: store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "subscription_quota: operator recovery required"}})
	if err != nil || settled.State != workflow.StateNeedsHuman || settled.SuccessorJobID != "" {
		t.Fatalf("early prerequisite settlement = %+v, %v", settled, err)
	}
}
