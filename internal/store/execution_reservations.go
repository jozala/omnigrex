package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// ExecutionLedger admits creation and phase transitions in short transactions.
// Each transaction uses the connection already acquired for its Turn fence and
// commits before the adapter dispatches external work. Facts returned by the
// backend can still be recorded after fence loss without adopting execution.
func (store *Store) ExecutionLedger(lease AgentTurnLease) execution.Ledger {
	return admittedExecutionLedger{Store: store, lease: lease}
}

type admittedExecutionLedger struct {
	*Store
	lease AgentTurnLease
}

func (ledger admittedExecutionLedger) admit(ctx context.Context, key execution.Key, operation func(pgx.Tx) error) error {
	if key.AgentTurnID != ledger.lease.ID || key.AssignmentID != ledger.lease.AgentAssignmentID || key.AgentSessionID != ledger.lease.AgentSessionID || key.ExecutionEpoch != ledger.lease.ExecutionEpoch {
		return ErrAgentTurnFenceLost
	}
	return ledger.withLockedAgentTurnLeaseOptions(ctx, ledger.lease, "admit execution operation", true, true, func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		return operation(tx)
	})
}
func (ledger admittedExecutionLedger) ReserveExecution(ctx context.Context, key execution.Key, digest string) (reservation execution.Reservation, fresh bool, err error) {
	err = ledger.admit(ctx, key, func(tx pgx.Tx) error {
		var err error
		reservation, fresh, err = reserveExecution(ctx, tx, key, digest)
		return err
	})
	return reservation, fresh, err
}
func (ledger admittedExecutionLedger) BeginExecutionPhase(ctx context.Context, key execution.Key, phase execution.Phase) error {
	return ledger.admit(ctx, key, func(tx pgx.Tx) error { return beginExecutionPhase(ctx, tx, key, phase) })
}

type executionQueryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

const executionReservationColumns = `COALESCE(specification_sha256, ''), COALESCE(resource_ref, ''), revoked, creation_settled,
    dispatch_phase, COALESCE(setup_resource_ref, ''), setup_settled, identity`

func scanExecutionReservation(row pgx.Row) (execution.Reservation, execution.Key, error) {
	var reservation execution.Reservation
	var identity []byte
	err := row.Scan(&reservation.Digest, &reservation.Resource, &reservation.Revoked, &reservation.CreationSettled,
		&reservation.Phase, &reservation.SetupResource, &reservation.SetupSettled, &identity)
	var key execution.Key
	if err == nil {
		err = json.Unmarshal(identity, &key)
	}
	reservation.Key = key
	return reservation, key, err
}

