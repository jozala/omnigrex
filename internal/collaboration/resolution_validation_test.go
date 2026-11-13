package collaboration_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
)

func TestRevisedQuestionsRejectMixedEvidenceAtomically(t *testing.T) {
	var state collaboration.State
	old, err := state.Request("old", "planner", "before", "head", []collaboration.Question{{ID: "deadline", Body: "Thirty minutes?"}, {ID: "budget", Body: "No resets?"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = state.Publish(old.ID, collaboration.Source{ID: "old", Channel: "fixture"}); err != nil {
		t.Fatal(err)
	}
	admit := func(id, target, body string) {
		t.Helper()
		receipt := state.Accept(id, collaboration.Command{Kind: "answer", Target: target, Body: body}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: "planner", Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: id}, At: time.Now()})
		if receipt.ResponseID != id {
			t.Fatal(receipt)
		}
	}
	admit("older", old.Reference, "Two hours and success resets")
	state.Prepare("planner", "revise")
	if err = state.Assess("assess-older", "planner", "revise", []collaboration.Assessment{{InputID: "older", EvidenceIDs: []string{"older"}, Disposition: "changed", Explanation: "Human requests revised recommendations"}}); err != nil {
		t.Fatal(err)
	}
	if err = state.Resolve(old.ID, "revise", []collaboration.Resolution{{QuestionID: "deadline", ResponseIDs: []string{"older"}, Explanation: "Human requests two hours; explain grace separately"}}); err != nil {
		t.Fatal(err)
	}
	current, err := state.Request("new", "planner", "revise", "head", []collaboration.Question{{ID: "deadline-v2", Replaces: "deadline", Body: "Two hours plus separate grace?"}, {ID: "budget-v2", Replaces: "budget", Body: "Only literal success resets?"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = state.Publish(current.ID, collaboration.Source{ID: "new", Channel: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if len(current.Resolutions) != 0 {
		t.Fatal("changed questions inherited prior approval")
	}
	admit("current", current.Reference, "yes/yes; secret-human-body")
	state.Prepare("planner", "resolve")
	if err = state.Assess("assessment", "planner", "resolve", []collaboration.Assessment{{InputID: "current", EvidenceIDs: []string{"current"}, Disposition: "changed", Explanation: "Accepts revisions"}}); err != nil {
		t.Fatal(err)
	}
	batch := []collaboration.Resolution{{QuestionID: "deadline-v2", ResponseIDs: []string{"current"}, Explanation: "Approve deadline"}, {QuestionID: "budget-v2", ResponseIDs: []string{"older", "current"}, Explanation: "Approve budget"}}
	before, _ := json.Marshal(state)
	err = state.Resolve(current.ID, "resolve", batch)
	var detail *collaboration.ResolutionValidationError
	if !errors.Is(err, collaboration.ErrInvalid) || !errors.As(err, &detail) || detail.Code != "response_not_valid_for_question" || detail.ResolutionIndex != 2 || detail.ResponseIndex != 1 {
		t.Fatalf("rejection = %#v %v", detail, err)
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("rejected batch changed prior inputs, assessments or interpretations")
	}
	batch[1].ResponseIDs = []string{"current"}
	if err = state.Resolve(current.ID, "resolve", batch); err != nil {
		t.Fatal(err)
	}
	if len(state.Rounds[len(state.Rounds)-1].Resolutions) != 2 {
		t.Fatal("valid batch not fully recorded")
	}
}
