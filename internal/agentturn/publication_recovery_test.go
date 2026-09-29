package agentturn_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

const (
	recoveryBase = "0123456789abcdef0123456789abcdef01234567"
	recoveryHead = "1123456789abcdef0123456789abcdef01234567"
)

func TestPublicationRecoveryBindsPriorParticipantPRWithoutRequestingReview(t *testing.T) {
	const publicationID = "10000000-0000-4000-8000-000000000001"
	const prID = "10000000-0000-4000-8000-000000000002"
	marker, err := githubapi.RenderMarker(githubapi.Marker{
		WorkflowID:        "40000000-0000-4000-8000-000000000001",
		AgentAssignmentID: "20000000-0000-4000-8000-000000000001", OperationID: prID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{
		{ID: publicationID, State: store.MutationSucceeded, ToolName: mcp.ToolPublishChanges,
			ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
			Result: json.RawMessage(`{"head":"` + recoveryHead + `","branch":"omnigrex/issue-18","changed":true}`)},
		{ID: "10000000-0000-4000-8000-000000000003", State: store.MutationSucceeded, ToolName: mcp.ToolPublishChanges,
			ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryHead,
			Result: json.RawMessage(`{"head":"` + recoveryHead + `","branch":"omnigrex/issue-18","changed":false}`)},
		{ID: prID, OperationID: "open-18", State: store.MutationSucceeded, ToolName: mcp.ToolOpenPR,
			ExternalService: "github", ExternalResourceID: "41:omnigrex/issue-18:main", ExpectedSHA: recoveryHead,
			Request: json.RawMessage(`{"operation_id":"open-18","title":"Fix it","body":"Ready"}`),
			Result:  json.RawMessage(`{"pull_request_id":901,"node_id":"PR_901","number":19,"html_url":"https://github.com/jozala/omnigrex/pull/19","head_sha":"` + recoveryHead + `"}`)},
	}}
	remote := &publicationRecoveryRemote{head: recoveryHead}
	github := &publicationRecoveryGitHub{pullRequests: []githubapi.PullRequest{{
		ID: 901, NodeID: "PR_901", Number: 19, State: "open", Title: "Fix it", HTMLURL: "https://github.com/jozala/omnigrex/pull/19",
		Body: githubapi.JoinBodyParts("Ready", "Closes #18", marker),
		Head: githubapi.PullRequestBranch{Ref: "omnigrex/issue-18", SHA: recoveryHead},
		Base: githubapi.PullRequestBranch{Ref: "main"},
	}}}
	recovery := agentturn.NewPublicationRecovery(ledger, remote, github)
	lease, execution := recoveryTurn()
	if err := recovery.Recover(context.Background(), lease, execution, "https://github.com/jozala/omnigrex.git", "installation-token", "main"); err != nil {
		t.Fatal(err)
	}
	if ledger.binding.HeadSHA != recoveryHead || ledger.binding.PullRequestID != 901 || ledger.binding.PullRequestNumber != 19 ||
		ledger.binding.SourcePublishMutationID != publicationID || ledger.binding.SourceOpenPRMutationID != prID || remote.preparedHead != recoveryHead {
		t.Fatalf("recovered publication = %#v, publication checkout = %s", ledger.binding, remote.preparedHead)
	}
	github.pullRequests[0].Body = "Ready without operation marker"
	ledger.binding = store.AgentTurnPublication{}
	remote.preparedHead = ""
	if err := recovery.Recover(context.Background(), lease, execution, "https://github.com/jozala/omnigrex.git", "installation-token", "main"); err != agentturn.ErrPublicationConflict || ledger.binding.HeadSHA != "" || remote.preparedHead != "" {
		t.Fatalf("edited PR marker recovery = %v, binding %#v", err, ledger.binding)
	}
}

func TestPublicationRecoveryRejectsUnprovenBranchBeforeBinding(t *testing.T) {
	ledger := &publicationRecoveryStore{}
	remote := &publicationRecoveryRemote{head: recoveryHead}
	recovery := agentturn.NewPublicationRecovery(ledger, remote, &publicationRecoveryGitHub{})
	lease, execution := recoveryTurn()
	err := recovery.Recover(context.Background(), lease, execution, "https://github.com/jozala/omnigrex.git", "installation-token", "main")
	if err != agentturn.ErrPublicationConflict || ledger.binding.HeadSHA != "" || remote.preparedHead != "" {
		t.Fatalf("unproven branch recovery = %v, binding %#v", err, ledger.binding)
	}
}

func TestPublicationRecoveryRejectsHumanPRForRecordedBranch(t *testing.T) {
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{{
		ID: "10000000-0000-4000-8000-000000000001", State: store.MutationSucceeded,
		ToolName: mcp.ToolPublishChanges, ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
		Result: json.RawMessage(`{"head":"` + recoveryHead + `","branch":"omnigrex/issue-18","changed":true}`),
	}}}
	remote := &publicationRecoveryRemote{head: recoveryHead}
	github := &publicationRecoveryGitHub{pullRequests: []githubapi.PullRequest{{
		ID: 999, Number: 99, State: "open", Head: githubapi.PullRequestBranch{Ref: "omnigrex/issue-18", SHA: recoveryHead},
	}}}
	lease, execution := recoveryTurn()
	err := agentturn.NewPublicationRecovery(ledger, remote, github).Recover(context.Background(), lease, execution,
		"https://github.com/jozala/omnigrex.git", "installation-token", "main")
	if err != agentturn.ErrPublicationConflict || ledger.binding.HeadSHA != "" || remote.preparedHead != "" {
		t.Fatalf("human PR recovery = %v, binding %#v", err, ledger.binding)
	}
}

