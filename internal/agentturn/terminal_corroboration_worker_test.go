package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

type redirectTestStore struct {
	lease         *store.JobLease
	pending       store.TerminalCorroborationContext
	retryCalls    int
	handoffCalls  int
	redirectCalls int
	redirectErr   error
}

func (database *redirectTestStore) ClaimJobKind(_ context.Context, _ string, kind, _ string, _ time.Duration) (*store.JobLease, error) {
	if kind == store.VerifyTerminalIntentJobKind {
		return nil, nil
	}
	return database.lease, nil
}

func (database *redirectTestStore) HeartbeatJob(context.Context, store.JobLease, time.Duration) error {
	return nil
}

func (database *redirectTestStore) GetTerminalCorroborationContext(context.Context, store.JobLease) (store.TerminalCorroborationContext, error) {
	return database.pending, nil
}

func (database *redirectTestStore) RetryTerminalCorroboration(context.Context, store.JobLease, string, time.Duration) error {
	database.retryCalls++
	return nil
}

func (database *redirectTestStore) SettleTerminalCorroboration(context.Context, store.JobLease, store.AgentTurnSettlementObservation) (store.AgentTurnSettlement, error) {
	return store.AgentTurnSettlement{}, errors.New("unexpected terminal corroboration settlement")
}

func (database *redirectTestStore) SettleTerminalRevalidation(context.Context, store.JobLease, int64, store.AgentTurnSettlementObservation) error {
	return errors.New("unexpected terminal revalidation settlement")
}

func (database *redirectTestStore) CompleteTerminalCorroborationHandoff(context.Context, store.JobLease, workflow.Reason, string) (store.AgentTurnSettlement, error) {
	return store.AgentTurnSettlement{}, errors.New("unexpected terminal corroboration handoff")
}

func (database *redirectTestStore) CompleteTerminalRevalidationHandoff(_ context.Context, _ store.JobLease, _ workflow.Reason, _ string) error {
	database.handoffCalls++
	return nil
}

func (database *redirectTestStore) RedirectRevalidationToFreshTurn(context.Context, store.JobLease, int64, string, int64, int64) error {
	database.redirectCalls++
	return database.redirectErr
}

type redirectTestCredentials struct {
	calls int
}

func (provider *redirectTestCredentials) RepositoryCredential(context.Context, string, string) (string, error) {
	provider.calls++
	return "installation-token", nil
}

type redirectTestGitHub struct {
	pullRequest githubapi.PullRequest
	err         error
	calls       int
}

func (api *redirectTestGitHub) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	api.calls++
	return api.pullRequest, api.err
}

func (api *redirectTestGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	api.calls++
	return nil, errors.New("unexpected Pull Request review read")
}

type redirectTestOutcomeStore struct{}

func (redirectTestOutcomeStore) ListAgentTurnMutationInvocations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error) {
	return nil, nil
}

func (redirectTestOutcomeStore) ListParticipantPublicationMutations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error) {
	return nil, nil
}

func (redirectTestOutcomeStore) GetChangeProposalReview(context.Context, int64, int64) (*workflow.ReviewIdentity, error) {
	return nil, nil
}

type redirectTestWorkspace struct {
	calls int
}

func (paths *redirectTestWorkspace) Paths(string) (workspace.Paths, error) {
	paths.calls++
	return workspace.Paths{}, nil
}

func redirectTestPending() store.TerminalCorroborationContext {
	return store.TerminalCorroborationContext{
		Checkpoint: store.TerminalCorroboration{
			TurnID: "turn-1", ExecutionEpoch: 1, WorkflowID: "workflow-1",
			WorkflowAttemptID: "attempt-old", PromptOutcome: json.RawMessage(`{"stop_reason":"end_turn"}`),
			PendingSince: time.Now(),
		},
		Execution: store.AgentTurnExecutionContext{
			WorkflowID: "workflow-1",
			Repository: store.AgentTurnRepository{ID: 91, Owner: "acme", Name: "widgets"},
			Assignment: store.AgentAssignment{Status: store.AgentAssignmentActive, Role: workflow.RoleDeveloper},
			Turn: store.AgentTurn{
				AgentTurnSpec: store.AgentTurnSpec{ExpectedHeadSHA: "head-a", ControlRevision: 1},
				ID:            "turn-1", ExecutionEpoch: 1,
			},
			ChangeProposal: &store.AgentTurnChangeProposal{
				PullRequestID: 64, PullRequestNumber: 12, HeadRef: "feature", BaseRef: "main",
			},
		},
		WorkflowRevision: 7,
	}
}

func redirectTestLease() *store.JobLease {
	return &store.JobLease{
		Job: store.Job{
			ID: "job-1", Queue: "agent-turn-recovery", Kind: store.RevalidateTerminalIntentJobKind,
			Status: store.JobLeased, WorkflowID: "workflow-1", WorkflowAttemptID: "attempt-new",
		},
		Attempt: 1,
	}
}

