package workflow

import "time"

func Reduce(snapshot Snapshot, event Event) Decision {
	base := cloneSnapshot(snapshot)
	if !validSnapshot(snapshot) {
		return Decision{Snapshot: base, Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	metadata, kind, ok := eventDetails(event)
	if !ok || !validEvent(event, metadata) {
		return Decision{Snapshot: base, Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
	if metadata.Duplicate {
		return Decision{Snapshot: base, Disposition: DispositionDuplicate, Reason: ReasonEventDuplicate}
	}
	if snapshot.State == StateAbsent {
		if kind != EventKindTrigger {
			return Decision{Snapshot: base, Disposition: DispositionUnrelated, Reason: ReasonWorkflowAbsent}
		}
	} else if metadata.WorkItem != snapshot.WorkItem {
		return Decision{Snapshot: base, Disposition: DispositionUnrelated, Reason: ReasonWorkItemUnrelated}
	}
	if reviewDecision, ok := existingReviewDecision(snapshot, event); ok {
		return reviewDecision
	}
	if metadata.ExpectedRevision < snapshot.Revision {
		return Decision{Snapshot: base, Disposition: DispositionStale, Reason: ReasonStateRevisionStale}
	}
	if metadata.ExpectedRevision > snapshot.Revision {
		return Decision{Snapshot: base, Disposition: DispositionStale, Reason: ReasonEventOutOfOrder}
	}
	if (kind == EventKindReviewObserved || kind == EventKindChangeProposalObserved) && snapshot.ActiveTurn == nil {
		return Decision{Snapshot: base, Disposition: DispositionUnrelated, Reason: ReasonCorroborationWithoutActiveTurn}
	}
	if shouldDefer(snapshot, kind) {
		return Decision{Snapshot: base, Disposition: DispositionDeferred, Reason: ReasonActiveTurn, Actions: []Action{
			RecordPendingEventAction{EventID: metadata.ID, Kind: kind, ObservedAt: metadata.ObservedAt},
		}}
	}
	if kind == EventKindTrigger && (snapshot.State == StateDeveloping || snapshot.State == StateReviewing) {
		return Decision{Snapshot: base, Disposition: DispositionDuplicate, Reason: ReasonAttemptAlreadyActive}
	}
	if !legalInState(snapshot.State, kind) {
		return Decision{Snapshot: base, Disposition: DispositionIllegal, Reason: ReasonEventIllegalInState}
	}

	decision := dispatch(snapshot, event)
	if decision.Disposition != DispositionApplied {
		decision.Snapshot.Revision = snapshot.Revision
	}
	if decision.Reason == "" {
		decision.Reason = ReasonInvalidEvent
	}
	if !validSnapshot(decision.Snapshot) {
		return Decision{Snapshot: base, Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	return decision
}

func dispatch(snapshot Snapshot, event Event) Decision {
	switch event := event.(type) {
	case TriggerEvent:
		return reduceTrigger(snapshot, event)
	case TurnSettledEvent:
		return reduceTurnSettled(snapshot, event)
	case SynchronizationEvent:
		return reduceSynchronization(snapshot, event)
	case ReviewObservedEvent:
		return reduceReviewObserved(snapshot, event)
	case ChangeProposalObservedEvent:
		return reduceChangeProposalObserved(snapshot, event)
	case IssueClosedEvent:
		return reduceIssueClosed(snapshot, event)
	case ClosureSettledEvent:
		return reduceClosureSettled(snapshot, event)
	case IssueReopenedEvent:
		return reduceIssueReopened(snapshot, event)
	case AssignmentsCollectedEvent:
		return reduceAssignmentsCollected(snapshot, event)
	default:
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
}

func reduceTrigger(snapshot Snapshot, event TriggerEvent) Decision {
	if event.AttemptNumber != snapshot.LastAttemptNumber+1 || (snapshot.CurrentAttempt != nil && event.AttemptID == snapshot.CurrentAttempt.ID) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
	attempt := WorkflowAttempt{
		ID: event.AttemptID, Number: event.AttemptNumber, StartedAt: event.ObservedAt, Lifecycle: AttemptActive,
		ReviewBudget: Budget{Limit: 3}, InfrastructureRetryBudget: Budget{Limit: 1},
	}
	next := cloneSnapshot(snapshot)
	next.WorkItem = event.WorkItem
	next.CurrentAttempt = &attempt
	next.LastAttemptNumber = event.AttemptNumber
	next.ActiveTurn = nil
	next.Closure = nil
	if next.ChangeProposal != nil {
		next.ChangeProposal.ReadyForSHA = ""
	}
	actions := make([]Action, 0, 7)
	if snapshot.CurrentAttempt != nil {
		actions = append(actions, CompleteAttemptAction{AttemptID: snapshot.CurrentAttempt.ID, Reason: AttemptCompletionSuperseded})
	}
	assignmentMode := AssignmentGenerationCurrent
	if snapshot.State == StateAbsent || snapshot.Assignments.RuntimeState == RuntimeStateCollected {
		assignmentMode = AssignmentGenerationNew
	} else if snapshot.Assignments.RuntimeState == RuntimeStateRetained {
		assignmentMode = AssignmentGenerationRetained
	}
	next.Assignments = Assignments{Status: AssignmentActive, RuntimeState: RuntimeStateActive}
	actions = append(actions, EnsureAssignmentsAction{Mode: assignmentMode})
	role := snapshot.ResumeRole
	if role == "" || (role == RoleReviewer && next.ChangeProposal == nil) {
		role = RoleDeveloper
	}
	head := ""
	if next.ChangeProposal != nil {
		head = next.ChangeProposal.HeadSHA
	}
	next.ResumeRole = ""
	if role == RoleReviewer {
		next.State = StateReviewing
	} else {
		next.State = StateDeveloping
	}
	next.Revision++
	purpose := TurnPurposeInitialDevelopment
	if snapshot.State != StateAbsent {
		purpose = TurnPurposeReactivation
	}
	actions = append(actions,
		CreateAttemptAction{Attempt: attempt},
		ConsumeRunLabelAction{},
		EnqueueTurnAction{Role: role, Purpose: purpose, ExpectedHeadSHA: head},
		ReconcileLabelsAction{State: next.State},
	)
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonTriggered, Actions: actions}
}

func reduceTurnSettled(snapshot Snapshot, event TurnSettledEvent) Decision {
	if snapshot.ActiveTurn == nil || !turnMatches(*snapshot.ActiveTurn, event.Turn) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	next := cloneSnapshot(snapshot)
	next.ActiveTurn = nil
	next.ResumeRole = ""

	switch event.Outcome {
	case TurnOutcomeChangeProposalReady:
		return reduceDeveloperSettled(snapshot, next, event)
	case TurnOutcomeChangesRequested, TurnOutcomeApproved:
		return reduceReviewSettled(snapshot, next, event)
	case TurnOutcomeBlocked:
		return reduceBlocked(next, event)
	case TurnOutcomeInfrastructureFailed:
		return reduceInfrastructureFailure(next, event)
	default:
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
}

func existingReviewDecision(snapshot Snapshot, event Event) (Decision, bool) {
	settled, ok := event.(TurnSettledEvent)
	if !ok || (settled.Outcome != TurnOutcomeChangesRequested && settled.Outcome != TurnOutcomeApproved) || settled.ExistingReview == nil {
		return Decision{}, false
	}
	if settled.ExistingReview.ID != settled.Review.ID || *settled.ExistingReview != *settled.Review {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonReviewIdentityConflict}, true
	}
	return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonReviewDuplicate}, true
}

func reduceDeveloperSettled(snapshot, next Snapshot, event TurnSettledEvent) Decision {
	if event.Turn.Role != RoleDeveloper {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
	if snapshot.ChangeProposal != nil && (event.ChangeProposal.ID != snapshot.ChangeProposal.ID || event.Turn.ChangeProposalID != snapshot.ChangeProposal.ID) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	if snapshot.ChangeProposal == nil && event.Turn.ChangeProposalID != 0 {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	proposal := *event.ChangeProposal
	proposal.ReadyForSHA = ""
	next.ChangeProposal = &proposal
	next.State = StateReviewing
	next.CurrentAttempt.InfrastructureRetryBudget.Used = 0
	next.Revision++
	intent := EnqueueTurnAction{Role: RoleReviewer, Purpose: TurnPurposeReview, ExpectedHeadSHA: proposal.HeadSHA}
	actions := successorActions(event.Turn, event.PendingEvents, intent)
	actions = append(actions, ReconcileLabelsAction{State: StateReviewing})
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonChangeProposalReady, Actions: actions}
}

func reduceReviewSettled(snapshot, next Snapshot, event TurnSettledEvent) Decision {
	if event.Turn.Role != RoleReviewer || snapshot.ChangeProposal == nil || event.Turn.ChangeProposalID != snapshot.ChangeProposal.ID || event.Review.ChangeProposalID != snapshot.ChangeProposal.ID || event.ChangeProposal.ID != snapshot.ChangeProposal.ID {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	if event.Review.ActorID != event.AuthorizedReviewerActorID {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonReviewerActorUnrelated}
	}
	proposal := *event.ChangeProposal
	proposal.ReadyForSHA = ""
	next.ChangeProposal = &proposal
	next.CurrentAttempt.InfrastructureRetryBudget.Used = 0
	staleHead := event.Review.HeadSHA != event.Turn.ExpectedHeadSHA || event.Review.HeadSHA != proposal.HeadSHA || snapshot.ChangeProposal.HeadSHA != event.Turn.ExpectedHeadSHA
	if staleHead {
		next.State = StateReviewing
		next.Revision++
		intent := EnqueueTurnAction{Role: RoleReviewer, Purpose: TurnPurposeSynchronization, ExpectedHeadSHA: proposal.HeadSHA}
		actions := []Action{RecordReviewAction{Review: *event.Review}}
		actions = append(actions, successorActions(event.Turn, event.PendingEvents, intent)...)
		actions = append(actions, ReconcileLabelsAction{State: StateReviewing})
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewHeadReplaced, Actions: actions}
	}

	next.CurrentAttempt.ReviewBudget.Used++
	if event.Outcome == TurnOutcomeApproved {
		next.State = StatePRReady
		next.ChangeProposal.ReadyForSHA = event.Review.HeadSHA
		next.Revision++
		actions := []Action{RecordReviewAction{Review: *event.Review, Accepted: true}, ReconcileLabelsAction{State: StatePRReady, ReadyForSHA: event.Review.HeadSHA}}
		if pendingRequiresReconciliation(event.PendingEvents) {
			actions = append([]Action{reconcilePendingAction(event.Turn, event.PendingEvents, EnqueueTurnAction{})}, actions...)
		}
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonApproved, Actions: actions}
	}
	if next.CurrentAttempt.ReviewBudget.Used >= next.CurrentAttempt.ReviewBudget.Limit {
		next.State = StateNeedsHuman
		next.ResumeRole = RoleDeveloper
		next.Assignments.Status = AssignmentWaitingForHuman
		next.Revision++
		actions := []Action{RecordReviewAction{Review: *event.Review, Accepted: true}, MarkHumanHandoffAction{Reason: ReasonReviewBudgetExhausted}, ReconcileLabelsAction{State: StateNeedsHuman}}
		if pendingRequiresReconciliation(event.PendingEvents) {
			actions = append([]Action{reconcilePendingAction(event.Turn, event.PendingEvents, EnqueueTurnAction{})}, actions...)
		}
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewBudgetExhausted, Actions: actions}
	}
	next.State = StateDeveloping
	next.Revision++
	intent := EnqueueTurnAction{Role: RoleDeveloper, Purpose: TurnPurposeRequestedChanges, ExpectedHeadSHA: proposal.HeadSHA}
	actions := []Action{RecordReviewAction{Review: *event.Review, Accepted: true}}
	actions = append(actions, successorActions(event.Turn, event.PendingEvents, intent)...)
	actions = append(actions, ReconcileLabelsAction{State: StateDeveloping})
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonChangesRequested, Actions: actions}
}

func successorActions(sourceTurn TurnGuard, pending PendingEventsObservation, intent EnqueueTurnAction) []Action {
	if pendingRequiresReconciliation(pending) {
		return []Action{reconcilePendingAction(sourceTurn, pending, intent)}
	}
	return []Action{intent}
}

func pendingRequiresReconciliation(pending PendingEventsObservation) bool {
	return pending.Count > 0
}

func reconcilePendingAction(sourceTurn TurnGuard, pending PendingEventsObservation, fallback EnqueueTurnAction) ReconcilePendingEventsAction {
	return ReconcilePendingEventsAction{
		SourceTurn: sourceTurn, Count: pending.Count, LatestObservedHeadSHA: pending.LatestObservedHeadSHA,
		FallbackRole: fallback.Role, FallbackPurpose: fallback.Purpose, FallbackExpectedHead: fallback.ExpectedHeadSHA, RetryOfTurnID: fallback.RetryOfTurnID,
	}
}

func reduceBlocked(next Snapshot, event TurnSettledEvent) Decision {
	next.State = StateNeedsHuman
	next.ResumeRole = event.Turn.Role
	next.Assignments.Status = AssignmentWaitingForHuman
	next.Revision++
	actions := []Action{
		MarkHumanHandoffAction{Reason: ReasonAgentBlocked, Diagnostic: event.Diagnostic},
		ReconcileLabelsAction{State: StateNeedsHuman},
	}
	if pendingRequiresReconciliation(event.PendingEvents) {
		actions = append([]Action{reconcilePendingAction(event.Turn, event.PendingEvents, EnqueueTurnAction{})}, actions...)
	}
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonAgentBlocked, Actions: actions}
}

func reduceInfrastructureFailure(next Snapshot, event TurnSettledEvent) Decision {
	if next.CurrentAttempt.InfrastructureRetryBudget.Used < next.CurrentAttempt.InfrastructureRetryBudget.Limit {
		next.CurrentAttempt.InfrastructureRetryBudget.Used++
		next.Revision++
		intent := EnqueueTurnAction{Role: event.Turn.Role, Purpose: TurnPurposeRetry, ExpectedHeadSHA: event.Turn.ExpectedHeadSHA, RetryOfTurnID: event.Turn.TurnID}
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonInfrastructureRetry, Actions: successorActions(event.Turn, event.PendingEvents, intent)}
	}
	next.State = StateNeedsHuman
	next.ResumeRole = event.Turn.Role
	next.Assignments.Status = AssignmentWaitingForHuman
	next.Revision++
	actions := []Action{
		MarkHumanHandoffAction{Reason: ReasonInfrastructureRetriesExhausted, Diagnostic: event.Diagnostic},
		ReconcileLabelsAction{State: StateNeedsHuman},
	}
	if pendingRequiresReconciliation(event.PendingEvents) {
		actions = append([]Action{reconcilePendingAction(event.Turn, event.PendingEvents, EnqueueTurnAction{})}, actions...)
	}
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonInfrastructureRetriesExhausted, Actions: actions}
}

