package retention_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/retention"
	"github.com/jozala/omnigrex/internal/store"
)

func TestWorkerAuthorizesBeforeDeduplicatedCanonicalCleanupAndExactFinalization(t *testing.T) {
	lease := collectionLease(1)
	targets := cleanupTargets()
	durable := &workerStore{leases: []*store.JobLease{&lease}, authorization: store.AssignmentCollectionAuthorization{Targets: targets}}
	cleaner := &recordingCleaner{store: durable}
	worker := newWorker(t, durable, cleaner, time.Second, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want collected", processed, err)
	}
	if durable.claimQueue != store.WorkflowActionQueue || durable.claimKind != store.CollectAssignmentsJobKind ||
		durable.claimOwner != "retention-worker" || durable.claimLease != time.Second {
		t.Fatalf("ClaimJobKind() = (%q, %q, %q, %s)", durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimLease)
	}
	wantPaths := []cleanupCall{
		{assignmentID: assignmentOne, path: assignmentPath(assignmentOne)},
		{assignmentID: assignmentTwo, path: assignmentPath(assignmentTwo)},
	}
	if calls := cleaner.callsSnapshot(); !slices.Equal(calls, wantPaths) {
		t.Fatalf("EnsureAbsent() calls = %#v, want %#v", calls, wantPaths)
	}
	if cleaner.unauthorizedCall {
		t.Fatal("EnsureAbsent() was called before durable authorization")
	}
	if durable.finalizations != 1 || !slices.Equal(durable.confirmed, targets) {
		t.Fatalf("FinalizeAssignmentCollection() = %d calls with %#v, want exact %#v", durable.finalizations, durable.confirmed, targets)
	}
	if durable.failures != 0 {
		t.Fatalf("FailJob() calls = %d, want 0", durable.failures)
	}
}

func TestWorkerValidatesCompleteTargetSetBeforeAnyCleanup(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]store.AssignmentCleanupTarget) []store.AssignmentCleanupTarget
	}{
		{name: "alternate session path", mutate: func(targets []store.AssignmentCleanupTarget) []store.AssignmentCleanupTarget {
			targets[1].RuntimeStatePath = assignmentPath(assignmentTwo)
			return targets
		}},
		{name: "session image mismatch", mutate: func(targets []store.AssignmentCleanupTarget) []store.AssignmentCleanupTarget {
			targets[1].RuntimeImageDigest = "sha256:other"
			return targets
		}},
		{name: "missing Assignment target", mutate: func(targets []store.AssignmentCleanupTarget) []store.AssignmentCleanupTarget {
			return targets[1:2]
		}},
		{name: "uppercase Assignment UUID", mutate: func(targets []store.AssignmentCleanupTarget) []store.AssignmentCleanupTarget {
			targets[0].AssignmentID = "AAAAAAAA-0000-4000-8000-000000000001"
			targets[0].RuntimeStatePath = assignmentPath(targets[0].AssignmentID)
			return targets[:1]
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := collectionLease(1)
			targets := test.mutate(cleanupTargets())
			durable := &workerStore{leases: []*store.JobLease{&lease}, authorization: store.AssignmentCollectionAuthorization{Targets: targets}}
			cleaner := &recordingCleaner{store: durable}
			worker := newWorker(t, durable, cleaner, time.Second, time.Millisecond)

			processed, err := worker.ProcessNext(context.Background())
			if !processed || !errors.Is(err, retention.ErrInvalidCleanupTargets) {
				t.Fatalf("ProcessNext() = (%t, %v), want invalid targets", processed, err)
			}
			if len(cleaner.callsSnapshot()) != 0 || durable.finalizations != 0 {
				t.Fatalf("cleanup/finalizations = (%d, %d), want none", len(cleaner.callsSnapshot()), durable.finalizations)
			}
			if durable.failures != 1 || durable.failureRetryable {
				t.Fatalf("FailJob() = %d calls, retryable %t", durable.failures, durable.failureRetryable)
			}
		})
	}
}

