package collaboration_test

import (
	"encoding/json"
	"errors"
	"github.com/jozala/omnigrex/internal/collaboration"
	"strings"
	"testing"
	"time"
)

func planningFixture(t *testing.T) *collaboration.State {
	t.Helper()
	s := &collaboration.State{}
	if err := s.StartPlanning(collaboration.OriginalRequest{Title: "Make onboarding useful", Body: "A rough idea, not a retention decision.", Source: collaboration.Source{Channel: "fixture", ID: "original"}}); err != nil {
		t.Fatal(err)
	}
	s.Prepare("planner", "turn-1")
	return s
}
func planningContent() collaboration.PlanningContent {
	return collaboration.PlanningContent{Title: "Onboarding", Status: "proposed", Changes: "Ephemeral local checklist.", RequirementsBrief: "Small teams, no retained personal data or external accounts. Reset discards status. Non-goals: implementation.", TechnicalPlan: "README.md describes a minimal repository. Separate transient status and UI; no migration. Test reset. If persistence becomes necessary, clarify again. Names are suggestions.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
}
func publishCheckpoint(t *testing.T, s *collaboration.State, id, turn string, c collaboration.PlanningContent) collaboration.PlanningRevision {
	t.Helper()
	r, err := s.ReserveCheckpoint(id, "planner", turn, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmCheckpoint(id, collaboration.Source{Channel: "fixture", ID: "comment-" + id}); err != nil {
		t.Fatal(err)
	}
	return r
}
func admitPlanning(s *collaboration.State, id string, c collaboration.Command) collaboration.Receipt {
	return s.Accept(id, c, collaboration.Admission{Active: true, Authorized: true, ParticipantID: "planner", Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: id}, At: time.Now()})
}
func TestPlanningImmutablePairOriginalSupersessionAndRestart(t *testing.T) {
	s := planningFixture(t)
	c := planningContent()
	c.Status = "draft"
	first := publishCheckpoint(t, s, "P1", "turn-1", c)
	first.Content.Evidence[0] = "tampered"
	if s.CurrentCheckpoint().Content.Evidence[0] != "README.md" {
		t.Fatal("revision aliases history")
	}
	if s.StartPlanning(collaboration.OriginalRequest{Title: "Edited", Source: collaboration.Source{Channel: "fixture", ID: "original"}}) == nil {
		t.Fatal("issue edit rewrote original")
	}
	s.Prepare("planner", "turn-2")
	second := publishCheckpoint(t, s, "P2", "turn-2", planningContent())
	if second.Supersedes != "P1" || s.Planning.Revisions[0].Content.Status != "draft" {
		t.Fatal(s.Planning)
	}
	if err := s.ConfirmCheckpoint("P1", collaboration.Source{Channel: "fixture", ID: "comment-P1"}); err != nil || s.CurrentCheckpoint().ID != "P2" {
		t.Fatal("replay rolled current backward", err)
	}
	c = planningContent()
	c.TechnicalPlan = "changed"
	if _, err := s.ReserveCheckpoint("P2", "planner", "turn-2", c); err == nil {
		t.Fatal("revision overwritten")
	}
	raw, _ := json.Marshal(s)
	var restored collaboration.State
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.CurrentCheckpoint().ID != "P2" || len(restored.Records("planner", "turn-2", "planning_history")) != 2 {
		t.Fatal("restart lost pair/history")
	}
}
func TestPlanningConfirmationAndFinalInputFences(t *testing.T) {
	s := planningFixture(t)
	publishCheckpoint(t, s, "P1", "turn-1", planningContent())
	validate := func(turn string) error {
		return s.ValidatePlanning("complete", "P1", "planner", turn, strings.Repeat("b", 40), "README.md changed only its title; commitments remain valid.", true)
	}
	if validate("turn-1") == nil {
		t.Fatal("missing confirmation accepted")
	}
	unauthorized := s.Accept("bad", collaboration.Command{Kind: "confirm-plan", Target: "P1"}, collaboration.Admission{Active: true, ParticipantID: "planner", Source: collaboration.Source{Channel: "fixture", ID: "bad"}})
	if unauthorized.ResponseID != "" {
		t.Fatal("unauthorized confirmation")
	}
	if r := admitPlanning(s, "confirmation", collaboration.Command{Kind: "confirm-plan", Target: "P1"}); r.ResponseID == "" {
		t.Fatal(r)
	}
	if validate("turn-1") == nil {
		t.Fatal("pending confirmation not fenced")
	}
	s.Prepare("planner", "turn-2")
	if _, err := s.Request("R1", "planner", "turn-2", "", []collaboration.Question{{ID: "Q1", Body: "Can personal data ever be retained?"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "questions"}); err != nil {
		t.Fatal(err)
	}
	if validate("turn-2") == nil {
		t.Fatal("unanswered consequential question ignored")
	}
	admitPlanning(s, "answer", collaboration.Command{Kind: "answer", Target: "R1", Body: "Never retain personal data."})
	s.Prepare("planner", "turn-3")
	if err := s.Resolve("R1", "turn-3", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"answer"}, Explanation: "Ephemeral local status only."}}); err != nil {
		t.Fatal(err)
	}
	if validate("unprepared") == nil {
		t.Fatal("unprepared completion")
	}
	if err := validate("turn-3"); err != nil {
		t.Fatal(err)
	}
	if s.Planning.ReadyRevision != "" || len(s.Planning.Revalidations) != 1 {
		t.Fatal("assessment prematurely marked ready")
	}
	if err := validate("turn-3"); err != nil || len(s.Planning.Revalidations) != 1 {
		t.Fatal("replay duplicated validation", err)
	}
}
func TestMaterialRevisionNeedsRenewedConfirmation(t *testing.T) {
	s := planningFixture(t)
	publishCheckpoint(t, s, "P1", "turn-1", planningContent())
	admitPlanning(s, "confirm", collaboration.Command{Kind: "confirm-plan", Target: "P1"})
	s.Prepare("planner", "turn-2")
	publishCheckpoint(t, s, "P2", "turn-2", planningContent())
	if err := s.ValidatePlanning("complete", "P2", "planner", "turn-2", strings.Repeat("b", 40), "Changed architecture investigated", true); err == nil {
		t.Fatal("old confirmation accepted changed pair")
	}
	r := admitPlanning(s, "stale", collaboration.Command{Kind: "confirm-plan", Target: "P1"})
	if r.ResponseID != "" || !strings.Contains(r.Feedback, "P2") {
		t.Fatal(r)
	}
}

