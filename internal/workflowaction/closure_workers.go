package workflowaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
)

const (
	maximumClosureWorkerDuration = 365 * 24 * time.Hour
	runtimeProfileLabel          = "io.omnigrex.runtime-profile"
)

var (
	ErrInvalidClosureWorkerConfiguration = errors.New("invalid closure Worker configuration")
	ErrInvalidClosureRuntimeIdentity     = errors.New("invalid closure Runtime Process identity")
	ErrClosureReconciliationUnresolved   = errors.New("closure mutation reconciliation is unresolved")
	ErrInvalidClosureReconciliation      = errors.New("invalid closure mutation reconciliation result")
)

// ClosureRuntimeCleaner removes every Runtime Process with one complete exact identity.
type ClosureRuntimeCleaner interface {
	EnsureAbsent(context.Context, map[string]string) error
}

// ClosureStopStore is the durable boundary for STOP_AGENT_TURN work.
type ClosureStopStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	GetClosureTurnRuntimeIdentity(context.Context, store.JobLease) (store.AgentTurnRuntimeIdentity, error)
	AcknowledgeClosureTurnStopped(context.Context, store.JobLease) (store.ClosureSettlement, error)
	AcknowledgeClosureActionFailure(context.Context, store.JobLease, error, bool, time.Duration) (store.WorkflowActionFailureAcknowledgement, error)
}

// ClosureSettlementStore is the durable boundary for SETTLE_CLOSURE work.
type ClosureSettlementStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	GetClosureMutationReconciliationContext(context.Context, store.JobLease) (store.AgentTurnMutationReconciliationContext, error)
	ListClosureMutationsForReconciliation(context.Context, store.JobLease) ([]store.MutationReservation, error)
	ReconcileClosureMutation(context.Context, store.JobLease, string, store.RecoveredMutationOutcome) (store.MutationReservation, error)
	CompleteClosureSettlement(context.Context, store.JobLease) (store.ClosureSettlement, error)
	AcknowledgeClosureActionFailure(context.Context, store.JobLease, error, bool, time.Duration) (store.WorkflowActionFailureAcknowledgement, error)
	AcknowledgeClosureSettlementWait(context.Context, store.JobLease, time.Duration) (store.WorkflowActionFailureAcknowledgement, error)
}

// ClosureWorkerConfig controls exact-kind claims, lease maintenance, retries, and idle polling.
type ClosureWorkerConfig struct {
	ClaimOwner        string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdlePollInterval  time.Duration
	RetryDelay        time.Duration
	OnError           func(error)
}

// ClosureStopWorker proves exact Runtime Process absence before crossing the stop barrier.
type ClosureStopWorker struct {
	store             ClosureStopStore
	cleaner           ClosureRuntimeCleaner
	claimOwner        string
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	idlePollInterval  time.Duration
	retryDelay        time.Duration
	onError           func(error)
}

// ClosureSettlementWorker reconciles ambiguous mutations and invokes the reducer-owned closure transition.
type ClosureSettlementWorker struct {
	store             ClosureSettlementStore
	reconciler        mcp.MutationReconciler
	claimOwner        string
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	idlePollInterval  time.Duration
	retryDelay        time.Duration
	onError           func(error)
}

var (
	_ ClosureStopStore       = (*store.Store)(nil)
	_ ClosureSettlementStore = (*store.Store)(nil)
)

