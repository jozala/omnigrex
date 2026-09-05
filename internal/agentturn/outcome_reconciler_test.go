package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

const (
	outcomeWorkflowID   = "10000000-0000-4000-8000-000000000001"
	outcomeAssignmentID = "20000000-0000-4000-8000-000000000001"
	outcomeSessionID    = "30000000-0000-4000-8000-000000000001"
	outcomeTurnID       = "40000000-0000-4000-8000-000000000001"
	outcomeJobID        = "50000000-0000-4000-8000-000000000001"
	outcomeHead         = "1111111111111111111111111111111111111111"
	outcomeNewHead      = "2222222222222222222222222222222222222222"
	outcomeBaseSHA      = "3333333333333333333333333333333333333333"
	outcomeCredential   = "installation-secret"
)

var outcomeObservedAt = time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)

func TestOutcomeReconcilerAcceptsDeveloperRequestReviewFromDurableAndFreshEvidence(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleDeveloper, false)
	ledger := []store.MutationReservation{
		outcomeSucceededMutation(1, mcp.ToolOpenPR, "open-1",
			`{"operation_id":"open-1","title":"Change","body":"Ready"}`,
			`{"pull_request_id":901,"node_id":"PR_node","number":23,"html_url":"https://github.test/acme/widgets/pull/23","head_sha":"`+outcomeHead+`"}`,
			"github", "41:feature/work:main", outcomeHead),
		outcomeSucceededMutation(2, mcp.ToolRequestReview, "review-1",
			`{"operation_id":"review-1","summary":"Ready"}`,
			`{"outcome":"REVIEW_REQUESTED","pull_request_id":901,"pull_request_number":23,"head_sha":"`+outcomeHead+`"}`,
			"omnigrex", "41:feature/work", outcomeHead),
	}
	storeAPI := &outcomeStore{mutations: ledger}
	github := &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead)}
	reconciler := newOutcomeReconciler(t, storeAPI, github)

	observation, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if observation.Outcome != workflow.TurnOutcomeChangeProposalReady || observation.Completion.Status != store.AgentTurnSucceeded {
		t.Fatalf("Reconcile() observation = %#v", observation)
	}
	proposal := observation.ChangeProposal
	if proposal == nil || proposal.PullRequestID != 901 || proposal.PullRequestNumber != 23 || proposal.PullRequestNodeID != "PR_node" ||
		proposal.HeadSHA != outcomeHead || proposal.HeadRef != "feature/work" || proposal.BaseRef != "main" || proposal.RepositoryID != 41 {
		t.Fatalf("Reconcile() Change Proposal = %#v", proposal)
	}
	if github.getCredential != outcomeCredential || github.getOwner != "acme" || github.getRepository != "widgets" || github.getNumber != 23 {
		t.Errorf("GetPullRequest scope = credential %q, %s/%s#%d", github.getCredential, github.getOwner, github.getRepository, github.getNumber)
	}
}

func TestOutcomeReconcilerRejectsDeveloperPullRequestMismatchAndDirtyTree(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*agentturn.OutcomeReconciliation, *outcomeGitHub)
		want   string
	}{
		{name: "wrong current head", mutate: func(_ *agentturn.OutcomeReconciliation, github *outcomeGitHub) {
			github.pullRequest.Head.SHA = outcomeNewHead
		}, want: "does not match"},
		{name: "wrong repository label", mutate: func(_ *agentturn.OutcomeReconciliation, github *outcomeGitHub) {
			github.pullRequest.Head.Label = "other:feature/work"
		}, want: "does not match"},
		{name: "wrong base", mutate: func(_ *agentturn.OutcomeReconciliation, github *outcomeGitHub) {
			github.pullRequest.Base.Ref = "release"
			github.pullRequest.Base.Label = "acme:release"
		}, want: "does not match"},
		{name: "dirty normalized tree", mutate: func(request *agentturn.OutcomeReconciliation, _ *outcomeGitHub) {
			if err := os.WriteFile(filepath.Join(request.Paths.Workspace, "unpublished.txt"), []byte("dirty"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "unpublished"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := outcomeRequest(t, workflow.RoleDeveloper, true)
			mutation := outcomeRequestReviewMutation(1)
			storeAPI := &outcomeStore{mutations: []store.MutationReservation{mutation}}
			github := &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead)}
			test.mutate(&request, github)

			observation, err := newOutcomeReconciler(t, storeAPI, github).Reconcile(context.Background(), request)
			assertOutcomeInfrastructureFailure(t, observation, err, store.AgentTurnFailed, test.want)
		})
	}
}

