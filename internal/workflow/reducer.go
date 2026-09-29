package workflow

import (
	"fmt"
	"time"
)

// Reducer deterministically applies Events under one immutable Workflow Definition.
type Reducer struct {
	definition               Definition
	infrastructureRetryLimit uint8
}

// NewReducer validates the coordinator policy around an immutable Workflow Definition.
func NewReducer(definition Definition, infrastructureRetryLimit uint8) (Reducer, error) {
	if !definition.Valid() || infrastructureRetryLimit == 0 {
		return Reducer{}, fmt.Errorf("%w: reducer policy", ErrInvalidDefinition)
	}
	return Reducer{definition: definition, infrastructureRetryLimit: infrastructureRetryLimit}, nil
}

// Valid reports whether the Reducer was constructed from a Definition.
func (reducer Reducer) Valid() bool {
	return reducer.definition.Valid() && reducer.infrastructureRetryLimit > 0
}

// Definition returns the immutable Workflow Definition used by the Reducer.
func (reducer Reducer) Definition() Definition {
	return reducer.definition
}

// DefinitionCompatible reports whether durable Stage and Role identities are known to this deployment.
func (reducer Reducer) DefinitionCompatible(snapshot Snapshot) bool {
	if snapshot.State == StateAbsent {
		return true
	}
	if snapshot.CurrentAttempt != nil {
		stage, ok := reducer.definition.Stage(snapshot.CurrentAttempt.CurrentStage)
		if !ok || (snapshot.State == StateDeveloping || snapshot.State == StateReviewing) && stage.State != snapshot.State {
			return false
		}
		for stageID, used := range snapshot.CurrentAttempt.ReviewUsage {
			configured, ok := reducer.definition.Stage(stageID)
			if !ok || configured.ReviewLimit == 0 || used > configured.ReviewLimit {
				return false
			}
		}
	}
	if snapshot.ContinuationStage != "" {
		if _, ok := reducer.definition.Stage(snapshot.ContinuationStage); !ok {
			return false
		}
	}
	if snapshot.ActiveTurn != nil {
		stage, ok := reducer.definition.Stage(snapshot.ActiveTurn.Stage)
		if !ok || stage.Role != snapshot.ActiveTurn.Role {
			return false
		}
	}
	if snapshot.ResumeRole != "" {
		known := false
		for _, roleID := range reducer.definition.Roles() {
			known = known || roleID == snapshot.ResumeRole
		}
		if !known {
			return false
		}
	}
	return true
}

// DefinitionIncompatible returns a deterministic Human Handoff for durable state this deployment cannot interpret.
func (reducer Reducer) DefinitionIncompatible(snapshot Snapshot) Decision {
	if snapshot.State == StateNeedsHuman {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonWorkflowDefinitionIncompatible}
	}
	next := cloneSnapshot(snapshot)
	next.State = StateNeedsHuman
	next.Assignments.Status = AssignmentWaitingForHuman
	if next.ActiveTurn != nil {
		next.ResumeRole = next.ActiveTurn.Role
		next.ActiveTurn = nil
	}
	next.Revision++
	actions := make([]Action, 0, 4)
	if snapshot.ActiveTurn != nil {
		turn := guardFromTurn(*snapshot.ActiveTurn)
		actions = append(actions, CloseMutationAdmissionAction{Turn: turn}, InterruptTurnForHumanHandoffAction{Turn: turn})
	}
	actions = append(actions,
		MarkHumanHandoffAction{Reason: ReasonWorkflowDefinitionIncompatible, Diagnostic: "Durable Workflow state is incompatible with the deployed Workflow Definition"},
		ReconcileLabelsAction{State: StateNeedsHuman},
	)
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonWorkflowDefinitionIncompatible, Actions: actions}
}