func reduceSynchronization(snapshot Snapshot, event SynchronizationEvent) Decision {
	if snapshot.ChangeProposal == nil || snapshot.ChangeProposal.ID != event.ChangeProposalID {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	if event.PreviousHeadSHA != snapshot.ChangeProposal.HeadSHA {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonSynchronizationStale}
	}
	if event.HeadSHA == snapshot.ChangeProposal.HeadSHA {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonSynchronizationDuplicate}
	}
	next := cloneSnapshot(snapshot)
	next.ChangeProposal.HeadSHA = event.HeadSHA
	next.ChangeProposal.ReadyForSHA = ""
	next.ActiveTurn = nil
	if snapshot.State == StateDeveloping {
		next.Revision++
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewHeadReplaced, Actions: []Action{
			ReconcileLabelsAction{State: StateDeveloping},
		}}
	}
	if next.CurrentAttempt.ReviewBudget.Used >= next.CurrentAttempt.ReviewBudget.Limit {
		next.State = StateNeedsHuman
		next.ResumeRole = RoleReviewer
		next.Assignments.Status = AssignmentWaitingForHuman
		next.Revision++
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewBudgetExhausted, Actions: []Action{
			MarkHumanHandoffAction{Reason: ReasonReviewBudgetExhausted},
			ReconcileLabelsAction{State: StateNeedsHuman},
		}}
	}
	next.State = StateReviewing
	next.Revision++
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewHeadReplaced, Actions: []Action{
		EnqueueTurnAction{Role: RoleReviewer, Purpose: TurnPurposeSynchronization, ExpectedHeadSHA: event.HeadSHA},
		ReconcileLabelsAction{State: StateReviewing},
	}}
}

