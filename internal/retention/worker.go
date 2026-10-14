package retention

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

const maximumWorkerDuration = 365 * 24 * time.Hour

var ErrInvalidCleanupTargets = store.ErrInvalidAssignmentCleanupTargets

// WorkerStore is the durable Assignment collection boundary used by Worker.
type WorkerStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	AuthorizeAssignmentCollection(context.Context, store.JobLease) (store.AssignmentCollectionAuthorization, error)
	FinalizeAssignmentCollection(context.Context, store.JobLease, []store.AssignmentCleanupTarget) (store.AssignmentCollection, error)
	AcknowledgeAssignmentCollectionFailure(context.Context, store.JobLease, error, bool, time.Duration) (store.WorkflowActionFailureAcknowledgement, error)
}

// Cleaner collects an authorized Participant's logical retained resources.
type Cleaner interface {
	CollectParticipant(context.Context, string) error
}

// WorkerConfig controls collection claims, heartbeats, retries, and idle polling.
type WorkerConfig struct {
	ClaimOwner        string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdlePollInterval  time.Duration
	RetryDelay        time.Duration
	OnError           func(error)
}

// Worker garbage-collects authorized Assignment runtime state while retaining database history.
type Worker struct {
	store             WorkerStore
	cleaner           Cleaner
	claimOwner        string
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	idlePollInterval  time.Duration
	retryDelay        time.Duration
	onError           func(error)
}

var _ WorkerStore = (*store.Store)(nil)

// NewWorker creates an Assignment retention worker with explicit lease timing.
func NewWorker(workerStore WorkerStore, cleaner Cleaner, config WorkerConfig) (*Worker, error) {
	if nilDependency(workerStore) || nilDependency(cleaner) {
		return nil, errors.New("Assignment retention Worker dependency is nil")
	}
	if strings.TrimSpace(config.ClaimOwner) == "" {
		return nil, errors.New("Assignment retention Worker claim owner is empty")
	}
	if !validDuration(config.LeaseDuration) || !validDuration(config.HeartbeatInterval) ||
		config.HeartbeatInterval >= config.LeaseDuration {
		return nil, errors.New("Assignment retention Worker heartbeat must be positive and shorter than its lease")
	}
	if !validDuration(config.IdlePollInterval) || !validDuration(config.RetryDelay) {
		return nil, errors.New("Assignment retention Worker polling and retry timing is invalid")
	}
	return &Worker{
		store: workerStore, cleaner: cleaner, claimOwner: config.ClaimOwner,
		leaseDuration: config.LeaseDuration, heartbeatInterval: config.HeartbeatInterval,
		idlePollInterval: config.IdlePollInterval, retryDelay: config.RetryDelay,
		onError: config.OnError,
	}, nil
}

// ProcessNext claims and durably handles at most one due COLLECT_ASSIGNMENTS job.
func (worker *Worker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim Assignment collection: %w", err)
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

	operationErr := worker.collect(workCtx, *lease)
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	if operationErr == nil {
		return true, nil
	}
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat Assignment collection: %w", heartbeatErr), operationErr)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if collectionFenceLost(operationErr) {
		return true, operationErr
	}
	retryable := !errors.Is(operationErr, ErrInvalidCleanupTargets) &&
		!errors.Is(operationErr, store.ErrAssignmentCollectionIncomplete)
	if _, failErr := worker.store.AcknowledgeAssignmentCollectionFailure(ctx, *lease, operationErr, retryable, worker.retryDelay); failErr != nil {
		return true, errors.Join(operationErr, fmt.Errorf("record Assignment collection failure: %w", failErr))
	}
	return true, operationErr
}

func (worker *Worker) collect(ctx context.Context, lease store.JobLease) error {
	authorization, err := worker.store.AuthorizeAssignmentCollection(ctx, lease)
	if err != nil {
		return fmt.Errorf("authorize Assignment collection: %w", err)
	}
	participants, err := cleanupParticipants(authorization.Targets)
	if err != nil {
		return err
	}
	for _, participant := range participants {
		if err := worker.cleaner.CollectParticipant(ctx, participant); err != nil {
			return fmt.Errorf("ensure Assignment runtime state absent: %w", err)
		}
	}
	if _, err := worker.store.FinalizeAssignmentCollection(ctx, lease, authorization.Targets); err != nil {
		return fmt.Errorf("finalize Assignment collection: %w", err)
	}
	return nil
}

func cleanupParticipants(targets []store.AssignmentCleanupTarget) ([]string, error) {
	if err := store.ValidateAssignmentCleanupTargets(targets); err != nil {
		return nil, err
	}
	assignments := make(map[string]bool)
	for _, target := range targets {
		assignments[target.AssignmentID] = true
	}

	participants := make([]string, 0, len(assignments))
	for assignmentID := range assignments {
		participants = append(participants, assignmentID)
	}
	sort.Strings(participants)
	return participants, nil
}

func (worker *Worker) heartbeat(ctx context.Context, lease store.JobLease) error {
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

// Run processes Assignment collection jobs until its context ends.
func (worker *Worker) Run(ctx context.Context) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil && ctx.Err() == nil && worker.onError != nil {
			worker.onError(err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && processed {
			continue
		}
		if err := wait(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
}

func collectionFenceLost(err error) bool {
	return errors.Is(err, store.ErrJobLeaseLost) || errors.Is(err, store.ErrAssignmentCollectionFenceLost)
}

func validDuration(duration time.Duration) bool {
	return duration >= time.Microsecond && duration <= maximumWorkerDuration
}

func nilDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func wait(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