// Reduce applies an Event using the Reducer's immutable Workflow Definition.
func (reducer Reducer) Reduce(snapshot Snapshot, event Event) Decision {
	base := cloneSnapshot(snapshot)
	if !validSnapshot(reducer.definition, reducer.infrastructureRetryLimit, snapshot) {
		return Decision{Snapshot: base, Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	metadata, kind, ok := eventDetails(event)
	if !ok || !validEvent(reducer.definition, event, metadata) {
		return Decision{Snapshot: base, Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
	if snapshot.State == StateAbsent {
		if kind != EventKindTrigger {
			return Decision{Snapshot: base, Disposition: DispositionUnrelated, Reason: ReasonWorkflowAbsent}
		}
	} else if metadata.WorkItem != snapshot.WorkItem {
		return Decision{Snapshot: base, Disposition: DispositionUnrelated, Reason: ReasonWorkItemUnrelated}
	}
	decision := reducer.dispatch(snapshot, event)
	if decision.Disposition != DispositionApplied {
		decision.Snapshot.Revision = snapshot.Revision
	}
	if decision.Reason == "" {
		decision.Reason = ReasonInvalidEvent
	}
	if !validSnapshot(reducer.definition, reducer.infrastructureRetryLimit, decision.Snapshot) {
		return Decision{Snapshot: base, Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	return decision
}

// Clone returns a deep copy whose mutable fields do not alias the Snapshot.
func (snapshot Snapshot) Clone() Snapshot {
	return cloneSnapshot(snapshot)
}

func (reducer Reducer) dispatch(snapshot Snapshot, event Event) Decision {
	switch event := event.(type) {
	case TriggerEvent:
		return reducer.reduceTrigger(snapshot, event)
	case TurnSettledEvent:
		return reducer.reduceTurnSettled(snapshot, event)
	case SynchronizationEvent:
		return reducer.reduceSynchronization(snapshot, event)
	case ReviewObservedEvent:
		return reduceReviewObserved(snapshot, event)
	case ChangeProposalObservedEvent:
		return reduceChangeProposalObserved(snapshot, event)
	case IssueClosedEvent:
		return reducer.reduceIssueClosed(snapshot, event)
	case ClosureSettledEvent:
		return reduceClosureSettled(snapshot, event)
	case IssueReopenedEvent:
		return reduceIssueReopened(snapshot, event)
	case AssignmentsCollectedEvent:
		return reduceAssignmentsCollected(snapshot, event)
	case AssignmentConfigurationConflictEvent:
		return reducer.reduceAssignmentConfigurationConflict(snapshot, event)
	case AgentTurnPreparationFailedEvent:
		return reducer.reduceAgentTurnPreparationFailed(snapshot, event)
	case AgentTurnMutationReconciliationExhaustedEvent:
		return reducer.reduceAgentTurnMutationReconciliationExhausted(snapshot, event)
	case WorkflowActionExhaustedEvent:
		return reducer.reduceWorkflowActionExhausted(snapshot, event)
	default:
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
}

func illegalInState(snapshot Snapshot) Decision {
	return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonEventIllegalInState}
}

func expectedRevisionDecision(snapshot Snapshot, metadata EventMetadata) (Decision, bool) {
	if metadata.ExpectedRevision < snapshot.Revision {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonStateRevisionStale}, true
	}
	if metadata.ExpectedRevision > snapshot.Revision {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonEventOutOfOrder}, true
	}
	return Decision{}, false
}

func deferForActiveTurn(snapshot Snapshot, metadata EventMetadata, kind EventKind) Decision {
	return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDeferred, Reason: ReasonActiveTurn, Actions: []Action{
		RecordPendingEventAction{EventID: metadata.ID, Kind: kind, ObservedAt: metadata.ObservedAt},
	}}
}

func (reducer Reducer) reduceWorkflowActionExhausted(snapshot Snapshot, event WorkflowActionExhaustedEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindWorkflowActionExhausted) {
		return illegalInState(snapshot)
	}
	if snapshot.State == StateNeedsHuman {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonWorkflowActionExhausted}
	}
	if snapshot.ActiveTurn != nil {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDeferred, Reason: ReasonActiveTurn}
	}
	stage, ok := reducer.definition.Stage(snapshot.CurrentAttempt.CurrentStage)
	if !ok {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	next := cloneSnapshot(snapshot)
	actions := make([]Action, 0, 2)
	if next.ChangeProposal != nil {
		next.ChangeProposal.ReadyForSHA = ""
	}
	next.State = StateNeedsHuman
	next.ResumeRole = stage.Role
	next.Assignments.Status = AssignmentWaitingForHuman
	next.Revision++
	actions = append(actions,
		MarkHumanHandoffAction{Reason: ReasonWorkflowActionExhausted, Diagnostic: event.Diagnostic},
		ReconcileLabelsAction{State: StateNeedsHuman},
	)
	return Decision{
		Snapshot: next, Disposition: DispositionApplied, Reason: ReasonWorkflowActionExhausted,
		Actions: actions,
	}
}

func (reducer Reducer) reduceAgentTurnMutationReconciliationExhausted(snapshot Snapshot, event AgentTurnMutationReconciliationExhaustedEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindAgentTurnMutationReconciliationExhausted) {
		return illegalInState(snapshot)
	}
	if snapshot.ActiveTurn != nil {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonActiveTurn}
	}
	stage, ok := reducer.definition.Stage(snapshot.CurrentAttempt.CurrentStage)
	if !ok || event.Role != stage.Role {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	next := cloneSnapshot(snapshot)
	next.State = StateNeedsHuman
	next.ResumeRole = event.Role
	next.Assignments.Status = AssignmentWaitingForHuman
	next.Revision++
	return Decision{
		Snapshot: next, Disposition: DispositionApplied, Reason: ReasonAgentTurnMutationReconciliationExhausted,
		Actions: []Action{
			MarkHumanHandoffAction{Reason: ReasonAgentTurnMutationReconciliationExhausted, Diagnostic: event.Diagnostic},
			ReconcileLabelsAction{State: StateNeedsHuman},
		},
	}
}