func TestOutcomeReconcilerAcceptsReviewerReviewAndRetrievesExistingIdentity(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	review := outcomeReview(outcomeHead, 701)
	existing := &workflow.ReviewIdentity{ID: 801, NodeID: "PRR_node", ChangeProposalID: 901, ActorID: 701, HeadSHA: outcomeHead}
	storeAPI := &outcomeStore{mutations: []store.MutationReservation{outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeHead, 701)}, existing: existing}
	github := &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead), reviews: []githubapi.Review{review}}

	observation, err := newOutcomeReconciler(t, storeAPI, github).Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if observation.Outcome != workflow.TurnOutcomeApproved || observation.Completion.Status != store.AgentTurnSucceeded ||
		observation.Review == nil || *observation.Review != *existing || observation.ExistingReview != existing ||
		observation.AuthorizedReviewerActorID != 701 {
		t.Fatalf("Reconcile() observation = %#v", observation)
	}
	if storeAPI.reviewRepositoryID != 41 || storeAPI.reviewID != 801 || github.listCredential != outcomeCredential {
		t.Errorf("review corroboration scope = store(%d,%d), GitHub credential %q", storeAPI.reviewRepositoryID, storeAPI.reviewID, github.listCredential)
	}
}

func TestOutcomeReconcilerExposesRacedReviewerHeadWithoutConsumingTheReviewEvidence(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	storeAPI := &outcomeStore{mutations: []store.MutationReservation{outcomeSubmitReviewMutation(1, "REQUEST_CHANGES", "CHANGES_REQUESTED", outcomeHead, 701)}}
	github := &outcomeGitHub{pullRequest: outcomePullRequest(outcomeNewHead), reviews: []githubapi.Review{outcomeReview(outcomeHead, 701)}}
	github.reviews[0].State = "CHANGES_REQUESTED"

	observation, err := newOutcomeReconciler(t, storeAPI, github).Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if observation.Outcome != workflow.TurnOutcomeChangesRequested || observation.Review == nil || observation.Review.HeadSHA != outcomeHead ||
		observation.ChangeProposal == nil || observation.ChangeProposal.HeadSHA != outcomeNewHead {
		t.Fatalf("raced-head observation = %#v", observation)
	}
}

func TestOutcomeReconcilerRejectsWrongReviewerActorAndSubmittedHead(t *testing.T) {
	tests := []struct {
		name     string
		mutation store.MutationReservation
		review   githubapi.Review
	}{
		{name: "wrong actor", mutation: outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeHead, 701), review: outcomeReview(outcomeHead, 702)},
		{name: "wrong submitted head", mutation: outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeNewHead, 701), review: outcomeReview(outcomeNewHead, 701)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := outcomeRequest(t, workflow.RoleReviewer, true)
			storeAPI := &outcomeStore{mutations: []store.MutationReservation{test.mutation}}
			github := &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead), reviews: []githubapi.Review{test.review}}
			observation, err := newOutcomeReconciler(t, storeAPI, github).Reconcile(context.Background(), request)
			assertOutcomeInfrastructureFailure(t, observation, err, store.AgentTurnFailed, "review")
		})
	}
}

func TestOutcomeReconcilerRejectsDuplicateAndConflictingTerminalIntents(t *testing.T) {
	tests := []struct {
		name      string
		mutations []store.MutationReservation
	}{
		{name: "duplicate", mutations: []store.MutationReservation{outcomeRequestReviewMutation(1), outcomeRequestReviewMutation(2)}},
		{name: "conflicting", mutations: []store.MutationReservation{outcomeRequestReviewMutation(1), outcomeBlockedMutation(2, "blocked", "details")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := outcomeRequest(t, workflow.RoleDeveloper, true)
			observation, err := newOutcomeReconciler(t, &outcomeStore{mutations: test.mutations}, &outcomeGitHub{}).Reconcile(context.Background(), request)
			assertOutcomeInfrastructureFailure(t, observation, err, store.AgentTurnFailed, "duplicate or conflicting")
		})
	}
}

