package workflowaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflowaction"
)

const (
	closureAssignment = "10000000-0000-4000-8000-000000000001"
	closureSession    = "20000000-0000-4000-8000-000000000001"
	closureTurn       = "30000000-0000-4000-8000-000000000001"
)

func TestClosureStopWorkerClaimsExactKindAndCleansCompleteIdentityBeforeAcknowledgement(t *testing.T) {
	lease := closureLease(store.StopAgentTurnJobKind, true)
	durable := &closureStopStore{lease: &lease, identity: closureRuntimeIdentity()}
	cleaner := &closureCleaner{}
	durable.cleaner = cleaner
	worker := newClosureStopWorker(t, durable, cleaner)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.claimQueue != store.WorkflowActionQueue || durable.claimKind != store.StopAgentTurnJobKind || durable.claimOwner != "closure-worker" {
		t.Fatalf("claim = (%q, %q, %q)", durable.claimQueue, durable.claimKind, durable.claimOwner)
	}
	want := map[string]string{
		store.RuntimeLabelAssignmentID: closureAssignment,
		store.RuntimeLabelSessionID:    closureSession,
		store.RuntimeLabelTurnID:       closureTurn,
		store.RuntimeLabelEpoch:        "7",
		"io.omnigrex.runtime-profile":  "opencode-acp/v1",
	}
	if cleaner.calls != 1 || !maps.Equal(cleaner.labels, want) {
		t.Fatalf("cleanup = %d %#v, want %#v", cleaner.calls, cleaner.labels, want)
	}
	if durable.acknowledgements != 1 || durable.cleanerCallsAtAck != 1 || durable.failures != 0 {
		t.Fatalf("acknowledgements/failures = %d/%d after %d cleanup calls", durable.acknowledgements, durable.failures, durable.cleanerCallsAtAck)
	}
}

func TestClosureStopWorkerNeverCleansSimilarDifferentEpochIdentity(t *testing.T) {
	lease := closureLease(store.StopAgentTurnJobKind, true)
	identity := closureRuntimeIdentity()
	identity.ExecutionEpoch++
	durable := &closureStopStore{lease: &lease, identity: identity}
	cleaner := &closureCleaner{}
	worker := newClosureStopWorker(t, durable, cleaner)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, workflowaction.ErrInvalidClosureRuntimeIdentity) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if cleaner.calls != 0 || durable.acknowledgements != 0 || durable.failures != 1 || durable.failureRetryable {
		t.Fatalf("cleanup/ack/failure/retryable = %d/%d/%d/%t", cleaner.calls, durable.acknowledgements, durable.failures, durable.failureRetryable)
	}
}

func TestClosureStopWorkerHeartbeatsAndSchedulesRetryOnCleanerFailure(t *testing.T) {
	lease := closureLease(store.StopAgentTurnJobKind, true)
	durable := &closureStopStore{lease: &lease, identity: closureRuntimeIdentity()}
	cleaner := &closureCleaner{err: errors.New("Docker unavailable"), delay: 5 * time.Millisecond}
	worker := newClosureStopWorker(t, durable, cleaner)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.heartbeats == 0 || durable.failures != 1 || !durable.failureRetryable || durable.retryDelay != 10*time.Millisecond || durable.acknowledgements != 0 {
		t.Fatalf("heartbeats/failure/retry/delay/ack = %d/%d/%t/%s/%d", durable.heartbeats, durable.failures, durable.failureRetryable, durable.retryDelay, durable.acknowledgements)
	}
}

func TestClosureStopWorkerDoesNotAcknowledgeAfterFenceLoss(t *testing.T) {
	lease := closureLease(store.StopAgentTurnJobKind, true)
	durable := &closureStopStore{lease: &lease, identityErr: store.ErrClosureSettlementFenceLost}
	worker := newClosureStopWorker(t, durable, &closureCleaner{})

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrClosureSettlementFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.failures != 0 || durable.acknowledgements != 0 {
		t.Fatalf("failure/acknowledgement calls = %d/%d", durable.failures, durable.acknowledgements)
	}
}

func TestClosureStopWorkerCancelsCleanupAfterHeartbeatLeaseLoss(t *testing.T) {
	lease := closureLease(store.StopAgentTurnJobKind, true)
	durable := &closureStopStore{
		lease: &lease, identity: closureRuntimeIdentity(), heartbeatErr: store.ErrJobLeaseLost,
	}
	cleaner := &closureCleaner{delay: time.Second}
	worker := newClosureStopWorker(t, durable, cleaner)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrJobLeaseLost) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.failures != 0 || durable.acknowledgements != 0 || cleaner.callCount() != 0 {
		t.Fatalf("failures/acknowledgements/cleanup completions = %d/%d/%d", durable.failures, durable.acknowledgements, cleaner.callCount())
	}
}

