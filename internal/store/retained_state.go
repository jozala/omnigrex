package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// BindExecutionBackend claims the transitional namespace once. Changing a
// configured backend or its storage namespace never reinterprets retained data.
func (store *Store) BindExecutionBackend(ctx context.Context, binding execution.BackendBinding) error {
	if binding.Family == "" || binding.Version <= 0 || binding.Namespace == "" {
		return execution.ErrBindingMismatch
	}
	result, err := store.pool.Exec(ctx, `UPDATE execution_backend_binding SET family = $1, contract_version = $2, namespace = $3
WHERE (family IS NULL OR (family = $1 AND contract_version = $2)) AND (namespace IS NULL OR namespace = $3)`, binding.Family, binding.Version, binding.Namespace)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return execution.ErrBindingMismatch
	}
	return nil
}

func readStateRef(ctx context.Context, queryer executionQueryer, participantID, sessionID string) (execution.StateRef, error) {
	var ref execution.StateRef
	err := queryer.QueryRow(ctx, `SELECT a.id::text, b.family, b.contract_version, COALESCE(b.namespace, ''), r.format,
COALESCE(a.runtime_profile_content_sha256, ''), a.runtime_image_digest
FROM retained_runtime_states r JOIN agent_assignments a ON a.id = r.agent_participant_id
CROSS JOIN execution_backend_binding b WHERE a.id = $1 AND a.state_deleted_at IS NULL
AND ($2::text = '' OR EXISTS (SELECT 1 FROM agent_sessions s WHERE s.id::text = $2 AND s.agent_assignment_id = a.id AND s.state_deleted_at IS NULL))`, participantID, sessionID).Scan(
		&ref.ID, &ref.Backend.Family, &ref.Backend.Version, &ref.Backend.Namespace, &ref.Format, &ref.RuntimeProfileSHA256, &ref.Image)
	if errors.Is(err, pgx.ErrNoRows) {
		return ref, execution.ErrStateUnavailable
	}
	ref.ParticipantID, ref.SessionID = participantID, sessionID
	return ref, err
}

// AgentTurnState admits an exact Session attachment under the live Turn fence.
// Existing admission/recovery barriers must have confirmed prior execution stop.
func (store *Store) AgentTurnState(ctx context.Context, lease AgentTurnLease) (ref execution.StateRef, err error) {
	err = store.withLockedAgentTurnLeaseOptions(ctx, lease, "bind retained state writer", true, true, func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		// Serialize with offline recovery before admitting another state writer.
		if _, err := tx.Exec(ctx, `SELECT singleton FROM execution_backend_binding FOR SHARE`); err != nil {
			return err
		}
		var err error
		ref, err = readStateRef(ctx, tx, lease.AgentAssignmentID, lease.AgentSessionID)
		return err
	})
	return ref, err
}

