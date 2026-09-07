//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestAgentTurnAllocationIsMonotonicAndAllowsOnlyOneActiveTurnPerWorkflow(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	invalid := fixture.turnSpec()
	invalid.Purpose = ""
	if _, err := databases[0].AllocateAgentTurn(ctx, invalid); err == nil || !strings.Contains(err.Error(), "invalid purpose") {
		t.Fatalf("AllocateAgentTurn() with empty purpose error = %v, want invalid purpose", err)
	}

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
	refreshed, err := databases[0].RefreshAgentTurnLease(ctx, lease, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("RefreshAgentTurnLease() error = %v", err)
	}
	if !refreshed.LeaseExpiresAt.After(lease.LeaseExpiresAt) || refreshed.JobLease.LeaseExpiresAt == nil ||
		!refreshed.JobLease.LeaseExpiresAt.Equal(refreshed.LeaseExpiresAt) {
		t.Fatalf("RefreshAgentTurnLease() expiration = turn %v, job %v, original %v", refreshed.LeaseExpiresAt, refreshed.JobLease.LeaseExpiresAt, lease.LeaseExpiresAt)
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

func TestAgentTurnExecutionContextIsReadOnlyUnderTheLiveEpochFence(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 7)
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

	execution, err := databases[0].GetAgentTurnExecutionContext(ctx, lease)
	if err != nil {
		t.Fatalf("GetAgentTurnExecutionContext() error = %v", err)
	}
	if execution.WorkflowID != fixture.workflowID || execution.Repository.ID != 7 || execution.Repository.Owner != "owner" || execution.Repository.Name != "repo" {
		t.Errorf("execution repository context = %#v", execution)
	}
	if execution.Issue.ID != 7 || execution.Issue.Number != 7 || execution.ChangeProposal != nil {
		t.Errorf("execution Work Item context = %#v", execution)
	}
	if execution.Assignment.ID != fixture.assignmentID || execution.Session.ID != fixture.sessionID || execution.Turn.ID != turn.ID {
		t.Errorf("execution hierarchy = %#v", execution)
	}

	stale := lease
	stale.ExecutionEpoch++
	if _, err := databases[0].GetAgentTurnExecutionContext(ctx, stale); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("GetAgentTurnExecutionContext() stale epoch error = %v, want ErrAgentTurnFenceLost", err)
	}
}

func TestAgentTurnFenceOperationRejectsStaleAuthorityWithoutCallingOperation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 9)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}

	stale := lease
	stale.JobLease.WorkflowAttemptID = "40000000-0000-4000-8000-000000000099"
	called := false
	err = databases[0].WithAgentTurnFence(ctx, stale, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("WithAgentTurnFence() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if called {
		t.Fatal("WithAgentTurnFence() called operation under stale authority")
	}
}

func TestAgentTurnFenceOperationAllowsRunningTurnWhileSessionIsCreating(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 11)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'CREATING', acp_session_id = NULL WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}

	called := false
	if err := databases[0].WithAgentTurnFence(ctx, lease, func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithAgentTurnFence() error = %v", err)
	}
	if !called {
		t.Fatal("WithAgentTurnFence() did not call operation for creating Session's RUNNING Turn")
	}
}

func TestExpiredAgentTurnRecoveryWaitsForFencedContainerCreation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, 80*time.Millisecond)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", 80*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}

	creationEntered := make(chan struct{})
	releaseCreation := make(chan struct{})
	creationResult := make(chan error, 1)
	go func() {
		creationResult <- databases[0].WithAgentTurnFence(ctx, lease, func(context.Context) error {
			close(creationEntered)
			<-releaseCreation
			return nil
		})
	}()
	select {
	case <-creationEntered:
	case <-ctx.Done():
		t.Fatal("fenced container creation did not start")
	}
	time.Sleep(100 * time.Millisecond)

	recoveryResult := make(chan error, 1)
	go func() {
		_, err := databases[1].RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
		recoveryResult <- err
	}()
	select {
	case err := <-recoveryResult:
		t.Fatalf("RecoverExpiredAgentTurn() returned before container creation exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseCreation)
	if err := <-creationResult; err != nil {
		t.Fatalf("WithAgentTurnFence() error = %v", err)
	}
	if err := <-recoveryResult; err != nil {
		t.Fatalf("RecoverExpiredAgentTurn() error after container creation = %v", err)
	}
}

func TestReadToolInvocationIsRecordedUnderTheLiveEpochFence(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 8)
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
	started := time.Now().UTC().Add(-25 * time.Millisecond)
	finished := started.Add(20 * time.Millisecond)
	recorded, err := databases[0].RecordReadInvocation(ctx, lease, store.ReadInvocation{
		ToolName: "get_issue", Request: json.RawMessage(`{}`), Result: json.RawMessage(`{"number":8}`),
		StartedAt: started, FinishedAt: finished,
	})
	if err != nil {
		t.Fatalf("RecordReadInvocation() error = %v", err)
	}
	if recorded.AgentTurnID != turn.ID || recorded.ExecutionEpoch != turn.ExecutionEpoch || recorded.InvocationNumber != 1 ||
		recorded.ToolName != "get_issue" || !recorded.Succeeded || recorded.Duration != 20*time.Millisecond {
		t.Errorf("recorded read invocation = %#v", recorded)
	}

	stale := lease
	stale.OwnerToken = "30000000-0000-4000-8000-000000000099"
	if _, err := databases[0].RecordReadInvocation(ctx, stale, store.ReadInvocation{
		ToolName: "get_issue", Request: json.RawMessage(`{}`), Result: json.RawMessage(`{}`), StartedAt: started, FinishedAt: finished,
	}); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("RecordReadInvocation() stale lease error = %v, want ErrAgentTurnFenceLost", err)
	}
}

func TestAgentSessionPromptFenceValidatesPersistedACPIdentityAndTurnLease(t *testing.T) {
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
	acpSessionID := "session-" + fixture.sessionID
	if err := databases[0].ValidateAgentSessionPromptFence(ctx, lease, acpSessionID); err != nil {
		t.Fatalf("ValidateAgentSessionPromptFence() error = %v", err)
	}
	if err := databases[0].ValidateAgentSessionPromptFence(ctx, lease, "stale-acp-session"); !errors.Is(err, store.ErrAgentSessionACPConflict) {
		t.Errorf("ValidateAgentSessionPromptFence() mismatch error = %v, want ErrAgentSessionACPConflict", err)
	}
	stale := lease
	stale.OwnerToken = "30000000-0000-4000-8000-000000000099"
	if err := databases[0].ValidateAgentSessionPromptFence(ctx, stale, acpSessionID); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("ValidateAgentSessionPromptFence() stale lease error = %v, want ErrAgentTurnFenceLost", err)
	}
}

