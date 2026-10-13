//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestAgentTurnTerminalCaptureRequiredUsesDurableSessionAndTurnEvidence(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for index, test := range []struct {
		name              string
		neverBound        bool
		promptEvidence    bool
		toolEvidence      bool
		priorActivation   bool
		creatingLifecycle bool
		phase             execution.Phase
		noExecution       bool
		boundResource     bool
		want              bool
	}{
		{name: "never prepared", neverBound: true, want: false},
		{name: "existing Session without current reservation", want: false},
		{name: "existing Session current reserved phase", phase: execution.Reserved, want: false},
		{name: "existing Session current setup phase", phase: execution.Setup, want: false},
		{name: "existing Session ambiguous main dispatch", phase: execution.Main, want: true},
		{name: "existing Session main creation authoritatively had no effects", phase: execution.Main, noExecution: true, want: false},
		{name: "existing Session settled creation with main resource", phase: execution.Main, boundResource: true, want: true},
		{name: "Turn activity overrides no-effect creation", phase: execution.Main, noExecution: true, toolEvidence: true, want: true},
		{name: "terminal prompt ending survives creating lifecycle", phase: execution.Main, promptEvidence: true, creatingLifecycle: true, want: true},
		{name: "unstarted mutation survives recovery closure", phase: execution.Main, toolEvidence: true, creatingLifecycle: true, want: true},
		{name: "historical activation without current dispatch", neverBound: true, priorActivation: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var fixture agentFixture
			if test.neverBound {
				// Seed an unactivated Session rather than clearing its immutable identity.
				fixture = seedCreatingAgentSession(t, pool, 1600+index)
			} else {
				fixture = seedAgentSession(t, pool, 1600+index)
			}
			turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			key := execution.Key{AssignmentID: fixture.assignmentID, AgentSessionID: fixture.sessionID,
				AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
			setCreatingLifecycle := func() {
				t.Helper()
				if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'CREATING' WHERE id = $1`, fixture.sessionID); err != nil {
					t.Fatal(err)
				}
			}
			if test.priorActivation {
				if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET activated_at = clock_timestamp() WHERE id = $1`, fixture.sessionID); err != nil {
					t.Fatal(err)
				}
			}
			lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "capture-runtime", time.Minute, 1)
			if err != nil {
				t.Fatal(err)
			}
			if test.phase != "" {
				ledger := database.ExecutionLedger(lease)
				if _, _, err := ledger.ReserveExecution(ctx, key, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
					t.Fatal(err)
				}
				if test.phase != execution.Reserved {
					if err := ledger.BeginExecutionPhase(ctx, key, test.phase); err != nil {
						t.Fatal(err)
					}
				}
				if test.noExecution {
					if err := ledger.RecordNoExecution(ctx, key); err != nil {
						t.Fatal(err)
					}
				}
				if test.boundResource {
					if err := ledger.BindExecution(ctx, key, "known-main-resource"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.promptEvidence || test.toolEvidence {
				if err := database.OpenMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
			}
			if test.promptEvidence {
				if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
					t.Fatal(err)
				}
			}
			if test.toolEvidence {
				if _, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
					ToolName: "comment_on_issue", OperationID: "capture-evidence", Request: json.RawMessage(`{}`),
				}); err != nil {
					t.Fatal(err)
				}
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
				t.Fatal(err)
			}
			if test.creatingLifecycle {
				setCreatingLifecycle()
			}
			recovery, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "capture-stop", time.Minute)
			if err != nil || recovery == nil || recovery.AgentTurnID != turn.ID {
				t.Fatalf("claim recovery = (%v, %v)", recovery, err)
			}
			required, err := database.AgentTurnTerminalCaptureRequired(ctx, *recovery)
			if err != nil || required != test.want {
				t.Fatalf("capture required = (%t, %v), want %t", required, err, test.want)
			}
			// Missing physical inventory is deliberately not consulted. Neither
			// closing mutation admission nor retrying this read erases evidence.
			if repeated, err := database.AgentTurnTerminalCaptureRequired(ctx, *recovery); err != nil || repeated != required {
				t.Fatalf("repeated capture requirement = (%t, %v)", repeated, err)
			}
			if err := database.BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("delayed main dispatch after capture decision = %v, want revoked dispatch", err)
			}
			for _, mutate := range []func(*store.JobLease){
				func(stale *store.JobLease) { stale.LeaseToken = "17000000-0000-4000-8000-000000000001" },
				func(stale *store.JobLease) { stale.ExecutionEpoch++ },
				func(stale *store.JobLease) { stale.Attempt++ },
			} {
				stale := *recovery
				mutate(&stale)
				if required, err := database.AgentTurnTerminalCaptureRequired(ctx, stale); !required || !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
					t.Fatalf("stale capture requirement = (%t, %v), want fail-closed recovery fence loss", required, err)
				}
			}
		})
	}
}

