package collaboration_test

import (
	"github.com/jozala/omnigrex/internal/collaboration"
	"testing"
	"time"
)

func admission(source string) collaboration.Admission {
	return collaboration.Admission{Active: true, Authorized: true, ParticipantID: "developer", Actor: collaboration.Actor{ID: "maintainer", Name: "M"}, Source: collaboration.Source{Channel: "fixture", ID: source}, At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
}
func TestTwoRoundsRetainPartialConflictingAndAmendedInput(t *testing.T) {
	for _, participant := range []string{"developer", "reviewer"} {
		t.Run(participant, func(t *testing.T) {
			var s collaboration.State
			q := []collaboration.Question{{ID: "Q1", Body: "Which compatibility policy?"}, {ID: "Q2", Body: "Which retention period?"}}
			if _, err := s.Request("R1", participant, "T1", "head1", q); err != nil {
				t.Fatal(err)
			}
			a := admission("reply-before-confirmation")
			a.ParticipantID = participant
			receipt := s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "1. Keep compatibility. Cannot decide 2."}, a)
			if receipt.ResponseID != "A1" || s.Rounds[0].Published || len(s.Rounds[0].Resolutions) != 0 {
				t.Fatalf("acceptance changed interpretation: %+v", s)
			}
			if err := s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "question1"}); err != nil {
				t.Fatal(err)
			}
			a.Source.ID = "conflict"
			a.Actor.ID = "other-maintainer"
			s.Accept("A2", collaboration.Command{Kind: "answer", Target: "R1", Body: "Break compatibility."}, a)
			batch := s.Prepare(participant, "T2")
			if len(batch) != 2 || batch[0].ID != "A1" || batch[1].Actor.ID != "other-maintainer" {
				t.Fatalf("batch=%+v", batch)
			}
			a.Source.ID = "amend"
			s.Accept("A3", collaboration.Command{Kind: "amend", Target: "A2", Body: "Correction: keep compatibility."}, a)
			if len(s.Prepare(participant, "T2")) != 2 || !s.Pending(participant) {
				t.Fatal("later input crossed preparation boundary")
			}
			if len(s.Prepare(participant, "T3")) != 1 || s.Inputs[1].Body != "Break compatibility." || s.Inputs[2].Amends != "A2" {
				t.Fatal("amendment rewrote history")
			}
			if err := s.Resolve("R1", "T3", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A3"}, Explanation: "Explicit correction restores compatibility."}}); err != nil {
				t.Fatal(err)
			}
			if len(s.Rounds[0].Resolutions) != 1 {
				t.Fatal("partial input silently settled unresolved questions")
			}
			if _, err := s.Request("R2", participant, "T3", "head2", []collaboration.Question{q[1], {ID: "Q3", Body: "Recommend seven days. Accept?"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.Publish("R2", collaboration.Source{Channel: "fixture", ID: "question2"}); err != nil {
				t.Fatal(err)
			}
			a.Source.ID = "late"
			if r := s.Accept("A4", collaboration.Command{Kind: "answer", Target: "R1", Body: "yes to all"}, a); r.ResponseID != "" {
				t.Fatal("accepted superseded round")
			}
			a.Source.ID = "current"
			s.Accept("A5", collaboration.Command{Kind: "answer", Target: "R2", Body: "yes to all"}, a)
			if len(s.Prepare(participant, "T4")) != 1 || len(s.Rounds[1].Resolutions) != 0 || s.Rounds[0].SupersededBy != "R2" {
				t.Fatal("round history or explicit interpretation missing")
			}
		})
	}
}
func TestAdmissionFencesAndDuplicateSources(t *testing.T) {
	var s collaboration.State
	a := admission("ordinary")
	a.Authorized = false
	if r := s.Accept("A1", collaboration.Command{Kind: "steer", Body: "Direction"}, a); r.ResponseID != "" {
		t.Fatal("unauthorized accepted")
	}
	a.Authorized = true
	if r := s.Accept("A2", collaboration.Command{Kind: "steer", Body: "Changed edited text"}, a); r.ResponseID != "" || len(s.Inputs) != 0 {
		t.Fatal("source replay gained authority")
	}
	for _, active := range []bool{false, true} {
		a.Source.ID = "new"
		a.Active = active
		r := s.Accept("A3", collaboration.Command{Kind: "steer", Body: "Direction"}, a)
		if r.ResponseID != "" {
			t.Fatal("duplicate rejected source was rerun")
		}
	}
	a.Source.ID = "active"
	a.Active = true
	if r := s.Accept("A4", collaboration.Command{Kind: "steer", Body: "Direction"}, a); r.ResponseID != "A4" {
		t.Fatal(r)
	}
	if len(s.Inputs) != 1 {
		t.Fatal("lost input")
	}
}
func TestRequestRejectsChangedQuestionIdentityAndUnpublishedResolution(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "h", []collaboration.Question{{ID: "Q1", Body: "First recommendation"}})
	if _, err := s.Request("R2", "developer", "T2", "h", []collaboration.Question{{ID: "Q1", Body: "Materially changed recommendation"}}); err == nil {
		t.Fatal("changed question reused identity")
	}
	if err := s.Resolve("R1", "T1", nil); err == nil {
		t.Fatal("unpublished round resolved")
	}
}

