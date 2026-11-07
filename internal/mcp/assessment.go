package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
)

type assessmentStore interface {
	AssessHumanInput(context.Context, store.AgentTurnLease, string, []collaboration.Assessment) error
	HasHumanInputAssessmentEffect(context.Context, string, string, string, []collaboration.Assessment) (bool, error)
}

type assessmentArguments struct {
	Assessments []collaboration.Assessment `json:"assessments"`
}

func (backend *ProductionBackend) assessHumanInput(ctx context.Context, in Invocation) (json.RawMessage, error) {
	ledger, ok := backend.clarifications.(assessmentStore)
	if !ok {
		return nil, ErrToolDependency
	}
	var args assessmentArguments
	if json.Unmarshal(in.Arguments, &args) != nil {
		return nil, ErrInvalidInvocation
	}
	if err := ledger.AssessHumanInput(ctx, in.lease, in.OperationID, args.Assessments); err != nil {
		if errors.Is(err, collaboration.ErrInvalid) {
			return nil, ErrInvalidInvocation
		}
		return nil, OutcomeUnknown(err)
	}
	return json.Marshal(map[string]any{"recorded": true})
}

func (reconciler *ProductionReconciler) reconcileHumanInputAssessment(ctx context.Context, scope store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	ledger, ok := reconciler.clarifications.(assessmentStore)
	if !ok || mutation.ExternalService != "omnigrex" || mutation.ExternalResourceID != scope.WorkflowID {
		return unresolvedReconciliation(), nil
	}
	var args assessmentArguments
	if json.Unmarshal(mutation.Request, &args) != nil || len(args.Assessments) == 0 {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	found, err := ledger.HasHumanInputAssessmentEffect(ctx, scope.WorkflowID, mutation.ID, scope.Turn.AgentParticipantID, args.Assessments)
	if err != nil || !found {
		return unresolvedReconciliation(), err
	}
	raw, _ := json.Marshal(map[string]any{"recorded": true})
	return foundReconciliation(raw), nil
}
