package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
)

type acpProbeOptions struct {
	Network    string
	MCPHost    string
	ExtraHosts []string
}

func checkACP(ctx context.Context, profile runtimeprofile.Profile, options acpProbeOptions) (err error) {
	contract := profile.Contract()
	return checkACPImage(ctx, profile, contract.Image, options)
}

func checkACPImage(ctx context.Context, profile runtimeprofile.Profile, image string, options acpProbeOptions) (err error) {
	contract := profile.Contract()
	if contract.Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	diagnostic, err := startDiagnosticMCP(options.MCPHost)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, diagnostic.Close(cleanupCtx))
	}()
	environment := make([]string, len(contract.Environment))
	environmentPolicy := make(map[string]string, len(contract.Environment))
	for index, variable := range contract.Environment {
		environment[index] = variable.Name + "=" + variable.Value
		environmentPolicy[variable.Name] = variable.Value
	}
	temporary := make([]dockerruntime.TmpfsMount, 0, len(contract.Tmpfs)+len(contract.Mounts))
	for _, mount := range contract.Tmpfs {
		temporary = append(temporary, dockerruntime.TmpfsMount{
			Target: mount.Path, SizeBytes: mount.SizeBytes, Executable: mount.Executable,
		})
	}
	for _, mount := range contract.Mounts {
		temporary = append(temporary, dockerruntime.TmpfsMount{Target: mount.Path, SizeBytes: 64 << 20})
	}
	platform := dockerruntime.Platform{OS: contract.Platform.OS, Architecture: contract.Platform.Arch}
	user := strconv.FormatUint(uint64(contract.User.UID), 10) + ":" + strconv.FormatUint(uint64(contract.User.GID), 10)
	policy := dockerruntime.RuntimePolicy{
		Image: image, Platform: platform, User: user, WorkingDir: contract.Workspace,
		Command: contract.Command, Tmpfs: temporary, Environment: environmentPolicy, Network: options.Network,
		MemoryBytes: contract.MemoryBytes, PIDsLimit: contract.PIDsLimit,
	}
	engine, err := dockerruntime.NewEngine(dockerruntime.EngineOptions{
		AgentNetwork: options.Network, AllowHostGateway: len(options.ExtraHosts) > 0, RuntimePolicy: policy,
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close()) }()
	process, err := engine.Start(ctx, dockerruntime.Spec{
		Name:  "omnigrex-doctor-acp-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Image: image, Platform: platform, User: user, WorkingDir: contract.Workspace,
		Command: contract.Command, Environment: environment, Tmpfs: temporary, Network: options.Network,
		ExtraHosts:  options.ExtraHosts,
		MemoryBytes: contract.MemoryBytes, PIDsLimit: contract.PIDsLimit,
	}, io.Discard)
	if err != nil {
		return err
	}
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