func TestHumanPromptLeaseValidatesControlRevisionOwnerAndExactACPIdentity(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	human, err := database.TransferAgentSessionControl(ctx, fixture.sessionID, 1, store.SessionControlHuman, "human-1")
	if err != nil {
		t.Fatalf("TransferAgentSessionControl() error = %v", err)
	}
	lease, err := database.AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.ACPSessionID, time.Second)
	if err != nil {
		t.Fatalf("AcquireHumanPromptLease() error = %v", err)
	}
	if _, err := database.AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision-1, human.ACPSessionID, time.Second); !errors.Is(err, store.ErrAgentSessionControlFenceLost) {
		t.Errorf("AcquireHumanPromptLease() stale revision error = %v, want ErrAgentSessionControlFenceLost", err)
	}
	if _, err := database.AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, "stale-acp-session", time.Second); !errors.Is(err, store.ErrAgentSessionACPConflict) {
		t.Errorf("AcquireHumanPromptLease() stale ACP identity error = %v, want ErrAgentSessionACPConflict", err)
	}
	if err := database.ReleaseHumanPromptLease(ctx, lease); err != nil {
		t.Fatalf("ReleaseHumanPromptLease() error = %v", err)
	}
	if _, err := database.TransferAgentSessionControl(ctx, human.ID, human.ControlRevision, store.SessionControlAutomation, "orchestrator"); err != nil {
		t.Fatalf("return control to automation: %v", err)
	}
	if _, err := database.AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.ACPSessionID, time.Second); !errors.Is(err, store.ErrAgentSessionControlFenceLost) {
		t.Errorf("AcquireHumanPromptLease() after transfer error = %v, want ErrAgentSessionControlFenceLost", err)
	}
}

func TestAgentTurnFenceAllowsCreatingSessionPreparation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'CREATING', acp_session_id = NULL WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatalf("restore creating Agent Session state: %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() for creating Session error = %v", err)
	}
	if err := databases[0].ValidateTurnFence(ctx, lease); err != nil {
		t.Fatalf("ValidateTurnFence() for creating Session error = %v", err)
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

func TestAgentTurnMutationOperationIDIsScopedToLineage(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	firstFixture := seedAgentSession(t, pool, 31)
	secondFixture := seedAgentSession(t, pool, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	acquire := func(fixture agentFixture, owner string) store.AgentTurnLease {
		t.Helper()
		turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
		if err != nil {
			t.Fatalf("AllocateAgentTurn(%s) error = %v", owner, err)
		}
		job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
		lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, owner, time.Second, 2)
		if err != nil {
			t.Fatalf("AcquireAgentTurn(%s) error = %v", owner, err)
		}
		if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
			t.Fatalf("OpenMutationAdmission(%s) error = %v", owner, err)
		}
		return lease
	}

	firstLease := acquire(firstFixture, "runtime-first")
	secondLease := acquire(secondFixture, "runtime-second")
	spec := store.MutationSpec{
		OperationID: "caller-operation-1", ToolName: "comment_on_issue",
		Request: json.RawMessage(`{"body":"same request"}`), ExternalService: "github",
		ExternalResourceID: "issue-1", ExpectedSHA: "head-1",
	}
	reservations := make(chan store.MutationReservation, 8)
	reservationErrors := make(chan error, 8)
	var wait sync.WaitGroup
	for index := range 8 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			reservation, err := databases[index%len(databases)].ReserveMutation(ctx, firstLease, spec)
			reservations <- reservation
			reservationErrors <- err
		}(index)
	}
	wait.Wait()
	close(reservations)
	close(reservationErrors)
	var first store.MutationReservation
	for err := range reservationErrors {
		if err != nil {
			t.Fatalf("concurrent ReserveMutation() error = %v", err)
		}
	}
	for reservation := range reservations {
		if first.ID == "" {
			first = reservation
		} else if reservation.ID != first.ID {
			t.Fatalf("concurrent ReserveMutation() IDs = %s and %s, want one reservation", first.ID, reservation.ID)
		}
	}
	var firstReservations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_invocations WHERE operation_lineage_id = $1 AND operation_id = $2`, firstLease.ID, spec.OperationID).Scan(&firstReservations); err != nil {
		t.Fatal(err)
	}
	if firstReservations != 1 {
		t.Fatalf("concurrent reservation rows = %d, want 1", firstReservations)
	}
	retry, err := databases[0].ReserveMutation(ctx, firstLease, spec)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("identical ReserveMutation() retry = (%#v, %v), want reservation %s", retry, err, first.ID)
	}
	crossTool := spec
	crossTool.ToolName = "comment_on_pull_request"
	crossToolReservation, err := databases[0].ReserveMutation(ctx, firstLease, crossTool)
	if err != nil {
		t.Fatalf("ReserveMutation() with same caller operation for another tool error = %v", err)
	}
	if crossToolReservation.ID == first.ID || crossToolReservation.OperationID != first.OperationID {
		t.Fatalf("cross-tool reservation = %#v, want distinct reservation with shared caller operation", crossToolReservation)
	}

	conflicts := []store.MutationSpec{
		{OperationID: spec.OperationID, ToolName: spec.ToolName, Request: json.RawMessage(`{"body":"changed"}`), ExternalService: spec.ExternalService, ExternalResourceID: spec.ExternalResourceID, ExpectedSHA: spec.ExpectedSHA},
		{OperationID: spec.OperationID, ToolName: spec.ToolName, Request: spec.Request, ExternalService: spec.ExternalService, ExternalResourceID: "issue-2", ExpectedSHA: spec.ExpectedSHA},
		{OperationID: spec.OperationID, ToolName: spec.ToolName, Request: spec.Request, ExternalService: spec.ExternalService, ExternalResourceID: spec.ExternalResourceID, ExpectedSHA: "different-head"},
	}
	for index, conflict := range conflicts {
		if _, err := databases[0].ReserveMutation(ctx, firstLease, conflict); !errors.Is(err, store.ErrMutationOperationConflict) {
			t.Errorf("ReserveMutation() conflict %d error = %v, want ErrMutationOperationConflict", index, err)
		}
	}

	second, err := databases[0].ReserveMutation(ctx, secondLease, spec)
	if err != nil {
		t.Fatalf("ReserveMutation() with same caller operation in unrelated turn error = %v", err)
	}
	if second.ID == first.ID || second.AgentTurnID != secondLease.ID || second.ExecutionEpoch != secondLease.ExecutionEpoch {
		t.Errorf("unrelated reservation = %#v, want distinct reservation in second turn epoch", second)
	}
}

func TestAgentTurnMutationReservationSurvivesProcessDeathRetryLineage(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 33)
	unrelatedFixture := seedAgentSession(t, pool, 34)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', state_revision = 1 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}

	root, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() root error = %v", err)
	}
	rootJob := claimAgentTurnJob(t, database, ctx, root, 70*time.Millisecond)
	rootLease, err := database.AcquireAgentTurn(ctx, rootJob, root.ControlRevision, "runtime-root", 70*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() root error = %v", err)
	}
	if err := database.OpenMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	spec := store.MutationSpec{
		OperationID: "caller-operation-after-process-death", ToolName: "comment_on_issue",
		Request: json.RawMessage(`{"body":"durable"}`), ExternalService: "github",
		ExternalResourceID: "issue-33", ExpectedSHA: "head-33",
	}
	original, err := database.ReserveMutation(ctx, rootLease, spec)
	if err != nil {
		t.Fatalf("ReserveMutation() root error = %v", err)
	}
	if _, err := database.StartMutation(ctx, rootLease, original.ID); err != nil {
		t.Fatal(err)
	}
	wantResult := json.RawMessage(`{"comment_id":330}`)
	if err := database.CompleteMutation(ctx, rootLease, original.ID, wantResult); err != nil {
		t.Fatal(err)
	}
	var originalResult string
	var originalUpdatedAt, originalFinishedAt time.Time
	if err := pool.QueryRow(ctx, `
