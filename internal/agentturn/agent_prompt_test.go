package agentturn_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestExecutionWorkerPreservesNeutralPromptEndings(t *testing.T) {
	for _, tc := range []struct {
		name           string
		outcome        agent.Outcome
		preserved      bool
		classification agentturn.PromptErrorClassification
	}{
		{"settled", agent.Outcome{Admitted: true, Status: agent.Completed}, true, ""},
		{"runtime limit", agent.Outcome{Admitted: true, Status: agent.Completed, LimitReached: true}, true, ""},
		{"abort", agent.Outcome{Admitted: true, Status: agent.Aborted}, true, ""},
		{"provider failure", agent.Outcome{Admitted: true, Status: agent.Failed}, false, agentturn.PromptErrorFailure},
		{"subscription quota", agent.Outcome{Admitted: true, Status: agent.Failed, FailureClass: "subscription_quota"}, false, agentturn.PromptErrorSubscriptionQuota},
		{"subscription login", agent.Outcome{Admitted: true, Status: agent.Failed, FailureClass: "subscription_auth"}, false, agentturn.PromptErrorSubscriptionAuth},
		{"subscription configuration", agent.Outcome{Admitted: true, Status: agent.Failed, FailureClass: "subscription_configuration"}, false, agentturn.PromptErrorSubscriptionConfiguration},
		{"temporary subscription failure", agent.Outcome{Admitted: true, Status: agent.Failed, FailureClass: "subscription_temporary"}, false, agentturn.PromptErrorFailure},
		{"lost settlement", agent.Outcome{Admitted: true, Status: agent.Unknown}, false, agentturn.PromptErrorFailure},
		{"rejected admission", agent.Outcome{Status: agent.Rejected}, false, agentturn.PromptErrorFailure},
		{"admitted rejection", agent.Outcome{Admitted: true, Status: agent.Rejected}, false, agentturn.PromptErrorFailure},
		{"unconfirmed completed admission", agent.Outcome{Status: agent.Completed}, false, agentturn.PromptErrorFailure},
		{"unconfirmed aborted admission", agent.Outcome{Status: agent.Aborted}, false, agentturn.PromptErrorFailure},
		{"unconfirmed unknown admission", agent.Outcome{Status: agent.Unknown}, false, agentturn.PromptErrorFailure},
		{"invalid status", agent.Outcome{Admitted: true, Status: "unrecognized"}, false, agentturn.PromptErrorInvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
			connection := &neutralPromptFixture{outcome: tc.outcome}
			fixture.runtime.agent = connection
			processed, err := fixture.worker(t).ProcessNext(t.Context())
			if !processed || !strings.Contains(connection.text, "agent_turn_id") {
				t.Fatal("neutral runtime did not receive the structured prompt")
			}
			if tc.preserved && err != nil || !tc.preserved && err == nil {
				t.Fatalf("ProcessNext() error = %v, preserved ending = %t", err, tc.preserved)
			}
			request := fixture.outcomes.request
			if request.PromptError != tc.classification {
				t.Fatalf("classification: %s", request.PromptError)
			}
			if !tc.preserved {
				if request.PromptResponse != nil {
					t.Fatal("uncertain/failure ending became completion")
				}
			} else if request.PromptResponse == nil || *request.PromptResponse != tc.outcome {
				t.Fatalf("ending: %+v", request.PromptResponse)
			}
		})
	}
}

type neutralPromptFixture struct {
	outcome agent.Outcome
	text    string
}

func (f *neutralPromptFixture) Prompt(_ context.Context, _ store.AgentTurnLease, _ store.AgentSession, text string, _ agentevent.Sink) (agent.Outcome, error) {
	f.text = text
	return f.outcome, nil
}
func (*neutralPromptFixture) Close() error { return nil }

func TestSubscriptionTemporarySummaryRetainsObservationInsteadOfRuntimeRejection(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.runtime.agent = &neutralPromptFixture{outcome: agent.Outcome{Admitted: true, Status: agent.Failed, FailureClass: "subscription_temporary", FailureDiagnostic: agent.ParseProviderDiagnostic([]byte(`{"stage":"provider_http","cause":"http","http_status":502,"code":"model_not_found"}`))}}
	_, err := fixture.worker(t).ProcessNext(t.Context())
	if err == nil {
		t.Fatal("expected failure")
	}
	diagnostic := fixture.outcomes.request.PromptDiagnostic
	for _, want := range []string{"subscription_temporary", "stage=provider_http", "http_status=502", "code=model_not_found"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("missing %s: %s", want, diagnostic)
		}
	}
	if strings.Contains(diagnostic, "runtime rejected") {
		t.Fatalf("misleading rejection: %s", diagnostic)
	}
}

func TestSummaryProviderCategorySurvivesWithoutChangingFailurePolicy(t *testing.T) {
	for _, category := range []string{"subscription_temporary", "subscription_quota", "subscription_auth", "subscription_configuration", "SENTINEL"} {
		t.Run(category, func(t *testing.T) {
			fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
			diagnostic := agent.ParseProviderDiagnostic([]byte(`{"category":"` + category + `","stage":"provider_http","cause":"http","http_status":502,"code":"subscription_sharing_usage_unavailable"}`))
			fixture.runtime.agent = &neutralPromptFixture{outcome: agent.Outcome{Admitted: true, Status: agent.Failed, FailureClass: "provider_failure", FailureDiagnostic: diagnostic}}
			_, err := fixture.worker(t).ProcessNext(t.Context())
			if err == nil || fixture.outcomes.request.PromptError != agentturn.PromptErrorFailure {
				t.Fatalf("summary failure policy changed: %v, %s", err, fixture.outcomes.request.PromptError)
			}
			request := outcomeRequest(t, workflow.RoleDeveloper, true)
			request.PromptResponse = nil
			request.PromptError = fixture.outcomes.request.PromptError
			request.PromptDiagnostic = fixture.outcomes.request.PromptDiagnostic
			request.PromptProviderDiagnostic = fixture.outcomes.request.PromptProviderDiagnostic
			observation, err := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(t.Context(), request)
			if err != nil || observation.Outcome != workflow.TurnOutcomeInfrastructureFailed || observation.Completion.Status != store.AgentTurnFailed {
				t.Fatalf("summary changed settlement/retry route: %+v, %v", observation, err)
			}
			wants := []string{"provider_failure", "stage=provider_http", "cause=http", "http_status=502", "code=subscription_sharing_usage_unavailable"}
			if category != "SENTINEL" {
				wants = append(wants, "category="+category)
			}
			for _, diagnostic := range []string{fixture.outcomes.request.PromptDiagnostic, observation.Diagnostic, observation.Completion.LastError} {
				for _, want := range wants {
					if !strings.Contains(diagnostic, want) {
						t.Errorf("summary diagnostic lost %s: %s", want, diagnostic)
					}
				}
				if strings.Contains(diagnostic, "runtime rejected") || strings.Contains(diagnostic, "SENTINEL") {
					t.Errorf("misleading/unsafe diagnostic: %s", diagnostic)
				}
			}
		})
	}
}
