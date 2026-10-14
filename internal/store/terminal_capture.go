package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// RecordRecoveredRuntimeNeverStarted persists an exact-resource observation under
// the stale-runtime recovery fence after creation dispatch has been revoked.
// Session binding may predate this execution; only current Turn activity denies
// the preparation-only cleanup proof. Removal must still precede recovery ack.
func (store *Store) RecordRecoveredRuntimeNeverStarted(ctx context.Context, lease JobLease, resourceRef string) error {
	if resourceRef == "" || len(resourceRef) > 128 || strings.TrimSpace(resourceRef) != resourceRef {
		return execution.ErrConflict
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin recovered never-started runtime observation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, turn, err := lockAgentTurnRecoveryJob(ctx, tx, lease, StopStaleRuntimeJobKind)
	if err != nil {
		return err
	}
	if turn.MutationAdmissionOpen {
		return execution.ErrConflict
	}
	var eligible bool
	if err := tx.QueryRow(ctx, `
SELECT session.human_prompt_token IS NULL
    AND turn.prompt_recorded_at IS NULL AND turn.next_invocation_number = 1
    AND NOT EXISTS (SELECT 1 FROM tool_invocations
                    WHERE agent_turn_id = turn.id AND execution_epoch = turn.execution_epoch)
    AND NOT EXISTS (SELECT 1 FROM tool_invocation_replays
                    WHERE agent_turn_id = turn.id AND execution_epoch = turn.execution_epoch)
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
WHERE turn.id = $1 AND turn.execution_epoch = $2 AND session.id = $3`,
		job.AgentTurnID, job.ExecutionEpoch, job.AgentSessionID).Scan(&eligible); err != nil {
		return fmt.Errorf("inspect recovered never-started runtime eligibility: %w", err)
	}
	if !eligible {
		return execution.ErrConflict
	}
	key := execution.Key{AssignmentID: job.AgentAssignmentID, AgentSessionID: job.AgentSessionID,
		AgentTurnID: job.AgentTurnID, ExecutionEpoch: job.ExecutionEpoch}
	if err := tx.QueryRow(ctx, `SELECT runtime_profile_name, runtime_profile_version FROM agent_assignments WHERE id = $1`,
		job.AgentAssignmentID).Scan(&key.RuntimeProfileName, &key.RuntimeProfileVersion); err != nil {
		return fmt.Errorf("read recovered never-started runtime identity: %w", err)
	}
	identity, err := json.Marshal(key)
	if err != nil {
		return err
	}
	updated, err := tx.Exec(ctx, `UPDATE execution_reservations SET preparation_only_cleanup = true
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND identity = $3::jsonb
  AND dispatch_phase = 'main' AND revoked AND resource_ref = $4 AND creation_settled`,
		key.AgentTurnID, key.ExecutionEpoch, identity, resourceRef)
	if err != nil {
		return fmt.Errorf("record recovered never-started runtime proof: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return execution.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit recovered never-started runtime proof: %w", err)
	}
	return nil
}

// RecordAgentTurnUnpromptedCleanup admits the Launcher's proof that this first
// Session never handed off prompt capability. It precedes removal of its known,
// stopped main resource and is durable across loss of the cleanup response.
func (store *Store) RecordAgentTurnUnpromptedCleanup(ctx context.Context, lease AgentTurnLease) error {
	return store.withLockedAgentTurnLeaseOptions(ctx, lease, "record unprompted runtime cleanup", true, true, func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning && turn.Status != AgentTurnSettling && turn.Status != AgentTurnReconciling || turn.MutationAdmissionOpen {
			return ErrAgentTurnFenceLost
		}
		var eligible bool
		if err := tx.QueryRow(ctx, `
SELECT session.status = 'CREATING' AND session.runtime_capabilities IS NULL
    AND session.activated_at IS NULL AND session.human_prompt_token IS NULL
    AND turn.prompt_recorded_at IS NULL AND turn.next_invocation_number = 1
    AND NOT EXISTS (SELECT 1 FROM tool_invocations
                    WHERE agent_turn_id = turn.id AND execution_epoch = turn.execution_epoch)
    AND NOT EXISTS (SELECT 1 FROM tool_invocation_replays
                    WHERE agent_turn_id = turn.id AND execution_epoch = turn.execution_epoch)
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
WHERE turn.id = $1 AND turn.execution_epoch = $2 AND session.id = $3`,
			turn.ID, turn.ExecutionEpoch, turn.AgentSessionID).Scan(&eligible); err != nil {
			return fmt.Errorf("inspect unprompted runtime cleanup eligibility: %w", err)
		}
		if !eligible {
			return errors.New("unprompted runtime cleanup conflicts with durable Session or Turn activity")
		}
		key := execution.Key{AssignmentID: turn.AgentAssignmentID, AgentSessionID: turn.AgentSessionID,
			AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch}
		if err := tx.QueryRow(ctx, `SELECT runtime_profile_name, runtime_profile_version FROM agent_assignments WHERE id = $1`,
			turn.AgentAssignmentID).Scan(&key.RuntimeProfileName, &key.RuntimeProfileVersion); err != nil {
			return fmt.Errorf("read unprompted runtime identity: %w", err)
		}
		identity, err := json.Marshal(key)
		if err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, `UPDATE execution_reservations SET preparation_only_cleanup = true
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND identity = $3::jsonb
  AND dispatch_phase = 'main' AND resource_ref IS NOT NULL AND creation_settled`, key.AgentTurnID, key.ExecutionEpoch, identity)
		if err != nil {
			return fmt.Errorf("record unprompted runtime cleanup proof: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return execution.ErrConflict
		}
		return nil
	})
}

