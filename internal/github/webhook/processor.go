package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/uuidtext"
	"github.com/jozala/omnigrex/internal/workflow"
)

var errInvalidPendingNormalizedEvent = store.ErrPendingNormalizedEventInvalid

// ProcessorStore is the durable inbox boundary used by Processor.
type ProcessorStore interface {
	ClaimWebhookDelivery(context.Context, string, time.Duration) (*store.WebhookClaim, error)
	CompleteWebhookDelivery(context.Context, string, string, store.WebhookCompletion) error
	CompleteWebhookTransition(context.Context, string, string, json.RawMessage, store.WorkflowLocator, store.WorkflowEventFactory) (store.WorkflowApplication, error)
	ApplyNextPendingNormalizedEvent(context.Context, store.PendingWorkflowEventFactory) (store.WorkflowApplication, bool, error)
	AcknowledgeWebhookDeliveryFailure(context.Context, string, string, int, error, bool) error
}

// ProcessorConfig controls claim ownership, lease duration, and idle polling.
type ProcessorConfig struct {
	ClaimOwner                  string
	LeaseDuration               time.Duration
	IdlePollInterval            time.Duration
	AssignmentRetentionDuration time.Duration
	OnError                     func(error)
}

// Processor claims and normalizes durable GitHub webhook deliveries.
type Processor struct {
	store                       ProcessorStore
	claimOwner                  string
	leaseDuration               time.Duration
	idlePollInterval            time.Duration
	assignmentRetentionDuration time.Duration
	onError                     func(error)
}

// NewProcessor creates a durable webhook processor with explicit polling bounds.
func NewProcessor(processorStore ProcessorStore, config ProcessorConfig) (*Processor, error) {
	if processorStore == nil {
		return nil, errors.New("webhook processor store is nil")
	}
	if strings.TrimSpace(config.ClaimOwner) == "" {
		return nil, errors.New("webhook processor claim owner is empty")
	}
	if config.LeaseDuration < 5*time.Second {
		return nil, errors.New("webhook processor lease duration must be at least five seconds")
	}
	if config.IdlePollInterval <= 0 {
		return nil, errors.New("webhook processor idle poll interval must be positive")
	}
	if config.AssignmentRetentionDuration <= 0 {
		return nil, errors.New("webhook processor assignment retention duration must be positive")
	}
	return &Processor{
		store:                       processorStore,
		claimOwner:                  config.ClaimOwner,
		leaseDuration:               config.LeaseDuration,
		idlePollInterval:            config.IdlePollInterval,
		assignmentRetentionDuration: config.AssignmentRetentionDuration,
		onError:                     config.OnError,
	}, nil
}

