//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
)

func TestAgentTurnAllocationIsMonotonicAndAllowsOnlyOneActiveTurnPerSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := make(chan struct{})
	turns := make(chan store.AgentTurn, 8)
	errs := make(chan error, 8)
	var wait sync.WaitGroup
	for index := range 8 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			turn, err := databases[index%2].AllocateAgentTurn(ctx, fixture.turnSpec())
			turns <- turn
			errs <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(turns)
	close(errs)
	var allocated store.AgentTurn
	successes := 0
	activeErrors := 0
	for turn := range turns {
		if turn.ID != "" {
			allocated = turn
			successes++
		}
	}
	for err := range errs {
		if errors.Is(err, store.ErrAgentTurnActive) {
			activeErrors++
		} else if err != nil {
			t.Errorf("AllocateAgentTurn() error = %v", err)
		}
	}
	if successes != 1 || activeErrors != 7 {
		t.Fatalf("concurrent allocations = %d success, %d active errors, want 1 and 7", successes, activeErrors)
	}
	if allocated.TurnNumber != 1 || allocated.ExecutionEpoch != 1 {
		t.Errorf("first turn identity = (%d, %d), want (1, 1)", allocated.TurnNumber, allocated.ExecutionEpoch)
	}

	job, err := databases[0].ClaimJob(ctx, store.AgentTurnQueue, "job-worker", time.Second)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() for allocated turn = (%#v, %v), want job", job, err)
	}
	if job.Kind != store.RunAgentTurnJobKind || job.MaxAttempts != 1 || job.AgentTurnID != allocated.ID || job.ExecutionEpoch != 1 {
		t.Errorf("allocated turn job = %#v, want single-attempt RUN_AGENT_TURN identity", job)
	}

	lease, err := databases[0].AcquireAgentTurn(ctx, *job, allocated.ControlRevision, "runtime-a", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	if err := databases[0].CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	if err := databases[0].FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnSucceeded, Outcome: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatalf("FinalizeAgentTurn() error = %v", err)
	}
	completedJob, err := databases[0].GetJob(ctx, job.ID)
	if err != nil || completedJob.Status != store.JobSucceeded {
		t.Errorf("Agent Turn job after finalization = (%#v, %v), want SUCCEEDED", completedJob, err)
	}
	second, err := databases[1].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("successor AllocateAgentTurn() error = %v", err)
	}
	if second.TurnNumber != 2 || second.ExecutionEpoch != 2 {
		t.Errorf("successor identity = (%d, %d), want (2, 2)", second.TurnNumber, second.ExecutionEpoch)
	}
}

func TestAgentTurnFenceRejectsStaleEpochOwnerAndControlRevision(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, 300*time.Millisecond)
	if err := databases[0].HeartbeatJob(ctx, job, time.Second); !errors.Is(err, store.ErrAgentTurnJobRequiresTurnFence) {
		t.Errorf("HeartbeatJob() for Agent Turn job error = %v, want ErrAgentTurnJobRequiresTurnFence", err)
	}
	wrongJob := job
	wrongJob.LeaseToken = "30000000-0000-4000-8000-000000000098"
	if _, err := databases[0].AcquireAgentTurn(ctx, wrongJob, turn.ControlRevision, "runtime-a", 80*time.Millisecond, 1); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("AcquireAgentTurn() with wrong job token error = %v, want ErrAgentTurnFenceLost", err)
	}
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-a", 80*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	time.Sleep(35 * time.Millisecond)
	if err := databases[0].HeartbeatAgentTurn(ctx, lease, 150*time.Millisecond); err != nil {
		t.Fatalf("HeartbeatAgentTurn() error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := databases[0].ValidateTurnFence(ctx, lease); err != nil {
		t.Fatalf("ValidateTurnFence() after heartbeat and original expiry error = %v", err)
	}

	wrongEpoch := lease
	wrongEpoch.ExecutionEpoch++
	if err := databases[0].ValidateTurnFence(ctx, wrongEpoch); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("ValidateTurnFence() with stale epoch error = %v, want ErrAgentTurnFenceLost", err)
	}
	wrongOwner := lease
	wrongOwner.OwnerToken = "30000000-0000-4000-8000-000000000099"
	if err := databases[0].ValidateTurnFence(ctx, wrongOwner); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("ValidateTurnFence() with stale owner error = %v, want ErrAgentTurnFenceLost", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET control_revision = control_revision + 1 WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatalf("advance control revision: %v", err)
	}
	if err := databases[0].ValidateTurnFence(ctx, lease); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("ValidateTurnFence() with stale control revision error = %v, want ErrAgentTurnFenceLost", err)
	}
}

