package retention_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/retention"
)

type orderedRuntimeCleaner struct {
	steps *[]string
	err   error
}

func (cleaner orderedRuntimeCleaner) EnsureAbsent(context.Context, string, string) error {
	*cleaner.steps = append(*cleaner.steps, "runtime-state")
	return cleaner.err
}

type orderedToolCleaner struct{ steps *[]string }

func (cleaner orderedToolCleaner) CleanupAssignmentToolPaths(context.Context, string) error {
	*cleaner.steps = append(*cleaner.steps, "tool-data")
	return nil
}

func TestAssignmentToolPathsAreCollectedOnlyAfterRuntimeState(t *testing.T) {
	steps := []string{}
	cleaner := retention.NewToolPathCleaner(orderedRuntimeCleaner{steps: &steps}, orderedToolCleaner{steps: &steps})
	if err := cleaner.EnsureAbsent(context.Background(), "assignment", "state"); err != nil || !reflect.DeepEqual(steps, []string{"runtime-state", "tool-data"}) {
		t.Fatalf("collection = (%v, %v)", steps, err)
	}
	steps = nil
	failure := errors.New("runtime state unavailable")
	cleaner = retention.NewToolPathCleaner(orderedRuntimeCleaner{steps: &steps, err: failure}, orderedToolCleaner{steps: &steps})
	if err := cleaner.EnsureAbsent(context.Background(), "assignment", "state"); !errors.Is(err, failure) || !reflect.DeepEqual(steps, []string{"runtime-state"}) {
		t.Fatalf("failed collection = (%v, %v)", steps, err)
	}
}
