package collaboration_test

import (
	"fmt"
	"github.com/jozala/omnigrex/internal/collaboration"
	"reflect"
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

func TestSiblingAmendmentsInvalidateDecisionUntilExplicitInterpretation(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "h", []collaboration.Question{{ID: "Q1", Body: "Policy?"}})
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub"})
	s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Keep"}, admission("a1"))
	s.Prepare("developer", "T2")
	if err := s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Keep"}}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"A2", "A3", "A4"} {
		s.Accept(id, collaboration.Command{Kind: "amend", Target: "A1", Body: "Explicit correction " + id}, admission(id))
		// A correction received after preparation cannot be acknowledged from IDs
		// alone, even when it addresses the same original evidence.
		before := len(s.Rounds[0].Resolutions)
		if err := s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{id}, Explanation: "Not delivered yet"}}); err == nil {
			t.Fatal("unread correction was interpreted")
		}
		if len(s.Rounds[0].Resolutions) != before {
			t.Fatal("rejected interpretation changed history")
		}
		s.Prepare("developer", fmt.Sprint("correction-", i))
		if !s.Unresolved("developer") {
			t.Fatal("sibling correction silently retained the prior interpretation")
		}
		if err := s.Resolve("R1", fmt.Sprint("correction-", i), []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{id}, Explanation: "This explicit correction governs; earlier corrections remain history"}}); err != nil {
			t.Fatal(err)
		}
		if s.Unresolved("developer") {
			t.Fatal("explicit superseding interpretation rejected")
		}
	}
	if len(s.Rounds[0].Resolutions) != 4 || s.Inputs[0].Body != "Keep" {
		t.Fatal("history rewritten")
	}
}

func TestResolvedQuestionCanBecomeObsoleteWithoutHumanAmendment(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "h", []collaboration.Question{{ID: "Q1", Body: "Artifact retention?"}})
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub"})
	s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Seven days"}, admission("a1"))
	s.Prepare("developer", "T2")
	s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Seven days"}})
	if err := s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", Obsolete: true, Explanation: "Current code removes the artifact and its retention configuration entirely"}}); err != nil {
		t.Fatal(err)
	}
	if s.Unresolved("developer") || len(s.Rounds[0].Resolutions) != 2 || s.Rounds[0].Resolutions[0].ResponseIDs[0] != "A1" {
		t.Fatal("obsolescence lost earlier evidence")
	}
}

func TestPreviouslyRecordedInterpretationRetainsAllCitedEvidence(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "h", []collaboration.Question{{ID: "Q1", Body: "Policy?"}})
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub"})
	s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Keep"}, admission("a1"))
	s.Accept("A2", collaboration.Command{Kind: "amend", Target: "A1", Body: "Keep with qualification"}, admission("a2"))
	s.Prepare("developer", "T2")
	if err := s.Resolve("R1", "T2", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1", "A2"}, Explanation: "The explicit qualification applies to the original answer"}}); err != nil {
		t.Fatal(err)
	}
	// Interpretations written before coverage metadata existed remain effective
	// when all competing evidence is explicitly cited or is its ancestor.
	s.Rounds[0].Resolutions[0].ConsideredResponseIDs = nil
	if s.Unresolved("developer") {
		t.Fatal("previously cited evidence was treated as unseen")
	}
	s.Accept("A3", collaboration.Command{Kind: "amend", Target: "A1", Body: "Further correction"}, admission("a3"))
	if !s.Unresolved("developer") {
		t.Fatal("new sibling correction failed to invalidate legacy evidence")
	}
}

