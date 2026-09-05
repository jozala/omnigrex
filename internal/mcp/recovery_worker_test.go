package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
)

func TestRecoveryWorkerClaimsOnlyReconciliationJobsAndRecordsMutationsInOrder(t *testing.T) {
	lease := reconciliationJobLease()
	first := reconciliationMutation(mcp.ToolReportBlocked, `{"operation_id":"first","reason":"blocked"}`)
	first.ID = "10000000-0000-4000-8000-000000000011"
	first.InvocationNumber = 1
	second := reconciliationMutation(mcp.ToolCommentOnIssue, `{"operation_id":"second","body":"update"}`)
	second.ID = "10000000-0000-4000-8000-000000000012"
	second.InvocationNumber = 2
	durable := &recoveryStore{lease: &lease, reconciliation: reconciliationContext(), mutations: []store.MutationReservation{first, second}}
	reconciler := &recordingMutationReconciler{results: []mcp.MutationReconciliationResult{
		{Disposition: mcp.ReconciliationFound, Outcome: store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: json.RawMessage(`{"outcome":"BLOCKED"}`)}},
		{Disposition: mcp.ReconciliationDefinitelyFailed, Outcome: store.RecoveredMutationOutcome{State: store.MutationFailed, LastError: "comment artifact was not found"}},
	}}
	worker := newRecoveryWorker(t, durable, reconciler, mcp.RecoveryWorkerConfig{
		ClaimOwner: "recovery-worker", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: 10 * time.Millisecond, RetryDelay: time.Minute,
	})

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.claimQueue != store.AgentTurnRecoveryQueue || durable.claimKind != store.ReconcileAgentTurnMutationsJobKind ||
		durable.claimOwner != "recovery-worker" || durable.claimDuration != time.Second {
		t.Fatalf("ClaimJobKind call = (%q, %q, %q, %s)", durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimDuration)
	}
	if !reflect.DeepEqual(reconciler.mutationIDs, []string{first.ID, second.ID}) || !reflect.DeepEqual(durable.reconciledIDs, []string{first.ID, second.ID}) {
		t.Fatalf("reconciliation order = reads %v, writes %v", reconciler.mutationIDs, durable.reconciledIDs)
	}
	if len(durable.outcomes) != 2 || durable.outcomes[0].State != store.MutationSucceeded || durable.outcomes[1].State != store.MutationFailed {
		t.Fatalf("recorded outcomes = %#v", durable.outcomes)
	}
	if durable.completedTurnID != "turn-1" || durable.completedEpoch != 4 || durable.acknowledgements != 0 {
		t.Fatalf("completion = (%q, %d), acknowledgements = %d", durable.completedTurnID, durable.completedEpoch, durable.acknowledgements)
	}
}

func TestRecoveryWorkerAcknowledgesUnresolvedAndDependencyFailuresForStoreBudgeting(t *testing.T) {
	tests := []struct {
		name         string
		escalated    bool
		result       mcp.MutationReconciliationResult
		reconcileErr error
	}{
		{name: "unresolved", result: mcp.MutationReconciliationResult{Disposition: mcp.ReconciliationUnresolved}},
		{name: "dependency on final attempt", escalated: true, reconcileErr: mcp.ErrMutationReconciliationDependency},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := reconciliationJobLease()
			if !test.escalated {
				lease.Attempt = 1
				lease.AttemptCount = 1
			}
			mutation := reconciliationMutation(mcp.ToolCommentOnIssue, `{"operation_id":"caller","body":"update"}`)
			durable := &recoveryStore{
				lease: &lease, reconciliation: reconciliationContext(), mutations: []store.MutationReservation{mutation},
				acknowledgement: store.AgentTurnMutationReconciliationAcknowledgement{Attempt: lease.Attempt, RetryScheduled: !test.escalated, Escalated: test.escalated},
			}
			reconciler := &recordingMutationReconciler{results: []mcp.MutationReconciliationResult{test.result}, err: test.reconcileErr}
			worker := newRecoveryWorker(t, durable, reconciler, mcp.RecoveryWorkerConfig{
				ClaimOwner: "recovery-worker", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
				IdlePollInterval: 10 * time.Millisecond, RetryDelay: 45 * time.Second,
			})

			processed, err := worker.ProcessNext(context.Background())
			if !processed || err == nil || strings.Contains(err.Error(), "credential-secret") {
				t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
			}
			wantCompletedTurn := ""
			if test.escalated {
				wantCompletedTurn = "turn-1"
			}
			if durable.acknowledgements != 1 || durable.retryDelay != 45*time.Second || durable.completedTurnID != wantCompletedTurn || len(durable.reconciledIDs) != 0 {
				t.Fatalf("failure handling = acknowledgements %d, delay %s, completion %q, writes %v", durable.acknowledgements, durable.retryDelay, durable.completedTurnID, durable.reconciledIDs)
			}
		})
	}
}

