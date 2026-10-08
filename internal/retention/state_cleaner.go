package retention

import (
	"context"
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
	return cleaner.States.Collect(ctx, ref)
}