// A question's identity follows its unchanged premise, not the temporary fact
// that repository evidence once made it irrelevant.
func TestObsoleteQuestionCanReceiveRenewedInterpretation(t *testing.T) {
	for _, participant := range []string{"developer", "reviewer"} {
		for _, route := range []string{"same round", "replacement round", "historical round"} {
			for _, evidence := range []string{"amendment", "new answer", "prior answer"} {
				t.Run(participant+"/"+route+"/"+evidence, func(t *testing.T) {
					var s collaboration.State
					questions := []collaboration.Question{{ID: "Q1", Body: "How long must this artifact be retained?"}}
					a := admission("original")
					a.ParticipantID = participant
					if _, err := s.Request("R1", participant, "T1", "head1", questions); err != nil {
						t.Fatal(err)
					}
					if err := s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub1"}); err != nil {
						t.Fatal(err)
					}
					if receipt := s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Seven days"}, a); receipt.ResponseID != "A1" {
						t.Fatal(receipt)
					}
					s.Prepare(participant, "T2")
					original := collaboration.Resolution{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "The maintainer requires seven days"}
					obsolete := collaboration.Resolution{QuestionID: "Q1", Obsolete: true, Explanation: "Current repository evidence removes the artifact entirely"}
					for _, resolution := range []collaboration.Resolution{original, obsolete} {
						if err := s.Resolve("R1", "T2", []collaboration.Resolution{resolution}); err != nil {
							t.Fatal(err)
						}
					}
					originalInput := s.Inputs[0]
					target, current := "R1", "R1"
					if route != "same round" {
						if _, err := s.Request("R2", participant, "T2", "restored-premise-head", questions); err != nil {
							t.Fatal(err)
						}
						if err := s.Publish("R2", collaboration.Source{Channel: "fixture", ID: "pub2"}); err != nil {
							t.Fatal(err)
						}
						current = "R2"
						if route == "replacement round" {
							target = "R2"
						}
					}
					response := "A1"
					if evidence != "prior answer" {
						response = "A2"
						command := collaboration.Command{Kind: "amend", Target: "A1", Body: "The restored artifact must be retained fourteen days"}
						if evidence == "new answer" {
							command.Kind = "answer"
							command.Target = current
						}
						a.Source.ID = "renewed"
						if receipt := s.Accept(response, command, a); receipt.ResponseID != response {
							t.Fatal(receipt)
						}
					}
					// The renewed interpretation needs evidence and an explanation even though
					// there is no governing human evidence in the obsolete entry to amend.
					before := len(s.Rounds[0].Resolutions)
					if err := s.Resolve(target, "T2", []collaboration.Resolution{{QuestionID: "Q1", Explanation: "The artifact is restored"}}); err == nil {
						t.Fatal("renewal without supporting evidence was accepted")
					}
					renewal := collaboration.Resolution{QuestionID: "Q1", ResponseIDs: []string{response}, Explanation: "The current Change Proposal restores this unchanged artifact; this delivered answer governs retention"}
					if evidence != "prior answer" {
						if err := s.Resolve(target, "T2", []collaboration.Resolution{renewal}); err == nil {
							t.Fatal("undelivered evidence was accepted")
						}
					}
					if len(s.Rounds[0].Resolutions) != before {
						t.Fatal("rejected renewal changed history")
					}
					s.Prepare(participant, "T3")
					if err := s.Resolve(target, "T3", []collaboration.Resolution{renewal}); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(s.Rounds[0].Resolutions[:2], []collaboration.Resolution{original, obsolete}) || !reflect.DeepEqual(s.Inputs[0], originalInput) {
						t.Fatal("renewal rewrote original answer or interpretations")
					}
					for _, round := range s.Rounds {
						if round.ID == current {
							last := round.Resolutions[len(round.Resolutions)-1]
							if last.Obsolete || !reflect.DeepEqual(last.ResponseIDs, []string{response}) || last.Explanation != renewal.Explanation {
								t.Fatal("obsolete entry still governs current round", round)
							}
						}
					}
					// A later replacement must inherit the renewed decision, not the obsolete
					// one, and an unsupported ordinary answer must not replace live evidence.
					if _, err := s.Request("R3", participant, "T3", "head3", questions); err != nil {
						t.Fatal(err)
					}
					if err := s.Publish("R3", collaboration.Source{Channel: "fixture", ID: "pub3"}); err != nil {
						t.Fatal(err)
					}
					last := s.Rounds[len(s.Rounds)-1].Resolutions[0]
					if last.Obsolete || last.ResponseIDs[0] != response || s.Unresolved(participant) {
						t.Fatal("replacement inherited stale obsolescence")
					}
					a.Source.ID = "unrelated"
					s.Accept("A3", collaboration.Command{Kind: "answer", Target: "R3", Body: "An unrelated new answer"}, a)
					s.Prepare(participant, "T4")
					if err := s.Resolve("R3", "T4", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A3"}, Explanation: "Do not silently replace a live decision"}}); err == nil {
						t.Fatal("renewal relaxed amendment requirements for live decisions")
					}
					// An archived obsolete entry must not become a bypass around the
					// current round's renewed governing evidence through propagation.
					originalCount, currentCount := len(s.Rounds[0].Resolutions), len(s.Rounds[len(s.Rounds)-1].Resolutions)
					if err := s.Resolve("R1", "T4", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A3"}, Explanation: "Do not bypass the current live decision through history"}}); err == nil {
						t.Fatal("archived obsolescence bypassed the current live decision")
					}
					if len(s.Rounds[0].Resolutions) != originalCount || len(s.Rounds[len(s.Rounds)-1].Resolutions) != currentCount {
						t.Fatal("rejected historical interpretation changed history")
					}
					a.Source.ID = "supported-correction"
					if receipt := s.Accept("A4", collaboration.Command{Kind: "amend", Target: response, Body: "An explicit correction to the renewed decision"}, a); receipt.ResponseID != "A4" {
						t.Fatal(receipt)
					}
					s.Prepare(participant, "T5")
					if err := s.Resolve("R1", "T5", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A4"}, Explanation: "The explicit correction governs both the historical and current unchanged question"}}); err != nil {
						t.Fatal(err)
					}
					latest := s.Rounds[len(s.Rounds)-1].Resolutions[len(s.Rounds[len(s.Rounds)-1].Resolutions)-1]
					if latest.Obsolete || latest.ResponseIDs[0] != "A4" || s.Unresolved(participant) {
						t.Fatal("supported historical correction failed to propagate")
					}

				})
			}
		}
	}
}
