//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestHandoffPublicationRecoveryAndReadScope(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 105)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const base = "0123456789abcdef0123456789abcdef01234567"
	const head = "1123456789abcdef0123456789abcdef01234567"
	const summary = "Fixed the boundary case. go test ./... passed."
	api := &replayGatewayGitHub{head: head, branch: "omnigrex/issue-105", base: "main", commentErr: context.DeadlineExceeded}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: replayGatewayCredentials{},
		Publisher: &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: head, Changed: true}}},
		Workflow:  mcp.LedgerWorkflowMutations{}, Handoffs: database,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, lease, execution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "handoff-recovery")
	gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
	callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish"}, false)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open", "title": "Changes", "body": "Summary"}, false)
	arguments := map[string]any{"operation_id": "handoff", "summary": summary}
	callReplayGatewayTool(t, gateway, registration, mcp.ToolRequestReview, arguments, true)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolRequestReview, arguments, true)
	if got, err := database.GetReviewHandoff(ctx, execution.WorkflowID, execution.Repository.ID, 654, head); err != nil || got != nil {
		t.Fatalf("unconfirmed handoff became readable: %#v, %v", got, err)
	}
	mutations, err := database.ListUnsettledMutations(ctx, lease)
	if err != nil || len(mutations) != 1 {
		t.Fatalf("unsettled handoff ledger = %#v, %v", mutations, err)
	}
	mutation := mutations[0]
	if err := gateway.CloseAndDrain(ctx, registration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: api, DeveloperCredentials: &integrationCredentialProvider{credential: "installation-token"}, ReviewerCredentials: &integrationCredentialProvider{credential: "installation-token"}})
	if err != nil {
		t.Fatal(err)
	}
	outcomeRequest := agentturn.OutcomeReconciliation{
		Lease: lease, Execution: execution, PromptResponse: &acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
		RepositoryCredential: "developer-token", Paths: committedPublicationPaths(t),
	}
	before, err := outcomes.Reconcile(ctx, outcomeRequest)
	if !errors.Is(err, store.ErrAgentTurnMutationsUnsettled) || before.Outcome == workflow.TurnOutcomeChangeProposalReady {
		t.Fatalf("unconfirmed publication admitted successful handoff: %#v, %v", before, err)
	}
	if mutation.State != store.MutationUnknown || mutation.ExternalService != "github" {
		t.Fatalf("handoff did not retain uncertain external effect: %#v", mutation)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{
		GitHub: api, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{},
	})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := reconciler.Reconcile(ctx, store.AgentTurnMutationReconciliationContext{
		WorkflowID: execution.WorkflowID, Repository: execution.Repository, Issue: execution.Issue,
		Role: execution.Assignment.Role, Turn: execution.Turn,
	}, mutation)
	if err != nil || proof.Disposition != mcp.ReconciliationFound {
		t.Fatalf("lost response was not recovered from the comment: %#v, %v", proof, err)
	}
	if err := database.BeginMutationReconciliation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, proof.Outcome.Result); err != nil {
		t.Fatal(err)
	}
	api.mutex.Lock()
	commentCount := len(api.comments)
	api.mutex.Unlock()
	if commentCount != 1 {
		t.Fatalf("lost response/replay duplicated the comment: %d", commentCount)
	}
	after, err := outcomes.Reconcile(ctx, outcomeRequest)
	if err != nil || after.Outcome != workflow.TurnOutcomeChangeProposalReady {
		t.Fatalf("confirmed publication cannot complete Stage: %#v, %v", after, err)
	}
	handoff, err := database.GetReviewHandoff(ctx, execution.WorkflowID, execution.Repository.ID, 654, head)
	if err != nil || handoff == nil || handoff.ID != mutation.ID || handoff.HeadSHA != head || handoff.Summary != summary || !handoff.PublicationConfirmed {
		t.Fatalf("completed handoff = %#v, %v", handoff, err)
	}
	for _, scope := range []struct {
		workflow string
		repo, pr int64
		head     string
	}{
		{"00000000-0000-4000-8000-000000000001", execution.Repository.ID, 654, head},
		{execution.WorkflowID, execution.Repository.ID + 1, 654, head},
		{execution.WorkflowID, execution.Repository.ID, 655, head},
		{execution.WorkflowID, execution.Repository.ID, 654, base},
	} {
		if got, err := database.GetReviewHandoff(ctx, scope.workflow, scope.repo, scope.pr, scope.head); err != nil || got != nil {
			t.Fatalf("handoff escaped its scope %#v: %#v, %v", scope, got, err)
		}
	}
}

type unusedHandoffPublications struct{}

func (unusedHandoffPublications) ReconcilePublication(context.Context, workspace.PublicationReconciliation) (workspace.PublicationReconciliationResult, error) {
	return workspace.PublicationReconciliationResult{}, errors.New("handoff reconciliation must not inspect publication workspaces")
}
