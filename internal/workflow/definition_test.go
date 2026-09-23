package workflow_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestBuiltinDefinitionDescribesCurrentWorkflow(t *testing.T) {
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatalf("NewBuiltinDefinition() error = %v", err)
	}

	initial := definition.InitialEntry()
	if initial.Stage != workflow.StageImplementation || initial.Purpose != workflow.TurnPurposeInitialDevelopment {
		t.Fatalf("initial entry = %#v", initial)
	}
	implementation, ok := definition.Stage(workflow.StageImplementation)
	if !ok || implementation.Role != role.Developer || implementation.State != workflow.StateDeveloping || implementation.ReviewLimit != 0 {
		t.Fatalf("implementation Stage = (%#v, %t)", implementation, ok)
	}
	if got := definition.ExpectedOutcomes(workflow.StageImplementation); !reflect.DeepEqual(got, []workflow.TurnOutcome{workflow.TurnOutcomeChangeProposalReady}) {
		t.Fatalf("implementation outcomes = %v", got)
	}
	review, ok := definition.Stage(workflow.StageReview)
	if !ok || review.Role != role.Reviewer || review.State != workflow.StateReviewing || review.ReviewLimit != 3 {
		t.Fatalf("review Stage = (%#v, %t)", review, ok)
	}
	if got := definition.ExpectedOutcomes(workflow.StageReview); !reflect.DeepEqual(got, []workflow.TurnOutcome{
		workflow.TurnOutcomeChangesRequested, workflow.TurnOutcomeApproved,
	}) {
		t.Fatalf("review outcomes = %v", got)
	}
	if got := definition.Roles(); !reflect.DeepEqual(got, []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer}) {
		t.Fatalf("Roles() = %v", got)
	}
	approved, ok := definition.Transition(workflow.StageReview, workflow.TurnOutcomeApproved)
	if !ok || approved.TerminalState != workflow.StatePRReady || approved.ContinuationStage != workflow.StageReview {
		t.Fatalf("approved transition = (%#v, %t)", approved, ok)
	}
}

func TestReducerValidatesRoleCapabilitiesAgainstStageOutcomes(t *testing.T) {
	definition := builtinReducer(t).Definition()
	if err := definition.ValidateRolePolicies(role.BuiltinPolicyCatalog()); err != nil {
		t.Fatalf("ValidateRolePolicies() built-in error = %v", err)
	}
	builtin := role.BuiltinPolicyCatalog()
	developer, _ := builtin.Lookup(role.Developer)
	reviewer, _ := builtin.Lookup(role.Reviewer)
	developer.MCPTools = []string{"get_issue", "report_blocked"}
	policies, err := role.NewPolicyCatalog([]role.ID{role.Developer, role.Reviewer}, []role.Policy{developer, reviewer})
	if err != nil {
		t.Fatal(err)
	}
	if err := definition.ValidateRolePolicies(policies); !errors.Is(err, workflow.ErrInvalidDefinition) {
		t.Fatalf("ValidateRolePolicies() error = %v, want ErrInvalidDefinition", err)
	}
}

func TestDefinitionSupportsSeveralStagesForOneRole(t *testing.T) {
	catalog, err := role.NewCatalog([]role.Metadata{{ID: role.Reviewer, DisplayName: "Reviewer", Description: "Reviews work."}})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	definition, err := workflow.NewDefinition(catalog, workflow.StageEntry{Stage: "code-review", Purpose: workflow.TurnPurposeInitialDevelopment}, []workflow.StageDefinition{
		{
			ID: "code-review", Role: role.Reviewer, State: workflow.StateDeveloping,
			AcceptedPurposes: []workflow.TurnPurpose{workflow.TurnPurposeInitialDevelopment, workflow.TurnPurposeRetry, workflow.TurnPurposeReactivation},
			Transitions:      []workflow.OutcomeTransition{{Outcome: workflow.TurnOutcomeChangeProposalReady, NextStage: "security-review", NextPurpose: workflow.TurnPurposeReview}},
		},
		{
			ID: "security-review", Role: role.Reviewer, State: workflow.StateReviewing,
			AcceptedPurposes: []workflow.TurnPurpose{workflow.TurnPurposeReview, workflow.TurnPurposeRetry, workflow.TurnPurposeSynchronization, workflow.TurnPurposeReactivation},
			Transitions:      []workflow.OutcomeTransition{{Outcome: workflow.TurnOutcomeApproved, TerminalState: workflow.StatePRReady, ContinuationStage: "security-review"}},
		},
	})
	if err != nil {
		t.Fatalf("NewDefinition() error = %v", err)
	}
	if stage, ok := definition.Stage("security-review"); !ok || stage.Role != role.Reviewer {
		t.Fatalf("security-review Stage = (%#v, %t)", stage, ok)
	}
}

