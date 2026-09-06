package workflow

import "time"

type State string

const (
	StateAbsent     State = "ABSENT"
	StateDormant    State = "DORMANT"
	StateDeveloping State = "DEVELOPING"
	StateReviewing  State = "REVIEWING"
	StatePRReady    State = "PR_READY"
	StateNeedsHuman State = "NEEDS_HUMAN"
	StateClosing    State = "CLOSING"
	StateClosed     State = "CLOSED"
)

type Role string

const (
	RoleDeveloper Role = "DEVELOPER"
	RoleReviewer  Role = "REVIEWER"
)

type Disposition string

const (
	DispositionApplied              Disposition = "APPLIED"
	DispositionDeferred             Disposition = "DEFERRED"
	DispositionDuplicate            Disposition = "DUPLICATE"
	DispositionStale                Disposition = "STALE"
	DispositionUnrelated            Disposition = "UNRELATED"
	DispositionIllegal              Disposition = "ILLEGAL"
	DispositionReconciliationFailed Disposition = "RECONCILIATION_FAILED"
)

type Reason string

const (
	ReasonTriggered                      Reason = "workflow_triggered"
	ReasonChangeProposalReady            Reason = "change_proposal_ready"
	ReasonChangesRequested               Reason = "changes_requested"
	ReasonApproved                       Reason = "approved"
	ReasonReviewBudgetExhausted          Reason = "review_budget_exhausted"
	ReasonReviewHeadReplaced             Reason = "review_head_replaced"
	ReasonAgentBlocked                   Reason = "agent_blocked"
	ReasonInfrastructureRetry            Reason = "infrastructure_retry"
	ReasonInfrastructureRetriesExhausted Reason = "infrastructure_retries_exhausted"
	ReasonClosureStarted                 Reason = "closure_started"
	ReasonClosureSettled                 Reason = "closure_settled"
	ReasonClosureCancellationRecorded    Reason = "closure_cancellation_recorded"
	ReasonIssueReopened                  Reason = "issue_reopened"
	ReasonActiveTurn                     Reason = "active_turn"
	ReasonEventDuplicate                 Reason = "event_duplicate"
	ReasonReviewDuplicate                Reason = "review_duplicate"
	ReasonReviewIdentityConflict         Reason = "review_identity_conflict"
	ReasonStateRevisionStale             Reason = "state_revision_stale"
	ReasonEventOutOfOrder                Reason = "event_out_of_order"
	ReasonSynchronizationStale           Reason = "synchronization_stale"
	ReasonSynchronizationDuplicate       Reason = "synchronization_duplicate"
	ReasonWorkItemUnrelated              Reason = "work_item_unrelated"
	ReasonReviewerActorUnrelated         Reason = "reviewer_actor_unrelated"
	ReasonTurnGuardStale                 Reason = "turn_guard_stale"
	ReasonChangeProposalUnrelated        Reason = "change_proposal_unrelated"
	ReasonAssignmentsCollected           Reason = "assignments_collected"
	ReasonRetentionGenerationStale       Reason = "retention_generation_stale"
	ReasonRetentionCancelled             Reason = "retention_cancelled"
	ReasonRetentionNotDue                Reason = "retention_not_due"
	ReasonEventIllegalInState            Reason = "event_illegal_in_state"
	ReasonInvariantViolation             Reason = "aggregate_invariant_violation"
	ReasonInvalidEvent                   Reason = "invalid_event"
	ReasonWorkflowAbsent                 Reason = "workflow_absent"
	ReasonAttemptAlreadyActive           Reason = "attempt_already_active"
	ReasonCorroborationWithoutActiveTurn Reason = "corroboration_without_active_turn"
)

const ReasonAssignmentConfigurationConflict Reason = "assignment_configuration_conflict"

const ReasonAgentTurnPreparationFailed Reason = "agent_turn_preparation_failed"