func TestCheckpointNonPublicationIsImmutableAndRetrievalPrecedesYield(t *testing.T) {
	s := planningFixture(t)
	c := planningContent()
	publishCheckpoint(t, s, "P1", "turn-1", c)
	// Two accepted comments after preparation, without timing-dependent sleeps.
	if r := admitPlanning(s, "steer", collaboration.Command{Kind: "steer", Body: "Use a standalone CLI."}); r.ResponseID == "" {
		t.Fatal(r)
	}
	if r := admitPlanning(s, "amend", collaboration.Command{Kind: "amend", Target: "steer", Body: "Users have no repository access."}); r.ResponseID == "" {
		t.Fatal(r)
	}
	_, err := s.ReserveCheckpoint("P2", "planner", "turn-1", c)
	var rejection *collaboration.CheckpointNotApplied
	if !errors.As(err, &rejection) || rejection.Reason != "late_input" || len(s.Planning.Revisions) != 1 {
		t.Fatal(rejection, err)
	}
	if _, err = s.ReserveCheckpoint("P1", "planner", "turn-1", c); err != nil {
		t.Fatal("late input invalidated reserved replay", err)
	}
	s.Prepare("planner", "turn-2")
	raw, _ := json.Marshal(s)
	var restored collaboration.State
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	_, err = restored.ReserveCheckpoint("P2", "planner", "turn-1", c)
	if !errors.As(err, &rejection) || rejection.Reason != "late_input" {
		t.Fatal("replay tried publication again", err)
	}
	changed := c
	changed.Title = "Changed"
	if _, err = restored.ReserveCheckpoint("P2", "planner", "turn-1", changed); !errors.Is(err, collaboration.ErrInvalid) {
		t.Fatal(err)
	}
	restored.Inputs[0].DeliveryPending = true
	_, err = restored.ReserveCheckpoint("P3", "planner", "turn-2", c)
	if !errors.As(err, &rejection) || rejection.Reason != "unread_input" {
		t.Fatal(err)
	}
	restored.Inputs[0].DeliveryPending = false
	if _, err = restored.ReserveCheckpoint("P4", "planner", "turn-2", c); err != nil {
		t.Fatal(err)
	}
	if restored.Inputs[0].Body != "Use a standalone CLI." || restored.Inputs[1].Amends != "steer" || restored.Planning.Revisions[0].ID != "P1" {
		t.Fatal("lost immutable history")
	}
}
