package collaboration

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func replanningFixture() State {
	return State{Planning: &PlanningState{
		CurrentRevision: "P1", ReadyRevision: "P1",
		Revisions:      []PlanningRevision{{ID: "P1", ParticipantID: "planner", Published: true, Source: &Source{Channel: "github", ID: "plan"}, Content: PlanningContent{Status: "proposed"}}},
		Implementation: ImplementationState{FirstDeveloperTurnID: "first", AcceptedRevisionID: "P1", AuthorizationID: "consent", Actions: []ImplementationAction{{ID: "consent", Kind: "implement", RevisionID: "P1", Actor: Actor{ID: "human"}, Source: Source{Channel: "fixture", ID: "consent"}}}},
	}, Preparations: []Preparation{{ParticipantID: "developer", TurnID: "dev-turn"}}}
}
func concernFixture() ReplanningConcern {
	return ReplanningConcern{AcceptedRevisionID: "P1", Commitment: "Preserve API compatibility", Evidence: []string{"Existing consumers require the removed field; isolated reproduction fails."}, Consequences: "The proposed removal breaks existing consumers.", Alternative: "Retain the field behind a compatibility adapter."}
}
func requestFixture() ReplanningRequestScope {
	return ReplanningRequestScope{OperationID: "operation", TurnID: "dev-turn", ExecutionEpoch: 1, AttemptID: "attempt", DeveloperParticipantID: "developer", PlannerParticipantID: "planner", HeadSHA: strings.Repeat("a", 40)}
}
func TestReplanningRequestRequiresAcceptedBindingAndExistingPlanner(t *testing.T) {
	tests := []struct {
		name   string
		change func(*State, *ReplanningRequestScope, *ReplanningConcern)
	}{
		{"direct development", func(s *State, _ *ReplanningRequestScope, _ *ReplanningConcern) { s.Planning = nil }},
		{"not started", func(s *State, _ *ReplanningRequestScope, _ *ReplanningConcern) {
			s.Planning.Implementation.FirstDeveloperTurnID = ""
		}},
		{"obsolete revision", func(_ *State, _ *ReplanningRequestScope, c *ReplanningConcern) { c.AcceptedRevisionID = "P0" }},
		{"missing Planner", func(_ *State, r *ReplanningRequestScope, _ *ReplanningConcern) { r.PlannerParticipantID = "" }},
		{"foreign Planner", func(_ *State, r *ReplanningRequestScope, _ *ReplanningConcern) { r.PlannerParticipantID = "foreign" }},
		{"foreign Turn", func(_ *State, r *ReplanningRequestScope, _ *ReplanningConcern) { r.TurnID = "foreign" }},
		{"no evidence", func(_ *State, _ *ReplanningRequestScope, c *ReplanningConcern) { c.Evidence = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := replanningFixture()
			r := requestFixture()
			c := concernFixture()
			tt.change(&s, &r, &c)
			before, _ := json.Marshal(s)
			if _, err := s.RequestReplanning(r, c); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v", err)
			}
			after, _ := json.Marshal(s)
			if string(before) != string(after) {
				t.Fatal("rejection mutated collaboration")
			}
		})
	}
	s := replanningFixture()
	cycle, err := s.RequestReplanning(requestFixture(), concernFixture())
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Phase != ReplanningRequested || cycle.Origin != ReplanningAutonomous {
		t.Fatalf("request must not dispatch: %+v", cycle)
	}
}
func TestReplanningRequestReplayAndRestartPreserveOneActiveCycle(t *testing.T) {
	s := replanningFixture()
	r := requestFixture()
	c := concernFixture()
	first, err := s.RequestReplanning(r, c)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	var restarted State
	if err = json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.RequestReplanning(r, c)
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	r.OperationID = "another"
	if _, err = restarted.RequestReplanning(r, c); !errors.Is(err, ErrInvalid) {
		t.Fatal("second active cycle accepted")
	}
	r = requestFixture()
	c.Consequences = "different"
	if _, err = restarted.RequestReplanning(r, c); !errors.Is(err, ErrInvalid) {
		t.Fatal("operation rebound")
	}
	first.Concern.Evidence[0] = "caller mutation"
	if restarted.Replanning.Cycles[0].Concern.Evidence[0] == "caller mutation" {
		t.Fatal("mutable receipt aliases history")
	}
}
func TestReplanningHumanExemptionRequiresIndependentAssessedDirection(t *testing.T) {
	for _, kind := range []string{"answer", "confirm-plan", "steer", "amend"} {
		t.Run(kind, func(t *testing.T) {
			s := replanningFixture()
			s.Inputs = []Input{{ID: "human", Kind: kind, ParticipantID: "developer", DeliveredTo: "dev-turn"}}
			s.Assessments = []Assessment{{InputID: "human", Disposition: "changed", InvalidateConfirmation: true}}
			c := concernFixture()
			c.HumanInputIDs = []string{"human"}
			cycle, err := s.RequestReplanning(requestFixture(), c)
			if err != nil {
				t.Fatal(err)
			}
			want := ReplanningAutonomous
			if kind == "steer" || kind == "amend" {
				want = ReplanningHuman
			}
			if cycle.Origin != want {
				t.Fatalf("origin=%s want %s", cycle.Origin, want)
			}
		})
	}
	s := replanningFixture()
	s.Inputs = []Input{{ID: "foreign", Kind: "steer", ParticipantID: "other", DeliveredTo: "other-turn"}}
	c := concernFixture()
	c.HumanInputIDs = []string{"foreign"}
	if _, err := s.RequestReplanning(requestFixture(), c); !errors.Is(err, ErrInvalid) {
		t.Fatal("foreign evidence accepted")
	}
}

