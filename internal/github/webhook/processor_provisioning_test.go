package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/store"
)

type fakeEnumerator struct {
	repositories []githubapi.InstallationRepository
	invalid      []string
	err          error
	calls        int
	lastID       int64
}

func (fake *fakeEnumerator) EnumerateInstallationRepositories(_ context.Context, installationID int64) ([]githubapi.InstallationRepository, []string, error) {
	fake.calls++
	fake.lastID = installationID
	return fake.repositories, fake.invalid, fake.err
}

func provisioningClaim(eventName, action, payload string) *store.WebhookClaim {
	return &store.WebhookClaim{
		WebhookDelivery: store.WebhookDelivery{
			DeliveryID: validDeliveryID(), EventName: eventName, Action: action, Payload: []byte(payload),
		},
		ClaimToken: "323e4567-e89b-12d3-a456-426614174000", AttemptCount: 1,
	}
}

func TestProcessorProvisionsAddedRepositoriesWithoutEnumeration(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{provisioningClaim("installation_repositories", "added",
		`{"action":"added","installation":{"id":99},"repositories_added":[{"id":9123,"name":"omnigrex","full_name":"jozala/omnigrex"}]}`)}}
	enumerator := &fakeEnumerator{}
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner: "processor-a", LeaseDuration: 30 * time.Second, IdlePollInterval: time.Second,
		AssignmentRetentionDuration: 30 * 24 * time.Hour, Enumerator: enumerator,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	processed, err := processor.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want provisioning processed", processed, err)
	}
	if enumerator.calls != 0 {
		t.Errorf("enumerator calls = %d, want 0 for webhook-listed repositories", enumerator.calls)
	}
	if len(inbox.provisioned) != 1 {
		t.Fatalf("provisioned = %#v, want one transition", inbox.provisioned)
	}
	transition := inbox.provisioned[0]
	if transition.installationID != 99 || len(transition.repositories) != 1 ||
		transition.repositories[0].ID != 9123 || transition.repositories[0].Owner != "jozala" {
		t.Errorf("provisioning transition = %#v, want repository 9123 jozala/omnigrex", transition)
	}
	if len(inbox.transitions) != 0 || len(inbox.completions) != 0 || len(inbox.failures) != 0 {
		t.Errorf("workflow work = (transitions %d, completions %d, failures %d), want none", len(inbox.transitions), len(inbox.completions), len(inbox.failures))
	}
}

func TestProcessorEnumeratesNewInstallationInsteadOfTrustingWebhookList(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{provisioningClaim("installation", "created",
		`{"action":"created","installation":{"id":99},"repositories":[{"id":1,"name":"stale","full_name":"acme/stale"}]}`)}}
	enumerator := &fakeEnumerator{repositories: []githubapi.InstallationRepository{
		{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		{ID: 9124, Owner: "jozala", Name: "widgets"},
	}}
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner: "processor-a", LeaseDuration: 30 * time.Second, IdlePollInterval: time.Second,
		AssignmentRetentionDuration: 30 * 24 * time.Hour, Enumerator: enumerator,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	processed, err := processor.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want enumerated provisioning", processed, err)
	}
	if enumerator.calls != 1 || enumerator.lastID != 99 {
		t.Errorf("enumerator = (%d calls, last %d), want one call for installation 99", enumerator.calls, enumerator.lastID)
	}
	if len(inbox.provisioned) != 1 || len(inbox.provisioned[0].repositories) != 2 {
		t.Fatalf("provisioned = %#v, want two enumerated repositories", inbox.provisioned)
	}
}

func TestProcessorRetriesTransientEnumerationFailure(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{provisioningClaim("installation", "created",
		`{"action":"created","installation":{"id":99}}`)}}
	enumerator := &fakeEnumerator{err: &githubapi.TransientError{Cause: errors.New("unavailable")}}
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner: "processor-a", LeaseDuration: 30 * time.Second, IdlePollInterval: time.Second,
		AssignmentRetentionDuration: 30 * 24 * time.Hour, Enumerator: enumerator,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	processed, err := processor.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want retryable enumeration failure", processed, err)
	}
	if len(inbox.failures) != 1 || !inbox.failures[0].retryable {
		t.Errorf("failures = %#v, want one retryable failure", inbox.failures)
	}
	if len(inbox.provisioned) != 0 {
		t.Errorf("provisioned = %#v, want none after enumeration failure", inbox.provisioned)
	}
}

