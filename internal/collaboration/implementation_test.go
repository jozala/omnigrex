package collaboration_test

import (
	"encoding/json"
	"github.com/jozala/omnigrex/internal/collaboration"
	"strings"
	"testing"
	"time"
)

func validateImplementation(t *testing.T, s *collaboration.State, turn string) {
	t.Helper()
	batch := s.Prepare("planner", turn)
	for _, in := range batch {
		disposition := "unchanged"
		if in.Kind == "implement" || in.Kind == "confirm-plan" {
			disposition = "confirmed"
		}
		if err := s.Assess("assess-"+in.ID, "planner", turn, []collaboration.Assessment{{InputID: in.ID, EvidenceIDs: []string{in.ID}, Disposition: disposition, Explanation: "Exact commitments remain applicable; no material change."}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ValidatePlanning("validate-"+turn, s.Planning.CurrentRevision, "planner", turn, strings.Repeat("a", 40), "Repository movement does not change the accepted behavior or commitments.", true); err != nil {
		t.Fatal(err)
	}
	s.Planning.ReadyRevision = s.Planning.CurrentRevision
}
func TestExactImplementationConsentWithdrawalRestartAndAdmission(t *testing.T) {
	s := planningFixture(t)
	publishCheckpoint(t, s, "pair-one", "turn-1", planningContent())
	for i, c := range []collaboration.Command{{Kind: "answer", Target: "P1", Body: "yes"}, {Kind: "implement", Target: "P2"}, {Kind: "implement", Target: "P1", Body: "But change storage"}} {
		if r := admitPlanning(s, string(rune('x'+i)), c); r.ResponseID != "" {
			t.Fatal("invalid consent admitted", r)
		}
	}
	admitPlanning(s, "confirm-only", collaboration.Command{Kind: "confirm-plan", Target: "P1"})
	validateImplementation(t, s, "confirm")
	if s.ImplementationGrant() != nil || s.ImplementationValidated() {
		t.Fatal("upgrade or confirmation granted implementation")
	}
	// A pre-feature ready pair is usable unchanged, without another checkpoint.
	s.Planning.ReadyRevision = "pair-one"
	r := admitPlanning(s, "authorize", collaboration.Command{Kind: "implement", Target: "P1"})
	if r.ResponseID == "" || s.ImplementationValidated() {
		t.Fatal("receipt implied validation", r)
	}
	s.Planning.ReadyRevision = ""
	validateImplementation(t, s, "authorize-turn")
	if !s.ImplementationValidated() || len(s.Planning.Revisions) != 1 {
		t.Fatal("same pair did not authorize")
	}
	raw, _ := json.Marshal(s)
	var restored collaboration.State
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	s = &restored
	if !s.ImplementationValidated() {
		t.Fatal("restart expired authorization")
	}
	withdraw := admitPlanning(s, "withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: "P1"})
	if withdraw.ResponseID == "" || s.ImplementationGrant() != nil || s.Planning.ReadyRevision != "pair-one" {
		t.Fatal("withdrawal altered confirmation or left authority", withdraw)
	}
	// Replaying the original source cannot restore withdrawal.
	admitPlanning(s, "authorize", collaboration.Command{Kind: "implement", Target: "P1"})
	if s.ImplementationGrant() != nil {
		t.Fatal("source replay restored withdrawn grant")
	}
	admitPlanning(s, "fresh", collaboration.Command{Kind: "implement", Target: "P1"})
	s.Planning.ReadyRevision = ""
	validateImplementation(t, s, "fresh-turn")
	if !s.ImplementationValidated() {
		t.Fatal("fresh explicit consent failed")
	}
	s.Planning.Implementation.FirstDeveloperTurnID = "developer-turn"
	if r := admitPlanning(s, "too-late", collaboration.Command{Kind: "withdraw-implementation", Target: "P1"}); r.ResponseID != "" || !strings.Contains(r.Feedback, "close the original") {
		t.Fatal("claimed to stop admitted work", r)
	}
}
func TestKnownInputAndMaterialRevisionFenceImplementation(t *testing.T) {
	for _, material := range []bool{false, true} {
		t.Run(map[bool]string{false: "immaterial", true: "material"}[material], func(t *testing.T) {
			s := planningFixture(t)
			publishCheckpoint(t, s, "pair-one", "turn-1", planningContent())
			admitPlanning(s, "authorize", collaboration.Command{Kind: "implement", Target: "P1"})
			validateImplementation(t, s, "authorized")
			admitPlanning(s, "steer", collaboration.Command{Kind: "steer", Body: "Correct an evidence link, or change retention."})
			if s.ImplementationValidated() {
				t.Fatal("unassessed input inherited acceptance")
			}
			s.Planning.ReadyRevision = ""
			s.Prepare("planner", "revision")
			if err := s.Assess("assessment", "planner", "revision", []collaboration.Assessment{{InputID: "steer", EvidenceIDs: []string{"steer"}, Disposition: "unchanged", Explanation: "Evidence assessed explicitly.", InvalidateConfirmation: material}}); err != nil {
				t.Fatal(err)
			}
			if material {
				if s.ImplementationGrant() != nil {
					t.Fatal("material change retained grant")
				}
				publishCheckpoint(t, s, "pair-two", "revision", planningContent())
				if s.ImplementationGrant() != nil {
					t.Fatal("N+1 inherited N acceptance")
				}
				if r := admitPlanning(s, "old", collaboration.Command{Kind: "implement", Target: "P1"}); r.ResponseID != "" {
					t.Fatal("superseded consent")
				}
			} else {
				validateImplementation(t, s, "revalidation")
				if !s.ImplementationValidated() {
					t.Fatal("immaterial evidence forced renewed authorization")
				}
			}
		})
	}
}
func TestUnpublishedAndUnauthorizedCannotAuthorize(t *testing.T) {
	s := planningFixture(t)
	if _, err := s.ReserveCheckpoint("pair", "planner", "turn-1", planningContent()); err != nil {
		t.Fatal(err)
	}
	if r := admitPlanning(s, "not-published", collaboration.Command{Kind: "implement", Target: "P1"}); r.ResponseID != "" {
		t.Fatal(r)
	}
	if err := s.ConfirmCheckpoint("pair", collaboration.Source{Channel: "fixture", ID: "published"}); err != nil {
		t.Fatal(err)
	}
	a := admission("unauthorized")
	a.ParticipantID = "planner"
	a.Authorized = false
	if r := s.Accept("denied", collaboration.Command{Kind: "implement", Target: "P1"}, a); r.ResponseID != "" {
		t.Fatal(r)
	}
	a.Authorized = true
	a.Active = false
	a.Source.ID = "closed"
	if r := s.Accept("closed", collaboration.Command{Kind: "implement", Target: "P1"}, a); r.ResponseID != "" {
		t.Fatal(r)
	}
}

func TestDelayedEarlierAuthorizationCannotUndoWithdrawal(t *testing.T) {
	s := planningFixture(t)
	publishCheckpoint(t, s, "pair", "turn-1", planningContent())
	a := admission("withdrawal")
	a.ParticipantID = "planner"
	a.SourceSequence = 20
	if r := s.Accept("withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: "P1"}, a); r.ResponseID == "" {
		t.Fatal(r)
	}
	// A command created before withdrawal but delivered afterward is not fresh consent.
	a.Source.ID = "delayed-authorization"
	a.SourceSequence = 19
	a.At = a.At.Add(time.Hour)
	if r := s.Accept("delayed", collaboration.Command{Kind: "implement", Target: "P1"}, a); r.ResponseID != "" || !strings.Contains(r.Feedback, "predates withdrawal") {
		t.Fatal(r)
	}
	a.Source.ID = "fresh-authorization"
	a.SourceSequence = 21
	if r := s.Accept("fresh", collaboration.Command{Kind: "implement", Target: "P1"}, a); r.ResponseID == "" {
		t.Fatal(r)
	}
}

func TestAcceptedHandoffIsFocusedWithoutLosingSupportingHistory(t *testing.T) {
	s := planningFixture(t)
	publishCheckpoint(t, s, "pair", "turn-1", planningContent())
	admitPlanning(s, "authorize", collaboration.Command{Kind: "implement", Target: "P1"})
	validateImplementation(t, s, "validation")
	s.Planning.Implementation.FirstDeveloperTurnID = "first-developer"
	s.Planning.Implementation.AcceptedRevisionID = "pair"
	projected := s.Project("developer", "first-developer")
	if !strings.Contains(string(projected["accepted_plan"]), planningContent().RequirementsBrief) || string(projected["human_input_history"]) != "[]" || string(projected["planning_history"]) != "[]" {
		t.Fatal("handoff lost exact pair or dumped all discussion", projected)
	}
	if len(s.Records("developer", "first-developer", "human_input_history")) != 1 || len(s.Records("developer", "first-developer", "planning_history")) != 1 {
		t.Fatal("focused orientation removed lossless original sources")
	}
}

func TestImplementationRenewalSurvivesEarlierInputAssessment(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared-batch", true: "historical-reassessment"}[historical], func(t *testing.T) {
			s := planningFixture(t)
			publishCheckpoint(t, s, "pair", "turn-1", planningContent())
			admitPlanning(s, "initial", collaboration.Command{Kind: "implement", Target: "P1"})
			validateImplementation(t, s, "initial-validation")
			s.Planning.ReadyRevision = ""
			admitPlanning(s, "steering", collaboration.Command{Kind: "steer", Body: "Withdraw earlier acceptance."})
			s.Prepare("planner", "steering-turn")
			assess := func(operation, turn string) {
				t.Helper()
				if err := s.Assess(operation, "planner", turn, []collaboration.Assessment{{InputID: "steering", EvidenceIDs: []string{"steering"}, Disposition: "unchanged", Explanation: "Earlier acceptance withdrawn; documents remain unchanged.", InvalidateConfirmation: true}}); err != nil {
					t.Fatal(err)
				}
			}
			if historical {
				assess("first-assessment", "steering-turn")
			}
			admitPlanning(s, "fresh-consent", collaboration.Command{Kind: "implement", Target: "P1"})
			s.Prepare("planner", "renewal-turn")
			assess("assessment-with-renewal", "renewal-turn")
			if err := s.Assess("confirm-renewal", "planner", "renewal-turn", []collaboration.Assessment{{InputID: "fresh-consent", EvidenceIDs: []string{"fresh-consent"}, Disposition: "confirmed", Explanation: "Fresh consent accepts the unchanged pair."}}); err != nil {
				t.Fatal(err)
			}
			validateImplementation(t, s, "renewal-validation")
			if !s.ImplementationValidated() || s.ImplementationGrant() == nil || s.ImplementationGrant().ID != "fresh-consent" {
				t.Fatal("earlier assessment invalidated later consent", s.Planning.Implementation)
			}
			s.Planning.ReadyRevision = ""
			s.Prepare("planner", "historical-turn")
			assess("reassessment", "historical-turn")
			validateImplementation(t, s, "historical-validation")
			if !s.ImplementationValidated() {
				t.Fatal("historical reassessment revoked newer consent")
			}
			if len(s.Planning.Implementation.Invalidated) != 1 || s.Planning.Implementation.Invalidated[0] != "initial" {
				t.Fatal("earlier authority not permanently invalidated", s.Planning.Implementation)
			}
		})
	}
}

func TestImplementationSourceOrderDominatesReceiptTimeAndLateWithdrawal(t *testing.T) {
	for _, lateWithdrawal := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry-newer-consent", true: "late-older-withdrawal"}[lateWithdrawal], func(t *testing.T) {
			s := planningFixture(t)
			publishCheckpoint(t, s, "pair", "turn-1", planningContent())
			a := admission("withdraw")
			a.ParticipantID = "planner"
			a.SourceSequence = 20
			withdrawal := a
			withdrawal.At = withdrawal.At.Add(time.Minute)
			a.Source.ID = "newer"
			a.SourceSequence = 21
			if !lateWithdrawal {
				if r := s.Accept("withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: "P1"}, withdrawal); r.ResponseID == "" {
					t.Fatal(r)
				}
			}
			if r := s.Accept("newer", collaboration.Command{Kind: "implement", Target: "P1"}, a); r.ResponseID == "" {
				t.Fatal("newer source consent rejected", r)
			}
			validateImplementation(t, s, "validation")
			if lateWithdrawal {
				if r := s.Accept("withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: "P1"}, withdrawal); r.ResponseID != "" {
					t.Fatal("older withdrawal accepted", r)
				}
			}
			raw, _ := json.Marshal(s)
			var restored collaboration.State
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if !restored.ImplementationValidated() || restored.ImplementationGrant().ID != "newer" {
				t.Fatal("delivery order displaced newer consent", restored.Planning.Implementation)
			}
			// Older consent also cannot displace a newer grant or restore withdrawn authority.
			a.Source.ID = "old"
			a.SourceSequence = 19
			a.At = a.At.Add(time.Hour)
			if r := restored.Accept("old", collaboration.Command{Kind: "implement", Target: "P1"}, a); r.ResponseID != "" {
				t.Fatal("older consent accepted", r)
			}
		})
	}
}
