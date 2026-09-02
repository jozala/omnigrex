package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestProcessorAppliesSupportedDeliveryAtomically(t *testing.T) {
	receivedAt := time.Date(2026, time.September, 2, 7, 30, 0, 0, time.UTC)
	inbox := &processorInbox{
		claims: []*store.WebhookClaim{{
			WebhookDelivery: store.WebhookDelivery{
				DeliveryID:      validDeliveryID(),
				EventName:       "issues",
				Action:          "labeled",
				RepositoryID:    9123,
				RepositoryOwner: "jozala",
				RepositoryName:  "omnigrex",
				IssueID:         456,
				IssueNumber:     12,
				Payload:         []byte(`{"action":"labeled","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"issue":{"id":456,"number":12},"label":{"name":"omnigrex:run"}}`),
			},
			ClaimOwner: "processor-a",
			ClaimToken: "323e4567-e89b-12d3-a456-426614174000",
			ReceivedAt: receivedAt,
		}},
		atomicSnapshot: workflow.Snapshot{State: workflow.StateAbsent},
	}
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner:                  "processor-a",
		LeaseDuration:               30 * time.Second,
		IdlePollInterval:            time.Second,
		AssignmentRetentionDuration: 30 * 24 * time.Hour,
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
	if !reflect.DeepEqual(inbox.operations, []string{"drain", "claim", "transition"}) {
		t.Fatalf("operations = %#v, want drain before claim and transition", inbox.operations)
	}
	if len(inbox.transitions) != 1 || len(inbox.completions) != 0 {
		t.Fatalf("transitions/completions = (%#v, %#v), want one atomic transition and no legacy completion", inbox.transitions, inbox.completions)
	}
	var event webhook.NormalizedEvent
	if err := json.Unmarshal(inbox.transitions[0].payload, &event); err != nil {
		t.Fatalf("decode normalized completion: %v", err)
	}
	if event.DeliveryID != validDeliveryID() || event.Issue == nil || event.Issue.ID != 456 {
		t.Errorf("normalized completion = %#v, want delivery and Issue identity", event)
	}
	decision := inbox.transitions[0].decision
	if decision.Disposition != workflow.DispositionApplied || decision.Snapshot.State != workflow.StateDeveloping || decision.Snapshot.Revision != 1 {
		t.Fatalf("trigger decision = %#v, want applied DEVELOPING revision 1", decision)
	}
	if decision.Snapshot.CurrentAttempt == nil || !decision.Snapshot.CurrentAttempt.StartedAt.Equal(receivedAt) || decision.Snapshot.CurrentAttempt.Number != 1 {
		t.Errorf("trigger attempt = %#v, want attempt 1 observed at ReceivedAt", decision.Snapshot.CurrentAttempt)
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
	if len(inbox.transitions) != 0 {
		t.Errorf("atomic transitions = %#v, want none for ignored delivery", inbox.transitions)
	}
	completion := inbox.completions[0]
	if completion.deliveryID != validDeliveryID() || completion.claimToken != "323e4567-e89b-12d3-a456-426614174000" {
		t.Errorf("completion fence = (%q, %q), want claimed delivery and token", completion.deliveryID, completion.claimToken)
	}
	if completion.completion.Outcome != store.WebhookOutcomeIgnored || len(completion.completion.NormalizedPayload) != 0 {
		t.Errorf("completion = %#v, want ignored outcome without normalized payload", completion.completion)
	}
}

func TestProcessorDrainsHistoricalPendingEventBeforeClaimingInbox(t *testing.T) {
	createdAt := time.Date(2026, time.August, 31, 19, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"delivery_id":"223e4567-e89b-12d3-a456-426614174000","event":"issues","action":"labeled","repository":{"id":9123,"owner":"jozala","name":"omnigrex"},"issue":{"id":456,"number":12},"label":"omnigrex:run"}`)
	inbox := &processorInbox{
		pending:       []store.NormalizedEventRecord{{DeliveryID: "223e4567-e89b-12d3-a456-426614174000", Payload: payload, Status: store.NormalizedEventPending, CreatedAt: createdAt}},
		drainSnapshot: workflow.Snapshot{State: workflow.StateAbsent},
	}
	processor := newTestProcessor(t, inbox)

	processed, err := processor.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want historical event processed", processed, err)
	}
	if !reflect.DeepEqual(inbox.operations, []string{"drain"}) || len(inbox.claims) != 0 {
		t.Errorf("operations = %#v, want only historical drain", inbox.operations)
	}
	if len(inbox.drained) != 1 || inbox.drained[0].decision.Snapshot.CurrentAttempt == nil || !inbox.drained[0].decision.Snapshot.CurrentAttempt.StartedAt.Equal(createdAt) {
		t.Errorf("drained transition = %#v, want CreatedAt event semantics", inbox.drained)
	}
}

func TestProcessorMapsIssueClosureFromReceivedAt(t *testing.T) {
	receivedAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	inbox := &processorInbox{
		claims: []*store.WebhookClaim{{
			WebhookDelivery: store.WebhookDelivery{DeliveryID: validDeliveryID(), EventName: "issues", Action: "closed", RepositoryID: 9123, IssueID: 456, IssueNumber: 12,
				Payload: []byte(`{"action":"closed","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"issue":{"id":456,"number":12}}`)},
			ClaimToken: "323e4567-e89b-12d3-a456-426614174000", ReceivedAt: receivedAt,
		}},
		atomicSnapshot: dormantSnapshot(),
	}
	processor := newTestProcessor(t, inbox)

	if processed, err := processor.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want closure applied", processed, err)
	}
	decision := inbox.transitions[0].decision
	wantDeadline := receivedAt.Add(30 * 24 * time.Hour)
	if decision.Disposition != workflow.DispositionApplied || decision.Snapshot.Closure == nil || !decision.Snapshot.Closure.RetainUntil.Equal(wantDeadline) {
		t.Fatalf("closure decision = %#v, want deadline %s", decision, wantDeadline)
	}
	if decision.Snapshot.Closure.ID == "" || decision.Snapshot.Closure.RetentionToken == "" || decision.Snapshot.Closure.ID == decision.Snapshot.Closure.RetentionToken {
		t.Errorf("closure identities = %#v, want independent random identities", decision.Snapshot.Closure)
	}
}