func TestAgentTurnMutationAdmissionCloseSerializesWithReservation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}

	start := make(chan struct{})
	closeResult := make(chan error, 1)
	reserveResult := make(chan error, 1)
	go func() {
		<-start
		closeResult <- databases[0].CloseMutationAdmission(ctx, lease)
	}()
	go func() {
		<-start
		_, err := databases[1].ReserveMutation(ctx, lease, store.MutationSpec{
			OperationID: "github:issue-comment:race", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"hello"}`),
		})
		reserveResult <- err
	}()
	close(start)
	if err := <-closeResult; err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	if err := <-reserveResult; err != nil && !errors.Is(err, store.ErrMutationAdmissionClosed) {
		t.Fatalf("racing ReserveMutation() error = %v", err)
	}
	if _, err := databases[1].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:issue-comment:after-close", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	}); !errors.Is(err, store.ErrMutationAdmissionClosed) {
		t.Errorf("ReserveMutation() after close error = %v, want ErrMutationAdmissionClosed", err)
	}
}

func TestAgentTurnRecoveryBlocksSuccessorWhileMutationIsUnsettled(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, 80*time.Millisecond)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", 80*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	reservation, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:pull-request:create:1", ToolName: "create_pull_request", Request: json.RawMessage(`{"head":"feature"}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() error = %v", err)
	}
	if reservation.InvocationNumber != 1 || reservation.State != store.MutationReserved {
		t.Errorf("reservation = %#v, want invocation 1 RESERVED", reservation)
	}
	if _, err := databases[0].StartMutation(ctx, lease, reservation.ID); err != nil {
		t.Fatalf("StartMutation() error = %v", err)
	}
	unstarted, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:issue-comment:never-started", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() for unstarted operation error = %v", err)
	}
	time.Sleep(110 * time.Millisecond)
	if reclaimed, err := databases[0].ReclaimExpiredJobs(ctx, 10); err != nil || reclaimed != 0 {
		t.Errorf("ReclaimExpiredJobs() for Agent Turn job = (%d, %v), want (0, nil)", reclaimed, err)
	}
	if reclaimed, err := databases[0].ClaimJob(ctx, store.AgentTurnQueue, "replacement", time.Second); err != nil || reclaimed != nil {
		t.Errorf("ClaimJob() for expired Agent Turn job = (%#v, %v), want (nil, nil)", reclaimed, err)
	}
	recovery, err := databases[0].RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("RecoverExpiredAgentTurn() error = %v", err)
	}
	if recovery.Status != store.AgentTurnReconciling || recovery.SuccessorAllowed || recovery.StopRuntimeJobID == "" || recovery.ReconcileMutationsJobID == "" {
		t.Errorf("recovery = %#v, want RECONCILING with successor blocked", recovery)
	}
	observedRecovery, err := databases[0].GetAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil || observedRecovery.SuccessorAllowed || observedRecovery.StopRuntimeJobID != recovery.StopRuntimeJobID {
		t.Errorf("GetAgentTurnRecovery() = (%#v, %v), want durable unsettled barrier", observedRecovery, err)
	}
	if err := databases[0].ValidateTurnFence(ctx, lease); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("old ValidateTurnFence() after recovery error = %v, want ErrAgentTurnFenceLost", err)
	}
	if _, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "replacement", time.Second, 1); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("same-epoch AcquireAgentTurn() after recovery error = %v, want ErrAgentTurnFenceLost", err)
	}
	if _, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("AllocateAgentTurn() with recovery barrier error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	stopJob, err := databases[0].ClaimJob(ctx, store.AgentTurnRecoveryQueue, "stop-worker", time.Second)
	if err != nil || stopJob == nil || stopJob.Kind != store.StopStaleRuntimeJobKind {
		t.Fatalf("ClaimJob() for stale runtime stop = (%#v, %v), want STOP_STALE_RUNTIME", stopJob, err)
	}
	if err := databases[0].CompleteJob(ctx, *stopJob, json.RawMessage(`{}`)); !errors.Is(err, store.ErrAgentTurnJobRequiresTurnFence) {
		t.Errorf("generic CompleteJob() for recovery job error = %v, want special-fence error", err)
	}
	reconcileJob, err := databases[0].ClaimJob(ctx, store.AgentTurnRecoveryQueue, "reconcile-worker", time.Second)
	if err != nil || reconcileJob == nil || reconcileJob.Kind != store.ReconcileAgentTurnMutationsJobKind {
		t.Fatalf("ClaimJob() for mutation reconciliation = (%#v, %v), want RECONCILE_AGENT_TURN_MUTATIONS", reconcileJob, err)
	}
	if _, err := databases[0].ReconcileRecoveredMutation(ctx, *reconcileJob, reservation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"premature":true}`),
	}); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("ReconcileRecoveredMutation() before runtime stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	staleStop := *stopJob
	staleStop.LeaseToken = "30000000-0000-4000-8000-000000000097"
	if _, err := databases[0].AcknowledgeRecoveredRuntimeStopped(ctx, staleStop); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("AcknowledgeRecoveredRuntimeStopped() with stale token error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	if _, err := databases[0].AcknowledgeRecoveredRuntimeStopped(ctx, *stopJob); err != nil {
		t.Fatalf("AcknowledgeRecoveredRuntimeStopped() error = %v", err)
	}
	if _, err := databases[0].CompleteAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("CompleteAgentTurnRecovery() before mutation reconciliation error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	if _, err := databases[0].ReconcileRecoveredMutation(ctx, *reconcileJob, unstarted.ID, store.RecoveredMutationOutcome{
		State: store.MutationFailed, LastError: "should already be terminal",
	}); !errors.Is(err, store.ErrMutationStateConflict) {
		t.Errorf("ReconcileRecoveredMutation() for unstarted reservation error = %v, want ErrMutationStateConflict", err)
	}
	wrongEpoch := *reconcileJob
	wrongEpoch.ExecutionEpoch++
	if _, err := databases[0].ReconcileRecoveredMutation(ctx, wrongEpoch, reservation.ID, store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: json.RawMessage(`{}`)}); !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Errorf("ReconcileRecoveredMutation() with wrong epoch error = %v, want ErrAgentTurnRecoveryFenceLost", err)
	}
	if _, err := databases[0].ReconcileRecoveredMutation(ctx, *reconcileJob, reservation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"pull_request_id":42}`),
	}); err != nil {
		t.Fatalf("ReconcileRecoveredMutation() error = %v", err)
	}
	settled, err := databases[0].CompleteAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("CompleteAgentTurnRecovery() error = %v", err)
	}
	if !settled.SuccessorAllowed || settled.RecoverySettledAt == nil {
		t.Errorf("settled recovery = %#v, want successor allowed", settled)
	}
	for _, jobID := range []string{recovery.StopRuntimeJobID, recovery.ReconcileMutationsJobID} {
		job, err := databases[0].GetJob(ctx, jobID)
		if err != nil || job.Status != store.JobSucceeded {
			t.Errorf("recovery job %s = (%#v, %v), want SUCCEEDED", jobID, job, err)
		}
	}
	successorSpec := fixture.turnSpec()
	successorSpec.RetryOfTurnID = turn.ID
	successor, err := databases[0].AllocateAgentTurn(ctx, successorSpec)
	if err != nil {
		t.Fatalf("AllocateAgentTurn() after mutation recovery error = %v", err)
	}
	if successor.ExecutionEpoch != turn.ExecutionEpoch+1 {
		t.Errorf("successor execution epoch = %d, want %d", successor.ExecutionEpoch, turn.ExecutionEpoch+1)
	}
}

