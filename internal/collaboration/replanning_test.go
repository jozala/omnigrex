package collaboration

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func replanningFixture() State {
	return State{Planning: &PlanningState{
		CurrentRevision: "P1", ReadyRevision: "P1",
		Revisions:      []PlanningRevision{{ID: "P1", ParticipantID: "planner", Published: true, Source: &Source{Channel: "github", ID: "plan"}, Content: PlanningContent{Status: "proposed"}}},
		Implementation: ImplementationState{FirstDeveloperTurnID: "first", AcceptedRevisionID: "P1", AuthorizationID: "consent"},
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
