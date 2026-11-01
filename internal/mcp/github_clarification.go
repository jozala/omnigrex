package mcp

import (
	"context"
	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

// Default adapter composition is shared by normal execution and recovery. The
// collaboration application operation never renders or reads GitHub artifacts.
func (backend *ProductionBackend) clarificationDelivery(invocation Invocation) collaboration.Publisher {
	if backend.clarificationPublisher != nil {
		return backend.clarificationPublisher
	}
	return githubapi.ClarificationPublisher{API: backend.github,
		Credential: func(ctx context.Context) (string, error) {
			return backend.credential(ctx, ToolRequestClarification, invocation.Scope.Role, invocation.Scope.Repository)
		},
		Owner: invocation.Scope.Repository.Owner, Repository: invocation.Scope.Repository.Name, IssueNumber: int(invocation.Scope.Issue.Number),
		Marker: githubapi.Marker{WorkflowID: invocation.Scope.WorkflowID, AgentParticipantID: invocation.Scope.AgentParticipantID, OperationID: invocation.OperationID}}
}
func (reconciler *ProductionReconciler) clarificationDelivery(scope store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (collaboration.Publisher, error) {
	if reconciler.clarificationPublisher != nil {
		return reconciler.clarificationPublisher, nil
	}
	if !(mutation.ExternalService == "omnigrex" && mutation.ExternalResourceID == scope.WorkflowID) && !(mutation.ExternalService == "github" && exactNumericResource(mutation.ExternalResourceID, scope.Repository.ID, scope.Issue.ID)) {
		return nil, ErrInvalidMutationReconciliation
	}
	return githubapi.ClarificationPublisher{API: reconciler.github,
		Credential: func(ctx context.Context) (string, error) {
			return reconciler.credentialForTool(ctx, scope.Role, mutation.ToolName, scope.Repository)
		},
		Owner: scope.Repository.Owner, Repository: scope.Repository.Name, IssueNumber: int(scope.Issue.Number),
		Marker: githubapi.Marker{WorkflowID: scope.WorkflowID, AgentParticipantID: scope.Turn.AgentParticipantID, OperationID: mutation.ID}}, nil
}