func TestAgentTurnGlobalConcurrencyLimitAcrossStoreConnections(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turns := make([]store.AgentTurn, 4)
	jobs := make([]store.JobLease, 4)
	for index := range turns {
		fixture := seedAgentSession(t, pool, index+1)
		turn, err := databases[index].AllocateAgentTurn(ctx, fixture.turnSpec())
		if err != nil {
			t.Fatalf("AllocateAgentTurn(%d) error = %v", index, err)
		}
		turns[index] = turn
		jobs[index] = claimAgentTurnJob(t, databases[index], ctx, turn, 5*time.Second)
	}

	start := make(chan struct{})
	errs := make(chan error, len(turns))
	var wait sync.WaitGroup
	for index, turn := range turns {
		wait.Add(1)
		go func(index int, turn store.AgentTurn) {
			defer wait.Done()
			<-start
			_, err := databases[index].AcquireAgentTurn(ctx, jobs[index], turn.ControlRevision, fmt.Sprintf("runtime-%d", index), time.Second, 2)
			errs <- err
		}(index, turn)
	}
	close(start)
	wait.Wait()
	close(errs)
	succeeded := 0
	limited := 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrAgentTurnConcurrencyLimit):
			limited++
		default:
			t.Errorf("AcquireAgentTurn() error = %v", err)
		}
	}
	if succeeded != 2 || limited != 2 {
		t.Errorf("global acquisitions = %d succeeded, %d limited, want 2 and 2", succeeded, limited)
	}
}

