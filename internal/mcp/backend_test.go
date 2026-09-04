package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestProductionBackendSelectsReviewerCredentialsByOperationPermission(t *testing.T) {
	api := &backendGitHub{}
	credentials := &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: credentials, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	scope := productionToolScope(workflow.RoleReviewer)
	tools := []string{
		mcp.ToolGetIssue, mcp.ToolListIssueComments, mcp.ToolGetPullRequest,
		mcp.ToolListPullRequestReviews, mcp.ToolListReviewThreads, mcp.ToolGetCheckRuns,
	}
	for _, name := range tools {
		result, err := backend.Execute(context.Background(), mcp.Invocation{Name: name, Arguments: json.RawMessage(`{}`), Scope: scope, Class: mcp.ReadTool})
		if err != nil || !json.Valid(result) {
			t.Fatalf("Execute(%s) = %s, %v", name, result, err)
		}
	}
	wantCalls := []backendGitHubCall{
		{name: mcp.ToolGetIssue, credential: "developer-secret", owner: "acme", repository: "widgets", number: 12},
		{name: mcp.ToolListIssueComments, credential: "developer-secret", owner: "acme", repository: "widgets", number: 12},
		{name: mcp.ToolGetPullRequest, credential: "reviewer-secret", owner: "acme", repository: "widgets", number: 23},
		{name: mcp.ToolListPullRequestReviews, credential: "reviewer-secret", owner: "acme", repository: "widgets", number: 23},
		{name: mcp.ToolListReviewThreads, credential: "reviewer-secret", owner: "acme", repository: "widgets", number: 23},
		{name: mcp.ToolGetCheckRuns, credential: "developer-secret", owner: "acme", repository: "widgets", head: productionHeadSHA},
	}
	if !reflect.DeepEqual(api.calls, wantCalls) {
		t.Fatalf("GitHub read calls = %#v, want %#v", api.calls, wantCalls)
	}
	if credentials.developerCalls != 3 || credentials.reviewerCalls != 3 {
		t.Fatalf("credential calls = developer %d, reviewer %d", credentials.developerCalls, credentials.reviewerCalls)
	}
}

func TestProductionBackendUsesDeveloperCredentialsForAllComments(t *testing.T) {
	api := &backendGitHub{}
	credentials := &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: credentials, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	scope := productionToolScope(workflow.RoleReviewer)
	invocations := []mcp.Invocation{
		{Name: mcp.ToolCommentOnIssue, Arguments: json.RawMessage(`{"operation_id":"issue-comment","body":"Issue"}`), Scope: scope, Class: mcp.MutationTool, OperationID: "issue-comment"},
		{Name: mcp.ToolCommentOnPullRequest, Arguments: json.RawMessage(`{"operation_id":"pr-comment","body":"Pull Request"}`), Scope: scope, Class: mcp.MutationTool, OperationID: "pr-comment"},
	}
	for _, invocation := range invocations {
		if _, err := backend.Execute(context.Background(), invocation); err != nil {
			t.Fatalf("Execute(%s) error = %v", invocation.Name, err)
		}
	}
	if api.issueCommentCredential != "developer-secret" || api.pullRequestCommentCredential != "developer-secret" {
		t.Fatalf("comment credentials = Issue %q, Pull Request %q", api.issueCommentCredential, api.pullRequestCommentCredential)
	}
	if credentials.developerCalls != 2 || credentials.reviewerCalls != 0 {
		t.Fatalf("credential calls = developer %d, reviewer %d", credentials.developerCalls, credentials.reviewerCalls)
	}
}

func TestProductionBackendPublishesOrderedDeterministicCommits(t *testing.T) {
	firstHead := "1123456789abcdef0123456789abcdef01234567"
	secondHead := "2123456789abcdef0123456789abcdef01234567"
	publisher := &backendPublisher{results: []workspace.PublicationResult{{Head: firstHead, Changed: true}, {Head: secondHead, Changed: true}}}
	credentials := &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &backendGitHub{}, Credentials: credentials, Publisher: publisher, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	scope := productionToolScope(workflow.RoleDeveloper)
	for index, operationID := range []string{"publish-1", "publish-2"} {
		arguments := json.RawMessage(`{"operation_id":"` + operationID + `","message":"Publish changes"}`)
		result, err := backend.Execute(context.Background(), mcp.Invocation{
			Name: mcp.ToolPublishChanges, Arguments: arguments, Scope: scope, Class: mcp.MutationTool, OperationID: operationID,
		})
		if err != nil || !json.Valid(result) {
			t.Fatalf("publish %d = %s, %v", index+1, result, err)
		}
	}
	if len(publisher.publications) != 2 {
		t.Fatalf("publications = %#v", publisher.publications)
	}
	first := publisher.publications[0]
	second := publisher.publications[1]
	if first.RepositoryURL != "https://github.com/acme/widgets.git" || first.BaseRevision != productionHeadSHA || first.ExpectedOldHead != productionHeadSHA ||
		first.Branch != scope.Branch || first.Credential != "developer-secret" || first.Time != scope.TurnCreatedAt ||
		first.Message != "Publish changes\n\nOmnigrex-Operation-ID: publish-1" ||
		first.Identity != (workspace.CommitIdentity{Name: "Omnigrex Developer", Email: "developer@omnigrex.invalid"}) {
		t.Fatalf("first publication = %#v", first)
	}
	if second.BaseRevision != firstHead || second.ExpectedOldHead != firstHead || second.Time != first.Time || second.RepositoryURL != first.RepositoryURL {
		t.Fatalf("second publication = %#v", second)
	}
	if credentials.developerCalls != 2 || credentials.reviewerCalls != 0 {
		t.Fatalf("credential calls = developer %d, reviewer %d", credentials.developerCalls, credentials.reviewerCalls)
	}
}