func TestRecoveryWorkerHeartbeatsWhileReconciliationIsActive(t *testing.T) {
	lease := reconciliationJobLease()
	mutation := reconciliationMutation(mcp.ToolCommentOnIssue, `{"operation_id":"caller","body":"update"}`)
	durable := &recoveryStore{lease: &lease, reconciliation: reconciliationContext(), mutations: []store.MutationReservation{mutation}}
	reconciler := &recordingMutationReconciler{
		results: []mcp.MutationReconciliationResult{{Disposition: mcp.ReconciliationUnresolved}},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	worker := newRecoveryWorker(t, durable, reconciler, mcp.RecoveryWorkerConfig{
		ClaimOwner: "recovery-worker", LeaseDuration: 100 * time.Millisecond, HeartbeatInterval: 5 * time.Millisecond,
		IdlePollInterval: 10 * time.Millisecond, RetryDelay: time.Second,
	})
	done := make(chan error, 1)
	go func() {
		_, err := worker.ProcessNext(context.Background())
		done <- err
	}()
	<-reconciler.started
	deadline := time.After(time.Second)
	for durable.heartbeatCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("reconciliation lease was not heartbeated")
		case <-time.After(time.Millisecond):
		}
	}
	close(reconciler.release)
	if err := <-done; err == nil {
		t.Fatal("ProcessNext() error = nil, want unresolved result")
	}
	if durable.heartbeatExtension != 100*time.Millisecond {
		t.Fatalf("HeartbeatJob extension = %s", durable.heartbeatExtension)
	}
}

func TestRecoveryWorkerDoesNotAcknowledgeAfterRecoveryFenceLoss(t *testing.T) {
	lease := reconciliationJobLease()
	durable := &recoveryStore{lease: &lease, contextErr: store.ErrAgentTurnRecoveryFenceLost}
	worker := newRecoveryWorker(t, durable, &recordingMutationReconciler{}, mcp.RecoveryWorkerConfig{
		ClaimOwner: "recovery-worker", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: 10 * time.Millisecond, RetryDelay: time.Second,
	})

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.acknowledgements != 0 {
		t.Fatalf("stale worker acknowledgements = %d, want zero", durable.acknowledgements)
	}
}

