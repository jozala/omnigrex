package agentturn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/agentprofile"
	githubapi "github.com/jozala/omnigrex/internal/github"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
)

const maximumWorkerDuration = 365 * 24 * time.Hour

// WorkerStore is the durable boundary used by a preparation Worker.
type WorkerStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	GetWorkflowRepository(context.Context, string) (store.WorkflowRepository, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	AcknowledgeAgentTurnPreparationFailure(context.Context, store.JobLease, error, bool, time.Duration) (store.AgentTurnPreparationFailureAcknowledgement, error)
	AcknowledgeAssignmentConfigurationConflict(context.Context, store.JobLease, store.AgentTurnPreparationSpec) (store.AssignmentConfigurationHandoff, error)
}

// RepositoryCredentialProvider supplies a short-lived Role credential for repository access.
type RepositoryCredentialProvider interface {
	RepositoryCredential(context.Context, string, string) (string, error)
}

// TurnPreparer performs the credentialed profile load and fenced durable preparation.
type TurnPreparer interface {
	Prepare(context.Context, Request) (Result, error)
}

var (
	_ WorkerStore                  = (*store.Store)(nil)
	_ RepositoryCredentialProvider = (*githubapi.RepositoryInstallationCredentialProvider)(nil)
	_ TurnPreparer                 = (*Preparer)(nil)
)

// WorkerConfig controls preparation claims, heartbeats, retries, and idle polling.
type WorkerConfig struct {
	ClaimOwner        string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdlePollInterval  time.Duration
	RetryDelay        time.Duration
	OnError           func(error)
}

// Worker makes PREPARE_AGENT_TURN actions reachable from the durable Workflow queue.
type Worker struct {
	store                WorkerStore
	developerCredentials RepositoryCredentialProvider
	reviewerCredentials  RepositoryCredentialProvider
	preparer             TurnPreparer
	claimOwner           string
	leaseDuration        time.Duration
	heartbeatInterval    time.Duration
	idlePollInterval     time.Duration
	retryDelay           time.Duration
	onError              func(error)
}

// NewWorker creates a preparation worker with explicit lease timing.
func NewWorker(workerStore WorkerStore, developerCredentials, reviewerCredentials RepositoryCredentialProvider, preparer TurnPreparer, config WorkerConfig) (*Worker, error) {
	if nilDependency(workerStore) || nilDependency(developerCredentials) || nilDependency(reviewerCredentials) || nilDependency(preparer) {
		return nil, errors.New("Agent Turn preparation Worker dependency is nil")
	}
	if strings.TrimSpace(config.ClaimOwner) == "" {
		return nil, errors.New("Agent Turn preparation Worker claim owner is empty")
	}
	if config.LeaseDuration < time.Microsecond || config.LeaseDuration > maximumWorkerDuration ||
		config.HeartbeatInterval < time.Microsecond || config.HeartbeatInterval > maximumWorkerDuration || config.HeartbeatInterval >= config.LeaseDuration {
		return nil, errors.New("Agent Turn preparation Worker heartbeat must be positive and shorter than its lease")
	}
	if config.IdlePollInterval < time.Microsecond || config.IdlePollInterval > maximumWorkerDuration ||
		config.RetryDelay < time.Microsecond || config.RetryDelay > maximumWorkerDuration {
		return nil, errors.New("Agent Turn preparation Worker polling and retry timing is invalid")
	}
	return &Worker{
		store: workerStore, developerCredentials: developerCredentials, reviewerCredentials: reviewerCredentials, preparer: preparer,
		claimOwner: config.ClaimOwner, leaseDuration: config.LeaseDuration,
		heartbeatInterval: config.HeartbeatInterval, idlePollInterval: config.IdlePollInterval,
		retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// ProcessNext claims and durably handles at most one PREPARE_AGENT_TURN action.
func (worker *Worker) ProcessNext(ctx context.Context) (processed bool, err error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.WorkflowActionQueue, store.PrepareAgentTurnJobKind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim Agent Turn preparation: %w", err)
	}
	if lease == nil {
		return false, nil
	}

	var developerCredential, reviewerCredential string
	defer func() {
		if err != nil {
			err = redactCredentials(err, developerCredential, reviewerCredential)
		}
	}()
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

	operationErr := func() error {
		repository, err := worker.store.GetWorkflowRepository(workCtx, lease.WorkflowID)
		if err != nil {
			return fmt.Errorf("resolve preparation Workflow repository: %w", err)
		}
		developerCredential, err = worker.developerCredentials.RepositoryCredential(workCtx, repository.Owner, repository.Name)
		if err != nil {
			return fmt.Errorf("obtain Developer repository credential: %w", err)
		}
		if strings.TrimSpace(developerCredential) == "" {
			return permanentError{cause: errors.New("Developer repository credential is empty")}
		}
		reviewerCredential, err = worker.reviewerCredentials.RepositoryCredential(workCtx, repository.Owner, repository.Name)
		if err != nil {
			return fmt.Errorf("obtain Reviewer repository credential: %w", err)
		}
		if strings.TrimSpace(reviewerCredential) == "" {
			return permanentError{cause: errors.New("Reviewer repository credential is empty")}
		}
		_, err = worker.preparer.Prepare(workCtx, Request{
			Lease: *lease, InstallationCredential: developerCredential,
			RepositoryOwner: repository.Owner, RepositoryName: repository.Name,
		})
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrAssignmentConfigurationConflict) {
			var conflict *AssignmentConfigurationConflictError
			if !errors.As(err, &conflict) {
				return permanentError{cause: fmt.Errorf("Agent Assignment configuration conflict has no preparation: %w", err)}
			}
			if _, acknowledgeErr := worker.store.AcknowledgeAssignmentConfigurationConflict(workCtx, *lease, conflict.Preparation); acknowledgeErr != nil {
				return errors.Join(err, fmt.Errorf("acknowledge Agent Assignment configuration conflict: %w", acknowledgeErr))
			}
			return nil
		}
		return fmt.Errorf("prepare Agent Turn: %w", err)
	}()

	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	if operationErr == nil {
		return true, nil
	}
	operationErr = redactCredentials(operationErr, developerCredential, reviewerCredential)
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat Agent Turn preparation: %w", heartbeatErr), operationErr)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}

	retryable, retryDelay := worker.classify(operationErr)
	if _, failErr := worker.store.AcknowledgeAgentTurnPreparationFailure(ctx, *lease, operationErr, retryable, retryDelay); failErr != nil {
		return true, errors.Join(operationErr, fmt.Errorf("record Agent Turn preparation failure: %w", failErr))
	}
	return true, operationErr
}

