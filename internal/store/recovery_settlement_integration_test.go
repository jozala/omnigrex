//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestControlledRecoverySettlementRequiresStopBeforeMutation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lease, mutation, recovery, stopLease, mutationLease := beginControlledRecovery(
		t, database, pool, ctx, 831, "controlled-order", "comment_on_issue", json.RawMessage(`{"body":"ambiguous"}`),
	)

	if _, err := database.ReconcileRecoveredMutation(ctx, mutationLease, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"comment_id":831}`),
	}); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Fatalf("ReconcileRecoveredMutation() before stop error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, mutationLease, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"comment_id":831}`),
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if completed.SettlementID == "" || !completed.SuccessorAllowed || completed.Status != store.AgentTurnInterrupted {
		t.Fatalf("completed recovery = %#v", completed)
	}
	assertRecoverySettlement(t, pool, ctx, lease, recovery, completed, mutationLease, workflow.ReasonInfrastructureRetry, 1, 0)
}

func TestExpiredRecoveryWithoutMutationsSettlesFromStopAcknowledgement(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 833, workflow.RoleDeveloper, "")
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	for _, expiration := range []struct {
		query string
		args  []any
	}{
		{`UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, []any{lease.JobLease.ID}},
		{`UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1 AND attempt_number = $2`, []any{lease.JobLease.ID, lease.JobLease.Attempt}},
		{`UPDATE agent_turns SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, []any{lease.ID}},
		{`UPDATE agent_turn_slots SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE agent_turn_id = $1`, []any{lease.ID}},
	} {
		if _, err := pool.Exec(ctx, expiration.query, expiration.args...); err != nil {
			t.Fatal(err)
		}
	}
	recovery, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.ReconcileMutationsJobID != "" {
		t.Fatalf("no-mutation recovery = %#v", recovery)
	}
	stopLease := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "expired-stop")
	completed, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoverySettlement(t, pool, ctx, lease, recovery, completed, stopLease, workflow.ReasonInfrastructureRetry, 1, 0)
}

func TestControlledRuntimeRecoveryWithoutMutationsSettlesOnlyFromStopAcknowledgement(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 837, workflow.RoleDeveloper, "")
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}

	recovery, err := database.BeginAgentTurnRecovery(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Status != store.AgentTurnInterrupted || recovery.MutationsUnsettled || recovery.SuccessorAllowed ||
		recovery.StopRuntimeJobID == "" || recovery.ReconcileMutationsJobID != "" || recovery.SettlementID != "" {
		t.Fatalf("controlled no-mutation recovery = %#v", recovery)
	}
	repeated, err := database.BeginAgentTurnRecovery(ctx, lease)
	if err != nil || repeated.StopRuntimeJobID != recovery.StopRuntimeJobID || repeated.ReconcileMutationsJobID != "" ||
		repeated.RecoveryStartedAt == nil || !repeated.RecoveryStartedAt.Equal(*recovery.RecoveryStartedAt) {
		t.Fatalf("repeated controlled no-mutation recovery = (%#v, %v)", repeated, err)
	}
	var settlements, successors int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = $1`, lease.ID).Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN' AND agent_turn_settlement_id IS NOT NULL`, lease.JobLease.WorkflowID).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if settlements != 0 || successors != 0 {
		t.Fatalf("before stop acknowledgement = %d settlements, %d successors", settlements, successors)
	}

	stopLease := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "controlled-no-mutation-stop")
	completed, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoverySettlement(t, pool, ctx, lease, recovery, completed, stopLease, workflow.ReasonInfrastructureRetry, 1, 0)
}

func TestRecoverySettlementCoalescesDeferredEventsWithRetryFallback(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lease, mutation, recovery, stopLease, mutationLease := beginControlledRecovery(
		t, database, pool, ctx, 834, "deferred-recovery", "comment_on_issue", json.RawMessage(`{"body":"ambiguous"}`),
	)
	insertDeferredSettlementEvent(t, pool, ctx, "78340000-0000-4000-8000-000000000001", lease.JobLease.WorkflowID, lease.ID, "deferred-head")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, mutationLease, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"comment_id":834}`),
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoverySettlement(t, pool, ctx, lease, recovery, completed, mutationLease, workflow.ReasonInfrastructureRetry, 0, 1)
	var fallbackRole, fallbackPurpose, retryOf string
	if err := pool.QueryRow(ctx, `
