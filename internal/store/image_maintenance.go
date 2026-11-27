package store

import (
	"context"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// ImageMaintenanceRoots includes COLLECTING Participants, unlike Session
// availability inventory. This is protection evidence, never deletion authority.
func (store *Store) ImageMaintenanceRoots(ctx context.Context, binding execution.BackendBinding, limit int) ([]execution.StateRef, bool, error) {
	if limit < 1 || limit > 10000 {
		return nil, false, execution.ErrInvalidSpec
	}
	var actual execution.BackendBinding
	if err := store.pool.QueryRow(ctx, `SELECT family,contract_version,namespace FROM execution_backend_binding`).Scan(&actual.Family, &actual.Version, &actual.Namespace); err != nil {
		return nil, false, err
	}
	if actual != binding {
		return nil, false, execution.ErrBindingMismatch
	}
	rows, err := store.pool.Query(ctx, `SELECT id::text,runtime_image_digest FROM agent_participants WHERE state_deleted_at IS NULL ORDER BY id LIMIT $1`, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	refs := make([]execution.StateRef, 0)
	for rows.Next() {
		var ref execution.StateRef
		if err := rows.Scan(&ref.ParticipantID, &ref.Image); err != nil {
			return nil, false, err
		}
		ref.Backend = binding
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(refs) > limit
	if truncated {
		refs = refs[:limit]
	}
	return refs, truncated, nil
}
