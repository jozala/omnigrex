package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/workflow"
)

// AgentTurnMutationReconciliationContext contains the durable external-read
// scope needed to reconcile mutations from one fenced Agent Turn epoch.
type AgentTurnMutationReconciliationContext struct {
	WorkflowID      string
	Repository      AgentTurnRepository
	Issue           AgentTurnIssue
	ChangeProposal  *AgentTurnChangeProposal
	Role            workflow.Role
	ReviewerActorID int64
	Turn            AgentTurn
}

// AgentTurnRuntimeCleanupContext identifies the recovered Assignment whose Runtime Process is being stopped.
type AgentTurnRuntimeCleanupContext struct {
	AssignmentID          string
	Role                  workflow.Role
	RuntimeProfileName    string
	RuntimeProfileVersion string
	DiscardWorkspace      bool
}

// AgentTurnRuntimeStopFailureAcknowledgement is the durable retry outcome of one stale-runtime stop attempt.
type AgentTurnRuntimeStopFailureAcknowledgement struct {
	JobID               string
	WorkflowID          string
	AgentTurnID         string
	Attempt             int
	RetryScheduled      bool
	EscalationScheduled bool
}

// AgentTurnMutationReconciliationAcknowledgement is the durable retry or
// Human Handoff outcome of one inconclusive reconciliation attempt.
type AgentTurnMutationReconciliationAcknowledgement struct {
	JobID                   string
	WorkflowID              string
	WorkflowAttemptID       string
	AgentTurnID             string
	Attempt                 int
	RetryScheduled          bool
	Escalated               bool
	UnresolvedMutationCount int
	Diagnostic              string
	WorkflowRevision        uint64
}

