package collaboration_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
)

func activeReplanning(t *testing.T) *collaboration.State {
	t.Helper()
	s := planningFixture(t)
	publishCheckpoint(t, s, "original", "turn-1", planningContent())
	admitPlanning(s, "original-consent", collaboration.Command{Kind: "implement", Target: "P1"})
	validateImplementation(t, s, "initial-validation")
	s.Planning.Implementation.FirstDeveloperTurnID = "first-developer"
	s.Planning.Implementation.AcceptedRevisionID = "original"
	s.Planning.Implementation.AuthorizationID = "original-consent"
	s.Prepare("developer", "source")
	if _, err := s.RequestReplanning(collaboration.ReplanningRequestScope{OperationID: "concern", TurnID: "source", ExecutionEpoch: 1, AttemptID: "attempt", DeveloperParticipantID: "developer", PlannerParticipantID: "planner", HeadSHA: strings.Repeat("b", 40)}, collaboration.ReplanningConcern{AcceptedRevisionID: "original", Commitment: "Transient status", Evidence: []string{"Restart loses a required decision record."}, Consequences: "The original persistence assumption is doubtful."}); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateReplanning("concern", "attempt"); err != nil {
		t.Fatal(err)
	}
	s.Prepare("planner", "resolution")
	s.Project("planner", "resolution")
	return s
}
func TestReplanningMaterialPairNeedsNewExactAuthorityAndPreservesFirstAdmission(t *testing.T) {
	s := activeReplanning(t)
	c := planningContent()
	c.Changes = "Retain only nonpersonal checklist decisions across restart"
	c.RequirementsBrief = "Retain nonpersonal decisions; no user identifiers."
	c.TechnicalPlan = "Persist a local decision record; test restart and reset."
	revised := publishCheckpoint(t, s, "revised", "resolution", c)
	if revised.Supersedes != "original" || s.Planning.Implementation.AcceptedRevisionID != "original" {
		t.Fatal("publication installed new authority", s)
	}
	admitPlanning(s, "confirmation-only", collaboration.Command{Kind: "confirm-plan", Target: revised.Reference})
	validateImplementation(t, s, "confirmed")
	resolution := collaboration.ReplanningResolution{OperationID: "resolved", TurnID: "confirmed", Disposition: "revised", RevisionID: "revised", RepositoryCommit: strings.Repeat("a", 40), Evidence: "Human agreed to minimal nonpersonal persistence."}
	if err := s.ResolveReplanning("planner", resolution); err == nil || s.ResumptionReady() {
		t.Fatal("confirmation-only resumed")
	}
	if r := admitPlanning(s, "obsolete", collaboration.Command{Kind: "implement", Target: "P1"}); r.ResponseID != "" {
		t.Fatal("old pair accepted", r)
	}
	if r := admitPlanning(s, "new-consent", collaboration.Command{Kind: "implement", Target: revised.Reference}); r.ResponseID == "" {
		t.Fatal(r)
	}
	s.Planning.ReadyRevision = "" // confirmation-only never queues a completed resolution
	validateImplementation(t, s, "authorized")
	resolution.TurnID = "authorized"
	if err := s.ResolveReplanning("planner", resolution); err != nil {
		t.Fatal(err)
	}
	if !s.ResumptionReady() {
		t.Fatal("exact revised authority not usable")
	}
	// Restart retains the queued grant without consuming or replacing the first.
	raw, _ := json.Marshal(s)
	var restored collaboration.State
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	s = &restored
	if r := admitPlanning(s, "withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: revised.Reference}); r.ResponseID == "" {
		t.Fatal(r)
	}
	if s.ResumptionReady() || s.AdmitResumption("must-not-start") == nil {
		t.Fatal("withdrawn resumption admitted")
	}
	admitPlanning(s, "new-consent", collaboration.Command{Kind: "implement", Target: revised.Reference})
	if s.ResumptionReady() {
		t.Fatal("duplicate consent restored withdrawal")
	}
	if err := s.ReconsiderReplanning(); err != nil {
		t.Fatal(err)
	}
	admitPlanning(s, "fresh-consent", collaboration.Command{Kind: "implement", Target: revised.Reference})
	validateImplementation(t, s, "fresh-validation")
	resolution.OperationID = "fresh-resolution"
	resolution.TurnID = "fresh-validation"
	if err := s.ResolveReplanning("planner", resolution); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitResumption("resumed-developer"); err != nil {
		t.Fatal(err)
	}
	b := s.Planning.Implementation
	if b.FirstDeveloperTurnID != "first-developer" || b.AcceptedRevisionID != "revised" || b.AuthorizationID != "fresh-consent" || len(b.Bindings) != 2 || b.Bindings[0].RevisionID != "original" {
		t.Fatal("lost admission provenance", b)
	}
	if r := admitPlanning(s, "late-withdrawal", collaboration.Command{Kind: "withdraw-implementation", Target: revised.Reference}); r.ResponseID != "" {
		t.Fatal("stopped consumed work", r)
	}
	s.Prepare("reviewer", "review")
	if len(s.Records("reviewer", "review", "replanning")) != 1 || len(s.Records("reviewer", "review", "accepted_plan")) != 1 {
		t.Fatal("lost review governing evidence")
	}
}

