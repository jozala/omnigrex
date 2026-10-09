//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestFailedTurnPublicationBindsNewSessionOfSameParticipant(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 196)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const base = "0123456789abcdef0123456789abcdef01234567"
	const head = "1123456789abcdef0123456789abcdef01234567"
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub:      &replayGatewayGitHub{head: head, branch: "omnigrex/issue-196", base: "main"},
		Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: head, Changed: true}}},
		Workflow: &replayGatewayWorkflow{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, lease, execution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "publication-root")
	gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
	callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1", "message": "Fix"}, false)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open-1", "title": "Fix", "body": "Ready"}, false)
	if err := gateway.CloseAndDrain(ctx, registration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "prompt ended"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'DELETED', state_deleted_at = clock_timestamp() WHERE id = $1`, fixture.sessionID); err != nil {
		t.Fatal(err)
	}
	const replacementSession = "20000000-0000-4000-8000-000000000196"
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name, runtime_profile_version,
    runtime_profile_content_sha256, runtime_image_digest, runtime_state_path, status
)
SELECT $1, agent_assignment_id, session_number + 1, 'new-acp-session', runtime_profile_name, runtime_profile_version,
       runtime_profile_content_sha256, runtime_image_digest, runtime_state_path, 'ACTIVE'
FROM agent_sessions WHERE id = $2`, replacementSession, fixture.sessionID); err != nil {
		t.Fatal(err)
	}
	fixture.sessionID = replacementSession
	_, retryLease, retryExecution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "publication-new-session")
	mutations, err := database.ListParticipantPublicationMutations(ctx, retryLease)
	if err != nil || len(mutations) != 2 || mutations[0].State != store.MutationSucceeded || mutations[0].ProposedSHA != head || mutations[1].State != store.MutationSucceeded {
		t.Fatalf("prior publication evidence = %#v, %v", mutations, err)
	}
	binding := store.AgentTurnPublication{
		HeadRef: fmt.Sprintf("omnigrex/issue-%d", retryExecution.Issue.Number), HeadSHA: head, BaseRef: "main",
		PullRequestID: 654, PullRequestNumber: 23, PullRequestNodeID: "PR_654",
		SourcePublishMutationID: mutations[0].ID, SourceOpenPRMutationID: mutations[1].ID,
	}
	partial := binding
	partial.PullRequestNumber, partial.PullRequestNodeID = 0, ""
	if err := database.BindAgentTurnPublication(ctx, retryLease, partial); err != store.ErrAgentTurnPublicationConflict {
		t.Fatalf("partial PR binding = %v, want conflict", err)
	}
	var partialRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turn_publications WHERE agent_turn_id = $1`, retryLease.ID).Scan(&partialRows); err != nil || partialRows != 0 {
		t.Fatalf("partial PR binding persisted %d rows: %v", partialRows, err)
	}
	if err := database.BindAgentTurnPublication(ctx, retryLease, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turn_publications SET pull_request_number = NULL, pull_request_node_id = NULL WHERE agent_turn_id = $1`, retryLease.ID); err == nil {
		t.Fatal("SQL accepted a partial PR binding without number or node ID")
	}
	if err := database.BindAgentTurnPublication(ctx, retryLease, binding); err != nil {
		t.Fatalf("idempotent publication binding: %v", err)
	}
	recovered, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil || recovered.Publication == nil || *recovered.Publication != binding || recovered.ChangeProposal != nil {
		t.Fatalf("recovered turn = %#v, error %v", recovered, err)
	}
	binding.HeadSHA = base
	if err := database.BindAgentTurnPublication(ctx, retryLease, binding); err != store.ErrAgentTurnPublicationConflict {
		t.Fatalf("conflicting publication binding = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turn_publications SET head_sha = repeat('a', 64) WHERE agent_turn_id = $1`, retryLease.ID); err != nil {
		t.Fatalf("schema rejected valid SHA-256 publication head: %v", err)
	}
}