func TestExistingSessionRecoveryCapturesOnlyCurrentMainDispatch(t *testing.T) {
	for index, test := range []struct {
		name           string
		mainDispatched bool
		noExecution    bool
	}{
		{name: "existing Session, never launched current Turn"},
		{name: "existing Session, ambiguous current main dispatch", mainDispatched: true},
		{name: "existing Session, current main creation had no effects", mainDispatched: true, noExecution: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			triggerPreparationWorkflow(t, database, ctx,
				fmt.Sprintf("7c000000-0000-4000-8000-%012d", index+1), fmt.Sprintf("7c100000-0000-4000-8000-%012d", index+1))
			root := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "capture-profile", "openai/capture")
			rootLease := acquireAndBindTurn(t, database, pool, ctx, root, "existing-capture-session")
			if err := phaseNineMainExecution(ctx, database, rootLease); err != nil {
				t.Fatal(err)
			}
			if err := database.OpenMutationAdmission(ctx, rootLease); err != nil {
				t.Fatal(err)
			}
			if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
				t.Fatal(err)
			}
			if settled, err := database.SettleAgentTurn(ctx, rootLease, failedSettlementObservation("prior runtime failed")); err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
				t.Fatalf("prior Turn settlement = (%#v, %v), want infrastructure retry", settled, err)
			}
			current := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "capture-profile", "openai/capture")
			if current.Session.ID != root.Session.ID || current.Session.RuntimeSessionID != root.Session.ID || current.Turn.RetryOfTurnID != root.Turn.ID {
				t.Fatal("regression fixture did not reuse the previous Turn's bound Session")
			}
			lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, current.Turn), current.Turn.ControlRevision, "capture-runtime", time.Minute, 1)
			if err != nil {
				t.Fatal(err)
			}
			key := execution.Key{AssignmentID: lease.AgentAssignmentID, AgentSessionID: lease.AgentSessionID,
				AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch,
				RuntimeProfileName: current.Participant.RuntimeProfileName, RuntimeProfileVersion: current.Participant.RuntimeProfileVersion}
			if test.mainDispatched {
				ledger := database.ExecutionLedger(lease)
				if _, _, err := ledger.ReserveExecution(ctx, key, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
					t.Fatal(err)
				}
				if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
					t.Fatal(err)
				}
				// No bind receipt: the creation acknowledgement may have been lost.
				if test.noExecution {
					// Production records this fact only after authoritative proof that
					// creation had no effects, such as a definitive no-image failure.
					if err := ledger.RecordNoExecution(ctx, key); err != nil {
						t.Fatal(err)
					}
				}
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
				t.Fatal(err)
			}
			cleaner := &phaseNineExactCleaner{}
			captures := 0
			unavailable := errors.New("no current environment available to capture")
			worker, err := agentturn.NewStopWorker(database, cleaner, phaseNineWorkspaceDiscarder{captureErr: unavailable, captures: &captures}, agentturn.StopWorkerConfig{
				ClaimOwner: "capture-stop", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
				IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Millisecond,
				TerminalCaptureRequired: database.AgentTurnTerminalCaptureRequired,
			})
			if err != nil {
				t.Fatal(err)
			}
			processed, err := worker.ProcessNext(ctx)
			if !processed {
				t.Fatal("recovery did not claim the source Turn")
			}
			if test.mainDispatched && !test.noExecution {
				if !errors.Is(err, unavailable) || captures != 1 || cleaner.calls != 0 {
					t.Fatalf("ambiguous main lost capture source: error %v, captures %d, removals %d", err, captures, cleaner.calls)
				}
			} else if err != nil || captures != 0 || cleaner.calls != 1 {
				t.Fatalf("unlaunched Turn failed recovery: error %v, captures %d, removals %d", err, captures, cleaner.calls)
			}
			if err := database.ExecutionLedger(lease).BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, store.ErrAgentTurnFenceLost) {
				t.Fatalf("late owner dispatch = %v, want revoked Turn fence", err)
			}
			if err := database.BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("late backend dispatch = %v, want revoked reservation", err)
			}
		})
	}
}

