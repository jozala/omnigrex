package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/telemetry"
	"github.com/jozala/omnigrex/internal/uuidtext"
	"github.com/jozala/omnigrex/internal/workflow"
	"go.opentelemetry.io/otel/attribute"
)

var errInvalidPendingNormalizedEvent = store.ErrPendingNormalizedEventInvalid

const maximumProvisioningRetryDelay = 365 * 24 * time.Hour

// ProcessorStore is the durable inbox boundary used by Processor.
type ProcessorStore interface {
	ClaimWorkflowWebhookDelivery(context.Context, string, time.Duration) (*store.WebhookClaim, error)
	ClaimProvisioningWebhookDelivery(context.Context, string, time.Duration) (*store.WebhookClaim, error)
	CompleteWebhookDelivery(context.Context, string, string, store.WebhookCompletion) error
	CompleteWebhookTransition(context.Context, string, string, json.RawMessage, store.WorkflowLocator, store.WorkflowEventFactory) (store.WorkflowApplication, error)
	CompleteLabelProvisioningTransition(context.Context, string, string, int64, []store.LabelProvisioningRepository, []string, time.Duration) error
	RenewWebhookClaim(context.Context, string, string, time.Duration) error
	ApplyNextPendingNormalizedEvent(context.Context, store.PendingWorkflowEventFactory) (store.WorkflowApplication, bool, error)
	AcknowledgeWebhookDeliveryFailure(context.Context, string, string, int, error, bool) error
	AcknowledgeWebhookDeliveryFailureAfter(context.Context, string, string, int, error, bool, time.Duration) error
}

// InstallationEnumerator lists every repository accessible to one installation.
// It is used only for installation.created, whose webhook repository list may be
// incomplete, and keeps provisioning complete without trusting that list.
type InstallationEnumerator interface {
	EnumerateInstallationRepositories(context.Context, int64) ([]githubapi.InstallationRepository, []string, error)
}

// ProcessorConfig controls claim ownership, lease duration, and idle polling.
type ProcessorConfig struct {
	ClaimOwner                  string
	LeaseDuration               time.Duration
	IdlePollInterval            time.Duration
	RetryDelay                  time.Duration
	AssignmentRetentionDuration time.Duration
	Enumerator                  InstallationEnumerator
	OnError                     func(error)
}

// Processor claims and normalizes durable GitHub webhook deliveries.
type Processor struct {
	store                       ProcessorStore
	claimOwner                  string
	leaseDuration               time.Duration
	idlePollInterval            time.Duration
	retryDelay                  time.Duration
	assignmentRetentionDuration time.Duration
	enumerator                  InstallationEnumerator
	onError                     func(error)
	provisioning                bool
}

// NewProcessor processes Workflow deliveries and pending normalized events.
func NewProcessor(processorStore ProcessorStore, config ProcessorConfig) (*Processor, error) {
	return newProcessor(processorStore, config, false)
}

// NewProvisioningProcessor processes repository onboarding deliveries independently of Workflow events.
func NewProvisioningProcessor(processorStore ProcessorStore, config ProcessorConfig) (*Processor, error) {
	return newProcessor(processorStore, config, true)
}

func newProcessor(processorStore ProcessorStore, config ProcessorConfig, provisioning bool) (*Processor, error) {
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
	if config.RetryDelay <= 0 || config.RetryDelay > maximumProvisioningRetryDelay {
		return nil, errors.New("webhook processor retry delay is out of range")
	}
	if config.AssignmentRetentionDuration <= 0 {
		return nil, errors.New("webhook processor assignment retention duration must be positive")
	}
	return &Processor{
		store:                       processorStore,
		claimOwner:                  config.ClaimOwner,
		leaseDuration:               config.LeaseDuration,
		idlePollInterval:            config.IdlePollInterval,
		retryDelay:                  config.RetryDelay,
		assignmentRetentionDuration: config.AssignmentRetentionDuration,
		enumerator:                  config.Enumerator,
		onError:                     config.OnError,
		provisioning:                provisioning,
	}, nil
}