func TestRecoveryWorkerWaitsAndHeartbeatsWithoutReconcilingBeforeRuntimeStop(t *testing.T) {
	lease := reconciliationJobLease()
	mutation := reconciliationMutation(mcp.ToolReportBlocked, `{"operation_id":"blocked","reason":"blocked"}`)
	durable := &recoveryStore{
		lease: &lease, reconciliation: reconciliationContext(), mutations: []store.MutationReservation{mutation},
		waitForRuntimeStop: true,
	}
	reconciler := &recordingMutationReconciler{results: []mcp.MutationReconciliationResult{{
		Disposition: mcp.ReconciliationFound,
		Outcome:     store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: json.RawMessage(`{"outcome":"BLOCKED"}`)},
	}}, started: make(chan struct{}), release: make(chan struct{})}
	worker := newRecoveryWorker(t, durable, reconciler, mcp.RecoveryWorkerConfig{
		ClaimOwner: "recovery-worker", LeaseDuration: 100 * time.Millisecond, HeartbeatInterval: 5 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct {
		processed bool
		err       error
	}, 1)
	go func() {
		processed, err := worker.ProcessNext(ctx)
		done <- struct {
			processed bool
			err       error
		}{processed: processed, err: err}
	}()
	deadline := time.After(time.Second)
	for durable.heartbeatCount() == 0 || durable.contextCallCount() < 2 {
		select {
		case <-deadline:
			t.Fatal("reconciliation lease was not heartbeated while waiting for runtime stop")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-reconciler.started:
		t.Fatal("external reconciler was called before runtime stop acknowledgement")
	default:
	}
	durable.acknowledgeRuntimeStop()
	select {
	case <-reconciler.started:
	case <-time.After(time.Second):
		t.Fatal("external reconciler was not called after runtime stop acknowledgement")
	}
	close(reconciler.release)
	result := <-done
	processed, err := result.processed, result.err
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if durable.acknowledgements != 0 {
		t.Fatalf("failure acknowledgements = %d, want zero", durable.acknowledgements)
	}
}

func TestNewRecoveryWorkerValidatesLeaseTiming(t *testing.T) {
	valid := mcp.RecoveryWorkerConfig{
		ClaimOwner: "worker", LeaseDuration: time.Second, HeartbeatInterval: time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	}
	tests := []struct {
		name   string
		mutate func(*mcp.RecoveryWorkerConfig)
	}{
		{name: "empty owner", mutate: func(config *mcp.RecoveryWorkerConfig) { config.ClaimOwner = "" }},
		{name: "zero lease", mutate: func(config *mcp.RecoveryWorkerConfig) { config.LeaseDuration = 0 }},
		{name: "zero heartbeat", mutate: func(config *mcp.RecoveryWorkerConfig) { config.HeartbeatInterval = 0 }},
		{name: "heartbeat equals lease", mutate: func(config *mcp.RecoveryWorkerConfig) { config.HeartbeatInterval = config.LeaseDuration }},
		{name: "zero polling", mutate: func(config *mcp.RecoveryWorkerConfig) { config.IdlePollInterval = 0 }},
		{name: "zero retry", mutate: func(config *mcp.RecoveryWorkerConfig) { config.RetryDelay = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := mcp.NewRecoveryWorker(&recoveryStore{}, &recordingMutationReconciler{}, config); !errors.Is(err, mcp.ErrInvalidRecoveryWorkerConfiguration) {
				t.Fatalf("NewRecoveryWorker() error = %v", err)
			}
		})
	}
}

func newRecoveryWorker(t *testing.T, durable mcp.RecoveryStore, reconciler mcp.MutationReconciler, config mcp.RecoveryWorkerConfig) *mcp.RecoveryWorker {
	t.Helper()
	worker, err := mcp.NewRecoveryWorker(durable, reconciler, config)
	if err != nil {
		t.Fatalf("NewRecoveryWorker() error = %v", err)
	}
	return worker
}

func reconciliationJobLease() store.JobLease {
	return store.JobLease{Job: store.Job{
		JobSpec: store.JobSpec{
			Queue: store.AgentTurnRecoveryQueue, Kind: store.ReconcileAgentTurnMutationsJobKind,
			WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", AgentTurnID: "turn-1", ExecutionEpoch: 4, MaxAttempts: 3,
		},
		ID: "20000000-0000-4000-8000-000000000001", Status: store.JobLeased, AttemptCount: 3,
		LeaseOwner: "recovery-worker", LeaseToken: "30000000-0000-4000-8000-000000000001",
	}, Attempt: 3}
}

type recoveryStore struct {
	mutex sync.Mutex
	lease *store.JobLease

	claimQueue, claimKind, claimOwner string
	claimDuration                     time.Duration
	reconciliation                    store.AgentTurnMutationReconciliationContext
	mutations                         []store.MutationReservation
	contextErr, listErr               error
	contextErrors                     []error
	contextCalls                      int
	waitForRuntimeStop                bool
	runtimeStopped                    bool
	reconciledIDs                     []string
	outcomes                          []store.RecoveredMutationOutcome
	reconcileErr                      error
	completedTurnID                   string
	completedEpoch                    int64
	completeErr                       error
	heartbeats                        int
	heartbeatExtension                time.Duration
	heartbeatErr                      error
	acknowledgements                  int
	retryDelay                        time.Duration
	acknowledgement                   store.AgentTurnMutationReconciliationAcknowledgement
	acknowledgeErr                    error
}

func (durable *recoveryStore) ClaimJobKind(_ context.Context, queue, kind, owner string, lease time.Duration) (*store.JobLease, error) {
	durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimDuration = queue, kind, owner, lease
	return durable.lease, nil
}

func (durable *recoveryStore) HeartbeatJob(_ context.Context, _ store.JobLease, extension time.Duration) error {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.heartbeats++
	durable.heartbeatExtension = extension
	return durable.heartbeatErr
}

func (durable *recoveryStore) heartbeatCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.heartbeats
}

