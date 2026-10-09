package agentturn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/turnconfig"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

var (
	ErrInvalidLauncher      = errors.New("invalid Runtime Process Launcher")
	ErrInvalidLaunchRequest = errors.New("invalid Runtime Process launch request")
	ErrRuntimeBinding       = errors.New("Runtime Process binding mismatch")
)

const (
	defaultCleanupTimeout = 10 * time.Second
	defaultStopTimeout    = 10 * time.Second
)

// LauncherStore supplies launch context only under an already-acquired Agent Turn fence.
type LauncherStore interface {
	AgentTurnState(context.Context, store.AgentTurnLease) (execution.StateRef, error)
	MarkAgentTurnStateInitialized(context.Context, store.AgentTurnLease, execution.StateRef) error
	ExecutionLedger(store.AgentTurnLease) execution.Ledger
	ValidateTurnFence(context.Context, store.AgentTurnLease) error
	RefreshAgentTurnLease(context.Context, store.AgentTurnLease, time.Duration) (store.AgentTurnLease, error)
	GetAgentTurnExecutionContext(context.Context, store.AgentTurnLease) (store.AgentTurnExecutionContext, error)
	WithAgentTurnFence(context.Context, store.AgentTurnLease, func(context.Context) error) error
	WithAgentTurnCleanupFence(context.Context, store.AgentTurnLease, func(context.Context) error) error
	RecordAgentTurnRuntimeOOM(context.Context, store.AgentTurnLease, string, int) error
	RecordAgentTurnUnpromptedCleanup(context.Context, store.AgentTurnLease) error
}

// RuntimeWorkspace prepares assignment-isolated checkout and trusted tool state.
type RuntimeWorkspace interface {
	Prepare(context.Context, workspace.Preparation) (execution.WorkspaceRef, error)
	Capture(context.Context, execution.CaptureRequest) (execution.CaptureRef, error)
	PrepareTurnPaths(context.Context, string, string, turnconfig.Configuration, workspace.WorkspaceFence) (map[string]string, error)
	CleanupTurnPaths(context.Context, string, string, workspace.WorkspaceFence) error
	Discard(context.Context, execution.WorkspaceRef, workspace.WorkspaceFence) error
}

// MCPRegistrar issues, drains, and revokes exact per-turn MCP authority.
type MCPRegistrar interface {
	Register(mcp.TokenScope) (mcp.Registration, error)
	Renew(mcp.Registration, time.Time) bool
	CloseAndDrain(context.Context, mcp.Registration) error
	Revoke(mcp.Registration) bool
}

// RuntimeProcess is the restricted process resource owned by a launch.
type RuntimeProcess = execution.Process

// RuntimeEngine creates one stopped process under a compiled Runtime Policy.
type RuntimeEngine = execution.Engine

// RuntimeEngineFactory binds an engine to the exact policy compiled for a launch.
type RuntimeEngineFactory = execution.Factory

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

// LauncherConfig supplies stable deployment dependencies.
type LauncherConfig struct {
	Store                    LauncherStore
	Registry                 RuntimeBindingResolver
	Workspace                RuntimeWorkspace
	Gateway                  MCPRegistrar
	Execution                RuntimeEngineFactory
	States                   execution.RetainedState
	ACP                      RuntimeACPFactory
	Sessions                 RuntimeSessionPreparer
	MemoryBytes              int64
	ACPOptions               acp.ClientOptions
	StopTimeout              time.Duration
	Policies                 role.PolicyCatalog
	PathEnvironmentAllowlist []string
	Definition               workflow.Definition
	OperatorInstructions     agentinstructions.Operator
	LegacyBootstrapImage     string
	AgentAdapters            map[string]session.TurnAdapter
}

// MCPRenewal can only extend the authority of the registration captured by its launcher.
type MCPRenewal func(time.Time) bool

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
	PublishMCPRenewal      func(MCPRenewal)
}

func (LaunchRequest) String() string   { return "Agent Turn launch request" }
func (LaunchRequest) GoString() string { return "agentturn.LaunchRequest{<credentials redacted>}" }