// ProcessNext durably applies at most one historical event or claimed delivery.
// The returned boolean reports whether work was processed.
func (processor *Processor) ProcessNext(ctx context.Context) (processed bool, err error) {
	if !processor.provisioning {
		if _, applied, err := processor.store.ApplyNextPendingNormalizedEvent(ctx, processor.pendingEventFactory); err != nil {
			return false, fmt.Errorf("apply pending normalized event: %w", err)
		} else if applied {
			return true, nil
		}
	}

	var claim *store.WebhookClaim
	if processor.provisioning {
		claim, err = processor.store.ClaimProvisioningWebhookDelivery(ctx, processor.claimOwner, processor.leaseDuration)
	} else {
		claim, err = processor.store.ClaimWorkflowWebhookDelivery(ctx, processor.claimOwner, processor.leaseDuration)
	}
	if err != nil {
		return false, fmt.Errorf("claim webhook delivery: %w", err)
	}
	if claim == nil {
		return false, nil
	}
	ctx, operation := telemetry.StartOperation(ctx, telemetry.WebhookProcess, attribute.String("delivery_id", claim.DeliveryID))
	defer operation.Finish(&err)

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
		application, err := processor.store.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, payload, locator, eventFactory)
		if err != nil {
			cause := fmt.Errorf("complete webhook transition %s: %w", claim.DeliveryID, err)
			return true, processor.acknowledgeFailure(ctx, claim, cause, !deterministicWebhookFailure(err))
		}
		telemetry.AddAttributes(ctx, attribute.String("workflow_id", application.WorkflowID))
	case NormalizationIgnored:
		telemetry.SetOutcome(ctx, telemetry.DomainOutcome, "")
		if normalization.Event != nil || normalization.Provisioning != nil {
			cause := errors.New("normalize webhook delivery: ignored outcome has an event")
			return true, processor.acknowledgeFailure(ctx, claim, cause, false)
		}
		if err := processor.store.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{Outcome: store.WebhookOutcomeIgnored}); err != nil {
			cause := fmt.Errorf("complete ignored webhook delivery %s: %w", claim.DeliveryID, err)
			return true, processor.acknowledgeFailure(ctx, claim, cause, true)
		}
	case NormalizationProvisioning:
		if normalization.Event != nil || normalization.Provisioning == nil {
			cause := errors.New("normalize webhook delivery: provisioning outcome has no provisioning event")
			return true, processor.acknowledgeFailure(ctx, claim, cause, false)
		}
		if err := processor.completeProvisioning(ctx, claim, *normalization.Provisioning); err != nil {
			cause := fmt.Errorf("complete label provisioning %s: %w", claim.DeliveryID, err)
			return true, processor.acknowledgeProvisioningFailure(ctx, claim, cause)
		}
	default:
		cause := fmt.Errorf("normalize webhook delivery: invalid outcome %q", normalization.Outcome)
		return true, processor.acknowledgeFailure(ctx, claim, cause, false)
	}
	return true, nil
}

func (processor *Processor) completeProvisioning(ctx context.Context, claim *store.WebhookClaim, event ProvisioningEvent) error {
	// Keep the webhook claim live from enumeration through durable job
	// insertion. For a large installation both the page scan and the
	// bounded job-insert batches can outlast one lease window; without
	// renewal a batch would roll back on an expired lease and eventually
	// exhaust the delivery without provisioning anything. Renewal loss
	// aborts the work below. Committed batches are durable before delivery
	// acknowledgement: a retry replays them idempotently through the same
	// per-delivery idempotency keys, converging on one job per repository
	// instead of starting with no queued work.
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	renewDone := make(chan struct{})
	go processor.renewLoop(workCtx, claim, cancelWork, renewDone)

	repositories := make([]store.LabelProvisioningRepository, 0, len(event.Repositories))
	invalid := append([]string{}, event.Invalid...)
	var err error
	if event.EventName == "installation" && event.Action == "created" {
		var enumerated []githubapi.InstallationRepository
		var enumeratedInvalid []string
		enumerated, enumeratedInvalid, err = processor.enumerateInstallation(workCtx, event.InstallationID)
		if err == nil {
			for _, repository := range enumerated {
				if repository.ID <= 0 || strings.TrimSpace(repository.Owner) == "" || strings.TrimSpace(repository.Name) == "" {
					err = fmt.Errorf("%w: enumerated repository identity is invalid", errInvalidPendingNormalizedEvent)
					break
				}
				repositories = append(repositories, store.LabelProvisioningRepository{
					ID: repository.ID, Owner: repository.Owner, Name: repository.Name,
				})
			}
			invalid = append(invalid, enumeratedInvalid...)
		}
	} else {
		for _, repository := range event.Repositories {
			repositories = append(repositories, store.LabelProvisioningRepository{
				ID: repository.ID, Owner: repository.Owner, Name: repository.Name,
			})
		}
	}
	if err == nil {
		err = processor.store.CompleteLabelProvisioningTransition(workCtx, claim.DeliveryID, claim.ClaimToken, event.InstallationID, repositories, invalid, processor.leaseDuration)
	}
	cancelWork()
	<-renewDone
	return err
}

