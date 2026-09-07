package agentturn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"

	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
)

var (
	ErrInvalidLauncher      = errors.New("invalid Runtime Process Launcher")
	ErrInvalidLaunchRequest = errors.New("invalid Runtime Process launch request")
	ErrRuntimeBinding       = errors.New("Runtime Process binding mismatch")
)

const (
	runtimeMisePath       = "/home/opencode/.local/share/mise"
	defaultCleanupTimeout = 10 * time.Second
	defaultStopTimeout    = 10 * time.Second
)

// LauncherStore supplies launch context only under an already-acquired Agent Turn fence.
type LauncherStore interface {
	ValidateTurnFence(context.Context, store.AgentTurnLease) error
	RefreshAgentTurnLease(context.Context, store.AgentTurnLease, time.Duration) (store.AgentTurnLease, error)
	GetAgentTurnExecutionContext(context.Context, store.AgentTurnLease) (store.AgentTurnExecutionContext, error)
	WithAgentTurnFence(context.Context, store.AgentTurnLease, func(context.Context) error) error
}

// RuntimeWorkspace prepares assignment-isolated checkout and trusted tool state.
type RuntimeWorkspace interface {
	PrepareWorkspace(context.Context, workspace.Checkout) (workspace.Paths, error)
	ProvisionMise(context.Context, workspace.MiseProvision) (workspace.MiseActivation, error)
	DiscardWorkspace(string) error
}

// MCPRegistrar issues, drains, and revokes exact per-turn MCP authority.
type MCPRegistrar interface {
	Register(mcp.TokenScope) (mcp.Registration, error)
	CloseAndDrain(context.Context, mcp.Registration) error
	Revoke(mcp.Registration) bool
}

// RuntimeProcess is the restricted process resource owned by a launch.
type RuntimeProcess interface {
	Start(context.Context) error
	Transport() io.ReadWriteCloser
	Stop(context.Context, time.Duration) error
	Remove(context.Context) error
}

// RuntimeEngine creates one stopped process under a compiled Runtime Policy.
type RuntimeEngine interface {
	Create(context.Context, dockerruntime.Spec, io.Writer) (RuntimeProcess, error)
	Close() error
}

// RuntimeEngineFactory binds an engine to the exact policy compiled for a launch.
type RuntimeEngineFactory interface {
	New(dockerruntime.EngineOptions) (RuntimeEngine, error)
}

// RuntimeBindingResolver resolves the complete immutable binding persisted for a generation.
type RuntimeBindingResolver interface {
	ResolveBinding(runtimeprofile.Binding) (runtimeprofile.Profile, error)
}

// RuntimeACPClient is the ACP capability retained for Phase 8 after launch preparation.
type RuntimeACPClient interface {
	session.AgentClient
	session.PromptClient
	Close() error
}

// RuntimeACPFactory attaches ACP to a Runtime Process transport.
type RuntimeACPFactory interface {
	New(io.ReadWriteCloser, acp.ClientOptions) RuntimeACPClient
}

// RuntimeSessionPreparer creates or continues and configures a Session without prompting it.
type RuntimeSessionPreparer interface {
	Prepare(context.Context, session.PrepareRequest) (session.Result, error)
}

var (
	_ LauncherStore          = (*store.Store)(nil)
	_ RuntimeWorkspace       = (*workspace.Lifecycle)(nil)
	_ MCPRegistrar           = (*mcp.Gateway)(nil)
	_ RuntimeACPClient       = (*acp.Client)(nil)
	_ RuntimeSessionPreparer = (*session.Coordinator)(nil)
)

// LauncherConfig supplies stable deployment dependencies and named Docker resources.
type LauncherConfig struct {
	Store              LauncherStore
	Registry           RuntimeBindingResolver
	Workspace          RuntimeWorkspace
	Gateway            MCPRegistrar
	Docker             RuntimeEngineFactory
	ACP                RuntimeACPFactory
	Sessions           RuntimeSessionPreparer
	Network            string
	WorkspaceVolume    string
	RuntimeStateVolume string
	MiseVolume         string
	ACPOptions         acp.ClientOptions
	StopTimeout        time.Duration
}

// LaunchRequest contains per-turn credentials and exact repository revisions.
type LaunchRequest struct {
	Lease                  store.AgentTurnLease
	LeaseDuration          time.Duration
	RepositoryURL          string
	DefaultBranchName      string
	DefaultBranchSHA       string
	InitialFeatureBranch   string
	RepositoryCredential   string
	ProviderCredentialJSON json.RawMessage
	Stderr                 io.Writer
}

