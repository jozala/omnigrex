package store

import (
	"context"
	"github.com/jozala/omnigrex/internal/collaboration"
)

// ReserveCollaborationReceipt gives exactly one contender permission to POST.
// A losing or restarted contender reconciles the frozen payload, never reposts
// after an uncertain effect. Admission deduplication alone cannot ensure this.
func (s *Store) ReserveCollaborationReceipt(ctx context.Context, workflowID, id string, artifact collaboration.RenderedPublication) (collaboration.RenderedPublication, uint64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return collaboration.RenderedPublication{}, 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = rehydrateWorkflow(ctx, tx, workflowID); err != nil {
		return collaboration.RenderedPublication{}, 0, err
	}
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return collaboration.RenderedPublication{}, 0, err
	}
	for i := range state.Receipts {
		r := &state.Receipts[i]
		if r.ID != id {
			continue
		}
		if r.Artifact != nil && !r.PublicationRejected {
			return *r.Artifact, 0, nil
		}
		if artifact.Body == "" || artifact.Marker == "" {
			return collaboration.RenderedPublication{}, 0, collaboration.ErrInvalid
		}
		if r.Artifact == nil {
			r.Artifact = &artifact
		}
		r.PublicationAttempt++
		r.PublicationRejected = false
		if err = saveCollaborationTx(ctx, tx, workflowID, state); err != nil {
			return collaboration.RenderedPublication{}, 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return collaboration.RenderedPublication{}, 0, err
		}
		return *r.Artifact, r.PublicationAttempt, nil
	}
	return collaboration.RenderedPublication{}, 0, collaboration.ErrInvalid
}

// RejectCollaborationReceipt releases only the caller's proven rejected attempt.
// A stale or repeated report cannot release a subsequent contender's ownership.
func (s *Store) RejectCollaborationReceipt(ctx context.Context, workflowID, id string, attempt uint64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = rehydrateWorkflow(ctx, tx, workflowID); err != nil {
		return err
	}
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return err
	}
	for i := range state.Receipts {
		r := &state.Receipts[i]
		if r.ID != id {
			continue
		}
		if attempt == 0 || r.Artifact == nil || r.PublicationAttempt != attempt {
			return collaboration.ErrInvalid
		}
		r.PublicationRejected = true
		if err = saveCollaborationTx(ctx, tx, workflowID, state); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	return collaboration.ErrInvalid
}
