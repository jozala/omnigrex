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
	"github.com/jozala/omnigrex/internal/workspace"
)

const (
	reconciliationMutationID = "10000000-0000-4000-8000-000000000001"
	advancedHeadSHA          = "2123456789abcdef0123456789abcdef01234567"
)

func TestProductionReconcilerFindsExactCommentArtifactUsingReservationIdentity(t *testing.T) {
	marker, err := githubapi.RenderMarker(githubapi.Marker{
		WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: reconciliationMutationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := &reconciliationGitHub{issueComments: []githubapi.IssueComment{
		{ID: 701, NodeID: "IC_701", Body: "Visible update\n\n" + marker, HTMLURL: "https://github.test/acme/widgets/issues/12#issuecomment-701"},
		{ID: 702, NodeID: "IC_702", Body: "Visible update\n\n<!-- omnigrex:v1 workflow=workflow-1 assignment=assignment-1 operation=caller-key -->"},
	}}
	reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
	mutation := reconciliationMutation(mcp.ToolCommentOnIssue, `{"operation_id":"caller-key","body":"Visible update"}`)
	mutation.ExternalResourceID = "9123:456"
	mutation.State = store.MutationSucceeded

	result, err := reconciler.Reconcile(context.Background(), reconciliationContext(), mutation)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Disposition != mcp.ReconciliationFound || result.Outcome.State != store.MutationSucceeded ||
		string(result.Outcome.Result) != `{"comment_id":701,"node_id":"IC_701","html_url":"https://github.test/acme/widgets/issues/12#issuecomment-701"}` {
		t.Fatalf("Reconcile() = %#v", result)
	}
	if api.issueNumber != 12 || api.credential != "developer-secret" {
		t.Fatalf("GitHub lookup = credential %q, Issue %d", api.credential, api.issueNumber)
	}
}

func TestProductionReconcilerUsesDeveloperCredentialsForReviewerPullRequestComments(t *testing.T) {
	marker := reconciliationMarker(t, reconciliationMutationID)
	api := &reconciliationGitHub{issueComments: []githubapi.IssueComment{{
		ID: 702, NodeID: "IC_702", Body: "Reviewer update\n\n" + marker,
		HTMLURL: "https://github.test/acme/widgets/pull/23#issuecomment-702",
	}}}
	reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
	reconciliation := reconciliationContext()
	reconciliation.Role = workflow.RoleReviewer
	mutation := reconciliationMutation(mcp.ToolCommentOnPullRequest, `{"operation_id":"caller-key","body":"Reviewer update"}`)
	mutation.ExternalResourceID = "9123:654"
	mutation.State = store.MutationSucceeded

	result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationFound {
		t.Fatalf("Reconcile(comment_on_pull_request) = (%#v, %v)", result, err)
	}
	if api.issueNumber != 23 || api.credential != "developer-secret" {
		t.Fatalf("GitHub lookup = credential %q, Pull Request %d", api.credential, api.issueNumber)
	}
}

func TestProductionReconcilerFindsExactPullRequestAndReviewArtifacts(t *testing.T) {
	reconciliation := reconciliationContext()
	marker := reconciliationMarker(t, reconciliationMutationID)
	t.Run("open Pull Request", func(t *testing.T) {
		api := &reconciliationGitHub{pullRequests: []githubapi.PullRequest{{
			ID: 654, NodeID: "PR_654", Number: 23, Title: "Implement it",
			Body: "Description\n\nCloses #12\n\n" + marker, State: "closed",
			HTMLURL: "https://github.test/acme/widgets/pull/23",
			Head:    githubapi.PullRequestBranch{Ref: "omnigrex/issue-12", SHA: productionHeadSHA},
			Base:    githubapi.PullRequestBranch{Ref: "main", SHA: "1123456789abcdef0123456789abcdef01234567"},
		}}}
		reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
		mutation := reconciliationMutation(mcp.ToolOpenPR, `{"operation_id":"caller-open","title":"Implement it","body":"Description"}`)
		mutation.ExternalResourceID = "9123:omnigrex/issue-12:main"
		mutation.State = store.MutationSucceeded

		result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound ||
			string(result.Outcome.Result) != `{"pull_request_id":654,"node_id":"PR_654","number":23,"html_url":"https://github.test/acme/widgets/pull/23","head_sha":"0123456789abcdef0123456789abcdef01234567"}` {
			t.Fatalf("Reconcile(open_pr) = (%#v, %v)", result, err)
		}
		if api.pullRequestQuery != (githubapi.ListPullRequestsRequest{Head: "omnigrex/issue-12", Base: "main"}) {
			t.Fatalf("ListPullRequests request = %#v", api.pullRequestQuery)
		}
	})

	t.Run("submitted review", func(t *testing.T) {
		reconciliation.Role = workflow.RoleReviewer
		reconciliation.ReviewerActorID = 91
		api := &reconciliationGitHub{reviews: []githubapi.Review{{
			ID: 801, NodeID: "PRR_801", State: "CHANGES_REQUESTED", Body: "Fix this\n\n" + marker,
			CommitID: productionHeadSHA, User: githubapi.User{ID: 91, Login: "reviewer-app"},
			HTMLURL: "https://github.test/acme/widgets/pull/23#pullrequestreview-801",
		}}}
		reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
		mutation := reconciliationMutation(mcp.ToolSubmitReview, `{"operation_id":"caller-review","event":"REQUEST_CHANGES","body":"Fix this","comments":[]}`)
		mutation.ExternalResourceID = "9123:654"
		mutation.State = store.MutationSucceeded

		result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound ||
			string(result.Outcome.Result) != `{"review_id":801,"node_id":"PRR_801","state":"CHANGES_REQUESTED","commit_id":"0123456789abcdef0123456789abcdef01234567","actor_id":91,"html_url":"https://github.test/acme/widgets/pull/23#pullrequestreview-801"}` {
			t.Fatalf("Reconcile(submit_review) = (%#v, %v)", result, err)
		}
		if api.reviewNumber != 23 || api.credential != "reviewer-secret" {
			t.Fatalf("review lookup = credential %q, Pull Request %d", api.credential, api.reviewNumber)
		}
	})
}

func TestProductionReconcilerFindsTurnScopedArtifactsAfterProposalHeadAdvances(t *testing.T) {
	reconciliation := reconciliationContext()
	reconciliation.ChangeProposal.HeadSHA = advancedHeadSHA
	marker := reconciliationMarker(t, reconciliationMutationID)

	t.Run("open Pull Request", func(t *testing.T) {
		api := &reconciliationGitHub{pullRequests: []githubapi.PullRequest{{
			ID: 654, NodeID: "PR_654", Number: 23, Title: "Implement it",
			Body: "Description\n\nCloses #12\n\n" + marker, HTMLURL: "https://github.test/acme/widgets/pull/23",
			Head: githubapi.PullRequestBranch{Ref: "omnigrex/issue-12", SHA: advancedHeadSHA},
			Base: githubapi.PullRequestBranch{Ref: "main"},
		}}}
		mutation := reconciliationMutation(mcp.ToolOpenPR, `{"operation_id":"caller-open","title":"Implement it","body":"Description"}`)
		mutation.ExternalResourceID = "9123:omnigrex/issue-12:main"

		result, err := newProductionReconciler(t, api, &reconciliationPublications{}).Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound ||
			!strings.Contains(string(result.Outcome.Result), `"head_sha":"`+productionHeadSHA+`"`) {
			t.Fatalf("Reconcile(open_pr) = (%#v, %v)", result, err)
		}
	})

	t.Run("Pull Request comment", func(t *testing.T) {
		api := &reconciliationGitHub{issueComments: []githubapi.IssueComment{{
			ID: 702, NodeID: "IC_702", Body: "Update\n\n" + marker,
			HTMLURL: "https://github.test/acme/widgets/pull/23#issuecomment-702",
		}}}
		mutation := reconciliationMutation(mcp.ToolCommentOnPullRequest, `{"operation_id":"caller-comment","body":"Update"}`)
		mutation.ExternalResourceID = "9123:654"

		result, err := newProductionReconciler(t, api, &reconciliationPublications{}).Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound {
			t.Fatalf("Reconcile(comment_on_pull_request) = (%#v, %v)", result, err)
		}
	})

	t.Run("submitted review", func(t *testing.T) {
		reviewContext := reconciliation
		reviewContext.Role = workflow.RoleReviewer
		reviewContext.ReviewerActorID = 91
		api := &reconciliationGitHub{reviews: []githubapi.Review{{
			ID: 801, NodeID: "PRR_801", State: "APPROVED", Body: "Looks good\n\n" + marker,
			CommitID: productionHeadSHA, User: githubapi.User{ID: 91, Login: "reviewer-app"},
			HTMLURL: "https://github.test/acme/widgets/pull/23#pullrequestreview-801",
		}}}
		mutation := reconciliationMutation(mcp.ToolSubmitReview, `{"operation_id":"caller-review","event":"APPROVE","body":"Looks good","comments":[]}`)
		mutation.ExternalResourceID = "9123:654"

		result, err := newProductionReconciler(t, api, &reconciliationPublications{}).Reconcile(context.Background(), reviewContext, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound {
			t.Fatalf("Reconcile(submit_review) = (%#v, %v)", result, err)
		}
	})

	t.Run("review request", func(t *testing.T) {
		mutation := reconciliationMutation(mcp.ToolRequestReview, `{"operation_id":"caller-request-review","summary":"Ready"}`)
		mutation.ExternalService = "omnigrex"
		mutation.ExternalResourceID = "9123:omnigrex/issue-12"

		result, err := newProductionReconciler(t, &reconciliationGitHub{}, &reconciliationPublications{}).Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound ||
			!strings.Contains(string(result.Outcome.Result), `"head_sha":"`+productionHeadSHA+`"`) {
			t.Fatalf("Reconcile(request_review) = (%#v, %v)", result, err)
		}
	})
}

func TestProductionReconcilerOpenPullRequestRequiresPersistedExactBase(t *testing.T) {
	marker := reconciliationMarker(t, reconciliationMutationID)
	matching := githubapi.PullRequest{
		ID: 654, NodeID: "PR_654", Number: 23, Title: "Implement it",
		Body: "Description\n\nCloses #12\n\n" + marker, HTMLURL: "https://github.test/acme/widgets/pull/23",
		Head: githubapi.PullRequestBranch{Ref: "omnigrex/issue-12", SHA: productionHeadSHA},
		Base: githubapi.PullRequestBranch{Ref: "main"},
	}
	newMutation := func(resource string) store.MutationReservation {
		mutation := reconciliationMutation(mcp.ToolOpenPR, `{"operation_id":"caller-open","title":"Implement it","body":"Description"}`)
		mutation.ExternalResourceID = resource
		return mutation
	}

	t.Run("Change Proposal absent", func(t *testing.T) {
		reconciliation := reconciliationContext()
		reconciliation.ChangeProposal = nil
		reconciliation.Turn.ChangeProposalID = ""
		reconciliation.Turn.ExpectedHeadSHA = ""
		api := &reconciliationGitHub{pullRequests: []githubapi.PullRequest{matching}}
		result, err := newProductionReconciler(t, api, &reconciliationPublications{}).Reconcile(
			context.Background(), reconciliation, newMutation("9123:omnigrex/issue-12:main"),
		)
		if err != nil || result.Disposition != mcp.ReconciliationFound {
			t.Fatalf("Reconcile() = (%#v, %v)", result, err)
		}
		if api.pullRequestQuery != (githubapi.ListPullRequestsRequest{Head: "omnigrex/issue-12", Base: "main"}) {
			t.Fatalf("ListPullRequests request = %#v", api.pullRequestQuery)
		}
	})

	t.Run("malformed resource", func(t *testing.T) {
		for _, resource := range []string{
			"9123:omnigrex/issue-12", "9123:omnigrex/issue-12:", "9123::main",
			"9123:omnigrex/issue-12:main:extra", "9124:omnigrex/issue-12:main",
		} {
			_, err := newProductionReconciler(t, &reconciliationGitHub{}, &reconciliationPublications{}).Reconcile(
				context.Background(), reconciliationContext(), newMutation(resource),
			)
			if !errors.Is(err, mcp.ErrInvalidMutationReconciliation) {
				t.Errorf("resource %q error = %v, want ErrInvalidMutationReconciliation", resource, err)
			}
		}
	})

	t.Run("marked Pull Request retargeted", func(t *testing.T) {
		retargeted := matching
		retargeted.Base.Ref = "release"
		api := &reconciliationGitHub{pullRequests: []githubapi.PullRequest{retargeted}}
		result, err := newProductionReconciler(t, api, &reconciliationPublications{}).Reconcile(
			context.Background(), reconciliationContext(), newMutation("9123:omnigrex/issue-12:main"),
		)
		if err != nil || result.Disposition != mcp.ReconciliationUnresolved {
			t.Fatalf("Reconcile() = (%#v, %v)", result, err)
		}
	})

	t.Run("Change Proposal base mismatches persisted intent", func(t *testing.T) {
		reconciliation := reconciliationContext()
		reconciliation.ChangeProposal.BaseRef = "release"
		api := &reconciliationGitHub{pullRequests: []githubapi.PullRequest{matching}}
		result, err := newProductionReconciler(t, api, &reconciliationPublications{}).Reconcile(
			context.Background(), reconciliation, newMutation("9123:omnigrex/issue-12:main"),
		)
		if err != nil || result.Disposition != mcp.ReconciliationUnresolved {
			t.Fatalf("Reconcile() = (%#v, %v)", result, err)
		}
	})
}

func TestProductionReconcilerBootstrapsReviewerActorFromOneExactReview(t *testing.T) {
	reconciliation := reconciliationContext()
	reconciliation.Role = workflow.RoleReviewer
	reconciliation.ReviewerActorID = 0
	marker := reconciliationMarker(t, reconciliationMutationID)
	api := &reconciliationGitHub{reviews: []githubapi.Review{{
		ID: 801, NodeID: "PRR_801", State: "CHANGES_REQUESTED", Body: "Fix this\n\n" + marker,
		CommitID: productionHeadSHA, User: githubapi.User{ID: 91, Login: "reviewer-app"},
		HTMLURL: "https://github.test/acme/widgets/pull/23#pullrequestreview-801",
	}}}
	reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
	mutation := reconciliationMutation(mcp.ToolSubmitReview, `{"operation_id":"caller-review","event":"REQUEST_CHANGES","body":"Fix this","comments":[]}`)
	mutation.ExternalResourceID = "9123:654"

	result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationFound || result.Outcome.State != store.MutationSucceeded ||
		!strings.Contains(string(result.Outcome.Result), `"actor_id":91`) {
		t.Fatalf("Reconcile(submit_review) = (%#v, %v)", result, err)
	}
}

func TestProductionReconcilerLeavesReviewerActorBootstrapConflictsUnresolved(t *testing.T) {
	reconciliation := reconciliationContext()
	reconciliation.Role = workflow.RoleReviewer
	reconciliation.ReviewerActorID = 0
	marker := reconciliationMarker(t, reconciliationMutationID)
	exactReview := githubapi.Review{
		ID: 801, NodeID: "PRR_801", State: "CHANGES_REQUESTED", Body: "Fix this\n\n" + marker,
		CommitID: productionHeadSHA, User: githubapi.User{ID: 91, Login: "reviewer-app"},
		HTMLURL: "https://github.test/acme/widgets/pull/23#pullrequestreview-801",
	}
	mutation := reconciliationMutation(mcp.ToolSubmitReview, `{"operation_id":"caller-review","event":"REQUEST_CHANGES","body":"Fix this","comments":[]}`)
	mutation.ExternalResourceID = "9123:654"

	for name, reviews := range map[string][]githubapi.Review{
		"duplicate": {exactReview, exactReview},
		"different marked actor": {exactReview, {
			ID: 802, NodeID: "PRR_802", State: "CHANGES_REQUESTED", Body: "Fix this\n\n" + marker,
			CommitID: productionHeadSHA, User: githubapi.User{ID: 92, Login: "other-reviewer-app"},
			HTMLURL: "https://github.test/acme/widgets/pull/23#pullrequestreview-802",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := newProductionReconciler(t, &reconciliationGitHub{reviews: reviews}, &reconciliationPublications{}).Reconcile(
				context.Background(), reconciliation, mutation,
			)
			if err != nil || result.Disposition != mcp.ReconciliationUnresolved || result.Outcome.State != "" {
				t.Fatalf("Reconcile(submit_review) = (%#v, %v)", result, err)
			}
		})
	}
}

func TestProductionReconcilerLeavesReviewUnresolvedForDifferentReviewerActor(t *testing.T) {
	reconciliation := reconciliationContext()
	reconciliation.Role = workflow.RoleReviewer
	reconciliation.ReviewerActorID = 92
	marker := reconciliationMarker(t, reconciliationMutationID)
	api := &reconciliationGitHub{reviews: []githubapi.Review{{
		ID: 801, NodeID: "PRR_801", State: "CHANGES_REQUESTED", Body: "Fix this\n\n" + marker,
		CommitID: productionHeadSHA, User: githubapi.User{ID: 91, Login: "other-reviewer-app"},
		HTMLURL: "https://github.test/acme/widgets/pull/23#pullrequestreview-801",
	}}}
	reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
	mutation := reconciliationMutation(mcp.ToolSubmitReview, `{"operation_id":"caller-review","event":"REQUEST_CHANGES","body":"Fix this","comments":[]}`)
	mutation.ExternalResourceID = "9123:654"

	result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationUnresolved || result.Outcome.State != "" {
		t.Fatalf("Reconcile(submit_review) = (%#v, %v)", result, err)
	}
}

func TestProductionReconcilerReconcilesPublicationAndInternalMutations(t *testing.T) {
	reconciliation := reconciliationContext()
	t.Run("publication", func(t *testing.T) {
		publications := &reconciliationPublications{result: workspace.PublicationReconciliationResult{
			Outcome: workspace.PublicationReconciliationFound, Head: "2123456789abcdef0123456789abcdef01234567",
		}}
		reconciler := newProductionReconciler(t, &reconciliationGitHub{}, publications)
		mutation := reconciliationMutation(mcp.ToolPublishChanges, `{"operation_id":"caller-publish","message":"Publish"}`)
		mutation.ExternalService = "git"
		mutation.ExternalResourceID = "9123:omnigrex/issue-12"
		mutation.State = store.MutationSucceeded

		result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound ||
			string(result.Outcome.Result) != `{"head":"2123456789abcdef0123456789abcdef01234567","branch":"omnigrex/issue-12","changed":true}` {
			t.Fatalf("Reconcile(publish_changes) = (%#v, %v)", result, err)
		}
		input := publications.input
		if input.AssignmentID != "assignment-1" || input.RepositoryURL != "https://github.com/acme/widgets.git" ||
			input.Credential != "developer-secret" || input.BaseRevision != productionHeadSHA || input.ExpectedOldHead != productionHeadSHA ||
			input.Branch != "omnigrex/issue-12" || input.OperationID != reconciliationMutationID {
			t.Fatalf("ReconcilePublication input = %#v", input)
		}
	})

	t.Run("report blocked", func(t *testing.T) {
		reconciler := newProductionReconciler(t, &reconciliationGitHub{}, &reconciliationPublications{})
		mutation := reconciliationMutation(mcp.ToolReportBlocked, `{"operation_id":"caller-blocked","reason":"Unavailable","details":"External dependency"}`)
		mutation.ExternalService = "omnigrex"
		mutation.ExternalResourceID = "workflow-1"
		mutation.ExpectedSHA = ""
		mutation.State = store.MutationSucceeded

		result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound ||
			string(result.Outcome.Result) != `{"outcome":"BLOCKED","reason":"Unavailable","details":"External dependency"}` {
			t.Fatalf("Reconcile(report_blocked) = (%#v, %v)", result, err)
		}
	})

	t.Run("request review from Change Proposal", func(t *testing.T) {
		reconciler := newProductionReconciler(t, &reconciliationGitHub{}, &reconciliationPublications{})
		mutation := reconciliationMutation(mcp.ToolRequestReview, `{"operation_id":"caller-request-review","summary":"Ready"}`)
		mutation.ExternalService = "omnigrex"
		mutation.ExternalResourceID = "9123:omnigrex/issue-12"
		mutation.State = store.MutationSucceeded

		result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound ||
			string(result.Outcome.Result) != `{"outcome":"REVIEW_REQUESTED","pull_request_id":654,"pull_request_number":23,"head_sha":"0123456789abcdef0123456789abcdef01234567"}` {
			t.Fatalf("Reconcile(request_review) = (%#v, %v)", result, err)
		}
	})
}

func TestProductionReconcilerTreatsAbsentExternalArtifactAsUnresolved(t *testing.T) {
	reconciliation := reconciliationContext()
	mutation := reconciliationMutation(mcp.ToolCommentOnIssue, `{"operation_id":"caller-key","body":"Visible update"}`)
	mutation.ExternalResourceID = "9123:456"

	t.Run("not yet visible", func(t *testing.T) {
		reconciler := newProductionReconciler(t, &reconciliationGitHub{}, &reconciliationPublications{})
		result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationUnresolved || result.Outcome.State != "" {
			t.Fatalf("Reconcile(absent) = (%#v, %v)", result, err)
		}
	})

	t.Run("marker collision", func(t *testing.T) {
		marker := reconciliationMarker(t, reconciliationMutationID)
		api := &reconciliationGitHub{issueComments: []githubapi.IssueComment{
			{ID: 701, NodeID: "IC_701", Body: "first\n\n" + marker, HTMLURL: "https://github.test/1"},
			{ID: 702, NodeID: "IC_702", Body: "second\n\n" + marker, HTMLURL: "https://github.test/2"},
		}}
		reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
		result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationUnresolved || result.Outcome.State != "" {
			t.Fatalf("Reconcile(collision) = (%#v, %v)", result, err)
		}
	})

	t.Run("dependency", func(t *testing.T) {
		api := &reconciliationGitHub{err: errors.New("credential-super-secret transport")}
		reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
		_, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
		if !errors.Is(err, mcp.ErrMutationReconciliationDependency) || strings.Contains(err.Error(), "super-secret") {
			t.Fatalf("Reconcile(dependency) error = %v", err)
		}
	})
}

func TestProductionReconcilerDiscoversRequestReviewPullRequestWithoutChangeProposal(t *testing.T) {
	reconciliation := reconciliationContext()
	reconciliation.ChangeProposal = nil
	reconciliation.Turn.ChangeProposalID = ""
	reconciliation.Turn.ExpectedHeadSHA = ""
	marker, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: "10000000-0000-4000-8000-000000000099"})
	if err != nil {
		t.Fatal(err)
	}
	api := &reconciliationGitHub{pullRequests: []githubapi.PullRequest{{
		ID: 654, NodeID: "PR_654", Number: 23, Title: "Implement", Body: "Closes #12\n\n" + marker,
		HTMLURL: "https://github.test/acme/widgets/pull/23", Head: githubapi.PullRequestBranch{Ref: "omnigrex/issue-12", SHA: productionHeadSHA},
		Base: githubapi.PullRequestBranch{Ref: "main"},
	}}}
	reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
	mutation := reconciliationMutation(mcp.ToolRequestReview, `{"operation_id":"caller-review","summary":"Ready"}`)
	mutation.ExternalService = "omnigrex"
	mutation.ExternalResourceID = "9123:omnigrex/issue-12"

	result, err := reconciler.Reconcile(context.Background(), reconciliation, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationFound || !strings.Contains(string(result.Outcome.Result), `"pull_request_id":654`) {
		t.Fatalf("Reconcile(discovered request_review) = (%#v, %v)", result, err)
	}
}

func newProductionReconciler(t *testing.T, api mcp.GitHubAPI, publications mcp.PublicationReconciler) *mcp.ProductionReconciler {
	t.Helper()
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{
		GitHub: api, Credentials: &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}, Publications: publications,
	})
	if err != nil {
		t.Fatalf("NewProductionReconciler() error = %v", err)
	}
	return reconciler
}