SELECT result::text, updated_at, finished_at FROM tool_invocations WHERE id = $1`, original.ID).Scan(
		&originalResult, &originalUpdatedAt, &originalFinishedAt,
	); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	recovery, err := database.RecoverExpiredAgentTurn(ctx, root.ID, root.ExecutionEpoch)
	if err != nil {
		t.Fatalf("RecoverExpiredAgentTurn() error = %v", err)
	}
	stopJob, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "stop-worker", time.Second)
	if err != nil || stopJob == nil {
		t.Fatalf("ClaimJobKind() stop = (%#v, %v)", stopJob, err)
	}
	acknowledged, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, *stopJob)
	if err != nil || acknowledged.SuccessorAllowed {
		t.Fatalf("AcknowledgeRecoveredRuntimeStopped() = (%#v, %v)", acknowledged, err)
	}
	if recovery.ReconcileMutationsJobID == "" || recovery.Status != store.AgentTurnReconciling {
		t.Fatalf("successful mutation recovery = %#v, want fresh verification barrier", recovery)
	}
	var prematureRetries int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM jobs
WHERE kind = 'PREPARE_AGENT_TURN' AND payload->>'retry_of_turn_id' = $1`, root.ID).Scan(&prematureRetries); err != nil {
		t.Fatal(err)
	}
	if prematureRetries != 0 {
		t.Fatalf("retry jobs before fresh verification = %d, want zero", prematureRetries)
	}

	verification := &successfulMutationVerificationReconciler{}
	recoveryWorker, err := mcp.NewRecoveryWorker(database, verification, mcp.RecoveryWorkerConfig{
		ClaimOwner: "successful-mutation-verifier", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := recoveryWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("ProcessNext() successful verification = (%t, %v)", processed, err)
	}
	if verification.callCount() != 1 {
		t.Fatalf("fresh successful mutation verifications = %d, want one", verification.callCount())
	}
	acknowledged, err = database.CompleteAgentTurnRecovery(ctx, root.ID, root.ExecutionEpoch)
	if err != nil || !acknowledged.SuccessorAllowed {
		t.Fatalf("CompleteAgentTurnRecovery() after verification = (%#v, %v)", acknowledged, err)
	}
	var verifiedResult string
	var verifiedUpdatedAt, verifiedFinishedAt time.Time
	if err := pool.QueryRow(ctx, `
SELECT result::text, updated_at, finished_at FROM tool_invocations WHERE id = $1`, original.ID).Scan(
		&verifiedResult, &verifiedUpdatedAt, &verifiedFinishedAt,
	); err != nil {
		t.Fatal(err)
	}
	if verifiedResult != originalResult || !verifiedUpdatedAt.Equal(originalUpdatedAt) || !verifiedFinishedAt.Equal(originalFinishedAt) {
		t.Fatalf("successful ledger evidence changed during verification: result %s -> %s, updated %s -> %s, finished %s -> %s",
			originalResult, verifiedResult, originalUpdatedAt, verifiedUpdatedAt, originalFinishedAt, verifiedFinishedAt)
	}
	if _, err := pool.Exec(ctx, `
UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE agent_turn_settlement_id = $1 AND kind = 'PREPARE_AGENT_TURN' AND status = 'AVAILABLE'`, acknowledged.SettlementID); err != nil {
		t.Fatal(err)
	}

	acquireRetry := func(target store.AgentTurn, owner string) (store.AgentTurn, store.AgentTurnLease) {
		t.Helper()
		retrySpec := fixture.turnSpec()
		retrySpec.Purpose = workflow.TurnPurposeRetry
		retrySpec.RetryOfTurnID = target.ID
		retry, err := database.AllocateAgentTurn(ctx, retrySpec)
		if err != nil {
			t.Fatalf("AllocateAgentTurn() retry of %s error = %v", target.ID, err)
		}
		job := claimAgentTurnJob(t, database, ctx, retry, time.Second)
		lease, err := database.AcquireAgentTurn(ctx, job, retry.ControlRevision, owner, time.Second, 2)
		if err != nil {
			t.Fatalf("AcquireAgentTurn() retry error = %v", err)
		}
		if err := database.OpenMutationAdmission(ctx, lease); err != nil {
			t.Fatal(err)
		}
		return retry, lease
	}

	firstRetry, firstRetryLease := acquireRetry(root, "runtime-retry-1")
	replaySpec := spec
	replaySpec.ExternalService = "planner-started-empty"
	replaySpec.ExternalResourceID = "planner-derived-resource"
	replaySpec.ExpectedSHA = "planner-derived-head"
	cached, err := database.ReserveMutation(ctx, firstRetryLease, replaySpec)
	if err != nil {
		t.Fatalf("ReserveMutation() process-death retry error = %v", err)
	}
	var cachedResult struct {
		CommentID int64 `json:"comment_id"`
	}
	if err := json.Unmarshal(cached.Result, &cachedResult); err != nil {
		t.Fatalf("decode cached mutation result: %v", err)
	}
	if cached.ID != original.ID || cached.AgentTurnID != root.ID || cached.ExecutionEpoch != root.ExecutionEpoch ||
		cached.State != store.MutationSucceeded || cachedResult.CommentID != 330 || cached.ExternalService != spec.ExternalService ||
		cached.ExternalResourceID != spec.ExternalResourceID || cached.ExpectedSHA != spec.ExpectedSHA {
		t.Fatalf("process-death retry reservation = %#v, want original SUCCEEDED reservation %#v", cached, original)
	}
	conflict := spec
	conflict.Request = json.RawMessage(`{"body":"different"}`)
	if _, err := database.ReserveMutation(ctx, firstRetryLease, conflict); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("ReserveMutation() conflicting retry definition error = %v, want ErrMutationOperationConflict", err)
	}
	if err := database.CloseMutationAdmission(ctx, firstRetryLease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, firstRetryLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "retry process died"}); err != nil {
		t.Fatal(err)
	}

	secondRetry, secondRetryLease := acquireRetry(firstRetry, "runtime-retry-2")
	secondCached, err := database.ReserveMutation(ctx, secondRetryLease, spec)
	if err != nil || secondCached.ID != original.ID {
		t.Fatalf("ReserveMutation() second-level retry = (%#v, %v), want reservation %s", secondCached, err, original.ID)
	}

	unrelated, err := database.AllocateAgentTurn(ctx, unrelatedFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	unrelatedJob := claimAgentTurnJob(t, database, ctx, unrelated, time.Second)
	unrelatedLease, err := database.AcquireAgentTurn(ctx, unrelatedJob, unrelated.ControlRevision, "runtime-unrelated", time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, unrelatedLease); err != nil {
		t.Fatal(err)
	}
	unrelatedReservation, err := database.ReserveMutation(ctx, unrelatedLease, spec)
	if err != nil {
		t.Fatalf("ReserveMutation() unrelated lineage error = %v", err)
	}
	if unrelatedReservation.ID == original.ID {
		t.Fatalf("unrelated lineage reused reservation %s", original.ID)
	}

	var rootLineage, firstRetryLineage, secondRetryLineage, unrelatedLineage string
	if err := pool.QueryRow(ctx, `
SELECT root.operation_lineage_id::text, first_retry.operation_lineage_id::text,
       second_retry.operation_lineage_id::text, unrelated.operation_lineage_id::text
FROM agent_turns AS root
JOIN agent_turns AS first_retry ON first_retry.id = $2
JOIN agent_turns AS second_retry ON second_retry.id = $3
JOIN agent_turns AS unrelated ON unrelated.id = $4
WHERE root.id = $1`, root.ID, firstRetry.ID, secondRetry.ID, unrelated.ID).Scan(
		&rootLineage, &firstRetryLineage, &secondRetryLineage, &unrelatedLineage,
	); err != nil {
		t.Fatal(err)
	}
	if rootLineage != root.ID || firstRetryLineage != root.ID || secondRetryLineage != root.ID || unrelatedLineage != unrelated.ID {
		t.Errorf("operation lineages = root %s, retry %s, second retry %s, unrelated %s", rootLineage, firstRetryLineage, secondRetryLineage, unrelatedLineage)
	}
	var lineageReservations int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM tool_invocations
WHERE operation_lineage_id = $1 AND operation_id = $2`, root.ID, spec.OperationID).Scan(&lineageReservations); err != nil {
		t.Fatal(err)
	}
	if lineageReservations != 1 {
		t.Errorf("retry lineage reservations = %d, want 1", lineageReservations)
	}
}

func TestAgentTurnRetryReturnsOriginalUnsettledMutationReservation(t *testing.T) {
	for _, state := range []store.MutationState{store.MutationUnknown, store.MutationReconciling} {
		t.Run(string(state), func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			fixture := seedAgentSession(t, pool, 36)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			root, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			rootLease, err := database.AcquireAgentTurn(ctx, claimAgentTurnJob(t, database, ctx, root, time.Second), root.ControlRevision, "runtime-root", time.Second, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.OpenMutationAdmission(ctx, rootLease); err != nil {
				t.Fatal(err)
			}
			if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
				t.Fatal(err)
			}
			if err := database.FinalizeAgentTurn(ctx, rootLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "infrastructure failure"}); err != nil {
				t.Fatal(err)
			}

			retrySpec := fixture.turnSpec()
			retrySpec.Purpose = workflow.TurnPurposeRetry
			retrySpec.RetryOfTurnID = root.ID
			retry, err := database.AllocateAgentTurn(ctx, retrySpec)
			if err != nil {
				t.Fatal(err)
			}
			retryLease, err := database.AcquireAgentTurn(ctx, claimAgentTurnJob(t, database, ctx, retry, time.Second), retry.ControlRevision, "runtime-retry", time.Second, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
				t.Fatal(err)
			}

			mutationID := "46000000-0000-4000-8000-000000000036"
			spec := store.MutationSpec{OperationID: "unsettled-lineage-operation", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"uncertain"}`)}
			if _, err := pool.Exec(ctx, `
INSERT INTO tool_invocations (
    id, agent_turn_id, execution_epoch, operation_lineage_id, invocation_number,
    tool_name, kind, state, idempotency_key, operation_id, request, started_at, last_error
)
VALUES ($1, $2, $3, $2, 1, $4, 'MUTATION', $5, $6, $6, $7, clock_timestamp(), 'uncertain outcome')`,
				mutationID, root.ID, root.ExecutionEpoch, spec.ToolName, state, spec.OperationID, spec.Request); err != nil {
				t.Fatal(err)
			}
			reservation, err := database.ReserveMutation(ctx, retryLease, spec)
			if err != nil {
				t.Fatalf("ReserveMutation() retry for %s error = %v", state, err)
			}
			if reservation.ID != mutationID || reservation.AgentTurnID != root.ID || reservation.State != state {
				t.Fatalf("ReserveMutation() retry for %s = %#v, want original reservation", state, reservation)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_invocations WHERE operation_lineage_id = $1 AND operation_id = $2`, root.ID, spec.OperationID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Errorf("%s retry reservations = %d, want 1", state, count)
			}
			var replays int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_invocation_replays WHERE agent_turn_id = $1`, retry.ID).Scan(&replays); err != nil {
				t.Fatal(err)
			}
			if replays != 0 {
				t.Errorf("%s retry replay rows = %d, want 0 for unresolved source", state, replays)
			}
		})
	}
}

