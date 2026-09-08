// Package startup reconciles durable Agent Turns with managed Runtime Processes before execution starts.
package startup

import (
	"context"
	"errors"
	"fmt"

	"github.com/jozala/omnigrex/internal/store"
)

var ErrReconciliationUnstable = errors.New("startup reconciliation did not reach a stable point")

// ManagedRuntimeProcess is one discovered Runtime Process with a complete label identity.
type ManagedRuntimeProcess struct {
	Identity    store.AgentTurnRuntimeIdentity
	ContainerID string
}

// DuplicateManagedRuntimeProcess groups every discovered Runtime Process carrying one exact identity.
type DuplicateManagedRuntimeProcess struct {
	Identity     store.AgentTurnRuntimeIdentity
	ContainerIDs []string
}

// MalformedManagedRuntimeProcess is marked as managed but cannot be assigned a trusted identity.
type MalformedManagedRuntimeProcess struct {
	ContainerID string
	Reason      string
}

// RuntimeInventorySnapshot is one complete observation of managed Runtime Processes.
type RuntimeInventorySnapshot struct {
	Processes  []ManagedRuntimeProcess
	Duplicates []DuplicateManagedRuntimeProcess
	Malformed  []MalformedManagedRuntimeProcess
}

type TurnStore interface {
	ClaimAndRecoverExpiredAgentTurn(context.Context) (store.AgentTurnRecovery, bool, error)
	HasRecoverableExpiredAgentTurn(context.Context) (bool, error)
	ClassifyAgentTurnRuntime(context.Context, store.AgentTurnRuntimeIdentity) (store.AgentTurnRuntimeState, bool, error)
	FenceDuplicateAgentTurnRuntime(context.Context, store.AgentTurnRuntimeIdentity) (store.AgentTurnRecovery, error)
	RecoverExpiredAgentTurn(context.Context, string, int64) (store.AgentTurnRecovery, error)
}

type RuntimeInventory interface {
	List(context.Context) (RuntimeInventorySnapshot, error)
}

// RuntimeCleaner must remove every Runtime Process carrying the supplied exact label identity.
type RuntimeCleaner interface {
	EnsureAbsent(context.Context, store.AgentTurnRuntimeIdentity) error
}

// MalformedRuntimeCleaner must revalidate that a container is managed before removing it by ID.
type MalformedRuntimeCleaner interface {
	EnsureMalformedAbsent(context.Context, MalformedManagedRuntimeProcess) error
}

type ReconcilerOptions struct {
	MaxPasses           int
	MaxRuntimeProcesses int
	MaxRecoveries       int
}

type ReconciliationResult struct {
	Passes                    int
	RecoveredAgentTurns       int
	RemovedRuntimeIdentities  int
	RemovedMalformedProcesses int
}

// Reconciler fences expired Agent Turns and removes Runtime Processes that PostgreSQL cannot authorize.
type Reconciler struct {
	turns            TurnStore
	inventory        RuntimeInventory
	cleaner          RuntimeCleaner
	malformedCleaner MalformedRuntimeCleaner
	options          ReconcilerOptions
}

func NewReconciler(turns TurnStore, inventory RuntimeInventory, cleaner RuntimeCleaner, malformedCleaner MalformedRuntimeCleaner, options ReconcilerOptions) (*Reconciler, error) {
	if turns == nil || inventory == nil || cleaner == nil || malformedCleaner == nil {
		return nil, errors.New("create startup reconciler: dependencies are required")
	}
	if options.MaxPasses < 2 || options.MaxPasses > 10_000 ||
		options.MaxRuntimeProcesses <= 0 || options.MaxRuntimeProcesses > 1_000_000 ||
		options.MaxRecoveries <= 0 || options.MaxRecoveries > 1_000_000 {
		return nil, errors.New("create startup reconciler: limits are invalid")
	}
	return &Reconciler{
		turns: turns, inventory: inventory, cleaner: cleaner,
		malformedCleaner: malformedCleaner, options: options,
	}, nil
}

