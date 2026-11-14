package store

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
)

const CollaborationStatusQueue = "collaboration-status"
const PublishCollaborationStatusJobKind = "PUBLISH_COLLABORATION_STATUS"

type CollaborationStatus struct {
	Receipt    collaboration.Receipt `json:"receipt"`
	Owner      string                `json:"owner"`
	Repository string                `json:"repository"`
	Issue      int                   `json:"issue"`
}

// One durable outbox event per milestone, independent of label projection and
// infrastructure retries. Publication reuses the shared collaboration receipt
// boundary, including exact frozen-artifact recovery after a lost response.
func enqueueCollaborationStatusTx(ctx context.Context, tx pgx.Tx, workflowID, key, feedback string, state *collaboration.State) error {
	id, err := randomUUID()
	if err != nil {
		return err
	}
	status := CollaborationStatus{Receipt: collaboration.Receipt{ID: "A" + id, Source: collaboration.Source{Channel: "omnigrex-status", ID: key}, Feedback: feedback}}
	for _, r := range state.Receipts {
		if r.Source == status.Receipt.Source {
			return nil
		}
	}
	if err = tx.QueryRow(ctx, `SELECT repository_owner,repository_name,issue_number FROM workflows WHERE id=$1`, workflowID).Scan(&status.Owner, &status.Repository, &status.Issue); err != nil {
		return err
	}
	state.Receipts = append(state.Receipts, status.Receipt)
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = insertJobTx(ctx, tx, jobInsert{queue: CollaborationStatusQueue, kind: PublishCollaborationStatusJobKind, payload: raw, maxAttempts: 3, idempotencyKey: "collaboration-status:" + workflowID + ":" + key, scope: jobInsertScope{workflowID: workflowID}})
	return err
}

// EnsurePlanningAuthorizationPrompts exposes the new action on pre-feature ready
// plans without republishing documents or changing confirmation/Workflow state.
// Bounded, locked outbox backfill is safe on restart and also covers operational
// restoration of a completed plan; it never reopens discussion or grants consent.
func (s *Store) EnsurePlanningAuthorizationPrompts(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT w.id::text FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id
 WHERE w.status='PLAN_READY' AND w.planning_only AND COALESCE(c.state->'planning'->>'ready_revision','')<>''
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(NULLIF(c.state->'receipts','null'::jsonb),'[]'::jsonb)) r WHERE r->'source'->>'channel'='omnigrex-status' AND r->'source'->>'id'='ready:'||(c.state->'planning'->>'ready_revision'))
 ORDER BY w.id LIMIT 20 FOR UPDATE OF w SKIP LOCKED`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		state, err := readCollaborationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		r := state.CurrentCheckpoint()
		if r == nil || !r.Published || r.Content.Status != "proposed" || state.Planning.ReadyRevision != r.ID {
			return collaboration.ErrInvalid
		}
		feedback := "Confirmed pair " + r.Reference + " is planning-ready. To authorize development, submit `/omnigrex implement " + r.Reference + "` alone. Confirmation alone never starts implementation."
		if err = enqueueCollaborationStatusTx(ctx, tx, id, "ready:"+r.ID, feedback, &state); err != nil {
			return err
		}
		if err = saveCollaborationTx(ctx, tx, id, state); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
