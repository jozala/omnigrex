package composition

import (
	"context"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/microsandbox"
)

func StateBinding(settings config.Config) execution.BackendBinding {
	if settings.ExecutionBackend == "microsandbox" {
		return execution.BackendBinding{Family: "microsandbox", Version: 1, Namespace: settings.MicrosandboxNamespace}
	}
	return execution.BackendBinding{Family: "docker", Version: 1, Namespace: settings.RuntimeStateVolume}
}

func StateRecovery(ctx context.Context, settings config.Config, source execution.StateMetadataSource) (execution.StateRecovery, error) {
	if settings.ExecutionBackend == "microsandbox" {
		backend, err := microsandbox.New(MicrosandboxOptions(settings), nil)
		if err != nil {
			return nil, err
		}
		if err := backend.CheckStateRecoveryBoundary(ctx); err != nil {
			return nil, err
		}
		return dirstate.New(settings.RuntimeStateRoot, StateBinding(settings), source)
	}
	if err := docker.CheckStateRecoveryBoundary(ctx, settings.RuntimeStateVolume); err != nil {
		return nil, err
	}
	return docker.NewRetainedState(settings.RuntimeStateRoot, settings.RuntimeStateVolume, source)
}
