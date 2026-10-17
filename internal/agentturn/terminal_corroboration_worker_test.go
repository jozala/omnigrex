package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

type redirectTestStore struct {
	lease                      *store.JobLease
	pending                    store.TerminalCorroborationContext
	retryCalls                 int
	handoffCalls               int
	redirectCalls              int
	redirectErr                error
	redirectLease              store.JobLease
	redirectRevision           int64
	redirectHead               string
	redirectID, redirectNumber int64
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

func (database *redirectTestStore) RedirectRevalidationToFreshTurn(_ context.Context, lease store.JobLease, revision int64, head string, id, number int64) error {
	database.redirectCalls++
	database.redirectLease, database.redirectRevision, database.redirectHead = lease, revision, head
	database.redirectID, database.redirectNumber = id, number
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
	key   execution.Key
}

func (captures *redirectTestWorkspace) TerminalCapture(_ context.Context, key execution.Key) (execution.CaptureRef, error) {
	captures.calls++
	captures.key = key
	return execution.CaptureRef{}, errors.New("original sealed capture unavailable")
}

func (*redirectTestWorkspace) CaptureTreesEqual(context.Context, execution.CaptureRef, workspace.PublicationObservation) (bool, error) {
	return false, errors.New("unexpected publication inspection")
}

func redirectTestPending() store.TerminalCorroborationContext {
	return store.TerminalCorroborationContext{
		Checkpoint: store.TerminalCorroboration{
			TurnID: "turn-1", ExecutionEpoch: 1, WorkflowID: "workflow-1",
			WorkflowAttemptID: "attempt-old", PromptOutcome: json.RawMessage(`{"admitted":true,"status":"completed"}`),
			PendingSince: time.Now(),
		},
		Execution: store.AgentTurnExecutionContext{
			WorkflowID: "workflow-1",
			Repository: store.AgentTurnRepository{ID: 91, Owner: "acme", Name: "widgets"},
			Participant: store.AgentParticipant{ID: "participant-1", WorkflowID: "workflow-1", Status: store.AgentParticipantActive, Role: workflow.RoleDeveloper,
				RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"},
			Session: store.AgentSession{ID: "session-1", AgentParticipantID: "participant-1"},
			Turn: store.AgentTurn{
				AgentTurnSpec: store.AgentTurnSpec{AgentSessionID: "session-1", WorkflowAttemptID: "attempt-old", ExpectedHeadSHA: "head-a", ControlRevision: 1},
				ID:            "turn-1", AgentParticipantID: "participant-1", ExecutionEpoch: 1,
			},
			ChangeProposal: &store.AgentTurnChangeProposal{
				PullRequestID: 64, PullRequestNumber: 12, HeadRef: "feature", BaseRef: "main",
			},
		},
		Mutations:        []store.MutationReservation{{State: store.MutationUnknown}},
		WorkflowRevision: 7,
	}
}

func redirectTestLease() *store.JobLease {
	return &store.JobLease{
		Job: store.Job{
			JobSpec: store.JobSpec{Queue: "agent-turn-recovery", Kind: store.RevalidateTerminalIntentJobKind,
				WorkflowID: "workflow-1", WorkflowAttemptID: "attempt-new", AgentParticipantID: "participant-1", AgentSessionID: "session-1", AgentTurnID: "turn-1", ExecutionEpoch: 1},
			ID: "job-1", LeaseToken: "verification-token", LeaseOwner: "redirect-test", AttemptCount: 1, Status: store.JobLeased,
		},
		Attempt: 1,
	}
}

func redirectTestWorker(t *testing.T, database *redirectTestStore, api *redirectTestGitHub) (*TerminalCorroborationWorker, *redirectTestCredentials, *redirectTestWorkspace) {
	t.Helper()
	credentials := &redirectTestCredentials{}
	paths := &redirectTestWorkspace{}
	outcomes, err := NewOutcomeReconciler(OutcomeReconcilerConfig{Store: redirectTestOutcomeStore{}, GitHub: api, Workspace: paths, DeveloperCredentials: credentials, ReviewerCredentials: credentials})
	if err != nil {
		t.Fatalf("NewOutcomeReconciler() error = %v", err)
	}
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
	if paths.key != runtimeKey(database.pending.Execution) || paths.key.AgentTurnID != "turn-1" || paths.key.ExecutionEpoch != 1 {
		t.Fatalf("matching-head revalidation inspected a replacement instead of the original epoch: %#v", paths.key)
	}
}

func TestChangedHeadRedirectPreservesFenceAndStopsBeforeCapture(t *testing.T) {
	for _, test := range []struct {
		name                   string
		err                    error
		wantRetry, wantHandoff int
	}{
		{name: "redirected"},
		{name: "lost fence", err: store.ErrJobLeaseLost},
		{name: "unsettled replacement", err: store.ErrAgentTurnRecoveryUnsettled, wantRetry: 1},
		{name: "unsettled mutation", err: store.ErrAgentTurnMutationsUnsettled, wantRetry: 1},
		{name: "source conflict", err: store.ErrTerminalCorroborationConflict, wantHandoff: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := &redirectTestStore{pending: redirectTestPending(), redirectErr: test.err, lease: redirectTestLease()}
			api := &redirectTestGitHub{pullRequest: redirectObservedPullRequest("open", "head-b")}
			worker, _, captures := redirectTestWorker(t, database, api)
			processed, err := worker.ProcessOne(t.Context())
			if err != nil || !processed || captures.calls != 0 || api.calls != 1 || database.redirectCalls != 1 || database.retryCalls != test.wantRetry || database.handoffCalls != test.wantHandoff {
				t.Fatalf("changed-head redirect = processed %t, error %v, captures %d, GitHub %d, redirects %d, retries %d, handoffs %d",
					processed, err, captures.calls, api.calls, database.redirectCalls, database.retryCalls, database.handoffCalls)
			}
			if !reflect.DeepEqual(database.redirectLease, *database.lease) || database.redirectRevision != database.pending.WorkflowRevision || database.redirectHead != "head-b" || database.redirectID != 64 || database.redirectNumber != 12 {
				t.Fatalf("redirect lost live-head scope or original job fence: %#v", database)
			}
		})
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

func TestTerminalCorroborationContextConflictHandsOffExactSource(t *testing.T) {
	for _, kind := range []string{store.VerifyTerminalIntentJobKind, store.RevalidateTerminalIntentJobKind} {
		t.Run(kind, func(t *testing.T) {
			database := newContextErrorCorroborationStore(kind, fmt.Errorf("read persisted checkpoint: %w", store.ErrTerminalCorroborationConflict))
			worker := &TerminalCorroborationWorker{store: database, config: TerminalCorroborationWorkerConfig{ClaimOwner: "verifier", Window: time.Hour, LeaseDuration: time.Minute}}
			processed, err := worker.ProcessOne(t.Context())
			if !processed || err != nil {
				t.Fatalf("ProcessOne = %t, %v", processed, err)
			}
			if database.handoffKind != kind || database.handoffReason != workflow.ReasonTerminalCorroborationPrerequisite || database.handoffCode != "terminal_evidence_conflict" {
				t.Fatalf("incompatible checkpoint did not reach prerequisite handoff: kind=%q, reason=%q, code=%q", database.handoffKind, database.handoffReason, database.handoffCode)
			}
			if !reflect.DeepEqual(database.resultLease, database.lease) || database.completedSource != database.checkpoint.SourceInvocationID {
				t.Fatalf("handoff lost exact source fence: lease=%#v, source=%q", database.resultLease, database.completedSource)
			}
			if database.lease.Status != store.JobLeased || database.completedSource == "" {
				t.Fatal("test did not exercise a valid leased source checkpoint")
			}
		})
	}
}

func TestTerminalCorroborationContextTransportErrorRemainsUnreadable(t *testing.T) {
	for _, kind := range []string{store.VerifyTerminalIntentJobKind, store.RevalidateTerminalIntentJobKind} {
		t.Run(kind, func(t *testing.T) {
			transportErr := errors.New("database transport unavailable")
			database := newContextErrorCorroborationStore(kind, fmt.Errorf("read persisted checkpoint: %w", transportErr))
			worker := &TerminalCorroborationWorker{store: database, config: TerminalCorroborationWorkerConfig{ClaimOwner: "verifier", Window: time.Hour, LeaseDuration: time.Minute}}
			processed, err := worker.ProcessOne(t.Context())
			if !processed || !errors.Is(err, transportErr) {
				t.Fatalf("ProcessOne = %t, %v, want unreadable context error", processed, err)
			}
			if database.handoffKind != "" || database.completedSource != "" {
				t.Fatalf("unreadable context fabricated a handoff: kind=%q, source=%q", database.handoffKind, database.completedSource)
			}
		})
	}
}

// The remaining Store methods deliberately have no implementation: unreadable
// or incompatible context cannot authorize retries or settlement of old evidence.
type contextErrorCorroborationStore struct {
	terminalCorroborationStore
	lease, resultLease       store.JobLease
	checkpoint               store.TerminalCorroboration
	contextErr               error
	handoffKind, handoffCode string
	handoffReason            workflow.Reason
	completedSource          string
}

func newContextErrorCorroborationStore(kind string, contextErr error) *contextErrorCorroborationStore {
	return &contextErrorCorroborationStore{
		lease: store.JobLease{Job: store.Job{
			JobSpec: store.JobSpec{Queue: "agent-turn-recovery", Kind: kind, WorkflowID: "workflow", WorkflowAttemptID: "attempt", AgentParticipantID: "participant", AgentSessionID: "session", AgentTurnID: "source-turn", ExecutionEpoch: 7},
			ID:      "verification-job", Status: store.JobLeased, LeaseOwner: "verifier", LeaseToken: "exact-token", AttemptCount: 2,
		}, Attempt: 2},
		checkpoint: store.TerminalCorroboration{TurnID: "source-turn", ExecutionEpoch: 7, SourceInvocationID: "original-terminal-mutation", PromptOutcome: json.RawMessage(`{"stop_reason":"end_turn"}`)},
		contextErr: contextErr,
	}
}

func (d *contextErrorCorroborationStore) ClaimJobKind(_ context.Context, _, kind, _ string, _ time.Duration) (*store.JobLease, error) {
	if kind != d.lease.Kind {
		return nil, nil
	}
	return &d.lease, nil
}
func (*contextErrorCorroborationStore) HeartbeatJob(context.Context, store.JobLease, time.Duration) error {
	return nil
}
func (d *contextErrorCorroborationStore) GetTerminalCorroborationContext(context.Context, store.JobLease) (store.TerminalCorroborationContext, error) {
	// The real Store returns no usable context when prompt validation fails.
	return store.TerminalCorroborationContext{}, d.contextErr
}
func (d *contextErrorCorroborationStore) CompleteTerminalCorroborationHandoff(_ context.Context, lease store.JobLease, reason workflow.Reason, code string) (store.AgentTurnSettlement, error) {
	d.completeHandoff(lease, reason, code, store.VerifyTerminalIntentJobKind)
	return store.AgentTurnSettlement{}, nil
}
func (d *contextErrorCorroborationStore) CompleteTerminalRevalidationHandoff(_ context.Context, lease store.JobLease, reason workflow.Reason, code string) error {
	d.completeHandoff(lease, reason, code, store.RevalidateTerminalIntentJobKind)
	return nil
}
func (d *contextErrorCorroborationStore) completeHandoff(lease store.JobLease, reason workflow.Reason, code, kind string) {
	d.resultLease, d.handoffReason, d.handoffCode, d.handoffKind = lease, reason, code, kind
	if reflect.DeepEqual(lease, d.lease) && lease.AgentTurnID == d.checkpoint.TurnID && lease.ExecutionEpoch == d.checkpoint.ExecutionEpoch {
		d.completedSource = d.checkpoint.SourceInvocationID
	}
}