const ReasonAgentTurnMutationReconciliationExhausted Reason = "agent_turn_mutation_reconciliation_exhausted"

const ReasonWorkflowActionExhausted Reason = "workflow_action_exhausted"

type WorkItem struct {
	RepositoryID int64
	IssueID      int64
	IssueNumber  int64
}

type Budget struct {
	Used  uint8
	Limit uint8
}

type AttemptLifecycle string

const AttemptActive AttemptLifecycle = "ACTIVE"

type WorkflowAttempt struct {
	ID                        string
	Number                    uint64
	StartedAt                 time.Time
	Lifecycle                 AttemptLifecycle
	ReviewBudget              Budget
	InfrastructureRetryBudget Budget
}

type AttemptCompletionReason string

const (
	AttemptCompletionSuperseded  AttemptCompletionReason = "SUPERSEDED"
	AttemptCompletionIssueClosed AttemptCompletionReason = "ISSUE_CLOSED"
)

type ActiveTurn struct {
	ID               string
	SessionID        string
	AttemptID        string
	Role             Role
	Epoch            uint64
	ControlRevision  uint64
	ChangeProposalID int64
	ExpectedHeadSHA  string
}

type TurnGuard struct {
	TurnID           string
	SessionID        string
	AttemptID        string
	Role             Role
	Epoch            uint64
	ControlRevision  uint64
	ChangeProposalID int64
	ExpectedHeadSHA  string
}

type ChangeProposal struct {
	ID          int64
	Number      int64
	HeadSHA     string
	Open        bool
	ReadyForSHA string
}

type ReviewIdentity struct {
	ID               int64
	NodeID           string
	ChangeProposalID int64
	ActorID          int64
	HeadSHA          string
}

type AssignmentStatus string

const (
	AssignmentActive          AssignmentStatus = "ACTIVE"
	AssignmentWaitingForHuman AssignmentStatus = "WAITING_FOR_HUMAN"
	AssignmentCompleted       AssignmentStatus = "COMPLETED"
)

type RuntimeState string

const (
	RuntimeStateActive    RuntimeState = "ACTIVE"
	RuntimeStateRetained  RuntimeState = "RETAINED"
	RuntimeStateCollected RuntimeState = "COLLECTED"
)

type Assignments struct {
	Status         AssignmentStatus
	RuntimeState   RuntimeState
	RetainedUntil  time.Time
	RetentionToken string
}

type Closure struct {
	ID              string
	RetainUntil     time.Time
	RetentionToken  string
	ReopenRequested bool
}

type Snapshot struct {
	State             State
	Revision          uint64
	WorkItem          WorkItem
	CurrentAttempt    *WorkflowAttempt
	ChangeProposal    *ChangeProposal
	ActiveTurn        *ActiveTurn
	ResumeRole        Role
	Assignments       Assignments
	Closure           *Closure
	LastAttemptNumber uint64
}

type Decision struct {
	Snapshot    Snapshot
	Disposition Disposition
	Reason      Reason
	Actions     []Action
}

type Event interface {
	isWorkflowEvent()
}

type EventMetadata struct {
	ID               string
	ObservedAt       time.Time
	WorkItem         WorkItem
	ExpectedRevision uint64
	Duplicate        bool
}

type TriggerEvent struct {
	EventMetadata
	AttemptID     string
	AttemptNumber uint64
}

func (TriggerEvent) isWorkflowEvent() {}

type TurnOutcome string

const (
	TurnOutcomeChangeProposalReady  TurnOutcome = "CHANGE_PROPOSAL_READY"
	TurnOutcomeChangesRequested     TurnOutcome = "CHANGES_REQUESTED"
	TurnOutcomeApproved             TurnOutcome = "APPROVED"
	TurnOutcomeBlocked              TurnOutcome = "BLOCKED"
	TurnOutcomeInfrastructureFailed TurnOutcome = "INFRASTRUCTURE_FAILED"
)