func TestOutcomeReconcilerAcceptsSanitizedBlockedEvidenceWithoutGitHub(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	storeAPI := &outcomeStore{mutations: []store.MutationReservation{outcomeBlockedMutation(1, " Cannot\nproceed ", " Needs\taccess ")}}
	github := &outcomeGitHub{}

	observation, err := newOutcomeReconciler(t, storeAPI, github).Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if observation.Outcome != workflow.TurnOutcomeBlocked || observation.Completion.Status != store.AgentTurnSucceeded ||
		observation.Diagnostic != "Cannot proceed: Needs access" {
		t.Fatalf("blocked observation = %#v", observation)
	}
	if github.getCalls != 0 || github.listCalls != 0 {
		t.Fatalf("blocked reconciliation made GitHub calls: get=%d list=%d", github.getCalls, github.listCalls)
	}
}

func TestOutcomeReconcilerMapsMissingIntentAndACPFailuresToTerminalStatuses(t *testing.T) {
	endTurn := acp.PromptResponse{StopReason: acp.StopReasonEndTurn}
	tests := []struct {
		name     string
		response *acp.PromptResponse
		failure  agentturn.PromptErrorClassification
		status   store.AgentTurnStatus
	}{
		{name: "end turn without intent", response: &endTurn, status: store.AgentTurnFailed},
		{name: "refusal", response: &acp.PromptResponse{StopReason: acp.StopReasonRefusal}, status: store.AgentTurnFailed},
		{name: "max tokens", response: &acp.PromptResponse{StopReason: acp.StopReasonMaxTokens}, status: store.AgentTurnFailed},
		{name: "max requests", response: &acp.PromptResponse{StopReason: acp.StopReasonMaxTurnRequests}, status: store.AgentTurnFailed},
		{name: "ACP cancellation", response: &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, status: store.AgentTurnInterrupted},
		{name: "deadline error", failure: agentturn.PromptErrorDeadline, status: store.AgentTurnTimedOut},
		{name: "cancellation error", failure: agentturn.PromptErrorCancellation, status: store.AgentTurnInterrupted},
		{name: "other error", failure: agentturn.PromptErrorFailure, status: store.AgentTurnFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := outcomeRequest(t, workflow.RoleDeveloper, true)
			request.PromptResponse = test.response
			request.PromptError = test.failure
			observation, err := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(context.Background(), request)
			assertOutcomeInfrastructureFailure(t, observation, err, test.status, "ACP prompt")
		})
	}
	if agentturn.ClassifyPromptError(context.DeadlineExceeded) != agentturn.PromptErrorDeadline ||
		agentturn.ClassifyPromptError(context.Canceled) != agentturn.PromptErrorCancellation ||
		agentturn.ClassifyPromptError(errors.New("transport")) != agentturn.PromptErrorFailure {
		t.Fatal("ClassifyPromptError() returned an incorrect classification")
	}

	request := outcomeRequest(t, workflow.RoleDeveloper, true)
	request.PromptResponse = &acp.PromptResponse{StopReason: acp.StopReasonRefusal}
	observation, err := newOutcomeReconciler(t, &outcomeStore{mutations: []store.MutationReservation{outcomeRequestReviewMutation(1)}}, &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead)}).Reconcile(context.Background(), request)
	assertOutcomeInfrastructureFailure(t, observation, err, store.AgentTurnFailed, "refused")
}

