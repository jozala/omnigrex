package workflow_test

import (
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/workflow"
)

func TestStateEventLegalityIsExhaustive(t *testing.T) {
	states := []workflow.State{
		workflow.StateAbsent, workflow.StateDormant, workflow.StateDeveloping, workflow.StateReviewing,
		workflow.StatePRReady, workflow.StateNeedsHuman, workflow.StateClosing, workflow.StateClosed,
	}
	tests := []struct {
		name string
		make func(workflow.Snapshot) workflow.Event
		want map[workflow.State]workflow.Disposition
	}{
		{name: "trigger", make: matrixTrigger, want: dispositions(
			workflow.DispositionApplied, workflow.DispositionApplied, workflow.DispositionDeferred, workflow.DispositionDeferred,
			workflow.DispositionApplied, workflow.DispositionApplied, workflow.DispositionIllegal, workflow.DispositionIllegal,
		)},
		{name: "turn settled", make: matrixTurnSettled, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionApplied, workflow.DispositionApplied,
			workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal,
		)},
		{name: "synchronization", make: matrixSynchronization, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionDeferred, workflow.DispositionDeferred,
			workflow.DispositionApplied, workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal,
		)},
		{name: "review observed", make: matrixReviewObserved, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionUnrelated, workflow.DispositionDeferred, workflow.DispositionDeferred,
			workflow.DispositionUnrelated, workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionUnrelated,
		)},
		{name: "Change Proposal observed", make: matrixChangeProposalObserved, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionUnrelated, workflow.DispositionDeferred, workflow.DispositionDeferred,
			workflow.DispositionUnrelated, workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionUnrelated,
		)},
		{name: "Issue closed", make: matrixIssueClosed, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionApplied, workflow.DispositionApplied, workflow.DispositionApplied,
			workflow.DispositionApplied, workflow.DispositionApplied, workflow.DispositionIllegal, workflow.DispositionIllegal,
		)},
		{name: "closure settled", make: matrixClosureSettled, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal,
			workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionApplied, workflow.DispositionIllegal,
		)},
		{name: "Issue reopened", make: matrixIssueReopened, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionDeferred, workflow.DispositionDeferred,
			workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionApplied, workflow.DispositionApplied,
		)},
		{name: "Assignments collected", make: matrixAssignmentsCollected, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionStale, workflow.DispositionIllegal, workflow.DispositionIllegal,
			workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionApplied,
		)},
		{name: "Assignment configuration conflict", make: matrixAssignmentConfigurationConflict, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionStale, workflow.DispositionStale,
			workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal,
		)},
		{name: "Agent Turn preparation failed", make: matrixAgentTurnPreparationFailed, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionStale, workflow.DispositionStale,
			workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal,
		)},
		{name: "Agent Turn mutation reconciliation exhausted", make: matrixAgentTurnMutationReconciliationExhausted, want: dispositions(
			workflow.DispositionUnrelated, workflow.DispositionIllegal, workflow.DispositionStale, workflow.DispositionStale,
			workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal, workflow.DispositionIllegal,
		)},
	}

	for _, test := range tests {
		for _, state := range states {
			t.Run(test.name+"/"+string(state), func(t *testing.T) {
				snapshot := snapshotForState(state)
				decision := workflow.Reduce(snapshot, test.make(snapshot))
				if decision.Disposition != test.want[state] {
					t.Errorf("disposition = %q (%q), want %q", decision.Disposition, decision.Reason, test.want[state])
				}
				if decision.Reason == "" {
					t.Error("reason is empty")
				}
				if decision.Disposition != workflow.DispositionApplied && decision.Snapshot.Revision != snapshot.Revision {
					t.Errorf("revision = %d, want unchanged %d", decision.Snapshot.Revision, snapshot.Revision)
				}
			})
		}
	}
}

func TestTurnSettlementGuardsSessionEpochControlAndChangeProposal(t *testing.T) {
	mutations := map[string]func(*workflow.TurnGuard){
		"turn ID":          func(g *workflow.TurnGuard) { g.TurnID = "other-turn" },
		"session ID":       func(g *workflow.TurnGuard) { g.SessionID = "other-session" },
		"attempt ID":       func(g *workflow.TurnGuard) { g.AttemptID = "other-attempt" },
		"Role":             func(g *workflow.TurnGuard) { g.Role = workflow.RoleDeveloper },
		"execution epoch":  func(g *workflow.TurnGuard) { g.Epoch++ },
		"control revision": func(g *workflow.TurnGuard) { g.ControlRevision++ },
		"Pull Request ID":  func(g *workflow.TurnGuard) { g.ChangeProposalID++ },
		"expected head":    func(g *workflow.TurnGuard) { g.ExpectedHeadSHA = "other-head" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			snapshot := reviewingSnapshot(0, "head-1")
			event := settledEvent(snapshot, "guard-"+name, workflow.TurnOutcomeBlocked)
			mutate(&event.Turn)
			decision := workflow.Reduce(snapshot, event)
			assertDecision(t, decision, workflow.DispositionStale, workflow.ReasonTurnGuardStale, snapshot.State, snapshot.Revision)
		})
	}
}