func TestUnpromptedCleanupProofRejectsSessionAndTurnActivity(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for index, evidence := range []string{"bound Session", "open admission", "prompt checkpoint", "invocation counter", "wrong runtime identity", "late invocation after proof"} {
		t.Run(evidence, func(t *testing.T) {
			var fixture agentFixture
			if evidence == "bound Session" {
				fixture = seedAgentSession(t, pool, 1700+index)
			} else {
				fixture = seedCreatingAgentSession(t, pool, 1700+index)
			}
			turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "proof-runtime", time.Minute, 1)
			if err != nil {
				t.Fatal(err)
			}
			key := execution.Key{AssignmentID: lease.AgentAssignmentID, AgentSessionID: lease.AgentSessionID,
				AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
			if evidence == "wrong runtime identity" {
				key.RuntimeProfileVersion = "other"
			}
			ledger := database.ExecutionLedger(lease)
			if _, _, err := ledger.ReserveExecution(ctx, key, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
				t.Fatal(err)
			}
			if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
				t.Fatal(err)
			}
			if err := ledger.BindExecution(ctx, key, "proof-main-resource"); err != nil {
				t.Fatal(err)
			}
			if evidence == "late invocation after proof" {
				if err := database.RecordAgentTurnUnpromptedCleanup(ctx, lease); err != nil {
					t.Fatal(err)
				}
				if err := database.RecordAgentTurnUnpromptedCleanup(ctx, lease); err != nil {
					t.Fatalf("idempotent cleanup proof = %v", err)
				}
			}
			var update string
			switch evidence {
			case "open admission":
				update = `UPDATE agent_turns SET mutation_admission_open = true WHERE id = $1`
			case "prompt checkpoint":
				update = `UPDATE agent_turns SET prompt_stop_reason = 'completed', prompt_recorded_at = clock_timestamp() WHERE id = $1`
			case "invocation counter", "late invocation after proof":
				update = `UPDATE agent_turns SET next_invocation_number = 2 WHERE id = $1`
			}
			if update != "" {
				if _, err := pool.Exec(ctx, update, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.RecordAgentTurnUnpromptedCleanup(ctx, lease); err == nil {
				t.Fatal("conflicting Session, activity or runtime identity admitted cleanup proof")
			}
			var proof bool
			if err := pool.QueryRow(ctx, `SELECT preparation_only_cleanup FROM execution_reservations
WHERE agent_turn_id = $1 AND execution_epoch = $2`, lease.ID, lease.ExecutionEpoch).Scan(&proof); err != nil || proof != (evidence == "late invocation after proof") {
				t.Fatalf("denied cleanup proof = (%t, %v)", proof, err)
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
				t.Fatal(err)
			}
			job, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "proof-stop", time.Minute)
			if err != nil || job == nil || job.AgentTurnID != lease.ID {
				t.Fatalf("claim proof recovery = (%v, %v)", job, err)
			}
			if evidence != "wrong runtime identity" {
				if required, err := database.AgentTurnTerminalCaptureRequired(ctx, *job); err != nil || !required {
					t.Fatalf("conflicting proof/activity capture requirement = (%t, %v)", required, err)
				}
			}
		})
	}
}
