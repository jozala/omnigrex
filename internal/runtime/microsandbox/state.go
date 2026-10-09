package microsandbox

import (
	"context"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func (backend *Backend) CheckStateRecoveryBoundary(ctx context.Context) error {
	snapshot, err := backend.List(ctx)
	if err != nil {
		return err
	}
	if len(snapshot.Malformed) > 0 || len(snapshot.Duplicates) > 0 {
		return execution.ErrUnknownEffect
	}
	for _, process := range snapshot.Processes {
		actual, err := backend.resolveRef(ctx, process.Resource)
		if err != nil {
			return err
		}
		if !terminalEnvironmentStatus(actual.Status) {
			return execution.ErrUnknownEffect
		}
	}
	return nil
}
