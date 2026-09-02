package workflow_test

import (
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/workflow"
)

var observedAt = time.Date(2026, time.September, 2, 10, 0, 0, 0, time.UTC)

func TestTriggerEmitsDeclarativeDeveloperIntentWithoutAllocatingTurn(t *testing.T) {
	event := workflow.TriggerEvent{
		EventMetadata: workflow.EventMetadata{
			ID:               "delivery-trigger",
			ObservedAt:       observedAt,
			WorkItem:         workflow.WorkItem{RepositoryID: 91, IssueID: 45, IssueNumber: 12},
			ExpectedRevision: 0,
		},
		AttemptID:     "attempt-1",
		AttemptNumber: 1,
	}

	decision := workflow.Reduce(workflow.Snapshot{State: workflow.StateAbsent}, event)

	if decision.Disposition != workflow.DispositionApplied {
		t.Fatalf("disposition = %q (%q), want APPLIED", decision.Disposition, decision.Reason)
	}
	if decision.Snapshot.ActiveTurn != nil {
		t.Fatalf("active Agent Turn = %#v, want nil until Store allocates it", decision.Snapshot.ActiveTurn)
	}
	if decision.Snapshot.CurrentAttempt == nil || decision.Snapshot.CurrentAttempt.Lifecycle != workflow.AttemptActive {
		t.Fatalf("current Workflow Attempt = %#v, want ACTIVE", decision.Snapshot.CurrentAttempt)
	}
	action := onlyAction[workflow.EnqueueTurnAction](t, decision.Actions)
	if action.Role != workflow.RoleDeveloper || action.Purpose != workflow.TurnPurposeInitialDevelopment || action.ExpectedHeadSHA != "" || action.RetryOfTurnID != "" {
		t.Errorf("enqueue intent = %#v, want identity-free initial Developer intent", action)
	}
	assignments := onlyAction[workflow.EnsureAssignmentsAction](t, decision.Actions)
	if assignments.Mode != workflow.AssignmentGenerationNew {
		t.Errorf("Assignment intent = %#v, want new generation", assignments)
	}
}

func onlyAction[T workflow.Action](t *testing.T, actions []workflow.Action) T {
	t.Helper()
	var found []T
	for _, action := range actions {
		if typed, ok := action.(T); ok {
			found = append(found, typed)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%T action count = %d, want 1 in %#v", *new(T), len(found), actions)
	}
	return found[0]
}
