// Package composition constructs the deployment's concrete runtime adapters.
package composition

import (
	"context"
	"io"

	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
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

func NewResourceProbe(options Resources) (ResourceProbe, error) {
	return docker.NewReadinessProbe(docker.ReadinessProbeOptions{
		AgentNetwork: options.AgentNetwork, WorkspaceVolume: options.WorkspaceVolume,
		RuntimeStateVolume: options.RuntimeStateVolume, MiseVolume: options.MiseVolume, AgentImage: options.AgentImage,
	})
}
func NewImageAvailability() (ImageAvailability, error) { return docker.NewExactImageAvailability() }
func CheckRepositoryEnvironment(ctx context.Context, profile profile.Profile) error {
	return docker.CheckRepositoryEnvironment(ctx, profile)
}
func StartDiagnostic(ctx context.Context, profile profile.Profile, image string, options ProbeOptions, stderr io.Writer) (execution.Process, io.Closer, error) {
	return docker.StartDiagnostic(ctx, profile, image, docker.DiagnosticOptions{Network: options.Network, ExtraHosts: options.ExtraHosts, MemoryBytes: options.MemoryBytes}, stderr)
}