func TestAgentTurnRecoveryBlocksSuccessorWhileMutationIsUnsettled(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', state_revision = 1 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
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
	if _, err := databases[0].GetAgentTurnMutationReconciliationContext(ctx, *reconcileJob); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("GetAgentTurnMutationReconciliationContext() before runtime stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
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
	if _, err := databases[0].CompleteAgentTurnMutationReconciliation(ctx, *reconcileJob); err != nil {
		t.Fatalf("CompleteAgentTurnMutationReconciliation() error = %v", err)
	}
	settled, err := databases[0].GetAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("GetAgentTurnRecovery() after final mutation error = %v", err)
	}
	if !settled.SuccessorAllowed || settled.RecoverySettledAt == nil {
		t.Errorf("recovery after final mutation = %#v, want successor allowed", settled)
	}
	var retryOf string
	if err := pool.QueryRow(ctx, `
SELECT payload->>'retry_of_turn_id' FROM jobs
WHERE agent_turn_settlement_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, settled.SettlementID).Scan(&retryOf); err != nil {
		t.Fatalf("read recovery successor preparation: %v", err)
	}
	if retryOf != turn.ID {
		t.Errorf("recovery successor retries %s, want %s", retryOf, turn.ID)
	}
	settled, err = databases[0].CompleteAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil || !settled.SuccessorAllowed {
		t.Errorf("CompleteAgentTurnRecovery() after atomic settlement = (%#v, %v)", settled, err)
	}
	for _, jobID := range []string{recovery.StopRuntimeJobID, recovery.ReconcileMutationsJobID} {
		job, err := databases[0].GetJob(ctx, jobID)
		if err != nil || job.Status != store.JobSucceeded {
			t.Errorf("recovery job %s = (%#v, %v), want SUCCEEDED", jobID, job, err)
		}
	}
}

func TestBeginAgentTurnMutationRecoveryHandsOffLiveTurnAndReleasesSlot(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 41)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, 5*time.Second)
	lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-live-handoff", 5*time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:live-handoff", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"ambiguous"}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() error = %v", err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatalf("StartMutation() error = %v", err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("MCP response was lost")); err != nil {
		t.Fatalf("MarkMutationUnknown() error = %v", err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}

	recovery, err := database.BeginAgentTurnMutationRecovery(ctx, lease)
	if err != nil {
		t.Fatalf("BeginAgentTurnMutationRecovery() before lease expiry error = %v", err)
	}
	if !time.Now().Before(lease.LeaseExpiresAt) {
		t.Fatal("live handoff did not complete before the original lease expired")
	}
	if recovery.TurnID != turn.ID || recovery.JobID != job.ID || recovery.ExecutionEpoch != turn.ExecutionEpoch ||
		recovery.Status != store.AgentTurnReconciling || !recovery.MutationsUnsettled || recovery.SuccessorAllowed ||
		!recovery.RuntimeStopRequired || recovery.RecoveryStartedAt == nil || recovery.RuntimeStoppedAt != nil ||
		recovery.RecoverySettledAt != nil || recovery.StopRuntimeJobID == "" || recovery.ReconcileMutationsJobID == "" {
		t.Fatalf("BeginAgentTurnMutationRecovery() = %#v", recovery)
	}
	repeated, err := database.BeginAgentTurnMutationRecovery(ctx, lease)
	if err != nil || repeated.StopRuntimeJobID != recovery.StopRuntimeJobID ||
		repeated.ReconcileMutationsJobID != recovery.ReconcileMutationsJobID || repeated.RecoveryStartedAt == nil ||
		!repeated.RecoveryStartedAt.Equal(*recovery.RecoveryStartedAt) {
		t.Fatalf("idempotent BeginAgentTurnMutationRecovery() = (%#v, %v), want original barrier", repeated, err)
	}
	stale := lease
	stale.OwnerToken = "30000000-0000-4000-8000-000000000099"
	if _, err := database.BeginAgentTurnMutationRecovery(ctx, stale); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Errorf("BeginAgentTurnMutationRecovery() with stale owner error = %v, want ErrAgentTurnFenceLost", err)
	}

	var mutationState, attemptStatus string
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id = $1`, mutation.ID).Scan(&mutationState); err != nil {
		t.Fatalf("read handed-off mutation: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM job_attempts WHERE job_id = $1 AND attempt_number = $2`, job.ID, job.Attempt).Scan(&attemptStatus); err != nil {
		t.Fatalf("read handed-off execution attempt: %v", err)
	}
	completedJob, err := database.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob() after live handoff error = %v", err)
	}
	var jobResult struct {
		ControlledHandoff bool `json:"controlled_handoff"`
	}
	if err := json.Unmarshal(completedJob.Result, &jobResult); err != nil {
		t.Fatalf("decode controlled-handoff result: %v", err)
	}
	var turnStatus string
	var turnActive, admissionOpen, runtimeStopRequired, ownerCleared, leaseCleared, completed, recoveryStarted, runtimeStoppedNull, recoverySettledNull bool
	var stopJobID, reconcileJobID string
	if err := pool.QueryRow(ctx, `
SELECT status, active, mutation_admission_open, runtime_stop_required,
       owner_id IS NULL AND owner_token IS NULL,
       leased_at IS NULL AND lease_expires_at IS NULL AND heartbeat_at IS NULL,
       completed_at IS NOT NULL, recovery_started_at IS NOT NULL,
       runtime_stopped_at IS NULL, recovery_settled_at IS NULL,
       stop_runtime_job_id::text, reconcile_mutations_job_id::text
FROM agent_turns WHERE id = $1`, turn.ID).Scan(
		&turnStatus, &turnActive, &admissionOpen, &runtimeStopRequired, &ownerCleared,
		&leaseCleared, &completed, &recoveryStarted, &runtimeStoppedNull, &recoverySettledNull,
		&stopJobID, &reconcileJobID,
	); err != nil {
		t.Fatalf("read handed-off Agent Turn: %v", err)
	}
	var slots int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turn_slots WHERE agent_turn_id = $1`, turn.ID).Scan(&slots); err != nil {
		t.Fatalf("count released Agent Turn slots: %v", err)
	}
	if mutationState != string(store.MutationReconciling) || attemptStatus != string(store.JobSucceeded) ||
		completedJob.Status != store.JobSucceeded || !jobResult.ControlledHandoff || turnStatus != string(store.AgentTurnReconciling) ||
		turnActive || admissionOpen || !runtimeStopRequired || !ownerCleared || !leaseCleared || !completed ||
		!recoveryStarted || !runtimeStoppedNull || !recoverySettledNull || stopJobID != recovery.StopRuntimeJobID ||
		reconcileJobID != recovery.ReconcileMutationsJobID || slots != 0 {
		t.Errorf("live handoff durability = mutation %s, attempt %s, job %s/%s, turn %s active=%t admission=%t stop=%t owner=%t lease=%t completed=%t recovery=%t stopped-null=%t settled-null=%t jobs=%s/%s slots=%d",
			mutationState, attemptStatus, completedJob.Status, completedJob.Result, turnStatus, turnActive,
			admissionOpen, runtimeStopRequired, ownerCleared, leaseCleared, completed, recoveryStarted,
			runtimeStoppedNull, recoverySettledNull, stopJobID, reconcileJobID, slots)
	}

	other := seedAgentSession(t, pool, 42)
	otherTurn, err := database.AllocateAgentTurn(ctx, other.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() in another Workflow after slot release error = %v", err)
	}
	otherJob := claimAgentTurnJob(t, database, ctx, otherTurn, time.Second)
	if _, err := database.AcquireAgentTurn(ctx, otherJob, otherTurn.ControlRevision, "runtime-after-handoff", time.Second, 1); err != nil {
		t.Fatalf("AcquireAgentTurn() in another Workflow after slot release error = %v", err)
	}
}

func TestBeginAgentTurnMutationRecoveryRejectsInvalidStateWithoutPartialJobs(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *store.Store, context.Context, store.AgentTurnLease)
		stale func(store.AgentTurnLease) store.AgentTurnLease
	}{
		{
			name: "invalid turn status",
			setup: func(t *testing.T, database *store.Store, ctx context.Context, lease store.AgentTurnLease) {
				t.Helper()
				if err := database.OpenMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "residual in-flight mutation",
			setup: func(t *testing.T, database *store.Store, ctx context.Context, lease store.AgentTurnLease) {
				t.Helper()
				if err := database.OpenMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				for index := range 2 {
					mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
						OperationID: fmt.Sprintf("recovery-state-%d", index), ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
					})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
						t.Fatal(err)
					}
					if index == 0 {
						if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("unknown")); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := database.CloseMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stale owner authority",
			setup: func(t *testing.T, database *store.Store, ctx context.Context, lease store.AgentTurnLease) {
				t.Helper()
				if err := database.OpenMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
					OperationID: "stale-recovery-authority", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
					t.Fatal(err)
				}
				if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("unknown")); err != nil {
					t.Fatal(err)
				}
				if err := database.CloseMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
			},
			stale: func(lease store.AgentTurnLease) store.AgentTurnLease {
				lease.OwnerToken = "30000000-0000-4000-8000-000000000099"
				return lease
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			fixture := seedAgentSession(t, pool, 43)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			job := claimAgentTurnJob(t, database, ctx, turn, 5*time.Second)
			lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-invalid-recovery", 5*time.Second, 1)
			if err != nil {
				t.Fatal(err)
			}
			test.setup(t, database, ctx, lease)
			if test.stale != nil {
				lease = test.stale(lease)
			}
			if _, err := database.BeginAgentTurnMutationRecovery(ctx, lease); err == nil {
				t.Fatal("BeginAgentTurnMutationRecovery() succeeded")
			}
			var recoveryJobs int
			var active bool
			var executionStatus string
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE agent_turn_id = $1 AND kind IN ('STOP_STALE_RUNTIME', 'RECONCILE_AGENT_TURN_MUTATIONS')`, turn.ID).Scan(&recoveryJobs); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT active FROM agent_turns WHERE id = $1`, turn.ID).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, job.ID).Scan(&executionStatus); err != nil {
				t.Fatal(err)
			}
			if recoveryJobs != 0 || !active || executionStatus != string(store.JobLeased) {
				t.Errorf("failed handoff left recovery jobs=%d active=%t execution=%s; want 0, true, LEASED", recoveryJobs, active, executionStatus)
			}
		})
	}
}

func TestAgentTurnRecoveryBlocksOtherSuccessorsAndSchedulesExactRole(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 44)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', state_revision = 1 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}

	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, 5*time.Second)
	lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-workflow-barrier", 5*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "workflow-barrier", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("unknown")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginAgentTurnMutationRecovery(ctx, lease); err != nil {
		t.Fatal(err)
	}

	reviewerAssignmentID := "40000000-0000-4000-8000-000000000445"
	reviewerSessionID := "40000000-0000-4000-8000-000000000446"
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, 'REVIEWER', 'ACTIVE', 'reviewer', 'runtime', '1', 'sha256:reviewer', $3)`,
		reviewerAssignmentID, fixture.workflowID, "/state/"+reviewerAssignmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ($1, $2, 1, 'reviewer-session', 'runtime', '1', 'sha256:reviewer', $3, 'ACTIVE')`,
		reviewerSessionID, reviewerAssignmentID, "/state/"+reviewerAssignmentID); err != nil {
		t.Fatal(err)
	}
	developerSpec := fixture.turnSpec()
	reviewerSpec := fixture.turnSpec()
	reviewerSpec.AgentSessionID = reviewerSessionID
	reviewerSpec.Purpose = workflow.TurnPurposeReview
	reviewerSpec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10, "Review test instructions.", nil)
	for role, spec := range map[workflow.Role]store.AgentTurnSpec{
		workflow.RoleDeveloper: developerSpec,
		workflow.RoleReviewer:  reviewerSpec,
	} {
		if _, err := database.AllocateAgentTurn(ctx, spec); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
			t.Errorf("AllocateAgentTurn() for %s successor error = %v, want ErrAgentTurnRecoveryUnsettled", role, err)
		}
	}
	reconcileJob, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.ReconcileAgentTurnMutationsJobKind, "reconcile-worker", time.Second)
	if err != nil || reconcileJob == nil {
		t.Fatalf("ClaimJobKind() reconciliation = (%#v, %v)", reconcileJob, err)
	}
	if _, err := database.GetAgentTurnMutationReconciliationContext(ctx, *reconcileJob); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("GetAgentTurnMutationReconciliationContext() before stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	stopJob, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "stop-worker", time.Second)
	if err != nil || stopJob == nil {
		t.Fatalf("ClaimJobKind() stop = (%#v, %v)", stopJob, err)
	}
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, *stopJob); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, *reconcileJob, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationFailed, LastError: "external outcome was not applied",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, *reconcileJob); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CompleteAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AllocateAgentTurn(ctx, reviewerSpec); !errors.Is(err, store.ErrWorkflowSuccessorConflict) {
		t.Fatalf("AllocateAgentTurn() for Reviewer beside recovery retry error = %v, want ErrWorkflowSuccessorConflict", err)
	}
	var role, retryOf string
	if err := pool.QueryRow(ctx, `
