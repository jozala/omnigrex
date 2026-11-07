package collaboration_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
)

func TestPermanentReferencesSurviveFailureReplacementAndRestart(t *testing.T) {
	var s collaboration.State
	questions := []collaboration.Question{{ID: "internal-question", Body: "Keep updates structured? I recommend facts and links."}}
	first, err := s.Request("internal-round-one", "developer", "turn-one", "", questions)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reference != "R1" {
		t.Fatal(first)
	}
	if err = s.FailPublication(first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.Request("internal-round-two", "developer", "turn-two", "", questions)
	if err != nil {
		t.Fatal(err)
	}
	if second.Reference != "R2" {
		t.Fatal("failed reservation reused reference", second)
	}
	if err = s.Publish(second.ID, collaboration.Source{Channel: "fixture", ID: "questions"}); err != nil {
		t.Fatal(err)
	}
	a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: "developer", Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "answer"}, At: time.Now()}
	for _, target := range []string{"", first.Reference, second.ID, "R02", "R999"} {
		if receipt := s.Accept("rejected-"+target, collaboration.Command{Kind: "answer", Target: target, Body: "yes"}, a); receipt.ResponseID != "" {
			t.Fatal("bad target accepted", target)
		}
		a.Source.ID += "-next"
	}
	receipt := s.Accept("internal-input", collaboration.Command{Kind: "answer", Target: "R2", Body: "yes to both, except retain diagnostics locally"}, a)
	if receipt.ResponseID != "internal-input" || s.Inputs[0].Reference != "A1" || s.Inputs[0].RoundID != second.ID || strings.Contains(receipt.Feedback, "internal") {
		t.Fatal(receipt, s.Inputs)
	}
	raw, _ := json.Marshal(s)
	var restarted collaboration.State
	if err = json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	duplicate := restarted.Accept("different-id", collaboration.Command{Kind: "steer", Body: "different"}, a)
	if duplicate != receipt || len(restarted.Inputs) != 1 {
		t.Fatal("source dedup rebound input", duplicate)
	}
	// A rejection never applies to the current round implicitly, including after restart.
	a.Source.ID = "missing-target-after-restart"
	if r := restarted.Accept("later", collaboration.Command{Kind: "answer", Body: "yes"}, a); r.ResponseID != "" {
		t.Fatal(r)
	}
	if restarted.Inputs[0].Body != "yes to both, except retain diagnostics locally" {
		t.Fatal("accepted contents changed")
	}
}

