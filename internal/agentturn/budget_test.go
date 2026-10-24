package agentturn_test

import (
	"context"
	"encoding/json"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/stepbudget"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"strings"
	"testing"
)

func TestPreparedTurnFreezesBudgetAndProfilePrecedence(t *testing.T) {
	for _, profileValue := range []string{"", "unlimited", "7"} {
		t.Run(profileValue, func(t *testing.T) {
			source := validProfileSource()
			if profileValue != "" {
				source.contents[".omnigrex/team/developer.md"] = []byte(strings.Replace(string(source.contents[".omnigrex/team/developer.md"]), "name:", "max_steps: "+profileValue+"\nname:", 1))
			}
			registry, err := runtimeprofile.NewRegistry(runtimeProfile(t, testImage))
			if err != nil {
				t.Fatal(err)
			}
			database := &preparationStore{selected: workflow.RoleDeveloper}
			preparer := agentturn.NewPreparer(agentprofile.NewLoader(source, role.BuiltinPolicyCatalog()), agentprofile.SingletonSelector{}, registry, database)
			for i, defaultValue := range []string{"3", "5"} {
				limit, _ := stepbudget.Parse(defaultValue)
				preparer.SetDefaultMaxSteps(limit)
				if _, err := preparer.Prepare(context.Background(), validRequest()); err != nil {
					t.Fatal(err)
				}
				var saved struct {
					Budget stepbudget.Limit `json:"step_budget"`
				}
				if err := json.Unmarshal(database.calls[i].Stages[workflow.StageImplementation].TurnConfiguration, &saved); err != nil {
					t.Fatal(err)
				}
				expected := profileValue
				if expected == "" {
					expected = defaultValue
				}
				if saved.Budget.String() != expected {
					t.Fatalf("budget %s want %s", saved.Budget.String(), expected)
				}
			}
			var first struct {
				Budget stepbudget.Limit `json:"step_budget"`
			}
			_ = json.Unmarshal(database.calls[0].Stages[workflow.StageImplementation].TurnConfiguration, &first)
			if profileValue == "" && first.Budget.String() != "3" {
				t.Fatal("prepared budget mutated")
			}
		})
	}
}
func TestStepBudgetExhaustionNeedsPositiveEvidenceAndNoSuccessfulTerminal(t *testing.T) {
	for _, kind := range []string{"exhausted", "summary-pressure", "partial", "cancelled", "token-limit", "provider-failure"} {
		t.Run(kind, func(t *testing.T) {
			request := outcomeRequest(t, workflow.RoleReviewer, false)
			limit, _ := stepbudget.Parse("3")
			b := &stepbudget.Observation{Limit: limit, Used: 3, Responses: 3, Summary: true, Stopped: true, Complete: true}
			request.PromptResponse = &agent.Outcome{Admitted: true, Status: agent.Completed, StepBudget: b, BudgetSummary: "unfinished verification"}
			switch kind {
			case "summary-pressure":
				b.Stopped = false
			case "partial":
				b.Complete = false
			case "cancelled":
				request.PromptResponse.Status = agent.Aborted
			case "token-limit":
				request.PromptResponse.LimitReached = true
			case "provider-failure":
				request.PromptResponse.Status = agent.Failed
			}
			reconciler := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{})
			got, err := reconciler.ReconcileRecorded(context.Background(), request, nil)
			if err != nil {
				t.Fatal(err)
			}
			expected := workflow.TurnOutcomeInfrastructureFailed
			if kind == "exhausted" {
				expected = workflow.TurnOutcomeBlocked
			}
			if got.Outcome != expected {
				t.Fatalf("%s: %+v", kind, got)
			}
			if kind == "exhausted" && (!strings.Contains(got.Diagnostic, "limit=3 used=3") || !strings.Contains(got.Diagnostic, "unfinished verification") || !strings.Contains(string(got.Completion.Outcome), `"step_budget_exhausted":true`)) {
				t.Fatalf("missing diagnostic evidence %+v", got)
			}
		})
	}
}
func TestStepBudgetPreservesCorroboratedReviewerSuccess(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	limit, _ := stepbudget.Parse("1")
	request.PromptResponse = &agent.Outcome{Admitted: true, Status: agent.Completed, StepBudget: &stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Stopped: true, Complete: true}}
	// Reuse the same authoritative GitHub fixture as ordinary Reviewer settlement.
	ledger := []store.MutationReservation{outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeHead, 701)}
	database := &outcomeStore{mutations: ledger}
	github := &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead), reviews: []githubapi.Review{outcomeReview(outcomeHead, 701)}}
	got, err := newOutcomeReconciler(t, database, github).Reconcile(context.Background(), request)
	if err != nil || got.Outcome != workflow.TurnOutcomeApproved {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestHistoricalRuntimeDoesNotSilentlyAcceptFiniteBudget(t *testing.T) {
	p := runtimeProfile(t, testImage)
	contract := p.Contract()
	var inventory struct {
		Version int    `json:"version"`
		Node    string `json:"node_version"`
		Mise    string `json:"mise_sha256"`
		Files   []struct {
			Name string `json:"name"`
			SHA  string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(contract.GoldenResourceContract, &inventory); err != nil {
		t.Fatal(err)
	}
	for i, file := range inventory.Files {
		if file.Name == "step-budget.mjs" {
			inventory.Files = append(inventory.Files[:i], inventory.Files[i+1:]...)
			break
		}
	}
	contract.GoldenResourceContract, _ = json.Marshal(inventory)
	historical, err := runtimeprofile.New(contract)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := runtimeprofile.NewRegistry(historical)
	if err != nil {
		t.Fatal(err)
	}
	database := &preparationStore{selected: workflow.RoleDeveloper}
	preparer := agentturn.NewPreparer(agentprofile.NewLoader(validProfileSource(), role.BuiltinPolicyCatalog()), agentprofile.SingletonSelector{}, registry, database)
	limit, _ := stepbudget.Parse("1")
	preparer.SetDefaultMaxSteps(limit)
	if _, err := preparer.Prepare(context.Background(), validRequest()); err == nil || !githubapi.ExtractSafeErrorMetadata(err).Permanent || !strings.Contains(err.Error(), "does not support step budgets") || len(database.calls) != 0 {
		t.Fatalf("historical finite budget was not a prerequisite failure: %v", err)
	}
	preparer.SetDefaultMaxSteps(stepbudget.Limit{})
	if _, err := preparer.Prepare(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(database.calls[0].Stages[workflow.StageImplementation].TurnConfiguration), `"step_budget":"unlimited"`) {
		t.Fatal("historical effective budget was not recorded")
	}
}

func TestBudgetHandoffSummaryIsBoundedAndCredentialRedacted(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleReviewer, false)
	limit, _ := stepbudget.Parse("1")
	request.PromptResponse = &agent.Outcome{Admitted: true, Status: agent.Completed, StepBudget: &stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Stopped: true, Complete: true}, BudgetSummary: "unfinished\n" + outcomeCredential + " provider-budget-secret " + strings.Repeat("x", 5000)}
	reconciler := newOutcomeReconcilerWithProviders(t, &outcomeStore{}, &outcomeGitHub{}, json.RawMessage(`{"fixture":{"type":"api_key","key":"provider-budget-secret"}}`))
	got, err := reconciler.ReconcileRecorded(context.Background(), request, nil)
	if err != nil || got.Outcome != workflow.TurnOutcomeBlocked || strings.Contains(got.Diagnostic, outcomeCredential) || strings.Contains(got.Diagnostic, "provider-budget-secret") || strings.Contains(got.Diagnostic, "\n") || len([]rune(got.Diagnostic)) > 4096 {
		t.Fatalf("unsafe handoff diagnostic %v", err)
	}
}
