package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
)

func TestStoreReadLedgerMapsCredentialFreeReadMetadata(t *testing.T) {
	durable := &readInvocationStore{}
	ledger, err := mcp.NewStoreReadLedger(durable)
	if err != nil {
		t.Fatalf("NewStoreReadLedger() error = %v", err)
	}
	lease := store.AgentTurnLease{AgentTurn: store.AgentTurn{ID: "turn", ExecutionEpoch: 3}, OwnerToken: "secret-owner-token"}
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	err = ledger.RecordRead(context.Background(), lease, mcp.ReadRecord{
		Invocation: mcp.Invocation{Name: mcp.ToolGetIssue, Arguments: json.RawMessage(`{}`)},
		Result:     json.RawMessage(`{"number":12}`), StartedAt: started, FinishedAt: started.Add(time.Millisecond), Succeeded: true,
	})
	if err != nil {
		t.Fatalf("RecordRead() error = %v", err)
	}
	if durable.lease.OwnerToken != lease.OwnerToken || durable.invocation.ToolName != mcp.ToolGetIssue ||
		string(durable.invocation.Request) != `{}` || string(durable.invocation.Result) != `{"number":12}` {
		t.Errorf("durable read = lease %#v invocation %#v", durable.lease, durable.invocation)
	}
}

type readInvocationStore struct {
	lease      store.AgentTurnLease
	invocation store.ReadInvocation
}

func (durable *readInvocationStore) RecordReadInvocation(_ context.Context, lease store.AgentTurnLease, invocation store.ReadInvocation) (store.ReadInvocationRecord, error) {
	durable.lease = lease
	durable.invocation = invocation
	return store.ReadInvocationRecord{}, nil
}
