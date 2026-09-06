package agentturn_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestStopWorkerClaimsOnlyStopJobsAndAcknowledgesExactRuntimeAbsence(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want acknowledged stop", processed, err)
	}
	if durable.claimQueue != store.AgentTurnRecoveryQueue || durable.claimKind != store.StopStaleRuntimeJobKind ||
		durable.claimOwner != "runtime-stop-worker" || durable.claimDuration != 100*time.Millisecond {
		t.Fatalf("ClaimJobKind() = (%q, %q, %q, %s)", durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimDuration)
	}
	wantLabels := map[string]string{
		store.RuntimeLabelAssignmentID: lease.AgentAssignmentID,
		store.RuntimeLabelSessionID:    lease.AgentSessionID,
		store.RuntimeLabelTurnID:       lease.AgentTurnID,
		store.RuntimeLabelEpoch:        "7",
	}
	if cleaner.callCount() != 1 || !maps.Equal(cleaner.labelsAt(0), wantLabels) {
		t.Fatalf("EnsureAbsent() labels = %#v, want %#v", cleaner.labelsAt(0), wantLabels)
	}
	if durable.acknowledgements != 1 || durable.acknowledged.ID != lease.ID || durable.acknowledged.LeaseToken != lease.LeaseToken {
		t.Fatalf("AcknowledgeRecoveredRuntimeStopped() calls = %d, lease %#v", durable.acknowledgements, durable.acknowledged)
	}
}

func TestStopWorkerDiscardsRecoveredReviewerWorkspaceBeforeAcknowledgement(t *testing.T) {
	lease := staleRuntimeLease()
	cleanup := store.AgentTurnRuntimeCleanupContext{
		AssignmentID: "60000000-0000-4000-8000-000000000001",
		Role:         workflow.RoleReviewer,
	}
	durable := &stopWorkerStore{lease: &lease, cleanupContext: cleanup}
	discarder := &recordingWorkspaceDiscarder{}
	worker := newStopWorkerWithWorkspace(t, durable, &recordingRuntimeCleaner{}, discarder, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want acknowledged Reviewer cleanup", processed, err)
	}
	if discarder.callCount() != 1 || discarder.assignmentAt(0) != cleanup.AssignmentID {
		t.Fatalf("DiscardWorkspace() calls = %d, assignment %q, want exactly %q", discarder.callCount(), discarder.assignmentAt(0), cleanup.AssignmentID)
	}
	if durable.acknowledgements != 1 || durable.discardSuccessesAtAcknowledgement != 1 {
		t.Fatalf("acknowledgements = %d after %d successful discards", durable.acknowledgements, durable.discardSuccessesAtAcknowledgement)
	}
}

func TestStopWorkerRetainsRecoveredDeveloperWorkspace(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
		AssignmentID: lease.AgentAssignmentID,
		Role:         workflow.RoleDeveloper,
	}}
	discarder := &recordingWorkspaceDiscarder{}
	worker := newStopWorkerWithWorkspace(t, durable, &recordingRuntimeCleaner{}, discarder, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want acknowledged Developer cleanup", processed, err)
	}
	if discarder.callCount() != 0 {
		t.Fatalf("DiscardWorkspace() calls = %d, want Developer workspace retained", discarder.callCount())
	}
	if durable.acknowledgements != 1 {
		t.Fatalf("acknowledgements = %d, want 1", durable.acknowledgements)
	}
}

func TestStopWorkerReviewerDiscardFailureConsumesOneClaimedAttempt(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
		AssignmentID: lease.AgentAssignmentID,
		Role:         workflow.RoleReviewer,
	}}
	failure := errors.New("workspace temporarily unavailable")
	discarder := &recordingWorkspaceDiscarder{results: []error{failure}}
	worker := newStopWorkerWithWorkspace(t, durable, &recordingRuntimeCleaner{}, discarder, 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !errors.Is(err, failure) || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want one failed discard attempt", processed, err)
	}
	if discarder.callCount() != 1 || durable.claims.Load() != 1 {
		t.Fatalf("DiscardWorkspace()/ClaimJobKind() calls = (%d, %d), want (1, 1)", discarder.callCount(), durable.claims.Load())
	}
	if durable.failureAcknowledgements != 1 || durable.acknowledgements != 0 {
		t.Fatalf("failure/success acknowledgements = (%d, %d), want (1, 0)", durable.failureAcknowledgements, durable.acknowledgements)
	}
}