func TestWorkerPartialCleanupConsumesAttemptAndRetryIsIdempotent(t *testing.T) {
	firstLease, secondLease := collectionLease(1), collectionLease(2)
	secondLease.LeaseToken = "eeeeeeee-0000-4000-8000-000000000002"
	targets := cleanupTargets()
	durable := &workerStore{
		leases:        []*store.JobLease{&firstLease, &secondLease},
		authorization: store.AssignmentCollectionAuthorization{Targets: targets},
	}
	cleaner := &recordingCleaner{store: durable, results: []error{nil, errors.New("storage unavailable")}}
	worker := newWorker(t, durable, cleaner, time.Second, 7*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("first ProcessNext() = (%t, %v), want partial cleanup failure", processed, err)
	}
	if durable.failures != 1 || !durable.failureRetryable || durable.failureDelay != 7*time.Millisecond || durable.finalizations != 0 {
		t.Fatalf("first failure = calls %d retryable %t delay %s, finalizations %d", durable.failures, durable.failureRetryable, durable.failureDelay, durable.finalizations)
	}

	processed, err = worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("retry ProcessNext() = (%t, %v), want collected", processed, err)
	}
	want := []cleanupCall{
		{assignmentID: assignmentOne, path: assignmentPath(assignmentOne)},
		{assignmentID: assignmentTwo, path: assignmentPath(assignmentTwo)},
		{assignmentID: assignmentOne, path: assignmentPath(assignmentOne)},
		{assignmentID: assignmentTwo, path: assignmentPath(assignmentTwo)},
	}
	if calls := cleaner.callsSnapshot(); !slices.Equal(calls, want) {
		t.Fatalf("idempotent cleanup calls = %#v, want %#v", calls, want)
	}
	if durable.authorizations != 2 || durable.finalizations != 1 || !slices.Equal(durable.confirmed, targets) {
		t.Fatalf("authorizations/finalizations = (%d, %d), confirmed %#v", durable.authorizations, durable.finalizations, durable.confirmed)
	}
}

func TestWorkerDoesNotCleanWhenAuthorizationFails(t *testing.T) {
	lease := collectionLease(1)
	durable := &workerStore{leases: []*store.JobLease{&lease}, authorizationErr: store.ErrAssignmentCollectionFenceLost}
	cleaner := &recordingCleaner{store: durable}
	worker := newWorker(t, durable, cleaner, time.Second, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want fence loss", processed, err)
	}
	if len(cleaner.callsSnapshot()) != 0 || durable.finalizations != 0 || durable.failures != 0 {
		t.Fatalf("cleanup/finalization/failure calls = (%d, %d, %d)", len(cleaner.callsSnapshot()), durable.finalizations, durable.failures)
	}
}

func TestWorkerHeartbeatFenceLossCancelsCleanupWithoutFinalizationOrFailure(t *testing.T) {
	lease := collectionLease(1)
	durable := &workerStore{
		leases: []*store.JobLease{&lease}, authorization: store.AssignmentCollectionAuthorization{Targets: cleanupTargets()},
		heartbeatErr: store.ErrJobLeaseLost,
	}
	cleaner := &recordingCleaner{store: durable, blockUntilCanceled: true}
	worker := newWorker(t, durable, cleaner, 100*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrJobLeaseLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want lease loss", processed, err)
	}
	if !cleaner.canceled || durable.heartbeats == 0 || durable.finalizations != 0 || durable.failures != 0 {
		t.Fatalf("canceled/heartbeats/finalizations/failures = (%t, %d, %d, %d)", cleaner.canceled, durable.heartbeats, durable.finalizations, durable.failures)
	}
}