SELECT payload->>'role', payload->>'retry_of_turn_id'
FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN' AND status = 'AVAILABLE'`, fixture.workflowID).Scan(&role, &retryOf); err != nil {
		t.Fatal(err)
	}
	if role != string(workflow.RoleDeveloper) || retryOf != turn.ID {
		t.Errorf("recovery preparation = %s retry of %s", role, retryOf)
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

func TestClaimAndAcquireAgentTurnReturnsNoWork(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lease, acquired, err := databases[0].ClaimAndAcquireAgentTurn(ctx, "runtime", time.Second, 1)
	if err != nil || acquired || lease.ID != "" {
		t.Fatalf("ClaimAndAcquireAgentTurn() = (%#v, %t, %v), want no work", lease, acquired, err)
	}
}

func TestClaimAndAcquireAgentTurnLeavesJobAvailableWhenConcurrencyIsFull(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	firstFixture := seedAgentSession(t, pool, 71)
	firstTurn, err := database.AllocateAgentTurn(ctx, firstFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	first, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, "runtime-first", time.Second, 1)
	if err != nil || !acquired || first.ID != firstTurn.ID {
		t.Fatalf("first ClaimAndAcquireAgentTurn() = (%#v, %t, %v)", first, acquired, err)
	}
	if first.JobLease.Status != store.JobLeased || first.JobLease.Attempt != 1 || first.JobLease.LeaseOwner != "runtime-first" {
		t.Fatalf("atomic Agent Turn Job lease = %#v, want leased attempt 1", first.JobLease)
	}
	if err := database.ValidateTurnFence(ctx, first); err != nil {
		t.Fatalf("ValidateTurnFence() for atomic acquisition error = %v", err)
	}

	secondFixture := seedAgentSession(t, pool, 72)
	secondTurn, err := database.AllocateAgentTurn(ctx, secondFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, "runtime-second", time.Second, 1)
	if err != nil || acquired || lease.ID != "" {
		t.Fatalf("capacity-limited ClaimAndAcquireAgentTurn() = (%#v, %t, %v), want no acquisition", lease, acquired, err)
	}

	var status string
	var attempts, attemptRows int
	if err := pool.QueryRow(ctx, `
SELECT status, attempt_count,
       (SELECT count(*) FROM job_attempts AS attempt WHERE attempt.job_id = job.id)
FROM jobs AS job WHERE agent_turn_id = $1`, secondTurn.ID).Scan(&status, &attempts, &attemptRows); err != nil {
		t.Fatal(err)
	}
	if status != string(store.JobAvailable) || attempts != 0 || attemptRows != 0 {
		t.Errorf("capacity-limited job = (%s, %d attempts, %d attempt rows), want AVAILABLE/0/0", status, attempts, attemptRows)
	}
}

func TestClaimAndAcquireAgentTurnCompetingClaimsRespectGlobalCapacity(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for index := range databases {
		fixture := seedAgentSession(t, pool, 80+index)
		if _, err := databases[index].AllocateAgentTurn(ctx, fixture.turnSpec()); err != nil {
			t.Fatalf("AllocateAgentTurn(%d) error = %v", index, err)
		}
	}

	type claimResult struct {
		lease    store.AgentTurnLease
		acquired bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan claimResult, len(databases))
	var wait sync.WaitGroup
	for index := range databases {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			lease, acquired, err := databases[index].ClaimAndAcquireAgentTurn(ctx, fmt.Sprintf("runtime-%d", index), time.Second, 2)
			results <- claimResult{lease: lease, acquired: acquired, err: err}
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)

	seen := make(map[string]bool)
	acquired := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("ClaimAndAcquireAgentTurn() error = %v", result.err)
		}
		if !result.acquired {
			continue
		}
		acquired++
		if result.lease.ID == "" || seen[result.lease.ID] {
			t.Errorf("acquired duplicate or empty Agent Turn %q", result.lease.ID)
		}
		seen[result.lease.ID] = true
	}
	var leasedJobs, availableJobs, attemptRows, slots int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'LEASED'),
       count(*) FILTER (WHERE status = 'AVAILABLE'),
       (SELECT count(*) FROM job_attempts),
       (SELECT count(*) FROM agent_turn_slots)
