package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

const maximumLabelProvisioningWorkerDuration = 365 * 24 * time.Hour

var (
	// ErrInvalidLabelProvisioningWorkerConfiguration means worker dependencies or timing are invalid.
	ErrInvalidLabelProvisioningWorkerConfiguration = errors.New("invalid label provisioning Worker configuration")
	// ErrLabelProvisioningRepositoryMismatch means the durable repository identity no longer matches GitHub.
	ErrLabelProvisioningRepositoryMismatch = errors.New("label provisioning repository identity mismatch")
)

// LabelProvisioningWorkerStore is the generic job boundary used by the provisioning Worker.
// Provisioning jobs reuse the shared jobs lifecycle (claim, heartbeat, retry,
// lease expiry in internal/store/jobs.go) so lifecycle fixes apply once.
type LabelProvisioningWorkerStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	CompleteJob(context.Context, store.JobLease, json.RawMessage) error
	FailJob(context.Context, store.JobLease, error, bool, time.Duration) error
}

// LabelProvisioningCredentialProvider supplies Developer installation credentials.
type LabelProvisioningCredentialProvider interface {
	InstallationCredential(context.Context, int64) (string, error)
}

// LabelProvisioningAPI is the narrow GitHub API surface needed for verification and creation.
type LabelProvisioningAPI interface {
	LabelAPI
	ListInstallationRepositories(context.Context, string) ([]InstallationRepository, []string, error)
	GetRepository(context.Context, string, string, string) (InstallationRepository, error)
}

// LabelProvisioningWorkerConfig controls claims, heartbeats, retries, and idle polling.
type LabelProvisioningWorkerConfig struct {
	ClaimOwner        string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdlePollInterval  time.Duration
	RetryDelay        time.Duration
	OnError           func(error)
}

// LabelProvisioningWorker provisions managed labels for repositories without starting a Workflow.
type LabelProvisioningWorker struct {
	store       LabelProvisioningWorkerStore
	credentials LabelProvisioningCredentialProvider
	api         LabelProvisioningAPI
	reconciler  *LabelReconciler
	claimOwner  string
	lease       time.Duration
	heartbeat   time.Duration
	idlePoll    time.Duration
	retryDelay  time.Duration
	onError     func(error)
}

var (
	_ LabelProvisioningWorkerStore        = (*store.Store)(nil)
	_ LabelProvisioningCredentialProvider = (*RepositoryInstallationCredentialProvider)(nil)
)

