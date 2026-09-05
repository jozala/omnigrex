package workflow_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/workflow"
)

func TestDeveloperOutcomeWithChangeProposalEnqueuesReviewerIntent(t *testing.T) {
	snapshot := developingSnapshot(nil)
	event := settledEvent(snapshot, "developer-settled", workflow.TurnOutcomeChangeProposalReady)
	event.ChangeProposal = proposal(64, "head-1")

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonChangeProposalReady, workflow.StateReviewing, snapshot.Revision+1)
	if decision.Snapshot.ChangeProposal == nil || decision.Snapshot.ChangeProposal.ID != 64 || decision.Snapshot.ChangeProposal.HeadSHA != "head-1" {
		t.Fatalf("Change Proposal = %#v, want Pull Request 64 at head-1", decision.Snapshot.ChangeProposal)
	}
	if decision.Snapshot.ActiveTurn != nil {
		t.Errorf("active Agent Turn = %#v, want nil until Store allocation", decision.Snapshot.ActiveTurn)
	}
	action := onlyAction[workflow.EnqueueTurnAction](t, decision.Actions)
	if action.Role != workflow.RoleReviewer || action.Purpose != workflow.TurnPurposeReview || action.ExpectedHeadSHA != "head-1" {
		t.Errorf("enqueue intent = %#v, want Reviewer for head-1", action)
	}
}

func TestDeveloperCannotReplaceUnrelatedActiveChangeProposal(t *testing.T) {
	snapshot := developingSnapshot(proposal(64, "head-1"))
	event := settledEvent(snapshot, "developer-unrelated-pr", workflow.TurnOutcomeChangeProposalReady)
	event.ChangeProposal = proposal(65, "other-head")

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionUnrelated, workflow.ReasonChangeProposalUnrelated, workflow.StateDeveloping, snapshot.Revision)
	if decision.Snapshot.ChangeProposal.ID != 64 || decision.Snapshot.ChangeProposal.HeadSHA != "head-1" {
		t.Errorf("Change Proposal regressed to %#v", decision.Snapshot.ChangeProposal)
	}
}

func TestAcceptedChangesRequestReturnsToDeveloper(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-1")
	event := reviewEvent(snapshot, "review-changes", workflow.TurnOutcomeChangesRequested, review(501, 64, "head-1"), proposal(64, "head-1"))

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonChangesRequested, workflow.StateDeveloping, snapshot.Revision+1)
	if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 1 {
		t.Errorf("used Review Cycles = %d, want 1", decision.Snapshot.CurrentAttempt.ReviewBudget.Used)
	}
	action := onlyAction[workflow.EnqueueTurnAction](t, decision.Actions)
	if action.Role != workflow.RoleDeveloper || action.Purpose != workflow.TurnPurposeRequestedChanges || action.ExpectedHeadSHA != "head-1" {
		t.Errorf("enqueue intent = %#v, want returning Developer", action)
	}
}

func TestAcceptedApprovalSetsReadyForSHA(t *testing.T) {
	snapshot := reviewingSnapshot(1, "head-2")
	event := reviewEvent(snapshot, "review-approval", workflow.TurnOutcomeApproved, review(502, 64, "head-2"), proposal(64, "head-2"))

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonApproved, workflow.StatePRReady, snapshot.Revision+1)
	if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 2 || decision.Snapshot.ChangeProposal.ReadyForSHA != "head-2" {
		t.Errorf("approval state = (%d, %#v), want two Review Cycles and head-2 readiness", decision.Snapshot.CurrentAttempt.ReviewBudget.Used, decision.Snapshot.ChangeProposal)
	}
	assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
}

func TestThirdAcceptedChangesRequestCreatesHumanHandoff(t *testing.T) {
	snapshot := reviewingSnapshot(2, "head-3")
	event := reviewEvent(snapshot, "review-third", workflow.TurnOutcomeChangesRequested, review(503, 64, "head-3"), proposal(64, "head-3"))

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonReviewBudgetExhausted, workflow.StateNeedsHuman, snapshot.Revision+1)
	if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 3 || decision.Snapshot.ResumeRole != workflow.RoleDeveloper {
		t.Errorf("handoff state = (%d, %q), want three Review Cycles and Developer resume", decision.Snapshot.CurrentAttempt.ReviewBudget.Used, decision.Snapshot.ResumeRole)
	}
	assertActionCount[workflow.MarkHumanHandoffAction](t, decision.Actions, 1)
	assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
}

func TestStaleHeadReviewIsAppliedWithoutConsumingReviewBudget(t *testing.T) {
	for _, outcome := range []workflow.TurnOutcome{workflow.TurnOutcomeApproved, workflow.TurnOutcomeChangesRequested} {
		t.Run(string(outcome), func(t *testing.T) {
			snapshot := reviewingSnapshot(1, "head-old")
			event := reviewEvent(snapshot, "stale-"+string(outcome), outcome, review(600, 64, "head-old"), proposal(64, "head-new"))

			decision := workflow.Reduce(snapshot, event)

			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonReviewHeadReplaced, workflow.StateReviewing, snapshot.Revision+1)
			if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 1 || decision.Snapshot.ChangeProposal.HeadSHA != "head-new" || decision.Snapshot.ChangeProposal.ReadyForSHA != "" {
				t.Errorf("stale review result = budget %d, proposal %#v", decision.Snapshot.CurrentAttempt.ReviewBudget.Used, decision.Snapshot.ChangeProposal)
			}
			action := onlyAction[workflow.EnqueueTurnAction](t, decision.Actions)
			if action.Role != workflow.RoleReviewer || action.Purpose != workflow.TurnPurposeSynchronization || action.ExpectedHeadSHA != "head-new" {
				t.Errorf("enqueue intent = %#v, want synchronized Reviewer", action)
			}
		})
	}
}

func TestReviewDeduplicatesByDurableIDAndRejectsConflictingContent(t *testing.T) {
	snapshot := reviewingSnapshot(1, "head-1")
	identity := review(700, 64, "head-1")

	t.Run("same identity", func(t *testing.T) {
		event := reviewEvent(snapshot, "review-redelivery", workflow.TurnOutcomeApproved, identity, proposal(64, "head-1"))
		event.ExistingReview = identity
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionDuplicate, workflow.ReasonReviewDuplicate, snapshot.State, snapshot.Revision)
	})

	t.Run("same durable ID with conflicting content", func(t *testing.T) {
		event := reviewEvent(snapshot, "review-conflict", workflow.TurnOutcomeApproved, identity, proposal(64, "head-1"))
		event.ExistingReview = review(700, 64, "different-head")
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionIllegal, workflow.ReasonReviewIdentityConflict, snapshot.State, snapshot.Revision)
	})
}

func TestReviewWebhookIsPendingCorroborationNotTurnSettlement(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-1")
	event := workflow.ReviewObservedEvent{
		EventMetadata: metadata(snapshot, "review-webhook"),
		Review:        *review(750, 64, "head-1"),
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionDeferred, workflow.ReasonActiveTurn, workflow.StateReviewing, snapshot.Revision)
	action := onlyAction[workflow.RecordPendingEventAction](t, decision.Actions)
	if action.EventID != "review-webhook" || action.Kind != workflow.EventKindReviewObserved {
		t.Errorf("pending review action = %#v", action)
	}
	if decision.Snapshot.ActiveTurn == nil || decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 0 {
		t.Errorf("review webhook changed Workflow state: %#v", decision.Snapshot)
	}
}

