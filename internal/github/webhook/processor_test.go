package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/store"
)

func TestProcessorClaimsNormalizesAndRecordsSupportedDelivery(t *testing.T) {
	inbox := &processorInbox{
		claims: []*store.WebhookClaim{{
			WebhookDelivery: store.WebhookDelivery{
				DeliveryID:      validDeliveryID(),
				EventName:       "issues",
				Action:          "closed",
				RepositoryID:    9123,
				RepositoryOwner: "jozala",
				RepositoryName:  "omnigrex",
				Payload:         []byte(`{"action":"closed","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"issue":{"id":456,"number":12}}`),
			},
			ClaimOwner: "processor-a",
			ClaimToken: "323e4567-e89b-12d3-a456-426614174000",
		}},
	}
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner:       "processor-a",
		LeaseDuration:    30 * time.Second,
		IdlePollInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	processed, err := processor.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if !processed {
		t.Fatal("ProcessNext() processed = false, want true")
	}
	if inbox.claimOwner != "processor-a" || inbox.claimLease != 30*time.Second {
		t.Errorf("claim arguments = (%q, %v), want (processor-a, 30s)", inbox.claimOwner, inbox.claimLease)
	}
	if len(inbox.completions) != 1 || inbox.completions[0].completion.Outcome != store.WebhookOutcomeProcessed {
		t.Fatalf("completions = %#v, want one processed completion", inbox.completions)
	}
	var event webhook.NormalizedEvent
	if err := json.Unmarshal(inbox.completions[0].completion.NormalizedPayload, &event); err != nil {
		t.Fatalf("decode normalized completion: %v", err)
	}
	if event.DeliveryID != validDeliveryID() || event.Issue == nil || event.Issue.ID != 456 {
		t.Errorf("normalized completion = %#v, want delivery and Issue identity", event)
	}
	if len(inbox.failures) != 0 {
		t.Errorf("failures = %#v, want none", inbox.failures)
	}
}

func TestProcessorRecordsIgnoredDeliveryWithoutNormalizedPayload(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{{
		WebhookDelivery: store.WebhookDelivery{
			DeliveryID: validDeliveryID(),
			EventName:  "push",
			Payload:    []byte(`{"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}}}`),
		},
		ClaimToken: "323e4567-e89b-12d3-a456-426614174000",
	}}}
	processor := newTestProcessor(t, inbox)

	processed, err := processor.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if !processed {
		t.Fatal("ProcessNext() processed = false, want true")
	}
	if len(inbox.completions) != 1 {
		t.Fatalf("completions = %#v, want one", inbox.completions)
	}
	completion := inbox.completions[0]
	if completion.deliveryID != validDeliveryID() || completion.claimToken != "323e4567-e89b-12d3-a456-426614174000" {
		t.Errorf("completion fence = (%q, %q), want claimed delivery and token", completion.deliveryID, completion.claimToken)
	}
	if completion.completion.Outcome != store.WebhookOutcomeIgnored || len(completion.completion.NormalizedPayload) != 0 {
		t.Errorf("completion = %#v, want ignored outcome without normalized payload", completion.completion)
	}
}

func TestProcessorRecordsMalformedClaimAsFailedAndContinues(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{{
		WebhookDelivery: store.WebhookDelivery{
			DeliveryID: validDeliveryID(),
			EventName:  "issues",
			Action:     "closed",
			Payload:    []byte(`{`),
		},
		ClaimToken: "323e4567-e89b-12d3-a456-426614174000",
	}}}
	processor := newTestProcessor(t, inbox)

	processed, err := processor.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext() error = %v, want durably handled malformed delivery", err)
	}
	if !processed {
		t.Fatal("ProcessNext() processed = false, want true")
	}
	if len(inbox.completions) != 0 {
		t.Errorf("completions = %#v, want none", inbox.completions)
	}
	if len(inbox.failures) != 1 {
		t.Fatalf("failures = %#v, want one", inbox.failures)
	}
	failure := inbox.failures[0]
	if failure.deliveryID != validDeliveryID() || failure.claimToken != "323e4567-e89b-12d3-a456-426614174000" {
		t.Errorf("failure fence = (%q, %q), want claimed delivery and token", failure.deliveryID, failure.claimToken)
	}
	if !errors.Is(failure.cause, webhook.ErrMalformedPayload) {
		t.Errorf("failure cause = %v, want ErrMalformedPayload", failure.cause)
	}
}

func TestProcessorReturnsDurableOperationErrors(t *testing.T) {
	claimError := errors.New("database unavailable")
	processor := newTestProcessor(t, &processorInbox{claimErr: claimError})
	processed, err := processor.ProcessNext(context.Background())
	if processed || !errors.Is(err, claimError) {
		t.Errorf("ProcessNext() = (%v, %v), want false and claim error", processed, err)
	}

	failError := errors.New("failure fence lost")
	inbox := &processorInbox{
		claims: []*store.WebhookClaim{{
			WebhookDelivery: store.WebhookDelivery{DeliveryID: validDeliveryID(), EventName: "issues", Action: "closed", Payload: []byte(`{`)},
			ClaimToken:      "323e4567-e89b-12d3-a456-426614174000",
		}},
		failErr: failError,
	}
	processor = newTestProcessor(t, inbox)
	processed, err = processor.ProcessNext(context.Background())
	if !processed || !errors.Is(err, failError) || !errors.Is(err, webhook.ErrMalformedPayload) {
		t.Errorf("ProcessNext() = (%v, %v), want handled claim with normalization and failure errors", processed, err)
	}
}

