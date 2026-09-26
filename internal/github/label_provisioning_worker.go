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

// LabelProvisioningWorkerStore is the durable job boundary used by the provisioning Worker.
type LabelProvisioningWorkerStore interface {
	ClaimLabelProvisioningJob(context.Context, string, time.Duration) (*store.LabelProvisioningLease, error)
	HeartbeatLabelProvisioningJob(context.Context, store.LabelProvisioningLease, time.Duration) error
	CompleteLabelProvisioningJob(context.Context, store.LabelProvisioningLease, json.RawMessage) error
	FailLabelProvisioningJob(context.Context, store.LabelProvisioningLease, error, bool, time.Duration) error
}

// LabelProvisioningCredentialProvider supplies Developer repository credentials.
type LabelProvisioningCredentialProvider interface {
	RepositoryCredential(context.Context, string, string) (string, error)
}

// LabelProvisioningAPI is the narrow GitHub API surface needed for verification and creation.
type LabelProvisioningAPI interface {
	LabelAPI
	ResolveRepositoryInstallation(context.Context, string, string, string) (int64, error)
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
	signer      AppJWTProvider
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
func NewLabelProvisioningWorker(workerStore LabelProvisioningWorkerStore, signer AppJWTProvider, credentials LabelProvisioningCredentialProvider, api LabelProvisioningAPI, config LabelProvisioningWorkerConfig) (*LabelProvisioningWorker, error) {
	if workerStore == nil || signer == nil || credentials == nil || api == nil {
		return nil, ErrInvalidLabelProvisioningWorkerConfiguration
	}
	if strings.TrimSpace(config.ClaimOwner) == "" ||
		!validLabelProvisioningDuration(config.LeaseDuration) || !validLabelProvisioningDuration(config.HeartbeatInterval) || config.HeartbeatInterval >= config.LeaseDuration ||
		!validLabelProvisioningDuration(config.IdlePollInterval) || !validLabelProvisioningDuration(config.RetryDelay) {
		return nil, ErrInvalidLabelProvisioningWorkerConfiguration
	}
	return &LabelProvisioningWorker{
		store: workerStore, signer: signer, credentials: credentials, api: api,
		reconciler: NewLabelReconciler(api),
		claimOwner: config.ClaimOwner, lease: config.LeaseDuration,
		heartbeat: config.HeartbeatInterval, idlePoll: config.IdlePollInterval,
		retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// ProcessNext claims and handles at most one repository provisioning job.
func (worker *LabelProvisioningWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease, err := worker.store.ClaimLabelProvisioningJob(ctx, worker.claimOwner, worker.lease)
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

func (worker *LabelProvisioningWorker) provision(ctx context.Context, lease store.LabelProvisioningLease) error {
	job := lease.LabelProvisioningJob
	if job.RepositoryID <= 0 || strings.TrimSpace(job.RepositoryOwner) == "" || strings.TrimSpace(job.RepositoryName) == "" ||
		strings.Contains(job.RepositoryOwner, "/") || strings.Contains(job.RepositoryName, "/") || job.InstallationID <= 0 {
		return worker.fail(ctx, lease, permanentProvisioningError{cause: fmt.Errorf("%w: provisioning job repository identity is invalid", ErrLabelProvisioningRepositoryMismatch)})
	}

	appJWT, err := worker.signer.AppJWT(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return worker.fail(ctx, lease, err)
	}
	actualInstallationID, err := worker.api.ResolveRepositoryInstallation(ctx, appJWT, job.RepositoryOwner, job.RepositoryName)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return worker.fail(ctx, lease, err)
	}
	if actualInstallationID != job.InstallationID {
		return worker.fail(ctx, lease, permanentProvisioningError{
			cause: fmt.Errorf("%w: repository %s/%s is installed as %d, want %d", ErrLabelProvisioningRepositoryMismatch, job.RepositoryOwner, job.RepositoryName, actualInstallationID, job.InstallationID),
		})
	}

	credential, err := worker.credentials.RepositoryCredential(ctx, job.RepositoryOwner, job.RepositoryName)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return worker.fail(ctx, lease, err)
	}
	if strings.TrimSpace(credential) == "" {
		return worker.fail(ctx, lease, permanentProvisioningError{cause: errors.New("Developer repository credential is empty")})
	}

	observed, err := worker.api.GetRepository(ctx, credential, job.RepositoryOwner, job.RepositoryName)
	if err != nil {
		if ctx.Err() != nil {
			return redactProvisioningError(err, credential)
		}
		return worker.fail(ctx, lease, redactProvisioningError(err, credential))
	}
	if observed.ID != job.RepositoryID || !strings.EqualFold(observed.Owner, job.RepositoryOwner) || !strings.EqualFold(observed.Name, job.RepositoryName) {
		return worker.fail(ctx, lease, permanentProvisioningError{
			cause: fmt.Errorf("%w: observed repository %d %s/%s does not match durable %d %s/%s",
				ErrLabelProvisioningRepositoryMismatch, observed.ID, observed.Owner, observed.Name,
				job.RepositoryID, job.RepositoryOwner, job.RepositoryName),
		})
	}

	if err := worker.reconciler.EnsureManagedLabels(ctx, credential, job.RepositoryOwner, job.RepositoryName); err != nil {
		if ctx.Err() != nil {
			return redactProvisioningError(err, credential)
		}
		return worker.fail(ctx, lease, redactProvisioningError(err, credential))
	}

	result, err := json.Marshal(map[string]any{
		"repository_id": job.RepositoryID, "installation_id": job.InstallationID,
		"owner": job.RepositoryOwner, "name": job.RepositoryName,
	})
	if err != nil {
		return worker.fail(ctx, lease, err)
	}
	if err := worker.store.CompleteLabelProvisioningJob(ctx, lease, result); err != nil {
		return fmt.Errorf("acknowledge label provisioning job: %w", err)
	}
	return nil
}

func (worker *LabelProvisioningWorker) fail(ctx context.Context, lease store.LabelProvisioningLease, cause error) error {
	retryable, delay := worker.classify(cause)
	if err := worker.store.FailLabelProvisioningJob(ctx, lease, cause, retryable, delay); err != nil {
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

func (worker *LabelProvisioningWorker) heartbeatLoop(ctx context.Context, lease store.LabelProvisioningLease) error {
	ticker := time.NewTicker(worker.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := worker.store.HeartbeatLabelProvisioningJob(ctx, lease, worker.lease); err != nil {
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
