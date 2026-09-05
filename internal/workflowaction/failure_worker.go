package workflowaction

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

const maximumFailureWorkerDuration = 365 * 24 * time.Hour

// FailureWorkerStore owns the atomic completion-barrier check and escalation transition.
type FailureWorkerStore interface {
	ApplyNextWorkflowActionFailureEscalation(context.Context, string, time.Duration) (*store.WorkflowActionFailureEscalation, error)
}

// FailureWorkerConfig controls durable escalation checks and idle polling.
type FailureWorkerConfig struct {
	ClaimOwner       string
	LeaseDuration    time.Duration
	IdlePollInterval time.Duration
	OnError          func(error)
}

// FailureWorker applies exhausted Workflow actions only after turn and successor authority settles.
type FailureWorker struct {
	store            FailureWorkerStore
	claimOwner       string
	leaseDuration    time.Duration
	idlePollInterval time.Duration
	onError          func(error)
}

var _ FailureWorkerStore = (*store.Store)(nil)

func NewFailureWorker(workerStore FailureWorkerStore, config FailureWorkerConfig) (*FailureWorker, error) {
	if workerStore == nil || strings.TrimSpace(config.ClaimOwner) == "" ||
		!validFailureWorkerDuration(config.LeaseDuration) || !validFailureWorkerDuration(config.IdlePollInterval) {
		return nil, errors.New("invalid Workflow action failure Worker configuration")
	}
	return &FailureWorker{
		store: workerStore, claimOwner: config.ClaimOwner, leaseDuration: config.LeaseDuration,
		idlePollInterval: config.IdlePollInterval, onError: config.OnError,
	}, nil
}

// ProcessNext checks and applies at most one barrier-safe Workflow action escalation.
func (worker *FailureWorker) ProcessNext(ctx context.Context) (bool, error) {
	escalation, err := worker.store.ApplyNextWorkflowActionFailureEscalation(ctx, worker.claimOwner, worker.leaseDuration)
	return escalation != nil, err
}

func (worker *FailureWorker) Run(ctx context.Context) error {
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
		timer := time.NewTimer(worker.idlePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func validFailureWorkerDuration(duration time.Duration) bool {
	return duration >= time.Microsecond && duration <= maximumFailureWorkerDuration
}