// GetAgentTurnRuntimeCleanupContext returns the source Assignment under the live stale-runtime stop-job fence.
func (store *Store) GetAgentTurnRuntimeCleanupContext(ctx context.Context, lease JobLease) (AgentTurnRuntimeCleanupContext, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRuntimeCleanupContext{}, fmt.Errorf("begin Agent Turn runtime cleanup context: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, StopStaleRuntimeJobKind)
	if err != nil {
		return AgentTurnRuntimeCleanupContext{}, err
	}
	cleanup := AgentTurnRuntimeCleanupContext{AssignmentID: job.AgentAssignmentID}
	if err := tx.QueryRow(ctx, `
SELECT role, runtime_profile_name, runtime_profile_version FROM agent_assignments
WHERE id = $1 AND workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(
		&cleanup.Role, &cleanup.RuntimeProfileName, &cleanup.RuntimeProfileVersion,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentTurnRuntimeCleanupContext{}, ErrAgentTurnRecoveryFenceLost
		}
		return AgentTurnRuntimeCleanupContext{}, fmt.Errorf("read Agent Turn runtime cleanup Assignment: %w", err)
	}
	if policy, ok := store.policies.Lookup(cleanup.Role); ok {
		cleanup.DiscardWorkspace = policy.DiscardWorkspace
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRuntimeCleanupContext{}, fmt.Errorf("commit Agent Turn runtime cleanup context: %w", err)
	}
	return cleanup, nil
}

// WithRecoveredRuntimeCleanupFence holds a live stop-job fence for the short workspace detachment.
func (store *Store) WithRecoveredRuntimeCleanupFence(ctx context.Context, lease JobLease, operation func(context.Context) error) error {
	if operation == nil {
		return errors.New("hold recovered Runtime Process cleanup fence: operation is nil")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin recovered Runtime Process cleanup fence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, StopStaleRuntimeJobKind); err != nil {
		return err
	}
	if err := operation(ctx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AcknowledgeRecoveredRuntimeStopFailure promptly releases a failed stop attempt while preserving cleanup authority.
func (store *Store) AcknowledgeRecoveredRuntimeStopFailure(ctx context.Context, lease JobLease, cause error, retryDelay time.Duration) (AgentTurnRuntimeStopFailureAcknowledgement, error) {
	if cause == nil || strings.TrimSpace(cause.Error()) == "" {
		return AgentTurnRuntimeStopFailureAcknowledgement{}, errors.New("acknowledge Agent Turn runtime stop failure: cause is empty")
	}
	if retryDelay < 0 || retryDelay > maximumJobDelay {
		return AgentTurnRuntimeStopFailureAcknowledgement{}, fmt.Errorf("acknowledge Agent Turn runtime stop failure: retry delay must be between zero and %s", maximumJobDelay)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRuntimeStopFailureAcknowledgement{}, fmt.Errorf("begin Agent Turn runtime stop failure acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, StopStaleRuntimeJobKind)
	if err != nil {
		return AgentTurnRuntimeStopFailureAcknowledgement{}, err
	}
	acknowledgement := AgentTurnRuntimeStopFailureAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, AgentTurnID: job.AgentTurnID,
		Attempt: job.AttemptCount, RetryScheduled: true,
	}
	if job.AttemptCount >= job.MaxAttempts {
		continued, scheduled, err := continueIrreversibleJobAfterExhaustionTx(ctx, tx, job)
		if err != nil {
			return AgentTurnRuntimeStopFailureAcknowledgement{}, err
		}
		if !continued {
			return AgentTurnRuntimeStopFailureAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
		}
		acknowledgement.EscalationScheduled = scheduled
	}
	if err := failWorkflowActionJobTx(ctx, tx, job, cause.Error(), true, true, retryDelay); err != nil {
		return AgentTurnRuntimeStopFailureAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRuntimeStopFailureAcknowledgement{}, fmt.Errorf("commit Agent Turn runtime stop failure acknowledgement: %w", err)
	}
	return acknowledgement, nil
}

// GetAgentTurnMutationReconciliationContext reads the repository, Work Item,
// Turn-bound Change Proposal, Role, Reviewer actor, and original Turn under the
// live reconciliation-job fence after stale Runtime Process stop is durable.
func (store *Store) GetAgentTurnMutationReconciliationContext(ctx context.Context, lease JobLease) (AgentTurnMutationReconciliationContext, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("begin Agent Turn mutation reconciliation context: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, turn, err := lockAgentTurnRecoveryJob(ctx, tx, lease, ReconcileAgentTurnMutationsJobKind)
	if err != nil {
		return AgentTurnMutationReconciliationContext{}, err
	}
	if err := requireRecoveredRuntimeStopped(ctx, tx, job); err != nil {
		return AgentTurnMutationReconciliationContext{}, err
	}
	var reconciliation AgentTurnMutationReconciliationContext
	if err := tx.QueryRow(ctx, `
SELECT id::text, repository_id, repository_owner, repository_name, issue_id, issue_number
FROM workflows WHERE id = $1`, job.WorkflowID).Scan(
		&reconciliation.WorkflowID, &reconciliation.Repository.ID,
		&reconciliation.Repository.Owner, &reconciliation.Repository.Name,
		&reconciliation.Issue.ID, &reconciliation.Issue.Number,
	); err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("read mutation reconciliation Work Item: %w", err)
	}
	reviewerRoles := store.rolesUsingToolAuthority("submit_review", role.ReviewerAuthority)
	if err := tx.QueryRow(ctx, `
SELECT source.role, COALESCE(source.github_app_actor_id, (
	SELECT reviewer.github_app_actor_id
	FROM agent_assignments AS reviewer
	WHERE reviewer.workflow_id = source.workflow_id
	  AND reviewer.role = ANY($3::text[])
	  AND reviewer.status <> 'SUPERSEDED'
	  AND reviewer.state_deleted_at IS NULL
	  AND reviewer.github_app_actor_id IS NOT NULL
	ORDER BY reviewer.generation DESC, reviewer.created_at DESC
	LIMIT 1
), 0)
FROM agent_assignments AS source
WHERE source.id = $1 AND source.workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID, reviewerRoles).Scan(
		&reconciliation.Role, &reconciliation.ReviewerActorID,
	); err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("read mutation reconciliation Role actors: %w", err)
	}
	turn.AgentParticipantID = job.AgentAssignmentID
	turn.AgentAssignmentID = job.AgentAssignmentID
	reconciliation.Turn = turn.AgentTurn

	if turn.ChangeProposalID != "" {
		proposal := &AgentTurnChangeProposal{}
		err = tx.QueryRow(ctx, `
SELECT id::text, pull_request_id, pull_request_number, base_ref, base_sha, head_ref, head_sha
FROM change_proposals
WHERE id = $1 AND workflow_id = $2 AND repository_id = $3
  AND repository_owner = $4 AND repository_name = $5`,
			turn.ChangeProposalID, job.WorkflowID, reconciliation.Repository.ID,
			reconciliation.Repository.Owner, reconciliation.Repository.Name,
		).Scan(
			&proposal.ID, &proposal.PullRequestID, &proposal.PullRequestNumber,
			&proposal.BaseRef, &proposal.BaseSHA, &proposal.HeadRef, &proposal.HeadSHA,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentTurnMutationReconciliationContext{}, ErrAgentTurnRecoveryFenceLost
		}
		if err != nil {
			return AgentTurnMutationReconciliationContext{}, fmt.Errorf("read Turn-bound mutation reconciliation Change Proposal: %w", err)
		}
		reconciliation.ChangeProposal = proposal
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("commit Agent Turn mutation reconciliation context: %w", err)
	}
	return reconciliation, nil
}

