package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
)

type acpProbeOptions struct {
	Network     string
	MCPHost     string
	ExtraHosts  []string
	MemoryBytes int64
}

func checkACP(ctx context.Context, profile runtimeprofile.Profile, options acpProbeOptions) (err error) {
	contract := profile.Contract()
	return checkACPImage(ctx, profile, contract.Image, options)
}

func checkACPImage(ctx context.Context, profile runtimeprofile.Profile, image string, options acpProbeOptions) (err error) {
	diagnostic, err := startDiagnosticMCP(options.MCPHost)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, diagnostic.Close(cleanupCtx))
	}()
	process, engine, err := composition.StartDiagnostic(ctx, profile, image, composition.ProbeOptions{
		Network: options.Network, ExtraHosts: options.ExtraHosts, MemoryBytes: options.MemoryBytes,
	}, io.Discard)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close()) }()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, process.Stop(cleanupCtx, 5*time.Second), process.Remove(cleanupCtx))
	}()
	client := acp.NewClient(process.Transport(), acp.ClientOptions{
		RequiredCapabilities: acp.RequiredCapabilities{SessionList: true, SessionResume: true, SessionLoad: true},
	})
	defer func() { err = errors.Join(err, client.Close()) }()
	response, err := client.Initialize(ctx)
	if err != nil {
		return err
	}
	if response.AgentInfo == nil || response.AgentInfo.Name != "OpenCode" || response.AgentInfo.Version == "" {
		return fmt.Errorf("ACP agent identity is not OpenCode with a version")
	}
	if !response.AgentCapabilities.MCPCapabilities.HTTP {
		return errors.New("ACP agent does not advertise HTTP MCP support")
	}
	if _, err := client.CreateSession(ctx, acp.CreateSessionRequest{
		CWD: acp.WorkspacePath,
		MCPServers: []acp.MCPServer{{
			Type: "http", Name: "omnigrex-doctor", URL: diagnostic.URL(),
			Headers: []acp.EnvironmentEntry{{Name: "Authorization", Value: "Bearer " + diagnostic.Token()}},
		}},
	}); err != nil {
		return fmt.Errorf("create diagnostic ACP session: %w", err)
	}
	select {
	case <-diagnostic.Ready():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for OpenCode MCP initialization: %w", context.Cause(ctx))
	}
}