// NewLabelProvisioningWorker creates a Worker that ensures managed labels exist.
func NewLabelProvisioningWorker(workerStore LabelProvisioningWorkerStore, credentials LabelProvisioningCredentialProvider, api LabelProvisioningAPI, config LabelProvisioningWorkerConfig) (*LabelProvisioningWorker, error) {
	if workerStore == nil || credentials == nil || api == nil {
		return nil, ErrInvalidLabelProvisioningWorkerConfiguration
	}
	if strings.TrimSpace(config.ClaimOwner) == "" ||
		!validLabelProvisioningDuration(config.LeaseDuration) || !validLabelProvisioningDuration(config.HeartbeatInterval) || config.HeartbeatInterval >= config.LeaseDuration ||
		!validLabelProvisioningDuration(config.IdlePollInterval) || !validLabelProvisioningDuration(config.RetryDelay) {
		return nil, ErrInvalidLabelProvisioningWorkerConfiguration
	}
	return &LabelProvisioningWorker{
		store: workerStore, credentials: credentials, api: api,
		reconciler: NewLabelReconciler(api),
		claimOwner: config.ClaimOwner, lease: config.LeaseDuration,
		heartbeat: config.HeartbeatInterval, idlePoll: config.IdlePollInterval,
		retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// ProcessNext claims and handles at most one repository provisioning job.
func (worker *LabelProvisioningWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, worker.claimOwner, worker.lease)
	if err != nil {
		return false, fmt.Errorf("claim label provisioning job: %w", err)
	}
	if lease == nil {
		return false, nil
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(workCtx)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := worker.heartbeatLoop(heartbeatCtx, *lease)
		if heartbeatErr != nil {
			cancelWork()
		}
		heartbeatDone <- heartbeatErr
	}()

	operationErr := worker.provision(workCtx, *lease)

	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	if operationErr == nil {
		return true, nil
	}
	if heartbeatErr != nil {
		return true, errors.Join(fmt.Errorf("heartbeat label provisioning job: %w", heartbeatErr), operationErr)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, operationErr
}

// Run processes provisioning jobs until its context ends.
func (worker *LabelProvisioningWorker) Run(ctx context.Context) error {
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
		timer := time.NewTimer(worker.idlePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (worker *LabelProvisioningWorker) provision(ctx context.Context, lease store.JobLease) error {
	if lease.Kind != store.ProvisionManagedLabelsJobKind || lease.Queue != store.LabelProvisioningQueue {
		return worker.fail(ctx, lease, permanentProvisioningError{cause: fmt.Errorf("%w: provisioning job kind is %q", ErrLabelProvisioningRepositoryMismatch, lease.Kind)})
	}
	payload, err := store.ParseLabelProvisioningPayload(lease.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return worker.fail(ctx, lease, permanentProvisioningError{cause: err})
	}
	job := payload

	// The durable payload carries only stable IDs. Resolve the current
	// owner and name by repository ID on every attempt so a rename between
	// queueing and execution provisions the repository under its live
	// identity instead of stranding the job on a stale name.
	credential, err := worker.credentials.InstallationCredential(ctx, job.InstallationID)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return worker.fail(ctx, lease, err)
	}
	if strings.TrimSpace(credential) == "" {
		return worker.fail(ctx, lease, permanentProvisioningError{cause: errors.New("Developer installation credential is empty")})
	}
	owner, name, err := worker.resolveRepositoryName(ctx, credential, job)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return worker.fail(ctx, lease, err)
	}

	observed, err := worker.api.GetRepository(ctx, credential, owner, name)
	if err != nil {
		if ctx.Err() != nil {
			return redactProvisioningError(err, credential)
		}
		return worker.fail(ctx, lease, redactProvisioningError(err, credential))
	}
	if observed.ID != job.RepositoryID {
		return worker.fail(ctx, lease, permanentProvisioningError{
			cause: fmt.Errorf("%w: observed repository %d does not match durable %d",
				ErrLabelProvisioningRepositoryMismatch, observed.ID, job.RepositoryID),
		})
	}

	if err := worker.reconciler.EnsureManagedLabels(ctx, credential, owner, name); err != nil {
		if ctx.Err() != nil {
			return redactProvisioningError(err, credential)
		}
		return worker.fail(ctx, lease, redactProvisioningError(err, credential))
	}

	result, err := json.Marshal(map[string]any{
		"repository_id": job.RepositoryID, "installation_id": job.InstallationID,
		"owner": owner, "name": name,
	})
	if err != nil {
		return worker.fail(ctx, lease, err)
	}
	if err := worker.store.CompleteJob(ctx, lease, result); err != nil {
		return fmt.Errorf("acknowledge label provisioning job: %w", err)
	}
	return nil
}

// resolveRepositoryName maps the durable repository ID to its current
// owner and name through the installation listing. A repository absent
// from its installation lost Developer App access and fails terminally.
func (worker *LabelProvisioningWorker) resolveRepositoryName(ctx context.Context, credential string, job store.LabelProvisioningPayload) (string, string, error) {
	repositories, _, err := worker.api.ListInstallationRepositories(ctx, credential)
	if err != nil {
		return "", "", redactProvisioningError(err, credential)
	}
	for _, repository := range repositories {
		if repository.ID == job.RepositoryID {
			return repository.Owner, repository.Name, nil
		}
	}
	return "", "", permanentProvisioningError{
		cause: fmt.Errorf("%w: repository %d is no longer accessible to installation %d",
			ErrLabelProvisioningRepositoryMismatch, job.RepositoryID, job.InstallationID),
	}
}

func (worker *LabelProvisioningWorker) fail(ctx context.Context, lease store.JobLease, cause error) error {
	retryable, delay := worker.classify(cause)
	if err := worker.store.FailJob(ctx, lease, cause, retryable, delay); err != nil {
		return errors.Join(cause, fmt.Errorf("record label provisioning job failure: %w", err))
	}
	return cause
}

func (worker *LabelProvisioningWorker) classify(err error) (bool, time.Duration) {
	metadata := ExtractSafeErrorMetadata(err)
	if metadata.Permanent {
		return false, worker.retryDelay
	}
	if metadata.Transient || metadata.APIRetryable {
		delay := worker.retryDelay
		rateLimitDelay := metadata.RetryAfter
		if rateLimitDelay <= 0 && !metadata.ResetAt.IsZero() {
			rateLimitDelay = time.Until(metadata.ResetAt)
		}
		if rateLimitDelay > delay {
			delay = min(rateLimitDelay, maximumLabelProvisioningWorkerDuration)
		}
		return true, delay
	}
	if metadata.APIClientError {
		return false, worker.retryDelay
	}
	return true, worker.retryDelay
}

func (worker *LabelProvisioningWorker) heartbeatLoop(ctx context.Context, lease store.JobLease) error {
	ticker := time.NewTicker(worker.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := worker.store.HeartbeatJob(ctx, lease, worker.lease); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func validLabelProvisioningDuration(duration time.Duration) bool {
	return duration >= time.Microsecond && duration <= maximumLabelProvisioningWorkerDuration
}

type permanentProvisioningError struct{ cause error }

func (err permanentProvisioningError) Error() string   { return err.cause.Error() }
func (err permanentProvisioningError) Unwrap() error   { return err.cause }
func (err permanentProvisioningError) Permanent() bool { return true }

type credentialSafeProvisioningError struct {
	message  string
	metadata SafeErrorMetadata
}

func (err credentialSafeProvisioningError) Error() string { return err.message }
func (err credentialSafeProvisioningError) SafeErrorMetadata() SafeErrorMetadata {
	return err.metadata
}

func redactProvisioningError(err error, credential string) error {
	if err == nil || credential == "" {
		return err
	}
	return credentialSafeProvisioningError{
		message:  strings.ReplaceAll(err.Error(), credential, "[REDACTED]"),
		metadata: ExtractSafeErrorMetadata(err),
	}
}