func TestPreparationBoundaryIsFrozenEvenWhenInitiallyEmpty(t *testing.T) {
	var s collaboration.State
	if len(s.Prepare("developer", "T1")) != 0 {
		t.Fatal("unexpected input")
	}
	s.Accept("A1", collaboration.Command{Kind: "steer", Body: "Direction"}, admission("later"))
	if len(s.Prepare("developer", "T1")) != 0 || !s.Pending("developer") {
		t.Fatal("empty boundary absorbed later input")
	}
	if batch := s.Prepare("developer", "T2"); len(batch) != 1 || batch[0].ID != "A1" {
		t.Fatal("later Turn lost input")
	}
}
func TestUnresolvedQuestionsRequireCarryForwardOrExplicitReplacement(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "head1", []collaboration.Question{{ID: "Q1", Body: "Retention?"}})
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "published"})
	if _, err := s.Request("R2", "developer", "T2", "head2", []collaboration.Question{{ID: "Q2", Body: "New recommendation"}}); err == nil {
		t.Fatal("unanswered question disappeared")
	}
	if _, err := s.Request("R2", "developer", "T2", "head2", []collaboration.Question{{ID: "Q2", Body: "Recommend seven days", Replaces: "Q1"}}); err != nil {
		t.Fatal(err)
	}
	s.Publish("R2", collaboration.Source{Channel: "fixture", ID: "published2"})
	s.Prepare("reviewer", "wrongTurn")
	if err := s.Resolve("R2", "wrongTurn", []collaboration.Resolution{{QuestionID: "Q2", Obsolete: true, Explanation: "Changed code."}}); err == nil {
		t.Fatal("another Participant resolved question")
	}
	s.Prepare("developer", "T3")
	if err := s.Resolve("R2", "T3", []collaboration.Resolution{{QuestionID: "Q2", Obsolete: true, Explanation: "Current code removed the retained artifact; question is obsolete."}}); err != nil {
		t.Fatal(err)
	}
	if s.Unresolved("developer") {
		t.Fatal("explicit evidence-based obsolescence did not settle question")
	}
}

func TestAmendmentSupersedesInterpretationAcrossUnchangedRounds(t *testing.T) {
	for _, mode := range []string{"same-round", "replacement-round", "historical-round"} {
		t.Run(mode, func(t *testing.T) {
			advance := mode != "same-round"
			var s collaboration.State
			qs := []collaboration.Question{{ID: "Q1", Body: "Compatibility?"}, {ID: "Q2", Body: "Retention?"}}
			s.Request("R1", "developer", "T1", "h", qs)
			s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub"})
			s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Keep compatibility"}, admission("answer"))
			s.Prepare("developer", "T2")
			original := collaboration.Resolution{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Keep compatibility"}
			if err := s.Resolve("R1", "T2", []collaboration.Resolution{original}); err != nil {
				t.Fatal(err)
			}
			round := "R1"
			if advance {
				if _, err := s.Request("R2", "developer", "T2", "h2", qs); err != nil {
					t.Fatal(err)
				}
				s.Publish("R2", collaboration.Source{Channel: "fixture", ID: "pub2"})
				round = "R2"
				if len(s.Rounds[1].Resolutions) != 1 {
					t.Fatal("unchanged resolved Q1 lost its accepted evidence")
				}
				if !s.Unresolved("developer") {
					t.Fatal("unanswered Q2 silently resolved")
				}
			}
			if r := s.Accept("A2", collaboration.Command{Kind: "amend", Target: "A1", Body: "Correction: break compatibility"}, admission("amend")); r.ResponseID != "A2" {
				t.Fatal(r)
			}
			s.Prepare("developer", "T3")
			resolutionRound := round
			if mode == "historical-round" {
				resolutionRound = "R1"
			}
			if err := s.Resolve(resolutionRound, "T3", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A2"}, Explanation: "Explicit amendment changes the decision"}}); err != nil {
				t.Fatal(err)
			}
			if s.Rounds[0].Resolutions[0].Explanation != original.Explanation || s.Inputs[0].Body != "Keep compatibility" {
				t.Fatal("original evidence rewritten")
			}
			if err := s.Resolve(round, "T3", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Stale original"}}); err == nil {
				t.Fatal("old evidence superseded amendment")
			}
		})
	}
}

func TestAmendedResolvedQuestionMustBeCarriedAndCannotReuseOldEvidence(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "h", []collaboration.Question{{ID: "Q1", Body: "Policy?"}})
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub"})
	s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Keep"}, admission("a1"))
	s.Prepare("developer", "T2")
	s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Keep"}})
	if s.Unresolved("developer") {
		t.Fatal("resolved question remains blocking")
	}
	s.Accept("A2", collaboration.Command{Kind: "amend", Target: "A1", Body: "Break"}, admission("a2"))
	s.Prepare("developer", "T3")
	if !s.Unresolved("developer") {
		t.Fatal("amendment silently retained stale interpretation")
	}
	if _, err := s.Request("R2", "developer", "T3", "h", []collaboration.Question{{ID: "Q2", Body: "Other?"}}); err == nil {
		t.Fatal("amended unresolved question dropped")
	}
	s.Accept("A3", collaboration.Command{Kind: "amend", Target: "A2", Body: "Actually keep"}, admission("a3"))
	s.Prepare("developer", "T4")
	if err := s.Resolve("R1", "T4", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1", "A3"}, Explanation: "Latest explicit amendment restores original"}}); err != nil {
		t.Fatal(err)
	}
	if s.Unresolved("developer") {
		t.Fatal("amendment chain did not settle")
	}
}

