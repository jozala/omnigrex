package workflow

func eventDetails(event Event) (EventMetadata, EventKind, bool) {
	switch event := event.(type) {
	case TriggerEvent:
		return event.EventMetadata, EventKindTrigger, true
	case TurnSettledEvent:
		return event.EventMetadata, EventKindTurnSettled, true
	case SynchronizationEvent:
		return event.EventMetadata, EventKindSynchronization, true
	case ReviewObservedEvent:
		return event.EventMetadata, EventKindReviewObserved, true
	case ChangeProposalObservedEvent:
		return event.EventMetadata, EventKindChangeProposalObserved, true
	case IssueClosedEvent:
		return event.EventMetadata, EventKindIssueClosed, true
	case ClosureSettledEvent:
		return event.EventMetadata, EventKindClosureSettled, true
	case IssueReopenedEvent:
		return event.EventMetadata, EventKindIssueReopened, true
	case AssignmentsCollectedEvent:
		return event.EventMetadata, EventKindAssignmentsCollected, true
	case AssignmentConfigurationConflictEvent:
		return event.EventMetadata, EventKindAssignmentConfigurationConflict, true
	case AgentTurnPreparationFailedEvent:
		return event.EventMetadata, EventKindAgentTurnPreparationFailed, true
	case AgentTurnMutationReconciliationExhaustedEvent:
		return event.EventMetadata, EventKindAgentTurnMutationReconciliationExhausted, true
	case WorkflowActionExhaustedEvent:
		return event.EventMetadata, EventKindWorkflowActionExhausted, true
	default:
		return EventMetadata{}, "", false
	}
}

// EventMetadataOf returns the metadata carried by a concrete Workflow Event.
func EventMetadataOf(event Event) (EventMetadata, bool) {
	metadata, _, ok := eventDetails(event)
	return metadata, ok
}

func validEvent(definition Definition, event Event, metadata EventMetadata) bool {
	if metadata.ID == "" || metadata.ObservedAt.IsZero() || !validWorkItem(metadata.WorkItem) {
		return false
	}
	switch event := event.(type) {
	case TriggerEvent:
		return event.AttemptID != "" && event.AttemptNumber > 0
	case TurnSettledEvent:
		if !validTurnGuard(definition, event.Turn) || !validPendingObservation(event.PendingEvents) {
			return false
		}
		switch event.Outcome {
		case TurnOutcomeChangeProposalReady:
			return validChangeProposal(event.ChangeProposal) && event.Review == nil && event.ExistingReview == nil
		case TurnOutcomeChangesRequested, TurnOutcomeApproved:
			return event.AuthorizedReviewerActorID > 0 && validChangeProposal(event.ChangeProposal) && validReview(event.Review) && (event.ExistingReview == nil || validReview(event.ExistingReview))
		case TurnOutcomeBlocked, TurnOutcomeInfrastructureFailed:
			return event.ChangeProposal == nil && event.Review == nil && event.ExistingReview == nil
		default:
			return false
		}
	case SynchronizationEvent:
		return event.ChangeProposalID > 0 && event.PreviousHeadSHA != "" && event.HeadSHA != ""
	case ReviewObservedEvent:
		return validReview(&event.Review)
	case ChangeProposalObservedEvent:
		return validChangeProposal(&event.ChangeProposal)
	case IssueClosedEvent:
		return event.ClosureID != "" && event.RetentionToken != "" && event.RetainUntil.After(metadata.ObservedAt)
	case ClosureSettledEvent:
		return event.ClosureID != "" && (event.Turn == nil || validTurnGuard(definition, *event.Turn))
	case IssueReopenedEvent:
		return true
	case AssignmentsCollectedEvent:
		return event.RetentionToken != "" && !event.RetainUntil.IsZero() && !event.CollectedAt.IsZero()
	case AssignmentConfigurationConflictEvent:
		return definition.ContainsRole(event.Role)
	case AgentTurnPreparationFailedEvent:
		return definition.ContainsRole(event.Role) && event.Diagnostic != ""
	case AgentTurnMutationReconciliationExhaustedEvent:
		return definition.ContainsRole(event.Role) && event.Diagnostic != ""
	case WorkflowActionExhaustedEvent:
		return (event.ResumeRole == "" || definition.ContainsRole(event.ResumeRole)) && event.Diagnostic != ""
	default:
		return false
	}
}

func validSnapshot(definition Definition, infrastructureRetryLimit uint8, snapshot Snapshot) bool {
	if !validState(snapshot.State) {
		return false
	}
	if snapshot.State == StateAbsent {
		return snapshot == (Snapshot{State: StateAbsent})
	}
	if !validWorkItem(snapshot.WorkItem) || !validAssignments(snapshot) || !validAttempt(definition, infrastructureRetryLimit, snapshot) || !validStateShape(definition, snapshot) {
		return false
	}
	if snapshot.ResumeRole != "" && !definition.ContainsRole(snapshot.ResumeRole) {
		return false
	}
	if snapshot.ChangeProposal != nil && snapshot.State != StatePRReady && snapshot.ChangeProposal.ReadyForSHA != "" {
		return false
	}
	return true
}