func TestDefinitionRejectsInvalidTopology(t *testing.T) {
	catalog := role.BuiltinCatalog()
	validStage := workflow.StageDefinition{
		ID: "implementation", Role: role.Developer, State: workflow.StateDeveloping,
		AcceptedPurposes: []workflow.TurnPurpose{workflow.TurnPurposeInitialDevelopment},
		Transitions: []workflow.OutcomeTransition{{
			Outcome: workflow.TurnOutcomeChangeProposalReady, TerminalState: workflow.StatePRReady, ContinuationStage: "implementation",
		}},
	}
	tests := []struct {
		name    string
		initial workflow.StageEntry
		stages  []workflow.StageDefinition
	}{
		{name: "no Stages", initial: workflow.StageEntry{Stage: "implementation", Purpose: workflow.TurnPurposeInitialDevelopment}},
		{name: "unknown initial Stage", initial: workflow.StageEntry{Stage: "missing", Purpose: workflow.TurnPurposeInitialDevelopment}, stages: []workflow.StageDefinition{validStage}},
		{name: "unknown Role", initial: workflow.StageEntry{Stage: "implementation", Purpose: workflow.TurnPurposeInitialDevelopment}, stages: []workflow.StageDefinition{{
			ID: "implementation", Role: "ARCHITECT", State: workflow.StateDeveloping,
			AcceptedPurposes: validStage.AcceptedPurposes, Transitions: validStage.Transitions,
		}}},
		{name: "invalid Stage ID", initial: workflow.StageEntry{Stage: "Implementation", Purpose: workflow.TurnPurposeInitialDevelopment}, stages: []workflow.StageDefinition{{
			ID: "Implementation", Role: role.Developer, State: workflow.StateDeveloping,
			AcceptedPurposes: validStage.AcceptedPurposes, Transitions: validStage.Transitions,
		}}},
		{name: "unknown successor", initial: workflow.StageEntry{Stage: "implementation", Purpose: workflow.TurnPurposeInitialDevelopment}, stages: []workflow.StageDefinition{{
			ID: "implementation", Role: role.Developer, State: workflow.StateDeveloping,
			AcceptedPurposes: validStage.AcceptedPurposes,
			Transitions:      []workflow.OutcomeTransition{{Outcome: workflow.TurnOutcomeChangeProposalReady, NextStage: "missing", NextPurpose: workflow.TurnPurposeReview}},
		}}},
		{name: "mixed terminal and successor", initial: workflow.StageEntry{Stage: "implementation", Purpose: workflow.TurnPurposeInitialDevelopment}, stages: []workflow.StageDefinition{{
			ID: "implementation", Role: role.Developer, State: workflow.StateDeveloping,
			AcceptedPurposes: validStage.AcceptedPurposes,
			Transitions: []workflow.OutcomeTransition{{
				Outcome: workflow.TurnOutcomeChangeProposalReady, NextStage: "implementation", NextPurpose: workflow.TurnPurposeInitialDevelopment,
				TerminalState: workflow.StatePRReady, ContinuationStage: "implementation",
			}},
		}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := workflow.NewDefinition(catalog, test.initial, test.stages); !errors.Is(err, workflow.ErrInvalidDefinition) {
				t.Fatalf("NewDefinition() error = %v, want ErrInvalidDefinition", err)
			}
		})
	}
}

func TestDefinitionResultsCannotMutateDefinition(t *testing.T) {
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatalf("NewBuiltinDefinition() error = %v", err)
	}
	stage, _ := definition.Stage(workflow.StageReview)
	stage.AcceptedPurposes[0] = workflow.TurnPurposeInitialDevelopment
	stage.Transitions[0].NextStage = "mutated"

	again, _ := definition.Stage(workflow.StageReview)
	transition, _ := definition.Transition(workflow.StageReview, workflow.TurnOutcomeChangesRequested)
	if again.AcceptedPurposes[0] != workflow.TurnPurposeReview || transition.NextStage != workflow.StageImplementation {
		t.Fatalf("Definition was mutated: %#v / %#v", again, transition)
	}
}