func TestNewProcessorValidatesConfig(t *testing.T) {
	valid := webhook.ProcessorConfig{ClaimOwner: "processor", LeaseDuration: 30 * time.Second, IdlePollInterval: time.Second}
	tests := []struct {
		name   string
		store  webhook.ProcessorStore
		config webhook.ProcessorConfig
	}{
		{name: "nil store", config: valid},
		{name: "empty owner", store: &processorInbox{}, config: webhook.ProcessorConfig{LeaseDuration: time.Second, IdlePollInterval: time.Second}},
		{name: "short lease", store: &processorInbox{}, config: webhook.ProcessorConfig{ClaimOwner: "processor", LeaseDuration: time.Second, IdlePollInterval: time.Second}},
		{name: "zero idle poll", store: &processorInbox{}, config: webhook.ProcessorConfig{ClaimOwner: "processor", LeaseDuration: time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := webhook.NewProcessor(test.store, test.config); err == nil {
				t.Error("NewProcessor() error = nil, want invalid config error")
			}
		})
	}
}

func TestProcessorRunUsesBoundedIdlePollingUntilContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inbox := &pollingInbox{cancel: cancel}
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner:       "processor",
		LeaseDuration:    30 * time.Second,
		IdlePollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	err = processor.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() error = %v, want context.Canceled", err)
	}
	if calls := inbox.calls.Load(); calls != 3 {
		t.Errorf("claim calls = %d, want 3 bounded polls", calls)
	}
}

func TestProcessorRunReportsAndRetriesOperationFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	operationErr := errors.New("database unavailable")
	inbox := &retryingInbox{operationErr: operationErr, cancel: cancel}
	var reported atomic.Int32
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner:       "processor",
		LeaseDuration:    30 * time.Second,
		IdlePollInterval: time.Millisecond,
		OnError: func(err error) {
			if !errors.Is(err, operationErr) {
				t.Errorf("reported error = %v, want database unavailable", err)
			}
			reported.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	if err := processor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run() error = %v, want context.Canceled", err)
	}
	if inbox.calls.Load() != 3 || reported.Load() != 2 {
		t.Errorf("retry calls/reports = (%d, %d), want (3, 2)", inbox.calls.Load(), reported.Load())
	}
}

type processorInbox struct {
	claims      []*store.WebhookClaim
	claimOwner  string
	claimLease  time.Duration
	claimErr    error
	completions []recordedCompletion
	completeErr error
	failures    []recordedFailure
	failErr     error
}

type recordedCompletion struct {
	deliveryID string
	claimToken string
	completion store.WebhookCompletion
}

type recordedFailure struct {
	deliveryID string
	claimToken string
	cause      error
}

func (inbox *processorInbox) ClaimWebhookDelivery(_ context.Context, owner string, lease time.Duration) (*store.WebhookClaim, error) {
	inbox.claimOwner = owner
	inbox.claimLease = lease
	if inbox.claimErr != nil {
		return nil, inbox.claimErr
	}
	if len(inbox.claims) == 0 {
		return nil, nil
	}
	claim := inbox.claims[0]
	inbox.claims = inbox.claims[1:]
	return claim, nil
}

func (inbox *processorInbox) CompleteWebhookDelivery(_ context.Context, deliveryID, claimToken string, completion store.WebhookCompletion) error {
	inbox.completions = append(inbox.completions, recordedCompletion{deliveryID: deliveryID, claimToken: claimToken, completion: completion})
	return inbox.completeErr
}

func (inbox *processorInbox) FailWebhookDelivery(_ context.Context, deliveryID, claimToken string, cause error) error {
	inbox.failures = append(inbox.failures, recordedFailure{deliveryID: deliveryID, claimToken: claimToken, cause: cause})
	return inbox.failErr
}

type pollingInbox struct {
	calls  atomic.Int32
	cancel context.CancelFunc
}

type retryingInbox struct {
	calls        atomic.Int32
	operationErr error
	cancel       context.CancelFunc
}

func (inbox *retryingInbox) ClaimWebhookDelivery(context.Context, string, time.Duration) (*store.WebhookClaim, error) {
	if inbox.calls.Add(1) == 3 {
		inbox.cancel()
		return nil, nil
	}
	return nil, inbox.operationErr
}

func (*retryingInbox) CompleteWebhookDelivery(context.Context, string, string, store.WebhookCompletion) error {
	return nil
}

func (*retryingInbox) FailWebhookDelivery(context.Context, string, string, error) error {
	return nil
}

func (inbox *pollingInbox) ClaimWebhookDelivery(_ context.Context, _ string, _ time.Duration) (*store.WebhookClaim, error) {
	if inbox.calls.Add(1) == 3 {
		inbox.cancel()
	}
	return nil, nil
}

func (*pollingInbox) CompleteWebhookDelivery(context.Context, string, string, store.WebhookCompletion) error {
	return nil
}

func (*pollingInbox) FailWebhookDelivery(context.Context, string, string, error) error {
	return nil
}

func newTestProcessor(t *testing.T, inbox webhook.ProcessorStore) *webhook.Processor {
	t.Helper()
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner:       "processor-a",
		LeaseDuration:    30 * time.Second,
		IdlePollInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}
	return processor
}