// WithStateRecovery protects an offline export/restore against new writers and
// collection. It is deliberately separate from heartbeat-driven Turn operations.
// The callback receives a snapshot source, so a single-connection pool suffices.
func (store *Store) WithStateRecovery(ctx context.Context, participantID string, operation func(execution.StateRef, execution.StateMetadataSource) error) error {
	if operation == nil {
		return execution.ErrInvalidSpec
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT singleton FROM execution_backend_binding FOR UPDATE`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE agent_turns, agent_sessions, agent_assignments, assignment_retention_generations, execution_reservations IN SHARE MODE`); err != nil {
		return err
	}
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_turns WHERE active)
OR EXISTS (SELECT 1 FROM agent_sessions WHERE human_prompt_token IS NOT NULL)
OR EXISTS (SELECT 1 FROM assignment_retention_generations WHERE status = 'COLLECTING')
OR EXISTS (SELECT 1 FROM execution_reservations WHERE NOT creation_settled AND dispatch_phase <> 'reserved')`).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return execution.ErrConflict
	}
	ref, err := readStateRef(ctx, tx, participantID, "")
	if err != nil {
		return err
	}
	var snapshot stateRecoverySnapshot
	snapshot.ref = ref
	if err := tx.QueryRow(ctx, `SELECT metadata, initialized FROM retained_runtime_states WHERE agent_participant_id = $1 FOR SHARE`, participantID).Scan(&snapshot.metadata, &snapshot.initialized); err != nil {
		return err
	}
	if err := operation(ref, snapshot); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type stateRecoverySnapshot struct {
	ref         execution.StateRef
	metadata    json.RawMessage
	initialized bool
}

func (source stateRecoverySnapshot) StateMetadata(_ context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	if source.ref != ref {
		return nil, false, execution.ErrBindingMismatch
	}
	return source.metadata, source.initialized, nil
}

func (store *Store) StateMetadata(ctx context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	actual, err := readStateRef(ctx, store.pool, ref.ParticipantID, ref.SessionID)
	if err != nil {
		return nil, false, err
	}
	if actual != ref {
		return nil, false, execution.ErrBindingMismatch
	}
	var metadata json.RawMessage
	var initialized bool
	err = store.pool.QueryRow(ctx, `SELECT metadata, initialized FROM retained_runtime_states WHERE agent_participant_id = $1`, ref.ID).Scan(&metadata, &initialized)
	return metadata, initialized, err
}

func (store *Store) MarkAgentTurnStateInitialized(ctx context.Context, lease AgentTurnLease, ref execution.StateRef) error {
	return store.withLockedAgentTurnLeaseOptions(ctx, lease, "record retained state availability", true, true, func(tx pgx.Tx, turn lockedTurn) error {
		actual, err := readStateRef(ctx, tx, lease.AgentAssignmentID, lease.AgentSessionID)
		if err != nil {
			return err
		}
		if actual != ref || turn.Status != AgentTurnRunning {
			return execution.ErrBindingMismatch
		}
		_, err = tx.Exec(ctx, `UPDATE retained_runtime_states SET initialized = TRUE WHERE agent_participant_id = $1`, ref.ID)
		return err
	})
}

// CollectionState resolves only irrevocably authorized Participant collection.
func (store *Store) CollectionState(ctx context.Context, participantID string) (execution.StateRef, error) {
	var allowed bool
	err := store.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM assignment_retention_targets t JOIN assignment_retention_generations g ON g.id = t.retention_generation_id
WHERE t.assignment_id = $1 AND t.session_id IS NULL AND g.status = 'COLLECTING'
AND NOT EXISTS (SELECT 1 FROM agent_turns x JOIN agent_sessions s ON s.id = x.agent_session_id WHERE s.agent_assignment_id = $1 AND x.active))`, participantID).Scan(&allowed)
	if err != nil {
		return execution.StateRef{}, err
	}
	if !allowed {
		return execution.StateRef{}, ErrAssignmentCollectionFenceLost
	}
	return readStateRef(ctx, store.pool, participantID, "")
}

func (store *Store) ListProtectedStates(ctx context.Context) ([]execution.StateRef, error) {
	rows, err := store.pool.Query(ctx, `SELECT a.id::text, b.family, b.contract_version, COALESCE(b.namespace, ''), r.format,
COALESCE(a.runtime_profile_content_sha256, ''), a.runtime_image_digest
FROM retained_runtime_states r JOIN agent_assignments a ON a.id = r.agent_participant_id
CROSS JOIN execution_backend_binding b WHERE a.state_deleted_at IS NULL
AND NOT EXISTS (SELECT 1 FROM assignment_retention_targets t
JOIN assignment_retention_generations g ON g.id = t.retention_generation_id
WHERE t.assignment_id = a.id AND t.session_id IS NULL AND g.status = 'COLLECTING')
ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	var refs []execution.StateRef
	for rows.Next() {
		var ref execution.StateRef
		if err := rows.Scan(&ref.ID, &ref.Backend.Family, &ref.Backend.Version, &ref.Backend.Namespace, &ref.Format, &ref.RuntimeProfileSHA256, &ref.Image); err != nil {
			rows.Close()
			return nil, err
		}
		ref.ParticipantID = ref.ID
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return refs, nil
}
