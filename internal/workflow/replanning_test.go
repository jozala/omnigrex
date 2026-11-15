package workflow_test

import (
	"github.com/jozala/omnigrex/internal/workflow"
	"testing"
	"time"
)

func replanningSnapshot(t *testing.T) workflow.Snapshot {
	t.Helper()
	d := reduce(workflow.Snapshot{State: workflow.StateAbsent}, workflow.TriggerEvent{EventMetadata: workflow.EventMetadata{ID: "start", ObservedAt: time.Now(), WorkItem: workflow.WorkItem{RepositoryID: 1, IssueID: 2, IssueNumber: 3}}, AttemptID: "attempt", AttemptNumber: 1})
	if d.Disposition != workflow.DispositionApplied {
		t.Fatal(d)
	}
	s := d.Snapshot
	s.AcceptedPlanRevisionID = "P1"
	s.ActiveTurn = &workflow.ActiveTurn{ID: "developer", SessionID: "dev-session", AttemptID: "attempt", Stage: workflow.StageImplementation, Role: workflow.RoleDeveloper, Epoch: 1, ControlRevision: 1}
	return s
}
func TestReplanningDispatchBudgetAndResolutionPreserveAttempt(t *testing.T) {
	s := replanningSnapshot(t)
	for cycle := uint8(0); cycle < 4; cycle++ {
		s.CurrentAttempt.AutonomousReplanningReturns = cycle
		s.CurrentAttempt.ReviewUsage[workflow.StageReview] = 2
		e := settledEvent(s, "request", workflow.TurnOutcomeReplanningRequested)
		e.ReplanningCycleID = "cycle"
		d := reduce(s, e)
		if d.Disposition != workflow.DispositionApplied {
			t.Fatal(d)
		}
		if cycle == 3 {
			if d.Snapshot.State != workflow.StateNeedsHuman || d.Snapshot.CurrentAttempt.AutonomousReplanningReturns != 3 {
				t.Fatal(d)
			}
			for _, a := range d.Actions {
				if _, ok := a.(workflow.EnqueueTurnAction); ok {
					t.Fatal("fourth dispatch allocated", d)
				}
			}
			continue
		}
		if d.Snapshot.State != workflow.StatePlanning || d.Snapshot.PlanningOnly || d.Snapshot.ActiveTurn != nil || d.Snapshot.CurrentAttempt.AutonomousReplanningReturns != cycle+1 {
			t.Fatal(d)
		}
		a := onlyAction[workflow.EnqueueTurnAction](t, d.Actions)
		if a.Role != workflow.RolePlanner || a.Purpose != workflow.TurnPurposeReplanning {
			t.Fatal(a)
		}
		if again := reduce(d.Snapshot, e); again.Disposition == workflow.DispositionApplied {
			t.Fatal("replay dispatched again", again)
		}
		p := d.Snapshot
		p.ActiveTurn = &workflow.ActiveTurn{ID: "planner", SessionID: "plan-session", AttemptID: "attempt", Stage: workflow.StagePlanning, Role: workflow.RolePlanner, Epoch: 1, ControlRevision: 1}
		done := settledEvent(p, "resolve", workflow.TurnOutcomeReplanningResolved)
		done.ReplanningCycleID = "cycle"
		done.ImplementationAuthorized = true
		done.ResolvedPlanRevisionID = "P1"
		resumed := reduce(p, done)
		if resumed.Disposition != workflow.DispositionApplied || resumed.Snapshot.State != workflow.StateDeveloping || resumed.Snapshot.CurrentAttempt.ReviewUsage[workflow.StageReview] != 2 || resumed.Snapshot.CurrentAttempt.AutonomousReplanningReturns != cycle+1 || resumed.Snapshot.CurrentAttempt.ID != "attempt" {
			t.Fatal(resumed)
		}
		if onlyAction[workflow.EnqueueTurnAction](t, resumed.Actions).Purpose != workflow.TurnPurposeResumedDevelopment {
			t.Fatal(resumed)
		}
	}
}
func TestReplanningDoesNotGrantDirectDevelopmentPlanning(t *testing.T) {
	s := replanningSnapshot(t)
	s.AcceptedPlanRevisionID = ""
	e := settledEvent(s, "request", workflow.TurnOutcomeReplanningRequested)
	e.ReplanningCycleID = "cycle"
	if d := reduce(s, e); d.Disposition == workflow.DispositionApplied {
		t.Fatal(d)
	}
	s.AcceptedPlanRevisionID = "P1"
	s.CurrentAttempt.AutonomousReplanningReturns = 2
	e = settledEvent(s, "human", workflow.TurnOutcomeReplanningRequested)
	e.ReplanningCycleID = "human-cycle"
	e.HumanOriginReplanning = true
	d := reduce(s, e)
	if d.Disposition != workflow.DispositionApplied || d.Snapshot.CurrentAttempt.AutonomousReplanningReturns != 2 {
		t.Fatal(d)
	}
}

