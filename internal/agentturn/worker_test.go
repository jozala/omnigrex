package agentturn_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

type workerStore struct {
	mutex          sync.Mutex
	lease          *store.JobLease
	claimErr       error
	claimQueue     string
	claimKind      string
	claimOwner     string
	claimDuration  time.Duration
	repository     store.WorkflowRepository
	repositoryErr  error
	heartbeat      func(context.Context) error
	heartbeatErr   error
	heartbeats     int
	heartbeatReady chan struct{}
	failures       []workerFailure
	failErr        error
	acknowledged   []store.AgentTurnPreparationSpec
	ackErr         error
	claims         atomic.Int32
}

type workerFailure struct {
	cause      error
	retryable  bool
	retryDelay time.Duration
}

func (database *workerStore) ClaimJobKind(_ context.Context, queue, kind, owner string, lease time.Duration) (*store.JobLease, error) {
	database.claims.Add(1)
	database.mutex.Lock()
	defer database.mutex.Unlock()
	database.claimQueue, database.claimKind, database.claimOwner, database.claimDuration = queue, kind, owner, lease
	claim := database.lease
	database.lease = nil
	return claim, database.claimErr
}

func (database *workerStore) GetWorkflowRepository(context.Context, string) (store.WorkflowRepository, error) {
	return database.repository, database.repositoryErr
}

func (database *workerStore) HeartbeatJob(ctx context.Context, _ store.JobLease, _ time.Duration) error {
	database.mutex.Lock()
	database.heartbeats++
	if database.heartbeatReady != nil && database.heartbeats == 1 {
		close(database.heartbeatReady)
	}
	err := database.heartbeatErr
	heartbeat := database.heartbeat
	database.mutex.Unlock()
	if heartbeat != nil {
		return heartbeat(ctx)
	}
	return err
}

func (database *workerStore) AcknowledgeAgentTurnPreparationFailure(_ context.Context, lease store.JobLease, cause error, retryable bool, retryDelay time.Duration) (store.AgentTurnPreparationFailureAcknowledgement, error) {
	database.mutex.Lock()
	database.failures = append(database.failures, workerFailure{cause: cause, retryable: retryable, retryDelay: retryDelay})
	database.mutex.Unlock()
	return store.AgentTurnPreparationFailureAcknowledgement{JobID: lease.ID}, database.failErr
}

func (database *workerStore) AcknowledgeAssignmentConfigurationConflict(_ context.Context, _ store.JobLease, spec store.AgentTurnPreparationSpec) (store.AssignmentConfigurationHandoff, error) {
	database.mutex.Lock()
	database.acknowledged = append(database.acknowledged, spec)
	database.mutex.Unlock()
	return store.AssignmentConfigurationHandoff{}, database.ackErr
}

type repositoryCredentialProvider struct {
	credential string
	err        error
	owner      string
	repository string
	calls      int
}

func (provider *repositoryCredentialProvider) RepositoryCredential(_ context.Context, owner, repository string) (string, error) {
	provider.calls++
	provider.owner, provider.repository = owner, repository
	return provider.credential, provider.err
}

type turnPreparerFunc func(context.Context, agentturn.Request) (agentturn.Result, error)

func (prepare turnPreparerFunc) Prepare(ctx context.Context, request agentturn.Request) (agentturn.Result, error) {
	return prepare(ctx, request)
}

type permanentWorkerError struct{ message string }

func (err permanentWorkerError) Error() string   { return err.message }
func (err permanentWorkerError) Permanent() bool { return true }

type transientWorkerError struct{ message string }

func (err transientWorkerError) Error() string   { return err.message }
func (err transientWorkerError) Transient() bool { return true }

