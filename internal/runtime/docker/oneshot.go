package docker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

type oneShotAPI interface {
	ContainerCreate(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error)
	ContainerStart(context.Context, string, mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error)
	ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult
	ContainerRemove(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error)
}

func runOneShotContainer(ctx context.Context, api oneShotAPI, options mobyclient.ContainerCreateOptions, operation string) (runErr error) {
	created, err := api.ContainerCreate(ctx, options)
	if err != nil {
		return fmt.Errorf("create %s container: %w", operation, err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := api.ContainerRemove(cleanupCtx, created.ID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("remove %s container: %w", operation, err))
		}
	}()

	if _, err := api.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start %s container: %w", operation, err)
	}
	wait := api.ContainerWait(ctx, created.ID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case result := <-wait.Result:
		if result.Error != nil {
			return fmt.Errorf("wait for %s container: %s", operation, result.Error.Message)
		}
		if result.StatusCode != 0 {
			return fmt.Errorf("%s container exited with status %d", operation, result.StatusCode)
		}
		return nil
	case err := <-wait.Error:
		return fmt.Errorf("wait for %s container: %w", operation, err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
