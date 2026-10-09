//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	runtimesession "github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

// expiringReconcilerGitHub rejects the stale Turn-start snapshot with HTTP 401
// while serving the current installation token, proving final observations do
// not use the expired snapshot.
type expiringReconcilerGitHub struct {
	mutex       sync.Mutex
	pullRequest githubapi.PullRequest
	seen        []string
	getCalls    int
}

func (github *expiringReconcilerGitHub) GetPullRequest(_ context.Context, credential, _, _ string, _ int) (githubapi.PullRequest, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.seen = append(github.seen, credential)
	github.getCalls++
	if credential == "stale-developer-token" {
		return githubapi.PullRequest{}, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	return github.pullRequest, nil
}

func (github *expiringReconcilerGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	return nil, fmt.Errorf("unexpected Pull Request review read in Developer handoff")
}

func (github *expiringReconcilerGitHub) sawOnly(token string) bool {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	if len(github.seen) == 0 {
		return false
	}
	for _, seen := range github.seen {
		if seen != token {
			return false
		}
	}
	return true
}

// TestStaleSnapshotReconcilesWithCurrentCredentialExactlyOnce proves a Turn
// that outlives its Turn-start token still settles directly using a current
// credential: exactly one settlement and successor, with no deferred
// corroboration checkpoint or recovery job.
func TestStaleSnapshotReconcilesWithCurrentCredentialExactlyOnce(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 98, workflow.RoleDeveloper, "")
	const head = "3123456789abcdef0123456789abcdef01234567"
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "request-review-expiry", ToolName: mcp.ToolRequestReview,
		Request:         json.RawMessage(`{"operation_id":"request-review-expiry","summary":"Ready for review"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:feature", proposal.RepositoryID), ExpectedSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{
		"outcome": "REVIEW_REQUESTED", "pull_request_id": proposal.PullRequestID,
		"pull_request_number": proposal.PullRequestNumber, "head_sha": head,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	github := &expiringReconcilerGitHub{pullRequest: githubapi.PullRequest{
		ID: proposal.PullRequestID, NodeID: proposal.PullRequestNodeID, Number: int(proposal.PullRequestNumber), State: "open",
		Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: head, Label: "owner:feature"},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:main"},
	}}
	developer := &integrationCredentialProvider{credential: "installation-token"}
	reviewer := &integrationCredentialProvider{credential: "installation-token"}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: database, GitHub: github,
		DeveloperCredentials: developer, ReviewerCredentials: reviewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := agentturn.OutcomeReconciliation{
		Lease: lease, Execution: execution, PromptResponse: &acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
		// The expired Turn-start snapshot must never reach GitHub.
		RepositoryCredential: "stale-developer-token", Paths: committedPublicationPaths(t),
	}
	var failure *agentturn.TerminalCorroborationFailure
	request.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) { failure = &observed }
	observation, err := reconciler.Reconcile(ctx, request)
	if err != nil || observation.Outcome != workflow.TurnOutcomeChangeProposalReady || observation.Completion.Status != store.AgentTurnSucceeded ||
		observation.ChangeProposal == nil || observation.ChangeProposal.HeadSHA != head {
		t.Fatalf("stale-snapshot handoff observation = (%#v, %v)", observation, err)
	}
	if failure != nil {
		t.Fatalf("successful intent took a recovery detour: %#v", failure)
	}
	if !github.sawOnly("installation-token") {
		t.Fatalf("final observations did not use only the current credential: %q", github.seen)
	}
	settled, err := database.SettleAgentTurn(ctx, lease, observation)
	if err != nil || settled.State != workflow.StateReviewing || settled.SuccessorJobID == "" {
		t.Fatalf("stale-snapshot settlement = (%#v, %v)", settled, err)
	}
	var settlements, successors, checkpoints, verifierJobs int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = $1),
       (SELECT count(*) FROM jobs AS successor JOIN agent_turn_settlements AS settlement
            ON successor.agent_turn_settlement_id = settlement.id
            WHERE settlement.agent_turn_id = $1 AND successor.kind = 'PREPARE_AGENT_TURN'),
       (SELECT count(*) FROM agent_turn_corroborations WHERE agent_turn_id = $1 AND state = 'PENDING'),
       (SELECT count(*) FROM jobs WHERE agent_turn_id = $1 AND kind = 'VERIFY_TERMINAL_INTENT' AND status = 'AVAILABLE')`,
		lease.ID).Scan(&settlements, &successors, &checkpoints, &verifierJobs); err != nil {
		t.Fatal(err)
	}
	if settlements != 1 || successors != 1 || checkpoints != 0 || verifierJobs != 0 {
		t.Fatalf("stale-snapshot effects = settlements %d, successors %d, checkpoints %d, verifier jobs %d; want 1, 1, 0, 0",
			settlements, successors, checkpoints, verifierJobs)
	}
}

type integrationExpiryClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *integrationExpiryClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *integrationExpiryClock) Advance(d time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(d)
}

type integrationExpiryJWT struct{}

func (integrationExpiryJWT) AppJWT(context.Context) (string, error) { return "app-jwt", nil }

type integrationExpiringRequester struct {
	clock  *integrationExpiryClock
	mutex  sync.Mutex
	calls  int
	prefix string
	first  time.Duration
	rest   time.Duration
}

func (requester *integrationExpiringRequester) CreateInstallationToken(_ context.Context, _ string, _ int64) (githubapi.InstallationToken, error) {
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

type integrationExpiringCacheProvider struct {
	cache *githubapi.InstallationTokenCache
	id    int64
	mutex sync.Mutex
	calls int
}

func (provider *integrationExpiringCacheProvider) RepositoryCredential(ctx context.Context, _, _ string) (string, error) {
	provider.mutex.Lock()
	provider.calls++
	provider.mutex.Unlock()
	return provider.cache.Token(ctx, provider.id)
}

// workerExpiringGitHub serves both the MCP backend and the outcome reconciler
// from one shared review record, rejecting the stale Turn-start snapshot with
// HTTP 401 on every operation while serving current credentials.
type workerExpiringGitHub struct {
	*replayGatewayGitHub
	mutex            sync.Mutex
	proposal         *store.AgentTurnSettlementChangeProposal
	head             string
	stale            string
	reviewID         int64
	actorID          int64
	submitted        *githubapi.Review
	seen             []string
	submitCalls      int
	submitCredential string
}

func (github *workerExpiringGitHub) note(credential string) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.seen = append(github.seen, credential)
}

func (github *workerExpiringGitHub) expired(credential string) bool {
	return credential == github.stale
}

