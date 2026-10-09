package agentturn_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

type expiryClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *expiryClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *expiryClock) Advance(d time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(d)
}

type expiryJWT struct{}

func (expiryJWT) AppJWT(context.Context) (string, error) { return "app-jwt", nil }

type expiryRequester struct {
	clock  *expiryClock
	mutex  sync.Mutex
	calls  int
	prefix string
	first  time.Duration
	rest   time.Duration
}

func (requester *expiryRequester) CreateInstallationToken(_ context.Context, _ string, installationID int64) (githubapi.InstallationToken, error) {
	requester.mutex.Lock()
	defer requester.mutex.Unlock()
	requester.calls++
	lifetime := requester.rest
	if requester.calls == 1 {
		lifetime = requester.first
	}
	return githubapi.InstallationToken{
		Token:     requester.prefix + "-token-" + string(rune('A'+requester.calls-1)),
		ExpiresAt: requester.clock.Now().Add(lifetime),
	}, nil
}

// Distinct tokens per mint so freshness is observable while still using the
// real cache expiry logic.
type rotatingRequester struct {
	clock  *expiryClock
	mutex  sync.Mutex
	calls  int
	prefix string
}

func (requester *rotatingRequester) CreateInstallationToken(_ context.Context, _ string, _ int64) (githubapi.InstallationToken, error) {
	requester.mutex.Lock()
	defer requester.mutex.Unlock()
	requester.calls++
	return githubapi.InstallationToken{
		Token:     requester.prefix + "-token-" + string(rune('A'+requester.calls-1)),
		ExpiresAt: requester.clock.Now().Add(time.Hour),
	}, nil
}

type cacheProvider struct {
	cache *githubapi.InstallationTokenCache
	id    int64
	mutex sync.Mutex
	calls int
}

func (provider *cacheProvider) RepositoryCredential(ctx context.Context, _, _ string) (string, error) {
	provider.mutex.Lock()
	provider.calls++
	provider.mutex.Unlock()
	return provider.cache.Token(ctx, provider.id)
}

func (provider *cacheProvider) Calls() int {
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	return provider.calls
}

type expiringGitHub struct {
	mutex       sync.Mutex
	pullRequest githubapi.PullRequest
	reviews     []githubapi.Review
	expired     map[string]bool
	seen        []string
	getCalls    int
	listCalls   int
	getErrs     []error
	listErrs    []error
}