func reduceReviewObserved(snapshot Snapshot, event ReviewObservedEvent) Decision {
	return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDeferred, Reason: ReasonActiveTurn, Actions: []Action{
		RecordPendingEventAction{EventID: event.ID, Kind: EventKindReviewObserved, ObservedAt: event.ObservedAt},
	}}
}

func reduceChangeProposalObserved(snapshot Snapshot, event ChangeProposalObservedEvent) Decision {
	return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDeferred, Reason: ReasonActiveTurn, Actions: []Action{
		RecordPendingEventAction{EventID: event.ID, Kind: EventKindChangeProposalObserved, ObservedAt: event.ObservedAt},
	}}
}

func reduceIssueClosed(snapshot Snapshot, event IssueClosedEvent) Decision {
	next := cloneSnapshot(snapshot)
	next.State = StateClosing
	next.Closure = &Closure{ID: event.ClosureID, RetainUntil: event.RetainUntil, RetentionToken: event.RetentionToken}
	if next.ActiveTurn != nil {
		next.ResumeRole = next.ActiveTurn.Role
	} else {
		switch snapshot.State {
		case StateDeveloping:
			next.ResumeRole = RoleDeveloper
		case StateReviewing, StatePRReady:
			next.ResumeRole = RoleReviewer
		}
	}
	if next.ChangeProposal != nil {
		next.ChangeProposal.ReadyForSHA = ""
	}
	next.Revision++
	actions := make([]Action, 0, 4)
	if next.ActiveTurn != nil {
		turn := guardFromTurn(*next.ActiveTurn)
		actions = append(actions, CloseMutationAdmissionAction{Turn: turn}, StopTurnAction{Turn: turn})
	}
	actions = append(actions, SettleClosureAction{ClosureID: event.ClosureID}, ReconcileLabelsAction{State: StateClosing})
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonClosureStarted, Actions: actions}
}

