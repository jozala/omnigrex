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
	"github.com/jozala/omnigrex/internal/workspace"
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
			// The replacement stays valid through final observations below,
			// demonstrating reuse of the MCP-refreshed token.
			clock.Advance(6 * time.Minute)
			refreshed, err := reviewer.RepositoryCredential(context.Background(), "acme", "widgets")
			if err != nil || refreshed != "reviewer-token-B" {
				t.Fatalf("refreshed reviewer token = %q, %v", refreshed, err)
			}

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
			if len(github.seen) != 2 || github.seen[0] != refreshed || github.seen[1] != refreshed {
				t.Fatalf("final observations did not reuse the MCP-refreshed credential; seen %q (refreshed was %q)", github.seen, refreshed)
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
	if err != nil || expiredSnapshot != "developer-token-A" {
		t.Fatalf("prime developer token = %q, %v", expiredSnapshot, err)
	}
	// No intervening MCP refresh; expiry happens before final observation.
	clock.Advance(6 * time.Minute)

	request := outcomeRequest(t, workflow.RoleDeveloper, false)
	request.RepositoryCredential = expiredSnapshot
	request.ReviewerRepositoryCredential = expiredSnapshot
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
	github := &expiringGitHub{pullRequest: outcomePullRequest(outcomeHead), expired: map[string]bool{expiredSnapshot: true}}
	marker, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: outcomeWorkflowID, AgentAssignmentID: outcomeAssignmentID, OperationID: ledger[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	github.pullRequest.Title = "Change"
	github.pullRequest.Body = githubapi.JoinBodyParts("Ready", "Closes #61", marker)

	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: ledger}, GitHub: github,
		DeveloperCredentials: developer, ReviewerCredentials: reviewer,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failure *agentturn.TerminalCorroborationFailure
	request.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) { failure = &observed }
	observation, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || observation.Outcome != workflow.TurnOutcomeChangeProposalReady {
		t.Fatalf("developer expiry reconciliation = (%#v, %v)", observation, err)
	}
	if failure != nil {
		t.Fatalf("developer handoff took recovery detour: %#v", failure)
	}
	if github.saw(expiredSnapshot) {
		t.Fatalf("developer observation used expired snapshot; seen %q", github.seen)
	}
	if len(github.seen) != 1 || github.seen[0] != "developer-token-B" {
		t.Fatalf("developer observation did not use the refreshed credential; seen %q", github.seen)
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
	var waits []time.Duration
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: ledger}, GitHub: github,
		DeveloperCredentials: provider, ReviewerCredentials: provider,
		Clock: outcomeClock{},
		RetryWait: func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
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
	// Retry timing is driven by the injected hook, with no real-time waiting.
	if len(waits) != 1 || waits[0] != 200*time.Millisecond {
		t.Fatalf("retry waits = %v, want [200ms]", waits)
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

type rateLimitedInstallationAPI struct{}

func (rateLimitedInstallationAPI) ResolveRepositoryInstallation(context.Context, string, string, string) (int64, error) {
	return 0, &githubapi.RateLimitError{
		APIError:   &githubapi.APIError{StatusCode: 429, Message: "rate limited"},
		RetryAfter: 2 * time.Minute,
	}
}

func (rateLimitedInstallationAPI) VerifyRepositoryInstallation(context.Context, string, int64, string, string) error {
	return nil
}

func (rateLimitedInstallationAPI) CreateInstallationToken(context.Context, string, int64) (githubapi.InstallationToken, error) {
	return githubapi.InstallationToken{}, errors.New("unexpected token creation")
}

func TestOutcomeReconcilerPreservesRetryAdviceForSanitizedAcquisitionRateLimit(t *testing.T) {
	provider, err := githubapi.NewRepositoryInstallationCredentialProvider(
		expiryJWT{}, rateLimitedInstallationAPI{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	request.RepositoryCredential = "stale"
	request.ReviewerRepositoryCredential = "stale"
	mutation := outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeHead, 701)
	github := &expiringGitHub{pullRequest: outcomePullRequest(outcomeHead), reviews: []githubapi.Review{outcomeReview(outcomeHead, 701)}}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: []store.MutationReservation{mutation}}, GitHub: github,
		DeveloperCredentials: provider, ReviewerCredentials: provider,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failure *agentturn.TerminalCorroborationFailure
	request.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) { failure = &observed }
	observation, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || observation.Outcome != workflow.TurnOutcomeInfrastructureFailed {
		t.Fatalf("rate-limited acquisition = (%#v, %v)", observation, err)
	}
	// The production provider sanitizes the typed rate-limit error, so the
	// retry delay must come from the safe metadata surface.
	if failure == nil || !failure.Retryable || failure.Code != "rate_limited" {
		t.Fatalf("rate-limited acquisition failure = %#v, want retryable rate_limited", failure)
	}
	if failure.RetryAfter != 2*time.Minute {
		t.Fatalf("rate-limited acquisition retry delay = %v, want 2m0s", failure.RetryAfter)
	}
	if github.getCalls != 0 || github.listCalls != 0 {
		t.Fatalf("rate-limited acquisition must not reach GitHub: get %d list %d", github.getCalls, github.listCalls)
	}
}