func (github *expiringGitHub) GetPullRequest(_ context.Context, credential, _, _ string, _ int) (githubapi.PullRequest, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.seen = append(github.seen, credential)
	github.getCalls++
	if len(github.getErrs) > 0 {
		err := github.getErrs[0]
		github.getErrs = github.getErrs[1:]
		if err != nil {
			return githubapi.PullRequest{}, err
		}
	}
	if github.expired[credential] {
		return githubapi.PullRequest{}, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	return github.pullRequest, nil
}

func (github *expiringGitHub) ListPullRequestReviews(_ context.Context, credential, _, _ string, _ int) ([]githubapi.Review, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.seen = append(github.seen, credential)
	github.listCalls++
	if len(github.listErrs) > 0 {
		err := github.listErrs[0]
		github.listErrs = github.listErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if github.expired[credential] {
		return nil, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	return github.reviews, nil
}

func (github *expiringGitHub) saw(token string) bool {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	for _, seen := range github.seen {
		if seen == token {
			return true
		}
	}
	return false
}

func newExpiryCache(t *testing.T, clock *expiryClock, prefix string, first, rest time.Duration) (*githubapi.InstallationTokenCache, *expiryRequester) {
	t.Helper()
	requester := &expiryRequester{clock: clock, prefix: prefix, first: first, rest: rest}
	cache := githubapi.NewInstallationTokenCache(expiryJWT{}, requester, clock)
	return cache, requester
}

func TestOutcomeReconcilerUsesCurrentReviewerCredentialAfterExpiry(t *testing.T) {
	for _, test := range []struct {
		name  string
		event string
		state string
		want  workflow.TurnOutcome
	}{
		{name: "approve", event: "APPROVE", state: "APPROVED", want: workflow.TurnOutcomeApproved},
		{name: "request changes", event: "REQUEST_CHANGES", state: "CHANGES_REQUESTED", want: workflow.TurnOutcomeChangesRequested},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Date(2026, time.October, 8, 7, 45, 0, 0, time.UTC)
			clock := &expiryClock{now: start}
			reviewerCache, _ := newExpiryCache(t, clock, "reviewer", 5*time.Minute, time.Hour)
			reviewer := &cacheProvider{cache: reviewerCache, id: 91}
			developer := &cacheProvider{cache: func() *githubapi.InstallationTokenCache {
				cache, _ := newExpiryCache(t, clock, "developer", time.Hour, time.Hour)
				return cache
			}(), id: 92}

			// Turn starts while the cached token is valid and outside the refresh window.
			expiredSnapshot, err := reviewer.RepositoryCredential(context.Background(), "acme", "widgets")
			if err != nil || expiredSnapshot != "reviewer-token-A" {
				t.Fatalf("prime reviewer token = %q, %v", expiredSnapshot, err)
			}
			// Simulate MCP submit_review refreshing the cache during the prompt.
			clock.Advance(6 * time.Minute)
			refreshed, err := reviewer.RepositoryCredential(context.Background(), "acme", "widgets")
			if err != nil || refreshed != "reviewer-token-B" {
				t.Fatalf("refreshed reviewer token = %q, %v", refreshed, err)
			}
			// The cache now holds a valid replacement; the worker still holds the earlier string value.
			// Advance further so the original mint lifetime has elapsed.
			clock.Advance(60 * time.Minute)

			request := outcomeRequest(t, workflow.RoleReviewer, true)
			request.RepositoryCredential = "stale-developer-token"
			request.ReviewerRepositoryCredential = expiredSnapshot
			mutation := outcomeSubmitReviewMutation(1, test.event, test.state, outcomeHead, 701)
			review := outcomeReview(outcomeHead, 701)
			review.State = test.state
			github := &expiringGitHub{
				pullRequest: outcomePullRequest(outcomeHead),
				reviews:     []githubapi.Review{review},
				expired:     map[string]bool{"expired-reviewer-token": true},
			}
			// Make the stale snapshot definitely unauthorized if it were used.
			github.expired[expiredSnapshot] = true
			if refreshed == expiredSnapshot {
				t.Fatalf("test setup did not rotate reviewer token: %q", refreshed)
			}

			reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
				Store: &outcomeStore{mutations: []store.MutationReservation{mutation}}, GitHub: github,
				DeveloperCredentials: developer, ReviewerCredentials: reviewer,
				Clock: outcomeClock{},
			})
			if err != nil {
				t.Fatal(err)
			}
			var failure *agentturn.TerminalCorroborationFailure
			request.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) {
				failure = &observed
			}
			observation, err := reconciler.Reconcile(context.Background(), request)
			if err != nil || observation.Outcome != test.want || observation.Completion.Status != store.AgentTurnSucceeded {
				t.Fatalf("expiry reconciliation = (%#v, %v)", observation, err)
			}
			if failure != nil {
				t.Fatalf("successful intent took recovery detour: %#v", failure)
			}
			if github.saw(expiredSnapshot) {
				t.Fatalf("final observations used expired snapshot %q; seen %q", expiredSnapshot, github.seen)
			}
			if len(github.seen) != 2 || github.seen[0] != github.seen[1] {
				t.Fatalf("final observations did not reuse one current credential; seen %q (refreshed was %q)", github.seen, refreshed)
			}
			if github.seen[0] == expiredSnapshot || github.seen[0] == "stale-developer-token" {
				t.Fatalf("final observations used stale credential; seen %q", github.seen)
			}
			if github.getCalls != 1 || github.listCalls != 1 {
				t.Fatalf("GitHub reads = get %d list %d, want 1 and 1", github.getCalls, github.listCalls)
			}
			for _, diagnostic := range []string{observation.Diagnostic, observation.Completion.LastError} {
				if strings.Contains(diagnostic, expiredSnapshot) || strings.Contains(diagnostic, refreshed) {
					t.Fatalf("diagnostic discloses credential material: %q", diagnostic)
				}
			}
		})
	}
}

