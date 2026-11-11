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
	op := collaboration.Publication{WorkflowID: in.Scope.WorkflowID, Publisher: backend.clarificationDelivery(in), SaveArtifact: backend.artifactSaver(in), RejectCheckpoint: func(ctx context.Context, id, reason string) error {
		ledger, ok := backend.clarifications.(interface {
			RejectPlanningPublication(context.Context, store.AgentTurnLease, string, string) error
		})
		if !ok {
			return ErrToolDependency
		}
		if err := ledger.RejectPlanningPublication(ctx, in.lease, id, reason); err != nil {
			return OutcomeUnknown(err)
		}
		return nil
	}, Confirm: func(ctx context.Context, id string, source collaboration.Source) error {
		if err := ledger.ConfirmPlanningPublication(ctx, in.Scope.WorkflowID, id, source); err != nil {
			return OutcomeUnknown(err)
		}
		return nil
	}}
	r, err := op.PublishCheckpoint(ctx, func(ctx context.Context) (collaboration.PlanningRevision, error) {
		r, err := ledger.ReservePlanningCheckpoint(ctx, in.lease, in.OperationID, args.Content)
		var rejection *collaboration.CheckpointNotApplied
		if err != nil && !errors.Is(err, collaboration.ErrInvalid) && !errors.As(err, &rejection) {
			err = OutcomeUnknown(err)
		}
		return r, err
	}, args.Signature)
	if IsOutcomeUnknown(err) {
		return nil, err
	}
	var rejection *collaboration.CheckpointNotApplied
	if errors.As(err, &rejection) {
		return planningNonPublicationResult(rejection)
	}
	if errors.Is(err, collaboration.ErrInvalid) {
		return nil, ErrInvalidInvocation
	}
	if err != nil {
		return nil, classifyGitHubMutationError(err)
	}
	return planningCheckpointResult(r)
}
func planningNonPublicationResult(r *collaboration.CheckpointNotApplied) (json.RawMessage, error) {
	if r == nil || (r.Reason != "late_input" && r.Reason != "unread_input" && r.Reason != "interrupted_before_reservation" && r.Reason != "payload_too_large" && r.Reason != "interrupted_before_publication" && r.Reason != "historical_decisions_unresolved") {
		return nil, ErrInvalidInvocation
	}
	action, instruction := "request_clarification_empty_questions", "Call request_clarification with empty questions and a fresh operation ID, then end this Turn without report_blocked. The orchestrator will deliver pending input in the retained Session's next Turn; interpret it and reconsider affected decisions before publishing revised content with a new operation ID."
	status := "input_pending"
	if r.Reason == "unread_input" {
		action, instruction = "retrieve_human_input", "Complete every scoped human_input record and chunk required by the frozen projection before dependent action. This operation remains not applied; a new publication attempt needs a new operation ID."
	}
	if r.Reason == "interrupted_before_reservation" || r.Reason == "interrupted_before_publication" {
		status, action, instruction = "not_applied", "publish_checkpoint_new_operation", "Recovery proved publication never reached the external effect after fencing and stopping the old Runtime Process. Reconsider current input and use a new operation ID for a fresh attempt."
	}
	if r.Reason == "payload_too_large" {
		status, action, instruction = "not_applied", "rewrite_complete_pair", "The complete rendered pair, including signature and marker, exceeds the single-comment limit. Rewrite both complete documents more concisely without dropping requirements or material commitments, then publish with a new operation ID. No comment was posted; this rejected operation will not publish on replay."
	}
	result := map[string]any{"status": status, "publication": "not_applied", "reason": r.Reason, "next_action": action, "instructions": instruction}
	if r.Reason == "historical_decisions_unresolved" {
		result["status"] = "not_applied"
		result["next_action"] = "reconcile_historical_decisions"
		result["decisions"] = r.Decisions
		result["instructions"] = "No pair was created or published. Read the scoped historical rounds and input evidence. Explicitly reconcile every affected interpretation using resolve_clarification, including reaffirming unchanged decisions against original/correction evidence. Current-round resolutions and input assessments alone do not clear historical decisions. Preserve correction links and history; do not ask humans to repeat settled answers. If evidence conflicts, keep uncertainty and ask for reconciliation. Replay returns this rejection unchanged; after reconciliation publish the complete pair with a new operation ID. Unknown outcomes still require exact-operation recovery."
	}
	return json.Marshal(result)
}
func planningCheckpointResult(r collaboration.PlanningRevision) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"revision_id": r.ID, "reference": r.Reference, "source": r.Source, "status": r.Content.Status})
}
func (reconciler *ProductionReconciler) reconcilePlanningCheckpoint(ctx context.Context, scope store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	if scope.Role != "PLANNER" || mutation.ExternalService != "omnigrex" || mutation.ExternalResourceID != scope.WorkflowID {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	ledger, ok := reconciler.clarifications.(PlanningStore)
	if !ok {
		return unresolvedReconciliation(), nil
	}
	if evidence, ok := ledger.(interface {
		ReconcilePlanningNonPublication(context.Context, string, string, string) (*collaboration.CheckpointNotApplied, error)
	}); ok {
		rejection, err := evidence.ReconcilePlanningNonPublication(ctx, scope.WorkflowID, mutation.AgentTurnID, mutation.ID)
		if err != nil {
			return unresolvedReconciliation(), err
		}
		if rejection != nil {
			var args struct {
				Content collaboration.PlanningContent `json:"content"`
			}
			if json.Unmarshal(mutation.Request, &args) != nil || rejection.OperationID != "P"+mutation.ID || rejection.TurnID != mutation.AgentTurnID || rejection.ParticipantID != scope.Turn.AgentParticipantID {
				return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
			}
			expected, _ := json.Marshal(args.Content)
			actual, _ := json.Marshal(rejection.Content)
			if string(expected) != string(actual) {
				return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
			}
			raw, err := planningNonPublicationResult(rejection)
			if err != nil {
				return unresolvedReconciliation(), err
			}
			return foundReconciliation(raw), nil
		}
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