func TestChangeProposalWebhookIsPendingCorroborationOnlyWithActiveTurn(t *testing.T) {
	for _, active := range []bool{true, false} {
		name := "queued"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			snapshot := developingSnapshot(nil)
			if !active {
				snapshot.ActiveTurn = nil
			}
			event := workflow.ChangeProposalObservedEvent{
				EventMetadata:  metadata(snapshot, "change-proposal-webhook-"+name),
				ChangeProposal: *proposal(64, "observed-head"),
			}

			decision := workflow.Reduce(snapshot, event)

			if active {
				assertDecision(t, decision, workflow.DispositionDeferred, workflow.ReasonActiveTurn, workflow.StateDeveloping, snapshot.Revision)
				action := onlyAction[workflow.RecordPendingEventAction](t, decision.Actions)
				if action.EventID != event.ID || action.Kind != workflow.EventKindChangeProposalObserved {
					t.Errorf("pending Change Proposal action = %#v", action)
				}
			} else {
				assertDecision(t, decision, workflow.DispositionUnrelated, workflow.ReasonCorroborationWithoutActiveTurn, workflow.StateDeveloping, snapshot.Revision)
				assertActionCount[workflow.RecordPendingEventAction](t, decision.Actions, 0)
			}
			turnChanged := (decision.Snapshot.ActiveTurn == nil) != (snapshot.ActiveTurn == nil)
			if decision.Snapshot.ActiveTurn != nil && snapshot.ActiveTurn != nil {
				turnChanged = *decision.Snapshot.ActiveTurn != *snapshot.ActiveTurn
			}
			if decision.Snapshot.ChangeProposal != nil || decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 0 || turnChanged {
				t.Errorf("Change Proposal webhook changed Workflow state: %#v", decision.Snapshot)
			}
		})
	}
}

func TestReviewWebhookWithoutActiveTurnIsUnrelated(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-1")
	snapshot.ActiveTurn = nil
	event := workflow.ReviewObservedEvent{
		EventMetadata: metadata(snapshot, "review-without-turn"),
		Review:        *review(751, 64, "head-1"),
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionUnrelated, workflow.ReasonCorroborationWithoutActiveTurn, snapshot.State, snapshot.Revision)
	assertActionCount[workflow.RecordPendingEventAction](t, decision.Actions, 0)
}

func TestReviewSettlementRequiresTrustedAuthorizedActorObservation(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-1")

	t.Run("missing trusted actor", func(t *testing.T) {
		event := reviewEvent(snapshot, "review-no-authorization", workflow.TurnOutcomeApproved, review(751, 64, "head-1"), proposal(64, "head-1"))
		event.AuthorizedReviewerActorID = 0
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionIllegal, workflow.ReasonInvalidEvent, snapshot.State, snapshot.Revision)
	})

	t.Run("actor mismatch", func(t *testing.T) {
		event := reviewEvent(snapshot, "review-wrong-actor", workflow.TurnOutcomeApproved, review(752, 64, "head-1"), proposal(64, "head-1"))
		event.AuthorizedReviewerActorID = 9999
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionUnrelated, workflow.ReasonReviewerActorUnrelated, snapshot.State, snapshot.Revision)
	})
}

func TestReviewIDDeduplicationSurvivesTerminalAttemptState(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-1")
	identity := review(701, 64, "head-1")
	applied := workflow.Reduce(snapshot, reviewEvent(snapshot, "review-first", workflow.TurnOutcomeApproved, identity, proposal(64, "head-1")))
	if applied.Snapshot.State != workflow.StatePRReady {
		t.Fatal("approval did not leave REVIEWING")
	}

	for _, test := range []struct {
		name     string
		existing *workflow.ReviewIdentity
		want     workflow.Disposition
		reason   workflow.Reason
	}{
		{name: "same identity", existing: identity, want: workflow.DispositionDuplicate, reason: workflow.ReasonReviewDuplicate},
		{name: "conflicting identity", existing: review(701, 64, "other-head"), want: workflow.DispositionIllegal, reason: workflow.ReasonReviewIdentityConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := reviewEvent(snapshot, "review-terminal-"+test.name, workflow.TurnOutcomeApproved, identity, proposal(64, "head-1"))
			event.EventMetadata = metadata(applied.Snapshot, "review-terminal-"+test.name)
			event.ExistingReview = test.existing
			decision := workflow.Reduce(applied.Snapshot, event)
			assertDecision(t, decision, test.want, test.reason, workflow.StatePRReady, applied.Snapshot.Revision)
		})
	}

	t.Run("recorded identity precedes stale CAS", func(t *testing.T) {
		event := reviewEvent(snapshot, "review-terminal-stale-cas", workflow.TurnOutcomeApproved, identity, proposal(64, "head-1"))
		event.EventMetadata = metadata(applied.Snapshot, "review-terminal-stale-cas")
		event.ExpectedRevision--
		event.ExistingReview = identity
		decision := workflow.Reduce(applied.Snapshot, event)
		assertDecision(t, decision, workflow.DispositionDuplicate, workflow.ReasonReviewDuplicate, workflow.StatePRReady, applied.Snapshot.Revision)
	})
}

func TestPublishedReviewEmitsDurableIdentityRecord(t *testing.T) {
	for _, test := range []struct {
		name         string
		observedHead string
		accepted     bool
	}{
		{name: "accepted", observedHead: "head-1", accepted: true},
		{name: "stale head", observedHead: "head-2", accepted: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := reviewingSnapshot(0, "head-1")
			identity := review(702, 64, "head-1")
			event := reviewEvent(snapshot, "record-review-"+test.name, workflow.TurnOutcomeApproved, identity, proposal(64, test.observedHead))
			decision := workflow.Reduce(snapshot, event)
			action := onlyAction[workflow.RecordReviewAction](t, decision.Actions)
			if action.Review.ID != 702 || action.Review.ChangeProposalID != 64 || action.Accepted != test.accepted {
				t.Errorf("record review action = %#v", action)
			}
		})
	}
}

func TestWebhookDuringActiveTurnDefersWithoutCopyingPendingState(t *testing.T) {
	snapshot := developingSnapshot(proposal(64, "head-old"))
	event := workflow.SynchronizationEvent{
		EventMetadata: metadata(snapshot, "sync-pending"), ChangeProposalID: 64, PreviousHeadSHA: "head-old", HeadSHA: "head-new",
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionDeferred, workflow.ReasonActiveTurn, snapshot.State, snapshot.Revision)
	action := onlyAction[workflow.RecordPendingEventAction](t, decision.Actions)
	if action.EventID != "sync-pending" || action.Kind != workflow.EventKindSynchronization || !action.ObservedAt.Equal(observedAt) {
		t.Errorf("record pending action = %#v", action)
	}
	if decision.Snapshot.ChangeProposal.HeadSHA != "head-old" || decision.Snapshot.ActiveTurn == nil {
		t.Errorf("deferred event mutated Workflow snapshot: %#v", decision.Snapshot)
	}
}

func TestTurnSettlementPreservesDifferingPendingHeadForAuthoritativeReconciliation(t *testing.T) {
	snapshot := developingSnapshot(nil)
	event := settledEvent(snapshot, "developer-with-pending", workflow.TurnOutcomeChangeProposalReady)
	event.ChangeProposal = proposal(64, "head-from-turn")
	event.PendingEvents = workflow.PendingEventsObservation{
		Count: 1, RequiresReconciliation: true, LatestObservedHeadSHA: "head-from-webhook",
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonChangeProposalReady, workflow.StateReviewing, snapshot.Revision+1)
	action := onlyAction[workflow.ReconcilePendingEventsAction](t, decision.Actions)
	if action.SourceTurn != event.Turn {
		t.Errorf("source turn = %#v, want %#v", action.SourceTurn, event.Turn)
	}
	if action.Count != 1 || action.LatestObservedHeadSHA != "head-from-webhook" || action.FallbackRole != workflow.RoleReviewer || action.FallbackExpectedHead != "head-from-turn" {
		t.Errorf("reconciliation action = %#v, want both observed heads and Reviewer fallback", action)
	}
	assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
}

