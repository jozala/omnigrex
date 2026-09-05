package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestReconciliationWorkerReturnsNoWork(t *testing.T) {
	durable := &reconciliationWorkerStore{}
	worker := newReconciliationWorker(t, durable, newTestProcessor(t, &processorInbox{}), time.Second, 10*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || processed {
		t.Fatalf("ProcessNext() = (%t, %v), want no work", processed, err)
	}
	if durable.claimQueue != store.WorkflowActionQueue || durable.claimKind != store.ReconcilePendingEventsJobKind ||
		durable.claimOwner != "pending-event-worker" || durable.claimLease != time.Second {
		t.Fatalf("ClaimJobKind() = (%q, %q, %q, %s)", durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimLease)
	}
	if durable.acknowledgementCount() != 0 || durable.heartbeatCount() != 0 || len(durable.failureSnapshot()) != 0 {
		t.Fatal("no-work claim performed processing operations")
	}
}

func TestReconciliationWorkerAcknowledgesWithProcessorFactoryAndHeartbeats(t *testing.T) {
	lease := pendingEventReconciliationLease(json.RawMessage(`{"workflow_id":"workflow"}`))
	createdAt := time.Date(2026, time.September, 2, 12, 30, 0, 0, time.UTC)
	record := store.NormalizedEventRecord{
		DeliveryID: "223e4567-e89b-12d3-a456-426614174000",
		Payload:    json.RawMessage(`{"delivery_id":"223e4567-e89b-12d3-a456-426614174000","event":"issues","action":"closed","repository":{"id":9123,"owner":"jozala","name":"omnigrex"},"issue":{"id":456,"number":12}}`),
		CreatedAt:  createdAt,
	}
	durable := &reconciliationWorkerStore{lease: &lease, record: record, snapshot: dormantSnapshot(), waitForHeartbeat: true}
	processor := newTestProcessor(t, &processorInbox{})
	worker := newReconciliationWorker(t, durable, processor, 100*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want successful acknowledgement", processed, err)
	}
	if durable.acknowledgementCount() != 1 || durable.acknowledgedLease().ID != lease.ID {
		t.Fatalf("AcknowledgePendingEventReconciliation() calls = %d, lease %#v", durable.acknowledgementCount(), durable.acknowledgedLease())
	}
	if durable.heartbeatCount() == 0 || durable.heartbeatExtension() != 100*time.Millisecond {
		t.Fatalf("HeartbeatJob() calls = %d, extension %s", durable.heartbeatCount(), durable.heartbeatExtension())
	}
	decision := durable.factoryDecision()
	wantRetainUntil := createdAt.Add(30 * 24 * time.Hour)
	if decision.Disposition != workflow.DispositionApplied || decision.Snapshot.Closure == nil ||
		!decision.Snapshot.Closure.RetainUntil.Equal(wantRetainUntil) {
		t.Fatalf("factory decision = %#v, want Processor retention through %s", decision, wantRetainUntil)
	}
	if len(durable.failureSnapshot()) != 0 {
		t.Fatalf("successful reconciliation failures = %#v", durable.failureSnapshot())
	}
}

func TestReconciliationWorkerFactoryReproducesProcessorPendingTransition(t *testing.T) {
	createdAt := time.Date(2026, time.September, 2, 13, 0, 0, 0, time.UTC)
	record := store.NormalizedEventRecord{
		DeliveryID: "323e4567-e89b-12d3-a456-426614174000",
		Payload:    json.RawMessage(`{"delivery_id":"323e4567-e89b-12d3-a456-426614174000","event":"pull_request","action":"synchronize","repository":{"id":9123,"owner":"jozala","name":"omnigrex"},"pull_request":{"id":654,"number":21,"head_sha":"new-head","before_sha":"old-head"}}`),
		CreatedAt:  createdAt,
	}
	snapshot := reviewingSnapshot()
	inbox := &processorInbox{pending: []store.NormalizedEventRecord{record}, drainSnapshot: snapshot}
	processor := newTestProcessor(t, inbox)
	if processed, err := processor.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("Processor.ProcessNext() = (%t, %v)", processed, err)
	}

	lease := pendingEventReconciliationLease(json.RawMessage(`{}`))
	durable := &reconciliationWorkerStore{lease: &lease, record: record, snapshot: snapshot}
	worker := newReconciliationWorker(t, durable, processor, time.Second, 10*time.Millisecond)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("ReconciliationWorker.ProcessNext() = (%t, %v)", processed, err)
	}
	if len(inbox.drained) != 1 {
		t.Fatalf("Processor transitions = %d, want one", len(inbox.drained))
	}
	if !reflect.DeepEqual(durable.factoryLocator(), inbox.drained[0].locator) ||
		!reflect.DeepEqual(durable.factoryDecision(), inbox.drained[0].decision) {
		t.Fatalf("worker factory = (%#v, %#v), Processor = (%#v, %#v)",
			durable.factoryLocator(), durable.factoryDecision(), inbox.drained[0].locator, inbox.drained[0].decision)
	}
}