func (LaunchRequest) String() string   { return "Agent Turn launch request" }
func (LaunchRequest) GoString() string { return "agentturn.LaunchRequest{<credentials redacted>}" }

// Launcher composes one epoch-fenced Runtime Process and Agent Session attachment.
type Launcher struct {
	store              LauncherStore
	registry           RuntimeBindingResolver
	workspace          RuntimeWorkspace
	gateway            MCPRegistrar
	docker             RuntimeEngineFactory
	acp                RuntimeACPFactory
	sessions           RuntimeSessionPreparer
	network            string
	workspaceVolume    string
	runtimeStateVolume string
	miseVolume         string
	acpOptions         acp.ClientOptions
	stopTimeout        time.Duration
}

// RuntimeHandle retains the attached ACP client and owns deterministic launch cleanup.
type RuntimeHandle struct {
	Client  RuntimeACPClient
	Session session.Result
	lease   store.AgentTurnLease

	gateway            MCPRegistrar
	registration       mcp.Registration
	registered         bool
	workspace          RuntimeWorkspace
	assignmentID       string
	engine             RuntimeEngine
	process            RuntimeProcess
	stopTimeout        time.Duration
	secrets            []string
	mcpMutex           sync.Mutex
	mcpDrained         bool
	cleanupMutex       sync.Mutex
	acpClosed          bool
	processStopped     bool
	processRemoved     bool
	workspaceDiscarded bool
	engineClosed       bool
}

// NewLauncher validates deployment dependencies before any turn is launched.
func NewLauncher(config LauncherConfig) (*Launcher, error) {
	if nilInterface(config.Store) || nilInterface(config.Registry) || nilInterface(config.Workspace) ||
		nilInterface(config.Gateway) || nilInterface(config.Docker) || nilInterface(config.ACP) || nilInterface(config.Sessions) {
		return nil, fmt.Errorf("%w: dependency is nil", ErrInvalidLauncher)
	}
	for _, field := range []struct{ name, value string }{
		{name: "network", value: config.Network},
		{name: "workspace volume", value: config.WorkspaceVolume},
		{name: "runtime state volume", value: config.RuntimeStateVolume},
		{name: "mise volume", value: config.MiseVolume},
	} {
		if strings.TrimSpace(field.value) == "" || strings.TrimSpace(field.value) != field.value {
			return nil, fmt.Errorf("%w: %s", ErrInvalidLauncher, field.name)
		}
	}
	if config.StopTimeout < 0 {
		return nil, fmt.Errorf("%w: stop timeout", ErrInvalidLauncher)
	}
	if config.StopTimeout == 0 {
		config.StopTimeout = defaultStopTimeout
	}
	return &Launcher{
		store: config.Store, registry: config.Registry, workspace: config.Workspace,
		gateway: config.Gateway, docker: config.Docker, acp: config.ACP, sessions: config.Sessions,
		network: config.Network, workspaceVolume: config.WorkspaceVolume,
		runtimeStateVolume: config.RuntimeStateVolume, miseVolume: config.MiseVolume,
		acpOptions: config.ACPOptions, stopTimeout: config.StopTimeout,
	}, nil
}