type PendingEventsObservation struct {
	Count                  uint32
	RequiresReconciliation bool
	LatestObservedHeadSHA  string
}

type TurnSettledEvent struct {
	EventMetadata
	Turn                      TurnGuard
	Outcome                   TurnOutcome
	ChangeProposal            *ChangeProposal
	Review                    *ReviewIdentity
	ExistingReview            *ReviewIdentity
	AuthorizedReviewerActorID int64
	PendingEvents             PendingEventsObservation
	Diagnostic                string
}

func (TurnSettledEvent) isWorkflowEvent() {}

type SynchronizationEvent struct {
	EventMetadata
	ChangeProposalID int64
	PreviousHeadSHA  string
	HeadSHA          string
}

func (SynchronizationEvent) isWorkflowEvent() {}

type ReviewObservedEvent struct {
	EventMetadata
	Review ReviewIdentity
}

func (ReviewObservedEvent) isWorkflowEvent() {}

type ChangeProposalObservedEvent struct {
	EventMetadata
	ChangeProposal ChangeProposal
}

func (ChangeProposalObservedEvent) isWorkflowEvent() {}

type IssueClosedEvent struct {
	EventMetadata
	ClosureID      string
	RetainUntil    time.Time
	RetentionToken string
}

func (IssueClosedEvent) isWorkflowEvent() {}

type ClosureSettledEvent struct {
	EventMetadata
	ClosureID        string
	Turn             *TurnGuard
	AssignmentsExist bool
}

func (ClosureSettledEvent) isWorkflowEvent() {}

type IssueReopenedEvent struct {
	EventMetadata
}

func (IssueReopenedEvent) isWorkflowEvent() {}

type AssignmentsCollectedEvent struct {
	EventMetadata
	RetentionToken string
	RetainUntil    time.Time
	CollectedAt    time.Time
}

func (AssignmentsCollectedEvent) isWorkflowEvent() {}

type AssignmentConfigurationConflictEvent struct {
	EventMetadata
	Role Role
}

func (AssignmentConfigurationConflictEvent) isWorkflowEvent() {}

type AgentTurnPreparationFailedEvent struct {
	EventMetadata
	Role             Role
	Diagnostic       string
	AssignmentsExist bool
}

func (AgentTurnPreparationFailedEvent) isWorkflowEvent() {}

type AgentTurnMutationReconciliationExhaustedEvent struct {
	EventMetadata
	Role       Role
	Diagnostic string
}

func (AgentTurnMutationReconciliationExhaustedEvent) isWorkflowEvent() {}

// WorkflowActionExhaustedEvent records terminal failure of durable Workflow coordination or an effect.
type WorkflowActionExhaustedEvent struct {
	EventMetadata
	ResumeRole Role
	Diagnostic string
}

func (WorkflowActionExhaustedEvent) isWorkflowEvent() {}

type EventKind string

const (
	EventKindTrigger                EventKind = "TRIGGER"
	EventKindTurnSettled            EventKind = "TURN_SETTLED"
	EventKindSynchronization        EventKind = "SYNCHRONIZATION"
	EventKindReviewObserved         EventKind = "REVIEW_OBSERVED"
	EventKindChangeProposalObserved EventKind = "CHANGE_PROPOSAL_OBSERVED"
	EventKindIssueClosed            EventKind = "ISSUE_CLOSED"
	EventKindClosureSettled         EventKind = "CLOSURE_SETTLED"
	EventKindIssueReopened          EventKind = "ISSUE_REOPENED"
	EventKindAssignmentsCollected   EventKind = "ASSIGNMENTS_COLLECTED"
)

const EventKindAssignmentConfigurationConflict EventKind = "ASSIGNMENT_CONFIGURATION_CONFLICT"

const EventKindAgentTurnPreparationFailed EventKind = "AGENT_TURN_PREPARATION_FAILED"

