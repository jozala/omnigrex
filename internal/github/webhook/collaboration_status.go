package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

// CollaborationStatusWorker delivers milestone receipts independently of labels.
// Retry re-observes the exact frozen artifact and never bypasses unknown POSTs.
type CollaborationStatusWorker struct {
	Store   *store.Store
	Adapter *IssueCollaboration
	Owner   string
	OnError func(error)
}

func (w *CollaborationStatusWorker) ProcessNext(ctx context.Context) (bool, error) {
	if err := w.Store.EnsurePlanningAuthorizationPrompts(ctx); err != nil {
		return false, err
	}
	lease, err := w.Store.ClaimJobKind(ctx, store.CollaborationStatusQueue, store.PublishCollaborationStatusJobKind, w.Owner, 2*time.Minute)
	if err != nil || lease == nil {
		return false, err
	}
	call, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var status store.CollaborationStatus
	err = json.Unmarshal(lease.Payload, &status)
	if err == nil {
		var credential string
		credential, err = w.Adapter.Credentials.RepositoryCredential(call, status.Owner, status.Repository)
		if err == nil {
			err = w.Adapter.publishReceipt(call, lease.WorkflowID, status.Receipt, credential, status.Owner, status.Repository, status.Issue)
		}
	}
	if err != nil {
		// Do not persist provider diagnostics or credentials in this status outbox.
		failure := errors.New("collaboration milestone publication not confirmed; inspect exact reserved receipt and infrastructure")
		return true, errors.Join(failure, w.Store.FailJob(ctx, *lease, failure, true, time.Second))
	}
	return true, w.Store.CompleteJob(ctx, *lease, json.RawMessage(`{"published":true}`))
}
func (w *CollaborationStatusWorker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		processed, err := w.ProcessNext(ctx)
		if err != nil && w.OnError != nil {
			w.OnError(err)
		}
		if !processed || err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return ctx.Err()
}