func TestOutcomeReconcilerRedactsFreshCredentialAcrossTruncationBoundary(t *testing.T) {
	provider := &outcomeCredentialProvider{credential: "reviewer-fresh-token-0123456789abcdef"}
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	request.RepositoryCredential = "stale-startup-token"
	request.ReviewerRepositoryCredential = "stale-startup-token"
	// Fabricate a prompt-failure diagnostic carrying the future fresh token
	// across the 4,096-rune truncation boundary.
	request.PromptResponse = nil
	request.PromptError = agentturn.PromptErrorFailure
	fresh := "reviewer-fresh-token-0123456789abcdef"
	// The token straddles the 4,096-rune truncation boundary (starting at
	// offset 4,079, well inside the 4,060-4,095 pinning range) so a
	// truncate-then-redact order would leave an unredacted prefix behind.
	request.PromptDiagnostic = strings.Repeat("x", 4060) + fresh + strings.Repeat("y", 100)
	mutation := outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeHead, 701)
	github := &expiringGitHub{pullRequest: outcomePullRequest(outcomeHead), reviews: []githubapi.Review{outcomeReview(outcomeHead, 701)}}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: &outcomeStore{mutations: []store.MutationReservation{mutation}}, GitHub: github,
		DeveloperCredentials: provider, ReviewerCredentials: provider,
		Clock: outcomeClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range []string{observation.Diagnostic, observation.Completion.LastError} {
		if strings.Contains(diagnostic, fresh) || strings.Contains(diagnostic, fresh[:8]) || strings.Contains(diagnostic, fresh[len(fresh)-8:]) {
			t.Fatalf("settlement diagnostic discloses a credential fragment: %q", diagnostic)
		}
		if strings.Contains(diagnostic, "stale-startup-token") {
			t.Fatalf("settlement diagnostic discloses the startup snapshot: %q", diagnostic)
		}
	}
}

// mcpBoundaryGitHubBase rejects every GitHub operation; the boundary test
// overrides only the operations submit_review and reconciliation exercise.
type mcpBoundaryGitHubBase struct{}

