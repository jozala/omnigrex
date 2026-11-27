package store

import (
	"context"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// WithParticipantStorageWriter is called after the workspace service acquires
// its shared physical-writer lock. Collection takes the exclusive lock before
// invoking any cleaner; no heartbeat-dependent row lock spans filesystem I/O.
func (store *Store) WithParticipantStorageWriter(ctx context.Context, id string, operation func(context.Context) error) error {
	if !validUUID(id) || operation == nil {
		return execution.ErrInvalidSpec
	}
	var allowed bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_participants a WHERE a.id=$1 AND a.state_deleted_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM assignment_retention_targets t JOIN assignment_retention_generations g ON g.id=t.retention_generation_id
 WHERE t.assignment_id=a.id AND t.session_id IS NULL AND g.status IN ('COLLECTING','COLLECTED')))`, id).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return execution.ErrConflict
	}
	return operation(ctx)
}