func TestWorkerRecordsFinalizeAmbiguityWithoutClaimingDatabaseDeletion(t *testing.T) {
	lease := collectionLease(1)
	durable := &workerStore{
		leases: []*store.JobLease{&lease}, authorization: store.AssignmentCollectionAuthorization{Targets: cleanupTargets()},
		finalizeErr: errors.New("commit outcome unknown"),
	}
	cleaner := &recordingCleaner{store: durable}
	worker := newWorker(t, durable, cleaner, time.Second, 3*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want commit ambiguity", processed, err)
	}
	if durable.finalizations != 1 || durable.failures != 1 || !durable.failureRetryable {
		t.Fatalf("finalizations/failures/retryable = (%d, %d, %t)", durable.finalizations, durable.failures, durable.failureRetryable)
	}
	if len(cleaner.callsSnapshot()) != 2 {
		t.Fatalf("EnsureAbsent() calls = %d, want all paths confirmed before finalization", len(cleaner.callsSnapshot()))
	}
}

func TestWorkerReturnsIdleWithoutAuthorization(t *testing.T) {
	durable := &workerStore{}
	cleaner := &recordingCleaner{store: durable}
	worker := newWorker(t, durable, cleaner, time.Second, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || processed || durable.authorizations != 0 || len(cleaner.callsSnapshot()) != 0 {
		t.Fatalf("ProcessNext() = (%t, %v), authorizations %d cleanup %d", processed, err, durable.authorizations, len(cleaner.callsSnapshot()))
	}
}

func TestNewWorkerValidatesConfiguration(t *testing.T) {
	valid := retention.WorkerConfig{
		ClaimOwner: "retention-worker", LeaseDuration: 2 * time.Microsecond,
		HeartbeatInterval: time.Microsecond, IdlePollInterval: time.Microsecond,
		RetryDelay: time.Microsecond,
	}
	for _, test := range []struct {
		name   string
		mutate func(*retention.WorkerConfig)
	}{
		{name: "empty owner", mutate: func(config *retention.WorkerConfig) { config.ClaimOwner = "" }},
		{name: "zero lease", mutate: func(config *retention.WorkerConfig) { config.LeaseDuration = 0 }},
		{name: "zero heartbeat", mutate: func(config *retention.WorkerConfig) { config.HeartbeatInterval = 0 }},
		{name: "heartbeat equals lease", mutate: func(config *retention.WorkerConfig) { config.HeartbeatInterval = config.LeaseDuration }},
		{name: "zero poll", mutate: func(config *retention.WorkerConfig) { config.IdlePollInterval = 0 }},
		{name: "zero retry", mutate: func(config *retention.WorkerConfig) { config.RetryDelay = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := retention.NewWorker(&workerStore{}, &recordingCleaner{}, config); err == nil {
				t.Fatal("NewWorker() error = nil, want validation error")
			}
		})
	}
	var nilCleaner *recordingCleaner
	if _, err := retention.NewWorker(&workerStore{}, nilCleaner, valid); err == nil {
		t.Fatal("NewWorker() with typed nil Cleaner error = nil")
	}
}

func newWorker(t *testing.T, durable *workerStore, cleaner *recordingCleaner, lease, retry time.Duration) *retention.Worker {
	t.Helper()
	heartbeat := lease / 5
	if heartbeat < time.Microsecond {
		heartbeat = time.Microsecond
	}
	worker, err := retention.NewWorker(durable, cleaner, retention.WorkerConfig{
		ClaimOwner: "retention-worker", LeaseDuration: lease, HeartbeatInterval: heartbeat,
		IdlePollInterval: time.Millisecond, RetryDelay: retry,
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return worker
}

const (
	assignmentOne = "aaaaaaaa-0000-4000-8000-000000000001"
	assignmentTwo = "bbbbbbbb-0000-4000-8000-000000000002"
	sessionOne    = "cccccccc-0000-4000-8000-000000000001"
)

func assignmentPath(assignmentID string) string {
	return "assignment-" + assignmentID + "/runtime-state"
}

func cleanupTargets() []store.AssignmentCleanupTarget {
	return []store.AssignmentCleanupTarget{
		{AssignmentID: assignmentOne, RuntimeStatePath: assignmentPath(assignmentOne), RuntimeImageDigest: "sha256:one"},
		{AssignmentID: assignmentOne, SessionID: sessionOne, RuntimeStatePath: assignmentPath(assignmentOne), RuntimeImageDigest: "sha256:one"},
		{AssignmentID: assignmentTwo, RuntimeStatePath: assignmentPath(assignmentTwo), RuntimeImageDigest: "sha256:two"},
	}
}

func collectionLease(attempt int) store.JobLease {
	return store.JobLease{Job: store.Job{
		JobSpec: store.JobSpec{Queue: store.WorkflowActionQueue, Kind: store.CollectAssignmentsJobKind},
		ID:      "dddddddd-0000-4000-8000-000000000001", Status: store.JobLeased,
		AttemptCount: attempt, LeaseOwner: "retention-worker", LeaseToken: "eeeeeeee-0000-4000-8000-000000000001",
	}, Attempt: attempt}
}

type workerStore struct {
	mutex sync.Mutex

	leases                            []*store.JobLease
	claimQueue, claimKind, claimOwner string
	claimLease                        time.Duration
	authorization                     store.AssignmentCollectionAuthorization
	authorizationErr                  error
	authorizations                    int
	authorized                        bool
	heartbeats                        int
	heartbeatErr                      error
	finalizations                     int
	confirmed                         []store.AssignmentCleanupTarget
	finalizeErr                       error
	failures                          int
	failureRetryable                  bool
	failureDelay                      time.Duration
}

func (durable *workerStore) ClaimJobKind(_ context.Context, queue, kind, owner string, lease time.Duration) (*store.JobLease, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimLease = queue, kind, owner, lease
	if len(durable.leases) == 0 {
		return nil, nil
	}
	claimed := durable.leases[0]
	durable.leases = durable.leases[1:]
	return claimed, nil
}

func (durable *workerStore) HeartbeatJob(_ context.Context, _ store.JobLease, _ time.Duration) error {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.heartbeats++
	return durable.heartbeatErr
}

func (durable *workerStore) AuthorizeAssignmentCollection(_ context.Context, _ store.JobLease) (store.AssignmentCollectionAuthorization, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.authorizations++
	if durable.authorizationErr == nil {
		durable.authorized = true
	}
	return durable.authorization, durable.authorizationErr
}

func (durable *workerStore) FinalizeAssignmentCollection(_ context.Context, _ store.JobLease, confirmed []store.AssignmentCleanupTarget) (store.AssignmentCollection, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.finalizations++
	durable.confirmed = slices.Clone(confirmed)
	return store.AssignmentCollection{}, durable.finalizeErr
}

func (durable *workerStore) AcknowledgeAssignmentCollectionFailure(_ context.Context, _ store.JobLease, _ error, retryable bool, delay time.Duration) (store.WorkflowActionFailureAcknowledgement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.failures++
	durable.failureRetryable, durable.failureDelay = retryable, delay
	return store.WorkflowActionFailureAcknowledgement{RetryScheduled: retryable}, nil
}

type cleanupCall struct {
	assignmentID string
	path         string
}

type recordingCleaner struct {
	mutex              sync.Mutex
	store              *workerStore
	calls              []cleanupCall
	results            []error
	unauthorizedCall   bool
	blockUntilCanceled bool
	canceled           bool
}

func (cleaner *recordingCleaner) EnsureAbsent(ctx context.Context, assignmentID, path string) error {
	cleaner.mutex.Lock()
	cleaner.calls = append(cleaner.calls, cleanupCall{assignmentID: assignmentID, path: path})
	if cleaner.store != nil {
		cleaner.store.mutex.Lock()
		cleaner.unauthorizedCall = cleaner.unauthorizedCall || !cleaner.store.authorized
		cleaner.store.mutex.Unlock()
	}
	block := cleaner.blockUntilCanceled
	var result error
	if len(cleaner.results) != 0 {
		result = cleaner.results[0]
		cleaner.results = cleaner.results[1:]
	}
	cleaner.mutex.Unlock()
	if block {
		<-ctx.Done()
		cleaner.mutex.Lock()
		cleaner.canceled = true
		cleaner.mutex.Unlock()
		return ctx.Err()
	}
	return result
}

func (cleaner *recordingCleaner) callsSnapshot() []cleanupCall {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	return slices.Clone(cleaner.calls)
}