// ListAgentTurnMutationsForReconciliation returns mutations that require fresh
// reconciliation in invocation order under the live recovery-job fence after stale Runtime Process stop is durable.
func (store *Store) ListAgentTurnMutationsForReconciliation(ctx context.Context, lease JobLease) ([]MutationReservation, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin Agent Turn mutation reconciliation list: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, ReconcileAgentTurnMutationsJobKind)
	if err != nil {
		return nil, err
	}
	if err := requireRecoveredRuntimeStopped(ctx, tx, job); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, mutationSelect+`
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state IN ('UNKNOWN', 'RECONCILING', 'SUCCEEDED')
ORDER BY invocation_number`, job.AgentTurnID, job.ExecutionEpoch)
	if err != nil {
		return nil, fmt.Errorf("list Agent Turn mutations for reconciliation: %w", err)
	}
	defer rows.Close()
	var mutations []MutationReservation
	for rows.Next() {
		mutation, err := scanMutation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Agent Turn mutation for reconciliation: %w", err)
		}
		mutations = append(mutations, mutation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Agent Turn mutations for reconciliation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit Agent Turn mutation reconciliation list: %w", err)
	}
	return mutations, nil
}

// AcknowledgeAgentTurnMutationReconciliationFailure retries an inconclusive
// reconciliation within the job budget. Final exhaustion atomically records
// unknowable mutation outcomes, completes the recovery barrier as escalated,
// and applies the reducer-owned Human Handoff transition.
func (store *Store) AcknowledgeAgentTurnMutationReconciliationFailure(ctx context.Context, lease JobLease, cause error, retryDelay time.Duration) (AgentTurnMutationReconciliationAcknowledgement, error) {
	if cause == nil || strings.TrimSpace(cause.Error()) == "" {
		return AgentTurnMutationReconciliationAcknowledgement{}, errors.New("acknowledge Agent Turn mutation reconciliation failure: cause is empty")
	}
	if retryDelay < 0 || retryDelay > maximumJobDelay {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("acknowledge Agent Turn mutation reconciliation failure: retry delay must be between zero and %s", maximumJobDelay)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("begin Agent Turn mutation reconciliation failure acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, ReconcileAgentTurnMutationsJobKind)
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	if err := requireRecoveredRuntimeStopped(ctx, tx, job); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	var candidates, unresolved int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE state IN ('UNKNOWN', 'RECONCILING', 'SUCCEEDED')),
       count(*) FILTER (WHERE state IN ('UNKNOWN', 'RECONCILING'))
FROM tool_invocations
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
	`, job.AgentTurnID, job.ExecutionEpoch).Scan(&candidates, &unresolved); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("count unresolved recovered mutations: %w", err)
	}
	if candidates == 0 {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryUnsettled
	}

	acknowledgement := AgentTurnMutationReconciliationAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, WorkflowAttemptID: job.WorkflowAttemptID,
		AgentTurnID: job.AgentTurnID, Attempt: job.AttemptCount,
		RetryScheduled:          job.AttemptCount < job.MaxAttempts,
		UnresolvedMutationCount: candidates,
	}
	if acknowledgement.RetryScheduled {
		if err := failAgentTurnMutationReconciliationAttemptTx(ctx, tx, job, cause.Error(), retryDelay); err != nil {
			return AgentTurnMutationReconciliationAcknowledgement{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("commit Agent Turn mutation reconciliation retry: %w", err)
		}
		return acknowledgement, nil
	}

	diagnostic := fmt.Sprintf("outcome unknowable; escalated after %d reconciliation attempts: %s", job.AttemptCount, cause.Error())
	result, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = 'FAILED', result = NULL, last_error = $3,
    updated_at = clock_timestamp(), finished_at = clock_timestamp(),
    duration_ms = CASE WHEN started_at IS NULL THEN NULL
        ELSE GREATEST(0, (EXTRACT(EPOCH FROM (clock_timestamp() - started_at)) * 1000)::bigint) END
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state IN ('UNKNOWN', 'RECONCILING')`, job.AgentTurnID, job.ExecutionEpoch, diagnostic)
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("escalate unresolved recovered mutations: %w", err)
	}
	if int(result.RowsAffected()) != unresolved {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
	}

	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("rehydrate mutation reconciliation Workflow: %w", err)
	}
	var role workflow.Role
	var continuation string
	var runtimeOOMKilled bool
	if err := tx.QueryRow(ctx, `
SELECT assignment.role, turn.recovery_continuation, turn.runtime_oom_container_id IS NOT NULL
FROM agent_assignments AS assignment
JOIN agent_turns AS turn ON turn.id = $3 AND turn.execution_epoch = $4
WHERE assignment.id = $1 AND assignment.workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID,
		job.AgentTurnID, job.ExecutionEpoch).Scan(&role, &continuation, &runtimeOOMKilled); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
	}
	observedAt := job.CreatedAt
	if job.LeasedAt != nil {
		observedAt = *job.LeasedAt
	}
	workflowRevision := snapshot.Revision
	nextContinuation := continuation
	if continuation == recoveryContinuationPendingInfrastructure {
		stage, stageKnown := store.reducer.Definition().Stage(snapshot.CurrentAttempt.CurrentStage)
		definitionCompatible := store.reducer.DefinitionCompatible(snapshot) && stageKnown && stage.Role == role
		decision := store.reducer.DefinitionIncompatible(snapshot)
		nextContinuation = recoveryContinuationDefinitionHandoff
		if definitionCompatible {
			handoffDiagnostic := diagnostic
			if runtimeOOMKilled {
				handoffDiagnostic = fmt.Sprintf("outcome unknowable; escalated after %d reconciliation attempts. %s Reconciliation detail: %s",
					job.AttemptCount, RuntimeOOMDiagnostic, cause.Error())
			}
			decision = store.reducer.Reduce(snapshot, workflow.AgentTurnMutationReconciliationExhaustedEvent{
				EventMetadata: workflow.EventMetadata{
					ID: job.ID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem,
					ExpectedRevision: snapshot.Revision,
				},
				Role: role, Diagnostic: handoffDiagnostic,
			})
			nextContinuation = recoveryContinuationMutationHandoff
		}
		if err := validateWorkflowDecision(snapshot, decision); err != nil ||
			decision.Disposition != workflow.DispositionApplied ||
			definitionCompatible && decision.Reason != workflow.ReasonAgentTurnMutationReconciliationExhausted ||
			!definitionCompatible && decision.Reason != workflow.ReasonWorkflowDefinitionIncompatible {
			return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
		}
		if err := persistAppliedDecision(ctx, tx, "", job.WorkflowID, snapshot, decision, "agent-turn-mutation-reconciliation:"+job.ID); err != nil {
			return AgentTurnMutationReconciliationAcknowledgement{}, err
		}
		workflowRevision = decision.Snapshot.Revision
	} else if continuation != recoveryContinuationDefinitionHandoff {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
	}
	continuationResult, err := tx.Exec(ctx, `
UPDATE agent_turns
SET recovery_continuation = $3
WHERE id = $1 AND execution_epoch = $2
  AND recovery_continuation = $4
  AND recovery_settlement_id IS NULL`, job.AgentTurnID, job.ExecutionEpoch,
		nextContinuation, continuation)
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("record mutation reconciliation Workflow continuation: %w", err)
	}
	if continuationResult.RowsAffected() != 1 {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
	}
	jobResult, err := json.Marshal(map[string]any{
		"escalated": true, "diagnostic": diagnostic,
		"unresolved_mutation_count": candidates, "workflow_revision": workflowRevision,
	})
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	if err := completeRecoveryJobTx(ctx, tx, job, jobResult); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	_, err = settleAgentTurnRecoveryTx(ctx, tx, store.reducer, job)
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("commit Agent Turn mutation reconciliation escalation: %w", err)
	}
	acknowledgement.Escalated = true
	acknowledgement.Diagnostic = diagnostic
	acknowledgement.WorkflowRevision = workflowRevision
	return acknowledgement, nil
}

// CompleteAgentTurnMutationReconciliation acknowledges that every listed
// successful mutation was freshly found and every ambiguous mutation reached a fenced terminal state.
func (store *Store) CompleteAgentTurnMutationReconciliation(ctx context.Context, lease JobLease) (AgentTurnRecovery, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin Agent Turn mutation reconciliation completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, ReconcileAgentTurnMutationsJobKind)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if err := requireRecoveredRuntimeStopped(ctx, tx, job); err != nil {
		return AgentTurnRecovery{}, err
	}
	var unresolved bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM tool_invocations
    WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
      AND state IN ('UNKNOWN', 'RECONCILING')
)`, job.AgentTurnID, job.ExecutionEpoch).Scan(&unresolved); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("check completed recovered mutations: %w", err)
	}
	if unresolved {
		return AgentTurnRecovery{}, ErrAgentTurnRecoveryUnsettled
	}
	if err := completeRecoveryJobTx(ctx, tx, job, json.RawMessage(`{"mutations_reconciled":true}`)); err != nil {
		return AgentTurnRecovery{}, err
	}
	if _, err := settleAgentTurnRecoveryTx(ctx, tx, store.reducer, job); err != nil {
		return AgentTurnRecovery{}, err
	}
	recovery, err := readAgentTurnRecovery(ctx, tx, job.AgentTurnID, job.ExecutionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("read completed mutation recovery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("commit Agent Turn mutation reconciliation completion: %w", err)
	}
	return recovery, nil
}