func TestOutcomeReconcilerUsesCurrentDeveloperCredentialAfterExpiry(t *testing.T) {
	start := time.Date(2026, time.October, 8, 7, 45, 0, 0, time.UTC)
	clock := &expiryClock{now: start}
	developerCache, _ := newExpiryCache(t, clock, "developer", 5*time.Minute, time.Hour)
	developer := &cacheProvider{cache: developerCache, id: 93}
	reviewer := &cacheProvider{cache: func() *githubapi.InstallationTokenCache {
		cache, _ := newExpiryCache(t, clock, "reviewer", time.Hour, time.Hour)
		return cache
	}(), id: 94}

	expiredSnapshot, err := developer.RepositoryCredential(context.Background(), "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	// No intervening MCP refresh; expiry happens before final observation.
	clock.Advance(6 * time.Minute)

	request := outcomeRequest(t, workflow.RoleDeveloper, false)
	request.RepositoryCredential = expiredSnapshot
	request.ReviewerRepositoryCredential = expiredSnapshot
	mutation := outcomeRequestReviewMutation(1)
	github := &expiringGitHub{pullRequest: outcomePullRequest(outcomeHead), expired: map[string]bool{expiredSnapshot: true}}
	marker, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: outcomeWorkflowID, AgentAssignmentID: outcomeAssignmentID, OperationID: mutation.ID})
	if err != nil {
		t.Fatal(err)
	}
	// outcomeRequestReviewMutation uses operation review-1, but our mutation helper uses mutation-1;
	// align the PR body marker with the actual mutation ID used here.
	_ = marker
	github.pullRequest.Title = "Change"
	// Rebuild marker for the actual mutation ID.
	actualMarker, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: outcomeWorkflowID, AgentAssignmentID: outcomeAssignmentID, OperationID: mutation.ID})
	if err != nil {
		t.Fatal(err)
	}
	github.pullRequest.Body = githubapi.JoinBodyParts("Ready", "Closes #61", actualMarker)
	// Fix the request_review mutation body/title expectation: use the standard fixture path instead.
	request2 := outcomeRequest(t, workflow.RoleDeveloper, false)
	request2.RepositoryCredential = expiredSnapshot
	request2.ReviewerRepositoryCredential = expiredSnapshot
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
	_ = mutation
	_ = request
	marker2, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: outcomeWorkflowID, AgentAssignmentID: outcomeAssignmentID, OperationID: ledger[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	github.pullRequest.Title = "Change"
	github.pullRequest.Body = githubapi.JoinBodyParts("Ready", "Closes #61", marker2)

	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: ledger}, GitHub: github,
		DeveloperCredentials: developer, ReviewerCredentials: reviewer,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failure *agentturn.TerminalCorroborationFailure
	request2.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) { failure = &observed }
	observation, err := reconciler.Reconcile(context.Background(), request2)
	if err != nil || observation.Outcome != workflow.TurnOutcomeChangeProposalReady {
		t.Fatalf("developer expiry reconciliation = (%#v, %v)", observation, err)
	}
	if failure != nil {
		t.Fatalf("developer handoff took recovery detour: %#v", failure)
	}
	if github.saw(expiredSnapshot) {
		t.Fatalf("developer observation used expired snapshot; seen %q", github.seen)
	}
}

func TestOutcomeReconcilerReacquiresCredentialOnTransientRetry(t *testing.T) {
	calls := 0
	provider := &stubRotatingProvider{tokens: []string{"retry-token-A", "retry-token-B"}, calls: &calls}
	github := &expiringGitHub{
		pullRequest: outcomePullRequest(outcomeHead),
		getErrs:     []error{&githubapi.APIError{StatusCode: 503}},
	}
	request := outcomeRequest(t, workflow.RoleDeveloper, false)
	request.RepositoryCredential = "stale"
	request.ReviewerRepositoryCredential = "stale"
	ledger := []store.MutationReservation{outcomeRequestReviewMutation(1)}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: ledger}, GitHub: github,
		DeveloperCredentials: provider, ReviewerCredentials: provider,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || observation.Outcome != workflow.TurnOutcomeChangeProposalReady {
		t.Fatalf("retry reacquire = (%#v, %v)", observation, err)
	}
	if calls != 2 || github.getCalls != 2 {
		t.Fatalf("retry did not reacquire per attempt: provider %d GitHub %d", calls, github.getCalls)
	}
	if github.seen[0] != "retry-token-A" || github.seen[1] != "retry-token-B" {
		t.Fatalf("retry credentials = %q", github.seen)
	}
}