func TestProcessorTerminallyFailsMalformedProvisioning(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{provisioningClaim("repository", "created",
		`{"action":"created","installation":{"id":99}}`)}}
	processor := newTestProcessor(t, inbox)

	processed, err := processor.ProcessNext(context.Background())
	if !processed || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want terminally handled malformed delivery", processed, err)
	}
	if len(inbox.failures) != 1 || inbox.failures[0].retryable {
		t.Errorf("failures = %#v, want one terminal failure", inbox.failures)
	}
	if !errors.Is(inbox.failures[0].cause, webhook.ErrMalformedPayload) {
		t.Errorf("failure cause = %v, want ErrMalformedPayload", inbox.failures[0].cause)
	}
}

func TestProcessorProvisioningNeverCreatesWorkflowTransition(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{provisioningClaim("repository", "created",
		`{"action":"created","installation":{"id":99},"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}}}`)}}
	processor := newTestProcessor(t, inbox)

	processed, err := processor.ProcessNext(context.Background())
	if !processed || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want provisioning processed", processed, err)
	}
	if len(inbox.transitions) != 0 {
		t.Errorf("transitions = %#v, want none for provisioning", inbox.transitions)
	}
	if len(inbox.provisioned) != 1 {
		t.Fatalf("provisioned = %#v, want one provisioning transition", inbox.provisioned)
	}
}

func TestProcessorQueuesValidRepositoriesWhileReportingInvalid(t *testing.T) {
	inbox := &processorInbox{claims: []*store.WebhookClaim{provisioningClaim("installation_repositories", "added",
		`{"action":"added","installation":{"id":99},"repositories_added":[
			{"id":9123,"name":"omnigrex","full_name":"jozala/omnigrex"},
			{"id":0,"name":"","full_name":""}]}`)}}
	processor := newTestProcessor(t, inbox)

	processed, err := processor.ProcessNext(context.Background())
	if !processed || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want mixed provisioning processed", processed, err)
	}
	if len(inbox.provisioned) != 1 {
		t.Fatalf("provisioned = %#v, want one provisioning transition", inbox.provisioned)
	}
	transition := inbox.provisioned[0]
	if len(transition.repositories) != 1 || transition.repositories[0].ID != 9123 {
		t.Errorf("repositories = %#v, want only the valid entry", transition.repositories)
	}
	if len(transition.invalid) != 1 {
		t.Errorf("invalid = %#v, want the malformed entry reported", transition.invalid)
	}
	if len(inbox.failures) != 0 {
		t.Errorf("failures = %#v, want none: invalid entries travel with the transition", inbox.failures)
	}
}

