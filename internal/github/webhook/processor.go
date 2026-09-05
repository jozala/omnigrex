package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

var errInvalidPendingNormalizedEvent = store.ErrPendingNormalizedEventInvalid

// ProcessorStore is the durable inbox boundary used by Processor.
type ProcessorStore interface {
	ClaimWebhookDelivery(context.Context, string, time.Duration) (*store.WebhookClaim, error)
	CompleteWebhookDelivery(context.Context, string, string, store.WebhookCompletion) error
	CompleteWebhookTransition(context.Context, string, string, json.RawMessage, store.WorkflowLocator, store.WorkflowTransition) (store.WorkflowApplication, error)
	ApplyNextPendingNormalizedEvent(context.Context, store.PendingTransitionFactory) (store.WorkflowApplication, bool, error)
	FailWebhookDelivery(context.Context, string, string, error) error
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
	if _, applied, err := processor.store.ApplyNextPendingNormalizedEvent(ctx, processor.pendingTransition); err != nil {
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
		if failErr := processor.store.FailWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, err); failErr != nil {
			return true, errors.Join(
				fmt.Errorf("normalize webhook delivery %s: %w", claim.DeliveryID, err),
				fmt.Errorf("record webhook delivery %s failure: %w", claim.DeliveryID, failErr),
			)
		}
		return true, nil
	}

	switch normalization.Outcome {
	case NormalizationSupported:
		if normalization.Event == nil {
			return true, errors.New("normalize webhook delivery: supported outcome has no event")
		}
		payload, err := json.Marshal(normalization.Event)
		if err != nil {
			return true, fmt.Errorf("encode normalized webhook delivery %s: %w", claim.DeliveryID, err)
		}
		locator, transition, err := processor.transition(*normalization.Event, claim.ReceivedAt)
		if err != nil {
			return true, fmt.Errorf("map normalized webhook delivery %s: %w", claim.DeliveryID, err)
		}
		if _, err := processor.store.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, payload, locator, transition); err != nil {
			return true, fmt.Errorf("complete webhook transition %s: %w", claim.DeliveryID, err)
		}
	case NormalizationIgnored:
		if normalization.Event != nil {
			return true, errors.New("normalize webhook delivery: ignored outcome has an event")
		}
		if err := processor.store.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{Outcome: store.WebhookOutcomeIgnored}); err != nil {
			return true, fmt.Errorf("complete ignored webhook delivery %s: %w", claim.DeliveryID, err)
		}
	default:
		return true, fmt.Errorf("normalize webhook delivery: invalid outcome %q", normalization.Outcome)
	}
	return true, nil
}

func (processor *Processor) pendingTransition(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
	var event NormalizedEvent
	if err := json.Unmarshal(record.Payload, &event); err != nil {
		return store.WorkflowLocator{}, nil, fmt.Errorf("%w: decode normalized event %s: %v", errInvalidPendingNormalizedEvent, record.DeliveryID, err)
	}
	return processor.transition(event, record.CreatedAt)
}

func (processor *Processor) transition(event NormalizedEvent, observedAt time.Time) (store.WorkflowLocator, store.WorkflowTransition, error) {
	if observedAt.IsZero() {
		return store.WorkflowLocator{}, nil, fmt.Errorf("%w: normalized event observed timestamp is zero", errInvalidPendingNormalizedEvent)
	}
	locator := store.WorkflowLocator{RepositoryID: event.Repository.ID}
	if event.Issue != nil {
		locator.IssueID, locator.IssueNumber = event.Issue.ID, event.Issue.Number
	}
	if event.PullRequest != nil {
		locator.PullRequestID = event.PullRequest.ID
		locator.WorkflowID = event.PullRequest.WorkflowMarkerID
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

	transition := func(snapshot workflow.Snapshot) workflow.Decision {
		workItem := snapshot.WorkItem
		if snapshot.State == workflow.StateAbsent {
			switch {
			case event.Issue != nil:
				workItem = workflow.WorkItem{RepositoryID: event.Repository.ID, IssueID: event.Issue.ID, IssueNumber: event.Issue.Number}
			case event.PullRequest != nil:
				workItem = workflow.WorkItem{RepositoryID: event.Repository.ID, IssueID: event.PullRequest.ID, IssueNumber: event.PullRequest.Number}
			}
		}
		metadata := workflow.EventMetadata{ID: event.DeliveryID, ObservedAt: observedAt, WorkItem: workItem, ExpectedRevision: snapshot.Revision}
		switch event.EventName + "." + event.Action {
		case "issues.labeled":
			if event.Issue == nil || event.Label != "omnigrex:run" {
				return invalidNormalizedDecision(snapshot)
			}
			return workflow.Reduce(snapshot, workflow.TriggerEvent{EventMetadata: metadata, AttemptID: attemptID, AttemptNumber: snapshot.LastAttemptNumber + 1})
		case "issues.closed":
			if event.Issue == nil {
				return invalidNormalizedDecision(snapshot)
			}
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{EventMetadata: metadata, ClosureID: closureID, RetainUntil: observedAt.Add(processor.assignmentRetentionDuration), RetentionToken: retentionToken})
		case "issues.reopened":
			return workflow.Reduce(snapshot, workflow.IssueReopenedEvent{EventMetadata: metadata})
		case "pull_request.opened":
			if event.PullRequest == nil {
				return invalidNormalizedDecision(snapshot)
			}
			return workflow.Reduce(snapshot, workflow.ChangeProposalObservedEvent{EventMetadata: metadata, ChangeProposal: workflow.ChangeProposal{ID: event.PullRequest.ID, Number: event.PullRequest.Number, HeadSHA: event.PullRequest.HeadSHA, Open: true}})
		case "pull_request.synchronize":
			if event.PullRequest == nil {
				return invalidNormalizedDecision(snapshot)
			}
			return workflow.Reduce(snapshot, workflow.SynchronizationEvent{EventMetadata: metadata, ChangeProposalID: event.PullRequest.ID, PreviousHeadSHA: event.PullRequest.BeforeSHA, HeadSHA: event.PullRequest.HeadSHA})
		case "pull_request_review.submitted":
			if event.PullRequest == nil || event.Review == nil || event.Review.User == nil {
				return invalidNormalizedDecision(snapshot)
			}
			return workflow.Reduce(snapshot, workflow.ReviewObservedEvent{EventMetadata: metadata, Review: workflow.ReviewIdentity{ID: event.Review.ID, NodeID: event.Review.NodeID, ChangeProposalID: event.PullRequest.ID, ActorID: event.Review.User.ID, HeadSHA: event.Review.CommitID}})
		default:
			return invalidNormalizedDecision(snapshot)
		}
	}
	return locator, transition, nil
}

func invalidNormalizedDecision(snapshot workflow.Snapshot) workflow.Decision {
	return workflow.Decision{Snapshot: snapshot, Disposition: workflow.DispositionIllegal, Reason: workflow.ReasonInvalidEvent}
}

func randomEventUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate Workflow event identity: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
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