func TestDuplicateOutOfOrderUnrelatedAndIllegalEvents(t *testing.T) {
	t.Run("duplicate delivery observation", func(t *testing.T) {
		snapshot := developingSnapshot(nil)
		event := settledEvent(snapshot, "duplicate", workflow.TurnOutcomeBlocked)
		event.Duplicate = true
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionDuplicate, workflow.ReasonEventDuplicate, snapshot.State, snapshot.Revision)
	})

	t.Run("out of order revision", func(t *testing.T) {
		snapshot := developingSnapshot(nil)
		event := settledEvent(snapshot, "future", workflow.TurnOutcomeBlocked)
		event.ExpectedRevision++
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionStale, workflow.ReasonEventOutOfOrder, snapshot.State, snapshot.Revision)
	})

	t.Run("unrelated Work Item", func(t *testing.T) {
		snapshot := developingSnapshot(nil)
		event := settledEvent(snapshot, "unrelated", workflow.TurnOutcomeBlocked)
		event.WorkItem.IssueID++
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionUnrelated, workflow.ReasonWorkItemUnrelated, snapshot.State, snapshot.Revision)
	})

	t.Run("event illegal in state", func(t *testing.T) {
		snapshot := closedSnapshot("retention-live")
		event := matrixTrigger(snapshot)
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionIllegal, workflow.ReasonEventIllegalInState, snapshot.State, snapshot.Revision)
	})

	t.Run("repeated trigger while successor is queued", func(t *testing.T) {
		snapshot := developingSnapshot(nil)
		snapshot.ActiveTurn = nil
		event := matrixTrigger(snapshot)
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionDuplicate, workflow.ReasonAttemptAlreadyActive, snapshot.State, snapshot.Revision)
	})
}

func TestAggregateInvariantsRejectImpossibleRelationalState(t *testing.T) {
	invalid := map[string]workflow.Snapshot{}

	badAttempt := developingSnapshot(nil)
	badAttempt.CurrentAttempt.Lifecycle = ""
	invalid["inactive current Workflow Attempt"] = badAttempt

	badTurnProposal := reviewingSnapshot(0, "head-1")
	badTurnProposal.ActiveTurn.ChangeProposalID++
	invalid["active Reviewer turn for other Pull Request"] = badTurnProposal

	badReady := baseSnapshot(workflow.StatePRReady, 0)
	badReady.ChangeProposal = proposal(64, "head-1")
	invalid["PR_READY without ready_for_sha"] = badReady

	badRetention := dormantSnapshot(workflow.RuntimeStateRetained)
	badRetention.Assignments.RetentionToken = "uncancelled"
	invalid["DORMANT with live retention generation"] = badRetention

	badClosing := snapshotForState(workflow.StateClosing)
	badClosing.ActiveTurn = nil
	badClosing.CurrentAttempt = nil
	invalid["CLOSING with active Assignments but no current attempt"] = badClosing

	for name, snapshot := range invalid {
		t.Run(name, func(t *testing.T) {
			decision := workflow.Reduce(snapshot, matrixIssueClosed(snapshot))
			assertDecision(t, decision, workflow.DispositionIllegal, workflow.ReasonInvariantViolation, snapshot.State, snapshot.Revision)
		})
	}
}

func TestTerminalSettlementWithPendingReconciliationNeverEnqueues(t *testing.T) {
	for _, outcome := range []workflow.TurnOutcome{workflow.TurnOutcomeBlocked, workflow.TurnOutcomeInfrastructureFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			snapshot := developingSnapshot(nil)
			event := settledEvent(snapshot, "terminal-pending-"+string(outcome), outcome)
			event.PendingEvents = workflow.PendingEventsObservation{Count: 1, RequiresReconciliation: true, LatestObservedHeadSHA: "pending-head"}
			decision := workflow.Reduce(snapshot, event)
			if decision.Disposition != workflow.DispositionApplied {
				t.Fatalf("disposition = %q (%q), want APPLIED", decision.Disposition, decision.Reason)
			}
			if actionCount[workflow.ReconcilePendingEventsAction](decision.Actions) != 1 || actionCount[workflow.EnqueueTurnAction](decision.Actions) != 0 {
				t.Errorf("actions = %#v, want one reconciliation and no enqueue", decision.Actions)
			}
		})
	}
}