FROM jobs WHERE kind = 'RUN_AGENT_TURN'`).Scan(&leasedJobs, &availableJobs, &attemptRows, &slots); err != nil {
		t.Fatal(err)
	}
	if acquired != 2 || leasedJobs != 2 || availableJobs != 2 || attemptRows != 2 || slots != 2 {
		t.Errorf("competing claims = %d acquired, jobs %d/%d, attempts %d, slots %d; want 2, 2/2, 2, 2",
			acquired, leasedJobs, availableJobs, attemptRows, slots)
	}
}

func TestClaimAndAcquireAgentTurnRejectsMalformedAndStaleJobsWithoutClaiming(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	malformedFixture := seedAgentSession(t, pool, 91)
	malformedTurn, err := database.AllocateAgentTurn(ctx, malformedFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET payload = '{}'::jsonb WHERE agent_turn_id = $1`, malformedTurn.ID); err != nil {
		t.Fatal(err)
	}
	if lease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, "runtime-malformed", time.Second, 1); !errors.Is(err, store.ErrAgentTurnFenceLost) || acquired || lease.ID != "" {
		t.Fatalf("malformed ClaimAndAcquireAgentTurn() = (%#v, %t, %v), want fence loss", lease, acquired, err)
	}
	assertUnclaimedAgentTurnJob(t, pool, ctx, malformedTurn.ID)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp() WHERE agent_turn_id = $1`, malformedTurn.ID); err != nil {
		t.Fatal(err)
	}

	staleFixture := seedAgentSession(t, pool, 92)
	staleTurn, err := database.AllocateAgentTurn(ctx, staleFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET control_revision = control_revision + 1 WHERE id = $1`, staleFixture.sessionID); err != nil {
		t.Fatal(err)
	}
	if lease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, "runtime-stale", time.Second, 1); !errors.Is(err, store.ErrAgentTurnFenceLost) || acquired || lease.ID != "" {
		t.Fatalf("stale ClaimAndAcquireAgentTurn() = (%#v, %t, %v), want fence loss", lease, acquired, err)
	}
	assertUnclaimedAgentTurnJob(t, pool, ctx, staleTurn.ID)
}