func TestProductionBackendReleaseTurnRetiresOnlyExactTurnState(t *testing.T) {
	publisher := &backendPublisher{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &backendGitHub{}, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: publisher, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	releaser, ok := any(backend).(interface{ ReleaseTurn(mcp.ToolScope) })
	if !ok {
		t.Fatal("ProductionBackend does not release turn-scoped state")
	}

	retained := productionToolScope(workflow.RoleDeveloper)
	retained.ExecutionEpoch++
	retained.AgentTurnID = "turn-retained"
	for index, scope := range []mcp.ToolScope{productionToolScope(workflow.RoleDeveloper), retained} {
		head := fmt.Sprintf("%040x", index+1)
		publisher.results = append(publisher.results, workspace.PublicationResult{Head: head, Changed: true})
		operationID := fmt.Sprintf("publish-%d", index)
		invocation := mcp.Invocation{
			Name: mcp.ToolPublishChanges, Arguments: json.RawMessage(`{"operation_id":"` + operationID + `","message":"Publish"}`),
			Scope: scope, Class: mcp.MutationTool, OperationID: operationID,
		}
		if _, err := backend.PlanMutation(context.Background(), invocation); err != nil {
			t.Fatalf("PlanMutation(%d) error = %v", index, err)
		}
		if _, err := backend.Execute(context.Background(), invocation); err != nil {
			t.Fatalf("Execute(%d) error = %v", index, err)
		}
	}

	releaser.ReleaseTurn(productionToolScope(workflow.RoleDeveloper))
	assertProductionBackendStateSizes(t, backend, 1, 1, 1)
	releaser.ReleaseTurn(retained)
	assertProductionBackendStateSizes(t, backend, 0, 0, 0)

	for index := 0; index < 100; index++ {
		scope := productionToolScope(workflow.RoleDeveloper)
		scope.PullRequest = nil
		scope.AgentTurnID = fmt.Sprintf("turn-churn-%d", index)
		scope.ExecutionEpoch = int64(index + 10)
		operationID := fmt.Sprintf("plan-%d", index)
		invocation := mcp.Invocation{
			Name: mcp.ToolOpenPR, Arguments: json.RawMessage(`{"operation_id":"` + operationID + `","title":"Open","body":"Body"}`),
			Scope: scope, Class: mcp.MutationTool, OperationID: operationID,
		}
		if _, err := backend.PlanMutation(context.Background(), invocation); err != nil {
			t.Fatalf("PlanMutation churn %d error = %v", index, err)
		}
		releaser.ReleaseTurn(scope)
	}
	assertProductionBackendStateSizes(t, backend, 0, 0, 0)
}

func assertProductionBackendStateSizes(t *testing.T, backend *mcp.ProductionBackend, locks, heads, plans int) {
	t.Helper()
	value := reflect.ValueOf(backend).Elem()
	gotLocks := value.FieldByName("publicationLocks").Len()
	gotHeads := value.FieldByName("publishedHeads").Len()
	gotPlans := value.FieldByName("plannedHeads").Len()
	if gotLocks != locks || gotHeads != heads || gotPlans != plans {
		t.Fatalf("backend state sizes = locks %d, heads %d, plans %d; want %d, %d, %d", gotLocks, gotHeads, gotPlans, locks, heads, plans)
	}
}

func TestProductionBackendPerformsMarkedGitHubMutationsAndCommitBoundReview(t *testing.T) {
	api := &backendGitHub{}
	credentials := &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: credentials, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	developer := productionToolScope(workflow.RoleDeveloper)
	initial := developer
	initial.PullRequest = nil
	invocations := []mcp.Invocation{
		{Name: mcp.ToolOpenPR, Arguments: json.RawMessage(`{"operation_id":"open-1","title":"Implement it","body":"Description"}`), Scope: initial, Class: mcp.MutationTool, OperationID: "open-1"},
		{Name: mcp.ToolCommentOnIssue, Arguments: json.RawMessage(`{"operation_id":"issue-comment-1","body":"Issue update"}`), Scope: developer, Class: mcp.MutationTool, OperationID: "issue-comment-1"},
		{Name: mcp.ToolCommentOnPullRequest, Arguments: json.RawMessage(`{"operation_id":"pr-comment-1","body":"PR update"}`), Scope: developer, Class: mcp.MutationTool, OperationID: "pr-comment-1"},
	}
	for _, invocation := range invocations {
		result, err := backend.Execute(context.Background(), invocation)
		if err != nil || !json.Valid(result) {
			t.Fatalf("Execute(%s) = %s, %v", invocation.Name, result, err)
		}
	}
	reviewer := productionToolScope(workflow.RoleReviewer)
	reviewArguments := json.RawMessage(`{"operation_id":"review-1","event":"REQUEST_CHANGES","body":"Fix this","comments":[{"path":"internal/mcp/backend.go","line":53,"side":"RIGHT","start_line":50,"start_side":"RIGHT","body":"Validate this range"}]}`)
	result, err := backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolSubmitReview, Arguments: reviewArguments, Scope: reviewer, Class: mcp.MutationTool, OperationID: "review-1",
	})
	if err != nil || !json.Valid(result) {
		t.Fatalf("Execute(submit_review) = %s, %v", result, err)
	}

	wantOpenMarker, _ := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: "open-1"})
	if api.openCredential != "developer-secret" || api.openRequest.Title != "Implement it" || api.openRequest.Body != "Description" ||
		api.openRequest.Head != initial.Branch || api.openRequest.Base != initial.DefaultBranch || api.openRequest.IssueNumber != 12 || api.openRequest.Marker != wantOpenMarker {
		t.Fatalf("open Pull Request call = credential %q, request %#v", api.openCredential, api.openRequest)
	}
	wantIssueMarker, _ := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: "issue-comment-1"})
	wantPRMarker, _ := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: "pr-comment-1"})
	if api.issueCommentNumber != 12 || api.issueCommentRequest.Marker != wantIssueMarker || api.issueCommentCredential != "developer-secret" {
		t.Fatalf("Issue comment call = number %d, credential %q, request %#v", api.issueCommentNumber, api.issueCommentCredential, api.issueCommentRequest)
	}
	if api.pullRequestCommentNumber != 23 || api.pullRequestCommentRequest.Marker != wantPRMarker || api.pullRequestCommentCredential != "developer-secret" {
		t.Fatalf("Pull Request comment call = number %d, credential %q, request %#v", api.pullRequestCommentNumber, api.pullRequestCommentCredential, api.pullRequestCommentRequest)
	}
	wantReviewMarker, _ := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: "review-1"})
	if api.reviewCredential != "reviewer-secret" || api.reviewNumber != 23 || api.reviewRequest.CommitID != productionHeadSHA || api.reviewRequest.Body != "Fix this\n\n"+wantReviewMarker ||
		api.reviewRequest.Event != githubapi.ReviewRequestChanges || len(api.reviewRequest.Comments) != 1 ||
		api.reviewRequest.Comments[0] != (githubapi.ReviewCommentRequest{Path: "internal/mcp/backend.go", Body: "Validate this range", Line: 53, Side: githubapi.ReviewSideRight, StartLine: 50, StartSide: githubapi.ReviewSideRight}) {
		t.Fatalf("review call = number %d, credential %q, request %#v", api.reviewNumber, api.reviewCredential, api.reviewRequest)
	}
	if api.pullRequestFilesCredential != "reviewer-secret" || api.pullRequestFilesNumber != 23 {
		t.Fatalf("Pull Request files call = number %d, credential %q", api.pullRequestFilesNumber, api.pullRequestFilesCredential)
	}
}

