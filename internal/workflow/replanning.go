package workflow

// Dispatch is reached only after the source Turn's mutation, stop and capture
// barriers. The request receipt alone cannot transfer Session control.
func (r Reducer) reduceReplanningRequested(snapshot, next Snapshot, event TurnSettledEvent) Decision {
	if snapshot.PlanningOnly || snapshot.AcceptedPlanRevisionID == "" || snapshot.ReplanningCycleID != "" || event.Turn.Stage != StageImplementation {
		return illegalInState(snapshot)
	}
	next.ReplanningCycleID = event.ReplanningCycleID
	if event.ChangeProposal != nil {
		if next.ChangeProposal != nil && next.ChangeProposal.ID != event.ChangeProposal.ID {
			return illegalInState(snapshot)
		}
		proposal := *event.ChangeProposal
		proposal.ReadyForSHA = ""
		next.ChangeProposal = &proposal
	}
	if !event.HumanOriginReplanning && next.CurrentAttempt.AutonomousReplanningReturns >= 3 {
		next.State = StateNeedsHuman
		next.CurrentAttempt.CurrentStage = StagePlanning
		next.ResumeRole = RolePlanner
		next.Assignments.Status = AssignmentWaitingForHuman
		next.Revision++
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: Reason("replanning_budget_exhausted"), Actions: []Action{MarkHumanHandoffAction{Reason: Reason("replanning_budget_exhausted"), Diagnostic: "Three autonomous replanning returns have already been dispatched in this Workflow Attempt. The new concern requires human recovery. " + event.Diagnostic}, ReconcileLabelsAction{State: StateNeedsHuman}}}
	}
	if !event.HumanOriginReplanning {
		next.CurrentAttempt.AutonomousReplanningReturns++
	}
	next.State = StatePlanning
	next.CurrentAttempt.CurrentStage = StagePlanning
	next.ClarificationWaiting = false
	next.Revision++
	head := ""
	if next.ChangeProposal != nil {
		head = next.ChangeProposal.HeadSHA
	}
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: Reason("replanning_requested"), Actions: successorActions(event.Turn, event.PendingEvents, EnqueueTurnAction{Stage: StagePlanning, Role: RolePlanner, Purpose: TurnPurposeReplanning, ExpectedHeadSHA: head})}
}
func (r Reducer) reduceReplanningResolved(snapshot, next Snapshot, event TurnSettledEvent) Decision {
	if snapshot.PlanningOnly || snapshot.ReplanningCycleID == "" || event.ReplanningCycleID != snapshot.ReplanningCycleID || event.Turn.Stage != StagePlanning {
		return illegalInState(snapshot)
	}
	if event.HumanInputPending {
		return clarificationWaitDecision(next, event.Turn, true)
	}
	next.State = StateDeveloping
	next.CurrentAttempt.CurrentStage = StageImplementation
	next.ClarificationWaiting = false
	next.Revision++
	// The accepted binding is installed by resumed admission, not publication or
	// settlement. Same-head readiness is obsolete for materially revised pairs.
	if next.ChangeProposal != nil && event.ResolvedPlanRevisionID != snapshot.AcceptedPlanRevisionID {
		next.ChangeProposal.ReadyForSHA = ""
	}
	head := ""
	if next.ChangeProposal != nil {
		head = next.ChangeProposal.HeadSHA
	}
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: Reason("replanning_resolved"), Actions: successorActions(event.Turn, event.PendingEvents, EnqueueTurnAction{Stage: StageImplementation, Role: RoleDeveloper, Purpose: TurnPurposeResumedDevelopment, ExpectedHeadSHA: head})}
}
