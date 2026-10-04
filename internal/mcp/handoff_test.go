package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestRequestReviewWaitsForConfirmedCommentPublication(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	api := &handoffGitHub{backendGitHub: &backendGitHub{}, started: started, release: release}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := backend.Execute(ctx, handoffInvocation())
		if err == nil && !strings.Contains(string(result), `"outcome":"REVIEW_REQUESTED"`) {
			err = errors.New("missing successful handoff outcome")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("publication did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("handoff completed before comment publication: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.posted.Body, productionHeadSHA) || !strings.Contains(api.posted.Body, "go test ./...: passed") ||
		!strings.Contains(api.posted.Body, "persisted-signature") || !strings.Contains(api.posted.Body, reconciliationMutationID) {
		t.Fatalf("handoff lacks exact head, evidence, reserved signature, or operation identity: %s", api.posted.Body)
	}
}

func TestRequestReviewLostResponseRequiresExactPublicationEvidence(t *testing.T) {
	api := &handoffGitHub{backendGitHub: &backendGitHub{}, afterPost: context.DeadlineExceeded}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{},
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation := handoffInvocation()
	if _, err := backend.Execute(context.Background(), invocation); !mcp.IsOutcomeUnknown(err) {
		t.Fatalf("lost comment response = %v, want uncertain effect", err)
	}
	mutation := reconciliationMutation(mcp.ToolRequestReview, string(invocation.Arguments))
	mutation.OperationID = "handoff-1"
	mutation.ExternalResourceID = "9123:" + invocation.Scope.Branch
	mutation.ExpectedSHA = productionHeadSHA
	for _, test := range []struct {
		name     string
		comments []githubapi.IssueComment
		want     mcp.MutationReconciliationDisposition
	}{
		{"exact published comment", []githubapi.IssueComment{api.posted}, mcp.ReconciliationFound},
		{"PR alone is insufficient", nil, mcp.ReconciliationUnresolved},
		{"duplicate marker", []githubapi.IssueComment{api.posted, api.posted}, mcp.ReconciliationUnresolved},
		{"altered summary", []githubapi.IssueComment{{ID: 702, NodeID: "IC_702", HTMLURL: api.posted.HTMLURL, Body: strings.Replace(api.posted.Body, "passed", "failed", 1)}}, mcp.ReconciliationUnresolved},
	} {
		t.Run(test.name, func(t *testing.T) {
			observations := &reconciliationGitHub{issueComments: test.comments}
			reconciler := newProductionReconciler(t, observations, &reconciliationPublications{})
			for _, returning := range []bool{false, true} {
				scope := reconciliationContext()
				if returning {
					// The new published head differs from the Turn's starting head.
					scope.Turn.ExpectedHeadSHA = advancedHeadSHA
				}
				result, err := reconciler.Reconcile(context.Background(), scope, mutation)
				if err != nil || result.Disposition != test.want {
					t.Fatalf("returning=%t: reconciliation = %#v, %v", returning, result, err)
				}
				if test.want == mcp.ReconciliationFound && !strings.Contains(string(result.Outcome.Result), productionHeadSHA) {
					t.Fatal("recovery lost the published head")
				}
			}
		})
	}
}

func TestRequestReviewRejectedPublicationCannotSucceed(t *testing.T) {
	api := &backendGitHub{mutationErr: &githubapi.PermissionError{APIError: &githubapi.APIError{StatusCode: 403}}}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: api, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Execute(context.Background(), handoffInvocation())
	if err == nil || result != nil {
		t.Fatalf("rejected publication returned handoff success: %s, %v", result, err)
	}
}

func TestGetHandoffUsesOnlyTheAuthorizedScope(t *testing.T) {
	reader := &handoffReader{}
	credentials := &backendCredentials{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: &backendGitHub{}, Credentials: credentials, Publisher: &backendPublisher{},
		Workflow: mcp.LedgerWorkflowMutations{}, Handoffs: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		scope := productionToolScope(role)
		result, err := backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolGetHandoff, Scope: scope, Class: mcp.ReadTool, Arguments: json.RawMessage(`{}`)})
		if err != nil || string(result) != "null" || reader.workflow != scope.WorkflowID || reader.repo != scope.Repository.ID || reader.pr != scope.PullRequest.ID || reader.head != scope.HeadSHA {
			t.Fatalf("get_handoff = %s, %v, scope %#v", result, err, reader)
		}
	}
	if credentials.developerCalls != 0 || credentials.reviewerCalls != 0 {
		t.Fatal("internal handoff read acquired external credentials")
	}
}

func handoffInvocation() mcp.Invocation {
	return mcp.Invocation{
		Name: mcp.ToolRequestReview, Class: mcp.MutationTool, Scope: productionToolScope(workflow.RoleDeveloper),
		OperationID: reconciliationMutationID,
		Arguments:   json.RawMessage(`{"operation_id":"handoff-1","summary":"Fixed the regression. go test ./...: passed","signature":"persisted-signature"}`),
	}
}

type handoffGitHub struct {
	*backendGitHub
	started   chan struct{}
	release   chan struct{}
	afterPost error
	posted    githubapi.IssueComment
}

func (api *handoffGitHub) CreatePullRequestComment(ctx context.Context, credential, owner, repository string, number int, request githubapi.CommentRequest) (githubapi.IssueComment, error) {
	if api.started != nil {
		close(api.started)
		select {
		case <-api.release:
		case <-ctx.Done():
			return githubapi.IssueComment{}, ctx.Err()
		}
	}
	comment, err := api.backendGitHub.CreatePullRequestComment(ctx, credential, owner, repository, number, request)
	api.posted = comment
	if api.afterPost != nil {
		return githubapi.IssueComment{}, api.afterPost
	}
	return comment, err
}

type handoffReader struct {
	workflow, head string
	repo, pr       int64
}

func (reader *handoffReader) GetReviewHandoff(_ context.Context, workflow string, repo, pr int64, head string) (*store.ReviewHandoff, error) {
	reader.workflow, reader.repo, reader.pr, reader.head = workflow, repo, pr, head
	return nil, nil
}
