package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/store"
)

type ClarificationStore interface {
	HasPendingHumanInput(context.Context, string, string) (bool, error)
	RequestClarification(context.Context, store.AgentTurnLease, string, []collaboration.Question) (collaboration.Round, error)
	ConfirmClarificationPublication(context.Context, string, string, collaboration.Source) error
	GetClarificationRound(context.Context, string, string) (collaboration.Round, error)
	ResolveClarification(context.Context, store.AgentTurnLease, string, string, []collaboration.Resolution) error
}

func (backend *ProductionBackend) requestClarification(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	if backend.clarifications == nil {
		return nil, ErrToolDependency
	}
	var request struct {
		Introduction string                   `json:"introduction"`
		Questions    []collaboration.Question `json:"questions"`
		Signature    string                   `json:"signature"`
	}
	if json.Unmarshal(invocation.Arguments, &request) != nil || invocation.Scope.Role == role.Planner && len(request.Questions) > 5 {
		return nil, ErrInvalidInvocation
	}
	publisher := backend.clarificationDelivery(invocation)
	operation := collaboration.Publication{WorkflowID: invocation.Scope.WorkflowID, Publisher: publisher, SaveArtifact: backend.artifactSaver(invocation), Confirm: func(ctx context.Context, id string, source collaboration.Source) error {
		if err := backend.clarifications.ConfirmClarificationPublication(ctx, invocation.Scope.WorkflowID, id, source); err != nil {
			return OutcomeUnknown(err)
		}
		return nil
	}}
	round, err := operation.Publish(ctx, func(ctx context.Context) (collaboration.Round, error) {
		round, err := backend.clarifications.RequestClarification(ctx, invocation.lease, invocation.OperationID, request.Questions)
		if err != nil && !errors.Is(err, collaboration.ErrInvalid) {
			err = OutcomeUnknown(err)
		}
		return round, err
	}, collaboration.Round{ID: "R" + invocation.OperationID, Supersedes: "R00000000-0000-4000-8000-000000000000", Questions: request.Questions, Introduction: request.Introduction}, request.Signature)
	if err != nil {
		if errors.Is(err, collaboration.ErrInvalid) {
			return nil, ErrInvalidInvocation
		}
		return nil, classifyGitHubMutationError(err)
	}
	return json.Marshal(map[string]any{"outcome": "CLARIFICATION_WAIT", "round_id": round.ID, "reference": round.Reference})
}
func (backend *ProductionBackend) resolveClarification(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	if backend.clarifications == nil {
		return nil, ErrToolDependency
	}
	var request struct {
		RoundID     string                     `json:"round_id"`
		Resolutions []collaboration.Resolution `json:"resolutions"`
	}
	if json.Unmarshal(invocation.Arguments, &request) != nil {
		return nil, ErrInvalidInvocation
	}
	if err := backend.clarifications.ResolveClarification(ctx, invocation.lease, invocation.OperationID, request.RoundID, request.Resolutions); err != nil {
		if IsOutcomeUnknown(err) {
			return nil, err
		}
		var rejection *collaboration.ResolutionValidationError
		if errors.As(err, &rejection) {
			return resolutionRejectedResult(rejection)
		}
		if errors.Is(err, collaboration.ErrInvalid) {
			return nil, ErrInvalidInvocation
		}
		return nil, OutcomeUnknown(err)
	}
	return json.Marshal(map[string]any{"round_id": request.RoundID, "recorded": true})
}

func (backend *ProductionBackend) artifactSaver(in Invocation) func(context.Context, string, collaboration.RenderedPublication) error {
	return func(ctx context.Context, id string, artifact collaboration.RenderedPublication) error {
		ledger, ok := backend.clarifications.(interface {
			SaveCollaborationArtifact(context.Context, store.AgentTurnLease, string, collaboration.RenderedPublication) error
		})
		if !ok {
			return ErrToolDependency
		}
		if err := ledger.SaveCollaborationArtifact(ctx, in.lease, id, artifact); err != nil {
			return OutcomeUnknown(err)
		}
		return nil
	}
}

// Successful MCP transport is not successful resolution: retained Pi bridges
// pass ordinary result content unchanged but mask tool errors.
func resolutionRejectedResult(rejection *collaboration.ResolutionValidationError) (json.RawMessage, error) {
	// The transport allowlist stays server-side, including for retained bridges.
	reason := "invalid_resolution"
	switch rejection.Code {
	case "round_not_found", "round_not_published", "round_not_prepared", "duplicate_question", "missing_explanation_or_evidence", "question_not_in_round", "superseded_question_without_interpretation", "historical_change_requires_correction", "current_change_requires_correction", "response_not_valid_for_question", "unresolved_competing_evidence":
		reason = rejection.Code
	}
	resolutionIndex, responseIndex := rejection.ResolutionIndex, rejection.ResponseIndex
	if resolutionIndex < 0 || resolutionIndex > 50 {
		resolutionIndex = 0
	}
	if responseIndex < 0 || responseIndex > 100 {
		responseIndex = 0
	}
	return json.Marshal(map[string]any{"status": "rejected", "recorded": false, "application": "not_applied", "reason": reason, "resolution_index": resolutionIndex, "response_index": responseIndex, "next_action": "correct_arguments_new_operation", "message": "The entire resolution batch was not applied. Cite delivered evidence scoped to each question; an older answer to replaced questions is history, not automatic approval of revised questions. Use the governing current answer, retaining explicit correction relationships for historical changes. Correct the request under a fresh operation identity; no new human answer is needed if the delivered answer suffices."})
}