func TestStopWorkerCleanupContextFailureConsumesOneClaimedAttempt(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{
		lease: &lease,
		cleanupContext: store.AgentTurnRuntimeCleanupContext{
			AssignmentID: lease.AgentAssignmentID,
			Role:         workflow.RoleDeveloper,
		},
		cleanupContextResults: []error{errors.New("database temporarily unavailable")},
	}
	worker := newStopWorker(t, durable, &recordingRuntimeCleaner{}, 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err == nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want one failed context-read attempt", processed, err)
	}
	if durable.cleanupContextCallCount() != 1 || durable.claims.Load() != 1 {
		t.Fatalf("cleanup context/claim calls = (%d, %d), want (1, 1)", durable.cleanupContextCallCount(), durable.claims.Load())
	}
	if durable.failureAcknowledgements != 1 || durable.acknowledgements != 0 {
		t.Fatalf("failure/success acknowledgements = (%d, %d), want (1, 0)", durable.failureAcknowledgements, durable.acknowledgements)
	}
}

func TestStopWorkerReturnsCleanupContextFenceLossWithoutRetry(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContextResults: []error{store.ErrAgentTurnRecoveryFenceLost}}
	worker := newStopWorker(t, durable, &recordingRuntimeCleaner{}, 100*time.Millisecond, time.Millisecond, time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want prompt recovery fence loss", processed, err)
	}
	if durable.cleanupContextCallCount() != 1 || durable.acknowledgements != 0 {
		t.Fatalf("cleanup context/acknowledgement calls = (%d, %d), want (1, 0)", durable.cleanupContextCallCount(), durable.acknowledgements)
	}
}

func TestStopWorkerRuntimeCleanupFailureConsumesOneClaimedAttempt(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	failure := errors.New("Docker temporarily unavailable")
	cleaner := &recordingRuntimeCleaner{results: []error{failure}}
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !errors.Is(err, failure) || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want one failed cleanup attempt", processed, err)
	}
	if cleaner.callCount() != 1 {
		t.Fatalf("EnsureAbsent() calls = %d, want 1 within one claim", cleaner.callCount())
	}
	if durable.claims.Load() != 1 {
		t.Fatalf("ClaimJobKind() calls = %d, want one durable attempt", durable.claims.Load())
	}
	if durable.failureAcknowledgements != 1 || durable.acknowledgements != 0 || durable.failureDelay != 5*time.Millisecond {
		t.Fatalf("failure/success acknowledgements = (%d, %d), delay %s", durable.failureAcknowledgements, durable.acknowledgements, durable.failureDelay)
	}
}

func TestStopWorkerStopsHeartbeatBeforeFailureAcknowledgement(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	failure := errors.New("Docker unavailable")
	cleaner := &recordingRuntimeCleaner{results: []error{failure}, delay: 10 * time.Millisecond}
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, failure) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	atAcknowledgement := durable.heartbeatsAtFailureAcknowledgement
	if atAcknowledgement == 0 {
		t.Fatal("HeartbeatJob() did not run during cleanup")
	}
	time.Sleep(5 * time.Millisecond)
	if got := durable.heartbeatCount(); got != atAcknowledgement {
		t.Fatalf("HeartbeatJob() calls after failure acknowledgement = %d, want %d", got, atAcknowledgement)
	}
}

