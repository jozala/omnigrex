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

type WorkflowActionFailureAcknowledgement struct {
	JobID                   string
	WorkflowID              string
	RetryScheduled          bool
	EscalationScheduled     bool
	HandoffApplied          bool
	WorkflowRevision        uint64
	DeferredEventsCompleted uint32
}

const EscalateWorkflowActionFailureJobKind = "ESCALATE_WORKFLOW_ACTION_FAILURE"

// WorkflowActionFailureEscalation is the terminal outcome of one barrier-safe escalation authority.
type WorkflowActionFailureEscalation struct {
	JobID                   string
	SourceJobID             string
	WorkflowID              string
	HandoffApplied          bool
	WorkflowRevision        uint64
	DeferredEventsCompleted uint32
}

type workflowActionFailurePayload struct {
	SourceJobID string        `json:"source_job_id"`
	SourceKind  string        `json:"source_kind"`
	ResumeRole  workflow.Role `json:"resume_role,omitempty"`
	Diagnostic  string        `json:"diagnostic"`
	ObservedAt  time.Time     `json:"observed_at"`
}

func failWorkflowActionJobTx(ctx context.Context, tx pgx.Tx, job Job, diagnostic string, retryable, retryScheduled bool, retryDelay time.Duration) error {
	attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = $4, last_error = $5
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, retryable, diagnostic)
	if err != nil || attemptResult.RowsAffected() != 1 {
		return ErrJobLeaseLost
	}
	jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = CASE WHEN $4 THEN 'AVAILABLE' ELSE 'FAILED' END,
    available_at = CASE WHEN $4 THEN clock_timestamp() + $5 * interval '1 microsecond' ELSE available_at END,
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = CASE WHEN $4 THEN NULL ELSE clock_timestamp() END,
    result = NULL, last_error = $6
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, retryScheduled, retryDelay.Microseconds(), diagnostic)
	if err != nil || jobResult.RowsAffected() != 1 {
		return ErrJobLeaseLost
	}
	return nil
}

func applyWorkflowActionExhaustionTx(ctx context.Context, tx pgx.Tx, job Job, resumeRole workflow.Role, diagnostic string, observedAt time.Time) (workflow.Decision, error) {
	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return workflow.Decision{}, fmt.Errorf("rehydrate exhausted Workflow action: %w", err)
	}
	if snapshot.State != workflow.StateDeveloping && snapshot.State != workflow.StateReviewing &&
		snapshot.State != workflow.StatePRReady && snapshot.State != workflow.StateNeedsHuman {
		return workflow.Decision{Snapshot: snapshot, Disposition: workflow.DispositionUnrelated, Reason: workflow.ReasonWorkflowActionExhausted}, nil
	}
	decision := workflow.Reduce(snapshot, workflow.WorkflowActionExhaustedEvent{
		EventMetadata: workflow.EventMetadata{
			ID: job.ID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem,
			ExpectedRevision: snapshot.Revision,
		},
		ResumeRole: resumeRole,
		Diagnostic: diagnostic,
	})
	if err := validateWorkflowDecision(snapshot, decision); err != nil {
		return workflow.Decision{}, err
	}
	if decision.Disposition == workflow.DispositionDuplicate {
		return decision, nil
	}
	if decision.Disposition != workflow.DispositionApplied || decision.Reason != workflow.ReasonWorkflowActionExhausted {
		return workflow.Decision{}, ErrWorkflowDecisionInvalid
	}
	deliveryID, settlementID, err := workflowJobActionProvenance(ctx, tx, job.ID)
	if err != nil {
		return workflow.Decision{}, err
	}
	namespace := "workflow-action:" + strings.ToLower(job.Kind) + ":" + job.ID
	if err := persistAppliedDecisionWithProvenance(ctx, tx, deliveryID, settlementID, job.WorkflowID, snapshot, decision, namespace); err != nil {
		return workflow.Decision{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET status = 'WAITING_FOR_HUMAN', completed_at = NULL, retention_until = NULL,
    updated_at = clock_timestamp()
WHERE workflow_id = $1 AND status IN ('ACTIVE', 'COMPLETED', 'WAITING_FOR_HUMAN')
  AND state_deleted_at IS NULL`, job.WorkflowID); err != nil {
		return workflow.Decision{}, fmt.Errorf("mark exhausted Workflow action Assignments waiting for Human Handoff: %w", err)
	}
	return decision, nil
}

func workflowJobActionProvenance(ctx context.Context, tx pgx.Tx, jobID string) (string, string, error) {
	var deliveryID, settlementID string
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(normalized_event_id::text, ''), COALESCE(agent_turn_settlement_id::text, '')
FROM jobs WHERE id = $1`, jobID).Scan(&deliveryID, &settlementID); err != nil {
		return "", "", err
	}
	if deliveryID != "" && settlementID != "" {
		return "", "", ErrWorkflowDecisionInvalid
	}
	return deliveryID, settlementID, nil
}