func TestEveryTurnSettlementEmitsAtMostOneSuccessorIntent(t *testing.T) {
	tests := []workflow.TurnSettledEvent{}
	developer := developingSnapshot(nil)
	developerReady := settledEvent(developer, "one-successor-developer", workflow.TurnOutcomeChangeProposalReady)
	developerReady.ChangeProposal = proposal(64, "head-1")
	tests = append(tests, developerReady)

	reviewer := reviewingSnapshot(0, "head-1")
	tests = append(tests,
		reviewEvent(reviewer, "one-successor-changes", workflow.TurnOutcomeChangesRequested, review(900, 64, "head-1"), proposal(64, "head-1")),
		reviewEvent(reviewer, "one-successor-approval", workflow.TurnOutcomeApproved, review(901, 64, "head-1"), proposal(64, "head-1")),
		settledEvent(reviewer, "one-successor-retry", workflow.TurnOutcomeInfrastructureFailed),
		settledEvent(reviewer, "one-successor-blocked", workflow.TurnOutcomeBlocked),
	)

	for _, event := range tests {
		snapshot := developer
		if event.Turn.Role == workflow.RoleReviewer {
			snapshot = reviewer
		}
		decision := workflow.Reduce(snapshot, event)
		if got := actionCount[workflow.EnqueueTurnAction](decision.Actions); got > 1 {
			t.Errorf("%s enqueue count = %d, want at most one", event.Outcome, got)
		}
	}
}

func dispositions(absent, dormant, developing, reviewing, prReady, needsHuman, closing, closed workflow.Disposition) map[workflow.State]workflow.Disposition {
	return map[workflow.State]workflow.Disposition{
		workflow.StateAbsent: absent, workflow.StateDormant: dormant, workflow.StateDeveloping: developing, workflow.StateReviewing: reviewing,
		workflow.StatePRReady: prReady, workflow.StateNeedsHuman: needsHuman, workflow.StateClosing: closing, workflow.StateClosed: closed,
	}
}

func snapshotForState(state workflow.State) workflow.Snapshot {
	switch state {
	case workflow.StateAbsent:
		return workflow.Snapshot{State: workflow.StateAbsent}
	case workflow.StateDormant:
		return dormantSnapshot(workflow.RuntimeStateRetained)
	case workflow.StateDeveloping:
		return developingSnapshot(nil)
	case workflow.StateReviewing:
		return reviewingSnapshot(0, "head-1")
	case workflow.StatePRReady:
		snapshot := baseSnapshot(state, 0)
		snapshot.ChangeProposal = proposal(64, "head-1")
		snapshot.ChangeProposal.ReadyForSHA = "head-1"
		return snapshot
	case workflow.StateNeedsHuman:
		snapshot := baseSnapshot(state, 1)
		snapshot.Assignments.Status = workflow.AssignmentWaitingForHuman
		snapshot.ResumeRole = workflow.RoleDeveloper
		return snapshot
	case workflow.StateClosing:
		snapshot := developingSnapshot(nil)
		decision := workflow.Reduce(snapshot, closeEvent(snapshot, "matrix-closure", "matrix-retention"))
		return decision.Snapshot
	case workflow.StateClosed:
		return closedSnapshot("matrix-retention")
	default:
		panic("unknown Workflow state")
	}
}

func matrixMetadata(snapshot workflow.Snapshot, id string) workflow.EventMetadata {
	workItem := snapshot.WorkItem
	if snapshot.State == workflow.StateAbsent {
		workItem = workflow.WorkItem{RepositoryID: 91, IssueID: 45, IssueNumber: 12}
	}
	return workflow.EventMetadata{ID: id, ObservedAt: observedAt, WorkItem: workItem, ExpectedRevision: snapshot.Revision}
}

func matrixTrigger(snapshot workflow.Snapshot) workflow.Event {
	return workflow.TriggerEvent{
		EventMetadata: matrixMetadata(snapshot, "matrix-trigger"), AttemptID: "matrix-attempt", AttemptNumber: snapshot.LastAttemptNumber + 1,
	}
}