func reconciliationContext() store.AgentTurnMutationReconciliationContext {
	return store.AgentTurnMutationReconciliationContext{
		WorkflowID: "workflow-1", Repository: store.AgentTurnRepository{ID: 9123, Owner: "acme", Name: "widgets"},
		Issue: store.AgentTurnIssue{ID: 456, Number: 12}, Role: workflow.RoleDeveloper,
		ChangeProposal: &store.AgentTurnChangeProposal{
			ID: "proposal-1", PullRequestID: 654, PullRequestNumber: 23, BaseRef: "main",
			BaseSHA: "1123456789abcdef0123456789abcdef01234567", HeadRef: "omnigrex/issue-12", HeadSHA: productionHeadSHA,
		},
		Turn: store.AgentTurn{
			AgentTurnSpec: store.AgentTurnSpec{ChangeProposalID: "proposal-1", ExpectedHeadSHA: productionHeadSHA},
			ID:            "turn-1", AgentAssignmentID: "assignment-1", ExecutionEpoch: 4, CreatedAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		},
	}
}

func reconciliationMutation(tool, request string) store.MutationReservation {
	var arguments struct {
		OperationID string `json:"operation_id"`
	}
	_ = json.Unmarshal([]byte(request), &arguments)
	return store.MutationReservation{
		ID: reconciliationMutationID, AgentTurnID: "turn-1", ExecutionEpoch: 4, InvocationNumber: 1,
		OperationID: arguments.OperationID, ToolName: tool, Request: json.RawMessage(request), State: store.MutationUnknown,
		ExternalService: "github", ExpectedSHA: productionHeadSHA,
	}
}