func TestTurnSettlementReconcilesSameHeadCorroboration(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-1")
	event := reviewEvent(snapshot, "same-head-pending", workflow.TurnOutcomeChangesRequested, review(711, 64, "head-1"), proposal(64, "head-1"))
	event.PendingEvents = workflow.PendingEventsObservation{Count: 1, LatestObservedHeadSHA: "head-1"}

	decision := workflow.Reduce(snapshot, event)

	assertActionCount[workflow.ReconcilePendingEventsAction](t, decision.Actions, 1)
	assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
}

func TestReviewOutcomesSuppressSuccessorWhenPendingEventsNeedReconciliation(t *testing.T) {
	for _, outcome := range []workflow.TurnOutcome{workflow.TurnOutcomeApproved, workflow.TurnOutcomeChangesRequested} {
		t.Run(string(outcome), func(t *testing.T) {
			snapshot := reviewingSnapshot(0, "head-1")
			event := reviewEvent(snapshot, "review-pending-"+string(outcome), outcome, review(710, 64, "head-1"), proposal(64, "head-1"))
			event.PendingEvents = workflow.PendingEventsObservation{Count: 2, RequiresReconciliation: true, LatestObservedHeadSHA: "head-2"}

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

func TestPendingSynchronizedHeadMakesReviewStaleWithoutConsumingBudget(t *testing.T) {
	for _, outcome := range []workflow.TurnOutcome{workflow.TurnOutcomeApproved, workflow.TurnOutcomeChangesRequested} {
		t.Run(string(outcome), func(t *testing.T) {
			snapshot := reviewingSnapshot(1, "head-reviewed")
			event := reviewEvent(snapshot, "review-raced-"+string(outcome), outcome,
				review(712, 64, "head-reviewed"), proposal(64, "head-reviewed"))
			event.PendingEvents = workflow.PendingEventsObservation{
				Count: 2, RequiresReconciliation: true, LatestObservedHeadSHA: "head-pushed",
			}

			decision := workflow.Reduce(snapshot, event)

			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonReviewHeadReplaced, workflow.StateReviewing, snapshot.Revision+1)
			if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 1 || decision.Snapshot.ChangeProposal.ReadyForSHA != "" {
				t.Errorf("raced review result = budget %d, proposal %#v", decision.Snapshot.CurrentAttempt.ReviewBudget.Used, decision.Snapshot.ChangeProposal)
			}
			recorded := onlyAction[workflow.RecordReviewAction](t, decision.Actions)
			if recorded.Accepted {
				t.Errorf("raced review was accepted: %#v", recorded)
			}
			reconciliation := onlyAction[workflow.ReconcilePendingEventsAction](t, decision.Actions)
			if reconciliation.LatestObservedHeadSHA != "head-pushed" || reconciliation.FallbackRole != workflow.RoleReviewer ||
				reconciliation.FallbackPurpose != workflow.TurnPurposeSynchronization || reconciliation.FallbackExpectedHead != "head-reviewed" {
				t.Errorf("raced review reconciliation = %#v", reconciliation)
			}
			assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
		})
	}
}

func TestSameHeadPendingSynchronizationDoesNotMakeReviewStale(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-reviewed")
	event := reviewEvent(snapshot, "review-same-pending-head", workflow.TurnOutcomeApproved,
		review(713, 64, "head-reviewed"), proposal(64, "head-reviewed"))
	event.PendingEvents = workflow.PendingEventsObservation{
		Count: 1, RequiresReconciliation: true, LatestObservedHeadSHA: "head-reviewed",
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonApproved, workflow.StatePRReady, snapshot.Revision+1)
	if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 1 || decision.Snapshot.ChangeProposal.ReadyForSHA != "head-reviewed" {
		t.Errorf("same-head review result = budget %d, proposal %#v", decision.Snapshot.CurrentAttempt.ReviewBudget.Used, decision.Snapshot.ChangeProposal)
	}
	if recorded := onlyAction[workflow.RecordReviewAction](t, decision.Actions); !recorded.Accepted {
		t.Errorf("same-head review was not accepted: %#v", recorded)
	}
	assertActionCount[workflow.ReconcilePendingEventsAction](t, decision.Actions, 1)
}

func TestAgentBlockerCreatesHumanHandoff(t *testing.T) {
	snapshot := developingSnapshot(nil)
	event := settledEvent(snapshot, "blocked", workflow.TurnOutcomeBlocked)
	event.Diagnostic = "repository policy blocks the change"

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonAgentBlocked, workflow.StateNeedsHuman, snapshot.Revision+1)
	if decision.Snapshot.ResumeRole != workflow.RoleDeveloper || decision.Snapshot.CurrentAttempt.Lifecycle != workflow.AttemptActive {
		t.Errorf("handoff state = %#v", decision.Snapshot)
	}
	assertActionCount[workflow.MarkHumanHandoffAction](t, decision.Actions, 1)
}

func TestAssignmentConfigurationConflictCreatesHumanHandoff(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot workflow.Snapshot
		role     workflow.Role
	}{
		{name: "Developer", snapshot: developingSnapshot(nil), role: workflow.RoleDeveloper},
		{name: "Reviewer", snapshot: reviewingSnapshot(1, "review-head"), role: workflow.RoleReviewer},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.snapshot.ActiveTurn = nil
			event := workflow.AssignmentConfigurationConflictEvent{
				EventMetadata: metadata(test.snapshot, "assignment-configuration-conflict"),
				Role:          test.role,
			}

			decision := workflow.Reduce(test.snapshot, event)

			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonAssignmentConfigurationConflict, workflow.StateNeedsHuman, test.snapshot.Revision+1)
			if decision.Snapshot.ResumeRole != test.role || decision.Snapshot.Assignments.Status != workflow.AssignmentWaitingForHuman {
				t.Errorf("configuration handoff state = %#v", decision.Snapshot)
			}
			handoff := onlyAction[workflow.MarkHumanHandoffAction](t, decision.Actions)
			if handoff.Reason != workflow.ReasonAssignmentConfigurationConflict || handoff.Diagnostic != "" {
				t.Errorf("configuration handoff action = %#v", handoff)
			}
			labels := onlyAction[workflow.ReconcileLabelsAction](t, decision.Actions)
			if labels.State != workflow.StateNeedsHuman {
				t.Errorf("label reconciliation = %#v", labels)
			}
			assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
		})
	}
}

func TestAgentTurnPreparationFailureCreatesHumanHandoffWithoutFabricatingTurn(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot workflow.Snapshot
		role     workflow.Role
	}{
		{name: "Developer", snapshot: developingSnapshot(nil), role: workflow.RoleDeveloper},
		{name: "Reviewer", snapshot: reviewingSnapshot(1, "review-head"), role: workflow.RoleReviewer},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.snapshot.ActiveTurn = nil
			event := workflow.AgentTurnPreparationFailedEvent{
				EventMetadata:    metadata(test.snapshot, "agent-turn-preparation-failed"),
				Role:             test.role,
				Diagnostic:       "GitHub App installation is missing",
				AssignmentsExist: true,
			}

			decision := workflow.Reduce(test.snapshot, event)

			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonAgentTurnPreparationFailed, workflow.StateNeedsHuman, test.snapshot.Revision+1)
			if decision.Snapshot.ActiveTurn != nil || decision.Snapshot.ResumeRole != test.role ||
				decision.Snapshot.Assignments.Status != workflow.AssignmentWaitingForHuman ||
				decision.Snapshot.CurrentAttempt.ID != test.snapshot.CurrentAttempt.ID {
				t.Errorf("preparation failure handoff state = %#v", decision.Snapshot)
			}
			handoff := onlyAction[workflow.MarkHumanHandoffAction](t, decision.Actions)
			if handoff.Reason != workflow.ReasonAgentTurnPreparationFailed || handoff.Diagnostic != event.Diagnostic {
				t.Errorf("preparation failure handoff = %#v", handoff)
			}
			if labels := onlyAction[workflow.ReconcileLabelsAction](t, decision.Actions); labels.State != workflow.StateNeedsHuman {
				t.Errorf("label reconciliation = %#v", labels)
			}
			assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
		})
	}
}

