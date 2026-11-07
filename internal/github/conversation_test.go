package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/collaboration"
)

func TestConversationPresentationUsesShortTargetsAndCompleteExpandablePair(t *testing.T) {
	round := collaboration.Round{ID: "internal-round-uuid", Reference: "R3", Supersedes: "internal-prior-uuid", SupersedesReference: "R2", ParticipantID: "internal-participant-uuid", Introduction: "Both earlier choices are settled. Let us decide how to start.", Questions: []collaboration.Question{{ID: "internal-question-one", Body: "Start locally? I recommend a local checklist first."}, {ID: "internal-question-two", Body: "Keep updates structured? I recommend facts and links."}}}
	body := clarificationBody(round)
	for _, want := range []string{"R3", "Replaces R2", "1. Start locally?", "2. Keep updates structured?", round.Introduction, "/omnigrex answer R3", "answer just part"} {
		if !strings.Contains(body, want) {
			t.Fatal("missing human-facing content", want)
		}
	}
	if strings.Contains(body, "internal-") || strings.Contains(body, "omnigrex:run") || strings.Contains(body, "Repository write permission") {
		t.Fatal("bookkeeping/procedural boilerplate exposed", body)
	}
	brief := "Complete requirements: 界\n\n- Preserve every accepted constraint."
	plan := "Complete plan: investigate, implement, test.\n\nUnresolved decisions: none."
	revision := collaboration.PlanningRevision{ID: "internal-pair-uuid", Reference: "P2", Supersedes: "internal-old-pair", SupersedesReference: "P1", Content: collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "Local checklist", Status: "proposed", Changes: "Retain no personal data. Unresolved decisions: none.", RequirementsBrief: brief, TechnicalPlan: plan}, Decisions: []collaboration.Input{{ID: "internal-input-uuid", Reference: "A7", Actor: collaboration.Actor{ID: "internal-actor-uuid", Name: "Maintainer"}, Source: collaboration.Source{URL: "https://example.test/answer"}}}}
	checkpoint := planningCheckpointBody(revision)
	visible := strings.Split(checkpoint, "<details>")[0]
	if !strings.Contains(visible, "/omnigrex confirm-plan P2") || !strings.Contains(visible, "Review both complete documents") || strings.Contains(visible, brief) || strings.Contains(visible, "internal-") {
		t.Fatal("checkpoint entry point is not concise/readable", visible)
	}
	for _, want := range []string{"<summary>Complete requirements brief</summary>", "<summary>Complete technical plan</summary>", "<summary>Evidence and history</summary>", brief, plan, "A7", "https://example.test/answer", "can change or disappear"} {
		if !strings.Contains(checkpoint, want) {
			t.Fatal("lost document or accessible provenance", want)
		}
	}
	if strings.Contains(checkpoint, "internal-") {
		t.Fatal("unnecessary diagnostic identities exposed", checkpoint)
	}
}

type exactConversationAPI struct {
	comments []IssueComment
	posts    int
}

func (a *exactConversationAPI) CreateIssueComment(_ context.Context, _ string, _, _ string, _ int, r CommentRequest) (IssueComment, error) {
	a.posts++
	a.comments = append(a.comments, IssueComment{ID: 71, NodeID: "IC71", HTMLURL: "https://example.test/comment", Body: JoinBodyParts(r.Body, r.Marker)})
	return IssueComment{}, context.DeadlineExceeded
}
func (a *exactConversationAPI) ListIssueComments(context.Context, string, string, string, int) ([]IssueComment, error) {
	return a.comments, nil
}

func TestFrozenPayloadReconcilesWithoutRendererOrSignatureRegeneration(t *testing.T) {
	api := &exactConversationAPI{}
	p := ClarificationPublisher{API: api, Credential: func(context.Context) (string, error) { return "fixture", nil }, Marker: Marker{WorkflowID: "workflow", OperationID: "operation"}}
	original := collaboration.PublicationMessage{WorkflowID: "workflow", Round: collaboration.Round{Reference: "R1", Introduction: "Original context.", Questions: []collaboration.Question{{Body: "Original question?"}}}, Signature: "Original signature"}
	artifact, err := p.Render(original)
	if err != nil {
		t.Fatal(err)
	}
	original.Artifact = &artifact
	if _, err = p.Publish(t.Context(), original); err == nil {
		t.Fatal("fixture should lose acknowledgment")
	}
	// Simulate a restart with a changed renderer input and display identity.
	recovered := original
	recovered.Signature = "Different signature"
	recovered.Round = collaboration.Round{Reference: "R99", Introduction: "Different context."}
	_, found, err := p.Reconcile(t.Context(), recovered)
	if err != nil || !found || api.posts != 1 {
		t.Fatal("exact frozen artifact not recovered", found, err, api.posts)
	}
	api.comments[0].Body += "tampered"
	if _, found, err = p.Reconcile(t.Context(), recovered); err != nil || found {
		t.Fatal("non-exact artifact accepted", found, err)
	}
}

func TestCompletePayloadLimitIncludesUnicodeSignatureAndMarker(t *testing.T) {
	p := ClarificationPublisher{Marker: Marker{WorkflowID: "workflow", OperationID: "operation"}}
	revision := collaboration.PlanningRevision{Reference: "P2", Content: collaboration.PlanningContent{Summary: "A complete local checklist with no retained personal data.", UnresolvedDecisions: []string{}, Title: "Plan", Status: "proposed", Changes: "No unresolved decisions.", RequirementsBrief: "界", TechnicalPlan: "Complete plan."}}
	m := collaboration.PublicationMessage{Checkpoint: &revision, Signature: "Planner signature"}
	artifact, err := p.Render(m)
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(artifact.Body) - len(revision.Content.RequirementsBrief)
	revision.Content.RequirementsBrief = strings.Repeat("x", MaxPostedBodyLength-overhead-3) + "界"
	if err = p.Validate(m); err != nil {
		t.Fatal("exact Unicode payload boundary rejected", err)
	}
	revision.Content.TechnicalPlan += "x"
	if err = p.Validate(m); !errors.Is(err, collaboration.ErrPublicationTooLarge) {
		t.Fatal("complete payload overflow not actionable", err)
	}
	if revision.Content.RequirementsBrief[len(revision.Content.RequirementsBrief)-3:] != "界" || !strings.HasSuffix(revision.Content.TechnicalPlan, "x") {
		t.Fatal("validator truncated documents")
	}
}