func TestOutcomeReconcilerRejectsMalformedAndUnsettledLedgers(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleDeveloper, true)
	malformedSuccess := outcomeRequestReviewMutation(1)
	malformedSuccess.Result = json.RawMessage(`{"outcome":"REVIEW_REQUESTED"}`)
	malformedFailure := outcomeFailedMutation(1, mcp.ToolRequestReview)
	malformedFailure.Result = json.RawMessage(`{}`)
	for _, test := range []struct {
		name     string
		mutation store.MutationReservation
	}{
		{name: "malformed succeeded intent", mutation: malformedSuccess},
		{name: "malformed terminal failure", mutation: malformedFailure},
		{name: "wrong turn", mutation: func() store.MutationReservation {
			mutation := outcomeRequestReviewMutation(1)
			mutation.AgentTurnID = "another-turn"
			return mutation
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation, err := newOutcomeReconciler(t, &outcomeStore{mutations: []store.MutationReservation{test.mutation}}, &outcomeGitHub{}).Reconcile(context.Background(), request)
			assertOutcomeInfrastructureFailure(t, observation, err, store.AgentTurnFailed, "malformed")
		})
	}

	for _, barrier := range []error{store.ErrAgentTurnMutationsUnsettled, store.ErrMutationAdmissionClosed, store.ErrAgentTurnFenceLost} {
		_, err := newOutcomeReconciler(t, &outcomeStore{err: fmt.Errorf("wrapped: %w", barrier)}, &outcomeGitHub{}).Reconcile(context.Background(), request)
		if !errors.Is(err, barrier) {
			t.Errorf("Reconcile() ledger barrier error = %v, want %v", err, barrier)
		}
	}
	unknown := outcomeRequestReviewMutation(1)
	unknown.State = store.MutationUnknown
	unknown.Result = nil
	unknown.FinishedAt = nil
	_, err := newOutcomeReconciler(t, &outcomeStore{mutations: []store.MutationReservation{unknown}}, &outcomeGitHub{}).Reconcile(context.Background(), request)
	if !errors.Is(err, store.ErrAgentTurnMutationsUnsettled) {
		t.Fatalf("Reconcile() returned UNKNOWN mutation error = %v, want ErrAgentTurnMutationsUnsettled", err)
	}
}

func TestOutcomeReconcilerNeverLeaksRepositoryCredential(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleDeveloper, true)
	request.PromptResponse = nil
	request.PromptError = agentturn.PromptErrorFailure
	request.PromptDiagnostic = "transport exposed " + outcomeCredential + strings.Repeat("x", 5000)
	observation, err := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertNoOutcomeCredential(t, observation, err)
	if len([]rune(observation.Diagnostic)) > 4096 || !strings.Contains(observation.Diagnostic, "[REDACTED]") {
		t.Fatalf("credential-safe diagnostic length/content = %d, %q", len([]rune(observation.Diagnostic)), observation.Diagnostic)
	}

	request = outcomeRequest(t, workflow.RoleDeveloper, true)
	github := &outcomeGitHub{err: errors.New("GitHub rejected " + outcomeCredential)}
	observation, err = newOutcomeReconciler(t, &outcomeStore{mutations: []store.MutationReservation{outcomeRequestReviewMutation(1)}}, github).Reconcile(context.Background(), request)
	assertOutcomeInfrastructureFailure(t, observation, err, store.AgentTurnFailed, "observation failed")
	assertNoOutcomeCredential(t, observation, err)

	_, err = newOutcomeReconciler(t, &outcomeStore{err: errors.New("store exposed " + outcomeCredential)}, &outcomeGitHub{}).Reconcile(context.Background(), request)
	if !errors.Is(err, agentturn.ErrOutcomeReconciliationUnavailable) || strings.Contains(err.Error(), outcomeCredential) {
		t.Fatalf("credential-safe Store error = %v", err)
	}
}

func TestOutcomeReconcilerRejectsRoleSpecificTerminalIntent(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	observation, err := newOutcomeReconciler(t, &outcomeStore{mutations: []store.MutationReservation{outcomeRequestReviewMutation(1)}}, &outcomeGitHub{}).Reconcile(context.Background(), request)
	assertOutcomeInfrastructureFailure(t, observation, err, store.AgentTurnFailed, "Reviewer")
}

type outcomeStore struct {
	mutations          []store.MutationReservation
	err                error
	existing           *workflow.ReviewIdentity
	reviewRepositoryID int64
	reviewID           int64
}

func (storeAPI *outcomeStore) ListAgentTurnMutationInvocations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error) {
	return storeAPI.mutations, storeAPI.err
}

func (storeAPI *outcomeStore) GetChangeProposalReview(_ context.Context, repositoryID, reviewID int64) (*workflow.ReviewIdentity, error) {
	storeAPI.reviewRepositoryID = repositoryID
	storeAPI.reviewID = reviewID
	return storeAPI.existing, storeAPI.err
}