func TestClosureSettlementWorkerCompletesNoTurnClosureDirectly(t *testing.T) {
	lease := closureLease(store.SettleClosureJobKind, false)
	durable := &closureSettlementStore{lease: &lease}
	reconciler := &closureReconciler{}
	worker := newClosureSettlementWorker(t, durable, reconciler)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.claimQueue != store.WorkflowActionQueue || durable.claimKind != store.SettleClosureJobKind || durable.claimOwner != "closure-worker" {
		t.Fatalf("claim = (%q, %q, %q)", durable.claimQueue, durable.claimKind, durable.claimOwner)
	}
	if durable.completions != 1 || durable.contextCalls != 0 || durable.listCalls != 0 || reconciler.calls != 0 {
		t.Fatalf("complete/context/list/reconcile = %d/%d/%d/%d", durable.completions, durable.contextCalls, durable.listCalls, reconciler.calls)
	}
}

func TestClosureSettlementWorkerDelaysStopBarrierRaceWithoutEscalation(t *testing.T) {
	lease := closureLease(store.SettleClosureJobKind, true)
	durable := &closureSettlementStore{
		lease: &lease, contextErr: store.ErrClosureSettlementUnsettled,
		waitAcknowledgement: store.WorkflowActionFailureAcknowledgement{RetryScheduled: true},
	}
	reconciler := &closureReconciler{}
	worker := newClosureSettlementWorker(t, durable, reconciler)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.waits != 1 || durable.waitDelay != 10*time.Millisecond || durable.failures != 0 || durable.completions != 0 || reconciler.calls != 0 {
		t.Fatalf("wait/delay/failure/complete/reconcile = %d/%s/%d/%d/%d", durable.waits, durable.waitDelay, durable.failures, durable.completions, reconciler.calls)
	}
}

func TestClosureSettlementWorkerReconcilesUnknownSuccessAndFailureBeforeCompletion(t *testing.T) {
	lease := closureLease(store.SettleClosureJobKind, true)
	mutations := []store.MutationReservation{
		{ID: "40000000-0000-4000-8000-000000000001", InvocationNumber: 1},
		{ID: "40000000-0000-4000-8000-000000000002", InvocationNumber: 2},
	}
	durable := &closureSettlementStore{lease: &lease, mutations: mutations}
	reconciler := &closureReconciler{results: []mcp.MutationReconciliationResult{
		{Disposition: mcp.ReconciliationFound, Outcome: store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: json.RawMessage(`{"comment_id":1}`)}},
		{Disposition: mcp.ReconciliationDefinitelyFailed, Outcome: store.RecoveredMutationOutcome{State: store.MutationFailed, LastError: "artifact absent"}},
	}}
	worker := newClosureSettlementWorker(t, durable, reconciler)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if !reflect.DeepEqual(reconciler.mutationIDs, []string{mutations[0].ID, mutations[1].ID}) ||
		!reflect.DeepEqual(durable.reconciledIDs, []string{mutations[0].ID, mutations[1].ID}) || durable.completions != 1 {
		t.Fatalf("reconciler/store/completions = %v/%v/%d", reconciler.mutationIDs, durable.reconciledIDs, durable.completions)
	}
	if durable.operations[len(durable.operations)-1] != "complete" || durable.outcomes[0].State != store.MutationSucceeded || durable.outcomes[1].State != store.MutationFailed {
		t.Fatalf("operations/outcomes = %v/%#v", durable.operations, durable.outcomes)
	}
}

func TestClosureSettlementWorkerRetriesUnresolvedRemoteOutcome(t *testing.T) {
	lease := closureLease(store.SettleClosureJobKind, true)
	durable := &closureSettlementStore{lease: &lease, mutations: []store.MutationReservation{{ID: "40000000-0000-4000-8000-000000000001", InvocationNumber: 1}}}
	reconciler := &closureReconciler{results: []mcp.MutationReconciliationResult{{Disposition: mcp.ReconciliationUnresolved}}}
	worker := newClosureSettlementWorker(t, durable, reconciler)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, workflowaction.ErrClosureReconciliationUnresolved) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.failures != 1 || !durable.failureRetryable || durable.completions != 0 || len(durable.reconciledIDs) != 0 {
		t.Fatalf("failure/retry/complete/writes = %d/%t/%d/%v", durable.failures, durable.failureRetryable, durable.completions, durable.reconciledIDs)
	}
}

