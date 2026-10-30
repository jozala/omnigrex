package composition

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/microsandbox"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

type Cleanup interface {
	execution.Cleaner
	EnsureKeyStopped(context.Context, execution.Key) error
}
type Recovery interface {
	execution.Inventory
	EnsureAbsent(context.Context, execution.Key) error
	execution.OrphanCleaner
	EnsureMalformedAbsent(context.Context, execution.MalformedProcess) error
}
type States interface {
	execution.RetainedState
	execution.StateRecovery
	Binding() execution.BackendBinding
	Check(context.Context) error
}
type Backend struct {
	Factory    execution.Factory
	Workspaces execution.WorkspaceSource
	Snapshots  execution.SnapshotInstaller
	Cleaner    Cleanup
	Recovery   Recovery
	States     States
	Images     ImageAvailability
	Check      func(context.Context) error
	close      []func() error
}

func (backend *Backend) Close() error {
	var failures []error
	for _, close := range backend.close {
		failures = append(failures, close())
	}
	return errors.Join(failures...)
}

func MicrosandboxOptions(settings config.Config) microsandbox.Options {
	return microsandbox.Options{Executable: settings.MicrosandboxExecutable, Firmware: settings.MicrosandboxFirmware, Home: settings.MicrosandboxHome, Namespace: settings.MicrosandboxNamespace, WorkspaceRoot: settings.WorkspaceRoot, StateRoot: settings.RuntimeStateRoot, ToolRoot: settings.MiseRoot, CPUs: settings.MicrosandboxCPUs, RootDiskMiB: settings.MicrosandboxRootDiskMiB, MaximumDuration: settings.AgentTurnExecutionTurnTimeout + settings.AgentTurnExecutionCleanupTimeout + time.Minute, MCPEndpoint: settings.MCPEndpointURL}
}

func NewBackend(ctx context.Context, settings config.Config, runtime profile.Profile, ledger execution.Ledger, source execution.StateMetadataSource) (result *Backend, err error) {
	result = &Backend{}
	defer func() {
		if err != nil {
			_ = result.Close()
		}
	}()
	switch settings.ExecutionBackend {
	case "microsandbox":
		native, err := microsandbox.New(MicrosandboxOptions(settings), ledger)
		if err != nil {
			return result, err
		}
		if err := native.Check(ctx); err != nil {
			return result, err
		}
		state, err := dirstate.New(settings.RuntimeStateRoot, native.Binding(), source)
		if err != nil {
			return result, err
		}
		result.Factory, result.Workspaces, result.Cleaner, result.Recovery, result.Images, result.Check = native, native, native, native, native, native.Check
		result.Snapshots = native
		result.States = state
		result.close = append(result.close, native.Close, state.Close)
	case "", "docker":
		workspaces, err := docker.NewWorkspaceSource()
		if err != nil {
			return result, err
		}
		result.Workspaces = workspaces
		result.Snapshots = workspaces
		result.close = append(result.close, workspaces.Close)
		cleaner, err := docker.NewExactLabelCleaner(docker.ExactLabelCleanerOptions{StopTimeout: settings.ShutdownTimeout, Ledger: ledger, DiagnosticNamespace: settings.RuntimeStateVolume})
		if err != nil {
			return result, err
		}
		result.Cleaner = cleaner
		result.close = append(result.close, cleaner.Close)
		inventory, err := docker.NewRuntimeProcessInventory(settings.RuntimeStateVolume)
		if err != nil {
			return result, err
		}
		result.Recovery = docker.NewRecovery(inventory, cleaner)
		result.close = append(result.close, inventory.Close)
		state, err := docker.NewRetainedState(settings.RuntimeStateRoot, settings.RuntimeStateVolume, source)
		if err != nil {
			return result, err
		}
		contract := runtime.Contract()
		state, err = state.WithCollector(docker.AssignmentRuntimeStateCleanerOptions{Image: contract.Image, Platform: docker.Platform{OS: contract.Platform.OS, Architecture: contract.Platform.Arch}, User: strconv.FormatUint(uint64(contract.User.UID), 10) + ":" + strconv.FormatUint(uint64(contract.User.GID), 10), RuntimeStateVolume: settings.RuntimeStateVolume})
		if err != nil {
			return result, err
		}
		result.States = state
		result.close = append(result.close, state.Close)
		result.Factory = docker.ExecutionFactory{Network: settings.DockerAgentNetwork, WorkspaceVolume: settings.WorkspaceVolume, RuntimeStateVolume: settings.RuntimeStateVolume, MiseVolume: settings.MiseVolume, States: state}
		images, err := docker.NewExactImageAvailability()
		if err != nil {
			return result, err
		}
		result.Images = images
		result.close = append(result.close, images.Close)
		probe, err := docker.NewReadinessProbe(docker.ReadinessProbeOptions{AgentNetwork: settings.DockerAgentNetwork, WorkspaceVolume: settings.WorkspaceVolume, RuntimeStateVolume: settings.RuntimeStateVolume, MiseVolume: settings.MiseVolume, AgentImage: contract.Image, RuntimeProfile: runtime})
		if err != nil {
			return result, err
		}
		result.Check = probe.Check
		result.close = append(result.close, probe.Close)
	default:
		return result, execution.ErrUnsupportedRequirement
	}
	return result, nil
}