func TestPublicationRecoveryRejectsUnrecordedDescendantCommit(t *testing.T) {
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{{
		ID: "10000000-0000-4000-8000-000000000001", State: store.MutationSucceeded,
		ToolName: mcp.ToolPublishChanges, ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
		Result: json.RawMessage(`{"head":"` + recoveryHead + `","branch":"omnigrex/issue-18","changed":true}`),
	}}}
	remote := &publicationRecoveryRemote{head: "2123456789abcdef0123456789abcdef01234567"}
	lease, execution := recoveryTurn()
	err := agentturn.NewPublicationRecovery(ledger, remote, &publicationRecoveryGitHub{}).Recover(context.Background(), lease, execution,
		"https://github.com/jozala/omnigrex.git", "installation-token", "main")
	if err != agentturn.ErrPublicationConflict || ledger.binding.HeadSHA != "" {
		t.Fatalf("unproven descendant recovery = %v, binding %#v", err, ledger.binding)
	}
}

func TestPublicationRecoveryRejectsBranchAdvancedDuringVerification(t *testing.T) {
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{{
		ID: "10000000-0000-4000-8000-000000000001", State: store.MutationSucceeded,
		ToolName: mcp.ToolPublishChanges, ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
		Result: json.RawMessage(`{"head":"` + recoveryHead + `","branch":"omnigrex/issue-18","changed":true}`),
	}}}
	remote := &publicationRecoveryRemote{heads: []string{recoveryHead, "2123456789abcdef0123456789abcdef01234567"}}
	lease, execution := recoveryTurn()
	err := agentturn.NewPublicationRecovery(ledger, remote, &publicationRecoveryGitHub{}).Recover(context.Background(), lease, execution,
		"https://github.com/jozala/omnigrex.git", "installation-token", "main")
	if err != agentturn.ErrPublicationConflict || ledger.binding.HeadSHA != "" || remote.observeCalls != 2 {
		t.Fatalf("branch advanced during verification = %v, binding %#v, observations %d", err, ledger.binding, remote.observeCalls)
	}
}

func TestPublicationRecoveryResumesPublishedBranchBeforePRCreation(t *testing.T) {
	const publicationID = "10000000-0000-4000-8000-000000000001"
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{{
		ID: publicationID, State: store.MutationSucceeded, ToolName: mcp.ToolPublishChanges,
		ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
		Result: json.RawMessage(`{"head":"` + recoveryHead + `","branch":"omnigrex/issue-18","changed":true}`),
	}}}
	remote := &publicationRecoveryRemote{head: recoveryHead}
	lease, execution := recoveryTurn()
	if err := agentturn.NewPublicationRecovery(ledger, remote, &publicationRecoveryGitHub{}).Recover(context.Background(), lease, execution,
		"https://github.com/jozala/omnigrex.git", "installation-token", "main"); err != nil {
		t.Fatal(err)
	}
	if ledger.binding.HeadSHA != recoveryHead || ledger.binding.PullRequestID != 0 || ledger.binding.SourceOpenPRMutationID != "" ||
		ledger.binding.SourcePublishMutationID != publicationID || remote.preparedHead != recoveryHead {
		t.Fatalf("recovered branch without PR = %#v", ledger.binding)
	}
}

func TestPublicationRecoveryBindsRecordedMultiCommitTipAfterWorkspaceReplacement(t *testing.T) {
	const publicationID = "10000000-0000-4000-8000-000000000001"
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{{
		ID: publicationID, State: store.MutationSucceeded, ToolName: mcp.ToolPublishChanges,
		ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
		ProposedSHA: recoveryHead, HistoryPublication: true,
		Result: json.RawMessage(`{"head":"` + recoveryHead + `","branch":"omnigrex/issue-18","changed":true}`),
	}}}
	remote := &publicationRecoveryRemote{head: recoveryHead}
	lease, execution := recoveryTurn()
	recovery := agentturn.NewPublicationRecovery(ledger, remote, &publicationRecoveryGitHub{})
	if err := recovery.Recover(context.Background(), lease, execution, "https://github.com/jozala/omnigrex.git", "installation-token", "main"); err != nil {
		t.Fatal(err)
	}
	if ledger.binding.HeadSHA != recoveryHead || remote.preparedHead != recoveryHead || remote.request.ProposedRevision != recoveryHead {
		t.Fatalf("recovered candidate = %#v, %#v", ledger.binding, remote.request)
	}
	ledger.mutations[0].ProposedSHA = "2123456789abcdef0123456789abcdef01234567"
	ledger.binding = store.AgentTurnPublication{}
	remote.preparedHead = ""
	if err := recovery.Recover(context.Background(), lease, execution, "https://github.com/jozala/omnigrex.git", "installation-token", "main"); err != agentturn.ErrPublicationConflict || ledger.binding.HeadSHA != "" {
		t.Fatalf("mismatched candidate recovery = %v, binding %#v", err, ledger.binding)
	}
}