// ProcessNext durably applies at most one historical event or claimed delivery.
// The returned boolean reports whether work was processed.
func (processor *Processor) ProcessNext(ctx context.Context) (bool, error) {
	if _, applied, err := processor.store.ApplyNextPendingNormalizedEvent(ctx, processor.pendingEventFactory); err != nil {
		return false, fmt.Errorf("apply pending normalized event: %w", err)
	} else if applied {
		return true, nil
	}

	claim, err := processor.store.ClaimWebhookDelivery(ctx, processor.claimOwner, processor.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim webhook delivery: %w", err)
	}
	if claim == nil {
		return false, nil
	}

	normalization, err := Normalize(Delivery{
		DeliveryID: claim.DeliveryID,
		EventName:  claim.EventName,
		Action:     claim.Action,
		Payload:    claim.Payload,
	})
	if err != nil {
		cause := fmt.Errorf("normalize webhook delivery %s: %w", claim.DeliveryID, err)
		return true, processor.acknowledgeFailure(ctx, claim, cause, false)
	}

	switch normalization.Outcome {
	case NormalizationSupported:
		if normalization.Event == nil {
			cause := errors.New("normalize webhook delivery: supported outcome has no event")
			return true, processor.acknowledgeFailure(ctx, claim, cause, false)
		}
		payload, err := json.Marshal(normalization.Event)
		if err != nil {
			cause := fmt.Errorf("encode normalized webhook delivery %s: %w", claim.DeliveryID, err)
			return true, processor.acknowledgeFailure(ctx, claim, cause, false)
		}
		locator, eventFactory, err := processor.eventFactory(*normalization.Event)
		if err != nil {
			cause := fmt.Errorf("map normalized webhook delivery %s: %w", claim.DeliveryID, err)
			return true, processor.acknowledgeFailure(ctx, claim, cause, !deterministicWebhookFailure(err))
		}
		if _, err := processor.store.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, payload, locator, eventFactory); err != nil {
			cause := fmt.Errorf("complete webhook transition %s: %w", claim.DeliveryID, err)
			return true, processor.acknowledgeFailure(ctx, claim, cause, !deterministicWebhookFailure(err))
		}
	case NormalizationIgnored:
		if normalization.Event != nil {
			cause := errors.New("normalize webhook delivery: ignored outcome has an event")
			return true, processor.acknowledgeFailure(ctx, claim, cause, false)
		}
		if err := processor.store.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{Outcome: store.WebhookOutcomeIgnored}); err != nil {
			cause := fmt.Errorf("complete ignored webhook delivery %s: %w", claim.DeliveryID, err)
			return true, processor.acknowledgeFailure(ctx, claim, cause, true)
		}
	default:
		cause := fmt.Errorf("normalize webhook delivery: invalid outcome %q", normalization.Outcome)
		return true, processor.acknowledgeFailure(ctx, claim, cause, false)
	}
	return true, nil
}

func (processor *Processor) acknowledgeFailure(ctx context.Context, claim *store.WebhookClaim, cause error, retryable bool) error {
	if err := processor.store.AcknowledgeWebhookDeliveryFailure(ctx, claim.DeliveryID, claim.ClaimToken, claim.AttemptCount, cause, retryable); err != nil {
		return errors.Join(cause, fmt.Errorf("acknowledge webhook delivery %s failure: %w", claim.DeliveryID, err))
	}
	if retryable {
		return cause
	}
	return nil
}

func deterministicWebhookFailure(err error) bool {
	return errors.Is(err, errInvalidPendingNormalizedEvent) ||
		errors.Is(err, store.ErrNormalizedEventDeliveryMismatch) ||
		errors.Is(err, store.ErrWorkflowLocatorMismatch) ||
		errors.Is(err, store.ErrWorkflowDecisionInvalid)
}

func (processor *Processor) pendingEventFactory(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowEventFactory, error) {
	var event NormalizedEvent
	if err := json.Unmarshal(record.Payload, &event); err != nil {
		return store.WorkflowLocator{}, nil, fmt.Errorf("%w: decode normalized event %s: %v", errInvalidPendingNormalizedEvent, record.DeliveryID, err)
	}
	return processor.eventFactory(event)
}