func TestStopWorkerRunReportsFailureWithoutBusyLoop(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{results: []error{errors.New("Docker unavailable")}}
	reported := make(chan error, 1)
	worker, err := agentturn.NewStopWorker(durable, cleaner, &recordingWorkspaceDiscarder{}, agentturn.StopWorkerConfig{
		ClaimOwner: "runtime-stop-worker", LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: time.Millisecond, IdlePollInterval: 50 * time.Millisecond,
		CleanupRetryInterval: time.Millisecond, OnError: func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("OnError received nil")
		}
	case <-time.After(time.Second):
		t.Fatal("OnError was not called")
	}
	time.Sleep(5 * time.Millisecond)
	if claims := durable.claims.Load(); claims != 1 {
		t.Fatalf("ClaimJobKind() calls before idle delay elapsed = %d, want 1", claims)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestStopWorkerCancelsCleanupAndStopsPollingWhenHeartbeatFenceIsStale(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, heartbeatErr: store.ErrJobLeaseLost}
	cleaner := &recordingRuntimeCleaner{blockUntilCanceled: true}
	durable.cleaner = cleaner
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, time.Millisecond, time.Millisecond)

	err := worker.Run(context.Background())
	if !errors.Is(err, store.ErrJobLeaseLost) {
		t.Fatalf("Run() error = %v, want ErrJobLeaseLost", err)
	}
	if !cleaner.wasCanceled() {
		t.Fatal("EnsureAbsent() context was not canceled after heartbeat fence loss")
	}
	if durable.claims.Load() != 1 {
		t.Fatalf("ClaimJobKind() calls = %d, want no polling after fence loss", durable.claims.Load())
	}
	if durable.acknowledgements != 0 {
		t.Fatalf("stale worker acknowledgements = %d, want zero", durable.acknowledgements)
	}
}

func TestStopWorkerReturnsIdleWithoutCleanup(t *testing.T) {
	durable := &stopWorkerStore{}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, time.Second, time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || processed {
		t.Fatalf("ProcessNext() = (%t, %v), want idle", processed, err)
	}
	if cleaner.callCount() != 0 || durable.acknowledgements != 0 {
		t.Fatalf("idle cleanup/acknowledgements = (%d, %d)", cleaner.callCount(), durable.acknowledgements)
	}
}

func TestStopWorkerRejectsInvalidJobIdentityWithoutCleanupOrAcknowledgement(t *testing.T) {
	lease := staleRuntimeLease()
	lease.AgentSessionID = "invalid-session"
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, time.Second, time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err == nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want invalid claimed job", processed, err)
	}
	if strings.Contains(err.Error(), lease.AgentSessionID) {
		t.Fatalf("ProcessNext() error exposes invalid identity: %v", err)
	}
	if cleaner.callCount() != 0 || durable.acknowledgements != 0 {
		t.Fatalf("invalid identity cleanup/acknowledgements = (%d, %d)", cleaner.callCount(), durable.acknowledgements)
	}
}

func TestStopWorkerRunPollsWhileNoJobIsAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	durable := &stopWorkerStore{}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, time.Second, time.Millisecond, time.Millisecond)
	go func() {
		for durable.claims.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	err := worker.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if durable.claims.Load() < 3 {
		t.Fatalf("ClaimJobKind() calls = %d, want idle polling", durable.claims.Load())
	}
	if cleaner.callCount() != 0 {
		t.Fatalf("EnsureAbsent() calls = %d without a job", cleaner.callCount())
	}
}

func TestNewStopWorkerValidatesConfiguration(t *testing.T) {
	valid := agentturn.StopWorkerConfig{
		ClaimOwner: "runtime-stop-worker", LeaseDuration: 2 * time.Microsecond,
		HeartbeatInterval: time.Microsecond, IdlePollInterval: time.Microsecond,
		CleanupRetryInterval: time.Microsecond,
	}
	for _, test := range []struct {
		name   string
		mutate func(*agentturn.StopWorkerConfig)
	}{
		{name: "empty owner", mutate: func(config *agentturn.StopWorkerConfig) { config.ClaimOwner = "" }},
		{name: "zero lease", mutate: func(config *agentturn.StopWorkerConfig) { config.LeaseDuration = 0 }},
		{name: "zero heartbeat", mutate: func(config *agentturn.StopWorkerConfig) { config.HeartbeatInterval = 0 }},
		{name: "heartbeat equals lease", mutate: func(config *agentturn.StopWorkerConfig) { config.HeartbeatInterval = config.LeaseDuration }},
		{name: "zero poll", mutate: func(config *agentturn.StopWorkerConfig) { config.IdlePollInterval = 0 }},
		{name: "zero cleanup retry", mutate: func(config *agentturn.StopWorkerConfig) { config.CleanupRetryInterval = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := agentturn.NewStopWorker(&stopWorkerStore{}, &recordingRuntimeCleaner{}, &recordingWorkspaceDiscarder{}, config); err == nil {
				t.Fatal("NewStopWorker() error = nil, want validation error")
			}
		})
	}
	var nilDiscarder *recordingWorkspaceDiscarder
	if _, err := agentturn.NewStopWorker(&stopWorkerStore{}, &recordingRuntimeCleaner{}, nilDiscarder, valid); err == nil {
		t.Fatal("NewStopWorker() with nil workspace discarder error = nil, want validation error")
	}
}

