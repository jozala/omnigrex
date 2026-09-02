package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

// ProcessorStore is the durable inbox boundary used by Processor.
type ProcessorStore interface {
	ClaimWebhookDelivery(context.Context, string, time.Duration) (*store.WebhookClaim, error)
	CompleteWebhookDelivery(context.Context, string, string, store.WebhookCompletion) error
	FailWebhookDelivery(context.Context, string, string, error) error
}

// ProcessorConfig controls claim ownership, lease duration, and idle polling.
type ProcessorConfig struct {
	ClaimOwner       string
	LeaseDuration    time.Duration
	IdlePollInterval time.Duration
	OnError          func(error)
}

// Processor claims and normalizes durable GitHub webhook deliveries.
type Processor struct {
	store            ProcessorStore
	claimOwner       string
	leaseDuration    time.Duration
	idlePollInterval time.Duration
	onError          func(error)
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
	return &Processor{
		store:            processorStore,
		claimOwner:       config.ClaimOwner,
		leaseDuration:    config.LeaseDuration,
		idlePollInterval: config.IdlePollInterval,
		onError:          config.OnError,
	}, nil
}

// ProcessNext claims and durably records the outcome of at most one delivery.
// The returned boolean reports whether a delivery was claimed.
func (processor *Processor) ProcessNext(ctx context.Context) (bool, error) {
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

	completion := store.WebhookCompletion{Outcome: store.WebhookOutcomeIgnored}
	switch normalization.Outcome {
	case NormalizationSupported:
		if normalization.Event == nil {
			return true, errors.New("normalize webhook delivery: supported outcome has no event")
		}
		payload, err := json.Marshal(normalization.Event)
		if err != nil {
			return true, fmt.Errorf("encode normalized webhook delivery %s: %w", claim.DeliveryID, err)
		}
		completion = store.WebhookCompletion{
			Outcome:           store.WebhookOutcomeProcessed,
			NormalizedPayload: payload,
		}
	case NormalizationIgnored:
		if normalization.Event != nil {
			return true, errors.New("normalize webhook delivery: ignored outcome has an event")
		}
	default:
		return true, fmt.Errorf("normalize webhook delivery: invalid outcome %q", normalization.Outcome)
	}
	if err := processor.store.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, completion); err != nil {
		return true, fmt.Errorf("complete webhook delivery %s: %w", claim.DeliveryID, err)
	}
	return true, nil
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
