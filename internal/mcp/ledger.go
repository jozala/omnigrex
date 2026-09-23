package mcp

import (
	"context"
	"errors"

	"github.com/jozala/omnigrex/internal/store"
)

// ReadInvocationStore is the durable read ledger implemented by Store.
type ReadInvocationStore interface {
	RecordReadInvocation(context.Context, store.AgentTurnLease, store.ReadInvocation) (store.ReadInvocationRecord, error)
}

type StoreReadLedger struct {
	store ReadInvocationStore
}

func NewStoreReadLedger(invocationStore ReadInvocationStore) (*StoreReadLedger, error) {
	if invocationStore == nil {
		return nil, errors.New("MCP read invocation Store is nil")
	}
	return &StoreReadLedger{store: invocationStore}, nil
}

func (ledger *StoreReadLedger) RecordRead(ctx context.Context, lease store.AgentTurnLease, record ReadRecord) error {
	_, err := ledger.store.RecordReadInvocation(ctx, lease, store.ReadInvocation{
		ToolName: record.Invocation.Name, Request: record.Invocation.Arguments,
		Result: record.Result, LastError: record.LastError,
		StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
	})
	return err
}

var _ ReadLedger = (*StoreReadLedger)(nil)
