package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const RuntimeOOMDiagnostic = "Docker confirmed the Runtime Process was OOM-killed (runtime_oom_killed). Inspect the affected turn and its bound Runtime Profile before starting another Workflow Attempt; see the operator guide."

var ErrRuntimeOOMEvidenceConflict = errors.New("Runtime Process OOM evidence conflicts with the Agent Turn")

// RecordAgentTurnRuntimeOOM saves a Docker-confirmed exit before the live process is removed.
// The caller supplies only an exit observed from the exact container belonging to this lease.
func (store *Store) RecordAgentTurnRuntimeOOM(ctx context.Context, lease AgentTurnLease, containerID string, exitCode int) error {
	if !validRuntimeOOMEvidence(containerID, exitCode) {
		return ErrRuntimeOOMEvidenceConflict
	}
	return store.withLockedAgentTurnLease(ctx, lease, "record Runtime Process OOM", func(tx pgx.Tx, turn lockedTurn) error {
		return recordRuntimeOOM(ctx, tx, turn.ID, lease.ExecutionEpoch, containerID, exitCode, false)
	})
}

// RecordRecoveredRuntimeOOM saves evidence under the exact stale-runtime stop job fence.
func (store *Store) RecordRecoveredRuntimeOOM(ctx context.Context, lease JobLease, containerID string, exitCode int) error {
	if !validRuntimeOOMEvidence(containerID, exitCode) {
		return ErrRuntimeOOMEvidenceConflict
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin recovered Runtime Process OOM observation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, StopStaleRuntimeJobKind)
	if err != nil {
		return err
	}
	if err := recordRuntimeOOM(ctx, tx, job.AgentTurnID, job.ExecutionEpoch, containerID, exitCode, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validRuntimeOOMEvidence(containerID string, exitCode int) bool {
	return containerID != "" && len(containerID) <= 128 && strings.TrimSpace(containerID) == containerID && exitCode >= 0
}

func recordRuntimeOOM(ctx context.Context, tx pgx.Tx, turnID string, epoch int64, containerID string, exitCode int, recovery bool) error {
	query := `UPDATE agent_turns
SET runtime_oom_container_id = $3, runtime_oom_exit_code = $4,
    runtime_oom_observed_at = COALESCE(runtime_oom_observed_at, clock_timestamp())
WHERE id = $1 AND execution_epoch = $2 AND (runtime_oom_container_id IS NULL
    OR (runtime_oom_container_id = $3 AND runtime_oom_exit_code = $4))`
	if recovery {
		query += ` AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL`
	} else {
		query += ` AND active`
	}
	result, err := tx.Exec(ctx, query, turnID, epoch, containerID, exitCode)
	if err != nil {
		return fmt.Errorf("record Runtime Process OOM: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrRuntimeOOMEvidenceConflict
	}
	return nil
}
