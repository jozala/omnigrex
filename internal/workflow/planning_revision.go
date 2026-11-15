package workflow

// ReopenPlanning is used only by the locked collaboration boundary before the
// first Developer admission. Operational projection recovery never calls it.
func (r Reducer) ReopenPlanning(snapshot Snapshot) Decision {
	if snapshot.ActiveTurn != nil || snapshot.CurrentAttempt == nil || snapshot.ChangeProposal != nil && snapshot.ReplanningCycleID == "" || (snapshot.State != StatePlanReady && snapshot.State != StateDeveloping && snapshot.State != StatePlanning) {
		return illegalInState(snapshot)
	}
	next := snapshot.Clone()
	next.PlanningOnly = snapshot.ReplanningCycleID == ""
	next.State = StatePlanning
	next.CurrentAttempt.CurrentStage = StagePlanning
	next.ContinuationStage = StagePlanning
	next.ResumeRole = ""
	next.ClarificationWaiting = false
	next.Revision++
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: Reason("planning_revision_requested")}
}

// WithdrawQueuedImplementation cancels only preparation; no admitted Turn exists.
func (r Reducer) WithdrawQueuedImplementation(snapshot Snapshot) Decision {
	if snapshot.State != StateDeveloping || snapshot.ActiveTurn != nil || snapshot.ChangeProposal != nil || snapshot.CurrentAttempt == nil {
		return illegalInState(snapshot)
	}
	next := snapshot.Clone()
	next.PlanningOnly = true
	next.State = StatePlanReady
	next.CurrentAttempt.CurrentStage = StagePlanning
	next.ContinuationStage = StagePlanning
	next.Revision++
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: Reason("implementation_withdrawn")}
}
