package mcp_test

import (
	"context"
	"encoding/json"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/store"
	"strings"
	"testing"
)

type planningLedger struct {
	clarificationLedger
	revision collaboration.PlanningRevision
}

func (f *planningLedger) ReservePlanningCheckpoint(_ context.Context, _ store.AgentTurnLease, id string, c collaboration.PlanningContent) (collaboration.PlanningRevision, error) {
	f.revision = collaboration.PlanningRevision{ID: "P" + id, ParticipantID: reconciliationContext().Turn.AgentParticipantID, TurnID: reconciliationContext().Turn.ID, Content: c, ProcedureVersion: collaboration.DiscoveryProcedureVersion, OriginalRequest: collaboration.OriginalRequest{Title: "Original idea", Body: "Keep the request unchanged.", Source: collaboration.Source{Channel: "fixture", ID: "issue"}}}
	return f.revision, nil
}
func (f *planningLedger) ConfirmPlanningPublication(_ context.Context, _, id string, source collaboration.Source) error {
	if id != f.revision.ID {
		panic("wrong pair")
	}
	f.revision.Published = true
	f.revision.Source = &source
	return nil
}
func (f *planningLedger) GetPlanningCheckpoint(context.Context, string, string) (collaboration.PlanningRevision, error) {
	return f.revision, nil
}
func TestPlanningCheckpointLostResponseUsesExactPairWithoutRepost(t *testing.T) {
	ledger := &planningLedger{}
	api := &clarificationLostGitHub{backendGitHub: &backendGitHub{}}
	credentials := &backendCredentials{developer: "developer-secret"}
	policies, err := role.NewBuiltinPolicyCatalog([]role.ID{role.Planner})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: credentials, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger, Policies: policies})
	if err != nil {
		t.Fatal(err)
	}
	scope := productionToolScope(role.Planner)
	scope.PullRequest = nil
	scope.AgentProfileName = "planner"
	scope.RoleDisplayName = "Planner"
	c := collaboration.PlanningContent{Title: "Rough idea", Status: "proposed", Changes: "No retention.", RequirementsBrief: "Ephemeral local onboarding; no external accounts.", TechnicalPlan: "Separate transient state, UI and tests. Verify reset discards status.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
	request, _ := json.Marshal(map[string]any{"operation_id": "pair", "content": c, "signature": "persisted-signature"})
	_, err = backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolPublishPlanCheckpoint, Class: mcp.MutationTool, Scope: scope, OperationID: reconciliationMutationID, Arguments: request})
	if !mcp.IsOutcomeUnknown(err) || ledger.revision.Published {
		t.Fatal("uncertain pair treated as published", err)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: api, Credentials: credentials, Publications: &reconciliationPublications{}, Clarifications: ledger, Policies: policies})
	if err != nil {
		t.Fatal(err)
	}
	recoveryScope := reconciliationContext()
	recoveryScope.Role = role.Planner
	mutation := reconciliationMutation(mcp.ToolPublishPlanCheckpoint, string(request))
	mutation.ExternalService = "omnigrex"
	mutation.ExternalResourceID = recoveryScope.WorkflowID
	api.tamper = true
	result, err := reconciler.Reconcile(t.Context(), recoveryScope, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationUnresolved || ledger.revision.Published {
		t.Fatal("tampered pair accepted", result, err)
	}
	api.tamper = false
	result, err = reconciler.Reconcile(t.Context(), recoveryScope, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationFound || !ledger.revision.Published || api.posts != 1 {
		t.Fatal("recovery failed", result, err, api.posts)
	}
	for _, want := range []string{"Requirements brief", "Technical plan", "persisted-signature", "Original idea", "confirm-plan"} {
		if !strings.Contains(api.posted.Body, want) {
			t.Fatal("presentation missing", want)
		}
	}
}