func TestReconciliationWorkerDoesNotFailAfterHeartbeatLeaseLoss(t *testing.T) {
	lease := pendingEventReconciliationLease(json.RawMessage(`{}`))
	durable := &reconciliationWorkerStore{
		lease: &lease, heartbeatErr: store.ErrJobLeaseLost,
		acknowledge: func(ctx context.Context, _ store.JobLease, _ store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
			<-ctx.Done()
			return store.PendingEventReconciliation{}, ctx.Err()
		},
	}
	worker := newReconciliationWorker(t, durable, newTestProcessor(t, &processorInbox{}), 100*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrJobLeaseLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want heartbeat lease loss", processed, err)
	}
	if durable.acknowledgementCount() != 1 || len(durable.failureSnapshot()) != 0 {
		t.Fatalf("heartbeat-loss acknowledgements/failures = (%d, %d), want (1, 0)", durable.acknowledgementCount(), len(durable.failureSnapshot()))
	}
}

func TestReconciliationWorkerClassifiesFailures(t *testing.T) {
	transientErr := errors.New("database temporarily unavailable")
	tests := []struct {
		name          string
		payload       json.RawMessage
		acknowledge   reconciliationAcknowledger
		wantErrorIs   error
		wantFailures  int
		wantRetryable bool
	}{
		{
			name: "transient acknowledgement", payload: json.RawMessage(`{}`), wantErrorIs: transientErr,
			acknowledge: func(context.Context, store.JobLease, store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
				return store.PendingEventReconciliation{}, transientErr
			},
			wantFailures: 1, wantRetryable: true,
		},
		{
			name: "synchronization causal gap", payload: json.RawMessage(`{}`), wantErrorIs: store.ErrPendingEventCausalGap,
			acknowledge: func(context.Context, store.JobLease, store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
				return store.PendingEventReconciliation{}, store.ErrPendingEventCausalGap
			},
			wantFailures: 1, wantRetryable: true,
		},
		{
			name: "malformed job payload", payload: json.RawMessage(`{}`), wantErrorIs: store.ErrPendingEventReconciliationPayloadInvalid,
			acknowledge: func(context.Context, store.JobLease, store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
				return store.PendingEventReconciliation{}, store.ErrPendingEventReconciliationPayloadInvalid
			},
			wantFailures: 1, wantRetryable: false,
		},
		{
			name: "malformed normalized event", payload: json.RawMessage(`{}`),
			acknowledge: func(_ context.Context, _ store.JobLease, factory store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
				_, _, err := factory(store.NormalizedEventRecord{DeliveryID: "bad-event", Payload: json.RawMessage(`{`), CreatedAt: time.Now()})
				return store.PendingEventReconciliation{}, err
			},
			wantFailures: 1, wantRetryable: false,
		},
		{
			name: "successor conflict", payload: json.RawMessage(`{}`), wantErrorIs: store.ErrWorkflowSuccessorConflict,
			acknowledge: func(context.Context, store.JobLease, store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
				return store.PendingEventReconciliation{}, store.ErrWorkflowSuccessorConflict
			},
			wantFailures: 1, wantRetryable: false,
		},
		{
			name: "stale reconciliation fence", payload: json.RawMessage(`{}`), wantErrorIs: store.ErrPendingEventReconciliationFenceLost,
			acknowledge: func(context.Context, store.JobLease, store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
				return store.PendingEventReconciliation{}, store.ErrPendingEventReconciliationFenceLost
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := pendingEventReconciliationLease(test.payload)
			lease.Attempt = lease.MaxAttempts
			lease.AttemptCount = lease.MaxAttempts
			durable := &reconciliationWorkerStore{lease: &lease, acknowledge: test.acknowledge}
			worker := newReconciliationWorker(t, durable, newTestProcessor(t, &processorInbox{}), time.Second, 10*time.Millisecond)

			processed, err := worker.ProcessNext(context.Background())
			if !processed || err == nil {
				t.Fatalf("ProcessNext() = (%t, %v), want classified failure", processed, err)
			}
			if test.wantErrorIs != nil && !errors.Is(err, test.wantErrorIs) {
				t.Fatalf("ProcessNext() error = %v, want %v", err, test.wantErrorIs)
			}
			failures := durable.failureSnapshot()
			if len(failures) != test.wantFailures {
				t.Fatalf("FailJob() calls = %#v, want %d", failures, test.wantFailures)
			}
			if test.wantFailures == 1 {
				if failures[0].retryable != test.wantRetryable || failures[0].retryDelay != 25*time.Millisecond {
					t.Fatalf("FailJob() = %#v, want retryable=%t delay=25ms", failures[0], test.wantRetryable)
				}
				if failures[0].retryScheduled {
					t.Fatal("final durable attempt scheduled another retry")
				}
			}
		})
	}
}