func (mcpBoundaryGitHubBase) GetIssue(context.Context, string, string, string, int) (githubapi.Issue, error) {
	return githubapi.Issue{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) ListIssueComments(context.Context, string, string, string, int) ([]githubapi.IssueComment, error) {
	return nil, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	return githubapi.PullRequest{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error) {
	return nil, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) ListPullRequestFiles(context.Context, string, string, string, int) ([]githubapi.PullRequestFile, error) {
	return nil, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	return nil, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) ListReviewThreads(context.Context, string, string, string, int) ([]githubapi.ReviewThread, error) {
	return nil, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) GetCheckRuns(context.Context, string, string, string, string) ([]githubapi.CheckRun, error) {
	return nil, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) GetCheckRunDiagnostics(context.Context, string, string, string, int64, string, int64, string) (githubapi.CheckDiagnostics, error) {
	return githubapi.CheckDiagnostics{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) ListWorkflowRunsForHead(context.Context, string, string, string, string, string, int64) (githubapi.CIRunList, error) {
	return githubapi.CIRunList{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) GetWorkflowRun(context.Context, string, string, string, int64) (githubapi.CIRun, error) {
	return githubapi.CIRun{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) GetWorkflowRunDetail(context.Context, string, string, string, int64, int, string, int64, string) (githubapi.CIRunDetail, error) {
	return githubapi.CIRunDetail{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) GetCIJob(context.Context, string, string, string, int64) (githubapi.CIJob, error) {
	return githubapi.CIJob{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) GetCIJobLogExcerpt(context.Context, string, string, string, githubapi.JobScope, string, int, int64) (githubapi.CILogExcerpt, error) {
	return githubapi.CILogExcerpt{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) SearchCIJobLogs(context.Context, string, string, string, githubapi.JobScope, string, int, string, int64) (githubapi.CILogSearchResult, error) {
	return githubapi.CILogSearchResult{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) OpenPullRequest(context.Context, string, string, string, githubapi.OpenPullRequestRequest) (githubapi.PullRequest, error) {
	return githubapi.PullRequest{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) CreateIssueComment(context.Context, string, string, string, int, githubapi.CommentRequest) (githubapi.IssueComment, error) {
	return githubapi.IssueComment{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) CreatePullRequestComment(context.Context, string, string, string, int, githubapi.CommentRequest) (githubapi.IssueComment, error) {
	return githubapi.IssueComment{}, errors.New("unexpected GitHub call")
}

func (mcpBoundaryGitHubBase) SubmitReview(context.Context, string, string, string, int, githubapi.ReviewRequest) (githubapi.Review, error) {
	return githubapi.Review{}, errors.New("unexpected GitHub call")
}

type mcpBoundaryGitHub struct {
	mcpBoundaryGitHubBase
	mutex            sync.Mutex
	pullRequest      githubapi.PullRequest
	submitCalls      int
	submitCredential string
	operationCalls   map[string]string
}

func (github *mcpBoundaryGitHub) record(operation, credential string) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	if github.operationCalls == nil {
		github.operationCalls = make(map[string]string)
	}
	github.operationCalls[operation] = credential
}

func (github *mcpBoundaryGitHub) GetPullRequest(_ context.Context, credential, _, _ string, _ int) (githubapi.PullRequest, error) {
	github.record("get-pull-request", credential)
	return github.pullRequest, nil
}

func (github *mcpBoundaryGitHub) ListPullRequestFiles(_ context.Context, credential, _, _ string, _ int) ([]githubapi.PullRequestFile, error) {
	github.record("list-files", credential)
	return nil, nil
}

func (github *mcpBoundaryGitHub) SubmitReview(_ context.Context, credential, _, _ string, _ int, request githubapi.ReviewRequest) (githubapi.Review, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.submitCalls++
	github.submitCredential = credential
	state := "CHANGES_REQUESTED"
	if request.Event == githubapi.ReviewApprove {
		state = "APPROVED"
	}
	return githubapi.Review{ID: 801, NodeID: "PRR_node", State: state, CommitID: request.CommitID,
		User: githubapi.User{ID: 701}, HTMLURL: "https://github.test/review/801"}, nil
}

type mcpBoundaryCredentials struct {
	reviewer *cacheProvider
}

func (credentials *mcpBoundaryCredentials) DeveloperCredential(context.Context, mcp.RepositoryScope) (string, error) {
	return "developer-static-token", nil
}

func (credentials *mcpBoundaryCredentials) ReviewerCredential(ctx context.Context, scope mcp.RepositoryScope) (string, error) {
	return credentials.reviewer.RepositoryCredential(ctx, scope.Owner, scope.Name)
}

type mcpBoundaryPublisher struct{}

func (mcpBoundaryPublisher) Publish(context.Context, workspace.Publication) (workspace.PublicationResult, error) {
	return workspace.PublicationResult{}, errors.New("unexpected publish")
}

// TestMCPSubmitReviewRefreshesCacheForDirectReconciliation drives submit_review
// through the real MCP backend using the same credential provider the
// reconciler uses, for both approval and requested changes. It asserts exactly
// one native review submission with the refreshed credential and direct
// settlement without a recovery detour.
func TestMCPSubmitReviewRefreshesCacheForDirectReconciliation(t *testing.T) {
	for _, test := range []struct {
		name  string
		event string
		body  string
		state string
		want  workflow.TurnOutcome
	}{
		{name: "approve", event: "APPROVE", body: "Looks good", state: "APPROVED", want: workflow.TurnOutcomeApproved},
		{name: "request changes", event: "REQUEST_CHANGES", body: "Please adjust", state: "CHANGES_REQUESTED", want: workflow.TurnOutcomeChangesRequested},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Date(2026, time.October, 8, 7, 45, 0, 0, time.UTC)
			clock := &expiryClock{now: start}
			reviewerCache, _ := newExpiryCache(t, clock, "reviewer", 5*time.Minute, time.Hour)
			reviewer := &cacheProvider{cache: reviewerCache, id: 91}
			developer := &outcomeCredentialProvider{credential: "developer-static-token"}

			// The worker holds this snapshot from Turn start.
			expiredSnapshot, err := reviewer.RepositoryCredential(context.Background(), "acme", "widgets")
			if err != nil || expiredSnapshot != "reviewer-token-A" {
				t.Fatalf("prime reviewer token = %q, %v", expiredSnapshot, err)
			}

			api := &mcpBoundaryGitHub{pullRequest: outcomePullRequest(outcomeHead)}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
				GitHub: api, Credentials: &mcpBoundaryCredentials{reviewer: reviewer},
				Publisher: mcpBoundaryPublisher{}, Workflow: mcp.LedgerWorkflowMutations{},
				GitRemoteBaseURL: "https://github.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			scope := mcp.ToolScope{
				WorkflowID: outcomeWorkflowID, AgentAssignmentID: outcomeAssignmentID,
				AgentSessionID: outcomeSessionID, AgentTurnID: outcomeTurnID, ExecutionEpoch: 7,
				Role: workflow.RoleReviewer, AgentProfileName: "review-specialist", RoleDisplayName: "Reviewer",
				Repository:  mcp.RepositoryScope{ID: 41, Owner: "acme", Name: "widgets"},
				Issue:       mcp.IssueScope{ID: 51, Number: 61},
				PullRequest: &mcp.PullRequestScope{ID: 901, Number: 23},
				Branch:      "feature/work", DefaultBranch: "main", HeadSHA: outcomeHead,
				TurnCreatedAt: start,
			}
			// The prompt outlives the Turn-start token; submit_review through
			// the MCP backend refreshes the shared cache. The replacement
			// stays valid through final observations below.
			clock.Advance(6 * time.Minute)
			arguments := `{"operation_id":"submit-1","event":"` + test.event + `","body":"` + test.body + `","comments":[]}`
			result, err := backend.Execute(context.Background(), mcp.Invocation{
				Name: mcp.ToolSubmitReview, Arguments: []byte(arguments),
				Scope: scope, Class: mcp.MutationTool, OperationID: "submit-1",
			})
			if err != nil {
				t.Fatalf("Execute(submit_review) error = %v", err)
			}
			if api.submitCalls != 1 || api.submitCredential != "reviewer-token-B" {
				t.Fatalf("native submissions = %d with %q, want exactly one with the refreshed credential",
					api.submitCalls, api.submitCredential)
			}
			if api.operationCalls["get-pull-request"] != "reviewer-token-B" || api.operationCalls["list-files"] != "reviewer-token-B" {
				t.Fatalf("backend reads did not use the refreshed credential: %#v", api.operationCalls)
			}

			mutation := outcomeSucceededMutation(1, mcp.ToolSubmitReview, "submit-1", arguments, string(result),
				"github", "41:901", outcomeHead)
			request := outcomeRequest(t, workflow.RoleReviewer, true)
			request.RepositoryCredential = "stale-developer-token"
			request.ReviewerRepositoryCredential = expiredSnapshot
			github := &expiringGitHub{
				pullRequest: outcomePullRequest(outcomeHead),
				reviews:     []githubapi.Review{outcomeReview(outcomeHead, 701)},
				expired:     map[string]bool{expiredSnapshot: true},
			}
			github.reviews[0].State = test.state
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
				t.Fatalf("MCP-refreshed reconciliation = (%#v, %v)", observation, err)
			}
			if failure != nil {
				t.Fatalf("MCP-refreshed intent took a recovery detour: %#v", failure)
			}
			if len(github.seen) != 2 || github.seen[0] != "reviewer-token-B" || github.seen[1] != "reviewer-token-B" {
				t.Fatalf("final observations did not reuse the MCP-refreshed credential; seen %q", github.seen)
			}
		})
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