func failAgentTurnMutationReconciliationAttemptTx(ctx context.Context, tx pgx.Tx, job Job, diagnostic string, retryDelay time.Duration) error {
	attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = TRUE, last_error = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, diagnostic)
	if err != nil || attemptResult.RowsAffected() != 1 {
		return ErrAgentTurnRecoveryFenceLost
	}
	jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'AVAILABLE', available_at = clock_timestamp() + $4 * interval '1 microsecond',
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(), completed_at = NULL,
    result = NULL, last_error = $5
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, retryDelay.Microseconds(), diagnostic)
	if err != nil || jobResult.RowsAffected() != 1 {
		return ErrAgentTurnRecoveryFenceLost
	}
	return nil
}

func requireRecoveredRuntimeStopped(ctx context.Context, tx pgx.Tx, job Job) error {
	var stopped bool
	if err := tx.QueryRow(ctx, `
SELECT turn.runtime_stopped_at IS NOT NULL AND stop.status = 'SUCCEEDED'
FROM agent_turns AS turn
JOIN jobs AS stop ON stop.id = turn.stop_runtime_job_id
WHERE turn.id = $1 AND turn.execution_epoch = $2`, job.AgentTurnID, job.ExecutionEpoch).Scan(&stopped); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentTurnRecoveryFenceLost
		}
		return fmt.Errorf("inspect recovered Runtime Process stop: %w", err)
	}
	if !stopped {
		return ErrAgentTurnRecoveryUnsettled
	}
	return nil
}
