package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

const (
	runtimeTestImage       = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runtimeTestDefaultSHA  = "1111111111111111111111111111111111111111"
	runtimeTestPRHeadSHA   = "2222222222222222222222222222222222222222"
	runtimeTestAssignment  = "10000000-0000-4000-8000-000000000001"
	runtimeTestSession     = "20000000-0000-4000-8000-000000000001"
	runtimeTestTurn        = "30000000-0000-4000-8000-000000000001"
	runtimeTestWorkflow    = "40000000-0000-4000-8000-000000000001"
	runtimeTestCredential  = "repository-secret"
	runtimeTestProviderKey = "provider-secret"
)

func TestLauncherLaunchesInitialDeveloperFromDefaultBranchUnderEpochFence(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	database := &runtimeStore{operations: &operations, execution: execution}
	workspaces := &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
		DataDir:        "/srv/mise/assignment-" + runtimeTestAssignment + "/mise",
		SourceRevision: runtimeTestDefaultSHA,
		Environment: map[string]string{
			"MISE_DATA_DIR": "/srv/mise/assignment-" + runtimeTestAssignment + "/mise",
			"PATH":          "/srv/mise/assignment-" + runtimeTestAssignment + "/mise/shims:/usr/bin",
			"PROJECT_MODE":  "development",
		},
	}}
	gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: acp.MCPServer{
		Type: "http", Name: "omnigrex", URL: "http://mcp:8080/mcp",
		Headers: []acp.EnvironmentEntry{{Name: "Authorization", Value: "Bearer mcp-secret"}},
	}}}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	client := &runtimeACPClient{operations: &operations}
	clientFactory := &runtimeACPFactory{operations: &operations, client: client}
	sessions := &runtimeSessionPreparer{operations: &operations, result: session.Result{AgentSessionID: runtimeTestSession, ACPSessionID: "acp-session"}}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: workspaces, Gateway: gateway, Docker: engineFactory, ACP: clientFactory, Sessions: sessions,
		Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	t.Cleanup(func() { _ = handle.Cleanup(context.Background()) })

	if len(operations) == 0 || operations[0] != "fence" {
		t.Fatalf("dependency operations = %v, want fence first", operations)
	}
	wantPrefix := []string{"fence", "execution", "profile", "workspace", "mise", "fence", "mcp-register", "docker-new", "turn-fence", "docker-create", "process-start", "fence", "acp-new", "session-prepare"}
	if !reflect.DeepEqual(operations, wantPrefix) {
		t.Fatalf("dependency operations = %v, want %v", operations, wantPrefix)
	}
	if workspaces.checkout.Revision != runtimeTestDefaultSHA || workspaces.mise.Revision != runtimeTestDefaultSHA {
		t.Errorf("workspace revision = %q and mise revision = %q, want default SHA", workspaces.checkout.Revision, workspaces.mise.Revision)
	}
	if workspaces.checkout.AssignmentID != runtimeTestAssignment || workspaces.checkout.Credential != runtimeTestCredential {
		t.Errorf("workspace checkout = %#v", workspaces.checkout)
	}
	if gateway.scope.WorkflowID != runtimeTestWorkflow || gateway.scope.Role != workflow.RoleDeveloper ||
		gateway.scope.Branch != "omnigrex/issue-17" || gateway.scope.DefaultBranch != "trunk" || gateway.scope.HeadSHA != runtimeTestDefaultSHA ||
		gateway.scope.PullRequest != nil || gateway.scope.ExpiresAt != lease.LeaseExpiresAt || gateway.scope.Lease.ExecutionEpoch != 7 {
		t.Errorf("MCP token scope = %#v", gateway.scope)
	}

	spec := engineFactory.engine.spec
	wantLabels := map[string]string{
		"io.omnigrex.agent-session":   runtimeTestSession,
		"io.omnigrex.agent-turn":      runtimeTestTurn,
		"io.omnigrex.assignment":      runtimeTestAssignment,
		"io.omnigrex.execution-epoch": "7",
		"io.omnigrex.runtime-profile": "opencode-acp/v1",
		"io.omnigrex.workflow":        runtimeTestWorkflow,
	}
	if !reflect.DeepEqual(spec.Labels, wantLabels) {
		t.Errorf("Docker labels = %#v, want %#v", spec.Labels, wantLabels)
	}
	wantVolumes := []dockerruntime.VolumeMount{
		{Name: "mise", Subpath: "assignment-" + runtimeTestAssignment + "/mise", Target: "/home/opencode/.local/share/mise"},
		{Name: "runtime-state", Subpath: "assignment-" + runtimeTestAssignment + "/runtime-state", Target: "/home/opencode/.local/share/opencode"},
		{Name: "workspaces", Subpath: "assignment-" + runtimeTestAssignment + "/workspace", Target: "/workspace"},
	}
	if !reflect.DeepEqual(spec.Volumes, wantVolumes) {
		t.Errorf("Docker volumes = %#v, want %#v", spec.Volumes, wantVolumes)
	}
	assertRuntimeEnvironment(t, spec.Environment, map[string]string{
		"MISE_DATA_DIR": "/home/opencode/.local/share/mise",
		"PATH":          "/home/opencode/.local/share/mise/shims:/usr/bin",
		"PROJECT_MODE":  "development",
	})
	for _, secret := range []string{runtimeTestCredential, "mcp-secret"} {
		if strings.Contains(strings.Join(spec.Environment, "\n"), secret) || strings.Contains(stringMap(spec.Labels), secret) {
			t.Errorf("Docker contract contains non-provider credential %q", secret)
		}
	}
	if engineFactory.options.AgentNetwork != "omnigrex-agent" || !reflect.DeepEqual(engineFactory.options.RuntimePolicy.Labels, spec.Labels) {
		t.Errorf("Docker Engine options = %#v", engineFactory.options)
	}
	if clientFactory.transport != engineFactory.engine.process.transport {
		t.Error("ACP client did not attach to Runtime Process transport")
	}
	if required := clientFactory.options.RequiredCapabilities; !required.SessionList || !required.SessionResume || !required.SessionLoad {
		t.Errorf("ACP required capabilities = %#v", required)
	}
	if len(sessions.request.MCPServers) != 1 || !reflect.DeepEqual(sessions.request.MCPServers[0], gateway.registration.Server) {
		t.Errorf("Session MCP servers = %#v, want registered descriptor", sessions.request.MCPServers)
	}
	if sessions.request.Lease.ExecutionEpoch != 7 || sessions.request.Session.ID != runtimeTestSession ||
		sessions.request.StatePath != "assignment-"+runtimeTestAssignment+"/runtime-state" || sessions.request.Client != client {
		t.Errorf("Session Prepare request = %#v", sessions.request)
	}
	if handle.Client != client || handle.Session.ACPSessionID != "acp-session" || client.prompts != 0 {
		t.Errorf("Runtime handle = %#v", handle)
	}
}