func TestReplanningSameHeadOldPlanReviewCannotCertifyRevisedCommitments(t *testing.T) {
	s := reviewingSnapshot(2, "same-head")
	s.AcceptedPlanRevisionID = "revised"
	old := &workflow.ReviewIdentity{ID: 90, NodeID: "old", ChangeProposalID: s.ChangeProposal.ID, ActorID: 7001, HeadSHA: "same-head", AcceptedPlanRevisionID: "original"}
	event := reviewEvent(s, "old-review", workflow.TurnOutcomeApproved, old, s.ChangeProposal)
	d := reduce(s, event)
	if d.Disposition != workflow.DispositionApplied || d.Snapshot.State == workflow.StatePRReady || d.Snapshot.ChangeProposal.ReadyForSHA != "" || d.Snapshot.CurrentAttempt.ReviewUsage[workflow.StageReview] != 2 {
		t.Fatal("old-plan same-head review became authority", d)
	}
	fresh := *old
	fresh.ID = 91
	fresh.NodeID = "fresh"
	fresh.AcceptedPlanRevisionID = "revised"
	d = reduce(s, reviewEvent(s, "fresh-review", workflow.TurnOutcomeApproved, &fresh, s.ChangeProposal))
	if d.Disposition != workflow.DispositionApplied || d.Snapshot.State != workflow.StatePRReady || d.Snapshot.ChangeProposal.ReadyForPlanRevisionID != "revised" || d.Snapshot.CurrentAttempt.ReviewUsage[workflow.StageReview] != 3 {
		t.Fatal("fresh review did not use remaining budget", d)
	}
}

func TestReplanningHeadMovementReassessesSamePlannerAndDrainsDeferredSync(t *testing.T) {
	s := replanningSnapshot(t)
	s.State = workflow.StatePlanning
	s.ReplanningCycleID = "cycle"
	s.CurrentAttempt.CurrentStage = workflow.StagePlanning
	s.CurrentAttempt.AutonomousReplanningReturns = 1
	s.CurrentAttempt.ReviewUsage[workflow.StageReview] = 2
	s.ActiveTurn = nil
	s.ChangeProposal = proposal(64, "old")
	s.ClarificationWaiting = true
	e := workflow.SynchronizationEvent{EventMetadata: workflow.EventMetadata{ID: "sync", ObservedAt: time.Now(), WorkItem: s.WorkItem, ExpectedRevision: s.Revision}, ChangeProposalID: 64, PreviousHeadSHA: "old", HeadSHA: "new"}
	d := reduce(s, e)
	if d.Disposition != workflow.DispositionApplied || d.Snapshot.State != workflow.StatePlanning || d.Snapshot.CurrentAttempt.AutonomousReplanningReturns != 1 || d.Snapshot.CurrentAttempt.ReviewUsage[workflow.StageReview] != 2 {
		t.Fatal(d)
	}
	intent := onlyAction[workflow.EnqueueTurnAction](t, d.Actions)
	if intent.Role != workflow.RolePlanner || intent.Purpose != workflow.TurnPurposeReactivation || intent.ExpectedHeadSHA != "new" {
		t.Fatal(intent)
	}
	s.ActiveTurn = &workflow.ActiveTurn{ID: "planner", SessionID: "session", AttemptID: s.CurrentAttempt.ID, Stage: workflow.StagePlanning, Role: workflow.RolePlanner, Epoch: 1, ControlRevision: 1, ChangeProposalID: 64, ExpectedHeadSHA: "old"}
	waiting := settledEvent(s, "yield-head-movement", workflow.TurnOutcomeClarificationWait)
	waiting.PendingEvents = workflow.PendingEventsObservation{Count: 1, RequiresReconciliation: true, LatestObservedHeadSHA: "new"}
	d = reduce(s, waiting)
	if d.Disposition != workflow.DispositionApplied {
		t.Fatal(d)
	}
	reconcile := onlyAction[workflow.ReconcilePendingEventsAction](t, d.Actions)
	if reconcile.FallbackRole != workflow.RolePlanner || reconcile.FallbackPurpose != workflow.TurnPurposeReactivation {
		t.Fatal(reconcile)
	}
}
