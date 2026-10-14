package doctor

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

type piProbeOptions struct {
	Settings    config.Config
	Network     string
	MCPHost     string
	ExtraHosts  []string
	MemoryBytes int64
}

// checkPi initializes the production runner and authenticated MCP extension
// without a model prompt, real credentials, or a retained-state attachment.
func checkPi(ctx context.Context, runtime profile.Profile, options piProbeOptions, selections ...agent.ModelSelection) (err error) {
	diagnostic, err := startDiagnosticMCP(options.MCPHost, map[string]any{"name": "get_issue", "description": "Disposable diagnostic tool", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, diagnostic.Close(cleanup))
	}()
	contract := runtime.Contract()
	spec := execution.DiagnosticSpec{Namespace: composition.StateBinding(options.Settings).Namespace, Image: contract.Image, Platform: execution.Platform{OS: contract.Platform.OS, Architecture: contract.Platform.Arch}, Command: contract.Command, MemoryBytes: min(options.MemoryBytes, 512<<20)}
	for _, variable := range contract.Environment {
		spec.Environment = append(spec.Environment, variable.Name+"="+variable.Value)
	}
	for _, mount := range contract.Mounts {
		spec.TemporaryDirectories = append(spec.TemporaryDirectories, execution.TemporaryDirectory{Target: mount.Path, SizeBytes: 64 << 20})
	}
	for _, mount := range contract.Tmpfs {
		// Diagnostic storage stays non-executable; no repository tools run here.
		spec.TemporaryDirectories = append(spec.TemporaryDirectories, execution.TemporaryDirectory{Target: mount.Path, SizeBytes: mount.SizeBytes})
	}
	process, engine, err := composition.StartProtocolDiagnostic(ctx, spec, composition.ProbeOptions{Network: options.Network, ExtraHosts: options.ExtraHosts}, io.Discard, options.Settings)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close()) }()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, process.Stop(cleanup, 5*time.Second), process.Remove(cleanup))
	}()
	selection := agent.ModelSelection{Model: "openai/gpt-4o-mini", Thinking: "off"}
	if len(selections) > 0 {
		selection = selections[0]
	}
	connection, err := composition.ConnectPiDiagnostic(ctx, process.Transport(), agent.MCPEndpoint{Name: "omnigrex", URL: diagnostic.URL(), Headers: []agent.Header{{Name: "Authorization", Value: "Bearer " + diagnostic.Token()}}}, selection)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, connection.Close()) }()
	_, err = connection.Prepare(ctx)
	return err
}