func TestLauncherUsesPullRequestRevisionAndReviewerTrustedDefaultBranchTools(t *testing.T) {
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	for _, testCase := range []struct {
		name             string
		role             workflow.Role
		wantMiseRevision string
	}{
		{name: "returning Developer", role: workflow.RoleDeveloper, wantMiseRevision: runtimeTestPRHeadSHA},
		{name: "Reviewer", role: workflow.RoleReviewer, wantMiseRevision: runtimeTestDefaultSHA},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			operations := []string{}
			runtimeProfile := runtimeLauncherProfile(t)
			execution, lease := runtimeExecutionContext(t, runtimeProfile, testCase.role, proposal)
			miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
			workspaces := &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
				DataDir: miseDir, SourceRevision: testCase.wantMiseRevision,
				Environment: map[string]string{"MISE_DATA_DIR": miseDir},
			}}
			gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: acp.MCPServer{
				Type: "http", Name: "omnigrex", URL: "http://mcp:8080/mcp",
			}}}
			engineFactory := &runtimeEngineFactory{operations: &operations}
			client := &runtimeACPClient{operations: &operations}
			sessions := &runtimeSessionPreparer{operations: &operations}
			launcher := runtimeLauncher(t, agentturn.LauncherConfig{
				Store:     &runtimeStore{operations: &operations, execution: execution},
				Registry:  &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
				Workspace: workspaces, Gateway: gateway, Docker: engineFactory,
				ACP: &runtimeACPFactory{operations: &operations, client: client}, Sessions: sessions,
				Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
			})

			handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
				Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
				DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "unused-initial-branch",
				RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
			})
			if err != nil {
				t.Fatalf("Launch() error = %v", err)
			}
			t.Cleanup(func() { _ = handle.Cleanup(context.Background()) })
			if workspaces.checkout.Revision != runtimeTestPRHeadSHA {
				t.Errorf("workspace revision = %q, want Pull Request head", workspaces.checkout.Revision)
			}
			if workspaces.mise.Revision != testCase.wantMiseRevision {
				t.Errorf("mise revision = %q, want %q", workspaces.mise.Revision, testCase.wantMiseRevision)
			}
			if gateway.scope.PullRequest == nil || gateway.scope.PullRequest.ID != 61 || gateway.scope.PullRequest.Number != 23 ||
				gateway.scope.Branch != proposal.HeadRef || gateway.scope.HeadSHA != proposal.HeadSHA {
				t.Errorf("MCP Pull Request scope = %#v", gateway.scope)
			}
			if testCase.role == workflow.RoleReviewer {
				assertRuntimeEnvironment(t, engineFactory.engine.spec.Environment, map[string]string{
					"OPENCODE_DISABLE_PROJECT_CONFIG":  "true",
					"OPENCODE_DISABLE_DEFAULT_PLUGINS": "true",
					"OPENCODE_DISABLE_EXTERNAL_SKILLS": "true",
					"OPENCODE_PURE":                    "true",
				})
			}
		})
	}
}

func TestLauncherFenceFailureHasNoOtherDependencyAction(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store:     &runtimeStore{operations: &operations, execution: execution, fenceErr: store.ErrAgentTurnFenceLost},
		Registry:  &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations}, Gateway: &runtimeGateway{operations: &operations},
		Docker:   &runtimeEngineFactory{operations: &operations},
		ACP:      &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}},
		Sessions: &runtimeSessionPreparer{operations: &operations},
		Network:  "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if !reflect.DeepEqual(operations, []string{"fence"}) {
		t.Fatalf("dependency operations = %v, want only fence", operations)
	}
}

