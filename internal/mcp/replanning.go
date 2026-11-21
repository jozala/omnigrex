package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

type replanningStore interface {
	RequestReplanning(context.Context, store.AgentTurnLease, string, collaboration.ReplanningConcern) (collaboration.ReplanningCycle, error)
	GetReplanningRequest(context.Context, string, string) (collaboration.ReplanningCycle, error)
}

func replanningRequestResult(c collaboration.ReplanningCycle) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"status": "recorded", "handoff": "pending_settlement", "outcome": "REPLANNING_REQUESTED", "cycle_id": c.Scope.OperationID, "accepted_revision_id": c.Concern.AcceptedRevisionID})
}
func (b *ProductionBackend) requestReplanning(ctx context.Context, in Invocation) (json.RawMessage, error) {
	ledger, ok := b.clarifications.(replanningStore)
	if !ok {
		return nil, ErrToolDependency
	}
	var concern collaboration.ReplanningConcern
	if json.Unmarshal(in.Arguments, &concern) != nil {
		return nil, ErrInvalidInvocation
	}
	cycle, err := ledger.RequestReplanning(ctx, in.lease, in.OperationID, concern)
	if errors.Is(err, collaboration.ErrInvalid) {
		return nil, ErrInvalidInvocation
	}
	if err != nil {
		return nil, OutcomeUnknown(err)
	}
	return replanningRequestResult(cycle)
}
func (r *ProductionReconciler) reconcileReplanningRequest(ctx context.Context, scope store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	if scope.Role != workflow.RoleDeveloper || mutation.ExternalService != "omnigrex" || mutation.ExternalResourceID != scope.WorkflowID {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	ledger, ok := r.clarifications.(replanningStore)
	if !ok {
		return unresolvedReconciliation(), nil
	}
	c, err := ledger.GetReplanningRequest(ctx, scope.WorkflowID, mutation.ID)
	if err != nil {
		return unresolvedReconciliation(), nil
	}
	var concern collaboration.ReplanningConcern
	if json.Unmarshal(mutation.Request, &concern) != nil || c.Scope.OperationID != mutation.ID || c.Scope.TurnID != mutation.AgentTurnID || c.Scope.ExecutionEpoch != uint64(mutation.ExecutionEpoch) || c.Scope.DeveloperParticipantID != scope.Turn.AgentParticipantID || !concern.Equal(c.Concern) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	raw, err := replanningRequestResult(c)
	if err != nil {
		return unresolvedReconciliation(), err
	}
	return foundReconciliation(raw), nil
}