func TestAgentTurnMutationReconciliationExhaustionCreatesHumanHandoff(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot workflow.Snapshot
		role     workflow.Role
	}{
		{name: "Developer", snapshot: developingSnapshot(nil), role: workflow.RoleDeveloper},
		{name: "Reviewer", snapshot: reviewingSnapshot(1, "review-head"), role: workflow.RoleReviewer},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.snapshot.ActiveTurn = nil
			event := workflow.AgentTurnMutationReconciliationExhaustedEvent{
				EventMetadata: metadata(test.snapshot, "agent-turn-mutation-reconciliation-exhausted"),
				Role:          test.role,
				Diagnostic:    "outcome unknowable; escalated: GitHub reconciliation remained inconclusive",
			}

			decision := workflow.Reduce(test.snapshot, event)

			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonAgentTurnMutationReconciliationExhausted, workflow.StateNeedsHuman, test.snapshot.Revision+1)
			if decision.Snapshot.ActiveTurn != nil || decision.Snapshot.ResumeRole != test.role ||
				decision.Snapshot.Assignments.Status != workflow.AssignmentWaitingForHuman ||
				decision.Snapshot.CurrentAttempt.ID != test.snapshot.CurrentAttempt.ID {
				t.Errorf("mutation reconciliation handoff state = %#v", decision.Snapshot)
			}
			handoff := onlyAction[workflow.MarkHumanHandoffAction](t, decision.Actions)
			if handoff.Reason != workflow.ReasonAgentTurnMutationReconciliationExhausted || handoff.Diagnostic != event.Diagnostic {
				t.Errorf("mutation reconciliation handoff = %#v", handoff)
			}
			if labels := onlyAction[workflow.ReconcileLabelsAction](t, decision.Actions); labels.State != workflow.StateNeedsHuman {
				t.Errorf("label reconciliation = %#v", labels)
			}
			assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
		})
	}
}

func TestWorkflowActionExhaustionCreatesHumanHandoffAndRetainsResumeRole(t *testing.T) {
	prReady := reviewingSnapshot(1, "ready-head")
	prReady.State = workflow.StatePRReady
	prReady.ActiveTurn = nil
	prReady.ChangeProposal.ReadyForSHA = "ready-head"
	tests := []struct {
		name       string
		snapshot   workflow.Snapshot
		resumeRole workflow.Role
		wantRole   workflow.Role
	}{
		{name: "pending fallback", snapshot: func() workflow.Snapshot {
			snapshot := developingSnapshot(nil)
			snapshot.ActiveTurn = nil
			return snapshot
		}(), resumeRole: workflow.RoleReviewer, wantRole: workflow.RoleReviewer},
		{name: "reviewing inference", snapshot: func() workflow.Snapshot {
			snapshot := reviewingSnapshot(1, "review-head")
			snapshot.ActiveTurn = nil
			return snapshot
		}(), wantRole: workflow.RoleReviewer},
		{name: "PR ready inference", snapshot: prReady, wantRole: workflow.RoleReviewer},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := workflow.Reduce(test.snapshot, workflow.WorkflowActionExhaustedEvent{
				EventMetadata: metadata(test.snapshot, "workflow-action-exhausted"),
				ResumeRole:    test.resumeRole,
				Diagnostic:    "durable action reached its terminal attempt",
			})

			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonWorkflowActionExhausted, workflow.StateNeedsHuman, test.snapshot.Revision+1)
			if decision.Snapshot.ActiveTurn != nil || decision.Snapshot.ResumeRole != test.wantRole ||
				decision.Snapshot.Assignments.Status != workflow.AssignmentWaitingForHuman {
				t.Errorf("action exhaustion handoff state = %#v", decision.Snapshot)
			}
			if decision.Snapshot.ChangeProposal != nil && decision.Snapshot.ChangeProposal.ReadyForSHA != "" {
				t.Errorf("action exhaustion retained readiness = %#v", decision.Snapshot.ChangeProposal)
			}
			assertActionCount[workflow.InterruptTurnForHumanHandoffAction](t, decision.Actions, 0)
			handoff := onlyAction[workflow.MarkHumanHandoffAction](t, decision.Actions)
			if handoff.Reason != workflow.ReasonWorkflowActionExhausted || handoff.Diagnostic == "" {
				t.Errorf("Human Handoff action = %#v", handoff)
			}
			assertActionCount[workflow.ReconcileLabelsAction](t, decision.Actions, 1)
		})
	}
}

func TestWorkflowActionExhaustionDefersWithoutInterruptingActiveTurn(t *testing.T) {
	snapshot := developingSnapshot(nil)
	decision := workflow.Reduce(snapshot, workflow.WorkflowActionExhaustedEvent{
		EventMetadata: metadata(snapshot, "workflow-action-exhausted-during-turn"),
		Diagnostic:    "unrelated GitHub acknowledgement exhausted",
	})

	assertDecision(t, decision, workflow.DispositionDeferred, workflow.ReasonActiveTurn, snapshot.State, snapshot.Revision)
	if len(decision.Actions) != 0 || !reflect.DeepEqual(decision.Snapshot.ActiveTurn, snapshot.ActiveTurn) {
		t.Errorf("deferred exhaustion altered active turn: %#v", decision)
	}
}

func TestWorkflowActionExhaustionDoesNotRecurseInHumanHandoff(t *testing.T) {
	snapshot := developingSnapshot(nil)
	snapshot.ActiveTurn = nil
	first := workflow.Reduce(snapshot, workflow.WorkflowActionExhaustedEvent{
		EventMetadata: metadata(snapshot, "first-workflow-action-exhausted"),
		Diagnostic:    "GitHub installation is missing",
	})
	second := workflow.Reduce(first.Snapshot, workflow.WorkflowActionExhaustedEvent{
		EventMetadata: metadata(first.Snapshot, "second-workflow-action-exhausted"),
		Diagnostic:    "Human Handoff publication is also forbidden",
	})

	assertDecision(t, second, workflow.DispositionDuplicate, workflow.ReasonWorkflowActionExhausted, workflow.StateNeedsHuman, first.Snapshot.Revision)
	if len(second.Actions) != 0 {
		t.Errorf("recursive exhaustion actions = %#v, want none", second.Actions)
	}
}

func TestIssueCanCloseAfterInitialPreparationFailureWithoutAssignments(t *testing.T) {
	snapshot := developingSnapshot(nil)
	snapshot.ActiveTurn = nil
	failure := workflow.Reduce(snapshot, workflow.AgentTurnPreparationFailedEvent{
		EventMetadata: metadata(snapshot, "initial-preparation-failed"),
		Role:          workflow.RoleDeveloper,
		Diagnostic:    "Reviewer GitHub App is not installed",
	})
	if failure.Disposition != workflow.DispositionApplied || failure.Snapshot.Assignments.RuntimeState != workflow.RuntimeStateCollected {
		t.Fatalf("preparation failure = %#v, want collected Human Handoff", failure)
	}

	closed := workflow.Reduce(failure.Snapshot, workflow.IssueClosedEvent{
		EventMetadata:  metadata(failure.Snapshot, "close-after-preparation-failure"),
		ClosureID:      "closure-after-preparation-failure",
		RetainUntil:    observedAt.Add(24 * time.Hour),
		RetentionToken: "retention-after-preparation-failure",
	})
	if closed.Disposition != workflow.DispositionApplied || closed.Snapshot.State != workflow.StateClosing {
		t.Fatalf("Issue close = %#v, want applied closing transition", closed)
	}
}