func TestLauncherRechecksFenceAfterWorkspaceProvisioningBeforeRuntimeSideEffects(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
	database := &runtimeStore{operations: &operations, execution: execution, fenceErrors: []error{nil, store.ErrAgentTurnFenceLost}}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
			DataDir: miseDir, SourceRevision: runtimeTestDefaultSHA, Environment: map[string]string{"MISE_DATA_DIR": miseDir},
		}},
		Gateway: &runtimeGateway{operations: &operations}, Docker: &runtimeEngineFactory{operations: &operations},
		ACP: &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}}, Sessions: &runtimeSessionPreparer{operations: &operations},
		Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	want := []string{"fence", "execution", "profile", "workspace", "mise", "fence"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("dependency operations = %v, want %v", operations, want)
	}
}

func TestLauncherDoesNotStartRuntimeProcessWhenLeaseBecomesStaleBeforeFenceAcquisition(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
	database := &runtimeStore{operations: &operations, execution: execution, withFenceAttempted: make(chan struct{})}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
			DataDir: miseDir, SourceRevision: runtimeTestDefaultSHA, Environment: map[string]string{"MISE_DATA_DIR": miseDir},
		}},
		Gateway: &runtimeGateway{operations: &operations}, Docker: engineFactory,
		ACP: &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}}, Sessions: &runtimeSessionPreparer{operations: &operations},
		Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	database.turnFence.Lock()
	locked := true
	defer func() {
		if locked {
			database.turnFence.Unlock()
		}
	}()
	launchResult := make(chan error, 1)
	go func() {
		_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
			Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
			DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
			RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
		})
		launchResult <- err
	}()
	select {
	case <-database.withFenceAttempted:
	case <-time.After(time.Second):
		t.Fatal("Launch() did not attempt to acquire the Agent Turn fence")
	}
	database.withFenceErr = store.ErrAgentTurnFenceLost
	database.turnFence.Unlock()
	locked = false
	err := <-launchResult
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if slices.Contains(operations, "docker-create") || slices.Contains(operations, "process-start") || slices.Contains(operations, "acp-new") {
		t.Fatalf("dependency operations = %v, want no create, start, or ACP attach", operations)
	}
}

func TestLauncherHoldsAgentTurnFenceUntilRuntimeProcessStartFinishes(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
	database := &runtimeStore{operations: &operations, execution: execution}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	engineFactory.engine.process.startEntered = startEntered
	engineFactory.engine.process.releaseStart = releaseStart
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
			DataDir: miseDir, SourceRevision: runtimeTestDefaultSHA, Environment: map[string]string{"MISE_DATA_DIR": miseDir},
		}},
		Gateway: &runtimeGateway{operations: &operations}, Docker: engineFactory,
		ACP: &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}}, Sessions: &runtimeSessionPreparer{operations: &operations},
		Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	launchResult := make(chan error, 1)
	go func() {
		handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
			Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
			DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
			RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
		})
		if handle != nil {
			err = errors.Join(err, handle.Cleanup(context.Background()))
		}
		launchResult <- err
	}()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("Runtime Process Start() was not called")
	}

	recoveryAttempted := make(chan struct{})
	recoveryAcquired := make(chan struct{})
	go database.acquireRecoveryLock(recoveryAttempted, recoveryAcquired)
	<-recoveryAttempted
	select {
	case <-recoveryAcquired:
		t.Fatal("recovery acquired its conflicting Store lock before Runtime Process Start() finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseStart)
	select {
	case <-recoveryAcquired:
	case <-time.After(time.Second):
		t.Fatal("recovery did not acquire its Store lock after Runtime Process Start() finished")
	}
	if err := <-launchResult; err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
}

func TestLauncherCleansCreatedProcessWhenStartCannotCrossRecoveryFence(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
	database := &runtimeStore{operations: &operations, execution: execution}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	engineFactory.engine.process.startErr = store.ErrAgentTurnFenceLost
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
			DataDir: miseDir, SourceRevision: runtimeTestDefaultSHA, Environment: map[string]string{"MISE_DATA_DIR": miseDir},
		}},
		Gateway: &runtimeGateway{operations: &operations}, Docker: engineFactory,
		ACP: &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}}, Sessions: &runtimeSessionPreparer{operations: &operations},
		Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if engineFactory.engine.process.started != 1 || engineFactory.engine.process.removed != 1 || slices.Contains(operations, "acp-new") {
		t.Fatalf("process lifecycle = started %d, removed %d, operations %v", engineFactory.engine.process.started, engineFactory.engine.process.removed, operations)
	}
}