func reduceClosureSettled(snapshot Snapshot, event ClosureSettledEvent) Decision {
	if snapshot.Closure == nil || snapshot.Closure.ID != event.ClosureID {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	if (snapshot.ActiveTurn == nil) != (event.Turn == nil) || (snapshot.ActiveTurn != nil && !turnMatches(*snapshot.ActiveTurn, *event.Turn)) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	next := cloneSnapshot(snapshot)
	closure := *next.Closure
	next.CurrentAttempt = nil
	next.ActiveTurn = nil
	next.Closure = nil
	next.Assignments.Status = AssignmentCompleted
	if next.ChangeProposal != nil {
		next.ChangeProposal.ReadyForSHA = ""
	}
	actions := make([]Action, 0, 4)
	if snapshot.CurrentAttempt != nil {
		actions = append(actions, CompleteAttemptAction{AttemptID: snapshot.CurrentAttempt.ID, Reason: AttemptCompletionIssueClosed})
	}
	actions = append(actions, CompleteAssignmentsAction{})
	if closure.ReopenRequested {
		next.State = StateDormant
		if next.Assignments.RuntimeState != RuntimeStateCollected {
			next.Assignments.RuntimeState = RuntimeStateRetained
		}
		next.Assignments.RetainedUntil = time.Time{}
		next.Assignments.RetentionToken = ""
	} else {
		next.State = StateClosed
		if next.Assignments.RuntimeState != RuntimeStateCollected {
			next.Assignments.RuntimeState = RuntimeStateRetained
			next.Assignments.RetainedUntil = closure.RetainUntil
			next.Assignments.RetentionToken = closure.RetentionToken
			actions = append(actions, ScheduleRetentionAction{RetentionToken: closure.RetentionToken, RetainUntil: closure.RetainUntil})
		} else {
			next.Assignments.RetainedUntil = time.Time{}
			next.Assignments.RetentionToken = ""
		}
	}
	next.Revision++
	actions = append(actions, ReconcileLabelsAction{State: next.State})
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonClosureSettled, Actions: actions}
}