func TestAmendmentOfEarlierDecisionOmittedFromCurrentQuestionSet(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "h", []collaboration.Question{{ID: "Q1", Body: "Compatibility?"}, {ID: "Q2", Body: "Retention?"}})
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub"})
	s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Keep compatibility"}, admission("a1"))
	s.Prepare("developer", "T2")
	s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Keep compatibility"}})
	if _, err := s.Request("R2", "developer", "T2", "h2", []collaboration.Question{{ID: "Q2", Body: "Retention?"}}); err != nil {
		t.Fatal(err)
	}
	s.Publish("R2", collaboration.Source{Channel: "fixture", ID: "pub2"})
	if r := s.Accept("A2", collaboration.Command{Kind: "amend", Target: "A1", Body: "Correction: break compatibility"}, admission("a2")); r.ResponseID == "" {
		t.Fatal(r)
	}
	s.Prepare("developer", "T3")
	if err := s.Resolve("R1", "T3", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A2"}, Explanation: "Explicit earlier decision correction"}}); err != nil {
		t.Fatal(err)
	}
	if len(s.Rounds[0].Resolutions) != 2 || len(s.Rounds) != 2 {
		t.Fatal("correction lost or unnecessary round added")
	}
}

func TestReplacementRoundPreservesAnsweredDecisionAndCarriesUnansweredQuestion(t *testing.T) {
	var s collaboration.State
	questions := []collaboration.Question{{ID: "Q1", Body: "Compatibility?"}, {ID: "Q2", Body: "Retention?"}}
	s.Request("R1", "developer", "T1", "h1", questions)
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub1"})
	s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Keep compatibility"}, admission("compatibility"))
	s.Prepare("developer", "T2")
	if err := s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Keep compatibility"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Request("R2", "developer", "T2", "h2", questions); err != nil {
		t.Fatal(err)
	}
	s.Publish("R2", collaboration.Source{Channel: "fixture", ID: "pub2"})
	if !s.Unresolved("developer") {
		t.Fatal("unanswered Q2 was silently settled")
	}
	s.Accept("A2", collaboration.Command{Kind: "answer", Target: "R2", Body: "Seven days retention"}, admission("retention"))
	s.Prepare("developer", "T3")
	if err := s.Resolve("R2", "T3", []collaboration.Resolution{{QuestionID: "Q2", ResponseIDs: []string{"A2"}, Explanation: "Seven days"}}); err != nil {
		t.Fatal(err)
	}
	if s.Unresolved("developer") {
		t.Fatal("unchanged Q1 requires a redundant human answer")
	}
	if s.Rounds[1].Resolutions[0].ResponseIDs[0] != "A1" || s.Inputs[0].RoundID != "R1" || s.Inputs[0].Source.ID != "compatibility" {
		t.Fatal("inherited decision lost its original evidence")
	}
}