func TestLauncherRevalidatesFenceImmediatelyAfterStartAndCleansBeforeReturn(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
	database := &runtimeStore{operations: &operations, execution: execution, fenceErrors: []error{nil, nil, store.ErrAgentTurnFenceLost}}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
			DataDir: miseDir, SourceRevision: runtimeTestDefaultSHA, Environment: map[string]string{"MISE_DATA_DIR": miseDir},
		}},
		Gateway: &runtimeGateway{operations: &operations}, Docker: engineFactory,
		ACP: &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}}, Sessions: &runtimeSessionPreparer{operations: &operations},
		Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if engineFactory.engine.process.started != 1 || engineFactory.engine.process.removed != 1 || slices.Contains(operations, "acp-new") {
		t.Fatalf("process lifecycle = started %d, removed %d, operations %v", engineFactory.engine.process.started, engineFactory.engine.process.removed, operations)
	}
	startIndex := slices.Index(operations, "process-start")
	fenceIndex := slices.Index(operations[startIndex+1:], "fence")
	removeIndex := slices.Index(operations, "process-remove")
	if startIndex < 0 || fenceIndex != 0 || removeIndex < startIndex {
		t.Fatalf("post-start fence and cleanup ordering = %v", operations)
	}
}

func TestLauncherRejectsRuntimeProfileDriftBeforeWorkspaceSideEffects(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*store.AgentTurnExecutionContext)
	}{
		{name: "Assignment hash", mutate: func(execution *store.AgentTurnExecutionContext) {
			execution.Assignment.RuntimeProfileContentSHA256 = strings.Repeat("b", 64)
		}},
		{name: "Assignment image", mutate: func(execution *store.AgentTurnExecutionContext) {
			execution.Assignment.RuntimeImageDigest = "registry.example/omnigrex/opencode@sha256:" + strings.Repeat("b", 64)
		}},
		{name: "Session binding", mutate: func(execution *store.AgentTurnExecutionContext) {
			execution.Session.RuntimeProfileVersion = "different"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			operations := []string{}
			runtimeProfile := runtimeLauncherProfile(t)
			execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
			testCase.mutate(&execution)
			launcher := runtimeLauncher(t, agentturn.LauncherConfig{
				Store:     &runtimeStore{operations: &operations, execution: execution},
				Registry:  &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
				Workspace: &runtimeWorkspace{operations: &operations}, Gateway: &runtimeGateway{operations: &operations},
				Docker:   &runtimeEngineFactory{operations: &operations},
				ACP:      &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}},
				Sessions: &runtimeSessionPreparer{operations: &operations},
				Network:  "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
			})

			_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
				Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
				DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
				RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
			})
			if !errors.Is(err, agentturn.ErrRuntimeBinding) {
				t.Fatalf("Launch() error = %v, want ErrRuntimeBinding", err)
			}
			if !reflect.DeepEqual(operations, []string{"fence", "execution", "profile"}) {
				t.Fatalf("dependency operations = %v, want no workspace side effect", operations)
			}
		})
	}
}

func TestLauncherRejectsReviewerMiseEnvironmentOverrideBeforeStartingDocker(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", HeadRef: "feature", HeadSHA: runtimeTestPRHeadSHA,
	}
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleReviewer, proposal)
	miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
	gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: acp.MCPServer{Type: "http", Name: "omnigrex", URL: "http://mcp/mcp"}}}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store:    &runtimeStore{operations: &operations, execution: execution},
		Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations, activation: workspace.MiseActivation{
			DataDir: miseDir, SourceRevision: runtimeTestDefaultSHA,
			Environment: map[string]string{"MISE_DATA_DIR": miseDir, "OPENCODE_DISABLE_PROJECT_CONFIG": "false"},
		}},
		Gateway: gateway, Docker: &runtimeEngineFactory{operations: &operations},
		ACP:      &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}},
		Sessions: &runtimeSessionPreparer{operations: &operations},
		Network:  "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "unused",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, opencode.ErrInvalidProcess) {
		t.Fatalf("Launch() error = %v, want ErrInvalidProcess", err)
	}
	if gateway.drained != 1 || slices.Contains(operations, "docker-new") {
		t.Fatalf("dependency operations = %v and drains = %d", operations, gateway.drained)
	}
}

func TestRuntimeHandleCleanupIsOrderedAndIdempotent(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	operations = operations[:0]
	resources.store.operations = &operations
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() second error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup operations = %v, want %v", operations, want)
	}
	if resources.gateway.drained != 1 || resources.client.closed != 1 || resources.engineFactory.engine.process.stopped != 1 ||
		resources.engineFactory.engine.process.removed != 1 || resources.engineFactory.engine.closed != 1 {
		t.Errorf("cleanup counts: gateway=%d client=%d stop=%d remove=%d engine=%d", resources.gateway.drained,
			resources.client.closed, resources.engineFactory.engine.process.stopped, resources.engineFactory.engine.process.removed, resources.engineFactory.engine.closed)
	}
}

func TestRuntimeHandleCloseMCPIsIdempotentAcrossCleanup(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.CloseMCP(context.Background()); err != nil {
		t.Fatalf("CloseMCP() error = %v", err)
	}
	if err := handle.CloseMCP(context.Background()); err != nil {
		t.Fatalf("CloseMCP() second error = %v", err)
	}
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("close and cleanup operations = %v, want %v", operations, want)
	}
	if resources.gateway.drained != 1 {
		t.Fatalf("CloseAndDrain calls = %d, want one", resources.gateway.drained)
	}
}

