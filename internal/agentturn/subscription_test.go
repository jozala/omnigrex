package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/providerauth"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestSubscriptionPrerequisiteFailuresAreDurableHandoffOutcomes(t *testing.T) {
	for _, class := range []agentturn.PromptErrorClassification{agentturn.PromptErrorSubscriptionQuota, agentturn.PromptErrorSubscriptionAuth, agentturn.PromptErrorSubscriptionConfiguration} {
		request := outcomeRequest(t, workflow.RoleDeveloper, true)
		request.PromptResponse, request.PromptError, request.PromptDiagnostic = nil, class, "untrusted-provider-text"
		observation, err := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(context.Background(), request)
		if err != nil || observation.Outcome != workflow.TurnOutcomePrerequisiteFailed || observation.Completion.Status != store.AgentTurnFailed || observation.Diagnostic == "" || strings.Contains(observation.Diagnostic, "untrusted") {
			t.Fatalf("prerequisite outcome = %+v, %v", observation, err)
		}
	}
}

func TestSubscriptionFailureDoesNotInvalidateCompletedTerminalMutation(t *testing.T) {
	for _, class := range []agentturn.PromptErrorClassification{agentturn.PromptErrorSubscriptionQuota, agentturn.PromptErrorSubscriptionAuth} {
		request := outcomeRequest(t, workflow.RoleDeveloper, true)
		request.PromptResponse, request.PromptError = nil, class
		observation, err := newOutcomeReconciler(t, &outcomeStore{mutations: []store.MutationReservation{outcomeRequestReviewMutation(1)}}, &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead)}).Reconcile(context.Background(), request)
		if err != nil || observation.Outcome != workflow.TurnOutcomeChangeProposalReady || observation.Completion.Status != store.AgentTurnSucceeded {
			t.Fatalf("completed terminal intent = %+v, %v", observation, err)
		}
	}
}

type blockedSubscription struct {
	reports int
	err     error
}

func (s *blockedSubscription) Check() error                { return s.err }
func (s *blockedSubscription) Report(string, string) error { s.reports++; return nil }

func TestSubscriptionAdmissionBlockPreventsRuntimeLaunch(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.store.execution.Turn.AgentProfileConfig = json.RawMessage(`{"model":"openai/fixture"}`)
	dependencies := fixture.dependencies()
	dependencies.Subscription = &blockedSubscription{err: providerauth.ErrQuota}
	worker, err := agentturn.NewExecutionWorker(dependencies, fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, providerauth.ErrQuota) {
		t.Fatalf("blocked execution = %t, %v", processed, err)
	}
	if fixture.launcher.request.Lease.ID != "" {
		t.Fatal("blocked subscription launched a Runtime Process")
	}
	if fixture.outcomes.request.PromptError != agentturn.PromptErrorSubscriptionQuota {
		t.Fatal("admission lost durable failure classification")
	}
}

func TestSubscriptionQuotaReportsAreRetriedInsideController(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.store.execution.Turn.AgentProfileConfig = json.RawMessage(`{"model":"openai/fixture"}`)
	fixture.runtime.agent = &neutralPromptFixture{outcome: agent.Outcome{Admitted: true, Status: agent.Failed, FailureClass: "subscription_quota", FailureGeneration: "fixture-generation"}}
	gate := &blockedSubscription{}
	dependencies := fixture.dependencies()
	dependencies.Subscription = gate
	worker, err := agentturn.NewExecutionWorker(dependencies, fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = worker.ProcessNext(context.Background())
	if gate.reports != 1 {
		t.Fatal("controller did not record observed quota failure")
	}
}

func TestSubscriptionHandoffIncludesOnlyValidatedStructuredEvidence(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleDeveloper, true)
	request.PromptResponse, request.PromptError, request.PromptDiagnostic = nil, agentturn.PromptErrorSubscriptionQuota, "SENTINEL"
	request.PromptProviderDiagnostic = &agent.ProviderDiagnostic{Stage: "provider_http", HTTPStatus: new(429), Code: "subscription_sharing_usage_limit_exceeded", Cause: "http", RequestIDHash: "SENTINEL", Attempt: "SENTINEL"}
	observation, err := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"subscription_quota", "stage=provider_http", "http_status=429", "code=subscription_sharing_usage_limit_exceeded"} {
		if !strings.Contains(observation.Diagnostic, want) {
			t.Errorf("missing %s: %s", want, observation.Diagnostic)
		}
	}
	if strings.Contains(observation.Diagnostic, "SENTINEL") {
		t.Fatal("untrusted detail persisted")
	}
}