func TestWorkerProcessNextClaimsOnlyPreparationJobsAndPreparesRepository(t *testing.T) {
	lease := preparationWorkerLease()
	database := &workerStore{lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"}}
	developerCredentials := &repositoryCredentialProvider{credential: "developer-installation-token"}
	reviewerCredentials := &repositoryCredentialProvider{credential: "reviewer-installation-token"}
	var gotRequest agentturn.Request
	worker := newPreparationWorker(t, database, developerCredentials, reviewerCredentials, turnPreparerFunc(func(_ context.Context, request agentturn.Request) (agentturn.Result, error) {
		gotRequest = request
		return agentturn.Result{}, nil
	}), 10*time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want prepared", processed, err)
	}
	if database.claimQueue != store.WorkflowActionQueue || database.claimKind != store.PrepareAgentTurnJobKind || database.claimOwner != "preparation-worker" || database.claimDuration != 30*time.Second {
		t.Errorf("claim = (%q, %q, %q, %s)", database.claimQueue, database.claimKind, database.claimOwner, database.claimDuration)
	}
	if developerCredentials.owner != "acme" || developerCredentials.repository != "widgets" || developerCredentials.calls != 1 ||
		reviewerCredentials.owner != "acme" || reviewerCredentials.repository != "widgets" || reviewerCredentials.calls != 1 {
		t.Errorf("credential providers = Developer %#v, Reviewer %#v", developerCredentials, reviewerCredentials)
	}
	if gotRequest.Lease.ID != lease.ID || gotRequest.InstallationCredential != "developer-installation-token" || gotRequest.RepositoryOwner != "acme" || gotRequest.RepositoryName != "widgets" {
		t.Errorf("Prepare() request = %#v", gotRequest)
	}
	if strings.Contains(gotRequest.InstallationCredential, "reviewer") {
		t.Errorf("Prepare() received Reviewer credential %q", gotRequest.InstallationCredential)
	}
	if len(database.failures) != 0 || len(database.acknowledged) != 0 {
		t.Errorf("unexpected failures/acknowledgements = (%#v, %#v)", database.failures, database.acknowledged)
	}
}

func TestWorkerProcessNextReturnsIdleWithoutPreparation(t *testing.T) {
	database := &workerStore{}
	credentials := &repositoryCredentialProvider{}
	worker := newPreparationWorker(t, database, credentials, &repositoryCredentialProvider{credential: "reviewer-token"}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		t.Fatal("Prepare() called without a job")
		return agentturn.Result{}, nil
	}), time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || processed {
		t.Fatalf("ProcessNext() = (%t, %v), want idle", processed, err)
	}
}