func TestClosureSettlementWorkersCannotConcurrentlyProcessOneClaim(t *testing.T) {
	lease := closureLease(store.SettleClosureJobKind, false)
	durable := &closureSettlementStore{lease: &lease}
	first := newClosureSettlementWorker(t, durable, &closureReconciler{})
	second := newClosureSettlementWorker(t, durable, &closureReconciler{})
	start := make(chan struct{})
	results := make(chan bool, 2)
	for _, worker := range []*workflowaction.ClosureSettlementWorker{first, second} {
		go func(worker *workflowaction.ClosureSettlementWorker) {
			<-start
			processed, err := worker.ProcessNext(context.Background())
			if err != nil {
				t.Errorf("ProcessNext() error = %v", err)
			}
			results <- processed
		}(worker)
	}
	close(start)
	processed := 0
	for range 2 {
		if <-results {
			processed++
		}
	}
	if processed != 1 || durable.completionCount() != 1 {
		t.Fatalf("processed/completions = %d/%d", processed, durable.completionCount())
	}
}

func TestNewClosureWorkersValidateConfiguration(t *testing.T) {
	valid := closureWorkerConfig()
	invalid := valid
	invalid.HeartbeatInterval = invalid.LeaseDuration
	if _, err := workflowaction.NewClosureStopWorker(&closureStopStore{}, &closureCleaner{}, invalid); !errors.Is(err, workflowaction.ErrInvalidClosureWorkerConfiguration) {
		t.Fatalf("NewClosureStopWorker() error = %v", err)
	}
	if _, err := workflowaction.NewClosureSettlementWorker(&closureSettlementStore{}, &closureReconciler{}, invalid); !errors.Is(err, workflowaction.ErrInvalidClosureWorkerConfiguration) {
		t.Fatalf("NewClosureSettlementWorker() error = %v", err)
	}
}