func TestReconciliationWorkerRunPollsReportsErrorsAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	operationErr := errors.New("database unavailable")
	durable := &reconciliationWorkerStore{}
	durable.claim = func() (*store.JobLease, error) {
		calls := durable.claims.Load()
		if calls < 3 {
			return nil, operationErr
		}
		cancel()
		return nil, nil
	}
	var reported atomic.Int32
	processor := newTestProcessor(t, &processorInbox{})
	worker, err := webhook.NewReconciliationWorker(durable, processor, webhook.ReconciliationWorkerConfig{
		ClaimOwner: "pending-event-worker", LeaseDuration: time.Second, HeartbeatInterval: 10 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: 25 * time.Millisecond,
		OnError: func(err error) {
			if !errors.Is(err, operationErr) {
				t.Errorf("OnError() = %v, want database unavailable", err)
			}
			reported.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("NewReconciliationWorker() error = %v", err)
	}

	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if durable.claims.Load() != 3 || reported.Load() != 2 {
		t.Fatalf("claim/error reports = (%d, %d), want (3, 2)", durable.claims.Load(), reported.Load())
	}
}

func TestNewReconciliationWorkerValidatesConfiguration(t *testing.T) {
	valid := webhook.ReconciliationWorkerConfig{
		ClaimOwner: "worker", LeaseDuration: time.Second, HeartbeatInterval: time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	}
	processor := newTestProcessor(t, &processorInbox{})
	tests := []struct {
		name   string
		store  webhook.ReconciliationWorkerStore
		worker *webhook.Processor
		mutate func(*webhook.ReconciliationWorkerConfig)
	}{
		{name: "nil store", worker: processor},
		{name: "nil processor", store: &reconciliationWorkerStore{}},
		{name: "empty owner", store: &reconciliationWorkerStore{}, worker: processor, mutate: func(config *webhook.ReconciliationWorkerConfig) { config.ClaimOwner = " " }},
		{name: "zero lease", store: &reconciliationWorkerStore{}, worker: processor, mutate: func(config *webhook.ReconciliationWorkerConfig) { config.LeaseDuration = 0 }},
		{name: "zero heartbeat", store: &reconciliationWorkerStore{}, worker: processor, mutate: func(config *webhook.ReconciliationWorkerConfig) { config.HeartbeatInterval = 0 }},
		{name: "heartbeat equals lease", store: &reconciliationWorkerStore{}, worker: processor, mutate: func(config *webhook.ReconciliationWorkerConfig) { config.HeartbeatInterval = config.LeaseDuration }},
		{name: "zero poll", store: &reconciliationWorkerStore{}, worker: processor, mutate: func(config *webhook.ReconciliationWorkerConfig) { config.IdlePollInterval = 0 }},
		{name: "zero retry", store: &reconciliationWorkerStore{}, worker: processor, mutate: func(config *webhook.ReconciliationWorkerConfig) { config.RetryDelay = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			if test.mutate != nil {
				test.mutate(&config)
			}
			if _, err := webhook.NewReconciliationWorker(test.store, test.worker, config); err == nil {
				t.Fatal("NewReconciliationWorker() error = nil, want invalid configuration")
			}
		})
	}
}

func newReconciliationWorker(t *testing.T, durable webhook.ReconciliationWorkerStore, processor *webhook.Processor, lease, heartbeat time.Duration) *webhook.ReconciliationWorker {
	t.Helper()
	worker, err := webhook.NewReconciliationWorker(durable, processor, webhook.ReconciliationWorkerConfig{
		ClaimOwner: "pending-event-worker", LeaseDuration: lease, HeartbeatInterval: heartbeat,
		IdlePollInterval: time.Millisecond, RetryDelay: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewReconciliationWorker() error = %v", err)
	}
	return worker
}

func pendingEventReconciliationLease(payload json.RawMessage) store.JobLease {
	return store.JobLease{Job: store.Job{
		JobSpec: store.JobSpec{
			Queue: store.WorkflowActionQueue, Kind: store.ReconcilePendingEventsJobKind,
			Payload: payload, MaxAttempts: 3, WorkflowID: "workflow", WorkflowAttemptID: "attempt",
			AgentAssignmentID: "assignment", AgentSessionID: "session", AgentTurnID: "turn", ExecutionEpoch: 2,
		},
		ID: "10000000-0000-4000-8000-000000000001", Status: store.JobLeased, AttemptCount: 1,
		LeaseOwner: "pending-event-worker", LeaseToken: "10000000-0000-4000-8000-000000000002",
	}, Attempt: 1}
}

type reconciliationAcknowledger func(context.Context, store.JobLease, store.PendingTransitionFactory) (store.PendingEventReconciliation, error)

type reconciliationFailure struct {
	cause          error
	retryable      bool
	retryDelay     time.Duration
	retryScheduled bool
}

type reconciliationWorkerStore struct {
	mutex sync.Mutex
	lease *store.JobLease
	claim func() (*store.JobLease, error)

	claims                            atomic.Int32
	claimQueue, claimKind, claimOwner string
	claimLease                        time.Duration
	heartbeats                        int
	heartbeatDuration                 time.Duration
	heartbeatErr                      error
	heartbeatObserved                 chan struct{}
	heartbeatOnce                     sync.Once
	waitForHeartbeat                  bool
	acknowledge                       reconciliationAcknowledger
	acknowledgements                  int
	acknowledged                      store.JobLease
	record                            store.NormalizedEventRecord
	snapshot                          workflow.Snapshot
	locator                           store.WorkflowLocator
	decision                          workflow.Decision
	failures                          []reconciliationFailure
}

func (durable *reconciliationWorkerStore) ClaimJobKind(_ context.Context, queue, kind, owner string, lease time.Duration) (*store.JobLease, error) {
	durable.claims.Add(1)
	durable.mutex.Lock()
	durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimLease = queue, kind, owner, lease
	claim := durable.claim
	result := durable.lease
	durable.lease = nil
	durable.mutex.Unlock()
	if claim != nil {
		return claim()
	}
	return result, nil
}

func (durable *reconciliationWorkerStore) HeartbeatJob(_ context.Context, _ store.JobLease, extension time.Duration) error {
	durable.mutex.Lock()
	durable.heartbeats++
	durable.heartbeatDuration = extension
	err := durable.heartbeatErr
	observed := durable.heartbeatObserved
	durable.mutex.Unlock()
	if observed != nil {
		durable.heartbeatOnce.Do(func() { close(observed) })
	}
	return err
}

func (durable *reconciliationWorkerStore) AcknowledgePendingEventReconciliation(ctx context.Context, lease store.JobLease, factory store.PendingTransitionFactory) (store.PendingEventReconciliation, error) {
	durable.mutex.Lock()
	durable.acknowledgements++
	durable.acknowledged = lease
	acknowledge := durable.acknowledge
	if durable.waitForHeartbeat && durable.heartbeatObserved == nil {
		durable.heartbeatObserved = make(chan struct{})
	}
	observed := durable.heartbeatObserved
	record, snapshot := durable.record, durable.snapshot
	durable.mutex.Unlock()
	if acknowledge != nil {
		return acknowledge(ctx, lease, factory)
	}
	if observed != nil {
		select {
		case <-observed:
		case <-ctx.Done():
			return store.PendingEventReconciliation{}, ctx.Err()
		}
	}
	locator, transition, err := factory(record)
	if err != nil {
		return store.PendingEventReconciliation{}, err
	}
	decision := transition(snapshot)
	durable.mutex.Lock()
	durable.locator, durable.decision = locator, decision
	durable.mutex.Unlock()
	return store.PendingEventReconciliation{JobID: lease.ID, CompletedCount: 1}, nil
}

func (durable *reconciliationWorkerStore) AcknowledgePendingEventReconciliationFailure(_ context.Context, lease store.JobLease, cause error, retryable bool, retryDelay time.Duration) (store.WorkflowActionFailureAcknowledgement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.failures = append(durable.failures, reconciliationFailure{
		cause: cause, retryable: retryable, retryDelay: retryDelay,
		retryScheduled: retryable && lease.Attempt < lease.MaxAttempts,
	})
	return store.WorkflowActionFailureAcknowledgement{RetryScheduled: retryable && lease.Attempt < lease.MaxAttempts}, nil
}

func (durable *reconciliationWorkerStore) acknowledgementCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.acknowledgements
}

func (durable *reconciliationWorkerStore) acknowledgedLease() store.JobLease {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.acknowledged
}

func (durable *reconciliationWorkerStore) heartbeatCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.heartbeats
}

func (durable *reconciliationWorkerStore) heartbeatExtension() time.Duration {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.heartbeatDuration
}

func (durable *reconciliationWorkerStore) factoryLocator() store.WorkflowLocator {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.locator
}

func (durable *reconciliationWorkerStore) factoryDecision() workflow.Decision {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.decision
}

func (durable *reconciliationWorkerStore) failureSnapshot() []reconciliationFailure {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return append([]reconciliationFailure(nil), durable.failures...)
}
