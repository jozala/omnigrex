package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
)

type ClarificationStore interface {
	HasPendingHumanInput(context.Context, string, string) (bool, error)
	RequestClarification(context.Context, store.AgentTurnLease, string, []collaboration.Question) (collaboration.Round, error)
	ConfirmClarificationPublication(context.Context, string, string, collaboration.Source) error
	GetClarificationRound(context.Context, string, string) (collaboration.Round, error)
	ResolveClarification(context.Context, store.AgentTurnLease, string, []collaboration.Resolution) error
}

func (backend *ProductionBackend) requestClarification(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	if backend.clarifications == nil {
		return nil, ErrToolDependency
	}
	var request struct {
		Questions []collaboration.Question `json:"questions"`
		Signature string                   `json:"signature"`
	}
	if json.Unmarshal(invocation.Arguments, &request) != nil {
		return nil, ErrInvalidInvocation
	}
	publisher := backend.clarificationDelivery(invocation)
	operation := collaboration.Publication{WorkflowID: invocation.Scope.WorkflowID, Publisher: publisher, Confirm: func(ctx context.Context, id string, source collaboration.Source) error {
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
	}, collaboration.Round{ID: "R" + invocation.OperationID, Supersedes: "R00000000-0000-4000-8000-000000000000", Questions: request.Questions}, request.Signature)
	if err != nil {
		if errors.Is(err, collaboration.ErrInvalid) {
			return nil, ErrInvalidInvocation
		}
		return nil, classifyGitHubMutationError(err)
	}
	return json.Marshal(map[string]any{"outcome": "CLARIFICATION_WAIT", "round_id": round.ID})
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
	if err := backend.clarifications.ResolveClarification(ctx, invocation.lease, request.RoundID, request.Resolutions); err != nil {
		if errors.Is(err, collaboration.ErrInvalid) {
			return nil, ErrInvalidInvocation
		}
		return nil, OutcomeUnknown(err)
	}
	return json.Marshal(map[string]any{"round_id": request.RoundID, "recorded": true})
}