func (github *workerExpiringGitHub) GetPullRequest(_ context.Context, credential, _, _ string, _ int) (githubapi.PullRequest, error) {
	github.note(credential)
	if github.expired(credential) {
		return githubapi.PullRequest{}, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	return githubapi.PullRequest{
		ID: github.proposal.PullRequestID, NodeID: github.proposal.PullRequestNodeID,
		Number: int(github.proposal.PullRequestNumber), State: "open",
		Head: githubapi.PullRequestBranch{Ref: github.proposal.HeadRef, SHA: github.head, Label: "owner:" + github.proposal.HeadRef},
		Base: githubapi.PullRequestBranch{Ref: github.proposal.BaseRef, SHA: github.proposal.BaseSHA, Label: "owner:" + github.proposal.BaseRef},
	}, nil
}

func (github *workerExpiringGitHub) ListPullRequestFiles(_ context.Context, credential, _, _ string, _ int) ([]githubapi.PullRequestFile, error) {
	github.note(credential)
	if github.expired(credential) {
		return nil, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	return nil, nil
}

func (github *workerExpiringGitHub) SubmitReview(_ context.Context, credential, _, _ string, _ int, request githubapi.ReviewRequest) (githubapi.Review, error) {
	github.note(credential)
	if github.expired(credential) {
		return githubapi.Review{}, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.submitCalls++
	github.submitCredential = credential
	state := "CHANGES_REQUESTED"
	if request.Event == githubapi.ReviewApprove {
		state = "APPROVED"
	}
	review := githubapi.Review{ID: github.reviewID, NodeID: "PRR_expiry", State: state, CommitID: request.CommitID,
		User: githubapi.User{ID: github.actorID}, HTMLURL: "https://github.test/review/expiry"}
	github.submitted = &review
	return review, nil
}

func (github *workerExpiringGitHub) ListPullRequestReviews(_ context.Context, credential, _, _ string, _ int) ([]githubapi.Review, error) {
	github.note(credential)
	if github.expired(credential) {
		return nil, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	github.mutex.Lock()
	defer github.mutex.Unlock()
	if github.submitted == nil {
		return nil, nil
	}
	return []githubapi.Review{*github.submitted}, nil
}

func (github *workerExpiringGitHub) sawOnly(token string) bool {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	if len(github.seen) == 0 {
		return false
	}
	for _, seen := range github.seen {
		if seen != token {
			return false
		}
	}
	return true
}

type workerExpiringCredentials struct {
	reviewer *integrationExpiringCacheProvider
}

func (credentials *workerExpiringCredentials) DeveloperCredential(context.Context, mcp.RepositoryScope) (string, error) {
	return "installation-token", nil
}

func (credentials *workerExpiringCredentials) ReviewerCredential(ctx context.Context, scope mcp.RepositoryScope) (string, error) {
	return credentials.reviewer.RepositoryCredential(ctx, scope.Owner, scope.Name)
}

type workerExpiringPublisher struct{}

func (workerExpiringPublisher) Publish(context.Context, workspace.Publication) (workspace.PublicationResult, error) {
	return workspace.PublicationResult{}, fmt.Errorf("unexpected publish")
}

type expiryWorkerPrompter struct {
	prompt func(context.Context, runtimesession.PromptRequest) (acp.PromptResponse, error)
}

func (prompter *expiryWorkerPrompter) Prompt(ctx context.Context, request runtimesession.PromptRequest) (acp.PromptResponse, error) {
	return prompter.prompt(ctx, request)
}

// TestExecutionWorkerSettlesReviewerTurnWithRefreshedCredential runs the real
// execution worker against the real Store while the Turn outlives its
// Turn-start token: the prompt hook advances past expiry, performs
// submit_review through the real MCP backend on the shared expiring cache, and
// the worker's final reconciliation must settle directly with exactly one
// native submission and no recovery detour.
func TestExecutionWorkerSettlesReviewerTurnWithRefreshedCredential(t *testing.T) {
	for _, test := range []struct {
		name       string
		number     int
		head       string
		event      string
		body       string
		wantState  string
		wantStatus workflow.State
	}{
		{name: "approve", number: 983, head: "4123456789abcdef0123456789abcdef01234567",
			event: "APPROVE", body: "Looks good", wantState: "APPROVED", wantStatus: workflow.StatePRReady},
		{name: "request changes", number: 984, head: "5123456789abcdef0123456789abcdef01234567",
			event: "REQUEST_CHANGES", body: "Please adjust", wantState: "CHANGES_REQUESTED", wantStatus: workflow.StateDeveloping},
	} {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			fixture := seedAgentSessionForRole(t, pool, test.number, workflow.RoleReviewer)
			if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = $2, state_revision = 1,
    desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE'
WHERE id = $1`, fixture.workflowID, workflow.StateReviewing); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
UPDATE workflow_attempts SET infrastructure_failure_limit = 1,
	    current_stage = $2 WHERE id = $1`, fixture.attemptID, workflow.StageReview); err != nil {
				t.Fatal(err)
			}
			actorID := int64(test.number*100 + 2)
			if _, err := pool.Exec(ctx, `
UPDATE agent_assignments SET github_app_actor_id = $2 WHERE id = $1`, fixture.assignmentID, actorID); err != nil {
				t.Fatal(err)
			}
			proposalRowID := fmt.Sprintf("79%04d00-0000-4000-8000-000000000001", test.number)
			proposal := settlementProposal(fixture, test.number, test.head)
			if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, pull_request_node_id, status, active,
    base_ref, base_sha, head_ref, head_sha
)
VALUES ($1, $2, $3, 'owner', 'repo', $4, $5, $6, 'OPEN', TRUE,
        'main', 'base-sha', 'feature', $7)`, proposalRowID, fixture.workflowID,
				proposal.RepositoryID, proposal.PullRequestID, proposal.PullRequestNumber,
				proposal.PullRequestNodeID, proposal.HeadSHA); err != nil {
				t.Fatal(err)
			}
			spec := fixture.turnSpec()
			spec.Stage = workflow.StageReview
			spec.Purpose = workflow.TurnPurposeReview
			spec.ChangeProposalID = proposalRowID
			spec.ExpectedHeadSHA = test.head
			spec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10, "Review test instructions.", nil)
			turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, spec)
			if err != nil {
				t.Fatalf("PrepareAgentTurn() error = %v", err)
			}
			job := agentTurnExecutionJob(t, pool, ctx, turn)
			if err := prioritizeFixtureJob(pool, ctx, job.ID, store.AgentTurnQueue, store.RunAgentTurnJobKind); err != nil {
				t.Fatal(err)
			}
			// The seed leaves the session ACTIVE with a seeded ACP binding,
			// which would conflict with the launcher's bind. Reset it to
			// unbound CREATING so launch binds exactly once, following the
			// established phase-nine fixture pattern.
			if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'CREATING', acp_session_id = NULL WHERE id = $1`, fixture.sessionID); err != nil {
				t.Fatal(err)
			}

			clock := &integrationExpiryClock{now: time.Date(2026, time.October, 8, 7, 45, 0, 0, time.UTC)}
			requester := &integrationExpiringRequester{clock: clock, prefix: "reviewer", first: 5 * time.Minute, rest: time.Hour}
			reviewerCache := githubapi.NewInstallationTokenCache(integrationExpiryJWT{}, requester, clock)
			reviewer := &integrationExpiringCacheProvider{cache: reviewerCache, id: int64(test.number)}
			developer := &integrationCredentialProvider{credential: "installation-token"}
			// The worker holds this snapshot from Turn start.
			expiredSnapshot, err := reviewer.RepositoryCredential(ctx, "owner", "repo")
			if err != nil || expiredSnapshot != "reviewer-token-A" {
				t.Fatalf("prime reviewer token = %q, %v", expiredSnapshot, err)
			}

			reviewID := int64(test.number*100 + 1)
			github := &workerExpiringGitHub{
				replayGatewayGitHub: &replayGatewayGitHub{},
				proposal:            proposal, head: test.head, stale: expiredSnapshot,
				reviewID: reviewID, actorID: actorID,
			}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
				GitHub: github, Credentials: &workerExpiringCredentials{reviewer: reviewer},
				Publisher: workerExpiringPublisher{}, Workflow: mcp.LedgerWorkflowMutations{},
				GitRemoteBaseURL: "https://github.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
				Store: database, GitHub: github,
				DeveloperCredentials: developer, ReviewerCredentials: reviewer,
			})
			if err != nil {
				t.Fatal(err)
			}
			operationID := "expiry-submit-1"
			prompter := &expiryWorkerPrompter{prompt: func(ctx context.Context, request runtimesession.PromptRequest) (acp.PromptResponse, error) {
				execution, err := database.GetAgentTurnExecutionContext(ctx, request.Lease)
				if err != nil {
					t.Fatalf("prompt execution context = %v", err)
				}
				// The prompt outlives the Turn-start token before submitting.
				clock.Advance(6 * time.Minute)
				scope := mcp.TokenScope{
					Lease: request.Lease, WorkflowID: execution.WorkflowID, Role: workflow.RoleReviewer,
					Repository:  mcp.RepositoryScope{ID: execution.Repository.ID, Owner: execution.Repository.Owner, Name: execution.Repository.Name},
					Issue:       mcp.IssueScope{ID: execution.Issue.ID, Number: execution.Issue.Number},
					PullRequest: &mcp.PullRequestScope{ID: proposal.PullRequestID, Number: proposal.PullRequestNumber},
					Branch:      proposal.HeadRef, DefaultBranch: proposal.BaseRef, HeadSHA: proposal.HeadSHA,
					ExpiresAt: request.Lease.LeaseExpiresAt.Add(-time.Second),
				}
				gateway, registration := openReplayGateway(t, database, backend, scope)
				callReplayGatewayTool(t, gateway, registration, mcp.ToolSubmitReview, map[string]any{
					"operation_id": operationID, "event": test.event, "body": test.body, "comments": []any{},
				}, false)
				if err := gateway.CloseAndDrain(ctx, registration); err != nil {
					t.Fatalf("drain MCP authority = %v", err)
				}
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}}
			workflowConfig := builtinStoreConfig(t)
			worker, err := agentturn.NewExecutionWorker(agentturn.ExecutionWorkerDependencies{
				Store: database, DeveloperCredentials: developer, ReviewerCredentials: reviewer,
				DefaultBranch: phaseNineDefaultBranch{}, Launcher: &phaseNineExecutionLauncher{store: database, acpSessionID: "expiry-worker"},
				Sessions: prompter, Outcomes: outcomes,
				Workspace: phaseNineWorkspace{paths: committedPublicationPaths(t)},
				PublicationRecovery: phaseNinePublicationRecoveryFunc(func(context.Context, store.AgentTurnLease, store.AgentTurnExecutionContext, string, string, string) error {
					return nil
				}),
				Definition: workflowConfig.Reducer.Definition(), Policies: workflowConfig.Policies,
			}, agentturn.ExecutionWorkerConfig{
				ClaimOwner: "expiry-worker", LeaseDuration: 20 * time.Second, HeartbeatInterval: 2 * time.Second,
				IdlePollInterval: 10 * time.Millisecond, TurnTimeout: 20 * time.Second, CleanupTimeout: 5 * time.Second,
				ConcurrencyLimit: 1, ProviderCredentialJSON: json.RawMessage(`{"token":"provider"}`), GitRemoteBaseURL: "https://github.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			processed, err := worker.ProcessNext(ctx)
			if err != nil || !processed {
				t.Fatalf("ProcessNext() = (%t, %v), want direct settlement", processed, err)
			}
			if github.submitCalls != 1 || github.submitCredential != "reviewer-token-B" {
				t.Fatalf("native submissions = %d with %q, want exactly one with the refreshed credential",
					github.submitCalls, github.submitCredential)
			}
			if !github.sawOnly("reviewer-token-B") {
				t.Fatalf("Turn used a stale credential; seen %q", github.seen)
			}
			var workflowStatus string
			var reviewCount, settlements, successors, checkpoints, verifierJobs int
			if err := pool.QueryRow(ctx, `
SELECT workflow.status, (SELECT count(*) FROM change_proposal_reviews WHERE review_id = $2),
       (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = $3),
       (SELECT count(*) FROM jobs AS successor JOIN agent_turn_settlements AS settlement
            ON successor.agent_turn_settlement_id = settlement.id
            WHERE settlement.agent_turn_id = $3 AND successor.kind = 'PREPARE_AGENT_TURN'),
       (SELECT count(*) FROM agent_turn_corroborations WHERE agent_turn_id = $3 AND state = 'PENDING'),
       (SELECT count(*) FROM jobs WHERE agent_turn_id = $3 AND kind = 'VERIFY_TERMINAL_INTENT' AND status = 'AVAILABLE')
FROM workflows AS workflow WHERE workflow.id = $1`,
				fixture.workflowID, reviewID, turn.ID).Scan(
				&workflowStatus, &reviewCount, &settlements, &successors, &checkpoints, &verifierJobs); err != nil {
				t.Fatal(err)
			}
			if workflowStatus != string(test.wantStatus) || reviewCount != 1 || settlements != 1 || checkpoints != 0 || verifierJobs != 0 {
				t.Fatalf("expired Turn effects = Workflow %s, reviews %d, settlements %d, checkpoints %d, verifier jobs %d",
					workflowStatus, reviewCount, settlements, checkpoints, verifierJobs)
			}
			if test.event == "REQUEST_CHANGES" && successors != 1 {
				t.Fatalf("requested-changes successors = %d, want 1", successors)
			}
		})
	}
}
