package pi

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/turnconfig"
)

type TurnAdapter struct{ Store session.RuntimeBindingStore }

func (TurnAdapter) BuildProcess(p profile.Profile, key execution.Key, workflowID string, memory int64, tools map[string]string) (execution.Request, error) {
	c := p.Contract()
	if c.Name != "pi-rpc" || c.Version != "v1" || key.RuntimeProfileName != c.Name || key.RuntimeProfileVersion != c.Version || memory <= 0 {
		return execution.Request{}, execution.ErrInvalidSpec
	}
	values := map[string]string{}
	for _, entry := range c.Environment {
		// TMPDIR is the unconfigured fallback. Approved control-plane path
		// bindings take precedence while all other fixed values remain protected.
		if entry.Name == "TMPDIR" {
			if _, admitted := tools[entry.Name]; admitted {
				continue
			}
		}
		values[entry.Name] = entry.Value
	}
	for name, value := range tools {
		if !turnconfig.SafeVariable(name) || strings.ContainsRune(value, 0) {
			return execution.Request{}, execution.ErrInvalidSpec
		}
		if old, exists := values[name]; exists && old != value {
			return execution.Request{}, execution.ErrConflict
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	request := execution.Request{Key: key, WorkflowID: workflowID, Image: c.Image, Platform: execution.Platform{OS: c.Platform.OS, Architecture: c.Platform.Arch}, User: strconv.Itoa(int(c.User.UID)) + ":" + strconv.Itoa(int(c.User.GID)), WorkingDir: c.Workspace, Command: c.Command, MemoryBytes: memory, PIDsLimit: c.PIDsLimit, GoldenResourceContract: c.GoldenResourceContract}
	for _, name := range names {
		request.Environment = append(request.Environment, name+"="+values[name])
	}
	for _, mount := range c.Mounts {
		request.Attachments = append(request.Attachments, execution.Attachment{Resource: mount.Name, Target: mount.Path})
	}
	for _, tmp := range c.Tmpfs {
		request.TemporaryDirectories = append(request.TemporaryDirectories, execution.TemporaryDirectory{Target: tmp.Path, SizeBytes: tmp.SizeBytes, Executable: tmp.Executable})
	}
	return request, nil
}

func (adapter TurnAdapter) Prepare(ctx context.Context, request session.TurnPreparation) (session.TurnConnection, error) {
	policies, err := role.NewPolicyCatalog([]role.ID{request.Policy.Role}, []role.Policy{request.Policy})
	if err != nil {
		return nil, ErrConfiguration
	}
	p, err := agentprofile.DecodeSnapshot(request.Profile, policies)
	if err != nil {
		return nil, ErrConfiguration
	}
	if string(p.Name()) != request.ProfileName || p.Runtime() != request.Session.RuntimeProfileName+"/"+request.Session.RuntimeProfileVersion {
		return nil, ErrConfiguration
	}
	configuration, err := Render(p, request.Policy, request.Operator, request.Stage, request.Purpose, request.MCP, request.Credentials)
	if err != nil {
		return nil, err
	}
	coordinator := session.NewRuntimeCoordinator(adapter.Store)
	result, err := coordinator.Prepare(ctx, session.RuntimePrepareRequest{Lease: request.Lease, Session: request.Session, State: request.State, Connect: func(ctx context.Context, identity agent.SessionIdentity) (agent.Connection, error) {
		configuration.Session = &identity
		return Start(ctx, request.Transport, configuration, Options{CallerDeadlineOnly: true})
	}})
	if err != nil {
		return nil, err
	}
	return &turnConnection{coordinator: coordinator, connection: result.Connection}, nil
}

type turnConnection struct {
	coordinator *session.RuntimeCoordinator
	connection  agent.Connection
}

func (c *turnConnection) Prompt(ctx context.Context, lease store.AgentTurnLease, durable store.AgentSession, text string, sink agentevent.Sink) (agent.Outcome, error) {
	return c.coordinator.Prompt(ctx, session.RuntimePromptRequest{Lease: lease, Session: durable, Connection: c.connection, Text: text, Sink: sink})
}
func (c *turnConnection) Close() error { return c.connection.Close() }
