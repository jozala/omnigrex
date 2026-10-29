package store

import (
	"context"
	"errors"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// WithSnapshotInstallationFence excludes exact-execution revocation/cleanup,
// not lease renewal. The backend must acquire its execution lock first, matching
// cleanup's backend-lock -> reservation-lock order. No lease rows are held while
// the callback runs. Recovery cannot admit a successor until execution stops.
func (store *Store) WithSnapshotInstallationFence(ctx context.Context, lease AgentTurnLease, key execution.Key, operation func(context.Context) error) error {
	if operation == nil {
		return errors.New("snapshot installation callback is nil")
	}
	if key.AgentTurnID != lease.ID || key.ExecutionEpoch != lease.ExecutionEpoch || key.AssignmentID != lease.AgentParticipantID || key.AgentSessionID != lease.AgentSessionID {
		return ErrAgentTurnFenceLost
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reservation, recorded, err := scanExecutionReservation(tx.QueryRow(ctx, `SELECT `+executionReservationColumns+` FROM execution_reservations WHERE agent_turn_id=$1 AND execution_epoch=$2 FOR SHARE`, key.AgentTurnID, key.ExecutionEpoch))
	if err != nil || recorded != key || reservation.Revoked || !reservation.CreationSettled || reservation.Phase != execution.Main || reservation.Resource == "" {
		return ErrAgentTurnFenceLost
	}
	validate := func() error {
		turn, err := validateAgentTurnLeaseTx(ctx, tx, lease, "snapshot installation", true, true, "")
		if err != nil {
			return err
		}
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		return nil
	}
	if err := validate(); err != nil {
		return err
	}
	if err := operation(ctx); err != nil {
		return err
	}
	// A concurrent control/lease change cannot turn an old execution's bytes into
	// success evidence, even though its cleanup waits for the reservation guard.
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
