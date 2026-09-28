package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

// findTerminalCorroborationSourceTx recognizes exactly one successful terminal
// intent in the complete Turn ledger, including ancestor mutation replays.
// An empty requestedID discovers the sole source for stopped-Turn recovery.
func findTerminalCorroborationSourceTx(ctx context.Context, tx pgx.Tx, turnID string, epoch int64,
	role workflow.Role, requestedID string) (string, bool, error) {
	rows, err := tx.Query(ctx, mutationLedgerSelect, turnID, epoch)
	if err != nil {
		return "", false, err
	}
	var sourceID, sourceTool string
	count := 0
	for rows.Next() {
		mutation, err := scanMutation(rows)
		if err != nil {
			rows.Close()
			return "", false, err
		}
		if mutation.State != MutationSucceeded {
			continue
		}
		switch mutation.ToolName {
		case "request_review", "submit_review", "report_blocked", "confirm_prior_terminal_intent":
			count++
			sourceID, sourceTool = mutation.ID, mutation.ToolName
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", false, err
	}
	if count != 1 || requestedID != "" && requestedID != sourceID ||
		role == workflow.RoleDeveloper && sourceTool != "request_review" && sourceTool != "confirm_prior_terminal_intent" ||
		role == workflow.RoleReviewer && sourceTool != "submit_review" && sourceTool != "confirm_prior_terminal_intent" ||
		role != workflow.RoleDeveloper && role != workflow.RoleReviewer {
		return "", false, nil
	}
	if sourceTool == "confirm_prior_terminal_intent" {
		var evidence struct {
			SourceTool         string `json:"source_tool"`
			SourceInvocationID string `json:"source_invocation_id"`
		}
		var result []byte
		err := tx.QueryRow(ctx, `SELECT result FROM tool_invocations WHERE id = $1`, sourceID).Scan(&result)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		if json.Unmarshal(result, &evidence) != nil || evidence.SourceInvocationID == "" ||
			role == workflow.RoleDeveloper && evidence.SourceTool != "request_review" ||
			role == workflow.RoleReviewer && evidence.SourceTool != "submit_review" {
			return "", false, nil
		}
	}
	return sourceID, true, nil
}

func createTerminalCorroborationCheckpointTx(ctx context.Context, tx pgx.Tx, executionJob Job, turn lockedTurn,
	sourceID string, promptOutcome json.RawMessage, promptError, failureCode, ownerID string, ownerHash []byte) (TerminalCorroboration, error) {
	if !validUUID(sourceID) || !validCorroborationPrompt(promptOutcome, promptError) ||
		!validCorroborationFailureCode(failureCode) || ownerID == "" || len(ownerHash) != sha256.Size {
		return TerminalCorroboration{}, ErrTerminalCorroborationConflict
	}
	verificationJobID, err := randomUUID()
	if err != nil {
		return TerminalCorroboration{}, err
	}
	checkpoint := TerminalCorroboration{
		TurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch, WorkflowID: executionJob.WorkflowID,
		WorkflowAttemptID: turn.WorkflowAttemptID, SourceInvocationID: sourceID,
		VerificationJobID: verificationJobID, ExecutionJobID: executionJob.ID,
		PromptOutcome: promptOutcome, PromptError: promptError, LastFailureCode: failureCode,
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO jobs (id, queue, kind, payload, status, priority, max_attempts,
                  idempotency_key, workflow_id, workflow_attempt_id,
                  agent_assignment_id, agent_session_id, agent_turn_id, execution_epoch)
VALUES ($1, 'agent-turn-recovery', $2, '{}'::jsonb, 'AVAILABLE', 80, 1000000,
        $3, $4, $5, $6, $7, $8, $9)
RETURNING id::text`, verificationJobID, VerifyTerminalIntentJobKind,
		"terminal-corroboration:"+turn.ID+":"+fmt.Sprint(turn.ExecutionEpoch),
		executionJob.WorkflowID, turn.WorkflowAttemptID, executionJob.AgentAssignmentID,
		turn.AgentSessionID, turn.ID, turn.ExecutionEpoch).Scan(&checkpoint.VerificationJobID); err != nil {
		return TerminalCorroboration{}, fmt.Errorf("enqueue terminal corroboration: %w", err)
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO agent_turn_corroborations (agent_turn_id, execution_epoch, workflow_id,
    workflow_attempt_id, source_invocation_id, execution_job_id,
    execution_owner_id, execution_owner_token_sha256, prompt_outcome, prompt_error,
    state, last_failure_code, verification_job_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'PENDING', $11, $12)
RETURNING pending_since`, turn.ID, turn.ExecutionEpoch, executionJob.WorkflowID,
		turn.WorkflowAttemptID, sourceID, executionJob.ID, ownerID, ownerHash,
		nullableJSON(promptOutcome), nullableString(promptError), failureCode, verificationJobID).Scan(&checkpoint.PendingSince); err != nil {
		return TerminalCorroboration{}, fmt.Errorf("persist terminal corroboration checkpoint: %w", err)
	}
	return checkpoint, nil
}

// beginRecoveredTerminalCorroborationTx runs only after the stop and mutation
// barriers have succeeded. A recorded eligible prompt ending preserves the
// original Turn's terminal intent instead of scheduling a fresh Agent prompt.
func beginRecoveredTerminalCorroborationTx(ctx context.Context, tx pgx.Tx, executionJob Job,
	turn lockedTurn, role workflow.Role, originalOwnerID string, originalOwnerHash []byte) (bool, error) {
	var stopReason, errorClass string
	var recorded bool
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(prompt_stop_reason, ''), COALESCE(prompt_error_class, ''),
       prompt_recorded_at IS NOT NULL FROM agent_turns WHERE id = $1`, turn.ID).Scan(
		&stopReason, &errorClass, &recorded); err != nil {
		return false, err
	}
	if !recorded || (stopReason != "end_turn" && errorClass != "FAILURE" && errorClass != "DEADLINE") {
		return false, nil
	}
	sourceID, valid, err := findTerminalCorroborationSourceTx(ctx, tx, turn.ID, turn.ExecutionEpoch, role, "")
	if err != nil || !valid {
		return false, err
	}
	var outcome json.RawMessage
	if stopReason == "end_turn" {
		outcome = json.RawMessage(`{"stop_reason":"end_turn"}`)
	}
	if _, err := createTerminalCorroborationCheckpointTx(ctx, tx, executionJob, turn, sourceID,
		outcome, errorClass, "runtime_interrupted_after_prompt", originalOwnerID, originalOwnerHash); err != nil {
		return false, err
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_turns
SET status = 'CORROBORATING', active = TRUE, completed_at = NULL,
    recovery_settled_at = clock_timestamp(),
    recovery_continuation = 'TERMINAL_CORROBORATION_PENDING'
WHERE id = $1 AND execution_epoch = $2 AND recovery_started_at IS NOT NULL
  AND recovery_continuation = 'PENDING_INFRASTRUCTURE_FAILURE'
  AND runtime_stopped_at IS NOT NULL AND NOT mutation_admission_open
  AND NOT EXISTS (
      SELECT 1 FROM tool_invocations WHERE agent_turn_id = $1 AND execution_epoch = $2
        AND kind = 'MUTATION' AND state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
  )`, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		return false, err
	}
	if updated.RowsAffected() != 1 {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	return true, nil
}