// AgentTurnTerminalCaptureRequired fences further dispatch and exempts only a
// current execution with no main dispatch or authoritative no-effect creation,
// and no Turn activity. An existing Session binding belongs to earlier Turns too,
// so it cannot alone require capture.
func (store *Store) AgentTurnTerminalCaptureRequired(ctx context.Context, lease JobLease) (bool, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return true, fmt.Errorf("begin terminal capture requirement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, StopStaleRuntimeJobKind)
	if err != nil {
		return true, err
	}
	// The recovery fence denies lease-bound dispatch. Also revoke the ledger
	// under this transaction, serializing with first reservation and phase
	// changes so a no-dispatch exemption cannot race delayed external creation.
	key := execution.Key{AssignmentID: job.AgentAssignmentID, AgentSessionID: job.AgentSessionID,
		AgentTurnID: job.AgentTurnID, ExecutionEpoch: job.ExecutionEpoch}
	if err := tx.QueryRow(ctx, `SELECT runtime_profile_name, runtime_profile_version FROM agent_assignments WHERE id = $1`,
		job.AgentAssignmentID).Scan(&key.RuntimeProfileName, &key.RuntimeProfileVersion); err != nil {
		return true, fmt.Errorf("read terminal capture runtime identity: %w", err)
	}
	identity, err := json.Marshal(key)
	if err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO execution_reservations
    (agent_turn_id, execution_epoch, identity, revoked, creation_settled)
    VALUES ($1, $2, $3, true, true) ON CONFLICT DO NOTHING`, key.AgentTurnID, key.ExecutionEpoch, identity); err != nil {
		return true, fmt.Errorf("fence terminal capture dispatch: %w", err)
	}
	reservation, recorded, err := scanExecutionReservation(tx.QueryRow(ctx, `SELECT `+executionReservationColumns+`
    FROM execution_reservations WHERE agent_turn_id = $1 AND execution_epoch = $2 FOR UPDATE`, key.AgentTurnID, key.ExecutionEpoch))
	if err != nil {
		return true, fmt.Errorf("read terminal capture dispatch: %w", err)
	}
	if recorded.AssignmentID != key.AssignmentID || recorded.AgentSessionID != key.AgentSessionID ||
		recorded.AgentTurnID != key.AgentTurnID || recorded.ExecutionEpoch != key.ExecutionEpoch ||
		(recorded.RuntimeProfileName != "" && (recorded.RuntimeProfileName != key.RuntimeProfileName || recorded.RuntimeProfileVersion != key.RuntimeProfileVersion)) {
		return true, execution.ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE execution_reservations SET revoked = true,
    creation_settled = creation_settled OR dispatch_phase = 'reserved'
    WHERE agent_turn_id = $1 AND execution_epoch = $2`, key.AgentTurnID, key.ExecutionEpoch); err != nil {
		return true, fmt.Errorf("revoke terminal capture dispatch: %w", err)
	}
	var activity bool
	if err := tx.QueryRow(ctx, `
SELECT session.human_prompt_token IS NOT NULL
    OR turn.mutation_admission_open
    OR turn.prompt_recorded_at IS NOT NULL
    OR turn.next_invocation_number <> 1
    OR EXISTS (SELECT 1 FROM tool_invocations
               WHERE agent_turn_id = turn.id AND execution_epoch = turn.execution_epoch)
    OR EXISTS (SELECT 1 FROM tool_invocation_replays
               WHERE agent_turn_id = turn.id AND execution_epoch = turn.execution_epoch)
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
WHERE turn.id = $1 AND turn.execution_epoch = $2 AND session.id = $3`,
		job.AgentTurnID, job.ExecutionEpoch, job.AgentSessionID).Scan(&activity); err != nil {
		return true, fmt.Errorf("read terminal capture requirement: %w", err)
	}
	var preparationOnly bool
	if err := tx.QueryRow(ctx, `SELECT preparation_only_cleanup FROM execution_reservations
WHERE agent_turn_id = $1 AND execution_epoch = $2`, key.AgentTurnID, key.ExecutionEpoch).Scan(&preparationOnly); err != nil {
		return true, fmt.Errorf("read preparation-only cleanup proof: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("commit terminal capture requirement: %w", err)
	}
	noMainDispatch := reservation.Phase == execution.Reserved || reservation.Phase == execution.Setup
	// RecordNoExecution positively settles creation without a main resource.
	// An unresolved main dispatch with an empty resource is still ambiguous.
	noCreationEffects := reservation.Phase == execution.Main && reservation.CreationSettled && reservation.Resource == ""
	// A proof belongs to this exact reservation; later Turn activity overrides it.
	return activity || !preparationOnly && (!(noMainDispatch || noCreationEffects) || reservation.Resource != ""), nil
}