func TestNaturalCorrectionsAndHistoricalConflictFenceEveryRole(t *testing.T) {
	for _, participant := range []string{"planner", "developer", "reviewer"} {
		t.Run(participant, func(t *testing.T) {
			var s collaboration.State
			round, err := s.Request("internal-round", participant, "turn-one", "", []collaboration.Question{{ID: "Q1", Body: "Keep API?"}, {ID: "Q2", Body: "Keep source links?"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Publish(round.ID, collaboration.Source{Channel: "fixture", ID: "questions"}); err != nil {
				t.Fatal(err)
			}
			a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: participant, Actor: collaboration.Actor{ID: "one", Name: "One"}, Source: collaboration.Source{Channel: "fixture", ID: "original"}, At: time.Now()}
			s.Accept("original", collaboration.Command{Kind: "answer", Target: "R1", Body: "yes to both"}, a)
			s.Prepare(participant, "turn-two")
			if !s.AssessmentBlocked(participant) {
				t.Fatal("delivery counted as consideration")
			}
			if err = s.Assess("assess-original", participant, "turn-two", []collaboration.Assessment{{InputID: "original", EvidenceIDs: []string{"original"}, Disposition: "changed", Explanation: "Both recommendations explicitly accepted."}}); err != nil {
				t.Fatal(err)
			}
			resolutions := []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"original"}, Explanation: "Keep API."}, {QuestionID: "Q2", ResponseIDs: []string{"original"}, Explanation: "Keep links."}}
			if err = s.Resolve(round.ID, "turn-two", resolutions); err != nil {
				t.Fatal(err)
			}
			// New frontier omits the settled historical decisions, without a checkpoint.
			next, err := s.Request("next-round", participant, "turn-two", "", []collaboration.Question{{ID: "Q3", Body: "Start locally?"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Publish(next.ID, collaboration.Source{Channel: "fixture", ID: "next"}); err != nil {
				t.Fatal(err)
			}
			a.Source.ID = "correction"
			s.Accept("correction", collaboration.Command{Kind: "steer", Body: "Actually change both earlier choices: remove API and links."}, a)
			s.Prepare(participant, "turn-three")
			correction := collaboration.Assessment{InputID: "correction", EvidenceIDs: []string{"correction"}, Corrects: []string{"original"}, Disposition: "changed", Explanation: "The same maintainer explicitly revises both earlier choices."}
			if err = s.Assess("assess-correction", participant, "turn-three", []collaboration.Assessment{correction}); err != nil {
				t.Fatal(err)
			}
			if !s.Unresolved(participant) {
				t.Fatal("historical correction disappeared outside current frontier")
			}
			for i := range resolutions {
				resolutions[i].ResponseIDs = []string{"correction"}
				resolutions[i].Explanation = "Explicit self-correction replaces this decision."
			}
			if err = s.Resolve(round.ID, "turn-three", resolutions); err != nil {
				t.Fatal("natural correction required fabricated amend", err)
			}
			// Another authorized human disagrees; the agent records, rather than the
			// application guesses, semantic conflict and its historical materiality.
			a.Actor = collaboration.Actor{ID: "two", Name: "Two"}
			a.Source.ID = "objection"
			s.Accept("objection", collaboration.Command{Kind: "steer", Body: "Keep the original API."}, a)
			s.Prepare(participant, "turn-four")
			conflict := collaboration.Assessment{InputID: "objection", EvidenceIDs: []string{"original", "correction", "objection"}, Disposition: "uncertain", Explanation: "Two authorized humans disagree about the historical API commitment; ask them to reconcile."}
			if err = s.Assess("assess-conflict", participant, "turn-four", []collaboration.Assessment{conflict}); err != nil {
				t.Fatal(err)
			}
			if !s.AssessmentBlocked(participant) {
				t.Fatal("historical conflict hidden by current frontier")
			}
			a.Source.ID = "answer-current"
			s.Accept("answer-current", collaboration.Command{Kind: "answer", Target: "R2", Body: "yes"}, a)
			s.Prepare(participant, "turn-five")
			if err = s.Assess("assess-current", participant, "turn-five", []collaboration.Assessment{{InputID: "answer-current", EvidenceIDs: []string{"answer-current"}, Disposition: "unchanged", Explanation: "Local first."}}); err != nil {
				t.Fatal(err)
			}
			if err = s.Resolve(next.ID, "turn-five", []collaboration.Resolution{{QuestionID: "Q3", ResponseIDs: []string{"answer-current"}, Explanation: "Local first."}}); err != nil {
				t.Fatal(err)
			}
			if s.Unresolved(participant) || !s.AssessmentBlocked(participant) {
				t.Fatal("current question resolution erased independent conflict")
			}
			a.Source.ID = "reconcile"
			s.Accept("reconcile", collaboration.Command{Kind: "steer", Body: "I accept One's revised proposal."}, a)
			s.Prepare(participant, "turn-six")
			conflict.Disposition = "unchanged"
			conflict.EvidenceIDs = append(conflict.EvidenceIDs, "reconcile")
			conflict.Explanation = "Two explicitly accepts One's self-revision; no competing direction remains."
			if err = s.Assess("assess-reconciliation", participant, "turn-six", []collaboration.Assessment{conflict, {InputID: "reconcile", EvidenceIDs: []string{"reconcile"}, Disposition: "unchanged", Explanation: "Explicit acceptance reconciles the conflict."}}); err != nil {
				t.Fatal(err)
			}
			if s.AssessmentBlocked(participant) || s.Unresolved(participant) {
				t.Fatal("reconciled evidence remained blocked")
			}
			if len(s.Inputs) != 5 || s.Inputs[0].Body != "yes to both" || !s.HasAssessmentEffect("assess-correction", []collaboration.Assessment{correction}) || s.HasAssessmentEffect("other-operation", []collaboration.Assessment{correction}) {
				t.Fatal("lost immutable evidence or operation provenance")
			}
		})
	}
}

func TestQualifiedWithdrawalAndRenewedExactConfirmation(t *testing.T) {
	for _, disposition := range []string{"changed", "uncertain"} {
		t.Run(disposition, func(t *testing.T) {
			s := planningFixture(t)
			publishCheckpoint(t, s, "internal-pair", "turn-1", planningContent())
			if r := admitPlanning(s, "initial-confirmation", collaboration.Command{Kind: "confirm-plan", Target: "P1", Body: "Yes, looks right."}); r.ResponseID == "" {
				t.Fatal(r)
			}
			s.Prepare("planner", "turn-2")
			assessment := collaboration.Assessment{InputID: "initial-confirmation", EvidenceIDs: []string{"initial-confirmation"}, Disposition: "confirmed", Explanation: "Harmless accompanying text accepts the exact published pair."}
			if err := s.Assess("assess-confirmation", "planner", "turn-2", []collaboration.Assessment{assessment}); err != nil {
				t.Fatal(err)
			}
			if err := s.ValidatePlanning("ready-before-objection", "internal-pair", "planner", "turn-2", strings.Repeat("b", 40), "Repository unchanged.", true); err != nil {
				t.Fatal(err)
			}
			admitPlanning(s, "objection", collaboration.Command{Kind: "steer", Body: "Withdraw my confirmation; reconsider retention."})
			s.Prepare("planner", "turn-3")
			objection := collaboration.Assessment{InputID: "objection", EvidenceIDs: []string{"objection"}, Disposition: disposition, Explanation: "Material withdrawal/objection to accepted commitments.", InvalidateConfirmation: true}
			if err := s.Assess("withdraw", "planner", "turn-3", []collaboration.Assessment{objection}); err != nil {
				t.Fatal(err)
			}
			if s.PlanningValidationCurrent(s.Planning.Revalidations[0]) {
				t.Fatal("old ready validation survived withdrawal")
			}
			objection.Disposition = "unchanged"
			objection.Explanation = "Investigation resolves the objection without changing the documents."
			objection.InvalidateConfirmation = false
			if err := s.Assess("resolve-objection", "planner", "turn-3", []collaboration.Assessment{objection}); err != nil {
				t.Fatal(err)
			}
			if err := s.ValidatePlanning("ready-again", "internal-pair", "planner", "turn-3", strings.Repeat("b", 40), "Unchanged pair remains relevant.", true); err == nil {
				t.Fatal("resolution resurrected old confirmation")
			}
			admitPlanning(s, "renewed", collaboration.Command{Kind: "confirm-plan", Target: "P1"})
			s.Prepare("planner", "turn-4")
			assessment.InputID = "renewed"
			assessment.EvidenceIDs = []string{"renewed"}
			if err := s.Assess("assess-renewed", "planner", "turn-4", []collaboration.Assessment{assessment}); err != nil {
				t.Fatal(err)
			}
			if err := s.ValidatePlanning("ready-renewed", "internal-pair", "planner", "turn-4", strings.Repeat("b", 40), "Fresh exact confirmation and unchanged documents.", true); err != nil {
				t.Fatal(err)
			}
			if !s.PlanningValidationCurrent(s.Planning.Revalidations[1]) || s.Inputs[0].Body != "Yes, looks right." || s.Planning.ReadyRevision != "" {
				t.Fatal("renewal lost history or granted premature readiness")
			}
		})
	}
	// A qualified confirmation requesting changes cannot confirm an unpublished
	// modified version, even when the application admits its natural-language body.
	s := planningFixture(t)
	publishCheckpoint(t, s, "pair", "turn-1", planningContent())
	admitPlanning(s, "qualified", collaboration.Command{Kind: "confirm-plan", Target: "P1", Body: "Yes, but change retention to seven days."})
	s.Prepare("planner", "turn-2")
	if err := s.Assess("qualified", "planner", "turn-2", []collaboration.Assessment{{InputID: "qualified", EvidenceIDs: []string{"qualified"}, Disposition: "changed", Explanation: "This requests a material change, not confirmation of an unpublished version."}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidatePlanning("ready", "pair", "planner", "turn-2", strings.Repeat("b", 40), "Changed commitments.", true); err == nil {
		t.Fatal("qualified input was automatic confirmation")
	}
}

func TestAssessmentBatchIsAtomicAndEvidenceBound(t *testing.T) {
	var s collaboration.State
	a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: "developer", Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "one"}, At: time.Now()}
	s.Accept("one", collaboration.Command{Kind: "steer", Body: "Keep scope small."}, a)
	s.Prepare("developer", "turn")
	good := collaboration.Assessment{InputID: "one", EvidenceIDs: []string{"one"}, Disposition: "unchanged", Explanation: "No change."}
	for i, bad := range []collaboration.Assessment{{InputID: "one", EvidenceIDs: []string{"not-delivered"}, Disposition: "unchanged", Explanation: "No evidence."}, {InputID: "one", EvidenceIDs: []string{"one"}, Disposition: "confirmed", Explanation: "An ordinary answer is not plan confirmation."}, {InputID: "one", EvidenceIDs: []string{"one"}, Disposition: "guessed", Explanation: "Invalid disposition."}} {
		if err := s.Assess(fmt.Sprint(i), "developer", "turn", []collaboration.Assessment{good, bad}); err == nil || len(s.Assessments) != 0 {
			t.Fatal("partial or invalid assessment applied", err)
		}
	}
	if err := s.Assess("good", "reviewer", "turn", []collaboration.Assessment{good}); err == nil {
		t.Fatal("cross-Role evidence admitted")
	}
}

func TestUnassessedInputAndHistoricalUncertaintyPermitOnlyIncompleteDraft(t *testing.T) {
	s := planningFixture(t)
	admitPlanning(s, "input", collaboration.Command{Kind: "steer", Body: "I am unsure about retention."})
	s.Prepare("planner", "turn-2")
	c := planningContent()
	if _, err := s.ReserveCheckpoint("blocked-unassessed", "planner", "turn-2", c); err == nil {
		t.Fatal("unassessed input allowed proposal")
	}
	if err := s.Assess("uncertain", "planner", "turn-2", []collaboration.Assessment{{InputID: "input", EvidenceIDs: []string{"input"}, Disposition: "uncertain", Explanation: "Retention is a material unresolved decision."}}); err != nil {
		t.Fatal(err)
	}
	c.Status = "draft"
	c.UnresolvedDecisions = []string{"Retention remains undecided."}
	draft, err := s.ReserveCheckpoint("draft", "planner", "turn-2", c)
	if err != nil {
		t.Fatal("explicitly incomplete draft rejected", err)
	}
	if err = s.ConfirmCheckpoint(draft.ID, collaboration.Source{Channel: "fixture", ID: "draft"}); err != nil {
		t.Fatal(err)
	}
	c.Status = "proposed"
	c.UnresolvedDecisions = nil
	if _, err = s.ReserveCheckpoint("blocked-historical", "planner", "turn-2", c); err == nil {
		t.Fatal("omitting uncertainty from the pair cleared the blocker")
	}
}

func TestMissingTargetsGiveUsableExamplesEvenWithoutAnswerContent(t *testing.T) {
	var s collaboration.State
	a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: "developer", Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "source"}, At: time.Now()}
	for _, kind := range []string{"answer", "amend", "confirm-plan", "invalid"} {
		a.Source.ID = kind
		r := s.Accept(kind, collaboration.Command{Kind: kind}, a)
		if r.ResponseID != "" || !strings.Contains(r.Feedback, "/omnigrex") || strings.Contains(r.Feedback, "internal-") {
			t.Fatal("missing target became opaque or inferred", r)
		}
	}
}