func TestAgentTurnExpiredOwnershipRequiresRecoveryAndNewEpoch(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, 500*time.Millisecond)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-a", 70*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-b", time.Second, 1); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("same-epoch AcquireAgentTurn() error = %v, want ErrAgentTurnFenceLost", err)
	}
	recovery, err := databases[0].RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("RecoverExpiredAgentTurn() error = %v", err)
	}
	if recovery.Status != store.AgentTurnInterrupted || recovery.SuccessorAllowed || recovery.StopRuntimeJobID == "" || recovery.ReconcileMutationsJobID != "" {
		t.Errorf("recovery = %#v, want INTERRUPTED with stop barrier and successor blocked", recovery)
	}
	if err := databases[0].ValidateTurnFence(ctx, lease); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("old ValidateTurnFence() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if _, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("AllocateAgentTurn() before recovery completion error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	stopJob, err := databases[0].ClaimJob(ctx, store.AgentTurnRecoveryQueue, "stop-worker", time.Second)
	if err != nil || stopJob == nil || stopJob.Kind != store.StopStaleRuntimeJobKind {
		t.Fatalf("ClaimJob() for stale runtime stop = (%#v, %v), want STOP_STALE_RUNTIME", stopJob, err)
	}
	acknowledged, err := databases[0].AcknowledgeRecoveredRuntimeStopped(ctx, *stopJob)
	if err != nil {
		t.Fatalf("AcknowledgeRecoveredRuntimeStopped() error = %v", err)
	}
	if acknowledged.RuntimeStoppedAt == nil || acknowledged.SuccessorAllowed {
		t.Errorf("acknowledged recovery = %#v, want stopped but unsettled barrier", acknowledged)
	}
	settled, err := databases[0].CompleteAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("CompleteAgentTurnRecovery() error = %v", err)
	}
	if !settled.SuccessorAllowed || settled.RecoverySettledAt == nil {
		t.Errorf("settled recovery = %#v, want successor allowed", settled)
	}
	successorSpec := fixture.turnSpec()
	successorSpec.RetryOfTurnID = turn.ID
	successor, err := databases[0].AllocateAgentTurn(ctx, successorSpec)
	if err != nil {
		t.Fatalf("successor AllocateAgentTurn() error = %v", err)
	}
	if successor.ExecutionEpoch != turn.ExecutionEpoch+1 {
		t.Errorf("successor epoch = %d, want %d", successor.ExecutionEpoch, turn.ExecutionEpoch+1)
	}
}