func validAssignments(snapshot Snapshot) bool {
	assignments := snapshot.Assignments
	switch snapshot.State {
	case StateDeveloping, StateReviewing, StatePRReady:
		return assignments.Status == AssignmentActive && assignments.RuntimeState == RuntimeStateActive && assignments.RetainedUntil.IsZero() && assignments.RetentionToken == ""
	case StateNeedsHuman:
		return assignments.Status == AssignmentWaitingForHuman &&
			(assignments.RuntimeState == RuntimeStateActive || assignments.RuntimeState == RuntimeStateCollected) &&
			assignments.RetainedUntil.IsZero() && assignments.RetentionToken == ""
	case StateClosing:
		if assignments.Status == AssignmentActive || assignments.Status == AssignmentWaitingForHuman {
			return snapshot.CurrentAttempt != nil &&
				(assignments.RuntimeState == RuntimeStateActive ||
					assignments.Status == AssignmentWaitingForHuman && assignments.RuntimeState == RuntimeStateCollected) &&
				assignments.RetainedUntil.IsZero() && assignments.RetentionToken == ""
		}
		return assignments.Status == AssignmentCompleted && snapshot.CurrentAttempt == nil && (assignments.RuntimeState == RuntimeStateRetained || assignments.RuntimeState == RuntimeStateCollected) && assignments.RetainedUntil.IsZero() && assignments.RetentionToken == ""
	case StateClosed:
		return assignments.Status == AssignmentCompleted && ((assignments.RuntimeState == RuntimeStateRetained && !assignments.RetainedUntil.IsZero() && assignments.RetentionToken != "") || (assignments.RuntimeState == RuntimeStateCollected && assignments.RetainedUntil.IsZero() && assignments.RetentionToken == ""))
	case StateDormant:
		return assignments.Status == AssignmentCompleted && (assignments.RuntimeState == RuntimeStateRetained || assignments.RuntimeState == RuntimeStateCollected) && assignments.RetainedUntil.IsZero() && assignments.RetentionToken == ""
	default:
		return false
	}
}

func validAttempt(definition Definition, infrastructureRetryLimit uint8, snapshot Snapshot) bool {
	required := snapshot.State == StateDeveloping || snapshot.State == StateReviewing || snapshot.State == StatePRReady || snapshot.State == StateNeedsHuman
	if required && snapshot.CurrentAttempt == nil {
		return false
	}
	if !required && snapshot.State != StateClosing && snapshot.CurrentAttempt != nil {
		return false
	}
	if snapshot.CurrentAttempt == nil {
		return snapshot.LastAttemptNumber > 0
	}
	attempt := snapshot.CurrentAttempt
	stage, stageExists := definition.Stage(attempt.CurrentStage)
	if attempt.ID == "" || attempt.Number == 0 || attempt.StartedAt.IsZero() || attempt.Lifecycle != AttemptActive || !stageExists ||
		attempt.InfrastructureRetryBudget.Limit != infrastructureRetryLimit || attempt.InfrastructureRetryBudget.Used > attempt.InfrastructureRetryBudget.Limit {
		return false
	}
	for stageID, used := range attempt.ReviewUsage {
		configured, ok := definition.Stage(stageID)
		if !ok || configured.ReviewLimit == 0 || used > configured.ReviewLimit {
			return false
		}
	}
	return attempt.Number == snapshot.LastAttemptNumber &&
		(snapshot.State != StateReviewing || stage.ReviewLimit == 0 || attempt.ReviewUsage[attempt.CurrentStage] < stage.ReviewLimit)
}

func validStateShape(definition Definition, snapshot Snapshot) bool {
	switch snapshot.State {
	case StateDormant, StateClosed:
		return snapshot.CurrentAttempt == nil && snapshot.ActiveTurn == nil && snapshot.Closure == nil
	case StateDeveloping:
		return snapshot.Closure == nil && validActiveStageState(definition, snapshot) && validOptionalActiveTurn(definition, snapshot)
	case StateReviewing:
		return snapshot.Closure == nil && validChangeProposal(snapshot.ChangeProposal) && validActiveStageState(definition, snapshot) && validOptionalActiveTurn(definition, snapshot)
	case StatePRReady:
		_, stageExists := definition.Stage(snapshot.CurrentAttempt.CurrentStage)
		return stageExists && snapshot.Closure == nil && snapshot.ActiveTurn == nil && validChangeProposal(snapshot.ChangeProposal) && snapshot.ChangeProposal.ReadyForSHA == snapshot.ChangeProposal.HeadSHA
	case StateNeedsHuman:
		_, stageExists := definition.Stage(snapshot.CurrentAttempt.CurrentStage)
		return stageExists && snapshot.Closure == nil && snapshot.ActiveTurn == nil
	case StateClosing:
		return snapshot.Closure != nil && snapshot.Closure.ID != "" && snapshot.Closure.RetentionToken != "" && !snapshot.Closure.RetainUntil.IsZero() && validClosingTurn(snapshot)
	default:
		return false
	}
}

