package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

const maximumRecoveryWorkerDuration = 365 * 24 * time.Hour

var (
	ErrInvalidRecoveryWorkerConfiguration = errors.New("invalid mutation recovery Worker configuration")
	ErrMutationReconciliationUnresolved   = errors.New("mutation reconciliation is unresolved")
)

// RecoveryStore is the durable recovery-job boundary used by RecoveryWorker.
type RecoveryStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	GetAgentTurnMutationReconciliationContext(context.Context, store.JobLease) (store.AgentTurnMutationReconciliationContext, error)
	ListAgentTurnMutationsForReconciliation(context.Context, store.JobLease) ([]store.MutationReservation, error)
	ReconcileRecoveredMutation(context.Context, store.JobLease, string, store.RecoveredMutationOutcome) (store.MutationReservation, error)
	AcknowledgeAgentTurnMutationReconciliationFailure(context.Context, store.JobLease, error, time.Duration) (store.AgentTurnMutationReconciliationAcknowledgement, error)
	CompleteAgentTurnRecovery(context.Context, string, int64) (store.AgentTurnRecovery, error)
}

type RecoveryWorkerConfig struct {
	ClaimOwner        string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdlePollInterval  time.Duration
	RetryDelay        time.Duration
	OnError           func(error)
}

// RecoveryWorker resolves unknown mutations before opening successor Agent Turn allocation.
type RecoveryWorker struct {
	store             RecoveryStore
	reconciler        MutationReconciler
	claimOwner        string
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	idlePollInterval  time.Duration
	retryDelay        time.Duration
	onError           func(error)
}

var _ RecoveryStore = (*store.Store)(nil)

func NewRecoveryWorker(recoveryStore RecoveryStore, reconciler MutationReconciler, config RecoveryWorkerConfig) (*RecoveryWorker, error) {
	if interfaceNil(recoveryStore) || interfaceNil(reconciler) || strings.TrimSpace(config.ClaimOwner) == "" {
		return nil, ErrInvalidRecoveryWorkerConfiguration
	}
	if !validRecoveryWorkerDuration(config.LeaseDuration) || !validRecoveryWorkerDuration(config.HeartbeatInterval) ||
		config.HeartbeatInterval >= config.LeaseDuration || !validRecoveryWorkerDuration(config.IdlePollInterval) ||
		!validRecoveryWorkerDuration(config.RetryDelay) {
		return nil, ErrInvalidRecoveryWorkerConfiguration
	}
	return &RecoveryWorker{
		store: recoveryStore, reconciler: reconciler, claimOwner: config.ClaimOwner,
		leaseDuration: config.LeaseDuration, heartbeatInterval: config.HeartbeatInterval,
		idlePollInterval: config.IdlePollInterval, retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// ProcessNext claims and handles at most one mutation reconciliation job.
func (worker *RecoveryWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.ReconcileAgentTurnMutationsJobKind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim mutation reconciliation: %w", err)
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

	operationErr := worker.reconcileJob(workCtx, ctx, *lease)
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	if operationErr == nil {
		return true, nil
	}
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat mutation reconciliation: %w", heartbeatErr), operationErr)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if staleRecoveryFence(operationErr) {
		return true, operationErr
	}
	if errors.Is(operationErr, errRecoveryCompletion) {
		return true, operationErr
	}

	cause := errors.New("mutation reconciliation remained unresolved")
	acknowledgement, acknowledgeErr := worker.store.AcknowledgeAgentTurnMutationReconciliationFailure(ctx, *lease, cause, worker.retryDelay)
	if acknowledgeErr != nil {
		if staleRecoveryFence(acknowledgeErr) {
			return true, acknowledgeErr
		}
		return true, errors.Join(operationErr, fmt.Errorf("acknowledge mutation reconciliation failure: %w", acknowledgeErr))
	}
	if acknowledgement.Escalated {
		if _, completeErr := worker.store.CompleteAgentTurnRecovery(ctx, lease.AgentTurnID, lease.ExecutionEpoch); completeErr != nil {
			return true, errors.Join(operationErr, errRecoveryCompletion, completeErr)
		}
	}
	return true, operationErr
}

var errRecoveryCompletion = errors.New("complete Agent Turn recovery")

func (worker *RecoveryWorker) reconcileJob(ctx, completionCtx context.Context, lease store.JobLease) error {
	var reconciliation store.AgentTurnMutationReconciliationContext
	for {
		var err error
		reconciliation, err = worker.store.GetAgentTurnMutationReconciliationContext(ctx, lease)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
			return fmt.Errorf("read mutation reconciliation context: %w", err)
		}
		if err := waitRecoveryPoll(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
	mutations, err := worker.store.ListAgentTurnMutationsForReconciliation(ctx, lease)
	if err != nil {
		return fmt.Errorf("list mutations for reconciliation: %w", err)
	}
	var previousInvocation int64
	for _, mutation := range mutations {
		if mutation.InvocationNumber <= previousInvocation {
			return errors.New("mutation reconciliation order is invalid")
		}
		previousInvocation = mutation.InvocationNumber
		result, reconcileErr := worker.reconciler.Reconcile(ctx, reconciliation, mutation)
		if reconcileErr != nil {
			return fmt.Errorf("%w: reconcile mutation artifact", ErrMutationReconciliationDependency)
		}
		switch result.Disposition {
		case ReconciliationUnresolved:
			return ErrMutationReconciliationUnresolved
		case ReconciliationFound:
			if result.Outcome.State != store.MutationSucceeded || len(result.Outcome.Result) == 0 || !json.Valid(result.Outcome.Result) || result.Outcome.LastError != "" {
				return errors.New("mutation reconciler returned an invalid found outcome")
			}
		case ReconciliationDefinitelyFailed:
			if result.Outcome.State != store.MutationFailed || strings.TrimSpace(result.Outcome.LastError) == "" || len(result.Outcome.Result) != 0 {
				return errors.New("mutation reconciler returned an invalid failed outcome")
			}
		default:
			return errors.New("mutation reconciler returned an invalid disposition")
		}
		if _, err := worker.store.ReconcileRecoveredMutation(ctx, lease, mutation.ID, result.Outcome); err != nil {
			return fmt.Errorf("record reconciled mutation: %w", err)
		}
	}
	// The final mutation write completes the reconciliation job, so its expected stale heartbeat must not cancel barrier settlement.
	if _, err := worker.store.CompleteAgentTurnRecovery(completionCtx, reconciliation.Turn.ID, reconciliation.Turn.ExecutionEpoch); err != nil {
		return errors.Join(errRecoveryCompletion, err)
	}
	return nil
}

func (worker *RecoveryWorker) heartbeat(ctx context.Context, lease store.JobLease) error {
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

// Run processes mutation recovery jobs until its context ends.
func (worker *RecoveryWorker) Run(ctx context.Context) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if worker.onError != nil {
				worker.onError(err)
			}
			if err := waitRecoveryPoll(ctx, worker.idlePollInterval); err != nil {
				return err
			}
			continue
		}
		if processed {
			continue
		}
		if err := waitRecoveryPoll(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
}

func validRecoveryWorkerDuration(duration time.Duration) bool {
	return duration >= time.Microsecond && duration <= maximumRecoveryWorkerDuration
}

func staleRecoveryFence(err error) bool {
	return errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) || errors.Is(err, store.ErrJobLeaseLost)
}

func waitRecoveryPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