func TestAgentTurnMutationLifecycleAllowsFinalizationAfterTerminalStates(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	completed, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:comment:complete", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() error = %v", err)
	}
	if _, err := databases[0].StartMutation(ctx, lease, completed.ID); err != nil {
		t.Fatalf("StartMutation() error = %v", err)
	}
	if err := databases[0].CompleteMutation(ctx, lease, completed.ID, json.RawMessage(`{"comment_id":42}`)); err != nil {
		t.Fatalf("CompleteMutation() error = %v", err)
	}
	if err := databases[0].CompleteMutation(ctx, lease, completed.ID, json.RawMessage(`{}`)); !errors.Is(err, store.ErrMutationStateConflict) {
		t.Errorf("duplicate CompleteMutation() error = %v, want ErrMutationStateConflict", err)
	}
	failed, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:comment:failed", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("second ReserveMutation() error = %v", err)
	}
	if _, err := databases[0].StartMutation(ctx, lease, failed.ID); err != nil {
		t.Fatalf("second StartMutation() error = %v", err)
	}
	if err := databases[0].FailMutation(ctx, lease, failed.ID, errors.New("denied")); err != nil {
		t.Fatalf("FailMutation() error = %v", err)
	}
	unsettled, err := databases[0].ListUnsettledMutations(ctx, lease)
	if err != nil {
		t.Fatalf("ListUnsettledMutations() error = %v", err)
	}
	if len(unsettled) != 0 {
		t.Errorf("unsettled mutations = %#v, want none", unsettled)
	}
	if err := databases[0].CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	if err := databases[0].FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnSucceeded}); err != nil {
		t.Fatalf("FinalizeAgentTurn() error = %v", err)
	}
}

func TestAgentTurnUnknownMutationCanBeginReconciliation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	mutation, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:unknown", ToolName: "create_pull_request", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() error = %v", err)
	}
	if _, err := databases[0].StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatalf("StartMutation() error = %v", err)
	}
	if err := databases[0].MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("timeout")); err != nil {
		t.Fatalf("MarkMutationUnknown() error = %v", err)
	}
	if err := databases[0].BeginMutationReconciliation(ctx, lease, mutation.ID); err != nil {
		t.Fatalf("BeginMutationReconciliation() error = %v", err)
	}
	unsettled, err := databases[0].ListUnsettledMutations(ctx, lease)
	if err != nil || len(unsettled) != 1 || unsettled[0].State != store.MutationReconciling {
		t.Errorf("ListUnsettledMutations() = (%#v, %v), want one RECONCILING", unsettled, err)
	}
}