func assertUnclaimedAgentTurnJob(t *testing.T, pool *pgxpool.Pool, ctx context.Context, turnID string) {
	t.Helper()
	var status, turnStatus string
	var attempts, attemptRows, slots int
	if err := pool.QueryRow(ctx, `
SELECT job.status, job.attempt_count,
       (SELECT count(*) FROM job_attempts AS attempt WHERE attempt.job_id = job.id),
       (SELECT count(*) FROM agent_turn_slots AS slot WHERE slot.agent_turn_id = job.agent_turn_id),
       turn.status
FROM jobs AS job
JOIN agent_turns AS turn ON turn.id = job.agent_turn_id
WHERE job.agent_turn_id = $1`, turnID).Scan(&status, &attempts, &attemptRows, &slots, &turnStatus); err != nil {
		t.Fatal(err)
	}
	if status != string(store.JobAvailable) || attempts != 0 || attemptRows != 0 || slots != 0 || turnStatus != string(store.AgentTurnQueued) {
		t.Errorf("rejected Agent Turn job = job %s/%d, attempts %d, slots %d, turn %s; want AVAILABLE/0/0/0/QUEUED",
			status, attempts, attemptRows, slots, turnStatus)
	}
}

func TestAgentTurnExpiredOwnershipRequiresRecoveryAndNewEpoch(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', state_revision = 1 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
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
	if acknowledged.RuntimeStoppedAt == nil || !acknowledged.SuccessorAllowed || acknowledged.RecoverySettledAt == nil {
		t.Errorf("acknowledged recovery = %#v, want stopped and settled barrier", acknowledged)
	}
	var purpose, retryOf string
	if err := pool.QueryRow(ctx, `
SELECT payload->>'purpose', payload->>'retry_of_turn_id'
FROM jobs WHERE agent_turn_settlement_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, acknowledged.SettlementID).Scan(&purpose, &retryOf); err != nil {
		t.Fatalf("read recovery preparation job: %v", err)
	}
	if purpose != string(workflow.TurnPurposeRetry) || retryOf != turn.ID {
		t.Errorf("recovery preparation = %s retry of %s", purpose, retryOf)
	}
	for range 2 {
		settled, err := databases[0].CompleteAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
		if err != nil {
			t.Fatalf("repeated CompleteAgentTurnRecovery() error = %v", err)
		}
		if !settled.SuccessorAllowed || settled.RecoverySettledAt == nil {
			t.Errorf("repeated recovery completion = %#v, want successor allowed", settled)
		}
	}
}

func TestRecoverExpiredAgentTurnAcceptsPostAdmissionCloseStatuses(t *testing.T) {
	for _, test := range []struct {
		name                   string
		mutationState          store.MutationState
		wantStatus             store.AgentTurnStatus
		wantReconciliationJob  bool
		wantMutationsUnsettled bool
	}{
		{name: "SETTLING without unknown mutations", wantStatus: store.AgentTurnInterrupted},
		{name: "RECONCILING with an unknown mutation", mutationState: store.MutationUnknown, wantStatus: store.AgentTurnReconciling, wantReconciliationJob: true, wantMutationsUnsettled: true},
		{name: "SETTLING with a successful mutation", mutationState: store.MutationSucceeded, wantStatus: store.AgentTurnReconciling, wantReconciliationJob: true},
		{name: "SETTLING with a failed mutation", mutationState: store.MutationFailed, wantStatus: store.AgentTurnInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			fixture := seedAgentSession(t, pool, 51)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			job := claimAgentTurnJob(t, database, ctx, turn, 80*time.Millisecond)
			lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-post-close", 80*time.Millisecond, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.OpenMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			if test.mutationState != "" {
				mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
					OperationID: "post-close-mutation", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
					t.Fatal(err)
				}
				switch test.mutationState {
				case store.MutationUnknown:
					if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("response lost")); err != nil {
						t.Fatal(err)
					}
				case store.MutationSucceeded:
					if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"comment_id":51}`)); err != nil {
						t.Fatal(err)
					}
				case store.MutationFailed:
					if err := database.FailMutation(ctx, lease, mutation.ID, errors.New("definite failure")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := database.CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}

			time.Sleep(110 * time.Millisecond)
			recovery, err := database.RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
			if err != nil {
				t.Fatalf("RecoverExpiredAgentTurn() after CloseMutationAdmission() error = %v", err)
			}
			if recovery.Status != test.wantStatus || recovery.StopRuntimeJobID == "" ||
				(recovery.ReconcileMutationsJobID != "") != test.wantReconciliationJob ||
				recovery.MutationsUnsettled != test.wantMutationsUnsettled {
				t.Errorf("recovery = %#v, want status %s, stop job, reconciliation job=%t", recovery, test.wantStatus, test.wantReconciliationJob)
			}
		})
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