func observedJobTime(job Job) time.Time {
	if job.LeasedAt != nil {
		return *job.LeasedAt
	}
	return job.CreatedAt
}

func requestWorkflowActionFailureEscalationTx(ctx context.Context, tx pgx.Tx, job Job, resumeRole workflow.Role, diagnostic string) (bool, error) {
	observedAt := observedJobTime(job)
	var state workflow.State
	if err := tx.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1 FOR UPDATE`, job.WorkflowID).Scan(&state); err != nil {
		return false, fmt.Errorf("lock Workflow for action failure: %w", err)
	}
	shouldEscalate := job.Kind == ReconcilePendingEventsJobKind || state == workflow.StateDeveloping ||
		state == workflow.StateReviewing || state == workflow.StatePRReady
	if !shouldEscalate {
		_, err := tx.Exec(ctx, `
INSERT INTO workflow_action_failures (
    source_job_id, workflow_id, source_kind, diagnostic, resume_role, observed_at, status, resolved_at
)
VALUES ($1, $2, $3, $4, $5, $6, 'TERMINAL', clock_timestamp())
ON CONFLICT (source_job_id) DO NOTHING`, job.ID, job.WorkflowID, job.Kind, diagnostic,
			nullableString(string(resumeRole)), observedAt)
		return false, err
	}
	payload, err := json.Marshal(workflowActionFailurePayload{
		SourceJobID: job.ID, SourceKind: job.Kind, ResumeRole: resumeRole,
		Diagnostic: diagnostic, ObservedAt: observedAt,
	})
	if err != nil {
		return false, fmt.Errorf("encode Workflow action failure escalation: %w", err)
	}
	escalationID, err := randomUUID()
	if err != nil {
		return false, err
	}
	idempotencyKey := fmt.Sprintf("workflow:%s:source-job:%s:escalate-action-failure", job.WorkflowID, job.ID)
	if _, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', 100, clock_timestamp(), 1, $5, $6, $7)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`, escalationID,
		WorkflowActionQueue, EscalateWorkflowActionFailureJobKind, payload, idempotencyKey,
		job.WorkflowID, nullableString(job.WorkflowAttemptID)); err != nil {
		return false, fmt.Errorf("enqueue Workflow action failure escalation: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT id::text FROM jobs WHERE idempotency_key = $1`, idempotencyKey).Scan(&escalationID); err != nil {
		return false, fmt.Errorf("resolve Workflow action failure escalation: %w", err)
	}
	result, err := tx.Exec(ctx, `
INSERT INTO workflow_action_failures (
    source_job_id, workflow_id, escalation_job_id, source_kind, diagnostic, resume_role, observed_at, status
)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING')
ON CONFLICT (source_job_id) DO NOTHING`, job.ID, job.WorkflowID, escalationID, job.Kind,
		diagnostic, nullableString(string(resumeRole)), observedAt)
	if err != nil {
		return false, fmt.Errorf("record Workflow action failure escalation: %w", err)
	}
	if result.RowsAffected() == 0 {
		var existingID, existingDiagnostic string
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(escalation_job_id::text, ''), diagnostic
FROM workflow_action_failures WHERE source_job_id = $1`, job.ID).Scan(&existingID, &existingDiagnostic); err != nil || existingID != escalationID || existingDiagnostic != diagnostic {
			return false, ErrJobIdempotencyConflict
		}
	}
	return true, nil
}

// ApplyNextWorkflowActionFailureEscalation atomically claims and applies one escalation whose completion barriers are clear.
func (store *Store) ApplyNextWorkflowActionFailureEscalation(ctx context.Context, owner string, lease time.Duration) (*WorkflowActionFailureEscalation, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("apply Workflow action failure escalation: owner is empty")
	}
	if err := validatePositiveDuration("apply Workflow action failure escalation lease", lease); err != nil {
		return nil, err
	}
	token, err := randomUUID()
	if err != nil {
		return nil, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin Workflow action failure escalation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var escalationID string
	err = tx.QueryRow(ctx, `
SELECT candidate.id::text
FROM jobs AS candidate
JOIN workflow_action_failures AS failure ON failure.escalation_job_id = candidate.id
JOIN workflows AS workflow ON workflow.id = candidate.workflow_id
WHERE candidate.queue = $1 AND candidate.kind = $2 AND candidate.status = 'AVAILABLE'
  AND candidate.available_at <= clock_timestamp() AND candidate.attempt_count < candidate.max_attempts
  AND failure.status = 'PENDING'
  AND NOT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = candidate.workflow_id AND active)
  AND NOT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = candidate.workflow_id
      AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL)
  AND NOT EXISTS (
      SELECT 1 FROM tool_invocations AS invocation
      JOIN agent_turns AS turn ON turn.id = invocation.agent_turn_id
      WHERE turn.workflow_id = candidate.workflow_id AND invocation.kind = 'MUTATION'
        AND invocation.state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
  )
  AND NOT EXISTS (SELECT 1 FROM jobs AS authority
      WHERE authority.workflow_id = candidate.workflow_id
        AND authority.id <> failure.source_job_id
        AND authority.kind IN ('PREPARE_AGENT_TURN', 'RECONCILE_PENDING_EVENTS')
        AND authority.status IN ('AVAILABLE', 'LEASED'))
