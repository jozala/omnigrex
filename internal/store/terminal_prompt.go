package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CloseMutationAdmissionWithPromptEvidence atomically fences new MCP mutations
// and records only the eligible or ineligible ACP ending before Runtime Process
// cleanup. A repeated identical call is safe after an ambiguous commit.
func (store *Store) CloseMutationAdmissionWithPromptEvidence(ctx context.Context, lease AgentTurnLease, stopReason, errorClass string) error {
	if !validAgentTurnPromptEvidence(stopReason, errorClass) {
		return ErrTerminalCorroborationConflict
	}
	return store.withLockedAgentTurnLease(ctx, lease, "close mutation admission with ACP ending", func(tx pgx.Tx, turn lockedTurn) error {
		return closeLiveMutationAdmissionTx(ctx, tx, turn, true, stopReason, errorClass)
	})
}

func closeLiveMutationAdmissionTx(ctx context.Context, tx pgx.Tx, turn lockedTurn, recordPrompt bool, stopReason, errorClass string) error {
	if turn.Status != AgentTurnRunning && turn.Status != AgentTurnSettling && turn.Status != AgentTurnReconciling {
		return ErrAgentTurnFenceLost
	}
	if recordPrompt {
		var recordedStop, recordedError string
		var recorded bool
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(prompt_stop_reason, ''), COALESCE(prompt_error_class, ''),
       prompt_recorded_at IS NOT NULL FROM agent_turns WHERE id = $1`, turn.ID).Scan(
			&recordedStop, &recordedError, &recorded); err != nil {
			return fmt.Errorf("inspect ACP prompt ending: %w", err)
		}
		if recorded && (recordedStop != stopReason || recordedError != errorClass) {
			return ErrTerminalCorroborationConflict
		}
		if !recorded && !turn.MutationAdmissionOpen {
			return ErrAgentTurnFenceLost
		}
	}
	var reconciliation bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM tool_invocations WHERE agent_turn_id = $1 AND execution_epoch = $2
AND kind = 'MUTATION' AND state IN ('UNKNOWN', 'RECONCILING'))`, turn.ID, turn.ExecutionEpoch).Scan(&reconciliation); err != nil {
		return err
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_turns SET mutation_admission_open = FALSE,
    mutation_admission_closed_at = COALESCE(mutation_admission_closed_at, clock_timestamp()),
    status = CASE WHEN $2 THEN 'RECONCILING' ELSE 'SETTLING' END,
    prompt_stop_reason = CASE WHEN $3 THEN COALESCE(prompt_stop_reason, NULLIF($4, '')) ELSE prompt_stop_reason END,
    prompt_error_class = CASE WHEN $3 THEN COALESCE(prompt_error_class, NULLIF($5, '')) ELSE prompt_error_class END,
    prompt_recorded_at = CASE WHEN $3 THEN COALESCE(prompt_recorded_at, clock_timestamp()) ELSE prompt_recorded_at END
WHERE id = $1`, turn.ID, reconciliation, recordPrompt, stopReason, errorClass)
	if err != nil {
		return err
	}
	if updated.RowsAffected() != 1 {
		return ErrAgentTurnFenceLost
	}
	return nil
}

func validAgentTurnPromptEvidence(stopReason, errorClass string) bool {
	if (stopReason == "") == (errorClass == "") {
		return false
	}
	if errorClass != "" {
		switch errorClass {
		case "FAILURE", "DEADLINE", "CANCELLATION", "INVALID_RESPONSE":
			return true
		default:
			return false
		}
	}
	switch stopReason {
	case "end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled":
		return true
	default:
		return false
	}
}
