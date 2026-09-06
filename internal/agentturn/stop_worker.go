package agentturn

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

// StopWorkerStore is the durable recovery-job boundary used by StopWorker.
type StopWorkerStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	GetAgentTurnRuntimeCleanupContext(context.Context, store.JobLease) (store.AgentTurnRuntimeCleanupContext, error)
	AcknowledgeRecoveredRuntimeStopped(context.Context, store.JobLease) (store.AgentTurnRecovery, error)
	AcknowledgeRecoveredRuntimeStopFailure(context.Context, store.JobLease, error, time.Duration) (store.AgentTurnRuntimeStopFailureAcknowledgement, error)
}

// ExactRuntimeCleaner removes every Runtime Process carrying an exact runtime identity.
type ExactRuntimeCleaner interface {
	EnsureAbsent(context.Context, map[string]string) error
}

// WorkspaceDiscarder removes recovered Reviewer workspace changes.
type WorkspaceDiscarder interface {
	DiscardWorkspace(string) error
}

// StopWorkerConfig controls stale-runtime recovery claims, heartbeats, retries, and idle polling.
type StopWorkerConfig struct {
	ClaimOwner           string
	LeaseDuration        time.Duration
	HeartbeatInterval    time.Duration
	IdlePollInterval     time.Duration
	CleanupRetryInterval time.Duration
	OnError              func(error)
}

// StopWorker makes STOP_STALE_RUNTIME recovery jobs reachable from the durable queue.
type StopWorker struct {
	store                StopWorkerStore
	cleaner              ExactRuntimeCleaner
	workspaces           WorkspaceDiscarder
	claimOwner           string
	leaseDuration        time.Duration
	heartbeatInterval    time.Duration
	idlePollInterval     time.Duration
	cleanupRetryInterval time.Duration
	onError              func(error)
}

var _ StopWorkerStore = (*store.Store)(nil)

// NewStopWorker creates a stale-runtime recovery worker with explicit lease timing.
func NewStopWorker(workerStore StopWorkerStore, cleaner ExactRuntimeCleaner, workspaces WorkspaceDiscarder, config StopWorkerConfig) (*StopWorker, error) {
	if nilDependency(workerStore) || nilDependency(cleaner) || nilDependency(workspaces) {
		return nil, errors.New("stale Runtime Process stop Worker dependency is nil")
	}
	if strings.TrimSpace(config.ClaimOwner) == "" {
		return nil, errors.New("stale Runtime Process stop Worker claim owner is empty")
	}
	if !validStopWorkerDuration(config.LeaseDuration) || !validStopWorkerDuration(config.HeartbeatInterval) ||
		config.HeartbeatInterval >= config.LeaseDuration {
		return nil, errors.New("stale Runtime Process stop Worker heartbeat must be positive and shorter than its lease")
	}
	if !validStopWorkerDuration(config.IdlePollInterval) || !validStopWorkerDuration(config.CleanupRetryInterval) {
		return nil, errors.New("stale Runtime Process stop Worker polling and retry timing is invalid")
	}
	return &StopWorker{
		store: workerStore, cleaner: cleaner, workspaces: workspaces, claimOwner: config.ClaimOwner,
		leaseDuration: config.LeaseDuration, heartbeatInterval: config.HeartbeatInterval,
		idlePollInterval: config.IdlePollInterval, cleanupRetryInterval: config.CleanupRetryInterval,
		onError: config.OnError,
	}, nil
}

// ProcessNext claims and handles at most one STOP_STALE_RUNTIME recovery job.
func (worker *StopWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim stale Runtime Process stop: %w", err)
	}
	if lease == nil {
		return false, nil
	}
	labels, err := recoveryRuntimeLabels(*lease)
	if err != nil {
		return true, err
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

	operationErr := worker.stopRuntime(workCtx, *lease, labels)
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	if operationErr == nil {
		return true, nil
	}
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat stale Runtime Process stop: %w", heartbeatErr), operationErr)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if staleStopWorkerFence(operationErr) {
		return true, operationErr
	}
	if _, acknowledgeErr := worker.store.AcknowledgeRecoveredRuntimeStopFailure(ctx, *lease, operationErr, worker.cleanupRetryInterval); acknowledgeErr != nil {
		return true, errors.Join(operationErr, fmt.Errorf("acknowledge stale Runtime Process stop failure: %w", acknowledgeErr))
	}
	return true, operationErr
}

func recoveryRuntimeLabels(lease store.JobLease) (map[string]string, error) {
	if lease.Queue != store.AgentTurnRecoveryQueue || lease.Kind != store.StopStaleRuntimeJobKind ||
		!validRuntimeLabelIdentity(lease.AgentAssignmentID) ||
		!validRuntimeLabelIdentity(lease.AgentSessionID) ||
		!validRuntimeLabelIdentity(lease.AgentTurnID) ||
		lease.ExecutionEpoch <= 0 {
		return nil, errors.New("stale Runtime Process stop job has invalid identity")
	}
	return map[string]string{
		store.RuntimeLabelAssignmentID: lease.AgentAssignmentID,
		store.RuntimeLabelSessionID:    lease.AgentSessionID,
		store.RuntimeLabelTurnID:       lease.AgentTurnID,
		store.RuntimeLabelEpoch:        strconv.FormatInt(lease.ExecutionEpoch, 10),
	}, nil
}

func validRuntimeLabelIdentity(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func (worker *StopWorker) stopRuntime(ctx context.Context, lease store.JobLease, labels map[string]string) error {
	cleanup, err := worker.store.GetAgentTurnRuntimeCleanupContext(ctx, lease)
	if err != nil {
		return fmt.Errorf("read stale Runtime Process cleanup context: %w", err)
	}

	if err := worker.cleaner.EnsureAbsent(ctx, labels); err != nil {
		return fmt.Errorf("ensure stale Runtime Process absent: %w", err)
	}

	if cleanup.Role == workflow.RoleReviewer {
		if err := worker.workspaces.DiscardWorkspace(cleanup.AssignmentID); err != nil {
			return fmt.Errorf("discard recovered Reviewer workspace: %w", err)
		}
	}

	if _, err := worker.store.AcknowledgeRecoveredRuntimeStopped(ctx, lease); err != nil {
		return fmt.Errorf("acknowledge stale Runtime Process stopped: %w", err)
	}
	return nil
}

func (worker *StopWorker) heartbeat(ctx context.Context, lease store.JobLease) error {
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

// Run processes stale-runtime recovery jobs until its context ends or its fence is lost.
func (worker *StopWorker) Run(ctx context.Context) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if staleStopWorkerFence(err) {
				return err
			}
			if worker.onError != nil {
				worker.onError(err)
			}
			if err := waitForStopWorker(ctx, worker.idlePollInterval); err != nil {
				return err
			}
			continue
		}
		if processed {
			continue
		}
		if err := waitForStopWorker(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
}

func validStopWorkerDuration(duration time.Duration) bool {
	return duration >= time.Microsecond && duration <= maximumWorkerDuration
}

func staleStopWorkerFence(err error) bool {
	return errors.Is(err, store.ErrJobLeaseLost) || errors.Is(err, store.ErrAgentTurnRecoveryFenceLost)
}

func waitForStopWorker(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