ORDER BY candidate.priority DESC, candidate.available_at, candidate.id
FOR UPDATE OF candidate, workflow SKIP LOCKED
LIMIT 1`, WorkflowActionQueue, EscalateWorkflowActionFailureJobKind).Scan(&escalationID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty Workflow action failure escalation: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select Workflow action failure escalation: %w", err)
	}
	leased, err := leaseJobTx(ctx, tx, escalationID, owner, token, lease)
	if err != nil {
		return nil, err
	}
	var payload workflowActionFailurePayload
	if err := json.Unmarshal(leased.Payload, &payload); err != nil || payload.SourceJobID == "" || payload.Diagnostic == "" || payload.ObservedAt.IsZero() {
		return nil, ErrWorkflowDecisionInvalid
	}
	var sourceKind, diagnostic string
	var resumeRole workflow.Role
	var observedAt time.Time
	if err := tx.QueryRow(ctx, `
SELECT source_kind, diagnostic, COALESCE(resume_role, ''), observed_at
FROM workflow_action_failures
WHERE escalation_job_id = $1 AND source_job_id = $2 AND workflow_id = $3 AND status = 'PENDING'
FOR UPDATE`, leased.ID, payload.SourceJobID, leased.WorkflowID).Scan(&sourceKind, &diagnostic, &resumeRole, &observedAt); err != nil ||
		sourceKind != payload.SourceKind || diagnostic != payload.Diagnostic || resumeRole != payload.ResumeRole || !observedAt.Equal(payload.ObservedAt) {
		return nil, ErrWorkflowDecisionInvalid
	}
	blocked, err := workflowActionEscalationBlockedTx(ctx, tx, leased.WorkflowID, payload.SourceJobID)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, nil
	}
	source, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 AND workflow_id = $2 FOR UPDATE`, payload.SourceJobID, leased.WorkflowID))
	if err != nil || source.Status != JobFailed || source.Kind != sourceKind {
		return nil, ErrWorkflowDecisionInvalid
	}
	decision, err := applyWorkflowActionExhaustionTx(ctx, tx, source, resumeRole, diagnostic, observedAt)
	if err != nil {
		return nil, err
	}
	completed, err := completeDeferredEventsAfterReconciliationFailureTx(ctx, tx, source, decision.Snapshot.Revision)
	if err != nil {
		return nil, err
	}
	status := "TERMINAL"
	if decision.Disposition == workflow.DispositionApplied {
		status = "APPLIED"
	}
	result, err := json.Marshal(map[string]any{
		"source_job_id": payload.SourceJobID, "handoff_applied": status == "APPLIED",
		"workflow_revision": decision.Snapshot.Revision, "deferred_events_completed": completed,
	})
	if err != nil {
		return nil, err
	}
	if err := completeAcknowledgementJobTx(ctx, tx, leased.Job, result, ErrJobLeaseLost); err != nil {
		return nil, err
	}
	updated, err := tx.Exec(ctx, `
UPDATE workflow_action_failures SET status = $2, resolved_at = clock_timestamp()
WHERE escalation_job_id = $1 AND status = 'PENDING'`, leased.ID, status)
	if err != nil || updated.RowsAffected() != 1 {
		return nil, ErrWorkflowDecisionInvalid
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit Workflow action failure escalation: %w", err)
	}
	return &WorkflowActionFailureEscalation{
		JobID: leased.ID, SourceJobID: source.ID, WorkflowID: source.WorkflowID,
		HandoffApplied: status == "APPLIED", WorkflowRevision: decision.Snapshot.Revision,
		DeferredEventsCompleted: completed,
	}, nil
}