// Launcher composes one epoch-fenced Runtime Process and Agent Session attachment.
type Launcher struct {
	store                    LauncherStore
	registry                 RuntimeBindingResolver
	workspace                RuntimeWorkspace
	gateway                  MCPRegistrar
	execution                RuntimeEngineFactory
	states                   execution.RetainedState
	acp                      RuntimeACPFactory
	sessions                 RuntimeSessionPreparer
	memoryBytes              int64
	acpOptions               acp.ClientOptions
	stopTimeout              time.Duration
	policies                 role.PolicyCatalog
	pathEnvironmentAllowlist []string
	definition               workflow.Definition
	operatorInstructions     agentinstructions.Operator
	legacyBootstrapImage     string
	agentAdapters            map[string]session.TurnAdapter
}

// RuntimeHandle retains the attached ACP client and owns deterministic launch cleanup.
type RuntimeHandle struct {
	Client  RuntimeACPClient
	Session session.Result
	agent   session.TurnConnection
	lease   store.AgentTurnLease

	gateway                     MCPRegistrar
	registration                mcp.Registration
	registered                  bool
	workspace                   RuntimeWorkspace
	workspaceRef                execution.WorkspaceRef
	sessionPrepared             bool
	sessionPreparationAttempted bool
	creationAttempted           bool
	terminalCapture             *execution.CaptureRef
	store                       LauncherStore
	assignmentID                string
	engine                      RuntimeEngine
	process                     RuntimeProcess
	exitObservation             *execution.ExitObservation
	oomRecorded                 bool
	observeExit                 bool
	stopTimeout                 time.Duration
	secrets                     []string
	mcpMutex                    sync.Mutex
	mcpClosed                   bool
	mcpDrained                  bool
	cleanupMutex                sync.Mutex
	acpClosed                   bool
	processStopped              bool
	processRemoved              bool
	workspaceDiscarded          bool
	scratchCleaned              bool
	scratchPrepared             bool
	discardWorkspace            bool
	engineClosed                bool
}