func TestWorkerHeartbeatsWhilePreparationLoadsProfiles(t *testing.T) {
	lease := preparationWorkerLease()
	heartbeat := make(chan struct{})
	database := &workerStore{
		lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"}, heartbeatReady: heartbeat,
	}
	worker := newPreparationWorker(t, database, &repositoryCredentialProvider{credential: "token"}, &repositoryCredentialProvider{credential: "reviewer-token"}, turnPreparerFunc(func(ctx context.Context, _ agentturn.Request) (agentturn.Result, error) {
		select {
		case <-heartbeat:
			return agentturn.Result{}, nil
		case <-ctx.Done():
			return agentturn.Result{}, ctx.Err()
		}
	}), time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	database.mutex.Lock()
	heartbeats := database.heartbeats
	database.mutex.Unlock()
	if heartbeats < 1 {
		t.Fatal("HeartbeatJob() was not called while Prepare() was active")
	}
}

func TestWorkerCancelsPreparationWhenHeartbeatLosesLease(t *testing.T) {
	lease := preparationWorkerLease()
	database := &workerStore{
		lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"}, heartbeatErr: store.ErrJobLeaseLost,
	}
	prepareCanceled := make(chan struct{})
	worker := newPreparationWorker(t, database, &repositoryCredentialProvider{credential: "token"}, &repositoryCredentialProvider{credential: "reviewer-token"}, turnPreparerFunc(func(ctx context.Context, _ agentturn.Request) (agentturn.Result, error) {
		<-ctx.Done()
		close(prepareCanceled)
		return agentturn.Result{}, ctx.Err()
	}), time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || !strings.Contains(err.Error(), store.ErrJobLeaseLost.Error()) {
		t.Fatalf("ProcessNext() = (%t, %v), want lease loss", processed, err)
	}
	select {
	case <-prepareCanceled:
	default:
		t.Fatal("Prepare() context was not canceled")
	}
	if len(database.failures) != 0 {
		t.Fatalf("FailJob() called after lease loss: %#v", database.failures)
	}
}

func TestWorkerCancellationOfInFlightHeartbeatDoesNotSuppressFailureAcknowledgement(t *testing.T) {
	lease := preparationWorkerLease()
	heartbeatStarted := make(chan struct{})
	database := &workerStore{
		lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"},
		heartbeat: func(ctx context.Context) error {
			close(heartbeatStarted)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	failure := errors.New("Agent Profile API unavailable")
	worker := newPreparationWorker(t, database, &repositoryCredentialProvider{credential: "token"}, &repositoryCredentialProvider{credential: "reviewer-token"}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		<-heartbeatStarted
		return agentturn.Result{}, failure
	}), time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || !strings.Contains(err.Error(), failure.Error()) {
		t.Fatalf("ProcessNext() = (%t, %v), want preparation failure", processed, err)
	}
	if errors.Is(err, failure) {
		t.Fatalf("ProcessNext() retained preparation failure: %v", err)
	}
	if len(database.failures) != 1 || !database.failures[0].retryable {
		t.Fatalf("preparation failure acknowledgements = %#v, want one retryable failure", database.failures)
	}
}

func TestWorkerAcknowledgesAssignmentConfigurationConflict(t *testing.T) {
	lease := preparationWorkerLease()
	database := &workerStore{lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"}}
	spec := store.AgentTurnPreparationSpec{Developer: store.RolePreparation{Binding: store.AssignmentRuntimeBinding{AgentProfileName: "developer"}}}
	worker := newPreparationWorker(t, database, &repositoryCredentialProvider{credential: "token"}, &repositoryCredentialProvider{credential: "reviewer-token"}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		return agentturn.Result{}, &agentturn.AssignmentConfigurationConflictError{Preparation: spec, Cause: store.ErrAssignmentConfigurationConflict}
	}), 10*time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want acknowledged conflict", processed, err)
	}
	if len(database.acknowledged) != 1 || database.acknowledged[0].Developer.Binding.AgentProfileName != "developer" {
		t.Fatalf("conflict acknowledgements = %#v, want exact preparation", database.acknowledged)
	}
	if len(database.failures) != 0 {
		t.Fatalf("FailJob() called for acknowledged conflict: %#v", database.failures)
	}
}

func TestWorkerCreatesHumanHandoffWhenReviewerAppIsNotInstalled(t *testing.T) {
	lease := preparationWorkerLease()
	database := &workerStore{lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"}}
	developerCredentials := &repositoryCredentialProvider{credential: "developer-token-secret"}
	reviewerCredentials := &repositoryCredentialProvider{err: &githubapi.NotInstalledError{Owner: "acme", Repository: "widgets"}}
	worker := newPreparationWorker(t, database, developerCredentials, reviewerCredentials, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		t.Fatal("Prepare() called without a Reviewer installation")
		return agentturn.Result{}, nil
	}), 10*time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if !processed {
		t.Fatal("ProcessNext() did not process preparation")
	}
	if err == nil || !githubapi.ExtractSafeErrorMetadata(err).Permanent {
		t.Fatalf("ProcessNext() error = %v, want permanent Reviewer installation failure", err)
	}
	var notInstalled *githubapi.NotInstalledError
	if errors.As(err, &notInstalled) {
		t.Fatalf("ProcessNext() exposed Reviewer NotInstalledError: %#v", notInstalled)
	}
	if len(database.failures) != 1 || database.failures[0].retryable {
		t.Fatalf("preparation failure acknowledgement = %#v, want terminal", database.failures)
	}
	if strings.Contains(err.Error(), "developer-token-secret") || strings.Contains(database.failures[0].cause.Error(), "developer-token-secret") {
		t.Fatalf("Reviewer installation failure exposed Developer token: returned %v, durable %v", err, database.failures[0].cause)
	}
}

func TestWorkerClassifiesPreparationFailuresAndRedactsCredential(t *testing.T) {
	tests := []struct {
		name          string
		failure       error
		wantRetryable bool
		wantDelay     time.Duration
	}{
		{name: "permanent", failure: permanentWorkerError{message: "bad profile token-secret"}, wantDelay: 2 * time.Second},
		{name: "GitHub profile missing", failure: &githubapi.APIError{StatusCode: 404, Message: "profile missing token-secret"}, wantDelay: 2 * time.Second},
		{name: "request timeout", failure: &githubapi.APIError{StatusCode: 408, Message: "request timed out token-secret"}, wantRetryable: true, wantDelay: 2 * time.Second},
		{name: "too many requests", failure: &githubapi.APIError{StatusCode: 429, Message: "rate limited token-secret"}, wantRetryable: true, wantDelay: 2 * time.Second},
		{name: "transient", failure: transientWorkerError{message: "API unavailable token-secret"}, wantRetryable: true, wantDelay: 2 * time.Second},
		{name: "rate limited", failure: &githubapi.RateLimitError{APIError: &githubapi.APIError{Message: "rate limited token-secret"}, RetryAfter: 7 * time.Second}, wantRetryable: true, wantDelay: 7 * time.Second},
		{name: "unknown infrastructure", failure: errors.New("database unavailable token-secret"), wantRetryable: true, wantDelay: 2 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := preparationWorkerLease()
			database := &workerStore{lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"}}
			worker := newPreparationWorker(t, database, &repositoryCredentialProvider{credential: "token-secret"}, &repositoryCredentialProvider{credential: "reviewer-token-secret"}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
				return agentturn.Result{}, test.failure
			}), 10*time.Second)

			processed, err := worker.ProcessNext(context.Background())
			if !processed || err == nil {
				t.Fatalf("ProcessNext() = (%t, %v), want sanitized failure", processed, err)
			}
			if errors.Is(err, test.failure) {
				t.Fatalf("ProcessNext() retained source failure: %v", err)
			}
			if strings.Contains(err.Error(), "token-secret") {
				t.Fatalf("ProcessNext() leaked credential: %v", err)
			}
			if len(database.failures) != 1 || database.failures[0].retryable != test.wantRetryable || database.failures[0].retryDelay != test.wantDelay {
				t.Fatalf("FailJob() = %#v, want retryable=%t delay=%s", database.failures, test.wantRetryable, test.wantDelay)
			}
			if strings.Contains(database.failures[0].cause.Error(), "token-secret") {
				t.Fatalf("durable failure leaked credential: %v", database.failures[0].cause)
			}
		})
	}
}