func TestInfrastructureFailureRetriesOnceWithoutReviewBudget(t *testing.T) {
	snapshot := reviewingSnapshot(2, "head-1")
	event := settledEvent(snapshot, "infrastructure-first", workflow.TurnOutcomeInfrastructureFailed)
	event.Diagnostic = "runtime exited"

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonInfrastructureRetry, workflow.StateReviewing, snapshot.Revision+1)
	if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 2 || decision.Snapshot.CurrentAttempt.InfrastructureRetryBudget.Used != 1 {
		t.Errorf("budgets = %#v", decision.Snapshot.CurrentAttempt)
	}
	action := onlyAction[workflow.EnqueueTurnAction](t, decision.Actions)
	if action.Role != workflow.RoleReviewer || action.Purpose != workflow.TurnPurposeRetry || action.RetryOfTurnID != "turn-active" || action.ExpectedHeadSHA != "head-1" {
		t.Errorf("retry intent = %#v", action)
	}
}

func TestSecondInfrastructureFailureCreatesHumanHandoff(t *testing.T) {
	snapshot := developingSnapshot(nil)
	snapshot.CurrentAttempt.InfrastructureRetryBudget.Used = 1
	event := settledEvent(snapshot, "infrastructure-second", workflow.TurnOutcomeInfrastructureFailed)

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonInfrastructureRetriesExhausted, workflow.StateNeedsHuman, snapshot.Revision+1)
	if decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 0 || decision.Snapshot.ResumeRole != workflow.RoleDeveloper {
		t.Errorf("terminal infrastructure state = %#v", decision.Snapshot)
	}
	assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
}

func TestRetriggerCompletesPriorAttemptBeforeCreatingFreshAttempt(t *testing.T) {
	snapshot := baseSnapshot(workflow.StateNeedsHuman, 3)
	snapshot.Assignments.Status = workflow.AssignmentWaitingForHuman
	snapshot.ResumeRole = workflow.RoleDeveloper
	snapshot.ChangeProposal = proposal(64, "existing-head")
	event := workflow.TriggerEvent{
		EventMetadata: metadata(snapshot, "retrigger"), AttemptID: "attempt-2", AttemptNumber: 2,
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonTriggered, workflow.StateDeveloping, snapshot.Revision+1)
	if decision.Snapshot.CurrentAttempt.ID != "attempt-2" || decision.Snapshot.CurrentAttempt.ReviewBudget.Used != 0 || decision.Snapshot.CurrentAttempt.InfrastructureRetryBudget.Used != 0 || decision.Snapshot.ActiveTurn != nil {
		t.Errorf("new Workflow Attempt = %#v", decision.Snapshot.CurrentAttempt)
	}
	if decision.Snapshot.Assignments.Status != workflow.AssignmentActive || decision.Snapshot.ChangeProposal.HeadSHA != "existing-head" {
		t.Errorf("reused state = %#v", decision.Snapshot)
	}
	if ensure := onlyAction[workflow.EnsureAssignmentsAction](t, decision.Actions); ensure.Mode != workflow.AssignmentGenerationCurrent {
		t.Errorf("Assignment intent = %#v, want current generation", ensure)
	}
	completeIndex := actionIndex[workflow.CompleteAttemptAction](decision.Actions)
	createIndex := actionIndex[workflow.CreateAttemptAction](decision.Actions)
	if completeIndex < 0 || createIndex < 0 || completeIndex >= createIndex {
		t.Errorf("actions = %#v, want prior completion before attempt creation", decision.Actions)
	}
}

func TestRetriggerCannotReusePriorAttemptIdentityOrSequence(t *testing.T) {
	snapshot := baseSnapshot(workflow.StateNeedsHuman, 3)
	snapshot.Assignments.Status = workflow.AssignmentWaitingForHuman
	snapshot.ResumeRole = workflow.RoleDeveloper
	for _, event := range []workflow.TriggerEvent{
		{EventMetadata: metadata(snapshot, "same-attempt-id"), AttemptID: "attempt-1", AttemptNumber: 2},
		{EventMetadata: metadata(snapshot, "same-attempt-number"), AttemptID: "attempt-2", AttemptNumber: 1},
	} {
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionIllegal, workflow.ReasonInvalidEvent, snapshot.State, snapshot.Revision)
	}
}

func TestPRReadyAttemptIsSupersededAtomicallyOnRetrigger(t *testing.T) {
	snapshot := baseSnapshot(workflow.StatePRReady, 1)
	snapshot.ChangeProposal = proposal(64, "ready-head")
	snapshot.ChangeProposal.ReadyForSHA = "ready-head"
	event := workflow.TriggerEvent{
		EventMetadata: metadata(snapshot, "retrigger-ready"), AttemptID: "attempt-2", AttemptNumber: 2,
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonTriggered, workflow.StateDeveloping, snapshot.Revision+1)
	if actionIndex[workflow.CompleteAttemptAction](decision.Actions) >= actionIndex[workflow.CreateAttemptAction](decision.Actions) {
		t.Errorf("actions = %#v, want supersession before creation", decision.Actions)
	}
	if decision.Snapshot.ChangeProposal.ReadyForSHA != "" || decision.Snapshot.CurrentAttempt.ID != "attempt-2" {
		t.Errorf("retrigger state = %#v", decision.Snapshot)
	}
}

func TestSynchronizationAfterThirdReviewClearsReadinessAndCreatesHumanHandoff(t *testing.T) {
	snapshot := baseSnapshot(workflow.StatePRReady, 3)
	snapshot.ChangeProposal = proposal(64, "head-old")
	snapshot.ChangeProposal.ReadyForSHA = "head-old"
	event := workflow.SynchronizationEvent{
		EventMetadata: metadata(snapshot, "sync-after-third"), ChangeProposalID: 64, PreviousHeadSHA: "head-old", HeadSHA: "head-new",
	}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonReviewBudgetExhausted, workflow.StateNeedsHuman, snapshot.Revision+1)
	if decision.Snapshot.ChangeProposal.HeadSHA != "head-new" || decision.Snapshot.ChangeProposal.ReadyForSHA != "" || decision.Snapshot.ResumeRole != workflow.RoleReviewer {
		t.Errorf("synchronized handoff = %#v", decision.Snapshot)
	}
	assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
}

func TestSynchronizationGuardsPreviousAndAuthoritativeHeads(t *testing.T) {
	t.Run("valid chain", func(t *testing.T) {
		snapshot := reviewingSnapshot(1, "head-1")
		snapshot.ActiveTurn = nil
		event := workflow.SynchronizationEvent{EventMetadata: metadata(snapshot, "sync-valid"), ChangeProposalID: 64, PreviousHeadSHA: "head-1", HeadSHA: "head-2"}
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonReviewHeadReplaced, workflow.StateReviewing, snapshot.Revision+1)
		if decision.Snapshot.ChangeProposal.HeadSHA != "head-2" {
			t.Errorf("head = %q, want head-2", decision.Snapshot.ChangeProposal.HeadSHA)
		}
	})

	t.Run("stale chain cannot regress", func(t *testing.T) {
		snapshot := reviewingSnapshot(1, "head-2")
		snapshot.ActiveTurn = nil
		event := workflow.SynchronizationEvent{EventMetadata: metadata(snapshot, "sync-stale"), ChangeProposalID: 64, PreviousHeadSHA: "head-0", HeadSHA: "head-1"}
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionStale, workflow.ReasonSynchronizationStale, snapshot.State, snapshot.Revision)
		if decision.Snapshot.ChangeProposal.HeadSHA != "head-2" {
			t.Errorf("head regressed to %q", decision.Snapshot.ChangeProposal.HeadSHA)
		}
	})

	t.Run("same-head redelivery is duplicate despite old previous head", func(t *testing.T) {
		snapshot := reviewingSnapshot(1, "head-2")
		snapshot.ActiveTurn = nil
		event := workflow.SynchronizationEvent{EventMetadata: metadata(snapshot, "sync-duplicate"), ChangeProposalID: 64, PreviousHeadSHA: "head-1", HeadSHA: "head-2"}
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionDuplicate, workflow.ReasonSynchronizationDuplicate, snapshot.State, snapshot.Revision)
	})
}