func TestRuntimeHandleCleanupRetriesMCPDrainBeforeTearingDownRuntime(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	drainErr := context.DeadlineExceeded
	resources.gateway.drainErr = drainErr
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	err := handle.Cleanup(context.Background())
	if !errors.Is(err, drainErr) {
		t.Fatalf("Cleanup() error = %v, want drain timeout", err)
	}
	want := []string{"mcp-close-and-drain"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup operations = %v, want %v", operations, want)
	}

	resources.gateway.drainErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want = []string{"mcp-close-and-drain", "mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup retry operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupRetriesACPClosureWithoutRepeatingSuccessfulSteps(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	closeErr := errors.New("ACP close failed")
	resources.client.closeErr = closeErr
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("Cleanup() error = %v, want ACP close failure", err)
	}
	resources.client.closeErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "docker-close", "acp-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup retry operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupRetriesProcessStopAndRemovalBeforeClosingEngine(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	stopErr := errors.New("process stop failed")
	removeErr := errors.New("process removal failed")
	resources.engineFactory.engine.process.stopErr = stopErr
	resources.engineFactory.engine.process.removeErr = removeErr
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, stopErr) || !errors.Is(err, removeErr) {
		t.Fatalf("Cleanup() error = %v, want stop and removal failures", err)
	}
	if slices.Contains(operations, "docker-close") {
		t.Fatalf("Docker Engine closed while process remains: %v", operations)
	}
	resources.engineFactory.engine.process.stopErr = nil
	resources.engineFactory.engine.process.removeErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "process-stop", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup retry operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupRetriesReviewerWorkspaceDiscard(t *testing.T) {
	operations := []string{}
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	handle, resources, err := launchRuntimeForRoleCleanupFailure(t, &operations, workflow.RoleReviewer, proposal, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	discardErr := errors.New("workspace discard failed")
	resources.workspace.discardErr = discardErr
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.workspace.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, discardErr) {
		t.Fatalf("Cleanup() error = %v, want workspace discard failure", err)
	}
	resources.workspace.discardErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "workspace-discard", "docker-close", "workspace-discard"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup retry operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupRetriesDockerEngineClosure(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	closeErr := errors.New("Docker Engine close failed")
	resources.engineFactory.engine.closeErr = closeErr
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("Cleanup() error = %v, want Docker Engine close failure", err)
	}
	resources.engineFactory.engine.closeErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "docker-close", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup retry operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupSerializesConcurrentCalls(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	start := make(chan struct{})
	errorsByCall := make(chan error, 32)
	for range 32 {
		go func() {
			<-start
			errorsByCall <- handle.Cleanup(context.Background())
		}()
	}
	close(start)
	for range 32 {
		if err := <-errorsByCall; err != nil {
			t.Errorf("concurrent Cleanup() error = %v", err)
		}
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("concurrent cleanup operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupDiscardsReviewerWorkspaceAfterProcessRemoval(t *testing.T) {
	operations := []string{}
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	handle, resources, err := launchRuntimeForRoleCleanupFailure(t, &operations, workflow.RoleReviewer, proposal, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.workspace.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "workspace-discard", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("Reviewer cleanup operations = %v, want %v", operations, want)
	}
	if resources.workspace.discardedAssignmentID != runtimeTestAssignment {
		t.Errorf("discarded assignment = %q, want %q", resources.workspace.discardedAssignmentID, runtimeTestAssignment)
	}
}

func TestRuntimeHandleCleanupKeepsReviewerWorkspaceWhenProcessRemovalFails(t *testing.T) {
	operations := []string{}
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	handle, resources, err := launchRuntimeForRoleCleanupFailure(t, &operations, workflow.RoleReviewer, proposal, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	removeErr := errors.New("process remains mounted")
	resources.engineFactory.engine.process.removeErr = removeErr
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.workspace.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, removeErr) {
		t.Fatalf("Cleanup() error = %v, want process removal error", err)
	}
	if slices.Contains(operations, "workspace-discard") {
		t.Fatalf("Reviewer workspace discarded while process remains: %v", operations)
	}
	if slices.Contains(operations, "docker-close") {
		t.Fatalf("Docker Engine closed while process remains: %v", operations)
	}
	resources.engineFactory.engine.process.removeErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "process-remove", "workspace-discard", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("Reviewer cleanup retry operations = %v, want %v", operations, want)
	}
}

func TestLauncherFailureCleansResourcesAndRedactsEveryCredential(t *testing.T) {
	operations := []string{}
	failure := errors.New("failed with repository-secret provider-secret mcp-secret")
	handle, resources, err := launchRuntimeForCleanupFailure(t, &operations, failure)
	if err == nil {
		t.Fatal("Launch() error = nil")
	}
	if handle != nil {
		t.Fatalf("Launch() handle = %#v, want nil", handle)
	}
	for _, secret := range []string{runtimeTestCredential, runtimeTestProviderKey, "mcp-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("Launch() error exposes credential %q: %v", secret, err)
		}
	}
	wantSuffix := []string{"session-prepare", "mcp-close-and-drain", "acp-close", "process-stop", "process-remove", "docker-close"}
	if len(operations) < len(wantSuffix) || !reflect.DeepEqual(operations[len(operations)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("failure operations = %v, want suffix %v", operations, wantSuffix)
	}
	if resources.gateway.drained != 1 || resources.client.closed != 1 || resources.engineFactory.engine.process.removed != 1 || resources.engineFactory.engine.closed != 1 {
		t.Errorf("failure cleanup did not release all resources")
	}
}

type runtimeLaunchResources struct {
	store         *runtimeStore
	workspace     *runtimeWorkspace
	gateway       *runtimeGateway
	engineFactory *runtimeEngineFactory
	client        *runtimeACPClient
}

func launchRuntimeForCleanupTest(t *testing.T, operations *[]string, sessionErr error) (*agentturn.RuntimeHandle, *runtimeLaunchResources) {
	t.Helper()
	handle, resources, err := launchRuntimeForCleanupFailure(t, operations, sessionErr)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	return handle, resources
}

func launchRuntimeForCleanupFailure(t *testing.T, operations *[]string, sessionErr error) (*agentturn.RuntimeHandle, *runtimeLaunchResources, error) {
	return launchRuntimeForRoleCleanupFailure(t, operations, workflow.RoleDeveloper, nil, sessionErr)
}

func launchRuntimeForRoleCleanupFailure(t *testing.T, operations *[]string, role workflow.Role, proposal *store.AgentTurnChangeProposal, sessionErr error) (*agentturn.RuntimeHandle, *runtimeLaunchResources, error) {
	t.Helper()
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, role, proposal)
	miseDir := "/srv/mise/assignment-" + runtimeTestAssignment + "/mise"
	database := &runtimeStore{operations: operations, execution: execution}
	workspaces := &runtimeWorkspace{operations: operations, activation: workspace.MiseActivation{
		DataDir: miseDir, SourceRevision: runtimeTestDefaultSHA, Environment: map[string]string{"MISE_DATA_DIR": miseDir},
	}}
	gateway := &runtimeGateway{operations: operations, registration: mcp.Registration{Server: acp.MCPServer{
		Type: "http", Name: "omnigrex", URL: "http://mcp:8080/mcp",
		Headers: []acp.EnvironmentEntry{{Name: "Authorization", Value: "Bearer mcp-secret"}},
	}}}
	engineFactory := &runtimeEngineFactory{operations: operations}
	client := &runtimeACPClient{operations: operations}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: operations, runtimeProfile: runtimeProfile},
		Workspace: workspaces,
		Gateway:   gateway, Docker: engineFactory, ACP: &runtimeACPFactory{operations: operations, client: client},
		Sessions: &runtimeSessionPreparer{operations: operations, err: sessionErr},
		Network:  "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
	})
	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	return handle, &runtimeLaunchResources{store: database, workspace: workspaces, gateway: gateway, engineFactory: engineFactory, client: client}, err
}