SELECT payload->>'fallback_role', payload->>'fallback_purpose', payload->>'retry_of_turn_id'
FROM jobs WHERE agent_turn_settlement_id = $1 AND kind = 'RECONCILE_PENDING_EVENTS'`, completed.SettlementID).Scan(
		&fallbackRole, &fallbackPurpose, &retryOf,
	); err != nil {
		t.Fatal(err)
	}
	if fallbackRole != string(workflow.RoleDeveloper) || fallbackPurpose != string(workflow.TurnPurposeRetry) || retryOf != lease.ID {
		t.Errorf("reconciliation fallback = %s/%s retry %s", fallbackRole, fallbackPurpose, retryOf)
	}
}

func TestRecoverySettlementExhaustsInfrastructureBudgetIntoHumanHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	triggerPreparationWorkflow(t, database, ctx, "78350000-0000-4000-8000-000000000001", "78350000-0000-4000-8000-000000000002")
	profile := preparationSpec("second-failure-profile", "openai/second-failure")
	root := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "second-failure-profile", "openai/second-failure")
	rootLease := acquireAndBindTurn(t, database, ctx, root, "second-failure-acp")
	if err := database.OpenMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	first, err := database.SettleAgentTurn(ctx, rootLease, failedSettlementObservation("first infrastructure failure"))
	if err != nil || first.Reason != workflow.ReasonInfrastructureRetry {
		t.Fatalf("first infrastructure settlement = (%#v, %v)", first, err)
	}
	retry, err := database.PrepareAgentTurn(ctx, claimPreparationJob(t, database, ctx), profile)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireAgentTurn(ctx, claimAgentTurnJob(t, database, ctx, retry.Turn, time.Second), retry.Turn.ControlRevision, "second-failure-runtime", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Session.ID != root.Session.ID || retry.Turn.RetryOfTurnID != root.Turn.ID {
		t.Fatalf("first retry = session %s retry of %s", retry.Session.ID, retry.Turn.RetryOfTurnID)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "second-failure-mutation", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"ambiguous"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("response lost")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	recovery, err := database.BeginAgentTurnMutationRecovery(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	stopLease := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "second-failure-stop")
	mutationLease := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "second-failure-mutation")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, mutationLease, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"comment_id":835}`),
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoverySettlement(t, pool, ctx, lease, recovery, completed, mutationLease, workflow.ReasonInfrastructureRetriesExhausted, 0, 0)
	var state, workflowReason, attemptReason string
	var handoffs, labels int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.human_handoff_reason, attempt.human_handoff_reason,
       count(*) FILTER (WHERE job.kind = 'PUBLISH_HUMAN_HANDOFF'),
       count(*) FILTER (WHERE job.kind = 'RECONCILE_GITHUB_LABELS')
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.id = $2
LEFT JOIN jobs AS job ON job.agent_turn_settlement_id = $3
WHERE workflow.id = $1
GROUP BY workflow.status, workflow.human_handoff_reason, attempt.human_handoff_reason`,
		lease.JobLease.WorkflowID, lease.WorkflowAttemptID, completed.SettlementID).Scan(
		&state, &workflowReason, &attemptReason, &handoffs, &labels,
	); err != nil {
		t.Fatal(err)
	}
	wantReason := string(workflow.ReasonInfrastructureRetriesExhausted)
	if state != string(workflow.StateNeedsHuman) || workflowReason != wantReason || attemptReason != wantReason || handoffs != 1 || labels != 1 {
		t.Errorf("Human Handoff = %s/%s/%s with %d handoffs and %d labels", state, workflowReason, attemptReason, handoffs, labels)
	}
}

func TestConcurrentStopAcknowledgementAndRecoveryCompletionAreSingleAndIdempotent(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, mutation, recovery, stopLease, mutationLease := beginControlledRecovery(
		t, databases[0], pool, ctx, 836, "concurrent-recovery", "comment_on_issue", json.RawMessage(`{"body":"ambiguous"}`),
	)
	const callers = 8
	results := make(chan store.AgentTurnRecovery, callers)
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for index := range callers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := databases[index%len(databases)].AcknowledgeRecoveredRuntimeStopped(ctx, stopLease)
			results <- result
			errs <- err
		}(index)
	}
	wait.Wait()
	close(results)
	close(errs)
	successes, fenced := 0, 0
	for result := range results {
		if result.SettlementID != "" || result.SuccessorAllowed {
			t.Fatalf("stop acknowledgement settled mutation-bearing recovery = %#v", result)
		}
	}
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrAgentTurnRecoveryFenceLost):
			fenced++
		default:
			t.Fatalf("concurrent stop acknowledgement error = %v", err)
		}
	}
	if successes != 1 || fenced != callers-1 {
		t.Fatalf("concurrent acknowledgements = %d successes, %d fenced", successes, fenced)
	}
	if _, err := databases[0].ReconcileRecoveredMutation(ctx, mutationLease, mutation.ID, store.RecoveredMutationOutcome{
		State: store.MutationSucceeded, Result: json.RawMessage(`{"comment_id":836}`),
	}); err != nil {
		t.Fatal(err)
	}
	successful, err := databases[0].CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoverySettlement(t, pool, ctx, lease, recovery, successful, mutationLease, workflow.ReasonInfrastructureRetry, 1, 0)

	settlementIDs := make(chan string, callers)
	errs = make(chan error, callers)
	for index := range callers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := databases[index%len(databases)].CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
			settlementIDs <- result.SettlementID
			errs <- err
		}(index)
	}
	wait.Wait()
	close(settlementIDs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for settlementID := range settlementIDs {
		if settlementID != successful.SettlementID {
			t.Errorf("idempotent completion settlement = %s, want %s", settlementID, successful.SettlementID)
		}
	}
}

func TestRecoveredTerminalMutationReplaysIntoFreshOutcomeReconciliation(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	triggerPreparationWorkflow(t, database, ctx, "78370000-0000-4000-8000-000000000001", "78370000-0000-4000-8000-000000000002")
	profile := preparationSpec("recovery-replay-profile", "openai/recovery-replay")
	root := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "recovery-replay-profile", "openai/recovery-replay")
	rootLease := acquireAndBindTurn(t, database, ctx, root, "recovery-replay-acp")
	if err := database.OpenMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	terminalSpec := store.MutationSpec{
		OperationID: "recovered-terminal-blocked", ToolName: mcp.ToolReportBlocked,
		Request:         json.RawMessage(`{"operation_id":"recovered-terminal-blocked","reason":"dependency unavailable","details":"recovered replay"}`),
		ExternalService: "omnigrex", ExternalResourceID: rootLease.JobLease.WorkflowID,
	}
	terminal, err := database.ReserveMutation(ctx, rootLease, terminalSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, rootLease, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkMutationUnknown(ctx, rootLease, terminal.ID, errors.New("response lost")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginAgentTurnMutationRecovery(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	stopLease := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "replay-stop")
	mutationLease := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "replay-mutation")
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReconcileRecoveredMutation(ctx, mutationLease, terminal.ID, store.RecoveredMutationOutcome{
		State:  store.MutationSucceeded,
		Result: json.RawMessage(`{"outcome":"BLOCKED","reason":"dependency unavailable","details":"recovered replay"}`),
	}); err != nil {
		t.Fatal(err)
	}
	preparationLease := claimPreparationJob(t, database, ctx)
	retry, err := database.PrepareAgentTurn(ctx, preparationLease, profile)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Session.ID != root.Session.ID || retry.Turn.RetryOfTurnID != root.Turn.ID {
		t.Fatalf("prepared retry = session %s retry %s", retry.Session.ID, retry.Turn.RetryOfTurnID)
	}
	retryLease, err := database.AcquireAgentTurn(ctx, claimAgentTurnJob(t, database, ctx, retry.Turn, time.Second), retry.Turn.ControlRevision, "replay-retry", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	replayed, err := database.ReserveMutation(ctx, retryLease, terminalSpec)
	if err != nil || replayed.ID != terminal.ID || replayed.State != store.MutationSucceeded {
		t.Fatalf("ReserveMutation() recovered replay = (%#v, %v)", replayed, err)
	}
	if err := database.AcknowledgeMutationReplay(ctx, retryLease, terminal.ID, terminalSpec); err != nil {
		t.Fatalf("AcknowledgeMutationReplay() error = %v", err)
	}
	if err := database.CloseMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: &replayOutcomeGitHub{}})
	if err != nil {
		t.Fatal(err)
	}
	response := acp.PromptResponse{StopReason: acp.StopReasonEndTurn}
	observation, err := reconciler.Reconcile(ctx, agentturn.OutcomeReconciliation{
		Lease: retryLease, Execution: execution, PromptResponse: &response,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Outcome != workflow.TurnOutcomeBlocked || observation.Completion.Status != store.AgentTurnSucceeded ||
		!strings.Contains(observation.Diagnostic, "dependency unavailable") {
		t.Fatalf("fresh retry reconciliation = %#v", observation)
	}
}

func beginControlledRecovery(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, number int, operationID, toolName string, request json.RawMessage) (store.AgentTurnLease, store.MutationReservation, store.AgentTurnRecovery, store.JobLease, store.JobLease) {
	t.Helper()
	_, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, number, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: operationID, ToolName: toolName, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("response lost")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	recovery, err := database.BeginAgentTurnMutationRecovery(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	stopLease := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "controlled-stop")
	mutationLease := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "controlled-mutation")
	return lease, mutation, recovery, stopLease, mutationLease
}

func claimRecoveryJob(t *testing.T, database *store.Store, ctx context.Context, kind, owner string) store.JobLease {
	t.Helper()
	lease, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, kind, owner, 10*time.Second)
	if err != nil || lease == nil {
		t.Fatalf("ClaimJobKind(%s) = (%#v, %v)", kind, lease, err)
	}
	return *lease
}

func assertRecoverySettlement(t *testing.T, pool *pgxpool.Pool, ctx context.Context, original store.AgentTurnLease, recovery store.AgentTurnRecovery, completed store.AgentTurnRecovery, authority store.JobLease, reason workflow.Reason, preparationJobs, reconciliationJobs int) {
	t.Helper()
	if completed.SettlementID == "" || completed.Continuation != "INFRASTRUCTURE_FAILURE_APPLIED" || completed.RecoverySettledAt == nil {
		t.Fatalf("recovery completion = %#v", completed)
	}
	wantOwnerHash := sha256.Sum256([]byte(original.OwnerToken))
	var settlementAuthority, executionJobID, executionLeaseOwner, executionLeaseToken string
	var recoveryJobID, recoveryJobKind, recoveryLeaseOwner, recoveryLeaseToken string
	var ownerID, terminalStatus, storedReason, observation, result, terminalError string
	var ownerHash []byte
	var executionAttempt, recoveryAttempt, settlements, used, preparations, reconciliations int
	if err := pool.QueryRow(ctx, `
