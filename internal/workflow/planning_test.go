package workflow_test

import (
	"github.com/jozala/omnigrex/internal/workflow"
	"testing"
	"time"
)

func TestPlanningOnlyEntryWaitingCompletionAndClosure(t *testing.T) {
	m := workflow.EventMetadata{ID: "plan", ObservedAt: time.Now(), WorkItem: workflow.WorkItem{RepositoryID: 1, IssueID: 2, IssueNumber: 3}}
	start := reduce(workflow.Snapshot{State: workflow.StateAbsent}, workflow.TriggerEvent{EventMetadata: m, PlanningOnly: true, AttemptID: "planning-attempt", AttemptNumber: 1})
	if start.Disposition != workflow.DispositionApplied || start.Snapshot.State != workflow.StatePlanning || !start.Snapshot.PlanningOnly {
		t.Fatal(start)
	}
	action := onlyAction[workflow.EnqueueTurnAction](t, start.Actions)
	if action.Role != workflow.RolePlanner || action.Stage != workflow.StagePlanning || action.Purpose != workflow.TurnPurposeInitialPlanning {
		t.Fatal(action)
	}
	s := start.Snapshot
	s.ActiveTurn = &workflow.ActiveTurn{ID: "turn", SessionID: "session", AttemptID: s.CurrentAttempt.ID, Stage: workflow.StagePlanning, Role: workflow.RolePlanner, Epoch: 1, ControlRevision: 1}
	wait := reduce(s, settledEvent(s, "wait", workflow.TurnOutcomeClarificationWait))
	if wait.Disposition != workflow.DispositionApplied || !wait.Snapshot.ClarificationWaiting || wait.Snapshot.State != workflow.StatePlanning || len(wait.Actions) != 0 {
		t.Fatal(wait)
	}
	ready := reduce(s, settledEvent(s, "ready", workflow.TurnOutcomePlanningReady))
	if ready.Disposition != workflow.DispositionApplied || ready.Snapshot.State != workflow.StatePlanReady || ready.Snapshot.ChangeProposal != nil || ready.Snapshot.ActiveTurn != nil {
		t.Fatal(ready)
	}
	for _, a := range ready.Actions {
		switch a.(type) {
		case workflow.EnqueueTurnAction, workflow.EnsureAssignmentsAction:
			t.Fatal("allocated implementation", a)
		}
	}
	for _, state := range []workflow.Snapshot{wait.Snapshot, ready.Snapshot} {
		if d := reduce(state, workflow.TriggerEvent{EventMetadata: metadata(state, "run"), AttemptID: "development", AttemptNumber: 2}); d.Disposition == workflow.DispositionApplied {
			t.Fatal("run converted planning", d)
		}
	}
	pending := settledEvent(s, "steering", workflow.TurnOutcomePlanningReady)
	pending.HumanInputPending = true
	fenced := reduce(s, pending)
	if fenced.Snapshot.State != workflow.StatePlanning || onlyAction[workflow.EnqueueTurnAction](t, fenced.Actions).Role != workflow.RolePlanner {
		t.Fatal(fenced)
	}
	closed := reduce(ready.Snapshot, workflow.IssueClosedEvent{EventMetadata: metadata(ready.Snapshot, "close"), ClosureID: "close", RetentionToken: "retention", RetainUntil: time.Now().Add(time.Hour)})
	if closed.Disposition != workflow.DispositionApplied || closed.Snapshot.State != workflow.StateClosing {
		t.Fatal(closed)
	}
}
