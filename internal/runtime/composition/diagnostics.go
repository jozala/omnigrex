// Package composition constructs the deployment's concrete runtime adapters.
package composition

import (
	"context"
	"io"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/microsandbox"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

type Resources struct{ AgentNetwork, WorkspaceVolume, RuntimeStateVolume, MiseVolume, AgentImage string }
type ResourceProbe interface {
	Inspect(context.Context) error
	Close() error
}
type ImageAvailability interface {
	Available(context.Context, string, profile.Platform) error
	Close() error
}
type ProbeOptions struct {
	Network     string
	ExtraHosts  []string
	MemoryBytes int64
}

func NewResourceProbe(options Resources, settings ...config.Config) (ResourceProbe, error) {
	if len(settings) > 0 && settings[0].ExecutionBackend == "microsandbox" {
		return microsandbox.New(MicrosandboxOptions(settings[0]), nil)
	}
	return docker.NewReadinessProbe(docker.ReadinessProbeOptions{
		AgentNetwork: options.AgentNetwork, WorkspaceVolume: options.WorkspaceVolume,
		RuntimeStateVolume: options.RuntimeStateVolume, MiseVolume: options.MiseVolume, AgentImage: options.AgentImage,
	})
}
func NewImageAvailability(settings ...config.Config) (ImageAvailability, error) {
	if len(settings) > 0 && settings[0].ExecutionBackend == "microsandbox" {
		return microsandbox.New(MicrosandboxOptions(settings[0]), nil)
	}
	return docker.NewExactImageAvailability()
}
func CheckRepositoryEnvironment(ctx context.Context, profile profile.Profile, settings ...config.Config) error {
	if len(settings) > 0 && settings[0].ExecutionBackend == "microsandbox" {
		backend, err := microsandbox.New(MicrosandboxOptions(settings[0]), nil)
		if err != nil {
			return err
		}
		return backend.CheckRepositoryEnvironment(ctx, profile)
	}
	return docker.CheckRepositoryEnvironment(ctx, profile)
}
func StartDiagnostic(ctx context.Context, profile profile.Profile, image string, options ProbeOptions, stderr io.Writer, settings ...config.Config) (execution.Process, io.Closer, error) {
	if len(settings) > 0 && settings[0].ExecutionBackend == "microsandbox" {
		backend, err := microsandbox.New(MicrosandboxOptions(settings[0]), nil)
		if err != nil {
			return nil, nil, err
		}
		process, err := backend.StartDiagnostic(ctx, profile, image, options.MemoryBytes, options.ExtraHosts, stderr)
		return process, backend, err
	}
	return docker.StartDiagnostic(ctx, profile, image, docker.DiagnosticOptions{Network: options.Network, ExtraHosts: options.ExtraHosts, MemoryBytes: options.MemoryBytes}, stderr)
}