func (reducer Reducer) reduceAgentTurnPreparationFailed(snapshot Snapshot, event AgentTurnPreparationFailedEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindAgentTurnPreparationFailed) {
		return illegalInState(snapshot)
	}
	if snapshot.ActiveTurn != nil {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonActiveTurn}
	}
	stage, ok := reducer.definition.Stage(snapshot.CurrentAttempt.CurrentStage)
	if !ok || event.Role != stage.Role {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	next := cloneSnapshot(snapshot)
	next.State = StateNeedsHuman
	next.ResumeRole = event.Role
	next.Assignments.Status = AssignmentWaitingForHuman
	if !event.AssignmentsExist {
		next.Assignments.RuntimeState = RuntimeStateCollected
	}
	next.Revision++
	return Decision{
		Snapshot: next, Disposition: DispositionApplied, Reason: ReasonAgentTurnPreparationFailed,
		Actions: []Action{
			MarkHumanHandoffAction{Reason: ReasonAgentTurnPreparationFailed, Diagnostic: event.Diagnostic},
			ReconcileLabelsAction{State: StateNeedsHuman},
		},
	}
}

func (reducer Reducer) reduceAssignmentConfigurationConflict(snapshot Snapshot, event AssignmentConfigurationConflictEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindAssignmentConfigurationConflict) {
		return illegalInState(snapshot)
	}
	if snapshot.ActiveTurn != nil {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonActiveTurn}
	}
	stage, ok := reducer.definition.Stage(snapshot.CurrentAttempt.CurrentStage)
	if !ok || event.Role != stage.Role {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	next := cloneSnapshot(snapshot)
	next.State = StateNeedsHuman
	next.ResumeRole = event.Role
	next.Assignments.Status = AssignmentWaitingForHuman
	next.Revision++
	return Decision{
		Snapshot: next, Disposition: DispositionApplied, Reason: ReasonAssignmentConfigurationConflict,
		Actions: []Action{
			MarkHumanHandoffAction{Reason: ReasonAssignmentConfigurationConflict},
			ReconcileLabelsAction{State: StateNeedsHuman},
		},
	}
}