const EventKindAgentTurnMutationReconciliationExhausted EventKind = "AGENT_TURN_MUTATION_RECONCILIATION_EXHAUSTED"

const EventKindWorkflowActionExhausted EventKind = "WORKFLOW_ACTION_EXHAUSTED"

type TurnPurpose string

const (
	TurnPurposeInitialDevelopment TurnPurpose = "INITIAL_DEVELOPMENT"
	TurnPurposeReview             TurnPurpose = "REVIEW"
	TurnPurposeRequestedChanges   TurnPurpose = "REQUESTED_CHANGES"
	TurnPurposeRetry              TurnPurpose = "RETRY"
	TurnPurposeSynchronization    TurnPurpose = "SYNCHRONIZATION"
	TurnPurposeReactivation       TurnPurpose = "REACTIVATION"
)

type Action interface {
	isWorkflowAction()
}

type AssignmentGeneration string

const (
	AssignmentGenerationCurrent  AssignmentGeneration = "CURRENT"
	AssignmentGenerationRetained AssignmentGeneration = "RETAINED"
	AssignmentGenerationNew      AssignmentGeneration = "NEW"
)

type EnsureAssignmentsAction struct {
	Mode AssignmentGeneration
}

func (EnsureAssignmentsAction) isWorkflowAction() {}

type CompleteAttemptAction struct {
	AttemptID string
	Reason    AttemptCompletionReason
}

func (CompleteAttemptAction) isWorkflowAction() {}

type CreateAttemptAction struct {
	Attempt WorkflowAttempt
}

func (CreateAttemptAction) isWorkflowAction() {}

type ConsumeRunLabelAction struct{}

func (ConsumeRunLabelAction) isWorkflowAction() {}

type EnqueueTurnAction struct {
	Role            Role
	Purpose         TurnPurpose
	ExpectedHeadSHA string
	RetryOfTurnID   string
}

func (EnqueueTurnAction) isWorkflowAction() {}

type ReconcileLabelsAction struct {
	State       State
	ReadyForSHA string
}

func (ReconcileLabelsAction) isWorkflowAction() {}

type RecordReviewAction struct {
	Review   ReviewIdentity
	Accepted bool
}

func (RecordReviewAction) isWorkflowAction() {}

type MarkHumanHandoffAction struct {
	Reason     Reason
	Diagnostic string
}

func (MarkHumanHandoffAction) isWorkflowAction() {}

type CloseMutationAdmissionAction struct {
	Turn TurnGuard
}

func (CloseMutationAdmissionAction) isWorkflowAction() {}

type StopTurnAction struct {
	Turn TurnGuard
}

func (StopTurnAction) isWorkflowAction() {}

type InterruptTurnForHumanHandoffAction struct {
	Turn TurnGuard
}

func (InterruptTurnForHumanHandoffAction) isWorkflowAction() {}

type SettleClosureAction struct {
	ClosureID string
}

func (SettleClosureAction) isWorkflowAction() {}

type CompleteAssignmentsAction struct{}

func (CompleteAssignmentsAction) isWorkflowAction() {}

type ScheduleRetentionAction struct {
	RetentionToken string
	RetainUntil    time.Time
}

func (ScheduleRetentionAction) isWorkflowAction() {}

type CancelRetentionAction struct {
	RetentionToken string
}

func (CancelRetentionAction) isWorkflowAction() {}

type RecordPendingEventAction struct {
	EventID    string
	Kind       EventKind
	ObservedAt time.Time
}

func (RecordPendingEventAction) isWorkflowAction() {}

type ReconcilePendingEventsAction struct {
	SourceTurn            TurnGuard
	Count                 uint32
	LatestObservedHeadSHA string
	FallbackRole          Role
	FallbackPurpose       TurnPurpose
	FallbackExpectedHead  string
	RetryOfTurnID         string
}

func (ReconcilePendingEventsAction) isWorkflowAction() {}
