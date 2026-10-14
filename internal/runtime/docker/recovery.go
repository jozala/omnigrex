package docker

import (
	"context"
	"math"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type recoveryInventory interface {
	List(context.Context) (RuntimeProcessInventorySnapshot, error)
}
type recoveryCleaner interface {
	EnsureKeyAbsent(context.Context, execution.Key) error
	execution.OrphanCleaner
	EnsureManagedContainerAbsent(context.Context, string) error
}

// Recovery translates vendor inventory and management operations at the adapter.
type Recovery struct {
	inventory recoveryInventory
	cleaner   recoveryCleaner
}

func NewRecovery(inventory recoveryInventory, cleaner recoveryCleaner) *Recovery {
	return &Recovery{inventory: inventory, cleaner: cleaner}
}

func (adapter *Recovery) List(ctx context.Context) (execution.Snapshot, error) {
	dockerSnapshot, err := adapter.inventory.List(ctx)
	if err != nil {
		return execution.Snapshot{}, err
	}
	var snapshot execution.Snapshot
	for _, process := range dockerSnapshot.Processes {
		identity, ok := executionKey(process.Identity)
		if !ok {
			snapshot.Malformed = append(snapshot.Malformed, execution.MalformedProcess{
				Resource: execution.Ref(process.ContainerID), Reason: string(MalformedExecutionEpoch),
			})
			continue
		}
		snapshot.Processes = append(snapshot.Processes, execution.ManagedProcess{Identity: identity, Resource: execution.Ref(process.ContainerID)})
	}
	for _, duplicate := range dockerSnapshot.Duplicates {
		identity, ok := executionKey(duplicate.Identity)
		var refs []execution.Ref
		for _, id := range duplicate.ContainerIDs {
			if !ok {
				snapshot.Malformed = append(snapshot.Malformed, execution.MalformedProcess{Resource: execution.Ref(id), Reason: string(MalformedExecutionEpoch)})
			} else {
				refs = append(refs, execution.Ref(id))
			}
		}
		if ok {
			snapshot.Duplicates = append(snapshot.Duplicates, execution.DuplicateProcesses{Identity: identity, Resources: refs})
		}
	}
	for _, malformed := range dockerSnapshot.Malformed {
		snapshot.Malformed = append(snapshot.Malformed, execution.MalformedProcess{Resource: execution.Ref(malformed.ContainerID), Reason: string(malformed.Reason)})
	}
	return snapshot, nil
}

func (adapter *Recovery) EnsureAbsent(ctx context.Context, key execution.Key) error {
	return adapter.cleaner.EnsureKeyAbsent(ctx, key)
}
func (adapter *Recovery) EnsureOrphansAbsent(ctx context.Context, key execution.Key, resources []execution.Ref) error {
	return adapter.cleaner.EnsureOrphansAbsent(ctx, key, resources)
}
func (adapter *Recovery) EnsureMalformedAbsent(ctx context.Context, process execution.MalformedProcess) error {
	return adapter.cleaner.EnsureManagedContainerAbsent(ctx, string(process.Resource))
}
func executionKey(identity RuntimeProcessIdentity) (execution.Key, bool) {
	if identity.ExecutionEpoch > math.MaxInt64 {
		return execution.Key{}, false
	}
	return execution.Key{
		AssignmentID: identity.AssignmentID, AgentSessionID: identity.AgentSessionID,
		AgentTurnID: identity.AgentTurnID, ExecutionEpoch: int64(identity.ExecutionEpoch),
		RuntimeProfileName: identity.RuntimeProfile.Name, RuntimeProfileVersion: identity.RuntimeProfile.Version,
	}, true
}