func (reducer Reducer) reduceTrigger(snapshot Snapshot, event TriggerEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if snapshot.State == StateDeveloping || snapshot.State == StateReviewing {
		if snapshot.ActiveTurn != nil {
			return deferForActiveTurn(snapshot, event.EventMetadata, EventKindTrigger)
		}
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonAttemptAlreadyActive}
	}
	if !legalInState(snapshot.State, EventKindTrigger) {
		return illegalInState(snapshot)
	}
	if event.AttemptNumber != snapshot.LastAttemptNumber+1 || (snapshot.CurrentAttempt != nil && event.AttemptID == snapshot.CurrentAttempt.ID) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
	entry := reducer.definition.InitialEntry()
	purpose := entry.Purpose
	if snapshot.State != StateAbsent {
		purpose = TurnPurposeReactivation
		switch {
		case snapshot.CurrentAttempt != nil:
			entry.Stage = snapshot.CurrentAttempt.CurrentStage
		case snapshot.ContinuationStage != "":
			entry.Stage = snapshot.ContinuationStage
		}
	}
	stage, ok := reducer.definition.Stage(entry.Stage)
	if !ok || !reducer.definition.AcceptsPurpose(entry.Stage, purpose) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	if stage.State == StateReviewing && snapshot.ChangeProposal == nil {
		entry = reducer.definition.InitialEntry()
		purpose = entry.Purpose
		stage, _ = reducer.definition.Stage(entry.Stage)
	}
	attempt := WorkflowAttempt{
		ID: event.AttemptID, Number: event.AttemptNumber, StartedAt: event.ObservedAt, Lifecycle: AttemptActive,
		CurrentStage: entry.Stage, ReviewUsage: make(map[StageID]uint8),
		InfrastructureRetryBudget: AttemptBudget{Limit: reducer.infrastructureRetryLimit},
	}
	next := cloneSnapshot(snapshot)
	next.WorkItem = event.WorkItem
	next.CurrentAttempt = &attempt
	next.LastAttemptNumber = event.AttemptNumber
	next.ActiveTurn = nil
	next.ContinuationStage = ""
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
	head := ""
	if next.ChangeProposal != nil {
		head = next.ChangeProposal.HeadSHA
	}
	next.ResumeRole = ""
	next.State = stage.State
	next.Revision++
	actions = append(actions,
		CreateAttemptAction{Attempt: attempt},
		ConsumeRunLabelAction{},
		EnqueueTurnAction{Stage: stage.ID, Role: stage.Role, Purpose: purpose, ExpectedHeadSHA: head},
		ReconcileLabelsAction{State: next.State},
	)
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonTriggered, Actions: actions}
}

func (reducer Reducer) reduceTurnSettled(snapshot Snapshot, event TurnSettledEvent) Decision {
	if event.ExistingReview != nil {
		if event.ExistingReview.ID != event.Review.ID || *event.ExistingReview != *event.Review {
			return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonReviewIdentityConflict}
		}
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonReviewDuplicate}
	}
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindTurnSettled) {
		return illegalInState(snapshot)
	}
	if snapshot.ActiveTurn == nil || !turnMatches(*snapshot.ActiveTurn, event.Turn) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonTurnGuardStale}
	}
	next := cloneSnapshot(snapshot)
	next.ActiveTurn = nil
	next.ResumeRole = ""

	if event.Outcome != TurnOutcomeBlocked && event.Outcome != TurnOutcomeInfrastructureFailed {
		if _, ok := reducer.definition.Transition(snapshot.CurrentAttempt.CurrentStage, event.Outcome); !ok {
			return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
		}
	}
	switch event.Outcome {
	case TurnOutcomeChangeProposalReady:
		return reducer.reduceDeveloperSettled(snapshot, next, event)
	case TurnOutcomeChangesRequested, TurnOutcomeApproved:
		return reducer.reduceReviewSettled(snapshot, next, event)
	case TurnOutcomeBlocked:
		return reduceBlocked(next, event)
	case TurnOutcomeInfrastructureFailed:
		return reduceInfrastructureFailure(next, event)
	default:
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvalidEvent}
	}
}