func reduceIssueReopened(snapshot Snapshot, _ IssueReopenedEvent) Decision {
	next := cloneSnapshot(snapshot)
	if snapshot.State == StateClosing {
		if next.Closure.ReopenRequested {
			return Decision{Snapshot: next, Disposition: DispositionDuplicate, Reason: ReasonEventDuplicate}
		}
		next.Closure.ReopenRequested = true
		next.Revision++
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonClosureCancellationRecorded, Actions: []Action{
			CancelRetentionAction{RetentionToken: next.Closure.RetentionToken},
			ReconcileLabelsAction{State: StateClosing},
		}}
	}
	if snapshot.State != StateClosed {
		return Decision{Snapshot: next, Disposition: DispositionIllegal, Reason: ReasonEventIllegalInState}
	}
	token := next.Assignments.RetentionToken
	next.State = StateDormant
	next.Assignments.RetentionToken = ""
	next.Assignments.RetainedUntil = time.Time{}
	next.Revision++
	actions := []Action{ReconcileLabelsAction{State: StateDormant}}
	if token != "" {
		actions = append([]Action{CancelRetentionAction{RetentionToken: token}}, actions...)
	}
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonIssueReopened, Actions: actions}
}

func reduceAssignmentsCollected(snapshot Snapshot, event AssignmentsCollectedEvent) Decision {
	if snapshot.State == StateDormant {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonRetentionCancelled}
	}
	if snapshot.Assignments.RuntimeState == RuntimeStateCollected {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonEventDuplicate}
	}
	if event.RetentionToken != snapshot.Assignments.RetentionToken || !event.RetainUntil.Equal(snapshot.Assignments.RetainedUntil) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonRetentionGenerationStale}
	}
	if event.CollectedAt.Before(event.RetainUntil) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonRetentionNotDue}
	}
	next := cloneSnapshot(snapshot)
	next.Assignments.RuntimeState = RuntimeStateCollected
	next.Assignments.RetentionToken = ""
	next.Assignments.RetainedUntil = time.Time{}
	next.Revision++
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonAssignmentsCollected}
}