func workflowActionEscalationBlockedTx(ctx context.Context, tx pgx.Tx, workflowID, sourceJobID string) (bool, error) {
	var blocked bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active)
    OR EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1
        AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL)
    OR EXISTS (
        SELECT 1 FROM tool_invocations AS invocation
        JOIN agent_turns AS turn ON turn.id = invocation.agent_turn_id
        WHERE turn.workflow_id = $1 AND invocation.kind = 'MUTATION'
          AND invocation.state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
    )
    OR EXISTS (SELECT 1 FROM jobs WHERE workflow_id = $1 AND id <> $2
        AND kind IN ('PREPARE_AGENT_TURN', 'RECONCILE_PENDING_EVENTS')
        AND status IN ('AVAILABLE', 'LEASED'))`, workflowID, sourceJobID).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("inspect Workflow action escalation barriers: %w", err)
	}
	return blocked, nil
}

func completeDeferredEventsAfterReconciliationFailureTx(ctx context.Context, tx pgx.Tx, job Job, revision uint64) (uint32, error) {
	result, err := tx.Exec(ctx, `
UPDATE normalized_events AS event
SET status = 'COMPLETED', disposition = $2, reason = $3, applied_revision = $4,
    deferred_for_turn_id = NULL, processed_at = clock_timestamp()
FROM job_normalized_events AS link
WHERE link.job_id = $1 AND event.delivery_id = link.normalized_event_id
  AND event.workflow_id = $5 AND event.status = 'DEFERRED'`, job.ID,
		workflow.DispositionReconciliationFailed, workflow.ReasonWorkflowActionExhausted,
		int64(revision), job.WorkflowID)
	if err != nil {
		return 0, fmt.Errorf("terminally dispose deferred normalized events: %w", err)
	}
	if result.RowsAffected() > int64(^uint32(0)) {
		return 0, errors.New("terminally disposed deferred event count overflows")
	}
	return uint32(result.RowsAffected()), nil
}

func validateWorkflowActionFailure(cause error, retryDelay time.Duration) error {
	if cause == nil || strings.TrimSpace(cause.Error()) == "" {
		return errors.New("Workflow action failure cause is empty")
	}
	if retryDelay < 0 || retryDelay > maximumJobDelay {
		return fmt.Errorf("Workflow action failure retry delay must be between zero and %s", maximumJobDelay)
	}
	return nil
}

func isExhaustionAwareWorkflowAction(kind string) bool {
	return kind == ReconcilePendingEventsJobKind || kind == ReconcileGitHubLabelsJobKind || kind == PublishHumanHandoffJobKind
}

func exhaustExpiredWorkflowActionTx(ctx context.Context, tx pgx.Tx, job Job) error {
	role := workflow.Role("")
	if job.Kind == ReconcilePendingEventsJobKind {
		var payload pendingEventReconciliationPayload
		_ = json.Unmarshal(job.Payload, &payload)
		role = payload.FallbackRole
		if role != workflow.RoleDeveloper && role != workflow.RoleReviewer {
			if err := tx.QueryRow(ctx, `SELECT role FROM agent_assignments WHERE id = $1 AND workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(&role); err != nil {
				return ErrPendingEventReconciliationFenceLost
			}
		}
	}
	diagnostic := "durable Workflow action lease expired on its final attempt"
	if job.Kind == PublishHumanHandoffJobKind {
		var cleanupPending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
    SELECT 1 FROM workflow_github_effect_cleanups WHERE job_id = $1 AND status = 'PENDING'
)`, job.ID).Scan(&cleanupPending); err != nil {
			return err
		}
		if cleanupPending {
			if err := exhaustWorkflowGitHubEffectCleanupTx(ctx, tx, job, diagnostic); err != nil {
				return err
			}
		}
	}
	_, err := requestWorkflowActionFailureEscalationTx(ctx, tx, job, role, diagnostic)
	return err
}