func redirectTestWorker(t *testing.T, database *redirectTestStore, api *redirectTestGitHub) (*TerminalCorroborationWorker, *redirectTestCredentials, *redirectTestWorkspace) {
	t.Helper()
	outcomes, err := NewOutcomeReconciler(OutcomeReconcilerConfig{Store: redirectTestOutcomeStore{}, GitHub: api})
	if err != nil {
		t.Fatalf("NewOutcomeReconciler() error = %v", err)
	}
	credentials := &redirectTestCredentials{}
	paths := &redirectTestWorkspace{}
	worker, err := NewTerminalCorroborationWorker(database, outcomes, credentials, credentials, paths, TerminalCorroborationWorkerConfig{
		ClaimOwner: "redirect-test", Window: time.Hour, PollInterval: time.Second, LeaseDuration: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewTerminalCorroborationWorker() error = %v", err)
	}
	return worker.WithChangeProposalVerifier(api), credentials, paths
}

func redirectObservedPullRequest(state, headSHA string) githubapi.PullRequest {
	return githubapi.PullRequest{
		ID: 64, NodeID: "PR_node", Number: 12, Title: "Change", State: state,
		Head: githubapi.PullRequestBranch{Ref: "feature", SHA: headSHA},
		Base: githubapi.PullRequestBranch{Ref: "main", SHA: "base-sha"},
	}
}

func TestRedirectStopsAfterRateLimitedVerification(t *testing.T) {
	database := &redirectTestStore{pending: redirectTestPending()}
	api := &redirectTestGitHub{err: &githubapi.RateLimitError{APIError: &githubapi.APIError{StatusCode: 429}, RetryAfter: time.Minute}}
	worker, _, paths := redirectTestWorker(t, database, api)
	database.lease = redirectTestLease()

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%t, %v), want (true, nil)", processed, err)
	}
	if api.calls != 1 || database.retryCalls != 1 || paths.calls != 0 || database.redirectCalls != 0 || database.handoffCalls != 0 {
		t.Errorf("redirect rate limit = (GitHub %d, retries %d, workspace %d, redirects %d, handoffs %d), want (1, 1, 0, 0, 0)",
			api.calls, database.retryCalls, paths.calls, database.redirectCalls, database.handoffCalls)
	}
}

func TestRedirectStopsAfterTransientVerificationFailure(t *testing.T) {
	database := &redirectTestStore{pending: redirectTestPending()}
	api := &redirectTestGitHub{err: errors.New("transport failed")}
	worker, _, paths := redirectTestWorker(t, database, api)
	database.lease = redirectTestLease()

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%t, %v), want (true, nil)", processed, err)
	}
	if api.calls != 1 || database.retryCalls != 1 || paths.calls != 0 || database.redirectCalls != 0 || database.handoffCalls != 0 {
		t.Errorf("redirect transient failure = (GitHub %d, retries %d, workspace %d, redirects %d, handoffs %d), want (1, 1, 0, 0, 0)",
			api.calls, database.retryCalls, paths.calls, database.redirectCalls, database.handoffCalls)
	}
}

func TestRedirectStopsAfterPermanentVerificationFailure(t *testing.T) {
	database := &redirectTestStore{pending: redirectTestPending()}
	api := &redirectTestGitHub{pullRequest: redirectObservedPullRequest("closed", "head-b")}
	worker, _, paths := redirectTestWorker(t, database, api)
	database.lease = redirectTestLease()

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%t, %v), want (true, nil)", processed, err)
	}
	if api.calls != 1 || database.handoffCalls != 1 || paths.calls != 0 || database.redirectCalls != 0 || database.retryCalls != 0 {
		t.Errorf("redirect permanent failure = (GitHub %d, handoffs %d, workspace %d, redirects %d, retries %d), want (1, 1, 0, 0, 0)",
			api.calls, database.handoffCalls, paths.calls, database.redirectCalls, database.retryCalls)
	}
}

func TestRedirectStopsAfterHeadMovedRetry(t *testing.T) {
	database := &redirectTestStore{pending: redirectTestPending(), redirectErr: store.ErrTerminalCorroborationHeadMoved}
	api := &redirectTestGitHub{pullRequest: redirectObservedPullRequest("open", "head-b")}
	worker, _, paths := redirectTestWorker(t, database, api)
	database.lease = redirectTestLease()

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%t, %v), want (true, nil)", processed, err)
	}
	if api.calls != 1 || database.redirectCalls != 1 || database.retryCalls != 1 || paths.calls != 0 || database.handoffCalls != 0 {
		t.Errorf("redirect head moved = (GitHub %d, redirects %d, retries %d, workspace %d, handoffs %d), want (1, 1, 1, 0, 0)",
			api.calls, database.redirectCalls, database.retryCalls, paths.calls, database.handoffCalls)
	}
}

func TestMatchingHeadContinuesOrdinaryRevalidation(t *testing.T) {
	database := &redirectTestStore{pending: redirectTestPending()}
	api := &redirectTestGitHub{pullRequest: redirectObservedPullRequest("open", "head-a")}
	worker, _, paths := redirectTestWorker(t, database, api)
	database.lease = redirectTestLease()

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%t, %v), want (true, nil)", processed, err)
	}
	if api.calls != 1 || database.redirectCalls != 0 || paths.calls != 1 || database.retryCalls != 1 || database.handoffCalls != 0 {
		t.Errorf("matching head = (GitHub %d, redirects %d, workspace %d, retries %d, handoffs %d), want (1, 0, 1, 1, 0)",
			api.calls, database.redirectCalls, paths.calls, database.retryCalls, database.handoffCalls)
	}
}

func TestTerminalCorroborationBackoffIsBounded(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second}, {2, 10 * time.Second},
		{3, 20 * time.Second}, {6, 2 * time.Minute},
		{1000000, 2 * time.Minute},
	} {
		if got := terminalCorroborationBackoff(test.attempt); got != test.want {
			t.Errorf("attempt %d backoff = %s, want %s", test.attempt, got, test.want)
		}
	}
}