func TestAgentTurnAllocationAndAcquisitionRejectInactiveHierarchy(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, status := range []string{"CLOSING", "CLOSED", "NEEDS_HUMAN"} {
		if _, err := pool.Exec(ctx, `UPDATE workflows SET status = $2 WHERE id = $1`, fixture.workflowID, status); err != nil {
			t.Fatalf("set workflow status %s: %v", status, err)
		}
		if _, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnHierarchyInactive) {
			t.Errorf("AllocateAgentTurn() for workflow %s error = %v, want ErrAgentTurnHierarchyInactive", status, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("reopen workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET active = FALSE, completed_at = clock_timestamp() WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("deactivate Workflow Attempt: %v", err)
	}
	if _, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnHierarchyInactive) {
		t.Errorf("AllocateAgentTurn() for inactive Workflow Attempt error = %v, want ErrAgentTurnHierarchyInactive", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET active = TRUE, completed_at = NULL WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("reactivate Workflow Attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET status = 'WAITING_FOR_HUMAN' WHERE id = $1`, fixture.assignmentID); err != nil {
		t.Fatalf("pause assignment: %v", err)
	}
	if _, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnHierarchyInactive) {
		t.Errorf("AllocateAgentTurn() for inactive Assignment error = %v, want ErrAgentTurnHierarchyInactive", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET status = 'ACTIVE' WHERE id = $1`, fixture.assignmentID); err != nil {
		t.Fatalf("reactivate assignment: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'RETAINED', retained_at = clock_timestamp() WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatalf("retain session: %v", err)
	}
	if _, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnHierarchyInactive) {
		t.Errorf("AllocateAgentTurn() for inactive Session error = %v, want ErrAgentTurnHierarchyInactive", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'ACTIVE', retained_at = NULL WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatalf("reactivate session: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET control_owner = 'HUMAN' WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatalf("transfer session control: %v", err)
	}
	if _, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnHierarchyInactive) {
		t.Errorf("AllocateAgentTurn() under human control error = %v, want ErrAgentTurnHierarchyInactive", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET control_owner = 'AUTOMATION' WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatalf("restore session control: %v", err)
	}
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET status = 'COMPLETED' WHERE id = $1`, fixture.assignmentID); err != nil {
		t.Fatalf("complete assignment: %v", err)
	}
	if _, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", time.Second, 1); !errors.Is(err, store.ErrAgentTurnHierarchyInactive) {
		t.Errorf("AcquireAgentTurn() for completed assignment error = %v, want ErrAgentTurnHierarchyInactive", err)
	}
}

func TestAgentTurnHeartbeatAndCompetingAcquireSerializeGlobalSlot(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	firstFixture := seedAgentSession(t, pool, 1)
	secondFixture := seedAgentSession(t, pool, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstTurn, err := databases[0].AllocateAgentTurn(ctx, firstFixture.turnSpec())
	if err != nil {
		t.Fatalf("first AllocateAgentTurn() error = %v", err)
	}
	secondTurn, err := databases[1].AllocateAgentTurn(ctx, secondFixture.turnSpec())
	if err != nil {
		t.Fatalf("second AllocateAgentTurn() error = %v", err)
	}
	firstJob := claimAgentTurnJob(t, databases[0], ctx, firstTurn, 300*time.Millisecond)
	secondJob := claimAgentTurnJob(t, databases[1], ctx, secondTurn, 300*time.Millisecond)
	firstLease, err := databases[0].AcquireAgentTurn(ctx, firstJob, firstTurn.ControlRevision, "runtime-a", 90*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("first AcquireAgentTurn() error = %v", err)
	}
	time.Sleep(55 * time.Millisecond)
	start := make(chan struct{})
	heartbeatResult := make(chan error, 1)
	acquireResult := make(chan error, 1)
	go func() {
		<-start
		heartbeatResult <- databases[0].HeartbeatAgentTurn(ctx, firstLease, 250*time.Millisecond)
	}()
	go func() {
		<-start
		_, err := databases[1].AcquireAgentTurn(ctx, secondJob, secondTurn.ControlRevision, "runtime-b", time.Second, 1)
		acquireResult <- err
	}()
	close(start)
	heartbeatErr := <-heartbeatResult
	acquireErr := <-acquireResult
	if heartbeatErr == nil && !errors.Is(acquireErr, store.ErrAgentTurnConcurrencyLimit) {
		t.Errorf("heartbeat won but competing acquire error = %v, want concurrency limit", acquireErr)
	}
	if acquireErr == nil && !errors.Is(heartbeatErr, store.ErrAgentTurnFenceLost) {
		t.Errorf("competing acquire won but heartbeat error = %v, want lost fence", heartbeatErr)
	}
	if heartbeatErr == nil && acquireErr == nil {
		t.Error("heartbeat and competing acquire both succeeded with global limit 1")
	}
}

func TestAgentTurnRuntimeLabelsContainIdentityWithoutOwnerToken(t *testing.T) {
	turn := store.AgentTurn{
		ID: "30000000-0000-4000-8000-000000000004", AgentSessionID: "30000000-0000-4000-8000-000000000003",
		AgentAssignmentID: "30000000-0000-4000-8000-000000000002", ExecutionEpoch: 9,
	}
	labels := store.RuntimeLabels(turn)
	for key, want := range map[string]string{
		store.RuntimeLabelAssignmentID: turn.AgentAssignmentID,
		store.RuntimeLabelSessionID:    turn.AgentSessionID,
		store.RuntimeLabelTurnID:       turn.ID,
		store.RuntimeLabelEpoch:        "9",
	} {
		if labels[key] != want {
			t.Errorf("RuntimeLabels()[%q] = %q, want %q", key, labels[key], want)
		}
	}
	for key, value := range labels {
		if key == "owner_token" || value == "30000000-0000-4000-8000-000000000099" {
			t.Errorf("RuntimeLabels() exposed secret token in %q=%q", key, value)
		}
	}
}

type agentFixture struct {
	workflowID, attemptID, assignmentID, sessionID string
}

func (fixture agentFixture) turnSpec() store.AgentTurnSpec {
	digest := sha256.Sum256([]byte("agent profile"))
	return store.AgentTurnSpec{
		AgentSessionID: fixture.sessionID, WorkflowAttemptID: fixture.attemptID,
		ControlRevision: 1, AgentProfileCommitSHA: "0123456789abcdef",
		AgentProfileContentSHA256: digest[:], AgentProfileConfig: json.RawMessage(`{"model":"test"}`),
	}
}

func claimAgentTurnJob(t *testing.T, database *store.Store, ctx context.Context, turn store.AgentTurn, lease time.Duration) store.JobLease {
	t.Helper()
	job, err := database.ClaimJob(ctx, store.AgentTurnQueue, "job-worker", lease)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() for Agent Turn = (%#v, %v), want lease", job, err)
	}
	if job.AgentTurnID != turn.ID || job.ExecutionEpoch != turn.ExecutionEpoch {
		t.Fatalf("claimed Agent Turn job identity = (%s, %d), want (%s, %d)", job.AgentTurnID, job.ExecutionEpoch, turn.ID, turn.ExecutionEpoch)
	}
	return *job
}

func seedAgentSession(t *testing.T, pool *pgxpool.Pool, number int) agentFixture {
	t.Helper()
	fixture := agentFixture{
		workflowID:   fmt.Sprintf("40000000-0000-4000-8000-%012d", number*10+1),
		attemptID:    fmt.Sprintf("40000000-0000-4000-8000-%012d", number*10+2),
		assignmentID: fmt.Sprintf("40000000-0000-4000-8000-%012d", number*10+3),
		sessionID:    fmt.Sprintf("40000000-0000-4000-8000-%012d", number*10+4),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ($1, $2, 'owner', 'repo', $2, $2, 'ACTIVE')`, fixture.workflowID, number)
	if err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ($1, $2, 1, 'ACTIVE')`, fixture.attemptID, fixture.workflowID)
	if err != nil {
		t.Fatalf("seed workflow attempt: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest
)
VALUES ($1, $2, 'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:test')`, fixture.assignmentID, fixture.workflowID)
	if err != nil {
		t.Fatalf("seed agent assignment: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ($1, $2, 1, $3, 'runtime', '1', 'sha256:test', '/state/session', 'ACTIVE')`, fixture.sessionID, fixture.assignmentID, "session-"+fixture.sessionID)
	if err != nil {
		t.Fatalf("seed agent session: %v", err)
	}
	return fixture
}