func TestReplanningUnchangedWithdrawnAuthorityNeedsFreshValidatedSamePairGrant(t *testing.T) {
	s := activeReplanning(t)
	v := collaboration.ReplanningResolution{OperationID: "unchanged", TurnID: "resolution", RevisionID: "original", RepositoryCommit: strings.Repeat("a", 40), Evidence: "The isolated concern is a false alarm.", Disposition: "unchanged"}
	if err := s.ResolveReplanning("planner", v); err != nil {
		t.Fatal(err)
	}
	admitPlanning(s, "withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: "P1"})
	if s.ResumptionReady() {
		t.Fatal("withdrawal did not fence unchanged resumption")
	}
	if err := s.ReconsiderReplanning(); err != nil {
		t.Fatal(err)
	}
	admitPlanning(s, "fresh-same-pair", collaboration.Command{Kind: "implement", Target: "P1"})
	if err := s.ResolveReplanning("planner", v); err == nil {
		t.Fatal("unassessed fresh input restored consumed authority")
	}
	validateImplementation(t, s, "fresh-same-validation")
	v.TurnID = "fresh-same-validation"
	v.OperationID = "fresh-same-resolution"
	if err := s.ResolveReplanning("planner", v); err != nil {
		t.Fatal(err)
	}
	if s.ActiveReplanning().Resolution.AuthorizationID != "fresh-same-pair" || !s.ResumptionReady() {
		t.Fatal("failed to select exact fresh authority", s)
	}
	if err := s.AdmitResumption("resumed"); err != nil {
		t.Fatal(err)
	}
	if len(s.Planning.Revisions) != 1 || s.Planning.Implementation.AcceptedRevisionID != "original" || s.Planning.Implementation.AuthorizationID != "fresh-same-pair" {
		t.Fatal("fabricated documents or restored old grant", s)
	}
}

func TestReplanningResumedConsentRetainsSourceOrdering(t *testing.T) {
	for _, lateWithdrawal := range []bool{false, true} {
		t.Run(map[bool]string{false: "withdraw-before-renewal", true: "late-older-withdrawal"}[lateWithdrawal], func(t *testing.T) {
			s := activeReplanning(t)
			content := planningContent()
			content.Changes = "Retain the required decision record across restart."
			publishCheckpoint(t, s, "revised", "resolution", content)
			a := admission("withdraw")
			a.ParticipantID = "planner"
			a.At = time.Now().Add(time.Minute)
			a.SourceSequence = 20
			withdrawal := a
			withdrawal.At = withdrawal.At.Add(time.Minute)
			if !lateWithdrawal {
				if r := s.Accept("withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: "P2"}, withdrawal); r.ResponseID == "" {
					t.Fatal(r)
				}
				older := a
				older.Source.ID = "older-consent"
				older.SourceSequence = 19
				older.At = older.At.Add(time.Hour)
				if r := s.Accept("older", collaboration.Command{Kind: "implement", Target: "P2"}, older); r.ResponseID != "" {
					t.Fatal("older delivery restored withdrawn authority", r)
				}
			}
			a.Source.ID = "newer"
			a.SourceSequence = 21
			if r := s.Accept("newer", collaboration.Command{Kind: "implement", Target: "P2"}, a); r.ResponseID == "" {
				t.Fatal(r)
			}
			validateImplementation(t, s, "validation")
			if lateWithdrawal {
				if r := s.Accept("withdraw", collaboration.Command{Kind: "withdraw-implementation", Target: "P2"}, withdrawal); r.ResponseID != "" {
					t.Fatal("old withdrawal revoked newer resumed consent", r)
				}
			}
			older := a
			older.Source.ID = "older-consent"
			older.SourceSequence = 19
			older.At = older.At.Add(time.Hour)
			if r := s.Accept("older", collaboration.Command{Kind: "implement", Target: "P2"}, older); r.ResponseID != "" {
				t.Fatal("late old authorization displaced fresh grant", r)
			}
			if !s.ImplementationValidated() || s.ImplementationGrant().ID != "newer" {
				t.Fatal(s.Planning.Implementation)
			}
		})
	}
}

func TestReplanningFreshConsentSurvivesEarlierInputReassessment(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared-batch", true: "historical-reassessment"}[historical], func(t *testing.T) {
			s := activeReplanning(t)
			admitPlanning(s, "initial", collaboration.Command{Kind: "implement", Target: "P1"})
			validateImplementation(t, s, "resumed-initial-validation")
			if err := s.ReconsiderReplanning(); err != nil {
				t.Fatal(err)
			}
			admitPlanning(s, "steering", collaboration.Command{Kind: "steer", Body: "Withdraw earlier acceptance; keep the same commitments."})
			s.Prepare("planner", "steering-turn")
			assess := func(op, turn string) {
				t.Helper()
				if err := s.Assess(op, "planner", turn, []collaboration.Assessment{{InputID: "steering", EvidenceIDs: []string{"steering"}, Disposition: "unchanged", Explanation: "Earlier acceptance withdrawn; document commitments remain unchanged.", InvalidateConfirmation: true}}); err != nil {
					t.Fatal(err)
				}
			}
			if historical {
				assess("earlier-assessment", "steering-turn")
			}
			admitPlanning(s, "fresh", collaboration.Command{Kind: "implement", Target: "P1"})
			s.Prepare("planner", "renewal-turn")
			assess("assessment-with-renewal", "renewal-turn")
			if err := s.Assess("confirm-fresh", "planner", "renewal-turn", []collaboration.Assessment{{InputID: "fresh", EvidenceIDs: []string{"fresh"}, Disposition: "confirmed", Explanation: "Fresh consent accepts the exact unchanged pair."}}); err != nil {
				t.Fatal(err)
			}
			validateImplementation(t, s, "renewal-validation")
			if err := s.ReconsiderReplanning(); err != nil {
				t.Fatal(err)
			}
			s.Prepare("planner", "historical-turn")
			assess("historical-assessment", "historical-turn")
			validateImplementation(t, s, "historical-validation")
			if !s.ImplementationValidated() || s.ImplementationGrant().ID != "fresh" {
				t.Fatal("historical assessment invalidated newer resumed consent", s.Planning.Implementation)
			}
			if err := s.ResolveReplanning("planner", collaboration.ReplanningResolution{OperationID: "resolution", TurnID: "historical-validation", RevisionID: "original", RepositoryCommit: strings.Repeat("a", 40), Evidence: "Commitments remain valid and fresh exact consent supersedes withdrawn authority.", Disposition: "unchanged"}); err != nil {
				t.Fatal(err)
			}
			if s.ActiveReplanning().Resolution.AuthorizationID != "fresh" {
				t.Fatal("silently restored consumed authority", s)
			}
		})
	}
}