func runtimeLauncher(t *testing.T, config agentturn.LauncherConfig) *agentturn.Launcher {
	t.Helper()
	launcher, err := agentturn.NewLauncher(config)
	if err != nil {
		t.Fatalf("NewLauncher() error = %v", err)
	}
	return launcher
}

func runtimeLauncherProfile(t *testing.T) profile.Profile {
	t.Helper()
	value, err := profile.NewOpenCodeV1(runtimeTestImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewOpenCodeV1() error = %v", err)
	}
	return value
}

func runtimeExecutionContext(t *testing.T, runtimeProfile profile.Profile, role workflow.Role, proposal *store.AgentTurnChangeProposal) (store.AgentTurnExecutionContext, store.AgentTurnLease) {
	t.Helper()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	config := json.RawMessage(`{"instructions":"Work carefully.","model":"openai/gpt-5","name":"developer","path":".omnigrex/team/developer.md","permissions":{"bash":"allow","edit":"allow","read":"allow"},"role":"DEVELOPER","runtime":"opencode-acp/v1","steps":50}`)
	profileName := "developer"
	if role == workflow.RoleReviewer {
		profileName = "reviewer"
		config = json.RawMessage(`{"instructions":"Review carefully.","model":"openai/gpt-5","name":"reviewer","path":".omnigrex/team/reviewer.md","permissions":{"bash":"allow","edit":"deny","read":"allow"},"role":"REVIEWER","runtime":"opencode-acp/v1","steps":50}`)
	}
	binding := store.AssignmentRuntimeBinding{
		AgentProfileName: profileName, RuntimeProfileName: "opencode-acp", RuntimeProfileVersion: "v1",
		RuntimeProfileContentSHA256: runtimeProfile.ContentSHA256(), RuntimeImageDigest: runtimeTestImage,
	}
	turn := store.AgentTurn{
		AgentTurnSpec: store.AgentTurnSpec{AgentSessionID: runtimeTestSession, ControlRevision: 9, AgentProfileConfig: config},
		ID:            runtimeTestTurn, AgentAssignmentID: runtimeTestAssignment, ExecutionEpoch: 7, CreatedAt: now,
	}
	if proposal != nil {
		turn.ChangeProposalID = proposal.ID
		turn.ExpectedHeadSHA = proposal.HeadSHA
	}
	lease := store.AgentTurnLease{
		AgentTurn: turn,
		JobLease: store.JobLease{Job: store.Job{ID: "50000000-0000-4000-8000-000000000001", Status: store.JobLeased, JobSpec: store.JobSpec{
			WorkflowID: runtimeTestWorkflow, AgentAssignmentID: runtimeTestAssignment, AgentSessionID: runtimeTestSession,
			AgentTurnID: runtimeTestTurn, ExecutionEpoch: 7,
		}}, Attempt: 1},
		OwnerID: "runtime-owner", OwnerToken: "owner-token", LeaseExpiresAt: now.Add(time.Minute),
	}
	execution := store.AgentTurnExecutionContext{
		WorkflowID: runtimeTestWorkflow,
		Repository: store.AgentTurnRepository{ID: 41, Owner: "acme", Name: "widgets"},
		Issue:      store.AgentTurnIssue{ID: 51, Number: 17}, ChangeProposal: proposal,
		Assignment: store.AgentAssignment{AssignmentRuntimeBinding: binding, ID: runtimeTestAssignment, WorkflowID: runtimeTestWorkflow, Role: role, Status: store.AgentAssignmentActive, RuntimeStatePath: "assignment-" + runtimeTestAssignment + "/runtime-state"},
		Session: store.AgentSession{ID: runtimeTestSession, AgentAssignmentID: runtimeTestAssignment, RuntimeProfileName: binding.RuntimeProfileName,
			RuntimeProfileVersion: binding.RuntimeProfileVersion, RuntimeProfileContentSHA256: binding.RuntimeProfileContentSHA256,
			RuntimeImageDigest: binding.RuntimeImageDigest, RuntimeStatePath: "assignment-" + runtimeTestAssignment + "/runtime-state",
			Status: store.AgentSessionCreating, ControlRevision: 9, NextExecutionEpoch: 8},
		Turn: turn,
	}
	return execution, lease
}