// Launch starts and attaches one Runtime Process without submitting an ACP prompt.
// ValidateTurnFence is always its first dependency action.
func (launcher *Launcher) Launch(ctx context.Context, request LaunchRequest) (handle *RuntimeHandle, err error) {
	secrets := launchSecrets(request)
	defer func() {
		if err != nil {
			err = sanitizeLaunchError(err, secrets)
		}
	}()
	if launcher == nil {
		return nil, ErrInvalidLauncher
	}
	if err := validateLaunchRequest(request); err != nil {
		return nil, err
	}
	if err := launcher.store.ValidateTurnFence(ctx, request.Lease); err != nil {
		return nil, fmt.Errorf("validate Agent Turn fence before launch: %w", err)
	}
	execution, err := launcher.store.GetAgentTurnExecutionContext(ctx, request.Lease)
	if err != nil {
		return nil, fmt.Errorf("get fenced Agent Turn execution context: %w", err)
	}
	if err := validateExecutionBinding(execution, request.Lease); err != nil {
		return nil, err
	}

	runtimeProfile, err := launcher.registry.ResolveBinding(runtimeprofile.Binding{
		Name: execution.Assignment.RuntimeProfileName, Version: execution.Assignment.RuntimeProfileVersion,
		ContentSHA256: execution.Assignment.RuntimeProfileContentSHA256,
		Image:         execution.Assignment.RuntimeImageDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve assigned Runtime Profile: %w", err)
	}
	if err := validateRuntimeProfileBinding(runtimeProfile, execution); err != nil {
		return nil, err
	}
	rendered, role, err := renderTurnProfile(execution)
	if err != nil {
		return nil, err
	}
	revision, branch, headSHA, pullRequest, err := checkoutSelection(execution, request)
	if err != nil {
		return nil, err
	}

	paths, err := launcher.workspace.PrepareWorkspace(ctx, workspace.Checkout{
		AssignmentID: execution.Assignment.ID, RepositoryURL: request.RepositoryURL,
		Credential: request.RepositoryCredential, Revision: revision,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare isolated Agent workspace: %w", err)
	}
	resources := &RuntimeHandle{
		gateway: launcher.gateway, stopTimeout: launcher.stopTimeout, lease: request.Lease,
		secrets: append([]string(nil), secrets...),
	}
	if execution.Assignment.Role == workflow.RoleReviewer {
		resources.workspace = launcher.workspace
		resources.assignmentID = execution.Assignment.ID
	}
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCleanupTimeout)
		defer cancel()
		cleanupErr := resources.Cleanup(cleanupCtx)
		err = errors.Join(err, cleanupErr)
		if cleanupErr != nil {
			handle = resources
		} else {
			handle = nil
		}
	}()
	miseRevision := revision
	if execution.Assignment.Role == workflow.RoleReviewer {
		miseRevision = request.DefaultBranchSHA
	}
	activation, err := launcher.workspace.ProvisionMise(ctx, workspace.MiseProvision{
		AssignmentID: execution.Assignment.ID, RepositoryURL: request.RepositoryURL,
		Credential: request.RepositoryCredential, Revision: miseRevision,
	})
	if err != nil {
		return nil, fmt.Errorf("provision trusted repository tools: %w", err)
	}
	if paths.Workspace == "" || paths.Mise == "" || activation.DataDir != paths.Mise || activation.SourceRevision != miseRevision {
		return nil, fmt.Errorf("%w: trusted mise source", ErrRuntimeBinding)
	}
	environment, err := runtimeMiseEnvironment(activation)
	if err != nil {
		return nil, err
	}
	refreshedLease, err := launcher.store.RefreshAgentTurnLease(ctx, request.Lease, request.LeaseDuration)
	if err != nil {
		return nil, fmt.Errorf("refresh Agent Turn lease before Runtime Process launch: %w", err)
	}
	request.Lease = refreshedLease
	resources.lease = refreshedLease

	registration, err := launcher.gateway.Register(mcp.TokenScope{
		Lease: request.Lease, WorkflowID: execution.WorkflowID, Role: execution.Assignment.Role,
		Repository: mcp.RepositoryScope{ID: execution.Repository.ID, Owner: execution.Repository.Owner, Name: execution.Repository.Name},
		Issue:      mcp.IssueScope{ID: execution.Issue.ID, Number: execution.Issue.Number}, PullRequest: pullRequest,
		Branch: branch, DefaultBranch: request.DefaultBranchName, HeadSHA: headSHA, ExpiresAt: request.Lease.LeaseExpiresAt,
	})
	resources.registration = registration
	secrets = append(secrets, registrationSecrets(registration)...)
	resources.secrets = append(resources.secrets, registrationSecrets(registration)...)
	if err != nil {
		launcher.gateway.Revoke(registration)
		return nil, fmt.Errorf("register per-turn MCP authority: %w", err)
	}
	resources.registered = true

	assignmentRoot := "assignment-" + execution.Assignment.ID
	statePath := assignmentRoot + "/runtime-state"
	if execution.Assignment.RuntimeStatePath != statePath || execution.Session.RuntimeStatePath != statePath {
		return nil, fmt.Errorf("%w: runtime state path", ErrRuntimeBinding)
	}
	policy, spec, err := opencode.BuildProcess(runtimeProfile, rendered, opencode.ProviderCredentials{
		Role: role, Content: append([]byte(nil), request.ProviderCredentialJSON...),
	}, opencode.ProcessOptions{
		Name:    "omnigrex-turn-" + execution.Turn.ID + "-epoch-" + strconv.FormatInt(execution.Turn.ExecutionEpoch, 10),
		Network: launcher.network, AssignmentID: execution.Assignment.ID, AgentSessionID: execution.Session.ID,
		AgentTurnID: execution.Turn.ID, ExecutionEpoch: uint64(execution.Turn.ExecutionEpoch),
		VolumeBindings:     map[string]string{"workspace": launcher.workspaceVolume, "state": launcher.runtimeStateVolume, "mise": launcher.miseVolume},
		AssignmentSubpaths: map[string]string{"workspace": assignmentRoot + "/workspace", "state": statePath, "mise": assignmentRoot + "/mise"},
		Environment:        environment, Labels: map[string]string{"io.omnigrex.workflow": execution.WorkflowID},
	})
	if err != nil {
		return nil, fmt.Errorf("compile Runtime Process contract: %w", err)
	}
	engine, err := launcher.docker.New(dockerruntime.EngineOptions{AgentNetwork: launcher.network, RuntimePolicy: policy})
	if !nilInterface(engine) {
		resources.engine = engine
	}
	if err != nil {
		return nil, fmt.Errorf("create restricted Docker Engine: %w", err)
	}
	if nilInterface(engine) {
		return nil, errors.New("create restricted Docker Engine: factory returned nil")
	}
	var process RuntimeProcess
	err = launcher.store.WithAgentTurnFence(ctx, request.Lease, func(fenceCtx context.Context) error {
		created, createErr := engine.Create(fenceCtx, spec, request.Stderr)
		process = created
		if !nilInterface(created) {
			resources.process = created
		}
		if createErr != nil {
			return fmt.Errorf("create restricted Runtime Process: %w", createErr)
		}
		if nilInterface(created) {
			return errors.New("create restricted Runtime Process: engine returned no process")
		}
		if startErr := created.Start(fenceCtx); startErr != nil {
			return fmt.Errorf("start restricted Runtime Process: %w", startErr)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create Runtime Process under Agent Turn fence: %w", err)
	}
	if err := launcher.store.ValidateTurnFence(ctx, request.Lease); err != nil {
		return nil, fmt.Errorf("validate Agent Turn fence after Runtime Process start: %w", err)
	}
	transport := process.Transport()
	if transport == nil {
		return nil, errors.New("start restricted Runtime Process: engine returned no transport")
	}
	clientOptions := launcher.acpOptions
	clientOptions.RequiredCapabilities = requiredACPCapabilities(runtimeProfile)
	// The rendered profile is the sole ACP permission authority for this assignment.
	clientOptions.DecidePermission = func(_ context.Context, request acp.PermissionRequest) acp.PermissionDecision {
		return rendered.DecidePermission(request)
	}
	client := launcher.acp.New(transport, clientOptions)
	if nilInterface(client) {
		return nil, errors.New("attach ACP client: factory returned nil")
	}
	resources.Client = client
	prepared, err := launcher.sessions.Prepare(ctx, session.PrepareRequest{
		Lease: request.Lease, Session: execution.Session, Client: client, StatePath: statePath,
		MCPServers: []acp.MCPServer{registration.Server}, Configuration: rendered.SessionConfiguration(),
	})
	if err != nil {
		return nil, fmt.Errorf("prepare Agent Session: %w", err)
	}
	resources.Session = prepared
	return resources, nil
}

// LaunchExecution exposes Launch through the execution worker's narrow runtime boundary.
func (launcher *Launcher) LaunchExecution(ctx context.Context, request LaunchRequest) (ExecutionRuntime, error) {
	handle, err := launcher.Launch(ctx, request)
	if handle == nil {
		return nil, err
	}
	return handle, err
}

// CloseMCP revokes MCP authority and waits for admitted mutations to reach durable completion.
func (handle *RuntimeHandle) CloseMCP(ctx context.Context) (err error) {
	if handle == nil {
		return nil
	}
	defer func() {
		if err != nil {
			err = sanitizeLaunchError(err, handle.secrets)
		}
	}()
	handle.mcpMutex.Lock()
	defer handle.mcpMutex.Unlock()
	if handle.mcpDrained || handle.gateway == nil || !handle.registered {
		return nil
	}
	drainErr := handle.gateway.CloseAndDrain(ctx, handle.registration)
	if drainErr == nil || errors.Is(drainErr, mcp.ErrMutationDrainUnresolved) {
		handle.mcpDrained = true
	}
	return drainErr
}

// CurrentLease returns the lease whose expiration was refreshed immediately before MCP registration.
func (handle *RuntimeHandle) CurrentLease() store.AgentTurnLease {
	if handle == nil {
		return store.AgentTurnLease{}
	}
	return handle.lease
}

// PromptClient exposes only the prompt capability retained by the execution worker.
func (handle *RuntimeHandle) PromptClient() session.PromptClient {
	if handle == nil {
		return nil
	}
	return handle.Client
}

// Cleanup drains MCP authority, closes ACP, stops and removes the process, discards Reviewer changes, and closes Docker resources.
func (handle *RuntimeHandle) Cleanup(ctx context.Context) (err error) {
	if handle == nil {
		return nil
	}
	defer func() {
		if err != nil {
			err = sanitizeLaunchError(err, handle.secrets)
		}
	}()
	handle.cleanupMutex.Lock()
	defer handle.cleanupMutex.Unlock()

	var cleanupErrors []error
	if err := handle.CloseMCP(ctx); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close and drain MCP authority: %w", err))
	}
	if handle.Client != nil && !handle.acpClosed {
		if err := handle.Client.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("close ACP client: %w", err))
		} else {
			handle.acpClosed = true
			handle.Client = nil
		}
	}
	if handle.process != nil && !handle.processRemoved {
		if !handle.processStopped {
			stopCtx, cancelStop := handle.teardownContext(ctx)
			err := handle.process.Stop(stopCtx, handle.stopTimeout)
			cancelStop()
			if err != nil {
				cleanupErrors = append(cleanupErrors, err)
			} else {
				handle.processStopped = true
			}
		}
		removeCtx, cancelRemove := handle.teardownContext(ctx)
		err := handle.process.Remove(removeCtx)
		cancelRemove()
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
		} else {
			handle.processRemoved = true
		}
	}
	processRemoved := handle.process == nil || handle.processRemoved
	if processRemoved && handle.workspace != nil && !handle.workspaceDiscarded {
		if err := handle.workspace.DiscardWorkspace(handle.assignmentID); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("discard Reviewer workspace: %w", err))
		} else {
			handle.workspaceDiscarded = true
		}
	}
	if processRemoved && handle.engine != nil && !handle.engineClosed {
		if err := handle.engine.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("close Docker Engine: %w", err))
		} else {
			handle.engineClosed = true
		}
	}
	return errors.Join(cleanupErrors...)
}

