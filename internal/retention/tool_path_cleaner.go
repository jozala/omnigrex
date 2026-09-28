package retention

import "context"

// AssignmentToolPathCleaner removes disposable tool caches after the retained runtime state is gone.
type AssignmentToolPathCleaner interface {
	CleanupAssignmentToolPaths(context.Context, string) error
}

type toolPathCleaner struct {
	runtime Cleaner
	paths   AssignmentToolPathCleaner
}

func NewToolPathCleaner(runtime Cleaner, paths AssignmentToolPathCleaner) Cleaner {
	return toolPathCleaner{runtime: runtime, paths: paths}
}

func (cleaner toolPathCleaner) EnsureAbsent(ctx context.Context, assignmentID, runtimeStatePath string) error {
	if err := cleaner.runtime.EnsureAbsent(ctx, assignmentID, runtimeStatePath); err != nil {
		return err
	}
	return cleaner.paths.CleanupAssignmentToolPaths(ctx, assignmentID)
}