func (reducer Reducer) reduceDeveloperSettled(snapshot, next Snapshot, event TurnSettledEvent) Decision {
	if snapshot.ChangeProposal != nil && (event.ChangeProposal.ID != snapshot.ChangeProposal.ID || event.Turn.ChangeProposalID != snapshot.ChangeProposal.ID) {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	if snapshot.ChangeProposal == nil && event.Turn.ChangeProposalID != 0 {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	proposal := *event.ChangeProposal
	proposal.ReadyForSHA = ""
	next.ChangeProposal = &proposal
	transition, _ := reducer.definition.Transition(snapshot.CurrentAttempt.CurrentStage, event.Outcome)
	target, ok := reducer.definition.Stage(transition.NextStage)
	if !ok {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	next.CurrentAttempt.CurrentStage = target.ID
	next.State = target.State
	next.CurrentAttempt.InfrastructureRetryBudget.Used = 0
	next.Revision++
	intent := EnqueueTurnAction{Stage: target.ID, Role: target.Role, Purpose: transition.NextPurpose, ExpectedHeadSHA: proposal.HeadSHA}
	actions := successorActions(event.Turn, event.PendingEvents, intent)
	actions = append(actions, ReconcileLabelsAction{State: target.State})
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonChangeProposalReady, Actions: actions}
}

func (reducer Reducer) reduceReviewSettled(snapshot, next Snapshot, event TurnSettledEvent) Decision {
	if snapshot.ChangeProposal == nil || event.Turn.ChangeProposalID != snapshot.ChangeProposal.ID || event.Review.ChangeProposalID != snapshot.ChangeProposal.ID || event.ChangeProposal.ID != snapshot.ChangeProposal.ID {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	if event.Review.ActorID != event.AuthorizedReviewerActorID {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonReviewerActorUnrelated}
	}
	proposal := *event.ChangeProposal
	proposal.ReadyForSHA = ""
	next.ChangeProposal = &proposal
	next.CurrentAttempt.InfrastructureRetryBudget.Used = 0
	pendingHead := event.PendingEvents.LatestObservedHeadSHA
	pendingHeadReplaced := pendingHead != "" && (pendingHead != event.Review.HeadSHA || pendingHead != event.Turn.ExpectedHeadSHA)
	staleHead := event.Review.HeadSHA != event.Turn.ExpectedHeadSHA || event.Review.HeadSHA != proposal.HeadSHA ||
		snapshot.ChangeProposal.HeadSHA != event.Turn.ExpectedHeadSHA || pendingHeadReplaced
	if staleHead {
		stage, _ := reducer.definition.Stage(snapshot.CurrentAttempt.CurrentStage)
		next.State = stage.State
		next.Revision++
		intent := EnqueueTurnAction{Stage: stage.ID, Role: stage.Role, Purpose: TurnPurposeSynchronization, ExpectedHeadSHA: proposal.HeadSHA}
		actions := []Action{RecordReviewAction{Review: *event.Review}}
		actions = append(actions, successorActions(event.Turn, event.PendingEvents, intent)...)
		actions = append(actions, ReconcileLabelsAction{State: stage.State})
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewHeadReplaced, Actions: actions}
	}

	sourceStage := snapshot.CurrentAttempt.CurrentStage
	stage, _ := reducer.definition.Stage(sourceStage)
	transition, _ := reducer.definition.Transition(sourceStage, event.Outcome)
	if transition.ConsumesReviewCycle {
		if next.CurrentAttempt.ReviewUsage == nil {
			next.CurrentAttempt.ReviewUsage = make(map[StageID]uint8)
		}
		next.CurrentAttempt.ReviewUsage[sourceStage]++
	}
	if event.Outcome == TurnOutcomeApproved {
		next.State = transition.TerminalState
		next.CurrentAttempt.CurrentStage = transition.ContinuationStage
		next.ChangeProposal.ReadyForSHA = event.Review.HeadSHA
		next.Revision++
		actions := []Action{RecordReviewAction{Review: *event.Review, Accepted: true}, ReconcileLabelsAction{State: StatePRReady, ReadyForSHA: event.Review.HeadSHA}}
		if pendingRequiresReconciliation(event.PendingEvents) {
			actions = append([]Action{reconcilePendingAction(event.Turn, event.PendingEvents, EnqueueTurnAction{})}, actions...)
		}
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonApproved, Actions: actions}
	}
	target, ok := reducer.definition.Stage(transition.NextStage)
	if !ok {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	next.CurrentAttempt.CurrentStage = target.ID
	if transition.ConsumesReviewCycle && next.CurrentAttempt.ReviewUsage[sourceStage] >= stage.ReviewLimit {
		next.State = StateNeedsHuman
		next.ResumeRole = target.Role
		next.Assignments.Status = AssignmentWaitingForHuman
		next.Revision++
		actions := []Action{RecordReviewAction{Review: *event.Review, Accepted: true}, MarkHumanHandoffAction{Reason: ReasonReviewBudgetExhausted}, ReconcileLabelsAction{State: StateNeedsHuman}}
		if pendingRequiresReconciliation(event.PendingEvents) {
			actions = append([]Action{reconcilePendingAction(event.Turn, event.PendingEvents, EnqueueTurnAction{})}, actions...)
		}
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewBudgetExhausted, Actions: actions}
	}
	next.State = target.State
	next.Revision++
	intent := EnqueueTurnAction{Stage: target.ID, Role: target.Role, Purpose: transition.NextPurpose, ExpectedHeadSHA: proposal.HeadSHA}
	actions := []Action{RecordReviewAction{Review: *event.Review, Accepted: true}}
	actions = append(actions, successorActions(event.Turn, event.PendingEvents, intent)...)
	actions = append(actions, ReconcileLabelsAction{State: target.State})
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
		FallbackStage: fallback.Stage, FallbackRole: fallback.Role, FallbackPurpose: fallback.Purpose,
		FallbackExpectedHead: fallback.ExpectedHeadSHA, RetryOfTurnID: fallback.RetryOfTurnID,
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
		intent := EnqueueTurnAction{Stage: event.Turn.Stage, Role: event.Turn.Role, Purpose: TurnPurposeRetry, ExpectedHeadSHA: event.Turn.ExpectedHeadSHA, RetryOfTurnID: event.Turn.TurnID}
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

func (reducer Reducer) reduceSynchronization(snapshot Snapshot, event SynchronizationEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if snapshot.ActiveTurn != nil && (snapshot.State == StateDeveloping || snapshot.State == StateReviewing) {
		return deferForActiveTurn(snapshot, event.EventMetadata, EventKindSynchronization)
	}
	if !legalInState(snapshot.State, EventKindSynchronization) {
		return illegalInState(snapshot)
	}
	if snapshot.ChangeProposal == nil || snapshot.ChangeProposal.ID != event.ChangeProposalID {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonChangeProposalUnrelated}
	}
	if event.HeadSHA == snapshot.ChangeProposal.HeadSHA {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionDuplicate, Reason: ReasonSynchronizationDuplicate}
	}
	if event.PreviousHeadSHA != snapshot.ChangeProposal.HeadSHA {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionStale, Reason: ReasonSynchronizationStale}
	}
	next := cloneSnapshot(snapshot)
	next.ChangeProposal.HeadSHA = event.HeadSHA
	next.ChangeProposal.ReadyForSHA = ""
	next.ActiveTurn = nil
	stage, stageExists := reducer.definition.Stage(snapshot.CurrentAttempt.CurrentStage)
	if !stageExists {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionIllegal, Reason: ReasonInvariantViolation}
	}
	if stage.State == StateDeveloping {
		next.Revision++
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewHeadReplaced, Actions: []Action{
			ReconcileLabelsAction{State: StateDeveloping},
		}}
	}
	if stage.ReviewLimit > 0 && next.CurrentAttempt.ReviewUsage[stage.ID] >= stage.ReviewLimit {
		next.State = StateNeedsHuman
		next.ResumeRole = stage.Role
		next.Assignments.Status = AssignmentWaitingForHuman
		next.Revision++
		return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewBudgetExhausted, Actions: []Action{
			MarkHumanHandoffAction{Reason: ReasonReviewBudgetExhausted},
			ReconcileLabelsAction{State: StateNeedsHuman},
		}}
	}
	next.State = stage.State
	next.Revision++
	return Decision{Snapshot: next, Disposition: DispositionApplied, Reason: ReasonReviewHeadReplaced, Actions: []Action{
		EnqueueTurnAction{Stage: stage.ID, Role: stage.Role, Purpose: TurnPurposeSynchronization, ExpectedHeadSHA: event.HeadSHA},
		ReconcileLabelsAction{State: stage.State},
	}}
}