func TestProcessorRetryKeepsWebhookSemanticTimestamp(t *testing.T) {
	receivedAt := time.Date(2026, time.September, 1, 8, 15, 0, 0, time.UTC)
	claim := func() *store.WebhookClaim {
		return &store.WebhookClaim{
			WebhookDelivery: store.WebhookDelivery{DeliveryID: validDeliveryID(), EventName: "issues", Action: "labeled", RepositoryID: 9123, IssueID: 456, IssueNumber: 12,
				Payload: []byte(`{"action":"labeled","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"issue":{"id":456,"number":12},"label":{"name":"omnigrex:run"}}`)},
			ClaimToken: "323e4567-e89b-12d3-a456-426614174000", ReceivedAt: receivedAt,
		}
	}
	inbox := &processorInbox{claims: []*store.WebhookClaim{claim(), claim()}, atomicSnapshot: workflow.Snapshot{State: workflow.StateAbsent}, transitionErr: errors.New("commit interrupted")}
	processor := newTestProcessor(t, inbox)

	for range 2 {
		if processed, err := processor.ProcessNext(context.Background()); !processed || err == nil {
			t.Fatalf("ProcessNext() = (%t, %v), want claimed retry failure", processed, err)
		}
	}
	if len(inbox.transitions) != 2 {
		t.Fatalf("transition attempts = %d, want 2", len(inbox.transitions))
	}
	for index, transition := range inbox.transitions {
		if transition.decision.Snapshot.CurrentAttempt == nil || !transition.decision.Snapshot.CurrentAttempt.StartedAt.Equal(receivedAt) {
			t.Errorf("retry %d StartedAt = %#v, want %s", index+1, transition.decision.Snapshot.CurrentAttempt, receivedAt)
		}
	}
}