type outcomeGitHub struct {
	pullRequest                            githubapi.PullRequest
	reviews                                []githubapi.Review
	err                                    error
	getCredential, getOwner, getRepository string
	listCredential                         string
	getNumber, getCalls, listCalls         int
}

func (github *outcomeGitHub) GetPullRequest(_ context.Context, credential, owner, repository string, number int) (githubapi.PullRequest, error) {
	github.getCredential, github.getOwner, github.getRepository, github.getNumber = credential, owner, repository, number
	github.getCalls++
	return github.pullRequest, github.err
}

func (github *outcomeGitHub) ListPullRequestReviews(_ context.Context, credential, _, _ string, _ int) ([]githubapi.Review, error) {
	github.listCredential = credential
	github.listCalls++
	return github.reviews, github.err
}

type outcomeClock struct{}

func (outcomeClock) Now() time.Time { return outcomeObservedAt }

func newOutcomeReconciler(t *testing.T, storeAPI agentturn.OutcomeReconcilerStore, github agentturn.OutcomeReconcilerGitHub) *agentturn.OutcomeReconciler {
	t.Helper()
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: storeAPI, GitHub: github, Clock: outcomeClock{}})
	if err != nil {
		t.Fatalf("NewOutcomeReconciler() error = %v", err)
	}
	return reconciler
}

func outcomeRequest(t *testing.T, role workflow.Role, existingProposal bool) agentturn.OutcomeReconciliation {
	t.Helper()
	paths := outcomeCleanPaths(t)
	lease := store.AgentTurnLease{
		AgentTurn: store.AgentTurn{
			AgentTurnSpec: store.AgentTurnSpec{AgentSessionID: outcomeSessionID, WorkflowAttemptID: "60000000-0000-4000-8000-000000000001", ControlRevision: 5},
			ID:            outcomeTurnID, AgentAssignmentID: outcomeAssignmentID, ExecutionEpoch: 7,
		},
		JobLease: store.JobLease{ID: outcomeJobID, WorkflowID: outcomeWorkflowID},
	}
	execution := store.AgentTurnExecutionContext{
		WorkflowID: outcomeWorkflowID,
		Repository: store.AgentTurnRepository{ID: 41, Owner: "acme", Name: "widgets"},
		Issue:      store.AgentTurnIssue{ID: 51, Number: 61},
		Assignment: store.AgentAssignment{ID: outcomeAssignmentID, WorkflowID: outcomeWorkflowID, Role: role},
		Session:    store.AgentSession{ID: outcomeSessionID, AgentAssignmentID: outcomeAssignmentID},
		Turn:       lease.AgentTurn,
	}
	if existingProposal {
		proposal := &store.AgentTurnChangeProposal{
			ID: "70000000-0000-4000-8000-000000000001", PullRequestID: 901, PullRequestNumber: 23,
			BaseRef: "main", BaseSHA: outcomeBaseSHA, HeadRef: "feature/work", HeadSHA: outcomeHead,
		}
		execution.ChangeProposal = proposal
		execution.Turn.ChangeProposalID = proposal.ID
		execution.Turn.ExpectedHeadSHA = outcomeHead
		lease.ChangeProposalID = proposal.ID
		lease.ExpectedHeadSHA = outcomeHead
	}
	response := acp.PromptResponse{StopReason: acp.StopReasonEndTurn}
	return agentturn.OutcomeReconciliation{
		Lease: lease, Execution: execution, PromptResponse: &response,
		RepositoryCredential: outcomeCredential, Paths: paths,
	}
}

