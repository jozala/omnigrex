package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
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
	f.revision = collaboration.PlanningRevision{Reference: "P1", ID: "P" + id, ParticipantID: reconciliationContext().Turn.AgentParticipantID, TurnID: reconciliationContext().Turn.ID, Content: c, ProcedureVersion: collaboration.DiscoveryProcedureVersion, OriginalRequest: collaboration.OriginalRequest{Title: "Original idea", Body: "Keep the request unchanged.", Source: collaboration.Source{Channel: "fixture", ID: "issue"}}}
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
	c := collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "Rough idea", Status: "proposed", Changes: "No retention.", RequirementsBrief: "Ephemeral local onboarding; no external accounts.", TechnicalPlan: "Separate transient state, UI and tests. Verify reset discards status.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
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
	for _, want := range []string{"Complete requirements brief", "Complete technical plan", "persisted-signature", "Original idea", "confirm-plan"} {
		if !strings.Contains(api.posted.Body, want) {
			t.Fatal("presentation missing", want)
		}
	}
}

type planningCompletionLedger struct {
	clarificationLedger
	validation collaboration.PlanRevalidation
	calls      int
	err        error
}

func (f *planningCompletionLedger) CompletePlanning(_ context.Context, _ store.AgentTurnLease, op, revision, commit, evidence string, _ bool) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.validation = collaboration.PlanRevalidation{SourceInvocationID: op, RevisionID: revision, RepositoryCommit: commit, Evidence: evidence, ParticipantID: reconciliationContext().Turn.AgentParticipantID}
	return errors.New("commit acknowledgment lost")
}
func (f *planningCompletionLedger) GetPlanningValidation(context.Context, string, string) (collaboration.PlanRevalidation, error) {
	return f.validation, nil
}
func TestPlanningCompletionLostAcknowledgmentReconcilesWithoutReapplying(t *testing.T) {
	ledger := &planningCompletionLedger{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: &backendGitHub{}, Credentials: &backendCredentials{developer: "fixture"}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	scope := productionToolScope(role.Planner)
	scope.PullRequest = nil
	request := json.RawMessage(`{"operation_id":"complete","revision_id":"P1","repository_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","evidence":"Only documentation changed.","material_decisions_resolved":true}`)
	_, err = backend.Execute(t.Context(), mcp.Invocation{Name: mcp.ToolCompletePlanning, Class: mcp.MutationTool, Scope: scope, OperationID: reconciliationMutationID, Arguments: request})
	if !mcp.IsOutcomeUnknown(err) {
		t.Fatalf("committed effect classified as definite failure: %v", err)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &backendGitHub{}, Credentials: &backendCredentials{developer: "fixture"}, Publications: &reconciliationPublications{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	recovery := reconciliationContext()
	recovery.Role = role.Planner
	mutation := reconciliationMutation(mcp.ToolCompletePlanning, string(request))
	mutation.ExternalService = "omnigrex"
	mutation.ExternalResourceID = recovery.WorkflowID
	for range 2 {
		result, err := reconciler.Reconcile(t.Context(), recovery, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound {
			t.Fatal(result, err)
		}
	}
	if ledger.calls != 1 {
		t.Fatal("completion reapplied", ledger.calls)
	}
	ledger.err = collaboration.ErrInvalid
	_, err = backend.Execute(t.Context(), mcp.Invocation{Name: mcp.ToolCompletePlanning, Class: mcp.MutationTool, Scope: scope, OperationID: reconciliationMutationID, Arguments: request})
	if !errors.Is(err, mcp.ErrInvalidInvocation) || mcp.IsOutcomeUnknown(err) {
		t.Fatal("invalid completion not definite", err)
	}
}

type rejectingPlanningLedger struct {
	planningLedger
	err error
}

func (f *rejectingPlanningLedger) ReservePlanningCheckpoint(context.Context, store.AgentTurnLease, string, collaboration.PlanningContent) (collaboration.PlanningRevision, error) {
	return collaboration.PlanningRevision{}, f.err
}
func TestPlanningExpectedNonPublicationIsNormalResultNotPublication(t *testing.T) {
	for _, reason := range []string{"late_input", "unread_input"} {
		t.Run(reason, func(t *testing.T) {
			ledger := &rejectingPlanningLedger{err: &collaboration.CheckpointNotApplied{Reason: reason}}
			api := &clarificationLostGitHub{backendGitHub: &backendGitHub{}}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "fixture"}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
			if err != nil {
				t.Fatal(err)
			}
			scope := productionToolScope(role.Planner)
			scope.PullRequest = nil
			request, _ := json.Marshal(map[string]any{"operation_id": "pending", "content": collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "CLI", Status: "draft", Changes: "Scope", RequirementsBrief: "Users", TechnicalPlan: "Local. Test reset.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}})
			invoke := func() (json.RawMessage, error) {
				return backend.Execute(t.Context(), mcp.Invocation{Name: mcp.ToolPublishPlanCheckpoint, Class: mcp.MutationTool, Scope: scope, OperationID: reconciliationMutationID, Arguments: request})
			}
			validRequest := request
			request = json.RawMessage(`{"operation_id":"malformed","content":{}}`)
			if _, err = invoke(); !errors.Is(err, mcp.ErrInvalidInvocation) {
				t.Fatal("malformed request became non-publication success", err)
			}
			request = validRequest
			raw, err := invoke()
			var result map[string]any
			if err != nil || json.Unmarshal(raw, &result) != nil || result["status"] != "input_pending" || result["publication"] != "not_applied" || result["reason"] != reason || result["revision_id"] != nil || result["source"] != nil || api.posts != 0 {
				t.Fatal(string(raw), err, api.posts)
			}
			want := "request_clarification_empty_questions"
			if reason == "unread_input" {
				want = "retrieve_human_input"
			}
			if result["next_action"] != want {
				t.Fatal(result)
			}
			ledger.err = collaboration.ErrInvalid
			if _, err = invoke(); !errors.Is(err, mcp.ErrInvalidInvocation) {
				t.Fatal("unrelated validation became successful", err)
			}
			ledger.err = errors.New("commit acknowledgment lost")
			if _, err = invoke(); !mcp.IsOutcomeUnknown(err) {
				t.Fatal("uncertain store result became definite", err)
			}
		})
	}
}

func (f *planningLedger) SaveCollaborationArtifact(_ context.Context, _ store.AgentTurnLease, id string, artifact collaboration.RenderedPublication) error {
	f.revision.Artifact = &artifact
	return nil
}

func (f *planningLedger) RejectPlanningPublication(_ context.Context, _ store.AgentTurnLease, id, reason string) error {
	if id != f.revision.ID || reason != "payload_too_large" {
		return collaboration.ErrInvalid
	}
	f.revision.PublicationFailed = true
	return nil
}

func TestOversizeCompletePairReturnsActionableDefiniteNonPublication(t *testing.T) {
	ledger := &planningLedger{}
	api := &clarificationLostGitHub{backendGitHub: &backendGitHub{}}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "fixture"}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	scope := productionToolScope(role.Planner)
	scope.AgentProfileName = "planner"
	scope.PullRequest = nil
	c := collaboration.PlanningContent{Title: "Complete pair", Summary: "Local checklist.", UnresolvedDecisions: []string{}, Status: "proposed", Changes: "Settled scope.", RequirementsBrief: strings.Repeat("界", 22000), TechnicalPlan: "Complete technical plan.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
	args, _ := json.Marshal(map[string]any{"operation_id": "oversize", "content": c, "signature": "Planner"})
	raw, err := backend.Execute(t.Context(), mcp.Invocation{Name: mcp.ToolPublishPlanCheckpoint, Class: mcp.MutationTool, Scope: scope, OperationID: reconciliationMutationID, Arguments: args})
	if err != nil {
		t.Fatal("definite size rejection became opaque tool failure", err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result["publication"] != "not_applied" || result["reason"] != "payload_too_large" || result["next_action"] != "rewrite_complete_pair" || result["revision_id"] != nil || result["source"] != nil || api.posts != 0 || !ledger.revision.PublicationFailed || ledger.revision.Content.RequirementsBrief != c.RequirementsBrief {
		t.Fatal("partial publication, truncation, or ambiguous outcome", string(raw), api.posts)
	}
}
