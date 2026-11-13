package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/role"
)

func TestPlanningRejectionProjectionBudgetsEncodedBytes(t *testing.T) {
	for _, mode := range []string{"small", "many", "single-large-record"} {
		t.Run(mode, func(t *testing.T) {
			decisions := []collaboration.StaleDecision{{RoundReference: "R1", QuestionID: "q", EvidenceReferences: []string{"A1"}, CorrectionReferences: []string{"A2"}}}
			if mode == "many" {
				for n := 1; n < 4500; n++ {
					decisions = append(decisions, collaboration.StaleDecision{RoundReference: fmt.Sprintf("R%d", n), QuestionID: strings.Repeat("\"\\\n界", 25), EvidenceReferences: []string{"A1"}, CorrectionReferences: []string{"A2"}})
				}
			}
			if mode == "single-large-record" {
				for n := 0; n < 10000; n++ {
					decisions[0].CorrectionReferences = append(decisions[0].CorrectionReferences, fmt.Sprintf("A%d", n+3))
				}
			}
			ledger := &rejectingPlanningLedger{err: &collaboration.CheckpointNotApplied{OperationID: "P" + reconciliationMutationID, Reason: "historical_decisions_unresolved", Decisions: decisions}}
			before, _ := json.Marshal(decisions)
			api := &clarificationLostGitHub{backendGitHub: &backendGitHub{}}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "fixture"}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
			if err != nil {
				t.Fatal(err)
			}
			scope := productionToolScope(role.Planner)
			scope.PullRequest = nil
			args := json.RawMessage(`{"operation_id":"reject","content":{"title":"Local checklist","summary":"No retention.","unresolved_decisions":[],"status":"proposed","changes":"Settled scope","requirements_brief":"Local checklist with no retention.","technical_plan":"Local CLI. Test reset.","investigated_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","evidence":["README.md"]}}`)
			invoke := func() json.RawMessage {
				t.Helper()
				raw, err := backend.Execute(t.Context(), mcp.Invocation{Name: mcp.ToolPublishPlanCheckpoint, Class: mcp.MutationTool, Scope: scope, OperationID: reconciliationMutationID, Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			raw := invoke()
			if len(raw) > 16*1024 || !reflect.DeepEqual(raw, invoke()) || api.posts != 0 {
				t.Fatal("unbounded, unstable, or published rejection")
			}
			var result struct {
				Count     int                           `json:"decision_count"`
				Decisions []collaboration.StaleDecision `json:"decisions"`
				Retrieval *struct {
					Collection  string `json:"collection"`
					RejectionID string `json:"rejection_id"`
					Index       int    `json:"index"`
					Offset      int    `json:"offset"`
				} `json:"retrieval"`
			}
			if err = json.Unmarshal(raw, &result); err != nil || result.Count != len(decisions) {
				t.Fatal(string(raw), err)
			}
			if !reflect.DeepEqual(result.Decisions, decisions[:len(result.Decisions)]) {
				t.Fatal("prefix changed")
			}
			if mode == "small" {
				if result.Retrieval != nil || len(result.Decisions) != 1 {
					t.Fatal("small result lost inline path")
				}
			} else if result.Retrieval == nil || result.Retrieval.Collection != "planning_rejection" || result.Retrieval.RejectionID != "P"+reconciliationMutationID || result.Retrieval.Index != len(result.Decisions) || result.Retrieval.Offset != 0 || len(result.Decisions) == len(decisions) {
				t.Fatal("missing remaining prerequisites")
			}
			if mode == "single-large-record" && len(result.Decisions) != 0 {
				t.Fatal("oversized first decision was inlined")
			}
			after, _ := json.Marshal(decisions)
			if string(before) != string(after) {
				t.Fatal("projection truncated durable receipt")
			}
			// Model the actual bridge's text content and outer Session JSON encoding.
			entry, _ := json.Marshal(map[string]any{"type": "message", "message": map[string]any{"role": "toolResult", "content": []any{map[string]any{"type": "text", "text": string(raw)}}}})
			if len(entry) >= 1<<20 {
				t.Fatal("outer encoding exceeded pinned Session bound")
			}
		})
	}
}

// The recovery path must project the original receipt, not current stale state
// or guessed frozen-history indices, even after some decisions are reconciled.
type projectionRecoveryLedger struct {
	rejectingPlanningLedger
	receipt *collaboration.CheckpointNotApplied
}

func (f *projectionRecoveryLedger) ReconcilePlanningNonPublication(context.Context, string, string, string) (*collaboration.CheckpointNotApplied, error) {
	return f.receipt, nil
}

func TestPlanningRejectionRecoveryUsesIdenticalBoundedProjection(t *testing.T) {
	scope := productionToolScope(role.Planner)
	scope.PullRequest = nil
	request := `{"operation_id":"pair","content":{"title":"Local checklist","summary":"No retention.","unresolved_decisions":[],"status":"proposed","changes":"Settled scope","requirements_brief":"Local checklist with no retention.","technical_plan":"Local CLI. Test reset.","investigated_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","evidence":["README.md"]}}`
	var args struct {
		Content collaboration.PlanningContent `json:"content"`
	}
	if err := json.Unmarshal([]byte(request), &args); err != nil {
		t.Fatal(err)
	}
	recovery := reconciliationContext()
	recovery.Role = role.Planner
	mutation := reconciliationMutation(mcp.ToolPublishPlanCheckpoint, request)
	mutation.ExternalService = "omnigrex"
	mutation.ExternalResourceID = recovery.WorkflowID
	receipt := &collaboration.CheckpointNotApplied{OperationID: "P" + mutation.ID, ParticipantID: recovery.Turn.AgentParticipantID, TurnID: mutation.AgentTurnID, Content: args.Content, Reason: "historical_decisions_unresolved"}
	for n := 0; n < 4500; n++ {
		receipt.Decisions = append(receipt.Decisions, collaboration.StaleDecision{RoundReference: fmt.Sprintf("R%d", n), QuestionID: strings.Repeat("q", 128), EvidenceReferences: []string{"A1"}, CorrectionReferences: []string{"A2"}})
	}
	ledger := &projectionRecoveryLedger{receipt: receipt, rejectingPlanningLedger: rejectingPlanningLedger{err: receipt}}
	api := &backendGitHub{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "fixture"}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := backend.Execute(t.Context(), mcp.Invocation{Name: mcp.ToolPublishPlanCheckpoint, Class: mcp.MutationTool, Scope: scope, OperationID: mutation.ID, Arguments: json.RawMessage(request)})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: api, Credentials: &backendCredentials{developer: "fixture"}, Publications: &reconciliationPublications{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := reconciler.Reconcile(t.Context(), recovery, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationFound || string(result.Outcome.Result) != string(expected) || len(result.Outcome.Result) > 16*1024 {
			t.Fatal("recovered projection changed", err)
		}
	}
}
