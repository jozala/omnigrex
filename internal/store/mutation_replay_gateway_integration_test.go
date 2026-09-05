//go:build integration

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestGatewayRestoresAncestorPublicationReplayForFreshOutcomeReconciliation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 95)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const (
		baseHead  = "0123456789abcdef0123456789abcdef01234567"
		firstHead = "1123456789abcdef0123456789abcdef01234567"
		finalHead = "2123456789abcdef0123456789abcdef01234567"
	)

	publisher := &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: firstHead, Changed: true}, {Head: finalHead, Changed: true}}}
	github := &replayGatewayGitHub{head: finalHead, branch: "omnigrex/issue-95", base: "main"}
	workflowMutations := &replayGatewayWorkflow{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: github, Credentials: replayGatewayCredentials{}, Publisher: publisher, Workflow: workflowMutations,
	})
	if err != nil {
		t.Fatal(err)
	}

	root, rootLease, rootExecution := acquireReplayGatewayTurn(t, database, fixture, store.AgentTurn{}, "replay-root")
	rootGateway, rootRegistration := openReplayGateway(t, database, backend, replayGatewayScope(rootLease, rootExecution, baseHead))
	callReplayGatewayTool(t, rootGateway, rootRegistration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1", "message": "First"}, false)
	callReplayGatewayTool(t, rootGateway, rootRegistration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-2", "message": "Second"}, false)
	callReplayGatewayTool(t, rootGateway, rootRegistration, mcp.ToolOpenPR, map[string]any{"operation_id": "open-1", "title": "Changes", "body": "Ready"}, false)
	if err := rootGateway.CloseAndDrain(ctx, rootRegistration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, rootLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "ACP prompt failed"}); err != nil {
		t.Fatal(err)
	}

	_, retryLease, retryExecution := acquireReplayGatewayTurn(t, database, fixture, root, "replay-retry")
	retryGateway, retryRegistration := openReplayGateway(t, database, backend, replayGatewayScope(retryLease, retryExecution, baseHead))
	callReplayGatewayTool(t, retryGateway, retryRegistration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1", "message": "First"}, false)
	callReplayGatewayTool(t, retryGateway, retryRegistration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-2", "message": "Second"}, false)
	callReplayGatewayTool(t, retryGateway, retryRegistration, mcp.ToolOpenPR, map[string]any{"operation_id": "open-1", "title": "Changes", "body": "Ready"}, false)
	callReplayGatewayTool(t, retryGateway, retryRegistration, mcp.ToolRequestReview, map[string]any{"operation_id": "request-review-1", "summary": "Ready"}, false)

	if publisher.callCount() != 2 || github.openCallCount() != 1 || workflowMutations.callCount() != 1 {
		t.Fatalf("external calls after replay = publishes %d, opens %d, review requests %d", publisher.callCount(), github.openCallCount(), workflowMutations.callCount())
	}
	if workflowMutations.last.HeadSHA != finalHead || workflowMutations.last.Scope.PullRequest == nil ||
		workflowMutations.last.Scope.PullRequest.ID != 654 || workflowMutations.last.Scope.PullRequest.Number != 23 {
		t.Fatalf("request_review restored scope = %#v", workflowMutations.last)
	}
	if err := retryGateway.CloseAndDrain(ctx, retryRegistration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	ledger, err := database.ListAgentTurnMutationInvocations(ctx, retryLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 4 {
		t.Fatalf("retry ledger = %#v", ledger)
	}
	for index, mutation := range ledger {
		if mutation.InvocationNumber != int64(index+1) {
			t.Fatalf("retry ledger order = %#v", ledger)
		}
	}
	if ledger[0].ExpectedSHA != baseHead || ledger[1].ExpectedSHA != firstHead || ledger[2].ExpectedSHA != finalHead || ledger[3].ExpectedSHA != finalHead {
		t.Fatalf("authoritative replay heads = %#v", ledger)
	}

	paths := workspace.Paths{Workspace: t.TempDir(), Publication: t.TempDir()}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: github})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reconciler.Reconcile(ctx, agentturn.OutcomeReconciliation{
		Lease: retryLease, Execution: retryExecution, PromptResponse: &acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
		RepositoryCredential: "developer-token", Paths: paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Outcome != workflow.TurnOutcomeChangeProposalReady || observation.Completion.Status != store.AgentTurnSucceeded ||
		observation.ChangeProposal == nil || observation.ChangeProposal.HeadSHA != finalHead || github.getCallCount() != 1 {
		t.Fatalf("fresh replay outcome = %#v, GitHub gets %d", observation, github.getCallCount())
	}
}

func TestGatewayMalformedAncestorReplayFailsClosedWithoutLedgerEvidence(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 96)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const head = "3123456789abcdef0123456789abcdef01234567"

	root, rootLease, rootExecution := acquireReplayGatewayTurn(t, database, fixture, store.AgentTurn{}, "malformed-root")
	spec := store.MutationSpec{
		OperationID: "malformed-publish", ToolName: mcp.ToolPublishChanges,
		Request:         json.RawMessage(`{"message":"Publish","operation_id":"malformed-publish"}`),
		ExternalService: "git", ExternalResourceID: fmt.Sprintf("%d:%s", rootExecution.Repository.ID, "omnigrex/issue-96"), ExpectedSHA: head,
	}
	mutation, err := database.ReserveMutation(ctx, rootLease, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, rootLease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, rootLease, mutation.ID, json.RawMessage(`{"credential":"credential-sentinel"}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, rootLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "prompt failed"}); err != nil {
		t.Fatal(err)
	}

	_, retryLease, retryExecution := acquireReplayGatewayTurn(t, database, fixture, root, "malformed-retry")
	publisher := &replayGatewayPublisher{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publisher: publisher, Workflow: &replayGatewayWorkflow{},
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(retryLease, retryExecution, head))
	body := callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "malformed-publish", "message": "Publish"}, true)
	if strings.Contains(body, "credential-sentinel") || !strings.Contains(body, "cached mutation replay failed") || publisher.callCount() != 0 {
		t.Fatalf("malformed replay response = %s, publisher calls %d", body, publisher.callCount())
	}
	var replayRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_invocation_replays WHERE agent_turn_id = $1`, retryLease.ID).Scan(&replayRows); err != nil {
		t.Fatal(err)
	}
	if replayRows != 0 {
		t.Fatalf("malformed result produced %d replay ledger rows", replayRows)
	}
}

func acquireReplayGatewayTurn(t *testing.T, database *store.Store, fixture agentFixture, retryOf store.AgentTurn, owner string) (store.AgentTurn, store.AgentTurnLease, store.AgentTurnExecutionContext) {
	t.Helper()
	spec := fixture.turnSpec()
	if retryOf.ID != "" {
		spec.Purpose = workflow.TurnPurposeRetry
		spec.RetryOfTurnID = retryOf.ID
	}
	turn, err := database.AllocateAgentTurn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := database.AcquireAgentTurn(context.Background(), claimAgentTurnJob(t, database, context.Background(), turn, time.Minute), turn.ControlRevision, owner, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	return turn, lease, execution
}

func replayGatewayScope(lease store.AgentTurnLease, execution store.AgentTurnExecutionContext, head string) mcp.TokenScope {
	return mcp.TokenScope{
		Lease: lease, WorkflowID: execution.WorkflowID, Role: workflow.RoleDeveloper,
		Repository: mcp.RepositoryScope{ID: execution.Repository.ID, Owner: execution.Repository.Owner, Name: execution.Repository.Name},
		Issue:      mcp.IssueScope{ID: execution.Issue.ID, Number: execution.Issue.Number},
		Branch:     "omnigrex/issue-" + fmt.Sprint(execution.Issue.Number), DefaultBranch: "main", HeadSHA: head,
		ExpiresAt: lease.LeaseExpiresAt.Add(-time.Second),
	}
}

func openReplayGateway(t *testing.T, database *store.Store, backend *mcp.ProductionBackend, scope mcp.TokenScope) (*mcp.Gateway, mcp.Registration) {
	t.Helper()
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: database, Backend: backend,
		Now: func() time.Time { return scope.Lease.CreatedAt.Add(time.Millisecond) }, MutationFenceCheckInterval: time.Hour,
		Random: bytes.NewReader(bytes.Repeat([]byte{byte(scope.Lease.ExecutionEpoch + 32)}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	replayGatewayRPC(t, gateway, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+mcp.ProtocolVersion+`","capabilities":{},"clientInfo":{"name":"integration","version":"1"}}}`)
	replayGatewayRPC(t, gateway, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	return gateway, registration
}

func callReplayGatewayTool(t *testing.T, gateway *mcp.Gateway, registration mcp.Registration, name string, arguments map[string]any, wantError bool) string {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":%s}`, params)
	response := replayGatewayRPC(t, gateway, registration, mcp.ProtocolVersion, body)
	hasError := strings.Contains(response, `"isError":true`)
	if hasError != wantError {
		t.Fatalf("tools/call %s response = %s", name, response)
	}
	return response
}

func replayGatewayRPC(t *testing.T, gateway *mcp.Gateway, registration mcp.Registration, protocol, body string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, registration.Server.URL, strings.NewReader(body))
	request.Header.Set("Authorization", registration.Server.Headers[0].Value)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	if protocol != "" {
		request.Header.Set("MCP-Protocol-Version", protocol)
	}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
		t.Fatalf("MCP response = %d %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

type replayGatewayCredentials struct{}

func (replayGatewayCredentials) DeveloperCredential(context.Context, mcp.RepositoryScope) (string, error) {
	return "developer-token", nil
}

func (replayGatewayCredentials) ReviewerCredential(context.Context, mcp.RepositoryScope) (string, error) {
	return "reviewer-token", nil
}

type replayGatewayPublisher struct {
	mutex   sync.Mutex
	results []workspace.PublicationResult
	calls   []workspace.Publication
}

func (publisher *replayGatewayPublisher) Publish(_ context.Context, publication workspace.Publication) (workspace.PublicationResult, error) {
	publisher.mutex.Lock()
	defer publisher.mutex.Unlock()
	publisher.calls = append(publisher.calls, publication)
	if len(publisher.results) == 0 {
		return workspace.PublicationResult{}, errors.New("unexpected publish")
	}
	result := publisher.results[0]
	publisher.results = publisher.results[1:]
	return result, nil
}

func (publisher *replayGatewayPublisher) callCount() int {
	publisher.mutex.Lock()
	defer publisher.mutex.Unlock()
	return len(publisher.calls)
}

type replayGatewayWorkflow struct {
	mutex sync.Mutex
	calls int
	last  mcp.RequestReviewMutation
}

func (workflowBackend *replayGatewayWorkflow) RequestReview(ctx context.Context, request mcp.RequestReviewMutation) (json.RawMessage, error) {
	workflowBackend.mutex.Lock()
	defer workflowBackend.mutex.Unlock()
	workflowBackend.calls++
	workflowBackend.last = request
	return mcp.LedgerWorkflowMutations{}.RequestReview(ctx, request)
}

func (*replayGatewayWorkflow) ReportBlocked(ctx context.Context, request mcp.ReportBlockedMutation) (json.RawMessage, error) {
	return mcp.LedgerWorkflowMutations{}.ReportBlocked(ctx, request)
}

func (workflowBackend *replayGatewayWorkflow) callCount() int {
	workflowBackend.mutex.Lock()
	defer workflowBackend.mutex.Unlock()
	return workflowBackend.calls
}

type replayGatewayGitHub struct {
	mutex     sync.Mutex
	head      string
	branch    string
	base      string
	openCalls int
	getCalls  int
}

func (github *replayGatewayGitHub) pullRequest() githubapi.PullRequest {
	return githubapi.PullRequest{
		ID: 654, NodeID: "PR_654", Number: 23, State: "open", HTMLURL: "https://github.com/owner/repo/pull/23",
		Head: githubapi.PullRequestBranch{Ref: github.branch, SHA: github.head, Label: "owner:" + github.branch},
		Base: githubapi.PullRequestBranch{Ref: github.base, SHA: "7123456789abcdef0123456789abcdef01234567", Label: "owner:" + github.base},
	}
}

func (*replayGatewayGitHub) GetIssue(context.Context, string, string, string, int) (githubapi.Issue, error) {
	return githubapi.Issue{}, errors.New("unexpected call")
}
func (*replayGatewayGitHub) ListIssueComments(context.Context, string, string, string, int) ([]githubapi.IssueComment, error) {
	return nil, errors.New("unexpected call")
}
func (github *replayGatewayGitHub) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.getCalls++
	return github.pullRequest(), nil
}
func (*replayGatewayGitHub) ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error) {
	return nil, errors.New("unexpected call")
}
func (*replayGatewayGitHub) ListPullRequestFiles(context.Context, string, string, string, int) ([]githubapi.PullRequestFile, error) {
	return nil, errors.New("unexpected call")
}
func (*replayGatewayGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	return nil, errors.New("unexpected call")
}
func (*replayGatewayGitHub) ListReviewThreads(context.Context, string, string, string, int) ([]githubapi.ReviewThread, error) {
	return nil, errors.New("unexpected call")
}
func (*replayGatewayGitHub) GetCheckRuns(context.Context, string, string, string, string) ([]githubapi.CheckRun, error) {
	return nil, errors.New("unexpected call")
}
func (github *replayGatewayGitHub) OpenPullRequest(context.Context, string, string, string, githubapi.OpenPullRequestRequest) (githubapi.PullRequest, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.openCalls++
	return github.pullRequest(), nil
}
func (*replayGatewayGitHub) CreateIssueComment(context.Context, string, string, string, int, githubapi.CommentRequest) (githubapi.IssueComment, error) {
	return githubapi.IssueComment{}, errors.New("unexpected call")
}
func (*replayGatewayGitHub) CreatePullRequestComment(context.Context, string, string, string, int, githubapi.CommentRequest) (githubapi.IssueComment, error) {
	return githubapi.IssueComment{}, errors.New("unexpected call")
}
func (*replayGatewayGitHub) SubmitReview(context.Context, string, string, string, int, githubapi.ReviewRequest) (githubapi.Review, error) {
	return githubapi.Review{}, errors.New("unexpected call")
}
func (github *replayGatewayGitHub) openCallCount() int {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	return github.openCalls
}
func (github *replayGatewayGitHub) getCallCount() int {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	return github.getCalls
}