func TestProcessorMapsSynchronizationAndReviewAsObservations(t *testing.T) {
	tests := []struct {
		name      string
		eventName string
		payload   []byte
		assert    func(*testing.T, workflow.Decision)
	}{
		{
			name:      "synchronize uses top-level before",
			eventName: "pull_request",
			payload:   []byte(`{"action":"synchronize","before":"old-head","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"pull_request":{"id":654,"number":21,"base":{"ref":"main","sha":"base"},"head":{"ref":"feature","sha":"new-head"}}}`),
			assert: func(t *testing.T, decision workflow.Decision) {
				if decision.Disposition != workflow.DispositionApplied || decision.Snapshot.ChangeProposal == nil || decision.Snapshot.ChangeProposal.HeadSHA != "new-head" {
					t.Errorf("synchronization decision = %#v, want applied new head", decision)
				}
			},
		},
		{
			name:      "review remains deferred without consuming budget",
			eventName: "pull_request_review",
			payload:   []byte(`{"action":"submitted","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"pull_request":{"id":654,"number":21,"base":{"ref":"main","sha":"base"},"head":{"ref":"feature","sha":"old-head"}},"review":{"id":987,"node_id":"PRR_node","state":"changes_requested","commit_id":"old-head","user":{"id":77,"login":"reviewer"}}}`),
			assert: func(t *testing.T, decision workflow.Decision) {
				if decision.Disposition != workflow.DispositionDeferred || decision.Snapshot.Revision != 4 || decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 1 {
					t.Errorf("review decision = %#v, want deferred at unchanged revision and budget", decision)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := reviewingSnapshot()
			if test.eventName == "pull_request_review" {
				snapshot.ActiveTurn = &workflow.ActiveTurn{
					ID: "turn", SessionID: "session", AttemptID: snapshot.CurrentAttempt.ID,
					Role: workflow.RoleReviewer, Epoch: 1, ControlRevision: 1,
					ChangeProposalID: snapshot.ChangeProposal.ID, ExpectedHeadSHA: snapshot.ChangeProposal.HeadSHA,
				}
			}
			inbox := &processorInbox{
				claims: []*store.WebhookClaim{{
					WebhookDelivery: store.WebhookDelivery{DeliveryID: validDeliveryID(), EventName: test.eventName, Action: map[string]string{"pull_request": "synchronize", "pull_request_review": "submitted"}[test.eventName], RepositoryID: 9123, Payload: test.payload},
					ClaimToken:      "323e4567-e89b-12d3-a456-426614174000", ReceivedAt: time.Date(2026, time.September, 2, 9, 0, 0, 0, time.UTC),
				}},
				atomicSnapshot: snapshot,
			}
			processor := newTestProcessor(t, inbox)
			if processed, err := processor.ProcessNext(context.Background()); err != nil || !processed {
				t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
			}
			test.assert(t, inbox.transitions[0].decision)
		})
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
	valid := webhook.ProcessorConfig{ClaimOwner: "processor", LeaseDuration: 30 * time.Second, IdlePollInterval: time.Second, AssignmentRetentionDuration: 30 * 24 * time.Hour}
	tests := []struct {
		name   string
		store  webhook.ProcessorStore
		config webhook.ProcessorConfig
	}{
		{name: "nil store", config: valid},
		{name: "empty owner", store: &processorInbox{}, config: webhook.ProcessorConfig{LeaseDuration: time.Second, IdlePollInterval: time.Second}},
		{name: "short lease", store: &processorInbox{}, config: webhook.ProcessorConfig{ClaimOwner: "processor", LeaseDuration: time.Second, IdlePollInterval: time.Second}},
		{name: "zero idle poll", store: &processorInbox{}, config: webhook.ProcessorConfig{ClaimOwner: "processor", LeaseDuration: time.Second}},
		{name: "zero assignment retention", store: &processorInbox{}, config: webhook.ProcessorConfig{ClaimOwner: "processor", LeaseDuration: 30 * time.Second, IdlePollInterval: time.Second}},
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
		ClaimOwner:                  "processor",
		LeaseDuration:               30 * time.Second,
		IdlePollInterval:            time.Millisecond,
		AssignmentRetentionDuration: 30 * 24 * time.Hour,
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
		ClaimOwner:                  "processor",
		LeaseDuration:               30 * time.Second,
		IdlePollInterval:            time.Millisecond,
		AssignmentRetentionDuration: 30 * 24 * time.Hour,
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
	claims         []*store.WebhookClaim
	claimOwner     string
	claimLease     time.Duration
	claimErr       error
	completions    []recordedCompletion
	completeErr    error
	failures       []recordedFailure
	failErr        error
	pending        []store.NormalizedEventRecord
	drained        []recordedTransition
	transitions    []recordedTransition
	atomicSnapshot workflow.Snapshot
	drainSnapshot  workflow.Snapshot
	transitionErr  error
	drainErr       error
	operations     []string
}

type recordedTransition struct {
	deliveryID string
	payload    json.RawMessage
	locator    store.WorkflowLocator
	decision   workflow.Decision
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
	inbox.operations = append(inbox.operations, "claim")
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

func (inbox *processorInbox) ApplyNextPendingNormalizedEvent(_ context.Context, factory store.PendingTransitionFactory) (store.WorkflowApplication, bool, error) {
	inbox.operations = append(inbox.operations, "drain")
	if inbox.drainErr != nil {
		return store.WorkflowApplication{}, false, inbox.drainErr
	}
	if len(inbox.pending) == 0 {
		return store.WorkflowApplication{}, false, nil
	}
	record := inbox.pending[0]
	inbox.pending = inbox.pending[1:]
	locator, transition, err := factory(record)
	if err != nil {
		return store.WorkflowApplication{}, false, err
	}
	decision := transition(inbox.drainSnapshot)
	inbox.drained = append(inbox.drained, recordedTransition{deliveryID: record.DeliveryID, payload: record.Payload, locator: locator, decision: decision})
	return store.WorkflowApplication{DeliveryID: record.DeliveryID}, true, nil
}

func (inbox *processorInbox) CompleteWebhookTransition(_ context.Context, deliveryID, _ string, payload json.RawMessage, locator store.WorkflowLocator, transition store.WorkflowTransition) (store.WorkflowApplication, error) {
	inbox.operations = append(inbox.operations, "transition")
	decision := transition(inbox.atomicSnapshot)
	inbox.transitions = append(inbox.transitions, recordedTransition{deliveryID: deliveryID, payload: payload, locator: locator, decision: decision})
	return store.WorkflowApplication{DeliveryID: deliveryID}, inbox.transitionErr
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

func (*retryingInbox) ApplyNextPendingNormalizedEvent(context.Context, store.PendingTransitionFactory) (store.WorkflowApplication, bool, error) {
	return store.WorkflowApplication{}, false, nil
}

func (*retryingInbox) CompleteWebhookTransition(context.Context, string, string, json.RawMessage, store.WorkflowLocator, store.WorkflowTransition) (store.WorkflowApplication, error) {
	return store.WorkflowApplication{}, nil
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

func (*pollingInbox) ApplyNextPendingNormalizedEvent(context.Context, store.PendingTransitionFactory) (store.WorkflowApplication, bool, error) {
	return store.WorkflowApplication{}, false, nil
}

func (*pollingInbox) CompleteWebhookTransition(context.Context, string, string, json.RawMessage, store.WorkflowLocator, store.WorkflowTransition) (store.WorkflowApplication, error) {
	return store.WorkflowApplication{}, nil
}

func (*pollingInbox) FailWebhookDelivery(context.Context, string, string, error) error {
	return nil
}

func newTestProcessor(t *testing.T, inbox webhook.ProcessorStore) *webhook.Processor {
	t.Helper()
	processor, err := webhook.NewProcessor(inbox, webhook.ProcessorConfig{
		ClaimOwner:                  "processor-a",
		LeaseDuration:               30 * time.Second,
		IdlePollInterval:            time.Second,
		AssignmentRetentionDuration: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewProcessor() error = %v", err)
	}
	return processor
}

func dormantSnapshot() workflow.Snapshot {
	return workflow.Snapshot{
		State:             workflow.StateDormant,
		Revision:          3,
		WorkItem:          workflow.WorkItem{RepositoryID: 9123, IssueID: 456, IssueNumber: 12},
		Assignments:       workflow.Assignments{Status: workflow.AssignmentCompleted, RuntimeState: workflow.RuntimeStateRetained},
		LastAttemptNumber: 1,
	}
}

func reviewingSnapshot() workflow.Snapshot {
	return workflow.Snapshot{
		State:    workflow.StateReviewing,
		Revision: 4,
		WorkItem: workflow.WorkItem{RepositoryID: 9123, IssueID: 456, IssueNumber: 12},
		CurrentAttempt: &workflow.WorkflowAttempt{
			ID: "50000000-0000-4000-8000-000000000001", Number: 1,
			StartedAt: time.Date(2026, time.August, 30, 10, 0, 0, 0, time.UTC), Lifecycle: workflow.AttemptActive,
			ReviewBudget: workflow.Budget{Used: 1, Limit: 3}, InfrastructureRetryBudget: workflow.Budget{Limit: 1},
		},
		ChangeProposal:    &workflow.ChangeProposal{ID: 654, Number: 21, HeadSHA: "old-head", Open: true},
		Assignments:       workflow.Assignments{Status: workflow.AssignmentActive, RuntimeState: workflow.RuntimeStateActive},
		LastAttemptNumber: 1,
	}
}