func TestWorkerUsesGitHubRateLimitResetWhenRetryAfterIsAbsent(t *testing.T) {
	lease := preparationWorkerLease()
	database := &workerStore{lease: &lease, repository: store.WorkflowRepository{Owner: "acme", Name: "widgets"}}
	resetAt := time.Now().Add(5 * time.Second)
	rateLimit := &githubapi.RateLimitError{APIError: &githubapi.APIError{StatusCode: 429, Message: "rate limited"}, ResetAt: resetAt}
	worker := newPreparationWorker(t, database, &repositoryCredentialProvider{credential: "developer-token"}, &repositoryCredentialProvider{credential: "reviewer-token"}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		return agentturn.Result{}, rateLimit
	}), 10*time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want rate limit", processed, err)
	}
	if errors.Is(err, rateLimit) {
		t.Fatalf("ProcessNext() retained rate-limit source: %v", err)
	}
	if len(database.failures) != 1 || !database.failures[0].retryable ||
		database.failures[0].retryDelay < 4*time.Second || database.failures[0].retryDelay > 5*time.Second {
		t.Fatalf("rate-limit acknowledgement = %#v, want delay until %s", database.failures, resetAt)
	}
}

func TestWorkerRunPollsUntilContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	database := &workerStore{}
	worker, err := agentturn.NewWorker(database, &repositoryCredentialProvider{}, &repositoryCredentialProvider{}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		return agentturn.Result{}, nil
	}), agentturn.WorkerConfig{
		ClaimOwner: "preparation-worker", LeaseDuration: 30 * time.Second, HeartbeatInterval: time.Second,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for database.claims.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	err = worker.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() error = %v, want context.Canceled", err)
	}
	if database.claims.Load() < 3 {
		t.Errorf("claim calls = %d, want at least 3", database.claims.Load())
	}
}