func newStopWorker(t *testing.T, durable *stopWorkerStore, cleaner *recordingRuntimeCleaner, lease, heartbeat, retry time.Duration) *agentturn.StopWorker {
	t.Helper()
	return newStopWorkerWithWorkspace(t, durable, cleaner, &recordingWorkspaceDiscarder{}, lease, heartbeat, retry)
}

func newStopWorkerWithWorkspace(t *testing.T, durable *stopWorkerStore, cleaner *recordingRuntimeCleaner, workspaces *recordingWorkspaceDiscarder, lease, heartbeat, retry time.Duration) *agentturn.StopWorker {
	t.Helper()
	durable.cleaner = cleaner
	durable.workspaces = workspaces
	if durable.cleanupContext.AssignmentID == "" && durable.lease != nil {
		durable.cleanupContext = store.AgentTurnRuntimeCleanupContext{
			AssignmentID: durable.lease.AgentAssignmentID,
			Role:         workflow.RoleDeveloper,
		}
	}
	worker, err := agentturn.NewStopWorker(durable, cleaner, workspaces, agentturn.StopWorkerConfig{
		ClaimOwner: "runtime-stop-worker", LeaseDuration: lease, HeartbeatInterval: heartbeat,
		IdlePollInterval: time.Millisecond, CleanupRetryInterval: retry,
	})
	if err != nil {
		t.Fatalf("NewStopWorker() error = %v", err)
	}
	return worker
}

func staleRuntimeLease() store.JobLease {
	return store.JobLease{Job: store.Job{
		JobSpec: store.JobSpec{
			Queue: store.AgentTurnRecoveryQueue, Kind: store.StopStaleRuntimeJobKind,
			AgentAssignmentID: "10000000-0000-4000-8000-000000000001",
			AgentSessionID:    "20000000-0000-4000-8000-000000000001",
			AgentTurnID:       "30000000-0000-4000-8000-000000000001",
			ExecutionEpoch:    7,
		},
		ID: "40000000-0000-4000-8000-000000000001", Status: store.JobLeased,
		AttemptCount: 1, LeaseOwner: "runtime-stop-worker", LeaseToken: "50000000-0000-4000-8000-000000000001",
	}, Attempt: 1}
}

type stopWorkerStore struct {
	mutex sync.Mutex
	lease *store.JobLease

	claims                             atomic.Int32
	claimQueue, claimKind, claimOwner  string
	claimDuration                      time.Duration
	heartbeats                         int
	heartbeatExtension                 time.Duration
	heartbeatErr                       error
	acknowledgements                   int
	acknowledged                       store.JobLease
	cleanupCallsAtAcknowledgement      int
	cleaner                            *recordingRuntimeCleaner
	cleanupContext                     store.AgentTurnRuntimeCleanupContext
	cleanupContextResults              []error
	cleanupContextCalls                int
	cleanupContextLease                store.JobLease
	discardCallsAtAcknowledgement      int
	discardSuccessesAtAcknowledgement  int
	workspaces                         *recordingWorkspaceDiscarder
	failureAcknowledgements            int
	failureAcknowledged                store.JobLease
	failureDelay                       time.Duration
	failureErr                         error
	failureAcknowledgementErr          error
	heartbeatsAtFailureAcknowledgement int
}

func (durable *stopWorkerStore) ClaimJobKind(_ context.Context, queue, kind, owner string, duration time.Duration) (*store.JobLease, error) {
	durable.claims.Add(1)
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimDuration = queue, kind, owner, duration
	lease := durable.lease
	durable.lease = nil
	return lease, nil
}

func (durable *stopWorkerStore) HeartbeatJob(_ context.Context, _ store.JobLease, extension time.Duration) error {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.heartbeats++
	durable.heartbeatExtension = extension
	return durable.heartbeatErr
}

