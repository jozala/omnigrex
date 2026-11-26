package store

import (
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/workflow"
	"testing"
)

func TestHumanInputWaiting(t *testing.T) {
	base := func() collaboration.State {
		return collaboration.State{Waiting: true, CurrentRound: "R1", Rounds: []collaboration.Round{{ID: "R1", ParticipantID: "p", Published: true, Source: &collaboration.Source{ID: "published"}, Questions: []collaboration.Question{{ID: "Q1"}}}}}
	}
	for _, status := range []workflow.State{workflow.StatePlanning, workflow.StateDeveloping, workflow.StateReviewing, workflow.StatePlanReady, workflow.StatePRReady, workflow.StateNeedsHuman, workflow.StateClosing, workflow.StateClosed, workflow.StateDormant} {
		t.Run(string(status), func(t *testing.T) {
			want := status == workflow.StatePlanning || status == workflow.StateDeveloping || status == workflow.StateReviewing
			if got := humanInputWaiting(status, true, false, false, "p", base()); got != want {
				t.Fatal(got, want)
			}
		})
	}
	for _, name := range []string{"active", "continuation", "unsettled", "not-waiting", "pending", "retrieval-pending", "no-participant", "unpublished", "no-source", "empty-yield", "superseded", "other-owner"} {
		t.Run(name, func(t *testing.T) {
			s := base()
			settled, active, continuation, p := true, false, false, "p"
			switch name {
			case "active":
				active = true
			case "continuation":
				continuation = true
			case "unsettled":
				settled = false
			case "not-waiting":
				s.Waiting = false
			case "pending":
				s.Inputs = []collaboration.Input{{ParticipantID: "p"}}
			case "retrieval-pending":
				s.Inputs = []collaboration.Input{{ParticipantID: "p", DeliveredTo: "t", DeliveryPending: true}}
			case "no-participant":
				p = ""
			case "unpublished":
				s.Rounds[0].Published = false
			case "no-source":
				s.Rounds[0].Source = nil
			case "empty-yield":
				s.Rounds[0].Questions = nil
			case "superseded":
				s.Rounds[0].SupersededBy = "R2"
			case "other-owner":
				s.Rounds[0].ParticipantID = "old"
			}
			if humanInputWaiting(workflow.StatePlanning, settled, active, continuation, p, s) {
				t.Fatal("not a genuine wait")
			}
		})
	}
	s := base()
	s.Inputs = []collaboration.Input{{ParticipantID: "old"}, {ParticipantID: "p", DeliveredTo: "old-turn"}}
	if !humanInputWaiting(workflow.StateDeveloping, true, false, false, "p", s) {
		t.Fatal("historical input must not hide a new wait")
	}
}

func TestHumanInputWaitingForProposedPairWithoutQuestions(t *testing.T) {
	for _, mode := range []string{"proposed", "unpublished", "failed", "no-source", "ready", "input", "active", "queued"} {
		t.Run(mode, func(t *testing.T) {
			pair := collaboration.PlanningRevision{ID: "P1", ParticipantID: "p", Published: true, Source: &collaboration.Source{ID: "published"}, Content: collaboration.PlanningContent{Status: "proposed"}}
			s := collaboration.State{Waiting: true, Planning: &collaboration.PlanningState{CurrentRevision: "P1", Revisions: []collaboration.PlanningRevision{pair}}}
			switch mode {
			case "unpublished":
				s.Planning.Revisions[0].Published = false
			case "failed":
				s.Planning.Revisions[0].PublicationFailed = true
			case "no-source":
				s.Planning.Revisions[0].Source = nil
			case "ready":
				s.Planning.ReadyRevision = "P1"
			case "input":
				s.Inputs = []collaboration.Input{{ParticipantID: "p", Kind: "implement"}}
			}
			if got := humanInputWaiting(workflow.StatePlanning, true, mode == "active", mode == "queued", "p", s); got != (mode == "proposed") {
				t.Fatal(mode, got)
			}
		})
	}
}
