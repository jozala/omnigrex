package collaboration

import (
	"testing"
	"time"
)

func transferredQuestionFixture(t *testing.T) (State, Round) {
	t.Helper()
	s := replanningFixture()
	r, err := s.Request("round", "developer", "dev-turn", "head", []Question{{ID: "retention", Body: "Which retained state is allowed?"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(r.ID, Source{Channel: "fixture", ID: "question"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestReplanning(requestFixture(), concernFixture()); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateReplanning("operation", "attempt"); err != nil {
		t.Fatal(err)
	}
	s.Prepare("planner", "planner-turn")
	s.Project("planner", "planner-turn")
	return s, r
}

func TestReplanningTransferredAnswerRetainsAdmissionFences(t *testing.T) {
	for _, scenario := range []string{"authorized", "unauthorized", "inactive", "foreign", "superseded", "not-dispatched", "completed"} {
		t.Run(scenario, func(t *testing.T) {
			s, r := transferredQuestionFixture(t)
			a := Admission{Active: true, Authorized: true, ParticipantID: "planner", Actor: Actor{ID: "human"}, Source: Source{Channel: "fixture", ID: "answer"}, At: time.Now()}
			switch scenario {
			case "unauthorized":
				a.Authorized = false
			case "inactive":
				a.Active = false
			case "foreign":
				a.ParticipantID = "other"
			case "superseded":
				replacement, err := s.Request("next", "planner", "planner-turn", "head", r.Questions)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Publish(replacement.ID, Source{Channel: "fixture", ID: "next"}); err != nil {
					t.Fatal(err)
				}
			case "not-dispatched":
				s.Replanning.Cycles[0].Phase = ReplanningRequested
			case "completed":
				s.Replanning.Cycles[0].Phase = ReplanningCompleted
			}
			receipt := s.Accept("answer", Command{Kind: "answer", Target: r.Reference, Body: "Only nonpersonal decisions."}, a)
			if (receipt.ResponseID != "") != (scenario == "authorized") {
				t.Fatal(receipt)
			}
			if s.Rounds[0].ParticipantID != "developer" {
				t.Fatal("round reparented")
			}
		})
	}
}

func TestReplanningTransferredHistoricalDecisionPropagatesAndCannotBypassCorrection(t *testing.T) {
	s, r := transferredQuestionFixture(t)
	a := Admission{Active: true, Authorized: true, ParticipantID: "planner", Actor: Actor{ID: "human"}, Source: Source{Channel: "fixture", ID: "answer"}, At: time.Now()}
	if receipt := s.Accept("answer", Command{Kind: "answer", Target: r.Reference, Body: "Only nonpersonal decisions."}, a); receipt.ResponseID == "" {
		t.Fatal(receipt)
	}
	s.Prepare("planner", "answer-turn")
	resolution := Resolution{QuestionID: "retention", ResponseIDs: []string{"answer"}, Explanation: "Only nonpersonal decisions are allowed."}
	if err := s.Resolve(r.ID, "answer-turn", []Resolution{resolution}); err != nil {
		t.Fatal(err)
	}
	next, err := s.Request("next", "planner", "answer-turn", "head", r.Questions)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(next.ID, Source{Channel: "fixture", ID: "next"}); err != nil {
		t.Fatal(err)
	}
	// Historical obsolescence must propagate to the unchanged inherited question.
	obsolete := Resolution{QuestionID: "retention", Obsolete: true, Explanation: "Current repository evidence removes the premise."}
	if err = s.Resolve(r.ID, "answer-turn", []Resolution{obsolete}); err != nil {
		t.Fatal(err)
	}
	if current := latestResolution(s.Rounds[1], "retention"); current == nil || !current.Obsolete {
		t.Fatal("historical interpretation did not propagate", current)
	}
	// Restore the current unchanged question with the original delivered evidence.
	if err = s.Resolve(next.ID, "answer-turn", []Resolution{resolution}); err != nil {
		t.Fatal(err)
	}
	// The archived obsolete interpretation cannot bypass the renewed decision's
	// correction requirement when a different input supplies conflicting evidence.
	a.Source.ID = "conflict"
	if receipt := s.Accept("conflict", Command{Kind: "answer", Target: next.Reference, Body: "Retain personal data instead."}, a); receipt.ResponseID == "" {
		t.Fatal(receipt)
	}
	s.Prepare("planner", "conflict-turn")
	changed := Resolution{QuestionID: "retention", ResponseIDs: []string{"conflict"}, Explanation: "Retain personal data."}
	if err = s.Resolve(r.ID, "conflict-turn", []Resolution{changed}); err == nil {
		t.Fatal("historical obsolescence bypassed current correction requirement")
	}
	if s.Rounds[0].ParticipantID != "developer" || s.Rounds[1].ParticipantID != "planner" {
		t.Fatal("round ownership changed")
	}
}