func TestNewWorkerEnforcesStoreDurationBounds(t *testing.T) {
	const maximum = 365 * 24 * time.Hour
	valid := agentturn.WorkerConfig{
		ClaimOwner: "preparation-worker", LeaseDuration: 2 * time.Microsecond,
		HeartbeatInterval: time.Microsecond, IdlePollInterval: time.Microsecond, RetryDelay: time.Microsecond,
	}
	for _, test := range []struct {
		name   string
		mutate func(*agentturn.WorkerConfig)
	}{
		{name: "lease below precision", mutate: func(config *agentturn.WorkerConfig) { config.LeaseDuration = time.Nanosecond }},
		{name: "lease above maximum", mutate: func(config *agentturn.WorkerConfig) { config.LeaseDuration = maximum + time.Microsecond }},
		{name: "heartbeat below precision", mutate: func(config *agentturn.WorkerConfig) { config.HeartbeatInterval = time.Nanosecond }},
		{name: "heartbeat above maximum", mutate: func(config *agentturn.WorkerConfig) {
			config.LeaseDuration = maximum
			config.HeartbeatInterval = maximum + time.Microsecond
		}},
		{name: "poll below precision", mutate: func(config *agentturn.WorkerConfig) { config.IdlePollInterval = time.Nanosecond }},
		{name: "poll above maximum", mutate: func(config *agentturn.WorkerConfig) { config.IdlePollInterval = maximum + time.Microsecond }},
		{name: "retry below precision", mutate: func(config *agentturn.WorkerConfig) { config.RetryDelay = time.Nanosecond }},
		{name: "retry above maximum", mutate: func(config *agentturn.WorkerConfig) { config.RetryDelay = maximum + time.Microsecond }},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := valid
			test.mutate(&settings)
			if _, err := agentturn.NewWorker(&workerStore{}, &repositoryCredentialProvider{}, &repositoryCredentialProvider{}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
				return agentturn.Result{}, nil
			}), settings); err == nil {
				t.Fatal("NewWorker() error = nil, want duration validation error")
			}
		})
	}
	valid.LeaseDuration = maximum
	valid.HeartbeatInterval = maximum - time.Microsecond
	valid.IdlePollInterval = maximum
	valid.RetryDelay = maximum
	if _, err := agentturn.NewWorker(&workerStore{}, &repositoryCredentialProvider{}, &repositoryCredentialProvider{}, turnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		return agentturn.Result{}, nil
	}), valid); err != nil {
		t.Fatalf("NewWorker() maximum boundaries error = %v", err)
	}
}

func newPreparationWorker(t *testing.T, database agentturn.WorkerStore, developerCredentials, reviewerCredentials agentturn.RepositoryCredentialProvider, preparer agentturn.TurnPreparer, heartbeat time.Duration) *agentturn.Worker {
	t.Helper()
	worker, err := agentturn.NewWorker(database, developerCredentials, reviewerCredentials, preparer, agentturn.WorkerConfig{
		ClaimOwner: "preparation-worker", LeaseDuration: 30 * time.Second, HeartbeatInterval: heartbeat,
		IdlePollInterval: time.Millisecond, RetryDelay: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return worker
}

func preparationWorkerLease() store.JobLease {
	return store.JobLease{
		Job: store.Job{
			ID: "64000000-0000-4000-8000-000000000001",
			JobSpec: store.JobSpec{
				Queue: store.WorkflowActionQueue, Kind: store.PrepareAgentTurnJobKind,
				WorkflowID: "10000000-0000-4000-8000-000000000001",
			},
			Status: store.JobLeased,
		},
		Attempt: 1,
	}
}