type runtimeStore struct {
	operations         *[]string
	execution          store.AgentTurnExecutionContext
	fenceErr           error
	fenceErrors        []error
	fenceCalls         int
	executionErr       error
	turnFence          sync.Mutex
	withFenceAttempted chan struct{}
	withFenceErr       error
}

func (database *runtimeStore) ValidateTurnFence(context.Context, store.AgentTurnLease) error {
	*database.operations = append(*database.operations, "fence")
	if database.fenceCalls < len(database.fenceErrors) {
		err := database.fenceErrors[database.fenceCalls]
		database.fenceCalls++
		return err
	}
	database.fenceCalls++
	return database.fenceErr
}

func (database *runtimeStore) GetAgentTurnExecutionContext(context.Context, store.AgentTurnLease) (store.AgentTurnExecutionContext, error) {
	*database.operations = append(*database.operations, "execution")
	return database.execution, database.executionErr
}

func (database *runtimeStore) WithAgentTurnFence(ctx context.Context, _ store.AgentTurnLease, operation func(context.Context) error) error {
	*database.operations = append(*database.operations, "turn-fence")
	if database.withFenceAttempted != nil {
		close(database.withFenceAttempted)
	}
	database.turnFence.Lock()
	defer database.turnFence.Unlock()
	if database.withFenceErr != nil {
		return database.withFenceErr
	}
	return operation(ctx)
}

func (database *runtimeStore) acquireRecoveryLock(attempted, acquired chan<- struct{}) {
	close(attempted)
	database.turnFence.Lock()
	close(acquired)
	database.turnFence.Unlock()
}

type runtimeRegistry struct {
	operations     *[]string
	runtimeProfile profile.Profile
	err            error
}

func (registry *runtimeRegistry) Resolve(string, string) (profile.Profile, error) {
	*registry.operations = append(*registry.operations, "profile")
	return registry.runtimeProfile, registry.err
}

type runtimeWorkspace struct {
	operations            *[]string
	checkout              workspace.Checkout
	mise                  workspace.MiseProvision
	activation            workspace.MiseActivation
	workspaceErr          error
	miseErr               error
	discardedAssignmentID string
	discardErr            error
}

func (lifecycle *runtimeWorkspace) PrepareWorkspace(_ context.Context, checkout workspace.Checkout) (workspace.Paths, error) {
	*lifecycle.operations = append(*lifecycle.operations, "workspace")
	lifecycle.checkout = checkout
	return workspace.Paths{Workspace: "/srv/workspace", Mise: lifecycle.activation.DataDir}, lifecycle.workspaceErr
}

func (lifecycle *runtimeWorkspace) ProvisionMise(_ context.Context, provision workspace.MiseProvision) (workspace.MiseActivation, error) {
	*lifecycle.operations = append(*lifecycle.operations, "mise")
	lifecycle.mise = provision
	return lifecycle.activation, lifecycle.miseErr
}

func (lifecycle *runtimeWorkspace) DiscardWorkspace(assignmentID string) error {
	*lifecycle.operations = append(*lifecycle.operations, "workspace-discard")
	lifecycle.discardedAssignmentID = assignmentID
	return lifecycle.discardErr
}

type runtimeGateway struct {
	operations   *[]string
	scope        mcp.TokenScope
	registration mcp.Registration
	err          error
	revoked      int
	drained      int
	drainErr     error
}

func (gateway *runtimeGateway) Register(scope mcp.TokenScope) (mcp.Registration, error) {
	*gateway.operations = append(*gateway.operations, "mcp-register")
	gateway.scope = scope
	return gateway.registration, gateway.err
}

func (gateway *runtimeGateway) Revoke(mcp.Registration) bool {
	*gateway.operations = append(*gateway.operations, "mcp-revoke")
	gateway.revoked++
	return true
}