func TestConfiguredReducerPersistsAndTransitionsAttemptStage(t *testing.T) {
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatalf("NewBuiltinDefinition() error = %v", err)
	}
	reducer, err := workflow.NewReducer(definition, 1)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	absent := workflow.Snapshot{State: workflow.StateAbsent}
	trigger := workflow.TriggerEvent{
		EventMetadata: workflow.EventMetadata{
			ID: "trigger-stage", ObservedAt: observedAt,
			WorkItem: workflow.WorkItem{RepositoryID: 91, IssueID: 45, IssueNumber: 12},
		},
		AttemptID: "attempt-stage", AttemptNumber: 1,
	}
	started := reducer.Reduce(absent, trigger)
	if started.Disposition != workflow.DispositionApplied || started.Snapshot.CurrentAttempt == nil ||
		started.Snapshot.CurrentAttempt.CurrentStage != workflow.StageImplementation {
		t.Fatalf("trigger decision = %#v", started)
	}
	startTurn := onlyAction[workflow.EnqueueTurnAction](t, started.Actions)
	if startTurn.Stage != workflow.StageImplementation || startTurn.Role != role.Developer || startTurn.Purpose != workflow.TurnPurposeInitialDevelopment {
		t.Fatalf("initial Turn = %#v", startTurn)
	}

	implementation := started.Snapshot
	implementation.ActiveTurn = activeTurn(implementation, role.Developer)
	ready := settledEvent(implementation, "implementation-ready", workflow.TurnOutcomeChangeProposalReady)
	ready.ChangeProposal = proposal(64, "head-1")
	reviewing := reducer.Reduce(implementation, ready)
	if reviewing.Disposition != workflow.DispositionApplied || reviewing.Snapshot.CurrentAttempt.CurrentStage != workflow.StageReview || reviewing.Snapshot.State != workflow.StateReviewing {
		t.Fatalf("ready decision = %#v", reviewing)
	}
	reviewTurn := onlyAction[workflow.EnqueueTurnAction](t, reviewing.Actions)
	if reviewTurn.Stage != workflow.StageReview || reviewTurn.Role != role.Reviewer || reviewTurn.Purpose != workflow.TurnPurposeReview {
		t.Fatalf("review Turn = %#v", reviewTurn)
	}

	reviewing.Snapshot.ActiveTurn = activeTurn(reviewing.Snapshot, role.Reviewer)
	approvedEvent := reviewEvent(reviewing.Snapshot, "review-approved", workflow.TurnOutcomeApproved,
		review(501, 64, "head-1"), proposal(64, "head-1"))
	approved := reducer.Reduce(reviewing.Snapshot, approvedEvent)
	if approved.Disposition != workflow.DispositionApplied || approved.Snapshot.State != workflow.StatePRReady ||
		approved.Snapshot.CurrentAttempt.CurrentStage != workflow.StageReview {
		t.Fatalf("approved decision = %#v", approved)
	}
}

func TestReducerFailsClosedForIncompatibleDurableStage(t *testing.T) {
	reducer := builtinReducer(t)
	snapshot := workflow.Snapshot{
		State: workflow.StateDeveloping, Revision: 7,
		Assignments: workflow.Assignments{Status: workflow.AssignmentActive, RuntimeState: workflow.RuntimeStateActive},
		CurrentAttempt: &workflow.WorkflowAttempt{
			ID: "attempt-1", Number: 1, Lifecycle: workflow.AttemptActive,
			CurrentStage: "removed-stage", ReviewUsage: map[workflow.StageID]uint8{},
			InfrastructureRetryBudget: workflow.AttemptBudget{Limit: 1},
		},
	}
	if reducer.DefinitionCompatible(snapshot) {
		t.Fatal("DefinitionCompatible() accepted a removed durable Stage")
	}
	decision := reducer.DefinitionIncompatible(snapshot)
	if decision.Disposition != workflow.DispositionApplied || decision.Reason != workflow.ReasonWorkflowDefinitionIncompatible ||
		decision.Snapshot.State != workflow.StateNeedsHuman || decision.Snapshot.Revision != snapshot.Revision+1 ||
		decision.Snapshot.Assignments.Status != workflow.AssignmentWaitingForHuman {
		t.Fatalf("DefinitionIncompatible() = %#v", decision)
	}
	handoff := onlyAction[workflow.MarkHumanHandoffAction](t, decision.Actions)
	if handoff.Reason != workflow.ReasonWorkflowDefinitionIncompatible {
		t.Fatalf("handoff = %#v", handoff)
	}
}

func TestReducerInterruptsActiveTurnForIncompatibleDefinition(t *testing.T) {
	reducer := builtinReducer(t)
	snapshot := developingSnapshot(nil)
	snapshot.CurrentAttempt.CurrentStage = "removed-stage"
	snapshot.ActiveTurn.Stage = "removed-stage"

	decision := reducer.DefinitionIncompatible(snapshot)
	if decision.Snapshot.ActiveTurn != nil || decision.Snapshot.State != workflow.StateNeedsHuman ||
		decision.Snapshot.ResumeRole != workflow.RoleDeveloper {
		t.Fatalf("DefinitionIncompatible() snapshot = %#v", decision.Snapshot)
	}
	assertActionCount[workflow.CloseMutationAdmissionAction](t, decision.Actions, 1)
	assertActionCount[workflow.InterruptTurnForHumanHandoffAction](t, decision.Actions, 1)
}

func TestDefinitionCompatibilityIncludesPersistedStageStateAndReviewUsage(t *testing.T) {
	reducer := builtinReducer(t)
	stateMismatch := developingSnapshot(nil)
	stateMismatch.State = workflow.StateReviewing
	stateMismatch.ChangeProposal = proposal(1, "head")
	if reducer.DefinitionCompatible(stateMismatch) {
		t.Fatal("DefinitionCompatible() accepted a durable State that disagrees with its Stage")
	}
	unknownReviewStage := developingSnapshot(nil)
	unknownReviewStage.CurrentAttempt.ReviewUsage["removed-review"] = 1
	if reducer.DefinitionCompatible(unknownReviewStage) {
		t.Fatal("DefinitionCompatible() accepted review usage for a removed Stage")
	}
}
