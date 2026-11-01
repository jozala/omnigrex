package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

type ClarificationStore interface {
	HasPendingHumanInput(context.Context, string, string) (bool, error)
	RequestClarification(context.Context, store.AgentTurnLease, string, []collaboration.Question) (collaboration.Round, error)
	ConfirmClarificationPublication(context.Context, string, string, collaboration.Source) error
	GetClarificationRound(context.Context, string, string) (collaboration.Round, error)
	ResolveClarification(context.Context, store.AgentTurnLease, string, []collaboration.Resolution) error
}

func clarificationBody(round collaboration.Round) string {
	var body strings.Builder
	fmt.Fprintf(&body, "## Clarification %s\n\n", round.ID)
	if round.Supersedes != "" {
		fmt.Fprintf(&body, "Supersedes round `%s`; earlier accepted responses remain in history.\n\n", round.Supersedes)
	}
	for _, question := range round.Questions {
		fmt.Fprintf(&body, "- **%s**: %s\n", question.ID, question.Body)
	}
	fmt.Fprintf(&body, "\nReply with `/omnigrex answer %s` on the first nonblank line of a new Issue comment, followed by your complete or partial answer. Repository write permission is required. Unanswered questions remain unresolved. This is expected waiting, not a Human Handoff; do not add omnigrex:run. Answers do not authorize implementation.\n", round.ID)
	return body.String()
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
	if len(request.Questions) > 0 {
		provisional := collaboration.Round{ID: "R" + invocation.OperationID, Supersedes: "R00000000-0000-4000-8000-000000000000", Questions: request.Questions}
		body := clarificationBody(provisional)
		if !markerFree(body) {
			return nil, ErrInvalidInvocation
		}
		marker, err := operationMarker(invocation)
		if err != nil {
			return nil, err
		}
		if err = checkSignedBodyLength(githubapi.AppendSignature(body, request.Signature), marker); err != nil {
			return nil, err
		}
	}
	round, err := backend.clarifications.RequestClarification(ctx, invocation.lease, invocation.OperationID, request.Questions)
	if err != nil {
		if errors.Is(err, collaboration.ErrInvalid) {
			return nil, ErrInvalidInvocation
		}
		return nil, OutcomeUnknown(err)
	}
	if round.ID == "" {
		return json.Marshal(map[string]any{"outcome": "CLARIFICATION_WAIT", "round_id": ""})
	}
	credential, err := backend.credential(ctx, ToolRequestClarification, invocation.Scope.Role, invocation.Scope.Repository)
	if err != nil {
		return nil, err
	}
	invocation.Arguments, _ = json.Marshal(map[string]any{"body": clarificationBody(round), "signature": request.Signature})
	result, err := backend.createIssueComment(ctx, invocation, credential, false)
	if err != nil {
		return nil, err
	}
	var comment struct {
		ID      int64  `json:"comment_id"`
		HTMLURL string `json:"html_url"`
	}
	if json.Unmarshal(result, &comment) != nil || comment.ID <= 0 {
		return nil, OutcomeUnknown(ErrToolPrecondition)
	}
	if err = backend.clarifications.ConfirmClarificationPublication(ctx, invocation.Scope.WorkflowID, round.ID, collaboration.Source{Channel: "github", ID: fmt.Sprint(comment.ID), URL: comment.HTMLURL}); err != nil {
		return nil, OutcomeUnknown(err)
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