func newClosureStopWorker(t *testing.T, durable workflowaction.ClosureStopStore, cleaner workflowaction.ClosureRuntimeCleaner) *workflowaction.ClosureStopWorker {
	t.Helper()
	worker, err := workflowaction.NewClosureStopWorker(durable, cleaner, closureWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func newClosureSettlementWorker(t *testing.T, durable workflowaction.ClosureSettlementStore, reconciler mcp.MutationReconciler) *workflowaction.ClosureSettlementWorker {
	t.Helper()
	worker, err := workflowaction.NewClosureSettlementWorker(durable, reconciler, closureWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func closureWorkerConfig() workflowaction.ClosureWorkerConfig {
	return workflowaction.ClosureWorkerConfig{
		ClaimOwner: "closure-worker", LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: time.Millisecond, IdlePollInterval: time.Millisecond, RetryDelay: 10 * time.Millisecond,
	}
}

func closureLease(kind string, active bool) store.JobLease {
	spec := store.JobSpec{
		Queue: store.WorkflowActionQueue, Kind: kind, WorkflowID: "50000000-0000-4000-8000-000000000001",
		MaxAttempts: 3,
	}
	if active {
		spec.WorkflowAttemptID = "60000000-0000-4000-8000-000000000001"
		spec.AgentAssignmentID = closureAssignment
		spec.AgentSessionID = closureSession
		spec.AgentTurnID = closureTurn
		spec.ExecutionEpoch = 7
	}
	return store.JobLease{Job: store.Job{
		JobSpec: spec, ID: "70000000-0000-4000-8000-000000000001", Status: store.JobLeased,
		AttemptCount: 1, LeaseOwner: "closure-worker", LeaseToken: "80000000-0000-4000-8000-000000000001",
	}, Attempt: 1}
}

func closureRuntimeIdentity() store.AgentTurnRuntimeIdentity {
	return store.AgentTurnRuntimeIdentity{
		AssignmentID: closureAssignment, AgentSessionID: closureSession, AgentTurnID: closureTurn,
		ExecutionEpoch: 7, RuntimeProfileName: "opencode-acp", RuntimeProfileVersion: "v1",
	}
}

type closureCleaner struct {
	mutex  sync.Mutex
	labels map[string]string
	calls  int
	err    error
	delay  time.Duration
}

func (cleaner *closureCleaner) EnsureAbsent(ctx context.Context, labels map[string]string) error {
	if cleaner.delay > 0 {
		timer := time.NewTimer(cleaner.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	cleaner.calls++
	cleaner.labels = maps.Clone(labels)
	return cleaner.err
}

func (cleaner *closureCleaner) callCount() int {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	return cleaner.calls
}

type closureStopStore struct {
	mutex sync.Mutex
	lease *store.JobLease

	claimQueue, claimKind, claimOwner string
	identity                          store.AgentTurnRuntimeIdentity
	identityErr                       error
	heartbeats                        int
	heartbeatErr                      error
	acknowledgements                  int
	cleanerCallsAtAck                 int
	cleaner                           *closureCleaner
	failures                          int
	failureRetryable                  bool
	retryDelay                        time.Duration
}

func (durable *closureStopStore) ClaimJobKind(_ context.Context, queue, kind, owner string, _ time.Duration) (*store.JobLease, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.claimQueue, durable.claimKind, durable.claimOwner = queue, kind, owner
	lease := durable.lease
	durable.lease = nil
	return lease, nil
}

func (durable *closureStopStore) HeartbeatJob(context.Context, store.JobLease, time.Duration) error {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.heartbeats++
	return durable.heartbeatErr
}

func (durable *closureStopStore) GetClosureTurnRuntimeIdentity(context.Context, store.JobLease) (store.AgentTurnRuntimeIdentity, error) {
	return durable.identity, durable.identityErr
}

func (durable *closureStopStore) AcknowledgeClosureTurnStopped(context.Context, store.JobLease) (store.ClosureSettlement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.acknowledgements++
	if durable.cleaner != nil {
		durable.cleanerCallsAtAck = durable.cleaner.callCount()
	}
	return store.ClosureSettlement{}, nil
}

func (durable *closureStopStore) AcknowledgeClosureActionFailure(_ context.Context, _ store.JobLease, _ error, retryable bool, delay time.Duration) (store.WorkflowActionFailureAcknowledgement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.failures++
	durable.failureRetryable, durable.retryDelay = retryable, delay
	return store.WorkflowActionFailureAcknowledgement{RetryScheduled: retryable}, nil
}

type closureSettlementStore struct {
	mutex sync.Mutex
	lease *store.JobLease

	claimQueue          string
	claimKind           string
	claimOwner          string
	contextCalls        int
	contextErr          error
	mutations           []store.MutationReservation
	listCalls           int
	reconciledIDs       []string
	outcomes            []store.RecoveredMutationOutcome
	completions         int
	completeErr         error
	waits               int
	waitDelay           time.Duration
	waitAcknowledgement store.WorkflowActionFailureAcknowledgement
	failures            int
	failureRetryable    bool
	operations          []string
}

func (durable *closureSettlementStore) ClaimJobKind(_ context.Context, queue, kind, owner string, _ time.Duration) (*store.JobLease, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.claimQueue, durable.claimKind, durable.claimOwner = queue, kind, owner
	lease := durable.lease
	durable.lease = nil
	return lease, nil
}

func (*closureSettlementStore) HeartbeatJob(context.Context, store.JobLease, time.Duration) error {
	return nil
}

func (durable *closureSettlementStore) GetClosureMutationReconciliationContext(context.Context, store.JobLease) (store.AgentTurnMutationReconciliationContext, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.contextCalls++
	return store.AgentTurnMutationReconciliationContext{}, durable.contextErr
}

func (durable *closureSettlementStore) ListClosureMutationsForReconciliation(context.Context, store.JobLease) ([]store.MutationReservation, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.listCalls++
	return append([]store.MutationReservation(nil), durable.mutations...), nil
}

func (durable *closureSettlementStore) ReconcileClosureMutation(_ context.Context, _ store.JobLease, id string, outcome store.RecoveredMutationOutcome) (store.MutationReservation, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.reconciledIDs = append(durable.reconciledIDs, id)
	durable.outcomes = append(durable.outcomes, outcome)
	durable.operations = append(durable.operations, "reconcile:"+id)
	return store.MutationReservation{ID: id, State: outcome.State}, nil
}

func (durable *closureSettlementStore) CompleteClosureSettlement(context.Context, store.JobLease) (store.ClosureSettlement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.completions++
	durable.operations = append(durable.operations, "complete")
	return store.ClosureSettlement{}, durable.completeErr
}

func (durable *closureSettlementStore) AcknowledgeClosureActionFailure(_ context.Context, _ store.JobLease, _ error, retryable bool, _ time.Duration) (store.WorkflowActionFailureAcknowledgement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.failures++
	durable.failureRetryable = retryable
	return store.WorkflowActionFailureAcknowledgement{RetryScheduled: retryable}, nil
}

func (durable *closureSettlementStore) AcknowledgeClosureSettlementWait(_ context.Context, _ store.JobLease, delay time.Duration) (store.WorkflowActionFailureAcknowledgement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.waits++
	durable.waitDelay = delay
	return durable.waitAcknowledgement, nil
}

func (durable *closureSettlementStore) completionCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.completions
}

type closureReconciler struct {
	mutex       sync.Mutex
	results     []mcp.MutationReconciliationResult
	mutationIDs []string
	calls       int
}

func (reconciler *closureReconciler) Reconcile(_ context.Context, _ store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (mcp.MutationReconciliationResult, error) {
	reconciler.mutex.Lock()
	defer reconciler.mutex.Unlock()
	reconciler.calls++
	reconciler.mutationIDs = append(reconciler.mutationIDs, mutation.ID)
	if len(reconciler.results) == 0 {
		return mcp.MutationReconciliationResult{}, errors.New("unexpected reconciliation")
	}
	result := reconciler.results[0]
	reconciler.results = reconciler.results[1:]
	return result, nil
}