// Reconcile returns after two consecutive action-free scans, or fails at a configured bound.
// Existing prompts are never adopted: only a currently live database lease can preserve a process.
func (reconciler *Reconciler) Reconcile(ctx context.Context) (ReconciliationResult, error) {
	var result ReconciliationResult
	actionFreePasses := 0
	for pass := 1; pass <= reconciler.options.MaxPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Passes = pass
		actions := 0
		for {
			if result.RecoveredAgentTurns >= reconciler.options.MaxRecoveries {
				more, err := reconciler.turns.HasRecoverableExpiredAgentTurn(ctx)
				if err != nil {
					return result, fmt.Errorf("inspect startup Agent Turn recovery bound: %w", err)
				}
				if more {
					return result, fmt.Errorf("%w: recovery limit reached", ErrReconciliationUnstable)
				}
				break
			}
			_, recovered, err := reconciler.turns.ClaimAndRecoverExpiredAgentTurn(ctx)
			if err != nil {
				return result, fmt.Errorf("recover expired Agent Turn during startup: %w", err)
			}
			if !recovered {
				break
			}
			result.RecoveredAgentTurns++
			actions++
		}

		snapshot, err := reconciler.inventory.List(ctx)
		if err != nil {
			return result, fmt.Errorf("list managed Runtime Processes during startup: %w", err)
		}
		processCount := len(snapshot.Processes) + len(snapshot.Malformed)
		for _, duplicate := range snapshot.Duplicates {
			processCount += len(duplicate.ContainerIDs)
		}
		if processCount > reconciler.options.MaxRuntimeProcesses {
			return result, fmt.Errorf("%w: Runtime Process limit reached", ErrReconciliationUnstable)
		}

		identities := make(map[store.AgentTurnRuntimeIdentity]bool, len(snapshot.Processes)+len(snapshot.Duplicates))
		for _, process := range snapshot.Processes {
			identities[process.Identity] = false
		}
		for _, duplicate := range snapshot.Duplicates {
			identities[duplicate.Identity] = true
		}
		for identity, duplicate := range identities {
			acted, removed, err := reconciler.reconcileIdentity(ctx, identity, duplicate)
			if err != nil {
				return result, err
			}
			if acted {
				actions++
			}
			if removed {
				result.RemovedRuntimeIdentities++
			}
		}
		for _, malformed := range snapshot.Malformed {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if err := reconciler.malformedCleaner.EnsureMalformedAbsent(ctx, malformed); err != nil {
				return result, fmt.Errorf("remove malformed managed Runtime Process %q: %w", malformed.ContainerID, err)
			}
			actions++
			result.RemovedMalformedProcesses++
		}

		if actions == 0 {
			actionFreePasses++
			if actionFreePasses == 2 {
				return result, nil
			}
		} else {
			actionFreePasses = 0
		}
	}
	return result, ErrReconciliationUnstable
}

func (reconciler *Reconciler) reconcileIdentity(ctx context.Context, identity store.AgentTurnRuntimeIdentity, duplicate bool) (bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	state, found, err := reconciler.turns.ClassifyAgentTurnRuntime(ctx, identity)
	if err != nil {
		return false, false, fmt.Errorf("classify managed Runtime Process: %w", err)
	}
	if found {
		switch state.Disposition {
		case store.AgentTurnRuntimeLive:
			if duplicate {
				if _, err := reconciler.turns.FenceDuplicateAgentTurnRuntime(ctx, identity); err != nil {
					if errors.Is(err, store.ErrAgentTurnFenceLost) {
						return true, false, nil
					}
					return false, false, fmt.Errorf("fence duplicate live Runtime Processes: %w", err)
				}
				break
			}
			return false, false, nil
		case store.AgentTurnRuntimeRecovery:
			if !duplicate {
				return false, false, nil
			}
		case store.AgentTurnRuntimeExpired:
			_, err := reconciler.turns.RecoverExpiredAgentTurn(ctx, identity.AgentTurnID, identity.ExecutionEpoch)
			if errors.Is(err, store.ErrAgentTurnNotExpired) {
				return duplicate, false, nil
			}
			if err != nil {
				return false, false, fmt.Errorf("fence discovered expired Agent Turn: %w", err)
			}
			if !duplicate {
				return true, false, nil
			}
		case store.AgentTurnRuntimeActive, store.AgentTurnRuntimeTerminal:
		default:
			return false, false, fmt.Errorf("classify managed Runtime Process: unknown disposition %q", state.Disposition)
		}
	}
	if err := reconciler.cleaner.EnsureAbsent(ctx, identity); err != nil {
		return false, false, fmt.Errorf("remove unauthorized Runtime Process identity: %w", err)
	}
	return true, true, nil
}