func TestRevisionMismatchDoesNotMakeEventNonRetryable(t *testing.T) {
	snapshot := reviewingSnapshot(0, "head-1")
	snapshot.ActiveTurn = nil
	event := workflow.SynchronizationEvent{EventMetadata: metadata(snapshot, "sync-cas"), ChangeProposalID: 64, PreviousHeadSHA: "head-1", HeadSHA: "head-2"}
	event.ExpectedRevision--

	stale := workflow.Reduce(snapshot, event)
	assertDecision(t, stale, workflow.DispositionStale, workflow.ReasonStateRevisionStale, snapshot.State, snapshot.Revision)

	event.ExpectedRevision = snapshot.Revision
	retried := workflow.Reduce(stale.Snapshot, event)
	assertDecision(t, retried, workflow.DispositionApplied, workflow.ReasonReviewHeadReplaced, workflow.StateReviewing, snapshot.Revision+1)
}

func TestIssueClosureFencesTurnAndSettlementSchedulesSuppliedRetentionGeneration(t *testing.T) {
	snapshot := developingSnapshot(nil)
	closeEvent := closeEvent(snapshot, "closure-1", "retention-token-1")

	closing := workflow.Reduce(snapshot, closeEvent)

	assertDecision(t, closing, workflow.DispositionApplied, workflow.ReasonClosureStarted, workflow.StateClosing, snapshot.Revision+1)
	if closing.Snapshot.Closure == nil || closing.Snapshot.Closure.RetentionToken != "retention-token-1" || !closing.Snapshot.Closure.RetainUntil.Equal(closeEvent.RetainUntil) {
		t.Fatalf("closure = %#v, want event-supplied retention generation", closing.Snapshot.Closure)
	}
	closeAdmission := onlyAction[workflow.CloseMutationAdmissionAction](t, closing.Actions)
	if closeAdmission.Turn.ControlRevision != 9 || closeAdmission.Turn.SessionID != "session-active" {
		t.Errorf("close admission guard = %#v", closeAdmission.Turn)
	}
	assertActionCount[workflow.StopTurnAction](t, closing.Actions, 1)
	assertActionCount[workflow.SettleClosureAction](t, closing.Actions, 1)

	settledEvent := workflow.ClosureSettledEvent{EventMetadata: metadata(closing.Snapshot, "closure-settled"), ClosureID: "closure-1", Turn: turnGuardPointer(closing.Snapshot)}
	settled := workflow.Reduce(closing.Snapshot, settledEvent)

	assertDecision(t, settled, workflow.DispositionApplied, workflow.ReasonClosureSettled, workflow.StateClosed, closing.Snapshot.Revision+1)
	if settled.Snapshot.CurrentAttempt != nil || settled.Snapshot.ActiveTurn != nil || settled.Snapshot.Closure != nil {
		t.Errorf("closed execution state = %#v", settled.Snapshot)
	}
	if settled.Snapshot.Assignments.RetentionToken != "retention-token-1" || !settled.Snapshot.Assignments.RetainedUntil.Equal(closeEvent.RetainUntil) {
		t.Errorf("retention state = %#v", settled.Snapshot.Assignments)
	}
	complete := onlyAction[workflow.CompleteAttemptAction](t, settled.Actions)
	if complete.AttemptID != "attempt-1" || complete.Reason != workflow.AttemptCompletionIssueClosed {
		t.Errorf("attempt completion = %#v", complete)
	}
	schedule := onlyAction[workflow.ScheduleRetentionAction](t, settled.Actions)
	if schedule.RetentionToken != "retention-token-1" || !schedule.RetainUntil.Equal(closeEvent.RetainUntil) {
		t.Errorf("retention action = %#v", schedule)
	}
}

func TestIssueClosureDerivesResumeRoleWithOrWithoutActiveTurn(t *testing.T) {
	needsHuman := baseSnapshot(workflow.StateNeedsHuman, 1)
	needsHuman.Assignments.Status = workflow.AssignmentWaitingForHuman
	needsHuman.ResumeRole = workflow.RoleDeveloper

	queuedDeveloper := developingSnapshot(nil)
	queuedDeveloper.ActiveTurn = nil
	queuedReviewer := reviewingSnapshot(1, "review-head")
	queuedReviewer.ActiveTurn = nil
	activeReviewer := reviewingSnapshot(1, "review-head")
	prReady := baseSnapshot(workflow.StatePRReady, 1)
	prReady.ChangeProposal = proposal(64, "ready-head")
	prReady.ChangeProposal.ReadyForSHA = "ready-head"

	for _, test := range []struct {
		name     string
		snapshot workflow.Snapshot
		want     workflow.Role
	}{
		{name: "queued Developer", snapshot: queuedDeveloper, want: workflow.RoleDeveloper},
		{name: "queued Reviewer", snapshot: queuedReviewer, want: workflow.RoleReviewer},
		{name: "active Reviewer", snapshot: activeReviewer, want: workflow.RoleReviewer},
		{name: "PR ready", snapshot: prReady, want: workflow.RoleReviewer},
		{name: "Human Handoff", snapshot: needsHuman, want: workflow.RoleDeveloper},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := workflow.Reduce(test.snapshot, closeEvent(test.snapshot, "resume-"+test.name, "retention-"+test.name))
			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonClosureStarted, workflow.StateClosing, test.snapshot.Revision+1)
			if decision.Snapshot.ResumeRole != test.want {
				t.Errorf("resume Role = %q, want %q", decision.Snapshot.ResumeRole, test.want)
			}
		})
	}
}

func TestQueuedReviewOrReadyClosureReopenRetriggerResumesReviewer(t *testing.T) {
	queuedReview := reviewingSnapshot(1, "queued-head")
	queuedReview.ActiveTurn = nil
	prReady := baseSnapshot(workflow.StatePRReady, 1)
	prReady.ChangeProposal = proposal(64, "ready-head")
	prReady.ChangeProposal.ReadyForSHA = "ready-head"

	for _, test := range []struct {
		name     string
		snapshot workflow.Snapshot
		head     string
	}{
		{name: "queued review", snapshot: queuedReview, head: "queued-head"},
		{name: "PR ready", snapshot: prReady, head: "ready-head"},
	} {
		t.Run(test.name, func(t *testing.T) {
			closing := workflow.Reduce(test.snapshot, closeEvent(test.snapshot, "closure-"+test.name, "retention-"+test.name))
			settled := workflow.Reduce(closing.Snapshot, workflow.ClosureSettledEvent{
				EventMetadata: metadata(closing.Snapshot, "settle-"+test.name), ClosureID: "closure-" + test.name, Turn: turnGuardPointer(closing.Snapshot),
			})
			assertDecision(t, settled, workflow.DispositionApplied, workflow.ReasonClosureSettled, workflow.StateClosed, closing.Snapshot.Revision+1)
			if settled.Snapshot.ResumeRole != workflow.RoleReviewer {
				t.Fatalf("settled resume Role = %q, want REVIEWER", settled.Snapshot.ResumeRole)
			}

			reopened := workflow.Reduce(settled.Snapshot, workflow.IssueReopenedEvent{EventMetadata: metadata(settled.Snapshot, "reopen-"+test.name)})
			assertDecision(t, reopened, workflow.DispositionApplied, workflow.ReasonIssueReopened, workflow.StateDormant, settled.Snapshot.Revision+1)
			if reopened.Snapshot.ResumeRole != workflow.RoleReviewer {
				t.Fatalf("reopened resume Role = %q, want REVIEWER", reopened.Snapshot.ResumeRole)
			}

			trigger := workflow.TriggerEvent{EventMetadata: metadata(reopened.Snapshot, "trigger-"+test.name), AttemptID: "attempt-2", AttemptNumber: 2}
			decision := workflow.Reduce(reopened.Snapshot, trigger)
			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonTriggered, workflow.StateReviewing, reopened.Snapshot.Revision+1)
			intent := onlyAction[workflow.EnqueueTurnAction](t, decision.Actions)
			if intent.Role != workflow.RoleReviewer || intent.Purpose != workflow.TurnPurposeReactivation || intent.ExpectedHeadSHA != test.head {
				t.Errorf("resume intent = %#v, want Reviewer for %s", intent, test.head)
			}
		})
	}
}