func (processor *Processor) enumerateInstallation(ctx context.Context, installationID int64) ([]githubapi.InstallationRepository, []string, error) {
	if processor.enumerator == nil {
		return nil, nil, fmt.Errorf("%w: installation enumerator is not configured", errInvalidPendingNormalizedEvent)
	}
	return processor.enumerator.EnumerateInstallationRepositories(ctx, installationID)
}

// renewLoop renews the webhook claim immediately and then periodically until
// ctx ends. Any renewal failure cancels the provisioning work: the claim is
// already lost, so enumeration and insertion abort instead of racing expiry.
func (processor *Processor) renewLoop(ctx context.Context, claim *store.WebhookClaim, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	interval := processor.leaseDuration / 3
	if interval < time.Second {
		interval = time.Second
	}
	renew := func() bool {
		if err := processor.store.RenewWebhookClaim(ctx, claim.DeliveryID, claim.ClaimToken, processor.leaseDuration); err != nil {
			cancel()
			return false
		}
		return true
	}
	if !renew() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !renew() {
				return
			}
		}
	}
}

func deterministicProvisioningFailure(err error) bool {
	return errors.Is(err, errInvalidPendingNormalizedEvent) ||
		errors.Is(err, store.ErrLabelProvisioningInvalid) ||
		errors.Is(err, store.ErrNormalizedEventDeliveryMismatch) ||
		errors.Is(err, store.ErrWorkflowLocatorMismatch) ||
		errors.Is(err, store.ErrWorkflowDecisionInvalid)
}

func (processor *Processor) acknowledgeProvisioningFailure(ctx context.Context, claim *store.WebhookClaim, cause error) error {
	telemetry.SetOutcome(ctx, telemetry.Failure, "provisioning_failed")
	metadata := githubapi.ExtractSafeErrorMetadata(cause)
	retryable := !deterministicProvisioningFailure(cause) && !metadata.Permanent && (!metadata.APIClientError || metadata.APIRetryable || metadata.Transient)
	delay := time.Duration(0)
	if retryable {
		delay = processor.retryAfter(claim.AttemptCount, metadata)
	}
	if err := processor.store.AcknowledgeWebhookDeliveryFailureAfter(ctx, claim.DeliveryID, claim.ClaimToken, claim.AttemptCount, cause, retryable, delay); err != nil {
		return errors.Join(cause, fmt.Errorf("acknowledge provisioning delivery %s failure: %w", claim.DeliveryID, err))
	}
	if retryable {
		return cause
	}
	return nil
}

func (processor *Processor) retryAfter(attempt int, metadata githubapi.SafeErrorMetadata) time.Duration {
	delay := processor.retryDelay
	for range max(0, attempt-1) {
		if delay > maximumProvisioningRetryDelay/2 {
			delay = maximumProvisioningRetryDelay
			break
		}
		delay *= 2
	}
	if metadata.RetryAfter > delay {
		delay = metadata.RetryAfter
	}
	if !metadata.ResetAt.IsZero() {
		if untilReset := time.Until(metadata.ResetAt); untilReset > delay {
			delay = untilReset
		}
	}
	return min(delay, maximumProvisioningRetryDelay)
}

func (processor *Processor) acknowledgeFailure(ctx context.Context, claim *store.WebhookClaim, cause error, retryable bool) error {
	telemetry.SetOutcome(ctx, telemetry.Failure, "webhook_processing_failed")
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