func reconciliationMarker(t *testing.T, operationID string) string {
	t.Helper()
	marker, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: operationID})
	if err != nil {
		t.Fatal(err)
	}
	return marker
}

type reconciliationGitHub struct {
	*backendGitHub
	pullRequests     []githubapi.PullRequest
	issueComments    []githubapi.IssueComment
	reviews          []githubapi.Review
	err              error
	credential       string
	issueNumber      int
	reviewNumber     int
	pullRequestQuery githubapi.ListPullRequestsRequest
}

func (api *reconciliationGitHub) backend() *backendGitHub {
	if api.backendGitHub == nil {
		api.backendGitHub = &backendGitHub{}
	}
	return api.backendGitHub
}

func (api *reconciliationGitHub) GetIssue(ctx context.Context, credential, owner, repository string, number int) (githubapi.Issue, error) {
	return api.backend().GetIssue(ctx, credential, owner, repository, number)
}

func (api *reconciliationGitHub) GetPullRequest(ctx context.Context, credential, owner, repository string, number int) (githubapi.PullRequest, error) {
	return api.backend().GetPullRequest(ctx, credential, owner, repository, number)
}

func (api *reconciliationGitHub) ListPullRequests(_ context.Context, credential, _, _ string, request githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error) {
	api.credential, api.pullRequestQuery = credential, request
	return api.pullRequests, api.err
}