// NewLauncher validates deployment dependencies before any turn is launched.
func NewLauncher(config LauncherConfig) (*Launcher, error) {
	if nilInterface(config.Store) || nilInterface(config.Registry) || nilInterface(config.Workspace) ||
		nilInterface(config.Gateway) || nilInterface(config.Execution) || nilInterface(config.ACP) || nilInterface(config.Sessions) {
		return nil, fmt.Errorf("%w: dependency is nil", ErrInvalidLauncher)
	}
	if config.MemoryBytes <= 0 {
		return nil, fmt.Errorf("%w: Agent Turn memory must be positive", ErrInvalidLauncher)
	}
	for reference, adapter := range config.AgentAdapters {
		if strings.TrimSpace(reference) == "" || nilInterface(adapter) {
			return nil, fmt.Errorf("%w: invalid agent adapter", ErrInvalidLauncher)
		}
	}
	if config.LegacyBootstrapImage != "" && !runtimeprofile.IsExactRegistryImage(config.LegacyBootstrapImage) {
		return nil, fmt.Errorf("%w: legacy bootstrap image must be digest-pinned", ErrInvalidLauncher)
	}
	if config.StopTimeout < 0 {
		return nil, fmt.Errorf("%w: stop timeout", ErrInvalidLauncher)
	}
	if config.StopTimeout == 0 {
		config.StopTimeout = defaultStopTimeout
	}
	if len(config.Policies.Roles()) == 0 {
		config.Policies = role.BuiltinPolicyCatalog()
	}
	if !config.Definition.Valid() {
		var err error
		config.Definition, err = workflow.NewBuiltinDefinition(role.BuiltinCatalog())
		if err != nil {
			return nil, fmt.Errorf("%w: Workflow Definition: %v", ErrInvalidLauncher, err)
		}
	}
	return &Launcher{
		store: config.Store, registry: config.Registry, workspace: config.Workspace,
		gateway: config.Gateway, execution: config.Execution, acp: config.ACP, sessions: config.Sessions,
		states:      config.States,
		memoryBytes: config.MemoryBytes,
		acpOptions:  config.ACPOptions, stopTimeout: config.StopTimeout, policies: config.Policies,
		pathEnvironmentAllowlist: append([]string(nil), config.PathEnvironmentAllowlist...),
		definition:               config.Definition, operatorInstructions: config.OperatorInstructions,
		legacyBootstrapImage: config.LegacyBootstrapImage,
		agentAdapters:        cloneAdapters(config.AgentAdapters),
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
	var stateRef execution.StateRef
	var spec execution.Request
	execution, err := launcher.store.GetAgentTurnExecutionContext(ctx, request.Lease)
	if err != nil {
		return nil, fmt.Errorf("get fenced Agent Turn execution context: %w", err)
	}
	if err := validateExecutionBinding(execution, request.Lease); err != nil {
		return nil, err
	}
	rolePolicy, ok := launcher.policies.Lookup(execution.Assignment.Role)
	if !ok {
		return nil, fmt.Errorf("%w: Role policy", ErrRuntimeBinding)
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
	legacyEnvironment := runtimeProfile.Contract().RepositoryEnvironment == nil
	if legacyEnvironment && launcher.legacyBootstrapImage == "" {
		return nil, fmt.Errorf("%w: retained Runtime Profile requires a deployment bootstrap image", ErrRuntimeBinding)
	}
	if err := launcher.execution.CheckRequirements(runtimeRequirements(runtimeProfile)); err != nil {
		return nil, fmt.Errorf("validate repository environment capabilities: %w", err)
	}
	adapter := launcher.agentAdapters[runtimeProfile.Contract().Name+"/"+runtimeProfile.Contract().Version]
	if adapter == nil && runtimeProfile.Contract().Name != "opencode-acp" {
		return nil, fmt.Errorf("%w: agent adapter is unavailable", ErrRuntimeBinding)
	}
	var rendered *opencode.RenderedProfile
	var roleID opencode.Role
	if adapter == nil {
		rendered, roleID, err = launcher.renderTurnProfile(execution, rolePolicy)
		if err != nil {
			return nil, err
		}
	}
	revision, branch, headSHA, pullRequest, err := checkoutSelection(execution, request, rolePolicy)
	if err != nil {
		return nil, err
	}

	key := runtimeKey(execution)
	workspaceRef, err := launcher.workspace.Prepare(ctx, workspace.Preparation{Key: key, Checkout: workspace.Checkout{
		AssignmentID: execution.Assignment.ID, RepositoryURL: request.RepositoryURL,
		Credential: request.RepositoryCredential, Revision: revision, ExecutionEpoch: request.Lease.ExecutionEpoch,
		Fence: func(ctx context.Context, operation func(context.Context) error) error {
			return launcher.store.WithAgentTurnFence(ctx, request.Lease, operation)
		},
	}})
	if err != nil {
		return nil, fmt.Errorf("prepare isolated Agent workspace: %w", err)
	}
	resources := &RuntimeHandle{
		gateway: launcher.gateway, stopTimeout: launcher.stopTimeout, lease: request.Lease,
		store:   launcher.store,
		secrets: append([]string(nil), secrets...),
	}
	resources.workspace = launcher.workspace
	resources.workspaceRef = workspaceRef
	resources.workspaceRef.Key = key
	resources.store = launcher.store
	resources.assignmentID = execution.Assignment.ID
	resources.discardWorkspace = rolePolicy.DiscardWorkspace
	// A failed or ambiguously acknowledged create/start may still have dispatched
	// the main environment for an existing Session. Establish its capture need
	// before either operation can fail, not only after Start acknowledges success.
	resources.sessionPrepared = execution.Session.Status != store.AgentSessionCreating || execution.Session.ACPSessionID != "" || len(execution.Session.RuntimeCapabilities) != 0
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCleanupTimeout)
		defer cancel()
		if resources.process != nil && resources.sessionPreparationAttempted && !resources.sessionPrepared {
			// ACP binding may have committed before continuation/configuration
			// failed. Match recovery's durable requirement before removing its
			// only source of terminal evidence; an unreadable binding is unknown.
			current, bindingErr := launcher.store.GetAgentTurnExecutionContext(cleanupCtx, resources.lease)
			resources.sessionPrepared = bindingErr != nil || current.Session.Status != store.AgentSessionCreating || current.Session.ACPSessionID != "" || len(current.Session.RuntimeCapabilities) != 0
			if bindingErr != nil {
				err = errors.Join(err, fmt.Errorf("inspect failed Agent Session preparation: %w", bindingErr))
			}
		}
		if resources.process != nil {
			_, observeErr := resources.ObserveOOM(cleanupCtx)
			if observeErr != nil {
				err = errors.Join(err, observeErr)
			} else if resources.oomRecorded {
				err = errors.Join(err, ErrRuntimeOOMKilled)
			}
		}
		cleanupErr := resources.Cleanup(cleanupCtx)
		if resources.ConfirmedOOM() && !errors.Is(err, ErrRuntimeOOMKilled) {
			err = errors.Join(err, ErrRuntimeOOMKilled)
		}
		err = errors.Join(err, cleanupErr)
		if cleanupErr != nil {
			handle = resources
		} else {
			handle = nil
		}
	}()
	if workspaceRef.Key != key || workspaceRef.Seed != strings.ToLower(revision) {
		return nil, fmt.Errorf("%w: prepared workspace identity", ErrRuntimeBinding)
	}
	toolEnvironment := make(map[string]string)
	var turnConfiguration turnconfig.Configuration
	if len(execution.Turn.TurnConfiguration) > 0 {
		if err := json.Unmarshal(execution.Turn.TurnConfiguration, &turnConfiguration); err != nil {
			return nil, fmt.Errorf("%w: turn configuration snapshot: %v", ErrRuntimeBinding, err)
		}
	}
	if err := turnConfiguration.Validate(launcher.pathEnvironmentAllowlist); err != nil {
		return nil, fmt.Errorf("%w: turn configuration snapshot: %v", ErrRuntimeBinding, err)
	}
	if legacyEnvironment {
		if turnConfiguration.Directories == nil {
			turnConfiguration.Directories = make(map[string]turnconfig.Lifecycle)
		}
		name := environment.BootstrapDirectory(execution.Turn.ID)
		if _, exists := turnConfiguration.Directories[name]; exists {
			return nil, fmt.Errorf("%w: reserved bootstrap scratch directory", ErrRuntimeBinding)
		}
		turnConfiguration.Directories[name] = turnconfig.Turn
	}
	resources.scratchPrepared = len(turnConfiguration.Directories) != 0
	pathEnvironment, err := launcher.workspace.PrepareTurnPaths(ctx, execution.Assignment.ID, execution.Turn.ID, turnConfiguration,
		func(ctx context.Context, operation func(context.Context) error) error {
			return launcher.store.WithAgentTurnFence(ctx, request.Lease, operation)
		})
	if err != nil {
		return nil, fmt.Errorf("prepare Agent Turn paths: %w", err)
	}
	for name, value := range pathEnvironment {
		toolEnvironment[name] = value
	}
	refreshedLease, err := launcher.store.RefreshAgentTurnLease(ctx, request.Lease, request.LeaseDuration)
	if err != nil {
		return nil, fmt.Errorf("refresh Agent Turn lease before Runtime Process launch: %w", err)
	}
	request.Lease = refreshedLease
	resources.lease = refreshedLease

	registration, err := launcher.gateway.Register(mcp.TokenScope{
		RuntimeProfileName: execution.Assignment.RuntimeProfileName, RuntimeProfileVersion: execution.Assignment.RuntimeProfileVersion,
		Lease: request.Lease, WorkflowID: execution.WorkflowID, Role: execution.Assignment.Role,
		Repository: mcp.RepositoryScope{ID: execution.Repository.ID, Owner: execution.Repository.Owner, Name: execution.Repository.Name},
		Issue:      mcp.IssueScope{ID: execution.Issue.ID, Number: execution.Issue.Number}, PullRequest: pullRequest,
		Branch: branch, BranchExists: execution.Publication != nil || execution.ChangeProposal != nil,
		RecoveredPublication: execution.Publication != nil,
		DefaultBranch:        request.DefaultBranchName, HeadSHA: headSHA, ExpiresAt: request.Lease.LeaseExpiresAt,
	})
	resources.registration = registration
	secrets = append(secrets, registrationSecrets(registration)...)
	resources.secrets = append(resources.secrets, registrationSecrets(registration)...)
	if err != nil {
		launcher.gateway.Revoke(registration)
		return nil, fmt.Errorf("register per-turn MCP authority: %w", err)
	}
	resources.registered = true
	if request.PublishMCPRenewal != nil {
		request.PublishMCPRenewal(resources.renewMCP)
	}

	if launcher.states != nil {
		stateRef, err = launcher.store.AgentTurnState(ctx, request.Lease)
		if err != nil {
			return nil, err
		}
		if stateRef.ParticipantID != key.AssignmentID || stateRef.SessionID != key.AgentSessionID {
			return nil, ErrRuntimeBinding
		}
		if err := launcher.states.Ensure(ctx, stateRef); err != nil {
			return nil, err
		}
		if err := launcher.store.MarkAgentTurnStateInitialized(ctx, request.Lease, stateRef); err != nil {
			return nil, err
		}
	} else {
		// Direct OpenCode qualification fixtures use the pre-boundary layout.
		// Production supplies States; remove this compatibility route in Step 17.
		statePath := "assignment-" + execution.Assignment.ID + "/runtime-state"
		if execution.Assignment.RuntimeStatePath != statePath || execution.Session.RuntimeStatePath != statePath {
			return nil, ErrRuntimeBinding
		}
	}
	if adapter != nil {
		if launcher.states == nil {
			return nil, ErrRuntimeBinding
		}
		spec, err = adapter.BuildProcess(runtimeProfile, runtimeKey(execution), execution.WorkflowID, launcher.memoryBytes, toolEnvironment)
	} else {
		spec, err = opencode.BuildProcess(runtimeProfile, rendered, opencode.ProviderCredentials{
			Role: roleID, Content: append([]byte(nil), request.ProviderCredentialJSON...),
		}, opencode.ProcessOptions{
			MemoryBytes: launcher.memoryBytes,
			Key:         runtimeKey(execution), WorkflowID: execution.WorkflowID, Environment: toolEnvironment,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("compile Runtime Process contract: %w", err)
	}
	spec.Workspace = resources.workspaceRef
	spec.State = stateRef
	if contract := runtimeProfile.Contract(); contract.RepositoryEnvironment != nil {
		spec.Command = append([]string{environment.Executable, "run"}, contract.Command...)
		spec.GoldenResources = true
	} else {
		spec.Command = append([]string{environment.RetainedBootstrapExecutable, "run"}, contract.Command...)
		spec.BootstrapImage = launcher.legacyBootstrapImage
	}
	spec.Environment = environment.BindEnvironment(spec.Environment)
	spec.Requirements = runtimeRequirements(runtimeProfile)
	engine, err := launcher.execution.New(spec, launcher.store.ExecutionLedger(request.Lease))
	if !nilInterface(engine) {
		resources.engine = engine
	}
	if err != nil {
		return nil, fmt.Errorf("create restricted execution engine: %w", err)
	}
	if nilInterface(engine) {
		return nil, errors.New("create restricted execution engine: factory returned nil")
	}
	// The lease-bound ledger commits admission before dispatch. Provisioning must
	// not run inside WithAgentTurnFence: its durable evidence operations need the
	// same pool, and lease heartbeats must remain possible during external calls.
	resources.creationAttempted = true
	process, createErr := engine.Create(ctx, spec, request.Stderr)
	if !nilInterface(process) {
		resources.process = process
	}
	if createErr != nil {
		return nil, fmt.Errorf("create restricted Runtime Process: %w", createErr)
	}
	if nilInterface(process) {
		return nil, errors.New("create restricted Runtime Process: engine returned no process")
	}
	err = launcher.store.WithAgentTurnFence(ctx, request.Lease, func(fenceCtx context.Context) error {
		if startErr := process.Start(fenceCtx); startErr != nil {
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
	if adapter != nil {
		stage, ok := launcher.definition.Stage(execution.Turn.Stage)
		if !ok || stage.Role != rolePolicy.Role || !launcher.definition.AcceptsPurpose(stage.ID, execution.Turn.Purpose) {
			return nil, ErrRuntimeBinding
		}
		resources.sessionPreparationAttempted = true
		prepared, prepareErr := adapter.Prepare(ctx, session.TurnPreparation{Lease: request.Lease, Session: execution.Session, State: stateRef, Transport: transport, Profile: execution.Turn.AgentProfileConfig, ProfileName: execution.Assignment.AgentProfileName, Policy: rolePolicy, Stage: stage, Purpose: execution.Turn.Purpose, Operator: launcher.operatorInstructions, MCP: registration.Server, Credentials: request.ProviderCredentialJSON})
		resources.agent = prepared
		if prepareErr != nil {
			return nil, fmt.Errorf("prepare Agent Session: %w", prepareErr)
		}
		if prepared == nil {
			return nil, agent.ErrNotReady
		}
		resources.sessionPrepared = true
		return resources, nil
	}
	clientOptions := launcher.acpOptions
	if clientOptions.Logger == nil {
		clientOptions.Logger = slog.Default()
	}
	clientOptions.Logger = clientOptions.Logger.With("workflow_id", execution.WorkflowID)
	clientOptions.DiagnosticContext = agentevent.Context{
		AssignmentID: execution.Assignment.ID, AgentSessionID: execution.Session.ID,
		ACPSessionID: execution.Session.ACPSessionID, TurnID: execution.Turn.ID,
		ExecutionEpoch: uint64(execution.Turn.ExecutionEpoch), ControlRevision: uint64(execution.Session.ControlRevision),
	}
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
	resources.sessionPreparationAttempted = true
	statePath := ""
	if launcher.states == nil {
		statePath = execution.Session.RuntimeStatePath
	}
	prepared, err := launcher.sessions.Prepare(ctx, session.PrepareRequest{
		Lease: request.Lease, Session: execution.Session, Client: client, State: stateRef, StatePath: statePath,
		MCPServers: []acp.MCPServer{acp.HTTPServer(registration.Server)}, Configuration: rendered.SessionConfiguration(),
	})
	if err != nil {
		return nil, fmt.Errorf("prepare Agent Session: %w", err)
	}
	resources.Session = prepared
	resources.sessionPrepared = true
	return resources, nil
}

// ErrRuntimeOOMKilled is attached only to a backend-confirmed and durably recorded OOM exit.
var ErrRuntimeOOMKilled = errors.New("Runtime Process was OOM-killed")

// ObserveOOM saves a stopped Runtime Process's OOM state before resource removal.
// A missing or running process cannot establish an OOM cause.
func (handle *RuntimeHandle) ObserveOOM(ctx context.Context) (bool, error) {
	if handle == nil || handle.process == nil {
		return false, nil
	}
	handle.observeExit = true
	if handle.exitObservation == nil {
		observation, err := handle.process.ObserveExit(ctx)
		if err != nil {
			return false, err
		}
		if observation.Exited && (observation.OOMKilled || observation.ExitCode != 137) {
			handle.exitObservation = &observation
		}
	}
	if handle.exitObservation == nil || !handle.exitObservation.OOMKilled {
		return false, nil
	}
	if !handle.oomRecorded {
		observation := handle.exitObservation
		if err := handle.store.RecordAgentTurnRuntimeOOM(ctx, handle.lease, string(observation.Resource), observation.ExitCode); err != nil {
			return false, fmt.Errorf("persist Runtime Process OOM before removal: %w", err)
		}
		handle.oomRecorded = true
	}
	return true, nil
}

// ConfirmedOOM reports whether backend OOM exit evidence was recorded before cleanup.
func (handle *RuntimeHandle) ConfirmedOOM() bool {
	return handle != nil && handle.oomRecorded
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
	handle.mcpClosed = true
	drainErr := handle.gateway.CloseAndDrain(ctx, handle.registration)
	if drainErr == nil || errors.Is(drainErr, mcp.ErrMutationDrainUnresolved) {
		handle.mcpDrained = true
	}
	return drainErr
}

func (handle *RuntimeHandle) renewMCP(expiresAt time.Time) bool {
	if handle == nil {
		return false
	}
	handle.mcpMutex.Lock()
	defer handle.mcpMutex.Unlock()
	if handle.mcpClosed {
		return true
	}
	return handle.gateway != nil && handle.registered && handle.gateway.Renew(handle.registration, expiresAt)
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

func (handle *RuntimeHandle) AgentConnection() session.TurnConnection {
	if handle == nil {
		return nil
	}
	return handle.agent
}

func cloneAdapters(adapters map[string]session.TurnAdapter) map[string]session.TurnAdapter {
	result := make(map[string]session.TurnAdapter, len(adapters))
	for key, value := range adapters {
		result[key] = value
	}
	return result
}

// Cleanup drains MCP authority, stops execution, seals terminal evidence, and then removes resources.
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
	if handle.observeExit && handle.process != nil && !handle.processRemoved {
		observeCtx, cancel := handle.teardownContext(ctx)
		_, observeErr := handle.ObserveOOM(observeCtx)
		cancel()
		if observeErr != nil {
			return fmt.Errorf("observe Runtime Process before cleanup: %w", observeErr)
		}
	}

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
	if handle.agent != nil {
		if err := handle.agent.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("close agent transport: %w", err))
		} else {
			handle.agent = nil
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
		// A drain timeout or an unconfirmed stop cannot authorize destruction of
		// the environment needed to retry the terminal observation.
		if handle.processStopped && (!handle.registered || handle.mcpDrained) {
			captureReady := handle.terminalCapture != nil
			if !handle.sessionPrepared {
				proofCtx, cancelProof := handle.teardownContext(ctx)
				proofErr := handle.store.RecordAgentTurnUnpromptedCleanup(proofCtx, handle.lease)
				cancelProof()
				if proofErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("record preparation-only Runtime Process cleanup: %w", proofErr))
				} else {
					captureReady = true
				}
			} else if !captureReady {
				captureCtx, cancelCapture := handle.teardownContext(ctx)
				fence := func(ctx context.Context, operation func(context.Context) error) error {
					return handle.store.WithAgentTurnCleanupFence(ctx, handle.lease, operation)
				}
				ref, captureErr := captureTerminalWorkspace(captureCtx, handle.workspaceRef.Key, handle.workspace.Capture, fence)
				cancelCapture()
				if captureErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("capture stopped Agent Turn workspace: %w", captureErr))
				} else {
					handle.terminalCapture = &ref
					captureReady = true
				}
			}
			if captureReady {
				removeCtx, cancelRemove := handle.teardownContext(ctx)
				err := handle.process.Remove(removeCtx)
				cancelRemove()
				if err != nil {
					cleanupErrors = append(cleanupErrors, err)
				} else {
					handle.processRemoved = true
				}
			}
		}
	}
	if handle.process == nil && handle.engine != nil && !handle.processRemoved {
		if handle.creationAttempted {
			// A missing process handle is not no-dispatch evidence. Exact-key
			// recovery must stop and capture any ambiguously created environment.
			cleanupErrors = append(cleanupErrors, errors.New("terminal capture unavailable after ambiguous Runtime Process creation; exact-key recovery required"))
		} else {
			removeCtx, cancel := handle.teardownContext(ctx)
			err := handle.engine.EnsureAbsent(removeCtx)
			cancel()
			if err != nil {
				cleanupErrors = append(cleanupErrors, err)
			} else {
				handle.processRemoved = true
			}
		}
	}
	processRemoved := handle.processRemoved || handle.process == nil && handle.engine == nil
	if processRemoved && handle.scratchPrepared && handle.workspace != nil && !handle.scratchCleaned {
		fence := func(ctx context.Context, operation func(context.Context) error) error {
			return handle.store.WithAgentTurnCleanupFence(ctx, handle.lease, operation)
		}
		if err := handle.workspace.CleanupTurnPaths(ctx, handle.assignmentID, handle.lease.ID, fence); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("clean Agent Turn scratch: %w", err))
		} else {
			handle.scratchCleaned = true
		}
	}
	if processRemoved && handle.discardWorkspace && handle.workspace != nil && !handle.workspaceDiscarded {
		fence := func(ctx context.Context, operation func(context.Context) error) error {
			return handle.store.WithAgentTurnCleanupFence(ctx, handle.lease, operation)
		}
		if err := handle.workspace.Discard(ctx, handle.workspaceRef, fence); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("discard Reviewer workspace: %w", err))
		} else {
			handle.workspaceDiscarded = true
		}
	}
	if processRemoved && handle.engine != nil && !handle.engineClosed {
		if err := handle.engine.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("close execution engine: %w", err))
		} else {
			handle.engineClosed = true
		}
	}
	return errors.Join(cleanupErrors...)
}