SELECT settlement.authority, settlement.execution_job_id::text,
       settlement.job_attempt_number, settlement.job_lease_owner,
       settlement.job_lease_token::text, settlement.owner_id, settlement.owner_token_sha256,
       settlement.recovery_job_id::text, settlement.recovery_job_kind,
       settlement.recovery_job_attempt_number, settlement.recovery_job_lease_owner,
       settlement.recovery_job_lease_token::text, settlement.terminal_turn_status,
       settlement.reason, settlement.observation::text, settlement.result::text,
       settlement.terminal_last_error, attempt.infrastructure_failures,
       (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = $1),
       count(job.id) FILTER (WHERE job.kind = 'PREPARE_AGENT_TURN'),
       count(job.id) FILTER (WHERE job.kind = 'RECONCILE_PENDING_EVENTS')
FROM agent_turn_settlements AS settlement
JOIN workflow_attempts AS attempt ON attempt.id = settlement.workflow_attempt_id
LEFT JOIN jobs AS job ON job.agent_turn_settlement_id = settlement.id
WHERE settlement.id = $2
GROUP BY settlement.id, attempt.infrastructure_failures`, original.ID, completed.SettlementID).Scan(
		&settlementAuthority, &executionJobID, &executionAttempt, &executionLeaseOwner,
		&executionLeaseToken, &ownerID, &ownerHash, &recoveryJobID, &recoveryJobKind,
		&recoveryAttempt, &recoveryLeaseOwner, &recoveryLeaseToken, &terminalStatus,
		&storedReason, &observation, &result, &terminalError, &used, &settlements,
		&preparations, &reconciliations,
	); err != nil {
		t.Fatal(err)
	}
	if settlementAuthority != string(store.AgentTurnSettlementRecovery) || executionJobID != original.JobLease.ID ||
		executionAttempt != original.JobLease.Attempt || executionLeaseOwner != original.JobLease.LeaseOwner ||
		executionLeaseToken != original.JobLease.LeaseToken || ownerID != original.OwnerID ||
		string(ownerHash) != string(wantOwnerHash[:]) || recoveryJobID != authority.ID ||
		recoveryJobKind != authority.Kind || recoveryAttempt != authority.Attempt ||
		recoveryLeaseOwner != authority.LeaseOwner || recoveryLeaseToken != authority.LeaseToken {
		t.Errorf("settlement authority = %s execution %s/%d/%s/%s owner %s/%x recovery %s/%s/%d/%s/%s",
			settlementAuthority, executionJobID, executionAttempt, executionLeaseOwner, executionLeaseToken,
			ownerID, ownerHash, recoveryJobID, recoveryJobKind, recoveryAttempt, recoveryLeaseOwner, recoveryLeaseToken)
	}
	if terminalStatus != string(store.AgentTurnInterrupted) || storedReason != string(reason) || settlements != 1 ||
		used != 1 || preparations != preparationJobs || reconciliations != reconciliationJobs {
		t.Errorf("settlement result = status %s reason %s rows %d budget %d jobs %d/%d",
			terminalStatus, storedReason, settlements, used, preparations, reconciliations)
	}
	for _, secret := range []string{original.OwnerToken, authority.LeaseToken} {
		if strings.Contains(observation, secret) || strings.Contains(result, secret) || strings.Contains(terminalError, secret) {
			t.Errorf("settlement diagnostic artifacts contain authority credential %q", secret)
		}
	}
	if recovery.JobID != original.JobLease.ID {
		t.Errorf("recovery execution job = %s, want %s", recovery.JobID, original.JobLease.ID)
	}
}
