package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
)

type PlanningStore interface {
	ReservePlanningCheckpoint(context.Context, store.AgentTurnLease, string, collaboration.PlanningContent) (collaboration.PlanningRevision, error)
	ConfirmPlanningPublication(context.Context, string, string, collaboration.Source) error
	GetPlanningCheckpoint(context.Context, string, string) (collaboration.PlanningRevision, error)
}

func (backend *ProductionBackend) publishPlanningCheckpoint(ctx context.Context, in Invocation) (json.RawMessage, error) {
	ledger, ok := backend.clarifications.(PlanningStore)
	if !ok {
		return nil, ErrToolDependency
	}
	var args struct {
		Content   collaboration.PlanningContent `json:"content"`
		Signature string                        `json:"signature"`
	}
	if json.Unmarshal(in.Arguments, &args) != nil {
		return nil, ErrInvalidInvocation
	}
	op := collaboration.Publication{WorkflowID: in.Scope.WorkflowID, Publisher: backend.clarificationDelivery(in), Confirm: func(ctx context.Context, id string, source collaboration.Source) error {
		if err := ledger.ConfirmPlanningPublication(ctx, in.Scope.WorkflowID, id, source); err != nil {
			return OutcomeUnknown(err)
		}
		return nil
	}}
	r, err := op.PublishCheckpoint(ctx, func(ctx context.Context) (collaboration.PlanningRevision, error) {
		r, err := ledger.ReservePlanningCheckpoint(ctx, in.lease, in.OperationID, args.Content)
		if err != nil && !errors.Is(err, collaboration.ErrInvalid) {
			err = OutcomeUnknown(err)
		}
		return r, err
	}, args.Signature)
	if errors.Is(err, collaboration.ErrInvalid) {
		return nil, ErrInvalidInvocation
	}
	if err != nil {
		return nil, classifyGitHubMutationError(err)
	}
	return planningCheckpointResult(r)
}
func planningCheckpointResult(r collaboration.PlanningRevision) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"revision_id": r.ID, "source": r.Source, "status": r.Content.Status})
}
func (reconciler *ProductionReconciler) reconcilePlanningCheckpoint(ctx context.Context, scope store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	ledger, ok := reconciler.clarifications.(PlanningStore)
	if !ok {
		return unresolvedReconciliation(), nil
	}
	r, err := ledger.GetPlanningCheckpoint(ctx, scope.WorkflowID, "P"+mutation.ID)
	if err != nil {
		return unresolvedReconciliation(), nil
	}
	var args struct {
		Content   collaboration.PlanningContent `json:"content"`
		Signature string                        `json:"signature"`
	}
	if json.Unmarshal(mutation.Request, &args) != nil || r.ParticipantID != scope.Turn.AgentParticipantID || r.TurnID != mutation.AgentTurnID {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	expected, _ := json.Marshal(args.Content)
	actual, _ := json.Marshal(r.Content)
	if string(expected) != string(actual) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	publisher, err := reconciler.clarificationDelivery(scope, mutation)
	if err != nil {
		return MutationReconciliationResult{}, err
	}
	op := collaboration.Publication{WorkflowID: scope.WorkflowID, Publisher: publisher, Confirm: func(ctx context.Context, id string, source collaboration.Source) error {
		return ledger.ConfirmPlanningPublication(ctx, scope.WorkflowID, id, source)
	}}
	found, err := op.ReconcileCheckpoint(ctx, r, args.Signature)
	if err != nil || !found {
		return unresolvedReconciliation(), err
	}
	r, err = ledger.GetPlanningCheckpoint(ctx, scope.WorkflowID, r.ID)
	if err != nil {
		return unresolvedReconciliation(), err
	}
	raw, _ := planningCheckpointResult(r)
	return foundReconciliation(raw), nil
}

type completePlanningArguments struct {
	RevisionID string `json:"revision_id"`
	Commit     string `json:"repository_commit"`
	Evidence   string `json:"evidence"`
	Resolved   bool   `json:"material_decisions_resolved"`
}

func (backend *ProductionBackend) completePlanning(ctx context.Context, in Invocation) (json.RawMessage, error) {
	ledger, ok := backend.clarifications.(interface {
		CompletePlanning(context.Context, store.AgentTurnLease, string, string, string, string, bool) error
	})
	if !ok {
		return nil, ErrToolDependency
	}
	var args completePlanningArguments
	if json.Unmarshal(in.Arguments, &args) != nil {
		return nil, ErrInvalidInvocation
	}
	if err := ledger.CompletePlanning(ctx, in.lease, in.OperationID, args.RevisionID, args.Commit, args.Evidence, args.Resolved); err != nil {
		if errors.Is(err, collaboration.ErrInvalid) {
			return nil, ErrInvalidInvocation
		}
		return nil, OutcomeUnknown(err)
	}
	return json.Marshal(map[string]any{"outcome": "PLANNING_READY", "revision_id": args.RevisionID, "repository_commit": args.Commit})
}
func (reconciler *ProductionReconciler) reconcilePlanningCompletion(ctx context.Context, scope store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	ledger, ok := reconciler.clarifications.(interface {
		GetPlanningValidation(context.Context, string, string) (collaboration.PlanRevalidation, error)
	})
	if !ok {
		return unresolvedReconciliation(), nil
	}
	v, err := ledger.GetPlanningValidation(ctx, scope.WorkflowID, mutation.AgentTurnID)
	if err != nil {
		return unresolvedReconciliation(), nil
	}
	var args completePlanningArguments
	if json.Unmarshal(mutation.Request, &args) != nil || !args.Resolved || v.SourceInvocationID != mutation.ID || v.ParticipantID != scope.Turn.AgentParticipantID || v.RevisionID != args.RevisionID || v.RepositoryCommit != args.Commit || v.Evidence != args.Evidence || scope.Role != "PLANNER" || mutation.ExternalService != "omnigrex" || mutation.ExternalResourceID != scope.WorkflowID {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	raw, _ := json.Marshal(map[string]any{"outcome": "PLANNING_READY", "revision_id": v.RevisionID, "repository_commit": v.RepositoryCommit})
	return foundReconciliation(raw), nil
}