func matrixTurnSettled(snapshot workflow.Snapshot) workflow.Event {
	turn := workflow.TurnGuard{TurnID: "matrix-turn", SessionID: "matrix-session", AttemptID: "matrix-attempt", Role: workflow.RoleDeveloper, Epoch: 1, ControlRevision: 1}
	if snapshot.ActiveTurn != nil {
		turn = guard(snapshot)
	}
	return workflow.TurnSettledEvent{EventMetadata: matrixMetadata(snapshot, "matrix-settled"), Turn: turn, Outcome: workflow.TurnOutcomeBlocked}
}

func matrixSynchronization(snapshot workflow.Snapshot) workflow.Event {
	previous := "head-1"
	if snapshot.ChangeProposal != nil {
		previous = snapshot.ChangeProposal.HeadSHA
	}
	return workflow.SynchronizationEvent{EventMetadata: matrixMetadata(snapshot, "matrix-sync"), ChangeProposalID: 64, PreviousHeadSHA: previous, HeadSHA: "matrix-new-head"}
}

func matrixReviewObserved(snapshot workflow.Snapshot) workflow.Event {
	return workflow.ReviewObservedEvent{
		EventMetadata: matrixMetadata(snapshot, "matrix-review-observed"),
		Review:        *review(999, 64, "head-1"),
	}
}

func matrixChangeProposalObserved(snapshot workflow.Snapshot) workflow.Event {
	return workflow.ChangeProposalObservedEvent{
		EventMetadata:  matrixMetadata(snapshot, "matrix-change-proposal-observed"),
		ChangeProposal: *proposal(64, "head-1"),
	}
}

func matrixIssueClosed(snapshot workflow.Snapshot) workflow.Event {
	return workflow.IssueClosedEvent{
		EventMetadata: matrixMetadata(snapshot, "matrix-close"), ClosureID: "matrix-close", RetentionToken: "matrix-token", RetainUntil: observedAt.Add(24 * time.Hour),
	}
}

func matrixClosureSettled(snapshot workflow.Snapshot) workflow.Event {
	closureID := "matrix-closure"
	if snapshot.Closure != nil {
		closureID = snapshot.Closure.ID
	}
	return workflow.ClosureSettledEvent{EventMetadata: matrixMetadata(snapshot, "matrix-closure-settled"), ClosureID: closureID, Turn: turnGuardPointer(snapshot), AssignmentsExist: true}
}

func matrixIssueReopened(snapshot workflow.Snapshot) workflow.Event {
	return workflow.IssueReopenedEvent{EventMetadata: matrixMetadata(snapshot, "matrix-reopen")}
}

func matrixAssignmentsCollected(snapshot workflow.Snapshot) workflow.Event {
	token := "matrix-token"
	deadline := observedAt.Add(24 * time.Hour)
	if snapshot.State == workflow.StateClosed {
		token = snapshot.Assignments.RetentionToken
		deadline = snapshot.Assignments.RetainedUntil
	}
	return workflow.AssignmentsCollectedEvent{
		EventMetadata: matrixMetadata(snapshot, "matrix-collect"), RetentionToken: token, RetainUntil: deadline, CollectedAt: deadline.Add(time.Minute),
	}
}

func matrixAssignmentConfigurationConflict(snapshot workflow.Snapshot) workflow.Event {
	role := workflow.RoleDeveloper
	if snapshot.State == workflow.StateReviewing {
		role = workflow.RoleReviewer
	}
	return workflow.AssignmentConfigurationConflictEvent{
		EventMetadata: matrixMetadata(snapshot, "matrix-assignment-configuration-conflict"),
		Role:          role,
	}
}

func matrixAgentTurnPreparationFailed(snapshot workflow.Snapshot) workflow.Event {
	role := workflow.RoleDeveloper
	if snapshot.State == workflow.StateReviewing {
		role = workflow.RoleReviewer
	}
	return workflow.AgentTurnPreparationFailedEvent{
		EventMetadata: matrixMetadata(snapshot, "matrix-agent-turn-preparation-failed"),
		Role:          role,
		Diagnostic:    "preparation failed",
	}
}

func matrixAgentTurnMutationReconciliationExhausted(snapshot workflow.Snapshot) workflow.Event {
	role := workflow.RoleDeveloper
	if snapshot.State == workflow.StateReviewing {
		role = workflow.RoleReviewer
	}
	return workflow.AgentTurnMutationReconciliationExhaustedEvent{
		EventMetadata: matrixMetadata(snapshot, "matrix-agent-turn-mutation-reconciliation-exhausted"),
		Role:          role,
		Diagnostic:    "outcome unknowable; escalated",
	}
}