func TestAmendmentAfterNaturalCorrectionRetainsLineage(t *testing.T) {
	for _, role := range []string{"planner", "developer", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			var s collaboration.State
			r, err := s.Request("round", role, "t1", "", []collaboration.Question{{ID: "Q1", Body: "Keep API?"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Publish(r.ID, collaboration.Source{Channel: "fixture", ID: "round"}); err != nil {
				t.Fatal(err)
			}
			a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: role, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "original"}, At: time.Now()}
			accept := func(id string, c collaboration.Command) {
				t.Helper()
				a.Source.ID = id
				if receipt := s.Accept(id, c, a); receipt.ResponseID == "" {
					t.Fatal(receipt)
				}
				s.Prepare(role, id)
			}
			resolve := func(id string, ids ...string) {
				t.Helper()
				if err := s.Resolve(r.ID, id, []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: ids, Explanation: "Explicit governing evidence."}}); err != nil {
					t.Fatal(err)
				}
			}
			accept("original", collaboration.Command{Kind: "answer", Target: "R1", Body: "yes"})
			if err := s.Resolve(r.ID, "original", []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"original"}, Explanation: "Keep API."}}); err != nil {
				t.Fatal(err)
			}
			accept("correction", collaboration.Command{Kind: "steer", Body: "Actually remove API only."})
			if err := s.Assess("correct", role, "correction", []collaboration.Assessment{{InputID: "correction", EvidenceIDs: []string{"correction"}, Corrects: []string{"original"}, Disposition: "changed", Explanation: "Self correction."}}); err != nil {
				t.Fatal(err)
			}
			resolve("correction", "correction")
			accept("amendment", collaboration.Command{Kind: "amend", Target: "A1", Body: "Revise my original answer: keep API."})
			if err := s.Assess("amend", role, "amendment", []collaboration.Assessment{{InputID: "amendment", EvidenceIDs: []string{"amendment"}, Disposition: "changed", Explanation: "Explicit amendment of the original."}}); err != nil {
				t.Fatal(err)
			}
			if !s.Unresolved(role) {
				t.Fatal("amendment lost the natural correction lineage")
			}
			resolve("amendment", "amendment")
			if s.Unresolved(role) {
				t.Fatal("explicit reconciliation remained fenced")
			}
			raw, _ := json.Marshal(s)
			s = collaboration.State{}
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			if s.Unresolved(role) {
				t.Fatal("lineage lost after persistence")
			}
		})
	}
}