func (durable *stopWorkerStore) heartbeatCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.heartbeats
}

func (durable *stopWorkerStore) GetAgentTurnRuntimeCleanupContext(_ context.Context, lease store.JobLease) (store.AgentTurnRuntimeCleanupContext, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.cleanupContextCalls++
	durable.cleanupContextLease = lease
	var result error
	if len(durable.cleanupContextResults) != 0 {
		result = durable.cleanupContextResults[0]
		durable.cleanupContextResults = durable.cleanupContextResults[1:]
	}
	return durable.cleanupContext, result
}

func (durable *stopWorkerStore) cleanupContextCallCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.cleanupContextCalls
}

func (durable *stopWorkerStore) AcknowledgeRecoveredRuntimeStopped(_ context.Context, lease store.JobLease) (store.AgentTurnRecovery, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.acknowledgements++
	durable.acknowledged = lease
	if durable.cleaner != nil {
		durable.cleanupCallsAtAcknowledgement = durable.cleaner.callCount()
	}
	if durable.workspaces != nil {
		durable.discardCallsAtAcknowledgement = durable.workspaces.callCount()
		durable.discardSuccessesAtAcknowledgement = durable.workspaces.successCount()
	}
	return store.AgentTurnRecovery{}, nil
}

func (durable *stopWorkerStore) AcknowledgeRecoveredRuntimeStopFailure(_ context.Context, lease store.JobLease, cause error, delay time.Duration) (store.AgentTurnRuntimeStopFailureAcknowledgement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.failureAcknowledgements++
	durable.failureAcknowledged = lease
	durable.failureErr = cause
	durable.failureDelay = delay
	durable.heartbeatsAtFailureAcknowledgement = durable.heartbeats
	return store.AgentTurnRuntimeStopFailureAcknowledgement{RetryScheduled: true}, durable.failureAcknowledgementErr
}

type recordingRuntimeCleaner struct {
	mutex              sync.Mutex
	results            []error
	labels             []map[string]string
	blockUntilCanceled bool
	canceled           bool
	delay              time.Duration
}

func (cleaner *recordingRuntimeCleaner) EnsureAbsent(ctx context.Context, labels map[string]string) error {
	cleaner.mutex.Lock()
	cleaner.labels = append(cleaner.labels, maps.Clone(labels))
	block := cleaner.blockUntilCanceled
	var result error
	if len(cleaner.results) != 0 {
		result = cleaner.results[0]
		cleaner.results = cleaner.results[1:]
	}
	cleaner.mutex.Unlock()
	if cleaner.delay > 0 {
		timer := time.NewTimer(cleaner.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if block {
		<-ctx.Done()
		cleaner.mutex.Lock()
		cleaner.canceled = true
		cleaner.mutex.Unlock()
		return ctx.Err()
	}
	return result
}

func (cleaner *recordingRuntimeCleaner) callCount() int {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	return len(cleaner.labels)
}

func (cleaner *recordingRuntimeCleaner) labelsAt(index int) map[string]string {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	if index >= len(cleaner.labels) {
		return nil
	}
	return maps.Clone(cleaner.labels[index])
}

func (cleaner *recordingRuntimeCleaner) wasCanceled() bool {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	return cleaner.canceled
}

type recordingWorkspaceDiscarder struct {
	mutex         sync.Mutex
	results       []error
	assignmentIDs []string
	successes     int
}

func (discarder *recordingWorkspaceDiscarder) DiscardWorkspace(assignmentID string) error {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	discarder.assignmentIDs = append(discarder.assignmentIDs, assignmentID)
	var result error
	if len(discarder.results) != 0 {
		result = discarder.results[0]
		discarder.results = discarder.results[1:]
	}
	if result == nil {
		discarder.successes++
	}
	return result
}

func (discarder *recordingWorkspaceDiscarder) callCount() int {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	return len(discarder.assignmentIDs)
}

func (discarder *recordingWorkspaceDiscarder) assignmentAt(index int) string {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	if index >= len(discarder.assignmentIDs) {
		return ""
	}
	return discarder.assignmentIDs[index]
}

func (discarder *recordingWorkspaceDiscarder) successCount() int {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	return discarder.successes
}