func TestReopenWhileClosingCancelsClosureAndSettlesDormantWithoutRetention(t *testing.T) {
	snapshot := reviewingSnapshot(1, "head-1")
	closing := workflow.Reduce(snapshot, closeEvent(snapshot, "closure-reopen", "retention-reopen"))
	reopenEvent := workflow.IssueReopenedEvent{EventMetadata: metadata(closing.Snapshot, "reopen-during-closing")}

	cancelled := workflow.Reduce(closing.Snapshot, reopenEvent)

	assertDecision(t, cancelled, workflow.DispositionApplied, workflow.ReasonClosureCancellationRecorded, workflow.StateClosing, closing.Snapshot.Revision+1)
	if cancelled.Snapshot.Closure == nil || !cancelled.Snapshot.Closure.ReopenRequested {
		t.Fatalf("closure = %#v, want reopen cancellation", cancelled.Snapshot.Closure)
	}
	cancel := onlyAction[workflow.CancelRetentionAction](t, cancelled.Actions)
	if cancel.RetentionToken != "retention-reopen" {
		t.Errorf("cancel retention = %#v", cancel)
	}

	settled := workflow.Reduce(cancelled.Snapshot, workflow.ClosureSettledEvent{
		EventMetadata: metadata(cancelled.Snapshot, "settled-after-reopen"), ClosureID: "closure-reopen", Turn: turnGuardPointer(cancelled.Snapshot),
	})

	assertDecision(t, settled, workflow.DispositionApplied, workflow.ReasonClosureSettled, workflow.StateDormant, cancelled.Snapshot.Revision+1)
	if settled.Snapshot.Assignments.RuntimeState != workflow.RuntimeStateRetained || !settled.Snapshot.Assignments.RetainedUntil.IsZero() || settled.Snapshot.Assignments.RetentionToken != "" {
		t.Errorf("cancelled retention state = %#v", settled.Snapshot.Assignments)
	}
	assertActionCount[workflow.ScheduleRetentionAction](t, settled.Actions, 0)
	assertActionCount[workflow.EnqueueTurnAction](t, settled.Actions, 0)
}

func TestReopenClosedCancelsRetentionWithoutStartingAutomation(t *testing.T) {
	snapshot := closedSnapshot("retention-closed")
	event := workflow.IssueReopenedEvent{EventMetadata: metadata(snapshot, "reopen-closed")}

	decision := workflow.Reduce(snapshot, event)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonIssueReopened, workflow.StateDormant, snapshot.Revision+1)
	if decision.Snapshot.Assignments.RetentionToken != "" || !decision.Snapshot.Assignments.RetainedUntil.IsZero() {
		t.Errorf("Assignments = %#v, want cancelled retention", decision.Snapshot.Assignments)
	}
	assertActionCount[workflow.CancelRetentionAction](t, decision.Actions, 1)
	assertActionCount[workflow.EnqueueTurnAction](t, decision.Actions, 0)
}

func TestTriggerAfterReviewClosureResumesReviewerAssignment(t *testing.T) {
	snapshot := reviewingSnapshot(1, "review-head")
	closing := workflow.Reduce(snapshot, closeEvent(snapshot, "closure-review", "retention-review"))
	settled := workflow.Reduce(closing.Snapshot, workflow.ClosureSettledEvent{
		EventMetadata: metadata(closing.Snapshot, "settle-review"), ClosureID: "closure-review", Turn: turnGuardPointer(closing.Snapshot),
	})
	reopened := workflow.Reduce(settled.Snapshot, workflow.IssueReopenedEvent{EventMetadata: metadata(settled.Snapshot, "reopen-review")})
	trigger := workflow.TriggerEvent{
		EventMetadata: metadata(reopened.Snapshot, "trigger-review"), AttemptID: "attempt-2", AttemptNumber: 2,
	}

	decision := workflow.Reduce(reopened.Snapshot, trigger)

	assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonTriggered, workflow.StateReviewing, reopened.Snapshot.Revision+1)
	intent := onlyAction[workflow.EnqueueTurnAction](t, decision.Actions)
	if intent.Role != workflow.RoleReviewer || intent.Purpose != workflow.TurnPurposeReactivation || intent.ExpectedHeadSHA != "review-head" {
		t.Errorf("resume intent = %#v", intent)
	}
}

func TestAssignmentsCollectionRequiresMatchingRetentionGenerationAndDeadline(t *testing.T) {
	snapshot := closedSnapshot("retention-live")

	t.Run("matching generation after deadline", func(t *testing.T) {
		event := collectionEvent(snapshot, "retention-live", snapshot.Assignments.RetainedUntil, snapshot.Assignments.RetainedUntil.Add(time.Minute))
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonAssignmentsCollected, workflow.StateClosed, snapshot.Revision+1)
		if decision.Snapshot.Assignments.RuntimeState != workflow.RuntimeStateCollected || decision.Snapshot.Assignments.RetentionToken != "" {
			t.Errorf("collected Assignments = %#v", decision.Snapshot.Assignments)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		event := collectionEvent(snapshot, "retention-old", snapshot.Assignments.RetainedUntil, snapshot.Assignments.RetainedUntil.Add(time.Minute))
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionStale, workflow.ReasonRetentionGenerationStale, workflow.StateClosed, snapshot.Revision)
	})

	t.Run("before deadline", func(t *testing.T) {
		event := collectionEvent(snapshot, "retention-live", snapshot.Assignments.RetainedUntil, snapshot.Assignments.RetainedUntil.Add(-time.Minute))
		decision := workflow.Reduce(snapshot, event)
		assertDecision(t, decision, workflow.DispositionIllegal, workflow.ReasonRetentionNotDue, workflow.StateClosed, snapshot.Revision)
	})
}

func TestCollectionAfterReopenCancellationIsStale(t *testing.T) {
	closed := closedSnapshot("retention-cancelled")
	deadline := closed.Assignments.RetainedUntil
	reopened := workflow.Reduce(closed, workflow.IssueReopenedEvent{EventMetadata: metadata(closed, "reopen-before-gc")})
	event := collectionEvent(reopened.Snapshot, "retention-cancelled", deadline, deadline.Add(time.Minute))

	decision := workflow.Reduce(reopened.Snapshot, event)

	assertDecision(t, decision, workflow.DispositionStale, workflow.ReasonRetentionCancelled, workflow.StateDormant, reopened.Snapshot.Revision)
}

func TestTriggerAfterReopenReactivatesRetainedOrCreatesCollectedAssignments(t *testing.T) {
	for _, test := range []struct {
		name         string
		runtimeState workflow.RuntimeState
		wantMode     workflow.AssignmentGeneration
	}{
		{name: "retained", runtimeState: workflow.RuntimeStateRetained, wantMode: workflow.AssignmentGenerationRetained},
		{name: "collected", runtimeState: workflow.RuntimeStateCollected, wantMode: workflow.AssignmentGenerationNew},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := dormantSnapshot(test.runtimeState)
			event := workflow.TriggerEvent{
				EventMetadata: metadata(snapshot, "trigger-"+test.name), AttemptID: "attempt-2", AttemptNumber: 2,
			}
			decision := workflow.Reduce(snapshot, event)
			assertDecision(t, decision, workflow.DispositionApplied, workflow.ReasonTriggered, workflow.StateDeveloping, snapshot.Revision+1)
			if decision.Snapshot.Assignments.Status != workflow.AssignmentActive || decision.Snapshot.Assignments.RuntimeState != workflow.RuntimeStateActive {
				t.Errorf("desired Assignment state = %#v", decision.Snapshot.Assignments)
			}
			if ensure := onlyAction[workflow.EnsureAssignmentsAction](t, decision.Actions); ensure.Mode != test.wantMode {
				t.Errorf("Assignment intent = %#v, want mode %q", ensure, test.wantMode)
			}
		})
	}
}