func (durable *recoveryStore) contextCallCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.contextCalls
}

func (durable *recoveryStore) acknowledgeRuntimeStop() {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.runtimeStopped = true
}

func (durable *recoveryStore) GetAgentTurnMutationReconciliationContext(context.Context, store.JobLease) (store.AgentTurnMutationReconciliationContext, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.contextCalls++
	if durable.waitForRuntimeStop && !durable.runtimeStopped {
		return store.AgentTurnMutationReconciliationContext{}, store.ErrAgentTurnRecoveryUnsettled
	}
	if len(durable.contextErrors) != 0 {
		err := durable.contextErrors[0]
		durable.contextErrors = durable.contextErrors[1:]
		return durable.reconciliation, err
	}
	return durable.reconciliation, durable.contextErr
}

func (durable *recoveryStore) ListAgentTurnMutationsForReconciliation(context.Context, store.JobLease) ([]store.MutationReservation, error) {
	return append([]store.MutationReservation(nil), durable.mutations...), durable.listErr
}

func (durable *recoveryStore) ReconcileRecoveredMutation(_ context.Context, _ store.JobLease, mutationID string, outcome store.RecoveredMutationOutcome) (store.MutationReservation, error) {
	durable.reconciledIDs = append(durable.reconciledIDs, mutationID)
	durable.outcomes = append(durable.outcomes, outcome)
	return store.MutationReservation{ID: mutationID, State: outcome.State, Result: outcome.Result, LastError: outcome.LastError}, durable.reconcileErr
}

func (durable *recoveryStore) AcknowledgeAgentTurnMutationReconciliationFailure(_ context.Context, _ store.JobLease, _ error, delay time.Duration) (store.AgentTurnMutationReconciliationAcknowledgement, error) {
	durable.acknowledgements++
	durable.retryDelay = delay
	return durable.acknowledgement, durable.acknowledgeErr
}

func (durable *recoveryStore) CompleteAgentTurnRecovery(_ context.Context, turnID string, epoch int64) (store.AgentTurnRecovery, error) {
	durable.completedTurnID, durable.completedEpoch = turnID, epoch
	return store.AgentTurnRecovery{TurnID: turnID, ExecutionEpoch: epoch}, durable.completeErr
}

type recordingMutationReconciler struct {
	mutationIDs []string
	results     []mcp.MutationReconciliationResult
	err         error
	started     chan struct{}
	release     chan struct{}
}

func (reconciler *recordingMutationReconciler) Reconcile(_ context.Context, _ store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (mcp.MutationReconciliationResult, error) {
	reconciler.mutationIDs = append(reconciler.mutationIDs, mutation.ID)
	if reconciler.started != nil {
		close(reconciler.started)
		<-reconciler.release
	}
	if reconciler.err != nil {
		return mcp.MutationReconciliationResult{}, reconciler.err
	}
	if len(reconciler.results) == 0 {
		return mcp.MutationReconciliationResult{}, errors.New("unexpected reconciliation")
	}
	result := reconciler.results[0]
	reconciler.results = reconciler.results[1:]
	return result, nil
}