// NewClosureStopWorker creates a STOP_AGENT_TURN worker with an injected exact-identity cleaner.
func NewClosureStopWorker(workerStore ClosureStopStore, cleaner ClosureRuntimeCleaner, config ClosureWorkerConfig) (*ClosureStopWorker, error) {
	if nilClosureDependency(workerStore) || nilClosureDependency(cleaner) || !validClosureWorkerConfig(config) {
		return nil, ErrInvalidClosureWorkerConfiguration
	}
	return &ClosureStopWorker{
		store: workerStore, cleaner: cleaner, claimOwner: config.ClaimOwner,
		leaseDuration: config.LeaseDuration, heartbeatInterval: config.HeartbeatInterval,
		idlePollInterval: config.IdlePollInterval, retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// NewClosureSettlementWorker creates a SETTLE_CLOSURE worker using the production reconciliation semantics.
func NewClosureSettlementWorker(workerStore ClosureSettlementStore, reconciler mcp.MutationReconciler, config ClosureWorkerConfig) (*ClosureSettlementWorker, error) {
	if nilClosureDependency(workerStore) || nilClosureDependency(reconciler) || !validClosureWorkerConfig(config) {
		return nil, ErrInvalidClosureWorkerConfiguration
	}
	return &ClosureSettlementWorker{
		store: workerStore, reconciler: reconciler, claimOwner: config.ClaimOwner,
		leaseDuration: config.LeaseDuration, heartbeatInterval: config.HeartbeatInterval,
		idlePollInterval: config.IdlePollInterval, retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// ProcessNext claims and handles at most one exact STOP_AGENT_TURN job.
func (worker *ClosureStopWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.WorkflowActionQueue, store.StopAgentTurnJobKind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim closure Agent Turn stop: %w", err)
	}
	if lease == nil {
		return false, nil
	}
	operationErr, heartbeatErr := runClosureLease(ctx, *lease, worker.heartbeatInterval, worker.leaseDuration, worker.store.HeartbeatJob,
		func(workCtx context.Context) error { return worker.stop(workCtx, *lease) })
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat closure Agent Turn stop: %w", heartbeatErr), operationErr)
	}
	if operationErr == nil {
		return true, nil
	}
	if ctx.Err() != nil || staleClosureFence(operationErr) {
		return true, operationErr
	}
	_, acknowledgeErr := worker.store.AcknowledgeClosureActionFailure(ctx, *lease, operationErr, retryableClosureFailure(operationErr), worker.retryDelay)
	if acknowledgeErr != nil {
		return true, errors.Join(operationErr, fmt.Errorf("acknowledge closure Agent Turn stop failure: %w", acknowledgeErr))
	}
	return true, operationErr
}

func (worker *ClosureStopWorker) stop(ctx context.Context, lease store.JobLease) error {
	identity, err := worker.store.GetClosureTurnRuntimeIdentity(ctx, lease)
	if err != nil {
		return fmt.Errorf("read closure Runtime Process identity: %w", err)
	}
	labels, err := closureRuntimeLabels(lease, identity)
	if err != nil {
		return err
	}
	if err := worker.cleaner.EnsureAbsent(ctx, labels); err != nil {
		return fmt.Errorf("ensure closure Runtime Process absent: %w", err)
	}
	if _, err := worker.store.AcknowledgeClosureTurnStopped(ctx, lease); err != nil {
		return fmt.Errorf("acknowledge closure Agent Turn stopped: %w", err)
	}
	return nil
}

// ProcessNext claims and handles at most one exact SETTLE_CLOSURE job.
func (worker *ClosureSettlementWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.WorkflowActionQueue, store.SettleClosureJobKind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim closure settlement: %w", err)
	}
	if lease == nil {
		return false, nil
	}
	operationErr, heartbeatErr := runClosureLease(ctx, *lease, worker.heartbeatInterval, worker.leaseDuration, worker.store.HeartbeatJob,
		func(workCtx context.Context) error { return worker.settle(workCtx, *lease) })
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat closure settlement: %w", heartbeatErr), operationErr)
	}
	if operationErr == nil {
		return true, nil
	}
	if ctx.Err() != nil || staleClosureFence(operationErr) {
		return true, operationErr
	}
	if errors.Is(operationErr, store.ErrClosureSettlementUnsettled) {
		acknowledgement, acknowledgeErr := worker.store.AcknowledgeClosureSettlementWait(ctx, *lease, worker.retryDelay)
		if acknowledgeErr != nil {
			return true, errors.Join(operationErr, fmt.Errorf("acknowledge closure stop-barrier wait: %w", acknowledgeErr))
		}
		if acknowledgement.RetryScheduled {
			return true, nil
		}
		return true, operationErr
	}
	_, acknowledgeErr := worker.store.AcknowledgeClosureActionFailure(ctx, *lease, operationErr, retryableClosureFailure(operationErr), worker.retryDelay)
	if acknowledgeErr != nil {
		return true, errors.Join(operationErr, fmt.Errorf("acknowledge closure settlement failure: %w", acknowledgeErr))
	}
	return true, operationErr
}

func (worker *ClosureSettlementWorker) settle(ctx context.Context, lease store.JobLease) error {
	if lease.AgentTurnID == "" {
		if _, err := worker.store.CompleteClosureSettlement(ctx, lease); err != nil {
			return fmt.Errorf("complete closure without active Agent Turn: %w", err)
		}
		return nil
	}
	reconciliation, err := worker.store.GetClosureMutationReconciliationContext(ctx, lease)
	if err != nil {
		return fmt.Errorf("read closure mutation reconciliation context: %w", err)
	}
	mutations, err := worker.store.ListClosureMutationsForReconciliation(ctx, lease)
	if err != nil {
		return fmt.Errorf("list closure mutations for reconciliation: %w", err)
	}
	var previousInvocation int64
	for _, mutation := range mutations {
		if mutation.InvocationNumber <= previousInvocation {
			return ErrInvalidClosureReconciliation
		}
		previousInvocation = mutation.InvocationNumber
		result, err := worker.reconciler.Reconcile(ctx, reconciliation, mutation)
		if err != nil {
			return fmt.Errorf("reconcile closure mutation artifact: %w", err)
		}
		if err := validateClosureReconciliation(result); err != nil {
			return err
		}
		if _, err := worker.store.ReconcileClosureMutation(ctx, lease, mutation.ID, result.Outcome); err != nil {
			return fmt.Errorf("record reconciled closure mutation: %w", err)
		}
	}
	if _, err := worker.store.CompleteClosureSettlement(ctx, lease); err != nil {
		return fmt.Errorf("complete closure settlement: %w", err)
	}
	return nil
}