type stubRotatingProvider struct {
	tokens []string
	calls  *int
}

func (provider *stubRotatingProvider) RepositoryCredential(context.Context, string, string) (string, error) {
	token := provider.tokens[*provider.calls]
	*provider.calls++
	return token, nil
}

func TestOutcomeReconcilerPreservesSafeFailureForAcquisitionAndAuthorization(t *testing.T) {
	// Transient acquisition failure retains durable corroboration eligibility.
	transientProvider := &outcomeCredentialProvider{err: &githubapi.TransientError{Cause: errors.New("transport down")}}
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	request.RepositoryCredential = "stale"
	request.ReviewerRepositoryCredential = "stale"
	mutation := outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeHead, 701)
	github := &expiringGitHub{pullRequest: outcomePullRequest(outcomeHead), reviews: []githubapi.Review{outcomeReview(outcomeHead, 701)}}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: []store.MutationReservation{mutation}}, GitHub: github,
		DeveloperCredentials: transientProvider, ReviewerCredentials: transientProvider,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failure *agentturn.TerminalCorroborationFailure
	request.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) { failure = &observed }
	observation, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || observation.Outcome != workflow.TurnOutcomeInfrastructureFailed {
		t.Fatalf("transient acquisition = (%#v, %v)", observation, err)
	}
	if failure == nil || !failure.Retryable || failure.SourceInvocationID != mutation.ID {
		t.Fatalf("transient acquisition must stay retryable: %#v", failure)
	}
	if github.getCalls != 0 {
		t.Fatalf("failed acquisition must not call GitHub: %d", github.getCalls)
	}

	// Genuine 401 with a current credential stays prerequisite without replay.
	fresh := &outcomeCredentialProvider{credential: "current-token"}
	genuine := &expiringGitHub{
		pullRequest: outcomePullRequest(outcomeHead),
		reviews:     []githubapi.Review{outcomeReview(outcomeHead, 701)},
		expired:     map[string]bool{"current-token": true},
	}
	request2 := outcomeRequest(t, workflow.RoleReviewer, true)
	request2.RepositoryCredential = "stale"
	request2.ReviewerRepositoryCredential = "stale"
	reconciler2, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: []store.MutationReservation{mutation}}, GitHub: genuine,
		DeveloperCredentials: fresh, ReviewerCredentials: fresh,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failure2 *agentturn.TerminalCorroborationFailure
	request2.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) { failure2 = &observed }
	observation2, err := reconciler2.Reconcile(context.Background(), request2)
	if err != nil || observation2.Outcome != workflow.TurnOutcomeInfrastructureFailed {
		t.Fatalf("genuine 401 = (%#v, %v)", observation2, err)
	}
	if failure2 == nil || !failure2.Prerequisite || failure2.Retryable {
		t.Fatalf("genuine 401 must be prerequisite handoff: %#v", failure2)
	}
}

func TestOutcomeReconcilerSkipsAcquisitionWithoutGitHubObservation(t *testing.T) {
	provider := &outcomeCredentialProvider{credential: "unused"}
	request := outcomeRequest(t, workflow.RoleDeveloper, false)
	request.RepositoryCredential = "stale"
	request.ReviewerRepositoryCredential = "stale"
	blocked := outcomeBlockedMutation(1, "cannot proceed", "needs access")
	github := &expiringGitHub{}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: []store.MutationReservation{blocked}}, GitHub: github,
		DeveloperCredentials: provider, ReviewerCredentials: provider,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || observation.Outcome != workflow.TurnOutcomeBlocked {
		t.Fatalf("blocked = (%#v, %v)", observation, err)
	}
	if provider.calls != 0 || github.getCalls != 0 || github.listCalls != 0 {
		t.Fatalf("blocked must not acquire credentials or read GitHub: provider %d get %d list %d", provider.calls, github.getCalls, github.listCalls)
	}
}
