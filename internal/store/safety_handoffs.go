package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	closureSafetyHandoffReason   = "closure_cleanup_exhausted"
	retentionSafetyHandoffReason = "assignment_collection_exhausted"
	recoverySafetyHandoffReason  = "agent_turn_recovery_exhausted"
	irreversibleRetryDelay       = time.Second
)

const (
	closureStopSafetyDiagnostic       = "Agent Turn stop failed three times; safe cleanup retries will continue."
	closureSettlementSafetyDiagnostic = "Workflow closure settlement failed three times; safe retries will continue."
	retentionSafetyDiagnostic         = "Assignment collection failed three times; safe cleanup retries will continue."
	recoveryStopSafetyDiagnostic      = "Stale Runtime Process recovery failed three times; safe recovery retries will continue."
	recoveryMutationSafetyDiagnostic  = "Agent Turn mutation recovery expired three times; safe recovery retries will continue."
)

func enqueueSafetyHandoffTx(ctx context.Context, tx pgx.Tx, source Job, reason, diagnostic string) (bool, error) {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(diagnostic) == "" {
		return false, ErrWorkflowDecisionInvalid
	}
	var revision int64
	var pullRequestNumber int64
	if err := tx.QueryRow(ctx, `
SELECT workflow.state_revision, COALESCE(proposal.pull_request_number, 0)
FROM workflows AS workflow
LEFT JOIN change_proposals AS proposal
  ON proposal.workflow_id = workflow.id AND proposal.active
WHERE workflow.id = $1 FOR SHARE OF workflow`, source.WorkflowID).Scan(&revision, &pullRequestNumber); err != nil {
		return false, fmt.Errorf("read safety Human Handoff context: %w", err)
	}
	payload := map[string]any{
		"revision": revision, "reason": reason, "diagnostic": diagnostic,
		"safety_diagnostic": true,
	}
	if pullRequestNumber > 0 {
		payload["pull_request_number"] = pullRequestNumber
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	jobID, err := randomUUID()
	if err != nil {
		return false, err
	}
	idempotencyKey := fmt.Sprintf("workflow:%s:source-job:%s:safety-handoff", source.WorkflowID, source.ID)
	actionKey := "safety-handoff:" + source.ID
	result, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id,
    normalized_event_id, agent_turn_settlement_id, workflow_internal_event_id,
    action_key
)
SELECT $1, $2, $3, $4, 'AVAILABLE', 100, clock_timestamp(), 3,
       $5, source.workflow_id, source.workflow_attempt_id,
       source.normalized_event_id, source.agent_turn_settlement_id,
       source.workflow_internal_event_id,
       CASE WHEN ((source.normalized_event_id IS NOT NULL)::integer
                       + (source.agent_turn_settlement_id IS NOT NULL)::integer
                       + (source.workflow_internal_event_id IS NOT NULL)::integer) = 1
            THEN $6 ELSE NULL END
FROM jobs AS source
WHERE source.id = $7 AND source.workflow_id = $8
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
		jobID, WorkflowActionQueue, PublishHumanHandoffJobKind, encoded,
		idempotencyKey, actionKey, source.ID, source.WorkflowID)
	if err != nil {
		return false, fmt.Errorf("enqueue safety Human Handoff: %w", err)
	}
	if result.RowsAffected() == 1 {
		return true, nil
	}
	var matching bool
	if err := tx.QueryRow(ctx, `
SELECT workflow_id = $2 AND kind = $3 AND payload = $4::jsonb
FROM jobs WHERE idempotency_key = $1`, idempotencyKey, source.WorkflowID,
		PublishHumanHandoffJobKind, encoded).Scan(&matching); err != nil || !matching {
		return false, ErrJobIdempotencyConflict
	}
	return false, nil
}

func continueIrreversibleJobAfterExhaustionTx(ctx context.Context, tx pgx.Tx, job Job) (bool, bool, error) {
	var reason, diagnostic string
	switch job.Kind {
	case StopAgentTurnJobKind:
		reason, diagnostic = closureSafetyHandoffReason, closureStopSafetyDiagnostic
	case SettleClosureJobKind:
		reason, diagnostic = closureSafetyHandoffReason, closureSettlementSafetyDiagnostic
	case CollectAssignmentsJobKind:
		var collecting bool
		if err := tx.QueryRow(ctx, `
SELECT status = 'COLLECTING'
FROM assignment_retention_generations
WHERE collection_job_id = $1 FOR UPDATE`, job.ID).Scan(&collecting); err != nil {
			return false, false, fmt.Errorf("inspect exhausted Assignment collection: %w", err)
		}
		if !collecting {
			return false, false, nil
		}
		reason, diagnostic = retentionSafetyHandoffReason, retentionSafetyDiagnostic
	case StopStaleRuntimeJobKind, ReconcileAgentTurnMutationsJobKind:
		var workflowID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM workflows WHERE id = $1 FOR SHARE`, job.WorkflowID).Scan(&workflowID); err != nil || workflowID != job.WorkflowID {
			return false, false, ErrAgentTurnRecoveryFenceLost
		}
		var unsettled, registered bool
		if err := tx.QueryRow(ctx, `
SELECT recovery_settled_at IS NULL,
       CASE WHEN $3 = 'STOP_STALE_RUNTIME' THEN stop_runtime_job_id = $4
            ELSE reconcile_mutations_job_id = $4 END
FROM agent_turns
WHERE id = $1 AND execution_epoch = $2 AND recovery_started_at IS NOT NULL
FOR UPDATE`, job.AgentTurnID, job.ExecutionEpoch, job.Kind, job.ID).Scan(&unsettled, &registered); err != nil {
			return false, false, fmt.Errorf("inspect exhausted Agent Turn recovery: %w", err)
		}
		if !unsettled {
			return false, false, nil
		}
		if !registered {
			return false, false, ErrAgentTurnRecoveryFenceLost
		}
		reason = recoverySafetyHandoffReason
		if job.Kind == StopStaleRuntimeJobKind {
			diagnostic = recoveryStopSafetyDiagnostic
		} else {
			diagnostic = recoveryMutationSafetyDiagnostic
		}
	default:
		return false, false, nil
	}
	scheduled, err := enqueueSafetyHandoffTx(ctx, tx, job, reason, diagnostic)
	if err != nil {
		return false, false, err
	}
	result, err := tx.Exec(ctx, `
UPDATE jobs SET max_attempts = GREATEST(max_attempts, attempt_count + 1), updated_at = clock_timestamp()
WHERE id = $1 AND status = 'LEASED' AND lease_token = $2 AND attempt_count = $3`,
		job.ID, job.LeaseToken, job.AttemptCount)
	if err != nil || result.RowsAffected() != 1 {
		return false, false, ErrJobLeaseLost
	}
	return true, scheduled, nil
}