func (processor *Processor) eventFactory(event NormalizedEvent) (store.WorkflowLocator, store.WorkflowEventFactory, error) {
	locator := store.WorkflowLocator{RepositoryID: event.Repository.ID}
	if event.Issue != nil {
		locator.IssueID, locator.IssueNumber = event.Issue.ID, event.Issue.Number
	}
	if event.PullRequest != nil {
		locator.PullRequestID, locator.PullRequestNumber = event.PullRequest.ID, event.PullRequest.Number
		locator.WorkflowID = event.PullRequest.WorkflowMarkerID
		locator.WorkflowMarkerInvalid = event.PullRequest.WorkflowMarkerInvalid
	}

	var attemptID, closureID, retentionToken string
	var err error
	switch event.EventName + "." + event.Action {
	case "issues.labeled":
		attemptID, err = randomEventUUID()
	case "issues.closed":
		closureID, err = randomEventUUID()
		if err == nil {
			retentionToken, err = randomEventUUID()
		}
	case "issues.reopened", "pull_request.opened", "pull_request.synchronize", "pull_request_review.submitted":
	default:
		return store.WorkflowLocator{}, nil, fmt.Errorf("%w: unsupported normalized event %s.%s", errInvalidPendingNormalizedEvent, event.EventName, event.Action)
	}
	if err != nil {
		return store.WorkflowLocator{}, nil, err
	}

	eventFactory := func(context store.WorkflowEventContext) (workflow.Event, error) {
		metadata := context.Metadata
		switch event.EventName + "." + event.Action {
		case "issues.labeled":
			if event.Issue == nil || event.Label != "omnigrex:run" {
				return nil, fmt.Errorf("%w: invalid issues.labeled event", errInvalidPendingNormalizedEvent)
			}
			return workflow.TriggerEvent{EventMetadata: metadata, AttemptID: attemptID, AttemptNumber: context.Snapshot.LastAttemptNumber + 1}, nil
		case "issues.closed":
			if event.Issue == nil {
				return nil, fmt.Errorf("%w: invalid issues.closed event", errInvalidPendingNormalizedEvent)
			}
			return workflow.IssueClosedEvent{EventMetadata: metadata, ClosureID: closureID, RetainUntil: metadata.ObservedAt.Add(processor.assignmentRetentionDuration), RetentionToken: retentionToken}, nil
		case "issues.reopened":
			return workflow.IssueReopenedEvent{EventMetadata: metadata}, nil
		case "pull_request.opened":
			if event.PullRequest == nil {
				return nil, fmt.Errorf("%w: invalid pull_request.opened event", errInvalidPendingNormalizedEvent)
			}
			return workflow.ChangeProposalObservedEvent{EventMetadata: metadata, ChangeProposal: workflow.ChangeProposal{ID: event.PullRequest.ID, Number: event.PullRequest.Number, HeadSHA: event.PullRequest.HeadSHA, Open: true}}, nil
		case "pull_request.synchronize":
			if event.PullRequest == nil {
				return nil, fmt.Errorf("%w: invalid pull_request.synchronize event", errInvalidPendingNormalizedEvent)
			}
			return workflow.SynchronizationEvent{EventMetadata: metadata, ChangeProposalID: event.PullRequest.ID, PreviousHeadSHA: event.PullRequest.BeforeSHA, HeadSHA: event.PullRequest.HeadSHA}, nil
		case "pull_request_review.submitted":
			if event.PullRequest == nil || event.Review == nil || event.Review.User == nil {
				return nil, fmt.Errorf("%w: invalid pull_request_review.submitted event", errInvalidPendingNormalizedEvent)
			}
			return workflow.ReviewObservedEvent{EventMetadata: metadata, Review: workflow.ReviewIdentity{ID: event.Review.ID, NodeID: event.Review.NodeID, ChangeProposalID: event.PullRequest.ID, ActorID: event.Review.User.ID, HeadSHA: event.Review.CommitID}}, nil
		default:
			return nil, fmt.Errorf("%w: unsupported normalized event %s.%s", errInvalidPendingNormalizedEvent, event.EventName, event.Action)
		}
	}
	return locator, eventFactory, nil
}

func randomEventUUID() (string, error) {
	value, err := uuidtext.NewRandom()
	if err != nil {
		return "", fmt.Errorf("generate Workflow event identity: %w", err)
	}
	return value, nil
}

// Run processes deliveries until the context ends or a durable store operation fails.
func (processor *Processor) Run(ctx context.Context) error {
	for {
		processed, err := processor.ProcessNext(ctx)
		if err != nil {
			if processor.onError != nil {
				processor.onError(err)
			}
			if err := waitForPoll(ctx, processor.idlePollInterval); err != nil {
				return err
			}
			continue
		}
		if processed {
			continue
		}

		if err := waitForPoll(ctx, processor.idlePollInterval); err != nil {
			return err
		}
	}
}

func waitForPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
