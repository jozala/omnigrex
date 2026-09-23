package workflow_test

import (
	"testing"

	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/workflow"
)

var testReducer = func() workflow.Reducer {
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		panic(err)
	}
	reducer, err := workflow.NewReducer(definition, workflow.BuiltinInfrastructureRetryLimit)
	if err != nil {
		panic(err)
	}
	return reducer
}()

func reduce(snapshot workflow.Snapshot, event workflow.Event) workflow.Decision {
	return testReducer.Reduce(snapshot, event)
}

func builtinReducer(t *testing.T) workflow.Reducer {
	t.Helper()
	return testReducer
}

func TestSnapshotCloneDoesNotAliasMutableState(t *testing.T) {
	original := workflow.Snapshot{
		CurrentAttempt: &workflow.WorkflowAttempt{ReviewUsage: map[workflow.StageID]uint8{workflow.StageReview: 1}},
		ChangeProposal: &workflow.ChangeProposal{HeadSHA: "original"},
		ActiveTurn:     &workflow.ActiveTurn{ID: "original"},
	}
	clone := original.Clone()
	clone.CurrentAttempt.ReviewUsage[workflow.StageReview] = 2
	clone.ChangeProposal.HeadSHA = "changed"
	clone.ActiveTurn.ID = "changed"
	if original.CurrentAttempt.ReviewUsage[workflow.StageReview] != 1 || original.ChangeProposal.HeadSHA != "original" || original.ActiveTurn.ID != "original" {
		t.Fatalf("Clone() mutated original Snapshot: %#v", original)
	}
}