func (api *reconciliationGitHub) ListPullRequestFiles(ctx context.Context, credential, owner, repository string, number int) ([]githubapi.PullRequestFile, error) {
	return api.backend().ListPullRequestFiles(ctx, credential, owner, repository, number)
}

func (api *reconciliationGitHub) ListIssueComments(_ context.Context, credential, _, _ string, number int) ([]githubapi.IssueComment, error) {
	api.credential, api.issueNumber = credential, number
	return api.issueComments, api.err
}

func (api *reconciliationGitHub) ListPullRequestReviews(_ context.Context, credential, _, _ string, number int) ([]githubapi.Review, error) {
	api.credential, api.reviewNumber = credential, number
	return api.reviews, api.err
}

func (api *reconciliationGitHub) ListReviewThreads(ctx context.Context, credential, owner, repository string, number int) ([]githubapi.ReviewThread, error) {
	return api.backend().ListReviewThreads(ctx, credential, owner, repository, number)
}

func (api *reconciliationGitHub) GetCheckRuns(ctx context.Context, credential, owner, repository, head string) ([]githubapi.CheckRun, error) {
	return api.backend().GetCheckRuns(ctx, credential, owner, repository, head)
}

func (api *reconciliationGitHub) OpenPullRequest(ctx context.Context, credential, owner, repository string, request githubapi.OpenPullRequestRequest) (githubapi.PullRequest, error) {
	return api.backend().OpenPullRequest(ctx, credential, owner, repository, request)
}

func (api *reconciliationGitHub) CreateIssueComment(ctx context.Context, credential, owner, repository string, number int, request githubapi.CommentRequest) (githubapi.IssueComment, error) {
	return api.backend().CreateIssueComment(ctx, credential, owner, repository, number, request)
}

func (api *reconciliationGitHub) CreatePullRequestComment(ctx context.Context, credential, owner, repository string, number int, request githubapi.CommentRequest) (githubapi.IssueComment, error) {
	return api.backend().CreatePullRequestComment(ctx, credential, owner, repository, number, request)
}

func (api *reconciliationGitHub) SubmitReview(ctx context.Context, credential, owner, repository string, number int, request githubapi.ReviewRequest) (githubapi.Review, error) {
	return api.backend().SubmitReview(ctx, credential, owner, repository, number, request)
}

type reconciliationPublications struct {
	input  workspace.PublicationReconciliation
	result workspace.PublicationReconciliationResult
	err    error
}

func (publications *reconciliationPublications) ReconcilePublication(_ context.Context, input workspace.PublicationReconciliation) (workspace.PublicationReconciliationResult, error) {
	publications.input = input
	return publications.result, publications.err
}