func TestSuccessfulSubmitReviewBindsReviewerActorWithCompareOrSet(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 33)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET role = 'REVIEWER', agent_profile_name = 'reviewer' WHERE id = $1`, fixture.assignmentID); err != nil {
		t.Fatalf("prepare Reviewer Assignment: %v", err)
	}
	turnSpec := fixture.turnSpec()
	turnSpec.Purpose = workflow.TurnPurposeReview
	turnSpec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10, "Review test instructions.", nil)
	turn, err := databases[0].AllocateAgentTurn(ctx, turnSpec)
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "review-runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}

	completeReview := func(operationID string, actorID int64) (store.MutationReservation, error) {
		t.Helper()
		mutation, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
			OperationID: operationID, ToolName: "submit_review", Request: json.RawMessage(`{"event":"APPROVE"}`),
		})
		if err != nil {
			return store.MutationReservation{}, err
		}
		if _, err := databases[0].StartMutation(ctx, lease, mutation.ID); err != nil {
			return store.MutationReservation{}, err
		}
		result := json.RawMessage(fmt.Sprintf(`{"review_id":1,"actor_id":%d}`, actorID))
		return mutation, databases[0].CompleteMutation(ctx, lease, mutation.ID, result)
	}

	if _, err := completeReview("submit-review-1", 9101); err != nil {
		t.Fatalf("first submit_review completion error = %v", err)
	}
	if _, err := completeReview("submit-review-2", 9101); err != nil {
		t.Fatalf("same-actor submit_review completion error = %v", err)
	}
	conflicting, err := completeReview("submit-review-3", 9102)
	if !errors.Is(err, store.ErrReviewerActorConflict) {
		t.Fatalf("different-actor submit_review completion error = %v, want ErrReviewerActorConflict", err)
	}
	var actorID int64
	var mutationState string
	if err := pool.QueryRow(ctx, `SELECT github_app_actor_id FROM agent_assignments WHERE id = $1`, fixture.assignmentID).Scan(&actorID); err != nil {
		t.Fatalf("read bound Reviewer actor: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id = $1`, conflicting.ID).Scan(&mutationState); err != nil {
		t.Fatalf("read conflicting submit_review mutation: %v", err)
	}
	if actorID != 9101 || mutationState != string(store.MutationInFlight) {
		t.Errorf("CAS outcome = actor %d, mutation %s; want actor 9101 and IN_FLIGHT rollback", actorID, mutationState)
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
	for name, value := range map[string]string{
		"assignment": store.RuntimeLabelAssignmentID,
		"session":    store.RuntimeLabelSessionID,
		"turn":       store.RuntimeLabelTurnID,
		"epoch":      store.RuntimeLabelEpoch,
	} {
		want := map[string]string{
			"assignment": "io.omnigrex.assignment",
			"session":    "io.omnigrex.agent-session",
			"turn":       "io.omnigrex.agent-turn",
			"epoch":      "io.omnigrex.execution-epoch",
		}[name]
		if value != want {
			t.Errorf("RuntimeLabel%s = %q, want %q", name, value, want)
		}
	}
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
		Purpose:         workflow.TurnPurposeInitialDevelopment,
		ControlRevision: 1, AgentProfileCommitSHA: "0123456789abcdef",
		AgentProfileContentSHA256: digest[:],
		AgentProfileConfig:        agentProfileConfig("developer", workflow.RoleDeveloper, "runtime/1", "provider/test", "", 10, "Test instructions.", nil),
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
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, 'DEVELOPER', 'ACTIVE', 'developer', 'runtime', '1', 'sha256:test', $3)`, fixture.assignmentID, fixture.workflowID, "/state/"+fixture.assignmentID)
	if err != nil {
		t.Fatalf("seed agent assignment: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ($1, $2, 1, $3, 'runtime', '1', 'sha256:test', $4, 'ACTIVE')`, fixture.sessionID, fixture.assignmentID, "session-"+fixture.sessionID, "/state/"+fixture.assignmentID)
	if err != nil {
		t.Fatalf("seed agent session: %v", err)
	}
	return fixture
}

type successfulMutationVerificationReconciler struct {
	mutex sync.Mutex
	calls int
}

func (reconciler *successfulMutationVerificationReconciler) Reconcile(_ context.Context, _ store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (mcp.MutationReconciliationResult, error) {
	reconciler.mutex.Lock()
	reconciler.calls++
	reconciler.mutex.Unlock()
	return mcp.MutationReconciliationResult{
		Disposition: mcp.ReconciliationFound,
		Outcome: store.RecoveredMutationOutcome{
			State: store.MutationSucceeded, Result: append(json.RawMessage(nil), mutation.Result...),
		},
	}, nil
}

func (reconciler *successfulMutationVerificationReconciler) callCount() int {
	reconciler.mutex.Lock()
	defer reconciler.mutex.Unlock()
	return reconciler.calls
}