func turnMatches(turn ActiveTurn, guard TurnGuard) bool {
	return turn.ID == guard.TurnID && turn.SessionID == guard.SessionID && turn.AttemptID == guard.AttemptID && turn.Role == guard.Role && turn.Epoch == guard.Epoch && turn.ControlRevision == guard.ControlRevision && turn.ChangeProposalID == guard.ChangeProposalID && turn.ExpectedHeadSHA == guard.ExpectedHeadSHA
}

func guardFromTurn(turn ActiveTurn) TurnGuard {
	return TurnGuard{
		TurnID: turn.ID, SessionID: turn.SessionID, AttemptID: turn.AttemptID, Role: turn.Role,
		Epoch: turn.Epoch, ControlRevision: turn.ControlRevision, ChangeProposalID: turn.ChangeProposalID, ExpectedHeadSHA: turn.ExpectedHeadSHA,
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	clone := snapshot
	if snapshot.CurrentAttempt != nil {
		attempt := *snapshot.CurrentAttempt
		clone.CurrentAttempt = &attempt
	}
	if snapshot.ChangeProposal != nil {
		proposal := *snapshot.ChangeProposal
		clone.ChangeProposal = &proposal
	}
	if snapshot.ActiveTurn != nil {
		turn := *snapshot.ActiveTurn
		clone.ActiveTurn = &turn
	}
	if snapshot.Closure != nil {
		closure := *snapshot.Closure
		clone.Closure = &closure
	}
	return clone
}