func validActiveStageState(definition Definition, snapshot Snapshot) bool {
	stage, ok := definition.Stage(snapshot.CurrentAttempt.CurrentStage)
	return ok && stage.State == snapshot.State
}

func validOptionalActiveTurn(definition Definition, snapshot Snapshot) bool {
	if snapshot.ActiveTurn == nil {
		return true
	}
	stage, ok := definition.Stage(snapshot.CurrentAttempt.CurrentStage)
	if !ok {
		return false
	}
	turn := snapshot.ActiveTurn
	if turn.ID == "" || turn.SessionID == "" || turn.AttemptID != snapshot.CurrentAttempt.ID || turn.Stage != stage.ID || turn.Role != stage.Role || turn.Epoch == 0 || turn.ControlRevision == 0 {
		return false
	}
	if snapshot.ChangeProposal == nil {
		return turn.ChangeProposalID == 0 && turn.ExpectedHeadSHA == ""
	}
	return turn.ChangeProposalID == snapshot.ChangeProposal.ID && turn.ExpectedHeadSHA == snapshot.ChangeProposal.HeadSHA
}

func validClosingTurn(snapshot Snapshot) bool {
	if snapshot.ActiveTurn == nil {
		return true
	}
	turn := snapshot.ActiveTurn
	return snapshot.CurrentAttempt != nil && turn.ID != "" && turn.SessionID != "" && turn.AttemptID == snapshot.CurrentAttempt.ID && turn.Stage == snapshot.CurrentAttempt.CurrentStage && turn.Epoch > 0 && turn.ControlRevision > 0
}

func legalInState(state State, kind EventKind) bool {
	switch kind {
	case EventKindTrigger:
		return state == StateAbsent || state == StateDormant || state == StatePRReady || state == StateNeedsHuman
	case EventKindTurnSettled:
		return state == StateDeveloping || state == StateReviewing
	case EventKindSynchronization:
		return state == StateDeveloping || state == StateReviewing || state == StatePRReady
	case EventKindReviewObserved:
		return state == StateDeveloping || state == StateReviewing
	case EventKindChangeProposalObserved:
		return state == StateDeveloping || state == StateReviewing
	case EventKindIssueClosed:
		return state == StateDormant || state == StateDeveloping || state == StateReviewing || state == StatePRReady || state == StateNeedsHuman
	case EventKindClosureSettled:
		return state == StateClosing
	case EventKindIssueReopened:
		return state == StateDeveloping || state == StateReviewing || state == StateClosing || state == StateClosed
	case EventKindAssignmentsCollected:
		return state == StateClosed || state == StateDormant
	case EventKindAssignmentConfigurationConflict:
		return state == StateDeveloping || state == StateReviewing
	case EventKindAgentTurnPreparationFailed:
		return state == StateDeveloping || state == StateReviewing
	case EventKindAgentTurnMutationReconciliationExhausted:
		return state == StateDeveloping || state == StateReviewing
	case EventKindWorkflowActionExhausted:
		return state == StateDeveloping || state == StateReviewing || state == StatePRReady || state == StateNeedsHuman
	default:
		return false
	}
}

func validWorkItem(workItem WorkItem) bool {
	return workItem.RepositoryID > 0 && workItem.IssueID > 0 && workItem.IssueNumber > 0
}

func validState(state State) bool {
	switch state {
	case StateAbsent, StateDormant, StateDeveloping, StateReviewing, StatePRReady, StateNeedsHuman, StateClosing, StateClosed:
		return true
	default:
		return false
	}
}

func validTurnGuard(definition Definition, turn TurnGuard) bool {
	_, ok := definition.Stage(turn.Stage)
	return ok && definition.ContainsRole(turn.Role) && turn.TurnID != "" && turn.SessionID != "" && turn.AttemptID != "" && turn.Epoch > 0 && turn.ControlRevision > 0
}

func validChangeProposal(proposal *ChangeProposal) bool {
	return proposal != nil && proposal.ID > 0 && proposal.Number > 0 && proposal.HeadSHA != "" && proposal.Open
}

func validReview(review *ReviewIdentity) bool {
	return review != nil && review.ID > 0 && review.NodeID != "" && review.ChangeProposalID > 0 && review.ActorID > 0 && review.HeadSHA != ""
}

func validPendingObservation(pending PendingEventsObservation) bool {
	if pending.Count == 0 {
		return !pending.RequiresReconciliation && pending.LatestObservedHeadSHA == ""
	}
	return !pending.RequiresReconciliation || pending.LatestObservedHeadSHA != ""
}