func TestProductionBackendSubmitsReviewCommentsOnlyForLinesInScopedDiffHunks(t *testing.T) {
	patch := "@@ -10,5 +10,5 @@ section\n context\n-removed\n-removed too\n+added\n+another added\n next context\n last context"
	api := &backendGitHub{pullRequestFiles: []githubapi.PullRequestFile{{
		Filename: "review.go", Status: githubapi.PullRequestFileModified, Patch: &patch,
	}}}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: &backendCredentials{reviewer: "reviewer-secret"}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	arguments := json.RawMessage(`{"operation_id":"review-lines","event":"REQUEST_CHANGES","body":"Findings","comments":[` +
		`{"path":"review.go","line":11,"side":"LEFT","body":"removed"},` +
		`{"path":"review.go","line":12,"side":"RIGHT","body":"added"},` +
		`{"path":"review.go","line":13,"side":"RIGHT","body":"context"},` +
		`{"path":"review.go","start_line":10,"start_side":"RIGHT","line":14,"side":"RIGHT","body":"right range"},` +
		`{"path":"review.go","start_line":11,"start_side":"LEFT","line":12,"side":"LEFT","body":"left range"}]}`)

	result, err := backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolSubmitReview, Arguments: arguments, Scope: productionToolScope(workflow.RoleReviewer), Class: mcp.MutationTool, OperationID: "review-lines",
	})
	if err != nil || !json.Valid(result) {
		t.Fatalf("Execute(submit_review) = %s, %v", result, err)
	}
	if len(api.reviewRequest.Comments) != 5 {
		t.Fatalf("submitted comments = %#v", api.reviewRequest.Comments)
	}
	if api.pullRequestFilesCredential != "reviewer-secret" || api.reviewCredential != "reviewer-secret" {
		t.Fatalf("credentials = files %q, review %q", api.pullRequestFilesCredential, api.reviewCredential)
	}
}