func (handle *RuntimeHandle) teardownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), handle.stopTimeout)
}

func validateLaunchRequest(request LaunchRequest) error {
	if request.Lease.ID == "" || request.Lease.ExecutionEpoch <= 0 || request.Lease.OwnerID == "" || request.Lease.OwnerToken == "" ||
		request.Lease.LeaseExpiresAt.IsZero() || request.LeaseDuration < time.Microsecond || request.LeaseDuration > maximumWorkerDuration ||
		strings.TrimSpace(request.RepositoryURL) == "" ||
		strings.TrimSpace(request.DefaultBranchName) == "" || strings.TrimSpace(request.DefaultBranchName) != request.DefaultBranchName ||
		strings.TrimSpace(request.DefaultBranchSHA) == "" ||
		strings.TrimSpace(request.RepositoryCredential) == "" || len(request.ProviderCredentialJSON) == 0 {
		return ErrInvalidLaunchRequest
	}
	return nil
}

func validateExecutionBinding(execution store.AgentTurnExecutionContext, lease store.AgentTurnLease) error {
	if execution.WorkflowID == "" || execution.WorkflowID != lease.JobLease.WorkflowID ||
		execution.Assignment.ID != lease.AgentAssignmentID || execution.Assignment.WorkflowID != execution.WorkflowID ||
		execution.Session.ID != lease.AgentSessionID || execution.Session.AgentAssignmentID != execution.Assignment.ID ||
		execution.Turn.ID != lease.ID || execution.Turn.AgentAssignmentID != lease.AgentAssignmentID ||
		execution.Turn.AgentSessionID != lease.AgentSessionID || execution.Turn.ExecutionEpoch != lease.ExecutionEpoch ||
		execution.Turn.ControlRevision != lease.ControlRevision || execution.Session.ControlRevision != lease.ControlRevision ||
		execution.Session.NextExecutionEpoch-1 != lease.ExecutionEpoch || execution.Repository.ID <= 0 ||
		strings.TrimSpace(execution.Repository.Owner) == "" || strings.TrimSpace(execution.Repository.Name) == "" ||
		execution.Issue.ID <= 0 || execution.Issue.Number <= 0 {
		return ErrRuntimeBinding
	}
	if execution.ChangeProposal == nil {
		if lease.ChangeProposalID != "" || lease.ExpectedHeadSHA != "" {
			return ErrRuntimeBinding
		}
		return nil
	}
	proposal := execution.ChangeProposal
	if proposal.ID == "" || proposal.ID != lease.ChangeProposalID || proposal.PullRequestID <= 0 || proposal.PullRequestNumber <= 0 ||
		proposal.HeadSHA == "" || proposal.HeadSHA != lease.ExpectedHeadSHA || proposal.HeadRef == "" {
		return ErrRuntimeBinding
	}
	return nil
}

