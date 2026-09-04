package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	AssignmentID string
	Role         workflow.Role
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
SELECT role FROM agent_assignments
WHERE id = $1 AND workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(&cleanup.Role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentTurnRuntimeCleanupContext{}, ErrAgentTurnRecoveryFenceLost
		}
		return AgentTurnRuntimeCleanupContext{}, fmt.Errorf("read Agent Turn runtime cleanup Assignment: %w", err)
	}
	if cleanup.Role != workflow.RoleDeveloper && cleanup.Role != workflow.RoleReviewer {
		return AgentTurnRuntimeCleanupContext{}, ErrAgentTurnRecoveryFenceLost
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRuntimeCleanupContext{}, fmt.Errorf("commit Agent Turn runtime cleanup context: %w", err)
	}
	return cleanup, nil
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
	if err := tx.QueryRow(ctx, `
SELECT source.role, COALESCE(reviewer.github_app_actor_id, 0)
FROM agent_assignments AS source
LEFT JOIN agent_assignments AS reviewer
  ON reviewer.workflow_id = source.workflow_id
 AND reviewer.role = 'REVIEWER'
 AND reviewer.status <> 'SUPERSEDED'
 AND reviewer.state_deleted_at IS NULL
WHERE source.id = $1 AND source.workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(
		&reconciliation.Role, &reconciliation.ReviewerActorID,
	); err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("read mutation reconciliation Role actors: %w", err)
	}
	if reconciliation.Role != workflow.RoleDeveloper && reconciliation.Role != workflow.RoleReviewer {
		return AgentTurnMutationReconciliationContext{}, ErrAgentTurnRecoveryFenceLost
	}
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

// ListAgentTurnMutationsForReconciliation returns UNKNOWN and RECONCILING
// mutations in invocation order under the live recovery-job fence.
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
	rows, err := tx.Query(ctx, mutationSelect+`
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state IN ('UNKNOWN', 'RECONCILING')
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
	var unresolved int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM tool_invocations
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state IN ('UNKNOWN', 'RECONCILING')`, job.AgentTurnID, job.ExecutionEpoch).Scan(&unresolved); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("count unresolved recovered mutations: %w", err)
	}
	if unresolved == 0 {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryUnsettled
	}

	acknowledgement := AgentTurnMutationReconciliationAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, WorkflowAttemptID: job.WorkflowAttemptID,
		AgentTurnID: job.AgentTurnID, Attempt: job.AttemptCount,
		RetryScheduled:          job.AttemptCount < job.MaxAttempts,
		UnresolvedMutationCount: unresolved,
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
	if err := tx.QueryRow(ctx, `SELECT role FROM agent_assignments WHERE id = $1 AND workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(&role); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
	}
	observedAt := job.CreatedAt
	if job.LeasedAt != nil {
		observedAt = *job.LeasedAt
	}
	decision := workflow.Reduce(snapshot, workflow.AgentTurnMutationReconciliationExhaustedEvent{
		EventMetadata: workflow.EventMetadata{
			ID: job.ID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem,
			ExpectedRevision: snapshot.Revision,
		},
		Role: role, Diagnostic: diagnostic,
	})
	if err := validateWorkflowDecision(snapshot, decision); err != nil ||
		decision.Disposition != workflow.DispositionApplied ||
		decision.Reason != workflow.ReasonAgentTurnMutationReconciliationExhausted {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryFenceLost
	}
	if err := persistAppliedDecision(ctx, tx, "", job.WorkflowID, snapshot, decision, "agent-turn-mutation-reconciliation:"+job.ID); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET status = 'WAITING_FOR_HUMAN', completed_at = NULL, retention_until = NULL,
    updated_at = clock_timestamp()
WHERE workflow_id = $1 AND status IN ('ACTIVE', 'COMPLETED', 'WAITING_FOR_HUMAN')
  AND state_deleted_at IS NULL`, job.WorkflowID); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("mark mutation reconciliation Assignments waiting for Human Handoff: %w", err)
	}
	jobResult, err := json.Marshal(map[string]any{
		"escalated": true, "diagnostic": diagnostic,
		"unresolved_mutation_count": unresolved, "workflow_revision": decision.Snapshot.Revision,
	})
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	if err := completeRecoveryJobTx(ctx, tx, job, jobResult); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	settled, err := settleAgentTurnRecoveryTx(ctx, tx, job.AgentTurnID, job.ExecutionEpoch)
	if err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, err
	}
	if !settled {
		return AgentTurnMutationReconciliationAcknowledgement{}, ErrAgentTurnRecoveryUnsettled
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnMutationReconciliationAcknowledgement{}, fmt.Errorf("commit Agent Turn mutation reconciliation escalation: %w", err)
	}
	acknowledgement.Escalated = true
	acknowledgement.Diagnostic = diagnostic
	acknowledgement.WorkflowRevision = decision.Snapshot.Revision
	return acknowledgement, nil
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