func TestReplanningTransfersImmutableDeveloperEvidenceAndHistoricalCorrectionFence(t *testing.T) {
	s := replanningFixture()
	s.Inputs = []Input{{ID: "consent", Kind: "implement", Target: "P1", ParticipantID: "planner", DeliveredTo: "original-planner"}}
	s.Assessments = []Assessment{{InputID: "consent", Disposition: "confirmed"}}
	round, err := s.Request("round", "developer", "dev-turn", strings.Repeat("a", 40), []Question{{ID: "retention", Body: "Keep decisions transient?"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(round.ID, Source{Channel: "fixture", ID: "question"}); err != nil {
		t.Fatal(err)
	}
	admission := Admission{At: time.Now(), Active: true, Authorized: true, ParticipantID: "developer", Actor: Actor{ID: "human"}, Source: Source{Channel: "fixture", ID: "answer"}}
	if receipt := s.Accept("answer", Command{Kind: "answer", Target: s.Rounds[0].Reference, Body: "Keep them transient."}, admission); receipt.ResponseID == "" {
		t.Fatal(receipt)
	}
	s.Prepare("developer", "dev-evidence")
	if err = s.Assess("source-assessment", "developer", "dev-evidence", []Assessment{{InputID: "answer", EvidenceIDs: []string{"answer"}, Disposition: "unchanged", Explanation: "Transient decisions accepted."}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Resolve(round.ID, "dev-evidence", []Resolution{{QuestionID: "retention", ResponseIDs: []string{"answer"}, Explanation: "Transient decisions."}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestReplanning(func() ReplanningRequestScope { r := requestFixture(); r.TurnID = "dev-evidence"; return r }(), concernFixture()); err != nil {
		t.Fatal(err)
	}
	// This arrives after request recording, before safe transfer. Its immutable
	// owner stays Developer; the Planner receives it through explicit cycle scope.
	admission.Source.ID = "late-steer"
	if receipt := s.Accept("correction", Command{Kind: "steer", Body: "Retain nonpersonal decisions instead."}, admission); receipt.ResponseID == "" {
		t.Fatal(receipt)
	}
	if err = s.ActivateReplanning("operation", "attempt"); err != nil {
		t.Fatal(err)
	}
	s.Prepare("planner", "planner-turn")
	if err = s.Assess("unread", "planner", "planner-turn", []Assessment{{InputID: "correction", EvidenceIDs: []string{"correction"}, Disposition: "changed", Explanation: "Retain decisions."}}); err == nil {
		t.Fatal("read-less cross-Participant assessment")
	}
	s.Project("planner", "planner-turn")
	if err = s.Assess("correction-assessment", "planner", "planner-turn", []Assessment{{InputID: "correction", EvidenceIDs: []string{"correction", "answer"}, Disposition: "changed", Corrects: []string{"answer"}, InvalidateConfirmation: true, Explanation: "The human explicitly replaced the earlier transient-state decision."}}); err != nil {
		t.Fatal(err)
	}
	if !s.Unresolved("planner") {
		t.Fatal("input-level correction cleared historical question automatically")
	}
	if err = s.Resolve(round.ID, "planner-turn", []Resolution{{QuestionID: "retention", ResponseIDs: []string{"correction"}, Explanation: "Retain only nonpersonal decisions; explicit steering supersedes the earlier interpretation."}}); err != nil {
		t.Fatal(err)
	}
	if s.Unresolved("planner") {
		t.Fatal("scoped original question could not be reconciled")
	}
	for _, in := range s.Inputs {
		if in.ID != "consent" && (in.ParticipantID != "developer" || in.Actor.ID != "human") {
			t.Fatal("reparented original source", in)
		}
	}
	if s.ActiveReplanning().Origin != ReplanningAutonomous {
		t.Fatal("late human input refunded autonomous origin")
	}
	if s.existingImplementationAuthorityValid() {
		t.Fatal("material correction retained original authority")
	}
	// No unrelated Participant can use the evidence scope.
	s.Prepare("other", "foreign-turn")
	if err = s.Assess("foreign", "other", "foreign-turn", []Assessment{{InputID: "answer", EvidenceIDs: []string{"answer"}, Disposition: "unchanged", Explanation: "Not my evidence."}}); err == nil {
		t.Fatal("unrelated cross-Participant assessment")
	}
}

func TestReplanningAlreadyConsideredHumanEvidenceCannotManufactureFreeReturn(t *testing.T) {
	for _, where := range []string{"transferred", "considered"} {
		t.Run(where, func(t *testing.T) {
			s := replanningFixture()
			s.Inputs = []Input{{ID: "human", Kind: "steer", ParticipantID: "developer", DeliveredTo: "dev-turn"}}
			s.Assessments = []Assessment{{InputID: "human", Disposition: "changed", InvalidateConfirmation: true}}
			old := ReplanningCycle{Scope: ReplanningRequestScope{OperationID: "old"}, Phase: ReplanningCompleted, Origin: ReplanningAutonomous}
			if where == "transferred" {
				old.EvidenceInputIDs = []string{"human"}
			} else {
				old.Resolution = &ReplanningResolution{ConsideredInputIDs: []string{"human"}}
			}
			s.Replanning = &ReplanningState{Cycles: []ReplanningCycle{old}}
			concern := concernFixture()
			concern.HumanInputIDs = []string{"human"}
			c, err := s.RequestReplanning(requestFixture(), concern)
			if err != nil || c.Origin != ReplanningAutonomous {
				t.Fatal("reused human evidence manufactured exemption", c, err)
			}
		})
	}
}

func TestReplanningEmptyOptionalHumanEvidenceSurvivesJSONReplay(t *testing.T) {
	s := replanningFixture()
	concern := concernFixture()
	concern.HumanInputIDs = []string{}
	original, err := s.RequestReplanning(requestFixture(), concern)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored State
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	replay, err := restored.RequestReplanning(requestFixture(), concern)
	if err != nil || replay.Scope != original.Scope || len(restored.Replanning.Cycles) != 1 || replay.Origin != ReplanningAutonomous {
		t.Fatal("empty list changed receipt identity or origin", replay, err)
	}
	for _, change := range []func(*ReplanningConcern){
		func(c *ReplanningConcern) { c.AcceptedRevisionID = "other" },
		func(c *ReplanningConcern) { c.Commitment += "different" },
		func(c *ReplanningConcern) { c.Evidence = []string{"different"} },
		func(c *ReplanningConcern) { c.Consequences += "different" },
		func(c *ReplanningConcern) { c.Alternative += "different" },
		func(c *ReplanningConcern) { c.HumanInputIDs = []string{"different"} },
	} {
		different := concern
		change(&different)
		if concern.Equal(different) {
			t.Fatal("substantive evidence changed without changing identity", different)
		}
		if _, err = restored.RequestReplanning(requestFixture(), different); err == nil {
			t.Fatal("changed concern reused original operation", different)
		}
	}
}