func TestProductionBackendRejectsReviewCommentsOutsideScopedDiffHunksWithoutSubmission(t *testing.T) {
	modifiedPatch := "@@ -10,2 +10,2 @@\n context\n-old\n+new"
	addedPatch := "@@ -0,0 +1,2 @@\n+first\n+second"
	malformedPatch := "@@ -10,3 +10,2 @@\n context\n-old\n+new"
	secondHunkPatch := "@@ -1,2 +1,2 @@\n first\n-old\n+new\n@@ -10,2 +10,2 @@\n tenth\n-old tenth\n+new tenth"
	tests := []struct {
		name    string
		files   []githubapi.PullRequestFile
		comment string
	}{
		{name: "path not in Pull Request", files: []githubapi.PullRequestFile{{Filename: "review.go", Status: githubapi.PullRequestFileModified, Patch: &modifiedPatch}}, comment: `{"path":"other.go","line":10,"side":"RIGHT","body":"finding"}`},
		{name: "side has no line", files: []githubapi.PullRequestFile{{Filename: "added.go", Status: githubapi.PullRequestFileAdded, Patch: &addedPatch}}, comment: `{"path":"added.go","line":1,"side":"LEFT","body":"finding"}`},
		{name: "context on left side", files: []githubapi.PullRequestFile{{Filename: "review.go", Status: githubapi.PullRequestFileModified, Patch: &modifiedPatch}}, comment: `{"path":"review.go","line":10,"side":"LEFT","body":"finding"}`},
		{name: "line outside hunks", files: []githubapi.PullRequestFile{{Filename: "review.go", Status: githubapi.PullRequestFileModified, Patch: &modifiedPatch}}, comment: `{"path":"review.go","line":12,"side":"RIGHT","body":"finding"}`},
		{name: "nil patch", files: []githubapi.PullRequestFile{{Filename: "image.png", Status: githubapi.PullRequestFileAdded}}, comment: `{"path":"image.png","line":1,"side":"RIGHT","body":"finding"}`},
		{name: "malformed patch range", files: []githubapi.PullRequestFile{{Filename: "review.go", Status: githubapi.PullRequestFileModified, Patch: &malformedPatch}}, comment: `{"path":"review.go","line":10,"side":"RIGHT","body":"finding"}`},
		{name: "reversed range", files: []githubapi.PullRequestFile{{Filename: "review.go", Status: githubapi.PullRequestFileModified, Patch: &modifiedPatch}}, comment: `{"path":"review.go","start_line":11,"start_side":"RIGHT","line":10,"side":"RIGHT","body":"finding"}`},
		{name: "range crosses hunks", files: []githubapi.PullRequestFile{{Filename: "review.go", Status: githubapi.PullRequestFileModified, Patch: &secondHunkPatch}}, comment: `{"path":"review.go","start_line":2,"start_side":"RIGHT","line":10,"side":"RIGHT","body":"finding"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &backendGitHub{pullRequestFiles: test.files}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
				GitHub: api, Credentials: &backendCredentials{reviewer: "reviewer-secret"}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
			})
			if err != nil {
				t.Fatalf("NewProductionBackend() error = %v", err)
			}
			arguments := json.RawMessage(`{"operation_id":"invalid-lines","event":"REQUEST_CHANGES","body":"Findings","comments":[` + test.comment + `]}`)

			_, err = backend.Execute(context.Background(), mcp.Invocation{
				Name: mcp.ToolSubmitReview, Arguments: arguments, Scope: productionToolScope(workflow.RoleReviewer), Class: mcp.MutationTool, OperationID: "invalid-lines",
			})
			if !errors.Is(err, mcp.ErrToolPrecondition) || mcp.IsOutcomeUnknown(err) {
				t.Fatalf("Execute(submit_review) error = %v", err)
			}
			if api.reviewCalls != 0 {
				t.Fatalf("SubmitReview calls = %d, want 0", api.reviewCalls)
			}
		})
	}
}

func TestProductionBackendPerformsInternalWorkflowMutationsWithDurableJSON(t *testing.T) {
	publishedHead := "3123456789abcdef0123456789abcdef01234567"
	publisher := &backendPublisher{results: []workspace.PublicationResult{{Head: publishedHead, Changed: true}}}
	workflowBackend := &backendWorkflow{
		requestReviewResult: json.RawMessage(" { \"handoff_id\" : \"review-handoff-1\" } "),
		reportBlockedResult: json.RawMessage(`{"handoff_id":"human-handoff-1"}`),
	}
	credentials := &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &backendGitHub{}, Credentials: credentials, Publisher: publisher, Workflow: workflowBackend,
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	developer := productionToolScope(workflow.RoleDeveloper)
	_, err = backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolPublishChanges, Arguments: json.RawMessage(`{"operation_id":"publish-before-review","message":"Publish"}`),
		Scope: developer, Class: mcp.MutationTool, OperationID: "publish-before-review",
	})
	if err != nil {
		t.Fatalf("publish before review error = %v", err)
	}
	requestResult, err := backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolRequestReview, Arguments: json.RawMessage(`{"operation_id":"request-review-1","summary":"Ready"}`),
		Scope: developer, Class: mcp.MutationTool, OperationID: "request-review-1",
	})
	if err != nil || string(requestResult) != `{"handoff_id":"review-handoff-1"}` {
		t.Fatalf("request_review = %s, %v", requestResult, err)
	}
	reviewer := productionToolScope(workflow.RoleReviewer)
	blockedResult, err := backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolReportBlocked, Arguments: json.RawMessage(`{"operation_id":"blocked-1","reason":"Unavailable","details":"External dependency"}`),
		Scope: reviewer, Class: mcp.MutationTool, OperationID: "blocked-1",
	})
	if err != nil || string(blockedResult) != `{"handoff_id":"human-handoff-1"}` {
		t.Fatalf("report_blocked = %s, %v", blockedResult, err)
	}
	if workflowBackend.requestReview.OperationID != "request-review-1" || workflowBackend.requestReview.HeadSHA != publishedHead || workflowBackend.requestReview.Summary != "Ready" {
		t.Fatalf("RequestReview mutation = %#v", workflowBackend.requestReview)
	}
	if workflowBackend.reportBlocked.OperationID != "blocked-1" || workflowBackend.reportBlocked.Reason != "Unavailable" || workflowBackend.reportBlocked.Details != "External dependency" || workflowBackend.reportBlocked.Scope.Role != workflow.RoleReviewer {
		t.Fatalf("ReportBlocked mutation = %#v", workflowBackend.reportBlocked)
	}
	if credentials.developerCalls != 1 || credentials.reviewerCalls != 0 {
		t.Fatalf("workflow mutation credential calls = developer %d, reviewer %d", credentials.developerCalls, credentials.reviewerCalls)
	}
}

func TestLedgerWorkflowMutationsReturnTurnSettlementIntents(t *testing.T) {
	workflowMutations := mcp.LedgerWorkflowMutations{}
	scope := productionToolScope(workflow.RoleDeveloper)
	scope.PullRequest = &mcp.PullRequestScope{ID: 654, Number: 23}
	result, err := workflowMutations.RequestReview(context.Background(), mcp.RequestReviewMutation{
		Scope: scope, OperationID: "request-review-1", HeadSHA: scope.HeadSHA, Summary: "Ready for review",
	})
	if err != nil || !strings.Contains(string(result), `"outcome":"REVIEW_REQUESTED"`) || !strings.Contains(string(result), `"pull_request_number":23`) {
		t.Fatalf("RequestReview() = %s, %v", result, err)
	}
	result, err = workflowMutations.ReportBlocked(context.Background(), mcp.ReportBlockedMutation{
		Scope: scope, OperationID: "blocked-1", Reason: "missing access", Details: "deployment unavailable",
	})
	if err != nil || !strings.Contains(string(result), `"outcome":"BLOCKED"`) || !strings.Contains(string(result), `"reason":"missing access"`) {
		t.Fatalf("ReportBlocked() = %s, %v", result, err)
	}
}

func TestProductionBackendCarriesNewPullRequestIntoSameTurnReviewRequest(t *testing.T) {
	workflowBackend := &backendWorkflow{requestReviewResult: json.RawMessage(`{"handoff_id":"review-handoff-2"}`)}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &backendGitHub{}, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: &backendPublisher{}, Workflow: workflowBackend,
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	scope := productionToolScope(workflow.RoleDeveloper)
	scope.PullRequest = nil
	_, err = backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolOpenPR, Arguments: json.RawMessage(`{"operation_id":"open-then-review","title":"Open","body":"Body"}`),
		Scope: scope, Class: mcp.MutationTool, OperationID: "open-then-review",
	})
	if err != nil {
		t.Fatalf("open_pr error = %v", err)
	}
	_, err = backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolRequestReview, Arguments: json.RawMessage(`{"operation_id":"review-after-open","summary":"Ready"}`),
		Scope: scope, Class: mcp.MutationTool, OperationID: "review-after-open",
	})
	if err != nil {
		t.Fatalf("request_review after open_pr error = %v", err)
	}
	if workflowBackend.requestReview.Scope.PullRequest == nil || workflowBackend.requestReview.Scope.PullRequest.ID != 654 || workflowBackend.requestReview.Scope.PullRequest.Number != 23 {
		t.Fatalf("request_review Pull Request scope = %#v", workflowBackend.requestReview.Scope.PullRequest)
	}
}

func TestProductionBackendClassifiesSideEffectFailuresWithoutCredentialLeakage(t *testing.T) {
	scope := productionToolScope(workflow.RoleDeveloper)
	comment := mcp.Invocation{
		Name: mcp.ToolCommentOnIssue, Arguments: json.RawMessage(`{"operation_id":"comment-1","body":"Update"}`),
		Scope: scope, Class: mcp.MutationTool, OperationID: "comment-1",
	}
	newBackend := func(api *backendGitHub, publisher *backendPublisher, workflowBackend *backendWorkflow) *mcp.ProductionBackend {
		backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
			GitHub: api, Credentials: &backendCredentials{developer: "credential-super-secret"}, Publisher: publisher, Workflow: workflowBackend,
		})
		if err != nil {
			t.Fatalf("NewProductionBackend() error = %v", err)
		}
		return backend
	}

	transport := newBackend(&backendGitHub{mutationErr: &githubapi.TransientError{Cause: errors.New("credential-super-secret transport")}}, &backendPublisher{}, &backendWorkflow{})
	if _, err := transport.Execute(context.Background(), comment); err == nil || !mcp.IsOutcomeUnknown(err) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("transport error = %v, unknown = %t", err, mcp.IsOutcomeUnknown(err))
	}

	known := newBackend(&backendGitHub{mutationErr: &githubapi.APIError{StatusCode: 422, Message: "credential-super-secret validation"}}, &backendPublisher{}, &backendWorkflow{})
	if _, err := known.Execute(context.Background(), comment); err == nil || mcp.IsOutcomeUnknown(err) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("known API error = %v, unknown = %t", err, mcp.IsOutcomeUnknown(err))
	}

	publish := mcp.Invocation{
		Name: mcp.ToolPublishChanges, Arguments: json.RawMessage(`{"operation_id":"publish-failure","message":"Publish"}`),
		Scope: scope, Class: mcp.MutationTool, OperationID: "publish-failure",
	}
	push := newBackend(&backendGitHub{}, &backendPublisher{err: fmt.Errorf("%w: credential-super-secret", workspace.ErrPushRejected)}, &backendWorkflow{})
	if _, err := push.Execute(context.Background(), publish); err == nil || !mcp.IsOutcomeUnknown(err) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("push error = %v, unknown = %t", err, mcp.IsOutcomeUnknown(err))
	}

	precondition := newBackend(&backendGitHub{}, &backendPublisher{err: workspace.ErrUnexpectedHead}, &backendWorkflow{})
	if _, err := precondition.Execute(context.Background(), publish); err == nil || mcp.IsOutcomeUnknown(err) {
		t.Fatalf("precondition error = %v, unknown = %t", err, mcp.IsOutcomeUnknown(err))
	}

	credentialFailure, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &backendGitHub{}, Credentials: &backendCredentials{err: errors.New("credential-super-secret resolution")},
		Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	read := mcp.Invocation{Name: mcp.ToolGetIssue, Arguments: json.RawMessage(`{}`), Scope: scope, Class: mcp.ReadTool}
	if _, err := credentialFailure.Execute(context.Background(), read); err == nil || strings.Contains(err.Error(), "super-secret") || mcp.IsOutcomeUnknown(err) {
		t.Fatalf("credential resolution error = %v", err)
	}

	workflowFailure := newBackend(&backendGitHub{}, &backendPublisher{}, &backendWorkflow{err: mcp.OutcomeUnknown(errors.New("credential-super-secret workflow transport"))})
	blocked := mcp.Invocation{
		Name: mcp.ToolReportBlocked, Arguments: json.RawMessage(`{"operation_id":"blocked-transport","reason":"Blocked"}`),
		Scope: scope, Class: mcp.MutationTool, OperationID: "blocked-transport",
	}
	if _, err := workflowFailure.Execute(context.Background(), blocked); err == nil || !mcp.IsOutcomeUnknown(err) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("workflow transport error = %v", err)
	}
}

func TestProductionBackendRejectsCrossRoleMutationsBeforeResolvingCredentials(t *testing.T) {
	credentials := &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &backendGitHub{}, Credentials: credentials, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	reviewer := productionToolScope(workflow.RoleReviewer)
	_, err = backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolPublishChanges, Arguments: json.RawMessage(`{"operation_id":"forbidden-publish","message":"No"}`),
		Scope: reviewer, Class: mcp.MutationTool, OperationID: "forbidden-publish",
	})
	if !errors.Is(err, mcp.ErrToolNotAuthorized) || mcp.IsOutcomeUnknown(err) {
		t.Fatalf("Reviewer publish error = %v", err)
	}
	developer := productionToolScope(workflow.RoleDeveloper)
	_, err = backend.Execute(context.Background(), mcp.Invocation{
		Name: mcp.ToolSubmitReview, Arguments: json.RawMessage(`{"operation_id":"forbidden-review","event":"APPROVE"}`),
		Scope: developer, Class: mcp.MutationTool, OperationID: "forbidden-review",
	})
	if !errors.Is(err, mcp.ErrToolNotAuthorized) || mcp.IsOutcomeUnknown(err) {
		t.Fatalf("Developer review error = %v", err)
	}
	if credentials.developerCalls != 0 || credentials.reviewerCalls != 0 {
		t.Fatalf("credentials resolved for forbidden calls: developer %d, reviewer %d", credentials.developerCalls, credentials.reviewerCalls)
	}
}

const productionHeadSHA = "0123456789abcdef0123456789abcdef01234567"

func productionToolScope(role workflow.Role) mcp.ToolScope {
	return mcp.ToolScope{
		WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", AgentSessionID: "session-1",
		AgentTurnID: "turn-1", ExecutionEpoch: 4, Role: role,
		Repository: mcp.RepositoryScope{ID: 9123, Owner: "acme", Name: "widgets"},
		Issue:      mcp.IssueScope{ID: 456, Number: 12}, PullRequest: &mcp.PullRequestScope{ID: 654, Number: 23},
		Branch: "omnigrex/issue-12", DefaultBranch: "main", HeadSHA: productionHeadSHA,
		TurnCreatedAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
}

type backendGitHubCall struct {
	name, credential, owner, repository, head string
	number                                    int
}

type backendGitHub struct {
	calls                        []backendGitHubCall
	openCredential               string
	openRequest                  githubapi.OpenPullRequestRequest
	issueCommentCredential       string
	issueCommentNumber           int
	issueCommentRequest          githubapi.CommentRequest
	pullRequestCommentCredential string
	pullRequestCommentNumber     int
	pullRequestCommentRequest    githubapi.CommentRequest
	reviewCredential             string
	reviewNumber                 int
	reviewRequest                githubapi.ReviewRequest
	reviewCalls                  int
	pullRequestFiles             []githubapi.PullRequestFile
	pullRequestFilesCredential   string
	pullRequestFilesNumber       int
	mutationErr                  error
	openHead                     string
}

func (api *backendGitHub) record(name, credential, owner, repository string, number int, head string) {
	api.calls = append(api.calls, backendGitHubCall{name: name, credential: credential, owner: owner, repository: repository, number: number, head: head})
}

func (api *backendGitHub) GetIssue(_ context.Context, credential, owner, repository string, number int) (githubapi.Issue, error) {
	api.record(mcp.ToolGetIssue, credential, owner, repository, number, "")
	return githubapi.Issue{ID: 456, Number: number, Title: "Issue"}, nil
}

func (api *backendGitHub) ListIssueComments(_ context.Context, credential, owner, repository string, number int) ([]githubapi.IssueComment, error) {
	api.record(mcp.ToolListIssueComments, credential, owner, repository, number, "")
	return []githubapi.IssueComment{}, nil
}

func (api *backendGitHub) GetPullRequest(_ context.Context, credential, owner, repository string, number int) (githubapi.PullRequest, error) {
	api.record(mcp.ToolGetPullRequest, credential, owner, repository, number, "")
	return githubapi.PullRequest{ID: 654, Number: number, Head: githubapi.PullRequestBranch{Ref: "omnigrex/issue-12", SHA: productionHeadSHA}, Base: githubapi.PullRequestBranch{Ref: "main"}}, nil
}

func (api *backendGitHub) ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error) {
	return nil, nil
}

func (api *backendGitHub) ListPullRequestFiles(_ context.Context, credential, _, _ string, number int) ([]githubapi.PullRequestFile, error) {
	api.pullRequestFilesCredential, api.pullRequestFilesNumber = credential, number
	if api.pullRequestFiles != nil {
		return api.pullRequestFiles, nil
	}
	patch := "@@ -48,6 +48,6 @@\n line 48\n line 49\n line 50\n line 51\n line 52\n-old line 53\n+new line 53"
	return []githubapi.PullRequestFile{{Filename: "internal/mcp/backend.go", Status: githubapi.PullRequestFileModified, Patch: &patch}}, nil
}

func (api *backendGitHub) ListPullRequestReviews(_ context.Context, credential, owner, repository string, number int) ([]githubapi.Review, error) {
	api.record(mcp.ToolListPullRequestReviews, credential, owner, repository, number, "")
	return []githubapi.Review{}, nil
}

func (api *backendGitHub) ListReviewThreads(_ context.Context, credential, owner, repository string, number int) ([]githubapi.ReviewThread, error) {
	api.record(mcp.ToolListReviewThreads, credential, owner, repository, number, "")
	return []githubapi.ReviewThread{}, nil
}

func (api *backendGitHub) GetCheckRuns(_ context.Context, credential, owner, repository, head string) ([]githubapi.CheckRun, error) {
	api.record(mcp.ToolGetCheckRuns, credential, owner, repository, 0, head)
	return []githubapi.CheckRun{}, nil
}

func (api *backendGitHub) OpenPullRequest(_ context.Context, credential, _, _ string, request githubapi.OpenPullRequestRequest) (githubapi.PullRequest, error) {
	api.openCredential, api.openRequest = credential, request
	if api.mutationErr != nil {
		return githubapi.PullRequest{}, api.mutationErr
	}
	head := api.openHead
	if head == "" {
		head = productionHeadSHA
	}
	return githubapi.PullRequest{ID: 654, NodeID: "PR_654", Number: 23, HTMLURL: "https://github.test/acme/widgets/pull/23", Head: githubapi.PullRequestBranch{Ref: request.Head, SHA: head}, Base: githubapi.PullRequestBranch{Ref: request.Base}}, nil
}

func (api *backendGitHub) CreateIssueComment(_ context.Context, credential, _, _ string, number int, request githubapi.CommentRequest) (githubapi.IssueComment, error) {
	api.issueCommentCredential, api.issueCommentNumber, api.issueCommentRequest = credential, number, request
	if api.mutationErr != nil {
		return githubapi.IssueComment{}, api.mutationErr
	}
	return githubapi.IssueComment{ID: 701, NodeID: "IC_701", HTMLURL: "https://github.test/acme/widgets/issues/12#issuecomment-701"}, nil
}

func (api *backendGitHub) CreatePullRequestComment(_ context.Context, credential, _, _ string, number int, request githubapi.CommentRequest) (githubapi.IssueComment, error) {
	api.pullRequestCommentCredential, api.pullRequestCommentNumber, api.pullRequestCommentRequest = credential, number, request
	if api.mutationErr != nil {
		return githubapi.IssueComment{}, api.mutationErr
	}
	return githubapi.IssueComment{ID: 702, NodeID: "IC_702", HTMLURL: "https://github.test/acme/widgets/pull/23#issuecomment-702"}, nil
}

func (api *backendGitHub) SubmitReview(_ context.Context, credential, _, _ string, number int, request githubapi.ReviewRequest) (githubapi.Review, error) {
	api.reviewCalls++
	api.reviewCredential, api.reviewNumber, api.reviewRequest = credential, number, request
	if api.mutationErr != nil {
		return githubapi.Review{}, api.mutationErr
	}
	return githubapi.Review{ID: 801, NodeID: "PRR_801", State: "CHANGES_REQUESTED", CommitID: request.CommitID, User: githubapi.User{ID: 91, Login: "reviewer-app"}, HTMLURL: "https://github.test/acme/widgets/pull/23#pullrequestreview-801"}, nil
}

type backendCredentials struct {
	developer, reviewer           string
	developerCalls, reviewerCalls int
	err                           error
}

func (credentials *backendCredentials) DeveloperCredential(context.Context, mcp.RepositoryScope) (string, error) {
	credentials.developerCalls++
	if credentials.err != nil {
		return "", credentials.err
	}
	return credentials.developer, nil
}

func (credentials *backendCredentials) ReviewerCredential(context.Context, mcp.RepositoryScope) (string, error) {
	credentials.reviewerCalls++
	if credentials.err != nil {
		return "", credentials.err
	}
	return credentials.reviewer, nil
}

type backendPublisher struct {
	publications []workspace.Publication
	results      []workspace.PublicationResult
	err          error
}

func (publisher *backendPublisher) Publish(_ context.Context, publication workspace.Publication) (workspace.PublicationResult, error) {
	publisher.publications = append(publisher.publications, publication)
	if publisher.err != nil {
		return workspace.PublicationResult{}, publisher.err
	}
	if len(publisher.results) == 0 {
		return workspace.PublicationResult{}, errors.New("unexpected Publish")
	}
	result := publisher.results[0]
	publisher.results = publisher.results[1:]
	return result, nil
}

type backendWorkflow struct {
	requestReview       mcp.RequestReviewMutation
	reportBlocked       mcp.ReportBlockedMutation
	requestReviewResult json.RawMessage
	reportBlockedResult json.RawMessage
	err                 error
}

func (workflow *backendWorkflow) RequestReview(_ context.Context, mutation mcp.RequestReviewMutation) (json.RawMessage, error) {
	workflow.requestReview = mutation
	if workflow.err != nil {
		return nil, workflow.err
	}
	if workflow.requestReviewResult == nil {
		return nil, errors.New("unexpected RequestReview")
	}
	return workflow.requestReviewResult, nil
}

func (workflow *backendWorkflow) ReportBlocked(_ context.Context, mutation mcp.ReportBlockedMutation) (json.RawMessage, error) {
	workflow.reportBlocked = mutation
	if workflow.err != nil {
		return nil, workflow.err
	}
	if workflow.reportBlockedResult == nil {
		return nil, errors.New("unexpected ReportBlocked")
	}
	return workflow.reportBlockedResult, nil
}