func TestEmptyCorrectionsRetainAssessmentProofAfterPersistence(t *testing.T) {
	var s collaboration.State
	a := collaboration.Admission{Active: true, Authorized: true, ParticipantID: "developer", Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "input"}, At: time.Now()}
	s.Accept("input", collaboration.Command{Kind: "steer", Body: "Keep scope."}, a)
	s.Prepare("developer", "turn")
	batch := []collaboration.Assessment{{InputID: "input", EvidenceIDs: []string{"input"}, Corrects: []string{}, Disposition: "unchanged", Explanation: "No correction."}}
	if err := s.Assess("operation", "developer", "turn", batch); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	s = collaboration.State{}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if !s.HasAssessmentEffect("operation", batch) {
		t.Fatal("empty optional corrections lost operation proof")
	}
}

func TestRenewalSurvivesEarlierWithdrawalInSharedBatch(t *testing.T) {
	for _, afterPreparation := range []bool{false, true} {
		t.Run(fmt.Sprint(afterPreparation), func(t *testing.T) {
			s := planningFixture(t)
			publishCheckpoint(t, s, "pair", "turn-1", planningContent())
			admitPlanning(s, "initial", collaboration.Command{Kind: "confirm-plan", Target: "P1"})
			s.Prepare("planner", "turn-2")
			assessment := func(id, disposition string, invalidate bool) collaboration.Assessment {
				return collaboration.Assessment{InputID: id, EvidenceIDs: []string{id}, Disposition: disposition, Explanation: "Explicit interpretation of exact input.", InvalidateConfirmation: invalidate}
			}
			if err := s.Assess("initial", "planner", "turn-2", []collaboration.Assessment{assessment("initial", "confirmed", false)}); err != nil {
				t.Fatal(err)
			}
			admitPlanning(s, "withdrawal", collaboration.Command{Kind: "steer", Body: "Withdraw confirmation."})
			if afterPreparation {
				s.Prepare("planner", "turn-3")
			}
			admitPlanning(s, "renewal", collaboration.Command{Kind: "confirm-plan", Target: "P1", Body: "I reconsidered; I accept the unchanged complete pair."})
			s.Prepare("planner", "turn-3")
			if err := s.Assess("withdraw", "planner", "turn-3", []collaboration.Assessment{assessment("withdrawal", "unchanged", true)}); err != nil {
				t.Fatal(err)
			}
			if err := s.ValidatePlanning("old", "pair", "planner", "turn-3", strings.Repeat("b", 40), "Unchanged.", true); err == nil {
				t.Fatal("old approval resurrected")
			}
			renewalTurn := "turn-3"
			if afterPreparation {
				renewalTurn = "turn-4"
				s.Prepare("planner", renewalTurn)
			}
			if err := s.Assess("renew", "planner", renewalTurn, []collaboration.Assessment{assessment("renewal", "confirmed", false)}); err != nil {
				t.Fatal(err)
			}
			if err := s.ValidatePlanning("fresh", "pair", "planner", renewalTurn, strings.Repeat("b", 40), "Unchanged; fresh exact approval.", true); err != nil {
				t.Fatal(err)
			}
			if len(s.Planning.InvalidatedConfirmations) != 1 || s.Planning.InvalidatedConfirmations[0] != "initial" {
				t.Fatal(s.Planning.InvalidatedConfirmations)
			}
		})
	}
}