func validateRuntimeProfileBinding(runtimeProfile runtimeprofile.Profile, execution store.AgentTurnExecutionContext) error {
	contract := runtimeProfile.Contract()
	binding := execution.Assignment.AssignmentRuntimeBinding
	if contract.Name != binding.RuntimeProfileName || contract.Version != binding.RuntimeProfileVersion ||
		runtimeProfile.ContentSHA256() != binding.RuntimeProfileContentSHA256 || contract.Image != binding.RuntimeImageDigest ||
		execution.Session.RuntimeProfileName != binding.RuntimeProfileName ||
		execution.Session.RuntimeProfileVersion != binding.RuntimeProfileVersion ||
		execution.Session.RuntimeProfileContentSHA256 != binding.RuntimeProfileContentSHA256 ||
		execution.Session.RuntimeImageDigest != binding.RuntimeImageDigest {
		return ErrRuntimeBinding
	}
	return nil
}

func renderTurnProfile(execution store.AgentTurnExecutionContext) (*opencode.RenderedProfile, opencode.Role, error) {
	var snapshot struct {
		Name         string            `json:"name"`
		Path         string            `json:"path"`
		Role         workflow.Role     `json:"role"`
		Runtime      string            `json:"runtime"`
		Model        string            `json:"model"`
		Variant      string            `json:"variant,omitempty"`
		Steps        uint              `json:"steps"`
		Permissions  map[string]string `json:"permissions"`
		Instructions string            `json:"instructions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(execution.Turn.AgentProfileConfig))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, "", fmt.Errorf("%w: Agent Profile snapshot", ErrRuntimeBinding)
	}
	role := opencode.RoleDeveloper
	wantPath := ".omnigrex/team/developer.md"
	if execution.Assignment.Role == workflow.RoleReviewer {
		role = opencode.RoleReviewer
		wantPath = ".omnigrex/team/reviewer.md"
	}
	if snapshot.Name != execution.Assignment.AgentProfileName || snapshot.Path != wantPath || snapshot.Role != execution.Assignment.Role ||
		snapshot.Runtime != execution.Assignment.RuntimeProfileName+"/"+execution.Assignment.RuntimeProfileVersion {
		return nil, "", ErrRuntimeBinding
	}
	permissions := make(opencode.PermissionPolicy, len(snapshot.Permissions))
	for name, value := range snapshot.Permissions {
		permissions[name] = opencode.Permission(value)
	}
	capabilities, err := mcp.CapabilitiesForRole(execution.Assignment.Role)
	if err != nil {
		return nil, "", ErrRuntimeBinding
	}
	runtimeTools := make([]string, len(capabilities))
	for index, capability := range capabilities {
		runtimeTools[index] = mcp.ServerName + "_" + capability
	}
	rendered, err := opencode.Render(role, opencode.Profile{
		Instructions: snapshot.Instructions, Model: snapshot.Model, Variant: snapshot.Variant,
		Steps: snapshot.Steps, Permissions: permissions, RuntimeTools: runtimeTools,
	})
	if err != nil {
		return nil, "", fmt.Errorf("render assigned Agent Profile: %w", err)
	}
	return rendered, role, nil
}

func checkoutSelection(execution store.AgentTurnExecutionContext, request LaunchRequest) (string, string, string, *mcp.PullRequestScope, error) {
	if execution.ChangeProposal == nil {
		if execution.Assignment.Role != workflow.RoleDeveloper {
			return "", "", "", nil, fmt.Errorf("%w: Reviewer requires a Change Proposal", ErrRuntimeBinding)
		}
		if strings.TrimSpace(request.InitialFeatureBranch) == "" {
			return "", "", "", nil, fmt.Errorf("%w: initial Developer branch", ErrRuntimeBinding)
		}
		return request.DefaultBranchSHA, request.InitialFeatureBranch, request.DefaultBranchSHA, nil, nil
	}
	proposal := execution.ChangeProposal
	if proposal.BaseRef != request.DefaultBranchName {
		return "", "", "", nil, fmt.Errorf("%w: Pull Request base branch", ErrRuntimeBinding)
	}
	return proposal.HeadSHA, proposal.HeadRef, proposal.HeadSHA,
		&mcp.PullRequestScope{ID: proposal.PullRequestID, Number: proposal.PullRequestNumber}, nil
}

func runtimeMiseEnvironment(activation workspace.MiseActivation) (map[string]string, error) {
	if activation.DataDir == "" || !filepath.IsAbs(activation.DataDir) || filepath.Clean(activation.DataDir) != activation.DataDir {
		return nil, fmt.Errorf("%w: mise data directory", ErrRuntimeBinding)
	}
	environment := make(map[string]string, len(activation.Environment))
	for name, value := range activation.Environment {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%w: mise environment", ErrRuntimeBinding)
		}
		environment[name] = strings.ReplaceAll(value, activation.DataDir, runtimeMisePath)
	}
	return environment, nil
}

func requiredACPCapabilities(runtimeProfile runtimeprofile.Profile) acp.RequiredCapabilities {
	var required acp.RequiredCapabilities
	for _, capability := range runtimeProfile.Contract().Capabilities {
		switch capability {
		case "session/list":
			required.SessionList = true
		case "session/resume":
			required.SessionResume = true
		case "session/load":
			required.SessionLoad = true
		}
	}
	return required
}

func launchSecrets(request LaunchRequest) []string {
	secrets := []string{request.RepositoryCredential, string(request.ProviderCredentialJSON)}
	var decoded any
	if json.Unmarshal(request.ProviderCredentialJSON, &decoded) == nil {
		collectStringSecrets(decoded, &secrets)
	}
	return secrets
}

func collectStringSecrets(value any, secrets *[]string) {
	switch typed := value.(type) {
	case string:
		*secrets = append(*secrets, typed)
	case []any:
		for _, item := range typed {
			collectStringSecrets(item, secrets)
		}
	case map[string]any:
		for _, item := range typed {
			collectStringSecrets(item, secrets)
		}
	}
}

func registrationSecrets(registration mcp.Registration) []string {
	secrets := make([]string, 0, len(registration.Server.Headers)*2)
	for _, header := range registration.Server.Headers {
		secrets = append(secrets, header.Value)
		if token, found := strings.CutPrefix(header.Value, "Bearer "); found {
			secrets = append(secrets, token)
		}
	}
	return secrets
}

type safeLaunchError struct {
	message string
	cause   error
}

func (err safeLaunchError) Error() string        { return err.message }
func (err safeLaunchError) Is(target error) bool { return errors.Is(err.cause, target) }

func sanitizeLaunchError(err error, secrets []string) error {
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return safeLaunchError{message: message, cause: err}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// ProductionDockerFactory creates Docker engines constrained by the launcher's compiled policy.
type ProductionDockerFactory struct{}

func (ProductionDockerFactory) New(options dockerruntime.EngineOptions) (RuntimeEngine, error) {
	engine, err := dockerruntime.NewEngine(options)
	if err != nil {
		return nil, err
	}
	return productionDockerEngine{engine: engine}, nil
}

type productionDockerEngine struct{ engine *dockerruntime.Engine }

func (engine productionDockerEngine) Create(ctx context.Context, spec dockerruntime.Spec, stderr io.Writer) (RuntimeProcess, error) {
	return engine.engine.Create(ctx, spec, stderr)
}

func (engine productionDockerEngine) Close() error { return engine.engine.Close() }

// ProductionACPFactory creates the protocol client attached to Docker's stream transport.
type ProductionACPFactory struct{}

func (ProductionACPFactory) New(transport io.ReadWriteCloser, options acp.ClientOptions) RuntimeACPClient {
	return acp.NewClient(transport, options)
}