func redactCredentials(err error, credentials ...string) error {
	for _, credential := range credentials {
		if credential != "" {
			err = redactCredential(err, credential)
		}
	}
	return err
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

func (worker *Worker) classify(err error) (bool, time.Duration) {
	metadata := githubapi.ExtractSafeErrorMetadata(err)
	if metadata.Permanent {
		return false, worker.retryDelay
	}
	if metadata.Transient || metadata.APIRetryable {
		return true, worker.retryAfter(err)
	}
	if metadata.APIClientError {
		return false, worker.retryDelay
	}
	if isPermanentWorkerError(err) {
		return false, worker.retryDelay
	}
	return true, worker.retryAfter(err)
}

func isPermanentWorkerError(err error) bool {
	return errors.Is(err, ErrDependencyNil) || errors.Is(err, ErrInvalidRequest) ||
		errors.Is(err, ErrInvalidRuntimeProfileReference) || errors.Is(err, ErrRuntimeProfileReferenceMismatch) ||
		errors.Is(err, agentprofile.ErrInvalidCommitSHA) || errors.Is(err, agentprofile.ErrMissingSource) ||
		errors.Is(err, agentprofile.ErrInvalidProfile) || errors.Is(err, agentprofile.ErrProfileTooLarge) ||
		errors.Is(err, agentprofile.ErrUnknownProfile) || errors.Is(err, runtimeprofile.ErrInvalid) ||
		errors.Is(err, runtimeprofile.ErrConflict) || errors.Is(err, runtimeprofile.ErrNotFound) ||
		errors.Is(err, store.ErrAgentTurnPreparationFenceLost) || errors.Is(err, store.ErrWorkflowNotFound)
}

func (worker *Worker) retryAfter(err error) time.Duration {
	delay := worker.retryDelay
	metadata := githubapi.ExtractSafeErrorMetadata(err)
	if metadata.RetryAfter != 0 || !metadata.ResetAt.IsZero() {
		rateLimitDelay := metadata.RetryAfter
		if rateLimitDelay <= 0 && !metadata.ResetAt.IsZero() {
			rateLimitDelay = time.Until(metadata.ResetAt)
		}
		if rateLimitDelay > delay {
			delay = min(rateLimitDelay, maximumWorkerDuration)
		}
	}
	return delay
}

// Run processes preparation actions until its context ends.
func (worker *Worker) Run(ctx context.Context) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if worker.onError != nil {
				worker.onError(err)
			}
			if err := waitForWorkerPoll(ctx, worker.idlePollInterval); err != nil {
				return err
			}
			continue
		}
		if processed {
			continue
		}
		if err := waitForWorkerPoll(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
}

func waitForWorkerPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type permanentError struct{ cause error }

func (err permanentError) Error() string   { return err.cause.Error() }
func (err permanentError) Unwrap() error   { return err.cause }
func (err permanentError) Permanent() bool { return true }