func TestProcessorRenewsClaimDuringSlowEnumeration(t *testing.T) {
	release := make(chan struct{})
	enumerator := &blockingEnumerator{
		release: release,
		repositories: []githubapi.InstallationRepository{
			{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		},
	}
	inbox := &processorInbox{claims: []*store.WebhookClaim{provisioningClaim("installation", "created",
		`{"action":"created","installation":{"id":99}}`)}}
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner: "processor-a", LeaseDuration: 5 * time.Second, IdlePollInterval: time.Second,
		AssignmentRetentionDuration: 30 * 24 * time.Hour, Enumerator: enumerator,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	processed := make(chan error, 1)
	go func() {
		_, err := processor.ProcessNext(context.Background())
		processed <- err
	}()
	deadline := time.After(10 * time.Second)
	for {
		enumerator.mu.Lock()
		started := enumerator.started
		enumerator.mu.Unlock()
		if started {
			break
		}
		select {
		case <-deadline:
			t.Fatal("enumeration did not start")
		case <-time.After(time.Millisecond):
		}
	}
	// Hold enumeration past the background renewal interval (lease/3) so the
	// slow path is exercised: the claim must be renewed while no repository
	// jobs exist yet.
	time.Sleep(4 * time.Second)
	close(release)
	select {
	case err := <-processed:
		if err != nil {
			t.Fatalf("ProcessNext() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ProcessNext() did not complete after enumeration finished")
	}
	if len(inbox.renewals) < 2 {
		t.Errorf("renewals = %d, want the pre-enumeration renewal plus background renewals", len(inbox.renewals))
	}
	if len(inbox.provisioned) != 1 || len(inbox.provisioned[0].repositories) != 1 {
		t.Errorf("provisioned = %#v, want one enumerated repository", inbox.provisioned)
	}
}

type blockingEnumerator struct {
	mu           sync.Mutex
	started      bool
	release      chan struct{}
	repositories []githubapi.InstallationRepository
}

func (fake *blockingEnumerator) EnumerateInstallationRepositories(ctx context.Context, _ int64) ([]githubapi.InstallationRepository, []string, error) {
	fake.mu.Lock()
	fake.started = true
	fake.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-fake.release:
		return fake.repositories, nil, nil
	}
}

func TestProcessorKeepsClaimLiveThroughSlowCompletion(t *testing.T) {
	durable := &slowCompletionStore{
		claim: &store.WebhookClaim{
			WebhookDelivery: store.WebhookDelivery{
				DeliveryID: validDeliveryID(), EventName: "repository", Action: "created",
				Payload: []byte(`{"action":"created","installation":{"id":99},"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}}}`),
			},
			ClaimToken: "323e4567-e89b-12d3-a456-426614174000", AttemptCount: 1,
		},
		release: make(chan struct{}),
	}
	processor, err := webhook.NewProcessor(durable, webhook.ProcessorConfig{
		ClaimOwner: "processor-a", LeaseDuration: 5 * time.Second, IdlePollInterval: time.Second,
		AssignmentRetentionDuration: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}

	processed := make(chan error, 1)
	go func() {
		ok, err := processor.ProcessNext(context.Background())
		if err == nil && !ok {
			err = errors.New("ProcessNext() processed nothing")
		}
		processed <- err
	}()
	// Hold the job-insert transaction past the background renewal interval
	// (lease/3) so renewals must fire while insertion is still blocked.
	deadline := time.After(15 * time.Second)
	for {
		durable.mu.Lock()
		renewals := durable.renewals
		durable.mu.Unlock()
		if renewals >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d renewals while completion blocked, want at least 3", renewals)
		case <-time.After(50 * time.Millisecond):
		}
	}
	close(durable.release)
	select {
	case err := <-processed:
		if err != nil {
			t.Fatalf("ProcessNext() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ProcessNext() did not complete after insertion finished")
	}
	durable.mu.Lock()
	defer durable.mu.Unlock()
	if !durable.provisioned {
		t.Errorf("provisioned = false, want the blocked transition to complete")
	}
}

// slowCompletionStore blocks the provisioning transition until released,
// proving claim renewal continues through durable job insertion.
type slowCompletionStore struct {
	mu          sync.Mutex
	claim       *store.WebhookClaim
	renewals    int
	release     chan struct{}
	provisioned bool
}

func (durable *slowCompletionStore) ClaimWebhookDelivery(context.Context, string, time.Duration) (*store.WebhookClaim, error) {
	return durable.claim, nil
}

func (durable *slowCompletionStore) RenewWebhookClaim(_ context.Context, _, _ string, _ time.Duration) error {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	durable.renewals++
	return nil
}

func (durable *slowCompletionStore) CompleteLabelProvisioningTransition(ctx context.Context, _, _ string, _ int64, _ []store.LabelProvisioningRepository, _ []string, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-durable.release:
		durable.mu.Lock()
		defer durable.mu.Unlock()
		durable.provisioned = true
		return nil
	}
}

func (*slowCompletionStore) CompleteWebhookDelivery(context.Context, string, string, store.WebhookCompletion) error {
	return nil
}

func (*slowCompletionStore) CompleteWebhookTransition(context.Context, string, string, json.RawMessage, store.WorkflowLocator, store.WorkflowEventFactory) (store.WorkflowApplication, error) {
	return store.WorkflowApplication{}, nil
}

func (*slowCompletionStore) ApplyNextPendingNormalizedEvent(context.Context, store.PendingWorkflowEventFactory) (store.WorkflowApplication, bool, error) {
	return store.WorkflowApplication{}, false, nil
}

func (*slowCompletionStore) AcknowledgeWebhookDeliveryFailure(context.Context, string, string, int, error, bool) error {
	return nil
}