func TestRecoveredPRRequestsReviewWithoutOpeningDuplicate(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 197)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	const base = "0123456789abcdef0123456789abcdef01234567"
	const head = "1123456789abcdef0123456789abcdef01234567"
	github := &recoveredPRGitHub{replayGatewayGitHub: &replayGatewayGitHub{head: head, branch: "omnigrex/issue-197", base: "main"}}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: github, Credentials: replayGatewayCredentials{},
		Publisher: &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: head, Changed: true}}},
		Workflow:  mcp.LedgerWorkflowMutations{},
	})
	if err != nil {
		t.Fatal(err)
	}
	root, lease, execution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "publication-root")
	gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
	callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1", "message": "Fix"}, false)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open-1", "title": "Fix", "body": "Ready"}, false)
	if err := gateway.CloseAndDrain(ctx, registration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "prompt ended"}); err != nil {
		t.Fatal(err)
	}
	_, nextLease, nextExecution := acquireReplayGatewayTurn(t, database, pool, fixture, root, "publication-retry")
	remote := &recoveredPRRemote{head: head}
	if err := agentturn.NewPublicationRecovery(database, remote, github).Recover(ctx, nextLease, nextExecution,
		"https://github.com/owner/repo.git", "developer-token", "main"); err != nil {
		t.Fatal(err)
	}
	bound, err := database.GetAgentTurnExecutionContext(ctx, nextLease)
	if err != nil || bound.Publication == nil || bound.Publication.PullRequestID != 654 || bound.Publication.HeadSHA != head {
		t.Fatalf("bound next Turn = %#v, error %v", bound, err)
	}
	scope := replayGatewayScope(nextLease, bound, head)
	scope.PullRequest = &mcp.PullRequestScope{ID: bound.Publication.PullRequestID, Number: bound.Publication.PullRequestNumber}
	scope.BranchExists = true
	nextGateway, nextRegistration := openReplayGateway(t, database, backend, scope)
	callReplayGatewayTool(t, nextGateway, nextRegistration, mcp.ToolRequestReview,
		map[string]any{"operation_id": "request-review-after-recovery", "summary": "Ready"}, false)
	if err := nextGateway.CloseAndDrain(ctx, nextRegistration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, nextLease); err != nil {
		t.Fatal(err)
	}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: github, DeveloperCredentials: &integrationCredentialProvider{credential: "installation-token"}, ReviewerCredentials: &integrationCredentialProvider{credential: "installation-token"}})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reconciler.Reconcile(ctx, agentturn.OutcomeReconciliation{
		Lease: nextLease, Execution: bound, PromptResponse: &acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
		RepositoryCredential: "developer-token", Paths: committedPublicationPaths(t),
	})
	if err != nil || observation.Outcome != workflow.TurnOutcomeChangeProposalReady || observation.Completion.Status != store.AgentTurnSucceeded {
		t.Fatalf("recovered PR handoff = %#v, error %v", observation, err)
	}
	settled, err := database.SettleAgentTurn(ctx, nextLease, observation)
	if err != nil || settled.State != workflow.StateReviewing || settled.ChangeProposalID == "" || github.openCallCount() != 1 {
		t.Fatalf("recovered PR settlement = %#v, error %v, PR opens %d", settled, err, github.openCallCount())
	}
}

func TestPublicationConflictCanHandOffBeforeRuntimeAndMutationAdmission(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 198)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	_, lease, _ := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "publication-conflict")
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, lease, store.AgentTurnSettlementObservation{
		ObservedAt: time.Now().UTC(), Outcome: workflow.TurnOutcomeBlocked,
		Diagnostic: "publication_conflict: unproven Pull Request",
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded},
	})
	if err != nil || settled.State != workflow.StateNeedsHuman || settled.TerminalStatus != store.AgentTurnSucceeded {
		t.Fatalf("pre-launch publication conflict settlement = %#v, error %v", settled, err)
	}
}

type recoveredPRRemote struct{ head string }

func (remote *recoveredPRRemote) ObserveRemoteBranch(context.Context, string, string, string) (string, error) {
	return remote.head, nil
}

func (remote *recoveredPRRemote) ReconcilePublication(_ context.Context, _ workspace.PublicationReconciliation) (workspace.PublicationReconciliationResult, error) {
	return workspace.PublicationReconciliationResult{Outcome: workspace.PublicationReconciliationFound, Head: remote.head}, nil
}

func (*recoveredPRRemote) PrepareRecoveredPublication(context.Context, string, string, string, string) error {
	return nil
}

type recoveredPRGitHub struct {
	*replayGatewayGitHub
	pr githubapi.PullRequest
}

func (github *recoveredPRGitHub) OpenPullRequest(_ context.Context, _, _, _ string, request githubapi.OpenPullRequestRequest) (githubapi.PullRequest, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.openCalls++
	github.pr = github.pullRequest()
	github.pr.Title = request.Title
	github.pr.Body = githubapi.JoinBodyParts(request.Body, fmt.Sprintf("Closes #%d", request.IssueNumber), request.Marker)
	return github.pr, nil
}

func (github *recoveredPRGitHub) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	return github.pr, nil
}

func (github *recoveredPRGitHub) ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error) {
	return []githubapi.PullRequest{github.pr}, nil
}