func TestPublicationRecoveryAcceptsSHA256CommitIdentity(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{{
		ID: "10000000-0000-4000-8000-000000000001", State: store.MutationSucceeded,
		ToolName: mcp.ToolPublishChanges, ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
		Result: json.RawMessage(`{"head":"` + head + `","branch":"omnigrex/issue-18","changed":true}`),
	}}}
	remote := &publicationRecoveryRemote{head: head}
	lease, execution := recoveryTurn()
	err := agentturn.NewPublicationRecovery(ledger, remote, &publicationRecoveryGitHub{}).Recover(context.Background(), lease, execution,
		"https://github.com/jozala/omnigrex.git", "installation-token", "main")
	if err != nil || ledger.binding.HeadSHA != head {
		t.Fatalf("SHA-256 publication = %#v, error %v", ledger.binding, err)
	}
}

func TestPublicationRecoveryIgnoresNoChangePublishWhenBranchNeverExisted(t *testing.T) {
	ledger := &publicationRecoveryStore{mutations: []store.MutationReservation{{
		ID: "10000000-0000-4000-8000-000000000004", State: store.MutationSucceeded,
		ToolName: mcp.ToolPublishChanges, ExternalService: "git", ExternalResourceID: "41:omnigrex/issue-18", ExpectedSHA: recoveryBase,
		Result: json.RawMessage(`{"head":"` + recoveryBase + `","branch":"omnigrex/issue-18","changed":false}`),
	}}}
	remote := &publicationRecoveryRemote{}
	lease, execution := recoveryTurn()
	if err := agentturn.NewPublicationRecovery(ledger, remote, &publicationRecoveryGitHub{}).Recover(context.Background(), lease, execution,
		"https://github.com/jozala/omnigrex.git", "installation-token", "main"); err != nil || ledger.binding.HeadSHA != "" {
		t.Fatalf("prior no-change publication = %v, binding %#v", err, ledger.binding)
	}
}

func recoveryTurn() (store.AgentTurnLease, store.AgentTurnExecutionContext) {
	lease := store.AgentTurnLease{AgentTurn: store.AgentTurn{
		ID: "30000000-0000-4000-8000-000000000001", AgentAssignmentID: "20000000-0000-4000-8000-000000000001", ExecutionEpoch: 4,
	}}
	return lease, store.AgentTurnExecutionContext{
		WorkflowID: "40000000-0000-4000-8000-000000000001",
		Repository: store.AgentTurnRepository{ID: 41, Owner: "jozala", Name: "omnigrex"},
		Issue:      store.AgentTurnIssue{ID: 18, Number: 18},
		Assignment: store.AgentAssignment{ID: lease.AgentAssignmentID, Role: workflow.RoleDeveloper},
		Turn:       lease.AgentTurn,
	}
}

type publicationRecoveryStore struct {
	mutations []store.MutationReservation
	binding   store.AgentTurnPublication
}

func (recovery *publicationRecoveryStore) ListParticipantPublicationMutations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error) {
	return recovery.mutations, nil
}

func (recovery *publicationRecoveryStore) BindAgentTurnPublication(_ context.Context, _ store.AgentTurnLease, binding store.AgentTurnPublication) error {
	recovery.binding = binding
	return nil
}

type publicationRecoveryRemote struct {
	head, preparedHead string
	heads              []string
	observeCalls       int
	request            workspace.PublicationReconciliation
}

func (remote *publicationRecoveryRemote) ObserveRemoteBranch(context.Context, string, string, string) (string, error) {
	remote.observeCalls++
	if len(remote.heads) != 0 {
		index := min(remote.observeCalls-1, len(remote.heads)-1)
		return remote.heads[index], nil
	}
	return remote.head, nil
}

func (remote *publicationRecoveryRemote) ReconcilePublication(_ context.Context, request workspace.PublicationReconciliation) (workspace.PublicationReconciliationResult, error) {
	remote.request = request
	found := remote.head
	if found == "" {
		found = recoveryHead
	}
	return workspace.PublicationReconciliationResult{Outcome: workspace.PublicationReconciliationFound, Head: found}, nil
}

func (remote *publicationRecoveryRemote) PrepareRecoveredPublication(_ context.Context, _, _, _, head string) error {
	remote.preparedHead = head
	return nil
}

type publicationRecoveryGitHub struct{ pullRequests []githubapi.PullRequest }

func (api *publicationRecoveryGitHub) ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error) {
	return api.pullRequests, nil
}