func outcomeCleanPaths(t *testing.T) workspace.Paths {
	t.Helper()
	root := t.TempDir()
	paths := workspace.Paths{Workspace: filepath.Join(root, "workspace"), Publication: filepath.Join(root, "publication")}
	for _, path := range []string{paths.Workspace, paths.Publication} {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("same\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, ".git", "workspace-only"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Publication, ".git", "publication-only"), []byte("also ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	return paths
}

func outcomeSucceededMutation(number int64, tool, operationID, request, result, service, resource, expectedSHA string) store.MutationReservation {
	finished := outcomeObservedAt.Add(-time.Minute)
	return store.MutationReservation{
		ID: fmt.Sprintf("mutation-%d", number), AgentTurnID: outcomeTurnID, ExecutionEpoch: 7,
		InvocationNumber: number, OperationID: operationID, ToolName: tool,
		Request: json.RawMessage(request), State: store.MutationSucceeded,
		ExternalService: service, ExternalResourceID: resource, ExpectedSHA: expectedSHA,
		Result: json.RawMessage(result), AdmittedAt: finished.Add(-time.Minute), FinishedAt: &finished,
	}
}

func outcomeFailedMutation(number int64, tool string) store.MutationReservation {
	finished := outcomeObservedAt.Add(-time.Minute)
	return store.MutationReservation{
		ID: fmt.Sprintf("mutation-%d", number), AgentTurnID: outcomeTurnID, ExecutionEpoch: 7,
		InvocationNumber: number, OperationID: fmt.Sprintf("failed-%d", number), ToolName: tool,
		Request: json.RawMessage(`{"operation_id":"failed"}`), State: store.MutationFailed,
		LastError: "mutation failed", AdmittedAt: finished.Add(-time.Minute), FinishedAt: &finished,
	}
}

func outcomeRequestReviewMutation(number int64) store.MutationReservation {
	operationID := fmt.Sprintf("review-%d", number)
	return outcomeSucceededMutation(number, mcp.ToolRequestReview, operationID,
		fmt.Sprintf(`{"operation_id":%q,"summary":"Ready"}`, operationID),
		`{"outcome":"REVIEW_REQUESTED","pull_request_id":901,"pull_request_number":23,"head_sha":"`+outcomeHead+`"}`,
		"omnigrex", "41:feature/work", outcomeHead)
}

func outcomeSubmitReviewMutation(number int64, event, state, commit string, actorID int64) store.MutationReservation {
	operationID := fmt.Sprintf("submit-%d", number)
	return outcomeSucceededMutation(number, mcp.ToolSubmitReview, operationID,
		fmt.Sprintf(`{"operation_id":%q,"event":%q,"body":"review","comments":[]}`, operationID, event),
		fmt.Sprintf(`{"review_id":801,"node_id":"PRR_node","state":%q,"commit_id":%q,"actor_id":%d,"html_url":"https://github.test/review/801"}`, state, commit, actorID),
		"github", "41:901", commit)
}

func outcomeBlockedMutation(number int64, reason, details string) store.MutationReservation {
	operationID := fmt.Sprintf("blocked-%d", number)
	request, _ := json.Marshal(map[string]string{"operation_id": operationID, "reason": reason, "details": details})
	result, _ := json.Marshal(map[string]string{"outcome": "BLOCKED", "reason": reason, "details": details})
	return outcomeSucceededMutation(number, mcp.ToolReportBlocked, operationID, string(request), string(result), "omnigrex", outcomeWorkflowID, "")
}

func outcomePullRequest(head string) githubapi.PullRequest {
	return githubapi.PullRequest{
		ID: 901, NodeID: "PR_node", Number: 23, State: "open",
		Head: githubapi.PullRequestBranch{Label: "acme:feature/work", Ref: "feature/work", SHA: head},
		Base: githubapi.PullRequestBranch{Label: "acme:main", Ref: "main", SHA: outcomeBaseSHA},
	}
}

func outcomeReview(commit string, actorID int64) githubapi.Review {
	return githubapi.Review{ID: 801, NodeID: "PRR_node", State: "APPROVED", CommitID: commit, User: githubapi.User{ID: actorID}}
}

func assertOutcomeInfrastructureFailure(t *testing.T, observation store.AgentTurnSettlementObservation, err error, status store.AgentTurnStatus, diagnostic string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if observation.Outcome != workflow.TurnOutcomeInfrastructureFailed || observation.Completion.Status != status ||
		observation.Completion.LastError != observation.Diagnostic || !strings.Contains(observation.Diagnostic, diagnostic) {
		t.Fatalf("infrastructure observation = %#v, want status %s and diagnostic containing %q", observation, status, diagnostic)
	}
}

func assertNoOutcomeCredential(t *testing.T, observation store.AgentTurnSettlementObservation, err error) {
	t.Helper()
	encoded := fmt.Sprintf("%#v %v", observation, err)
	if strings.Contains(encoded, outcomeCredential) {
		t.Fatalf("outcome leaked repository credential: %s", encoded)
	}
}
