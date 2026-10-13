package docker

import (
	"context"
	"crypto/rand"
	"io"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

const diagnosticMarkerLabel = "io.omnigrex.diagnostic"
const diagnosticExpiryLabel = "io.omnigrex.diagnostic-expiry"
const diagnosticNamespaceLabel = "io.omnigrex.diagnostic-namespace"
const diagnosticProbeLabel = "io.omnigrex.diagnostic-probe"

// Namespaced protocol probes can only be inventoried/collected by their owner.
func diagnosticOwned(labels map[string]string, namespace string) bool {
	return namespace != "" && labels[diagnosticNamespaceLabel] == namespace
}

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

// StartProtocolDiagnostic starts a backend-neutral disposable protocol probe.
func StartProtocolDiagnostic(ctx context.Context, request execution.DiagnosticSpec, options DiagnosticOptions, stderr io.Writer) (execution.Process, io.Closer, error) {
	if err := request.Validate(); err != nil {
		return nil, nil, err
	}
	temporary := make([]TmpfsMount, 0, len(request.TemporaryDirectories))
	for _, directory := range request.TemporaryDirectories {
		temporary = append(temporary, TmpfsMount{Target: directory.Target, SizeBytes: directory.SizeBytes})
	}
	environment := map[string]string{}
	for _, entry := range request.Environment {
		name, value, _ := strings.Cut(entry, "=")
		environment[name] = value
	}
	platform := Platform{OS: request.Platform.OS, Architecture: request.Platform.Architecture}
	probe := strings.ToLower(rand.Text())
	labels := map[string]string{diagnosticMarkerLabel: "true", diagnosticExpiryLabel: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano), diagnosticNamespaceLabel: request.Namespace, diagnosticProbeLabel: probe}
	policy := RuntimePolicy{Image: request.Image, Platform: platform, User: "10001:10001", WorkingDir: "/workspace", Command: request.Command, Tmpfs: temporary, Environment: environment, Network: options.Network, MemoryBytes: request.MemoryBytes, PIDsLimit: 128, Labels: labels}
	engine, err := NewEngine(EngineOptions{AgentNetwork: options.Network, AllowHostGateway: len(options.ExtraHosts) > 0, RuntimePolicy: policy})
	if err != nil {
		return nil, nil, err
	}
	process, err := engine.Start(ctx, Spec{Name: "omnigrex-protocol-" + probe, Image: request.Image, Platform: platform, User: policy.User, WorkingDir: policy.WorkingDir, Command: request.Command, Environment: request.Environment, Tmpfs: temporary, Network: options.Network, ExtraHosts: options.ExtraHosts, MemoryBytes: request.MemoryBytes, PIDsLimit: 128, Labels: labels}, stderr)
	if err != nil {
		_ = engine.Close()
		return nil, nil, err
	}
	return process, engine, nil
}