func reduceReviewObserved(snapshot Snapshot, event ReviewObservedEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if snapshot.ActiveTurn == nil {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonCorroborationWithoutActiveTurn}
	}
	if !legalInState(snapshot.State, EventKindReviewObserved) {
		return illegalInState(snapshot)
	}
	return deferForActiveTurn(snapshot, event.EventMetadata, EventKindReviewObserved)
}

func reduceChangeProposalObserved(snapshot Snapshot, event ChangeProposalObservedEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if snapshot.ActiveTurn == nil {
		return Decision{Snapshot: cloneSnapshot(snapshot), Disposition: DispositionUnrelated, Reason: ReasonCorroborationWithoutActiveTurn}
	}
	if !legalInState(snapshot.State, EventKindChangeProposalObserved) {
		return illegalInState(snapshot)
	}
	return deferForActiveTurn(snapshot, event.EventMetadata, EventKindChangeProposalObserved)
}

func (reducer Reducer) reduceIssueClosed(snapshot Snapshot, event IssueClosedEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindIssueClosed) {
		return illegalInState(snapshot)
	}
	next := cloneSnapshot(snapshot)
	next.State = StateClosing
	if next.CurrentAttempt != nil {
		next.ContinuationStage = next.CurrentAttempt.CurrentStage
	}
	next.Closure = &Closure{ID: event.ClosureID, RetainUntil: event.RetainUntil, RetentionToken: event.RetentionToken}
	if next.ActiveTurn != nil {
		next.ResumeRole = next.ActiveTurn.Role
	} else if next.ContinuationStage != "" {
		if stage, ok := reducer.definition.Stage(next.ContinuationStage); ok {
			next.ResumeRole = stage.Role
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
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindClosureSettled) {
		return illegalInState(snapshot)
	}
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
	if !event.AssignmentsExist {
		next.Assignments.RuntimeState = RuntimeStateCollected
	}
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

func reduceIssueReopened(snapshot Snapshot, event IssueReopenedEvent) Decision {
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if snapshot.ActiveTurn != nil && (snapshot.State == StateDeveloping || snapshot.State == StateReviewing) {
		return deferForActiveTurn(snapshot, event.EventMetadata, EventKindIssueReopened)
	}
	if !legalInState(snapshot.State, EventKindIssueReopened) {
		return illegalInState(snapshot)
	}
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
	if decision, stale := expectedRevisionDecision(snapshot, event.EventMetadata); stale {
		return decision
	}
	if !legalInState(snapshot.State, EventKindAssignmentsCollected) {
		return illegalInState(snapshot)
	}
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
	return turn.ID == guard.TurnID && turn.SessionID == guard.SessionID && turn.AttemptID == guard.AttemptID && turn.Stage == guard.Stage && turn.Role == guard.Role && turn.Epoch == guard.Epoch && turn.ControlRevision == guard.ControlRevision && turn.ChangeProposalID == guard.ChangeProposalID && turn.ExpectedHeadSHA == guard.ExpectedHeadSHA
}

func guardFromTurn(turn ActiveTurn) TurnGuard {
	return TurnGuard{
		TurnID: turn.ID, SessionID: turn.SessionID, AttemptID: turn.AttemptID, Stage: turn.Stage, Role: turn.Role,
		Epoch: turn.Epoch, ControlRevision: turn.ControlRevision, ChangeProposalID: turn.ChangeProposalID, ExpectedHeadSHA: turn.ExpectedHeadSHA,
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	clone := snapshot
	if snapshot.CurrentAttempt != nil {
		attempt := *snapshot.CurrentAttempt
		if snapshot.CurrentAttempt.ReviewUsage != nil {
			attempt.ReviewUsage = make(map[StageID]uint8, len(snapshot.CurrentAttempt.ReviewUsage))
			for stage, used := range snapshot.CurrentAttempt.ReviewUsage {
				attempt.ReviewUsage[stage] = used
			}
		}
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
