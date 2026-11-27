package retention

import "context"

// AssignmentToolPathCleaner completes remaining Participant-owned storage after backend state collection.
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

func (cleaner toolPathCleaner) CollectParticipant(ctx context.Context, assignmentID string) error {
	if err := cleaner.runtime.CollectParticipant(ctx, assignmentID); err != nil {
		return err
	}
	return cleaner.paths.CleanupAssignmentToolPaths(ctx, assignmentID)
}