func (gateway *runtimeGateway) CloseAndDrain(context.Context, mcp.Registration) error {
	*gateway.operations = append(*gateway.operations, "mcp-close-and-drain")
	gateway.drained++
	return gateway.drainErr
}

type runtimeEngineFactory struct {
	operations *[]string
	options    dockerruntime.EngineOptions
	engine     runtimeEngine
	err        error
}

func (factory *runtimeEngineFactory) New(options dockerruntime.EngineOptions) (agentturn.RuntimeEngine, error) {
	*factory.operations = append(*factory.operations, "docker-new")
	factory.options = options
	factory.engine.operations = factory.operations
	if factory.engine.process.transport == nil {
		factory.engine.process.transport = &runtimeTransport{}
	}
	return &factory.engine, factory.err
}

type runtimeEngine struct {
	operations *[]string
	spec       dockerruntime.Spec
	process    runtimeProcess
	createErr  error
	closed     int
	closeErr   error
}

func (engine *runtimeEngine) Create(_ context.Context, spec dockerruntime.Spec, _ io.Writer) (agentturn.RuntimeProcess, error) {
	*engine.operations = append(*engine.operations, "docker-create")
	engine.spec = spec
	engine.process.operations = engine.operations
	return &engine.process, engine.createErr
}

func (engine *runtimeEngine) Close() error {
	*engine.operations = append(*engine.operations, "docker-close")
	engine.closed++
	return engine.closeErr
}

type runtimeProcess struct {
	operations   *[]string
	transport    io.ReadWriteCloser
	stopped      int
	removed      int
	stopErr      error
	removeErr    error
	startErr     error
	started      int
	startEntered chan<- struct{}
	releaseStart <-chan struct{}
}

func (process *runtimeProcess) Transport() io.ReadWriteCloser { return process.transport }
func (process *runtimeProcess) Start(context.Context) error {
	*process.operations = append(*process.operations, "process-start")
	process.started++
	if process.startEntered != nil {
		close(process.startEntered)
	}
	if process.releaseStart != nil {
		<-process.releaseStart
	}
	return process.startErr
}
func (process *runtimeProcess) Stop(context.Context, time.Duration) error {
	*process.operations = append(*process.operations, "process-stop")
	process.stopped++
	return process.stopErr
}
func (process *runtimeProcess) Remove(context.Context) error {
	*process.operations = append(*process.operations, "process-remove")
	process.removed++
	return process.removeErr
}

type runtimeTransport struct{}

func (*runtimeTransport) Read([]byte) (int, error)       { return 0, io.EOF }
func (*runtimeTransport) Write(data []byte) (int, error) { return len(data), nil }
func (*runtimeTransport) Close() error                   { return nil }

type runtimeACPFactory struct {
	operations *[]string
	transport  io.ReadWriteCloser
	options    acp.ClientOptions
	client     agentturn.RuntimeACPClient
}

func (factory *runtimeACPFactory) New(transport io.ReadWriteCloser, options acp.ClientOptions) agentturn.RuntimeACPClient {
	*factory.operations = append(*factory.operations, "acp-new")
	factory.transport = transport
	factory.options = options
	return factory.client
}

type runtimeACPClient struct {
	operations *[]string
	closed     int
	prompts    int
	closeErr   error
}

func (*runtimeACPClient) SetAgentEventContext(agentevent.Context) {}
func (*runtimeACPClient) Initialize(context.Context) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{}, nil
}
func (*runtimeACPClient) RecoverCreatedSession(context.Context, string) (acp.SessionInfo, error) {
	return acp.SessionInfo{}, nil
}
func (*runtimeACPClient) CreateSession(context.Context, acp.CreateSessionRequest) (acp.Session, error) {
	return acp.Session{}, nil
}
func (*runtimeACPClient) ContinueSessionWithOptions(context.Context, acp.ContinueSessionRequest) ([]json.RawMessage, error) {
	return nil, nil
}
func (*runtimeACPClient) SetConfigOption(context.Context, string, string, any) ([]json.RawMessage, error) {
	return nil, nil
}

func (client *runtimeACPClient) Prompt(context.Context, string, []acp.ContentBlock) (acp.PromptResponse, error) {
	client.prompts++
	return acp.PromptResponse{}, nil
}
func (client *runtimeACPClient) Close() error {
	if client.operations != nil {
		*client.operations = append(*client.operations, "acp-close")
	}
	client.closed++
	return client.closeErr
}

type runtimeSessionPreparer struct {
	operations *[]string
	request    session.PrepareRequest
	result     session.Result
	err        error
}

func (preparer *runtimeSessionPreparer) Prepare(_ context.Context, request session.PrepareRequest) (session.Result, error) {
	*preparer.operations = append(*preparer.operations, "session-prepare")
	preparer.request = request
	return preparer.result, preparer.err
}

func assertRuntimeEnvironment(t *testing.T, entries []string, wanted map[string]string) {
	t.Helper()
	actual := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("invalid environment entry %q", entry)
		}
		actual[name] = value
	}
	for name, value := range wanted {
		if actual[name] != value {
			t.Errorf("environment %s = %q, want %q", name, actual[name], value)
		}
	}
}

func stringMap(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for name, value := range values {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "\n")
}
