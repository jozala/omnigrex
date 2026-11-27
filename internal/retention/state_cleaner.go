package retention

import (
	"context"
	"log/slog"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type CollectionStateSource interface {
	CollectionState(context.Context, string) (execution.StateRef, error)
}

type StateCleaner struct {
	Source CollectionStateSource
	States execution.RetainedState
}

func (cleaner StateCleaner) CollectParticipant(ctx context.Context, participantID string) error {
	ref, err := cleaner.Source.CollectionState(ctx, participantID)
	if err != nil {
		return err
	}
	err = cleaner.States.Collect(ctx, ref)
	status := "absent"
	if err != nil {
		status = "incomplete"
	}
	slog.InfoContext(ctx, "Participant retained state collection", "agent_participant_id", participantID,
		"category", "session-restore-staging-native-logs", "status", status, "reason", cleanupReason(err), "measurement", "unavailable; physical release unknown")
	return err
}

func cleanupReason(err error) string {
	if err != nil {
		return "required retained-state cleanup failed; collection incomplete"
	}
	return "required retained-state categories absent"
}
