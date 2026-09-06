package startup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

const maximumMonitorDuration = 365 * 24 * time.Hour

// ExpiredTurnStore is the database-only recovery authority used by Monitor.
type ExpiredTurnStore interface {
	ClaimAndRecoverExpiredAgentTurn(context.Context) (store.AgentTurnRecovery, bool, error)
}

// MonitorOptions bounds each periodic recovery scan and controls idle polling.
type MonitorOptions struct {
	PollInterval         time.Duration
	MaxRecoveriesPerPoll int
	OnError              func(error)
}

// Monitor periodically fences expired Agent Turns without inspecting or adopting Runtime Process prompt state.
type Monitor struct {
	turns                ExpiredTurnStore
	pollInterval         time.Duration
	maxRecoveriesPerPoll int
	onError              func(error)
}

func NewMonitor(turns ExpiredTurnStore, options MonitorOptions) (*Monitor, error) {
	if turns == nil || options.PollInterval < time.Microsecond || options.PollInterval > maximumMonitorDuration ||
		options.MaxRecoveriesPerPoll <= 0 || options.MaxRecoveriesPerPoll > 1_000_000 {
		return nil, errors.New("create expired Agent Turn monitor: configuration is invalid")
	}
	return &Monitor{
		turns: turns, pollInterval: options.PollInterval,
		maxRecoveriesPerPoll: options.MaxRecoveriesPerPoll, onError: options.OnError,
	}, nil
}

// Reconcile recovers a bounded number of currently expired Agent Turns.
func (monitor *Monitor) Reconcile(ctx context.Context) (int, error) {
	recovered := 0
	for recovered < monitor.maxRecoveriesPerPoll {
		if err := ctx.Err(); err != nil {
			return recovered, err
		}
		_, claimed, err := monitor.turns.ClaimAndRecoverExpiredAgentTurn(ctx)
		if err != nil {
			return recovered, fmt.Errorf("periodically recover expired Agent Turn: %w", err)
		}
		if !claimed {
			return recovered, nil
		}
		recovered++
	}
	return recovered, nil
}

// Run polls until cancellation. A failed scan is delayed before retrying to avoid a busy loop.
func (monitor *Monitor) Run(ctx context.Context) error {
	for {
		if _, err := monitor.Reconcile(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if monitor.onError != nil {
				monitor.onError(err)
			}
		}
		timer := time.NewTimer(monitor.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