// ReserveExecution is also used by isolated qualification paths. Production
// dispatch uses the lease-bound ExecutionLedger above.
func (store *Store) ReserveExecution(ctx context.Context, key execution.Key, digest string) (execution.Reservation, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return execution.Reservation{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reservation, fresh, err := reserveExecution(ctx, tx, key, digest)
	if err != nil {
		return reservation, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return reservation, false, err
	}
	return reservation, fresh, nil
}
func reserveExecution(ctx context.Context, tx pgx.Tx, key execution.Key, digest string) (execution.Reservation, bool, error) {
	identity, err := json.Marshal(key)
	if err != nil {
		return execution.Reservation{}, false, err
	}
	result, err := tx.Exec(ctx, `INSERT INTO execution_reservations (agent_turn_id, execution_epoch, identity, specification_sha256)
        VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, key.AgentTurnID, key.ExecutionEpoch, identity, digest)
	if err != nil {
		return execution.Reservation{}, false, err
	}
	reservation, recorded, err := scanExecutionReservation(tx.QueryRow(ctx, `SELECT `+executionReservationColumns+`
        FROM execution_reservations WHERE agent_turn_id = $1 AND execution_epoch = $2 FOR UPDATE`, key.AgentTurnID, key.ExecutionEpoch))
	if err != nil {
		return reservation, false, err
	}
	if reservation.Revoked || recorded != key || reservation.Digest != digest {
		return reservation, false, execution.ErrConflict
	}
	return reservation, result.RowsAffected() == 1, nil
}

func (store *Store) BeginExecutionPhase(ctx context.Context, key execution.Key, phase execution.Phase) error {
	return beginExecutionPhase(ctx, store.pool, key, phase)
}
func beginExecutionPhase(ctx context.Context, queryer executionQueryer, key execution.Key, phase execution.Phase) error {
	if phase != execution.Setup && phase != execution.Main {
		return execution.ErrInvalidSpec
	}
	identity, err := json.Marshal(key)
	if err != nil {
		return err
	}
	result, err := queryer.Exec(ctx, `UPDATE execution_reservations SET dispatch_phase = $4
        WHERE agent_turn_id = $1 AND execution_epoch = $2 AND identity = $3::jsonb
        AND NOT revoked AND NOT creation_settled
        AND (dispatch_phase = 'reserved' OR ($4 = 'main' AND dispatch_phase = 'setup' AND setup_settled))`,
		key.AgentTurnID, key.ExecutionEpoch, identity, phase)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return execution.ErrConflict
	}
	return nil
}

func (store *Store) BindSetupResource(ctx context.Context, key execution.Key, resource execution.Ref) error {
	if resource == "" {
		return execution.ErrConflict
	}
	return store.updateExecutionFact(ctx, key, resource, `UPDATE execution_reservations SET setup_resource_ref = $4
        WHERE agent_turn_id = $1 AND execution_epoch = $2 AND identity = $3::jsonb AND dispatch_phase = 'setup'
        AND NOT setup_settled AND (setup_resource_ref IS NULL OR setup_resource_ref = $4)`)
}
func (store *Store) RecordSetupStopped(ctx context.Context, key execution.Key, resource execution.Ref) error {
	if resource == "" {
		return execution.ErrConflict
	}
	return store.updateExecutionFact(ctx, key, resource, `UPDATE execution_reservations SET setup_settled = true
        WHERE agent_turn_id = $1 AND execution_epoch = $2 AND identity = $3::jsonb AND setup_resource_ref = $4`)
}
func (store *Store) BindExecution(ctx context.Context, key execution.Key, resource execution.Ref) error {
	if resource == "" {
		return execution.ErrConflict
	}
	return store.updateExecutionFact(ctx, key, resource, `UPDATE execution_reservations SET resource_ref = $4, creation_settled = true
        WHERE agent_turn_id = $1 AND execution_epoch = $2 AND identity = $3::jsonb AND dispatch_phase = 'main'
        AND ((resource_ref IS NULL AND NOT creation_settled) OR resource_ref = $4)`)
}
func (store *Store) RecordNoExecution(ctx context.Context, key execution.Key) error {
	return store.updateExecutionFact(ctx, key, nil, `UPDATE execution_reservations SET creation_settled = true
        WHERE agent_turn_id = $1 AND execution_epoch = $2 AND identity = $3::jsonb AND resource_ref IS NULL
        AND (setup_resource_ref IS NULL OR setup_settled) AND $4::text IS NULL`)
}
func (store *Store) updateExecutionFact(ctx context.Context, key execution.Key, value any, query string) error {
	identity, err := json.Marshal(key)
	if err != nil {
		return err
	}
	result, err := store.pool.Exec(ctx, query, key.AgentTurnID, key.ExecutionEpoch, identity, value)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return execution.ErrConflict
	}
	return nil
}

// RevokeExecution inserts a tombstone even when no reservation exists. The
// primary-key conflict serializes it with first reservation, rather than relying
// on a row lock on an absent row. A reserved phase has dispatched no external work.
func (store *Store) RevokeExecution(ctx context.Context, key execution.Key) (execution.Reservation, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return execution.Reservation{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	identity, err := json.Marshal(key)
	if err != nil {
		return execution.Reservation{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO execution_reservations
        (agent_turn_id, execution_epoch, identity, revoked, creation_settled)
        VALUES ($1, $2, $3, true, true) ON CONFLICT DO NOTHING`, key.AgentTurnID, key.ExecutionEpoch, identity); err != nil {
		return execution.Reservation{}, false, err
	}
	reservation, recorded, err := scanExecutionReservation(tx.QueryRow(ctx, `SELECT `+executionReservationColumns+`
        FROM execution_reservations WHERE agent_turn_id = $1 AND execution_epoch = $2 FOR UPDATE`, key.AgentTurnID, key.ExecutionEpoch))
	if err != nil {
		return reservation, false, err
	}
	if recorded.AssignmentID != key.AssignmentID || recorded.AgentSessionID != key.AgentSessionID ||
		(key.RuntimeProfileName != "" && recorded.RuntimeProfileName != "" && (recorded.RuntimeProfileName != key.RuntimeProfileName || recorded.RuntimeProfileVersion != key.RuntimeProfileVersion)) {
		return execution.Reservation{}, false, execution.ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE execution_reservations SET revoked = true,
        creation_settled = creation_settled OR dispatch_phase = 'reserved'
        WHERE agent_turn_id = $1 AND execution_epoch = $2`, key.AgentTurnID, key.ExecutionEpoch); err != nil {
		return execution.Reservation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return execution.Reservation{}, false, fmt.Errorf("commit execution revocation: %w", err)
	}
	reservation.Revoked = true
	reservation.CreationSettled = reservation.CreationSettled || reservation.Phase == execution.Reserved
	return reservation, true, nil
}
