package session

import (
	"context"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

type humanPromptHeartbeater interface {
	HeartbeatHumanPromptLease(context.Context, store.HumanPromptLease, time.Duration) error
}

func heartbeatHumanPromptLease(ctx context.Context, admission humanPromptHeartbeater, lease store.HumanPromptLease, extension, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := admission.HeartbeatHumanPromptLease(ctx, lease, extension); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}
