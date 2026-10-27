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

func TestAdmittedProviderAbortRetainsValidatedCauseInSettlement(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(map[bool]string{false: "response evidence", true: "separately carried evidence"}[separate], func(t *testing.T) {
			request := outcomeRequest(t, workflow.RoleDeveloper, true)
			diagnostic := &agent.ProviderDiagnostic{Stage: "transport", Cause: "timeout", Code: "SENTINEL", RequestIDHash: "SENTINEL", Attempt: "SENTINEL"}
			request.PromptResponse = &agent.Outcome{Admitted: true, Status: agent.Aborted, FailureDiagnostic: diagnostic}
			if separate {
				request.PromptProviderDiagnostic = diagnostic
			}
			observation, err := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(context.Background(), request)
			if err != nil || observation.Outcome != workflow.TurnOutcomeInfrastructureFailed || observation.Completion.Status != store.AgentTurnInterrupted {
				t.Fatalf("abort settlement changed: %+v, %v", observation, err)
			}
			for _, want := range []string{"timed out", "stage=transport", "cause=timeout", "http_status=unavailable", "code=unknown"} {
				if !strings.Contains(observation.Diagnostic, want) || !strings.Contains(observation.Completion.LastError, want) {
					t.Errorf("lost %s in settlement: %+v", want, observation)
				}
			}
			if strings.Contains(observation.Diagnostic, "SENTINEL") || strings.Contains(observation.Diagnostic, "was cancelled") {
				t.Fatalf("unsafe or misleading abort diagnostic: %s", observation.Diagnostic)
			}
		})
	}
}

func TestExecutionWorkerCarriesAdmittedAbortDiagnostic(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	diagnostic := agent.ParseProviderDiagnostic(json.RawMessage(`{"stage":"transport","cause":"timeout"}`))
	fixture.prompter.prompt = func(context.Context) (agent.Outcome, error) {
		return agent.Outcome{Admitted: true, Status: agent.Aborted, FailureDiagnostic: diagnostic}, nil
	}
	fixture.outcomes.reconcile = func(request agentturn.OutcomeReconciliation) store.AgentTurnSettlementObservation {
		if request.PromptProviderDiagnostic == nil || request.PromptProviderDiagnostic.Cause != "timeout" || request.PromptResponse == nil || request.PromptResponse.Status != agent.Aborted || request.PromptError != "" {
			t.Fatalf("lost admitted abort evidence: %+v", request)
		}
		return executionObservation(workflow.TurnOutcomeInfrastructureFailed, store.AgentTurnInterrupted)
	}
	processed, err := fixture.worker(t).ProcessNext(context.Background())
	if !processed || err != nil || fixture.store.settled.Completion.Status != store.AgentTurnInterrupted {
		t.Fatalf("abort policy changed: processed=%t, err=%v, settlement=%+v", processed, err, fixture.store.settled)
	}
}

func TestAdmittedAbortDoesNotInferTimeoutFromUntrustedEvidence(t *testing.T) {
	for _, cause := range []string{"cancelled", "SENTINEL"} {
		request := outcomeRequest(t, workflow.RoleDeveloper, true)
		request.PromptResponse = &agent.Outcome{Admitted: true, Status: agent.Aborted,
			FailureDiagnostic: &agent.ProviderDiagnostic{Stage: "SENTINEL", Cause: cause, Code: "SENTINEL"}}
		observation, err := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(context.Background(), request)
		if err != nil || observation.Completion.Status != store.AgentTurnInterrupted || !strings.Contains(observation.Diagnostic, "was cancelled") || strings.Contains(observation.Diagnostic, "timed out") || strings.Contains(observation.Diagnostic, "SENTINEL") {
			t.Fatalf("invalid abort explanation: %+v, %v", observation, err)
		}
	}
}
