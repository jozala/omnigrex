package docker

import (
	"context"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

// CheckStateRecoveryBoundary proves that no managed execution can currently write
// this volume. The offline control-plane command itself also mounts the volume.
// Collection helpers are excluded by the Store's recovery/collection barrier.
func CheckStateRecoveryBoundary(ctx context.Context, volume string) error {
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := client.VolumeInspect(ctx, volume, mobyclient.VolumeInspectOptions{}); err != nil {
		return err
	}
	resources, err := client.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: make(mobyclient.Filters).Add("volume", volume).Add("label", RuntimeProcessMarkerLabel+"="+RuntimeProcessMarkerValue)})
	if err != nil {
		return err
	}
	for _, resource := range resources.Items {
		actual, err := client.ContainerInspect(ctx, resource.ID, mobyclient.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		if actual.Container.State == nil || actual.Container.State.Running || actual.Container.State.Status != container.StateExited && actual.Container.State.Status != container.StateDead {
			return execution.ErrUnknownEffect
		}
	}
	return nil
}
