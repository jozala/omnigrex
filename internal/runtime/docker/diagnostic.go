package docker

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const diagnosticMarkerLabel = "io.omnigrex.diagnostic"
const diagnosticExpiryLabel = "io.omnigrex.diagnostic-expiry"

func diagnosticCleanupEligible(labels map[string]string, now time.Time) bool {
	if labels[diagnosticMarkerLabel] != "true" {
		return false
	}
	expiry, err := time.Parse(time.RFC3339Nano, labels[diagnosticExpiryLabel])
	return err != nil || !expiry.After(now)
}

type DiagnosticOptions struct {
	Network     string
	ExtraHosts  []string
	MemoryBytes int64
}

// StartDiagnostic uses ephemeral storage and a separate, expiring management identity.
func StartDiagnostic(ctx context.Context, profile profile.Profile, image string, options DiagnosticOptions, stderr io.Writer) (execution.Process, io.Closer, error) {
	policy, spec, err := diagnosticContract(profile, image, options)
	if err != nil {
		return nil, nil, err
	}
	engine, err := NewEngine(EngineOptions{AgentNetwork: options.Network, AllowHostGateway: len(options.ExtraHosts) > 0, RuntimePolicy: policy})
	if err != nil {
		return nil, nil, err
	}
	process, err := engine.Start(ctx, spec, stderr)
	if err != nil {
		_ = engine.Close()
		return nil, nil, err
	}
	return process, engine, nil
}

func diagnosticContract(profile profile.Profile, image string, options DiagnosticOptions) (RuntimePolicy, Spec, error) {
	contract := profile.Contract()
	if contract.Name == "" {
		return RuntimePolicy{}, Spec{}, errors.New("Runtime Profile contract check did not pass")
	}
	if options.MemoryBytes <= 0 {
		return RuntimePolicy{}, Spec{}, errors.New("diagnostic memory must be positive")
	}
	var environment []string
	environmentPolicy := make(map[string]string)
	for _, variable := range contract.Environment {
		environment = append(environment, variable.Name+"="+variable.Value)
		environmentPolicy[variable.Name] = variable.Value
	}
	var temporary []TmpfsMount
	for _, mount := range contract.Tmpfs {
		temporary = append(temporary, TmpfsMount{Target: mount.Path, SizeBytes: mount.SizeBytes, Executable: mount.Executable})
	}
	for _, mount := range contract.Mounts {
		temporary = append(temporary, TmpfsMount{Target: mount.Path, SizeBytes: 64 << 20})
	}
	platform := Platform{OS: contract.Platform.OS, Architecture: contract.Platform.Arch}
	user := strconv.FormatUint(uint64(contract.User.UID), 10) + ":" + strconv.FormatUint(uint64(contract.User.GID), 10)
	labels := map[string]string{diagnosticMarkerLabel: "true", diagnosticExpiryLabel: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)}
	policy := RuntimePolicy{
		Image: image, Platform: platform, User: user, WorkingDir: contract.Workspace, Command: contract.Command,
		Tmpfs: temporary, Environment: environmentPolicy, Network: options.Network, MemoryBytes: options.MemoryBytes, PIDsLimit: contract.PIDsLimit, Labels: labels,
	}
	spec := Spec{
		Name: "omnigrex-doctor-acp-" + strconv.FormatInt(time.Now().UnixNano(), 10), Image: image, Platform: platform,
		User: user, WorkingDir: contract.Workspace, Command: contract.Command, Environment: environment, Tmpfs: temporary,
		Network: options.Network, ExtraHosts: options.ExtraHosts, MemoryBytes: options.MemoryBytes, PIDsLimit: contract.PIDsLimit, Labels: labels,
	}
	return policy, spec, nil
}