func closeEvent(snapshot workflow.Snapshot, closureID, token string) workflow.IssueClosedEvent {
	return workflow.IssueClosedEvent{
		EventMetadata: metadata(snapshot, "close-"+closureID), ClosureID: closureID,
		RetainUntil: observedAt.Add(30 * 24 * time.Hour), RetentionToken: token,
	}
}

func turnGuardPointer(snapshot workflow.Snapshot) *workflow.TurnGuard {
	if snapshot.ActiveTurn == nil {
		return nil
	}
	turn := guard(snapshot)
	return &turn
}

func closedSnapshot(token string) workflow.Snapshot {
	return workflow.Snapshot{
		State: workflow.StateClosed, Revision: 20,
		WorkItem:       workflow.WorkItem{RepositoryID: 91, IssueID: 45, IssueNumber: 12},
		ChangeProposal: proposal(64, "head-closed"),
		Assignments: workflow.Assignments{
			Status:       workflow.AssignmentCompleted,
			RuntimeState: workflow.RuntimeStateRetained, RetentionToken: token, RetainedUntil: observedAt.Add(30 * 24 * time.Hour),
		},
		ResumeRole: workflow.RoleDeveloper, LastAttemptNumber: 1,
	}
}

func dormantSnapshot(runtimeState workflow.RuntimeState) workflow.Snapshot {
	snapshot := closedSnapshot("retention-old")
	snapshot.State = workflow.StateDormant
	snapshot.Assignments.RuntimeState = runtimeState
	snapshot.Assignments.RetentionToken = ""
	snapshot.Assignments.RetainedUntil = time.Time{}
	return snapshot
}

func collectionEvent(snapshot workflow.Snapshot, token string, deadline, collectedAt time.Time) workflow.AssignmentsCollectedEvent {
	return workflow.AssignmentsCollectedEvent{
		EventMetadata: metadata(snapshot, "collect-"+token), RetentionToken: token, RetainUntil: deadline, CollectedAt: collectedAt,
	}
}

func actionCount[T workflow.Action](actions []workflow.Action) int {
	count := 0
	for _, action := range actions {
		if _, ok := action.(T); ok {
			count++
		}
	}
	return count
}

func actionIndex[T workflow.Action](actions []workflow.Action) int {
	for index, action := range actions {
		if _, ok := action.(T); ok {
			return index
		}
	}
	return -1
}

func developingSnapshot(changeProposal *workflow.ChangeProposal) workflow.Snapshot {
	snapshot := baseSnapshot(workflow.StateDeveloping, 0)
	snapshot.ChangeProposal = changeProposal
	snapshot.ActiveTurn = activeTurn(snapshot, workflow.RoleDeveloper)
	return snapshot
}

func reviewingSnapshot(usedReviews uint8, head string) workflow.Snapshot {
	snapshot := baseSnapshot(workflow.StateReviewing, usedReviews)
	snapshot.ChangeProposal = proposal(64, head)
	snapshot.ActiveTurn = activeTurn(snapshot, workflow.RoleReviewer)
	return snapshot
}

func baseSnapshot(state workflow.State, usedReviews uint8) workflow.Snapshot {
	attempt := workflow.WorkflowAttempt{
		ID: "attempt-1", Number: 1, StartedAt: observedAt.Add(-time.Hour), Lifecycle: workflow.AttemptActive,
		ReviewBudget: workflow.Budget{Used: usedReviews, Limit: 3}, InfrastructureRetryBudget: workflow.Budget{Limit: 1},
	}
	return workflow.Snapshot{
		State: state, Revision: 7,
		WorkItem:          workflow.WorkItem{RepositoryID: 91, IssueID: 45, IssueNumber: 12},
		CurrentAttempt:    &attempt,
		Assignments:       workflow.Assignments{Status: workflow.AssignmentActive, RuntimeState: workflow.RuntimeStateActive},
		LastAttemptNumber: 1,
	}
}

func activeTurn(snapshot workflow.Snapshot, role workflow.Role) *workflow.ActiveTurn {
	turn := &workflow.ActiveTurn{
		ID: "turn-active", SessionID: "session-active", AttemptID: snapshot.CurrentAttempt.ID, Role: role,
		Epoch: 4, ControlRevision: 9,
	}
	if snapshot.ChangeProposal != nil {
		turn.ChangeProposalID = snapshot.ChangeProposal.ID
		turn.ExpectedHeadSHA = snapshot.ChangeProposal.HeadSHA
	}
	return turn
}

func metadata(snapshot workflow.Snapshot, id string) workflow.EventMetadata {
	return workflow.EventMetadata{ID: id, ObservedAt: observedAt, WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision}
}

func guard(snapshot workflow.Snapshot) workflow.TurnGuard {
	turn := snapshot.ActiveTurn
	return workflow.TurnGuard{
		TurnID: turn.ID, SessionID: turn.SessionID, AttemptID: turn.AttemptID, Role: turn.Role,
		Epoch: turn.Epoch, ControlRevision: turn.ControlRevision, ChangeProposalID: turn.ChangeProposalID, ExpectedHeadSHA: turn.ExpectedHeadSHA,
	}
}

func settledEvent(snapshot workflow.Snapshot, id string, outcome workflow.TurnOutcome) workflow.TurnSettledEvent {
	return workflow.TurnSettledEvent{EventMetadata: metadata(snapshot, id), Turn: guard(snapshot), Outcome: outcome}
}

func reviewEvent(snapshot workflow.Snapshot, id string, outcome workflow.TurnOutcome, identity *workflow.ReviewIdentity, observedProposal *workflow.ChangeProposal) workflow.TurnSettledEvent {
	event := settledEvent(snapshot, id, outcome)
	event.Review = &workflow.ReviewIdentity{
		ID: identity.ID, NodeID: identity.NodeID, ChangeProposalID: identity.ChangeProposalID, ActorID: identity.ActorID, HeadSHA: identity.HeadSHA,
	}
	event.ChangeProposal = observedProposal
	event.AuthorizedReviewerActorID = 7001
	return event
}

func proposal(id int64, head string) *workflow.ChangeProposal {
	return &workflow.ChangeProposal{ID: id, Number: id, HeadSHA: head, Open: true}
}

func review(id, changeProposalID int64, head string) *workflow.ReviewIdentity {
	return &workflow.ReviewIdentity{ID: id, NodeID: "review-node", ChangeProposalID: changeProposalID, ActorID: 7001, HeadSHA: head}
}

func assertDecision(t *testing.T, decision workflow.Decision, disposition workflow.Disposition, reason workflow.Reason, state workflow.State, revision uint64) {
	t.Helper()
	if decision.Disposition != disposition || decision.Reason != reason || decision.Snapshot.State != state || decision.Snapshot.Revision != revision {
		t.Fatalf("decision = (%q, %q, %q, %d), want (%q, %q, %q, %d)", decision.Disposition, decision.Reason, decision.Snapshot.State, decision.Snapshot.Revision, disposition, reason, state, revision)
	}
}

func assertActionCount[T workflow.Action](t *testing.T, actions []workflow.Action, want int) {
	t.Helper()
	count := actionCount[T](actions)
	if count != want {
		t.Errorf("%T action count = %d, want %d in %#v", *new(T), count, want, actions)
	}
}