func terminalCaptureRequest(key execution.Key) execution.CaptureRequest {
	return execution.CaptureRequest{
		Workspace: execution.WorkspaceRef{Key: key}, OperationID: key.AgentTurnID, Purpose: execution.TerminalCapture,
	}
}

// Capture transfer and sealing run outside database transactions so heartbeats
// and recovery can progress. Both admission and adoption require a short fence.
func captureTerminalWorkspace(ctx context.Context, key execution.Key,
	capture func(context.Context, execution.CaptureRequest) (execution.CaptureRef, error),
	fence workspace.WorkspaceFence) (execution.CaptureRef, error) {
	validate := func(context.Context) error { return nil }
	if err := fence(ctx, validate); err != nil {
		return execution.CaptureRef{}, fmt.Errorf("admit terminal workspace capture: %w", err)
	}
	request := terminalCaptureRequest(key)
	ref, err := capture(ctx, request)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	if ref.Request != request || len(ref.SHA256) != 64 {
		return execution.CaptureRef{}, fmt.Errorf("%w: terminal capture identity", ErrRuntimeBinding)
	}
	if err := fence(ctx, validate); err != nil {
		return execution.CaptureRef{}, fmt.Errorf("adopt sealed terminal workspace capture: %w", err)
	}
	return ref, nil
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

func (launcher *Launcher) renderTurnProfile(execution store.AgentTurnExecutionContext, policy role.Policy) (*opencode.RenderedProfile, opencode.Role, error) {
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
	if policy.Role != execution.Assignment.Role || snapshot.Name != execution.Assignment.AgentProfileName ||
		!agentprofile.ValidPath(snapshot.Path) || snapshot.Role != execution.Assignment.Role ||
		snapshot.Runtime != execution.Assignment.RuntimeProfileName+"/"+execution.Assignment.RuntimeProfileVersion {
		return nil, "", ErrRuntimeBinding
	}
	permissions := make(opencode.PermissionPolicy, len(snapshot.Permissions))
	stage, ok := launcher.definition.Stage(execution.Turn.Stage)
	if !ok || stage.Role != policy.Role || !launcher.definition.AcceptsPurpose(stage.ID, execution.Turn.Purpose) {
		return nil, "", fmt.Errorf("%w: Stage instruction binding", ErrRuntimeBinding)
	}
	for name, value := range snapshot.Permissions {
		permissions[name] = opencode.Permission(value)
	}
	runtimeTools := make([]string, len(policy.MCPTools))
	for index, capability := range policy.MCPTools {
		runtimeTools[index] = mcp.ServerName + "_" + capability
	}
	rendered, err := opencode.RenderWithPolicy(policy, opencode.Profile{
		Instructions: launcher.operatorInstructions.Compose(policy, stage, execution.Turn.Purpose, snapshot.Instructions), Model: snapshot.Model, Variant: snapshot.Variant,
		Steps: snapshot.Steps, Permissions: permissions, RuntimeTools: runtimeTools,
	})
	if err != nil {
		return nil, "", fmt.Errorf("render assigned Agent Profile: %w", err)
	}
	return rendered, opencode.Role(policy.Role), nil
}

func checkoutSelection(execution store.AgentTurnExecutionContext, request LaunchRequest, policy role.Policy) (string, string, string, *mcp.PullRequestScope, error) {
	if execution.ChangeProposal == nil {
		if policy.Role != execution.Assignment.Role || policy.RequiresChangeProposal {
			return "", "", "", nil, fmt.Errorf("%w: Role requires a Change Proposal", ErrRuntimeBinding)
		}
		if publication := execution.Publication; publication != nil {
			if publication.HeadRef != request.InitialFeatureBranch || publication.BaseRef != request.DefaultBranchName || publication.HeadSHA == "" ||
				(publication.PullRequestID == 0) != (publication.PullRequestNumber == 0) {
				return "", "", "", nil, fmt.Errorf("%w: in-progress publication", ErrRuntimeBinding)
			}
			var pullRequest *mcp.PullRequestScope
			if publication.PullRequestID > 0 {
				pullRequest = &mcp.PullRequestScope{ID: publication.PullRequestID, Number: publication.PullRequestNumber}
			}
			return publication.HeadSHA, publication.HeadRef, publication.HeadSHA, pullRequest, nil
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

func runtimeRequirements(profile runtimeprofile.Profile) []execution.Requirement {
	requirements := []execution.Requirement{execution.ToolInstallation}
	if environment := profile.Contract().RepositoryEnvironment; environment != nil {
		for _, requirement := range environment.Requirements {
			if requirement != string(execution.ToolInstallation) {
				requirements = append(requirements, execution.Requirement(requirement))
			}
		}
	}
	return requirements
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

func runtimeKey(context store.AgentTurnExecutionContext) execution.Key {
	return execution.Key{
		AssignmentID: context.Assignment.ID, AgentSessionID: context.Session.ID,
		AgentTurnID: context.Turn.ID, ExecutionEpoch: context.Turn.ExecutionEpoch,
		RuntimeProfileName: context.Assignment.RuntimeProfileName, RuntimeProfileVersion: context.Assignment.RuntimeProfileVersion,
	}
}

// ProductionACPFactory creates the protocol client attached to Docker's stream transport.
type ProductionACPFactory struct{}

func (ProductionACPFactory) New(transport io.ReadWriteCloser, options acp.ClientOptions) RuntimeACPClient {
	return acp.NewClient(transport, options)
}