func TestAmendmentOfLaterRoundAnswerCanReviseHistoricalCorrection(t *testing.T) {
	for _, role := range []string{"planner", "developer", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			var s collaboration.State
			request := func(id, turn, question string) collaboration.Round {
				t.Helper()
				r, err := s.Request(id, role, turn, "", []collaboration.Question{{ID: question, Body: "Keep this recommendation?"}})
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Publish(id, collaboration.Source{Channel: "fixture", ID: id}); err != nil {
					t.Fatal(err)
				}
				return r
			}
			accept := func(id, participant string, command collaboration.Command) {
				t.Helper()
				receipt := s.Accept(id, command, collaboration.Admission{Active: true, Authorized: true, ParticipantID: participant, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: id}, At: time.Now()})
				if receipt.ResponseID != id {
					t.Fatal(receipt)
				}
			}
			assess := func(id string, corrects ...string) {
				t.Helper()
				s.Prepare(role, id)
				if err := s.Assess("assess-"+id, role, id, []collaboration.Assessment{{InputID: id, EvidenceIDs: []string{id}, Corrects: corrects, Disposition: "changed", Explanation: "Explicit interpretation of the admitted input."}}); err != nil {
					t.Fatal(err)
				}
			}
			resolution := func(question string, evidence ...string) []collaboration.Resolution {
				return []collaboration.Resolution{{QuestionID: question, ResponseIDs: evidence, Explanation: "The explicit amendment governs; earlier interpretations remain history."}}
			}
			resolve := func(round, turn, question, evidence string) {
				t.Helper()
				if err := s.Resolve(round, turn, resolution(question, evidence)); err != nil {
					t.Fatal(err)
				}
			}
			first := request("first", "initial", "Q1")
			accept("original", role, collaboration.Command{Kind: "answer", Target: first.Reference, Body: "yes"})
			assess("original")
			resolve(first.ID, "original", "Q1", "original")
			second := request("second", "original", "Q2")
			accept("correction", role, collaboration.Command{Kind: "answer", Target: second.Reference, Body: "Yes to Q2, but revise the earlier Q1 decision."})
			assess("correction", "original")
			resolve(first.ID, "correction", "Q1", "correction")
			resolve(second.ID, "correction", "Q2", "correction")
			if s.Unresolved(role) || s.AssessmentBlocked(role) {
				t.Fatal("governing correction was not current")
			}

			accept("amendment", role, collaboration.Command{Kind: "amend", Target: "A2", Body: "Revise that answer, including its earlier correction."})
			if s.Inputs[2].RoundID != second.ID {
				t.Fatal("amendment lost its actual addressed round")
			}
			if err := s.Resolve(first.ID, "correction", resolution("Q1", "amendment")); err == nil {
				t.Fatal("undelivered evidence bypassed the scope check")
			}
			assess("amendment")
			if !s.Unresolved(role) {
				t.Fatal("amendment failed to fence governing interpretations")
			}
			s.Inputs[2].DeliveryPending = true
			if err := s.Resolve(first.ID, "amendment", resolution("Q1", "amendment")); err == nil {
				t.Fatal("unread evidence bypassed the scope check")
			}
			s.Inputs[2].DeliveryPending = false

			// A valid amendment cannot launder unrelated or another Participant's evidence.
			accept("unrelated", role, collaboration.Command{Kind: "answer", Target: second.Reference, Body: "Another Q2 answer only."})
			assess("unrelated")
			accept("foreign", "another-participant", collaboration.Command{Kind: "steer", Body: "Other Stage input."})
			s.Prepare("another-participant", "foreign")
			for _, invalid := range []string{"unrelated", "foreign"} {
				if err := s.Resolve(first.ID, "unrelated", resolution("Q1", "amendment", invalid)); err == nil {
					t.Fatalf("accepted out-of-scope %s evidence", invalid)
				}
			}
			resolve(second.ID, "unrelated", "Q2", "amendment")
			if !s.Unresolved(role) {
				t.Fatal("current round resolution silently cleared the historical decision")
			}
			// Persistence must preserve lineage without inventing Corrects on the amendment.
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			s = collaboration.State{}
			if err = json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			resolve(first.ID, "unrelated", "Q1", "amendment")
			if s.Unresolved(role) || s.AssessmentBlocked(role) {
				t.Fatal("valid recorded amendment remained fenced")
			}
			if s.Inputs[1].Body != "Yes to Q2, but revise the earlier Q1 decision." || s.Inputs[2].Amends != "correction" || len(s.Assessments[2].Corrects) != 0 {
				t.Fatal("source or correction relationships rewritten")
			}
		})
	}
}