func validateClosureReconciliation(result mcp.MutationReconciliationResult) error {
	switch result.Disposition {
	case mcp.ReconciliationUnresolved:
		return ErrClosureReconciliationUnresolved
	case mcp.ReconciliationFound:
		if result.Outcome.State != store.MutationSucceeded || len(result.Outcome.Result) == 0 || !json.Valid(result.Outcome.Result) || result.Outcome.LastError != "" {
			return ErrInvalidClosureReconciliation
		}
	case mcp.ReconciliationDefinitelyFailed:
		if result.Outcome.State != store.MutationFailed || strings.TrimSpace(result.Outcome.LastError) == "" || len(result.Outcome.Result) != 0 {
			return ErrInvalidClosureReconciliation
		}
	default:
		return ErrInvalidClosureReconciliation
	}
	return nil
}

func closureRuntimeLabels(lease store.JobLease, identity store.AgentTurnRuntimeIdentity) (map[string]string, error) {
	if lease.Queue != store.WorkflowActionQueue || lease.Kind != store.StopAgentTurnJobKind ||
		identity.AssignmentID != lease.AgentAssignmentID || identity.AgentSessionID != lease.AgentSessionID ||
		identity.AgentTurnID != lease.AgentTurnID || identity.ExecutionEpoch != lease.ExecutionEpoch ||
		!validClosureUUID(identity.AssignmentID) || !validClosureUUID(identity.AgentSessionID) ||
		!validClosureUUID(identity.AgentTurnID) || identity.ExecutionEpoch <= 0 ||
		!validRuntimeProfilePart(identity.RuntimeProfileName) || !validRuntimeProfilePart(identity.RuntimeProfileVersion) {
		return nil, ErrInvalidClosureRuntimeIdentity
	}
	return map[string]string{
		store.RuntimeLabelAssignmentID: identity.AssignmentID,
		store.RuntimeLabelSessionID:    identity.AgentSessionID,
		store.RuntimeLabelTurnID:       identity.AgentTurnID,
		store.RuntimeLabelEpoch:        strconv.FormatInt(identity.ExecutionEpoch, 10),
		runtimeProfileLabel:            identity.RuntimeProfileName + "/" + identity.RuntimeProfileVersion,
	}, nil
}

func (worker *ClosureStopWorker) Run(ctx context.Context) error {
	return runClosureWorker(ctx, worker.idlePollInterval, worker.onError, worker.ProcessNext)
}

func (worker *ClosureSettlementWorker) Run(ctx context.Context) error {
	return runClosureWorker(ctx, worker.idlePollInterval, worker.onError, worker.ProcessNext)
}

func runClosureWorker(ctx context.Context, idlePollInterval time.Duration, onError func(error), process func(context.Context) (bool, error)) error {
	for {
		processed, err := process(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if onError != nil {
				onError(err)
			}
		}
		if err == nil && processed {
			continue
		}
		if err := waitClosureWorker(ctx, idlePollInterval); err != nil {
			return err
		}
	}
}

func runClosureLease(ctx context.Context, lease store.JobLease, heartbeatInterval, leaseDuration time.Duration,
	heartbeat func(context.Context, store.JobLease, time.Duration) error, operation func(context.Context) error,
) (error, error) {
	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(workCtx)
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				heartbeatDone <- nil
				return
			case <-ticker.C:
				if err := heartbeat(heartbeatCtx, lease, leaseDuration); err != nil {
					if heartbeatCtx.Err() != nil {
						heartbeatDone <- nil
						return
					}
					cancelWork()
					heartbeatDone <- err
					return
				}
			}
		}
	}()
	operationErr := operation(workCtx)
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	return operationErr, heartbeatErr
}

func validClosureWorkerConfig(config ClosureWorkerConfig) bool {
	return strings.TrimSpace(config.ClaimOwner) != "" && validClosureDuration(config.LeaseDuration) &&
		validClosureDuration(config.HeartbeatInterval) && config.HeartbeatInterval < config.LeaseDuration &&
		validClosureDuration(config.IdlePollInterval) && validClosureDuration(config.RetryDelay)
}

func validClosureDuration(value time.Duration) bool {
	return value >= time.Microsecond && value <= maximumClosureWorkerDuration
}

func retryableClosureFailure(err error) bool {
	return !errors.Is(err, ErrInvalidClosureRuntimeIdentity) &&
		!errors.Is(err, ErrInvalidClosureReconciliation) &&
		!errors.Is(err, mcp.ErrInvalidMutationReconciliation)
}

func staleClosureFence(err error) bool {
	return errors.Is(err, store.ErrJobLeaseLost) || errors.Is(err, store.ErrClosureSettlementFenceLost)
}

func validClosureUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return value != "00000000-0000-0000-0000-000000000000"
}

func validRuntimeProfilePart(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.Contains(value, "/") ||
		!lowerAlphaNumeric(value[0]) || !lowerAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if !lowerAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func lowerAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}

func nilClosureDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func waitClosureWorker(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
