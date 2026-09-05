package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

const maximumReconciliationWorkerDuration = 365 * 24 * time.Hour

// ReconciliationWorkerStore is the durable job boundary used by ReconciliationWorker.
type ReconciliationWorkerStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	AcknowledgePendingEventReconciliation(context.Context, store.JobLease, store.PendingTransitionFactory) (store.PendingEventReconciliation, error)
	AcknowledgePendingEventReconciliationFailure(context.Context, store.JobLease, error, bool, time.Duration) (store.WorkflowActionFailureAcknowledgement, error)
}

// ReconciliationWorkerConfig controls pending-event claims, heartbeats, retries, and idle polling.
type ReconciliationWorkerConfig struct {
	ClaimOwner        string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdlePollInterval  time.Duration
	RetryDelay        time.Duration
	OnError           func(error)
}

// ReconciliationWorker makes RECONCILE_PENDING_EVENTS actions reachable from the Workflow queue.
type ReconciliationWorker struct {
	store             ReconciliationWorkerStore
	transitionFactory store.PendingTransitionFactory
	claimOwner        string
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	idlePollInterval  time.Duration
	retryDelay        time.Duration
	onError           func(error)
}

var _ ReconciliationWorkerStore = (*store.Store)(nil)

// NewReconciliationWorker binds reconciliation to the exact transition semantics configured on processor.
func NewReconciliationWorker(workerStore ReconciliationWorkerStore, processor *Processor, config ReconciliationWorkerConfig) (*ReconciliationWorker, error) {
	if workerStore == nil || processor == nil {
		return nil, errors.New("pending-event reconciliation Worker dependency is nil")
	}
	if strings.TrimSpace(config.ClaimOwner) == "" {
		return nil, errors.New("pending-event reconciliation Worker claim owner is empty")
	}
	if !validReconciliationWorkerDuration(config.LeaseDuration) ||
		!validReconciliationWorkerDuration(config.HeartbeatInterval) ||
		config.HeartbeatInterval >= config.LeaseDuration {
		return nil, errors.New("pending-event reconciliation Worker heartbeat must be positive and shorter than its lease")
	}
	if !validReconciliationWorkerDuration(config.IdlePollInterval) || !validReconciliationWorkerDuration(config.RetryDelay) {
		return nil, errors.New("pending-event reconciliation Worker polling and retry timing is invalid")
	}
	return &ReconciliationWorker{
		store: workerStore, transitionFactory: processor.pendingTransition,
		claimOwner: config.ClaimOwner, leaseDuration: config.LeaseDuration,
		heartbeatInterval: config.HeartbeatInterval, idlePollInterval: config.IdlePollInterval,
		retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// ProcessNext claims and durably handles at most one RECONCILE_PENDING_EVENTS action.
func (worker *ReconciliationWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim pending-event reconciliation: %w", err)
	}
	if lease == nil {
		return false, nil
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(workCtx)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := worker.heartbeat(heartbeatCtx, *lease)
		if heartbeatErr != nil {
			cancelWork()
		}
		heartbeatDone <- heartbeatErr
	}()

	_, operationErr := worker.store.AcknowledgePendingEventReconciliation(workCtx, *lease, worker.transitionFactory)
	if operationErr != nil {
		operationErr = fmt.Errorf("acknowledge pending-event reconciliation: %w", operationErr)
	}
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	if operationErr == nil {
		return true, nil
	}
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat pending-event reconciliation: %w", heartbeatErr), operationErr)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if lostReconciliationFence(operationErr) {
		return true, operationErr
	}
	return true, worker.fail(ctx, *lease, operationErr, !permanentReconciliationFailure(operationErr))
}

func (worker *ReconciliationWorker) fail(ctx context.Context, lease store.JobLease, cause error, retryable bool) error {
	if _, err := worker.store.AcknowledgePendingEventReconciliationFailure(ctx, lease, cause, retryable, worker.retryDelay); err != nil {
		return errors.Join(cause, fmt.Errorf("record pending-event reconciliation failure: %w", err))
	}
	return cause
}

func (worker *ReconciliationWorker) heartbeat(ctx context.Context, lease store.JobLease) error {
	ticker := time.NewTicker(worker.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := worker.store.HeartbeatJob(ctx, lease, worker.leaseDuration); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// Run processes pending-event reconciliation actions until its context ends.
func (worker *ReconciliationWorker) Run(ctx context.Context) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if worker.onError != nil {
				worker.onError(err)
			}
			if err := waitForPoll(ctx, worker.idlePollInterval); err != nil {
				return err
			}
			continue
		}
		if processed {
			continue
		}
		if err := waitForPoll(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
}

func validReconciliationWorkerDuration(duration time.Duration) bool {
	return duration >= time.Microsecond && duration <= maximumReconciliationWorkerDuration
}

func lostReconciliationFence(err error) bool {
	return errors.Is(err, store.ErrJobLeaseLost) || errors.Is(err, store.ErrPendingEventReconciliationFenceLost)
}

func permanentReconciliationFailure(err error) bool {
	return errors.Is(err, errInvalidPendingNormalizedEvent) ||
		errors.Is(err, store.ErrPendingEventReconciliationPayloadInvalid) ||
		errors.Is(err, store.ErrWorkflowLocatorMismatch) ||
		errors.Is(err, store.ErrWorkflowDecisionInvalid) ||
		errors.Is(err, store.ErrWorkflowSuccessorConflict) ||
		errors.Is(err, store.ErrWorkflowNotFound)
}
