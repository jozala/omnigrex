package store

import (
	"context"
	"github.com/jozala/omnigrex/internal/collaboration"
)

// ReserveCollaborationReceipt gives exactly one contender permission to POST.
// A losing or restarted contender reconciles the frozen payload, never reposts
// after an uncertain effect. Admission deduplication alone cannot ensure this.
func (s *Store) ReserveCollaborationReceipt(ctx context.Context, workflowID, id string, artifact collaboration.RenderedPublication) (collaboration.RenderedPublication, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return collaboration.RenderedPublication{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = rehydrateWorkflow(ctx, tx, workflowID); err != nil {
		return collaboration.RenderedPublication{}, false, err
	}
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return collaboration.RenderedPublication{}, false, err
	}
	for i := range state.Receipts {
		r := &state.Receipts[i]
		if r.ID != id {
			continue
		}
		if r.Artifact != nil {
			return *r.Artifact, false, nil
		}
		if artifact.Body == "" || artifact.Marker == "" {
			return collaboration.RenderedPublication{}, false, collaboration.ErrInvalid
		}
		r.Artifact = &artifact
		if err = saveCollaborationTx(ctx, tx, workflowID, state); err != nil {
			return collaboration.RenderedPublication{}, false, err
		}
		if err = tx.Commit(ctx); err != nil {
			return collaboration.RenderedPublication{}, false, err
		}
		return artifact, true, nil
	}
	return collaboration.RenderedPublication{}, false, collaboration.ErrInvalid
}
