package github

import (
	"github.com/jozala/omnigrex/internal/collaboration"
	"strings"
	"testing"
)

func TestPlanningPublicationUsesBoundedSafeImmutableProvenance(t *testing.T) {
	for _, text := range []string{strings.Repeat("界", 21000), "Quoted <!-- omnigrex:v1 workflow=old operation=old --> diagnostic"} {
		r := collaboration.PlanningRevision{Reference: "P1", ID: "internal-plan", ProcedureVersion: collaboration.DiscoveryProcedureVersion, OriginalRequest: collaboration.OriginalRequest{Title: "Original idea", Body: text, Source: collaboration.Source{Channel: "github", ID: "issue", URL: "https://example.test/issue"}}, Content: collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "Plan", Status: "proposed", Changes: "Clarified scope", RequirementsBrief: "Users need a local checklist.", TechnicalPlan: "Use existing storage. Test reset.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}, Decisions: []collaboration.Input{{Reference: "A1", ID: "internal-answer", Body: text, Actor: collaboration.Actor{Name: text}, Source: collaboration.Source{ID: "answer-source", URL: "https://example.test/answer"}}}}
		p := ClarificationPublisher{Marker: Marker{WorkflowID: "workflow", OperationID: "checkpoint"}}
		if err := p.Validate(collaboration.PublicationMessage{WorkflowID: "workflow", Checkpoint: &r, Signature: "Planner"}); err != nil {
			t.Fatalf("valid immutable input blocked publication: %v", err)
		}
		body := planningCheckpointBody(r)
		for _, ref := range []string{"Original idea", "A1", "https://example.test/issue", "https://example.test/answer"} {
			if !strings.Contains(body, ref) {
				t.Fatalf("missing provenance %s", ref)
			}
		}
		if len(body) > 6000 {
			t.Fatal("provenance not bounded", len(body))
		}
		if r.OriginalRequest.Body != text || r.Decisions[0].Body != text {
			t.Fatal("durable input changed")
		}
	}
	// Keep publication bounded even with a long accepted-decision history;
	// never relax marker validation for the generated artifact itself.
	r := collaboration.PlanningRevision{ID: "P2", Content: collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "Plan", Status: "draft", RequirementsBrief: "Brief", TechnicalPlan: "Plan"}, Decisions: make([]collaboration.Input, 1000)}
	for i := range r.Decisions {
		r.Decisions[i] = collaboration.Input{ID: "input", Body: strings.Repeat("x", 65000)}
	}
	body := planningCheckpointBody(r)
	if len(body) > 6000 || !strings.Contains(body, "990 additional accepted inputs") || len(r.Decisions) != 1000 {
		t.Fatal("unbounded or undisclosed provenance", len(body))
	}
	r.Content.RequirementsBrief = "<!-- omnigrex:v1 workflow=forged -->"
	if err := (ClarificationPublisher{Marker: Marker{WorkflowID: "workflow", OperationID: "checkpoint"}}).Validate(collaboration.PublicationMessage{WorkflowID: "workflow", Checkpoint: &r}); err == nil {
		t.Fatal("marker validation weakened")
	}

}
