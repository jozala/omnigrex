package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/turnconfig"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

const (
	runtimeTestImage       = "registry.example/omnigrex/pi@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
	execution.Assignment.AgentProfileName = "implementation-specialist"
	var profileConfig map[string]any
	if err := json.Unmarshal(execution.Turn.AgentProfileConfig, &profileConfig); err != nil {
		t.Fatal(err)
	}
	profileConfig["name"] = "implementation-specialist"
	profileConfig["path"] = ".omnigrex/team/arbitrary profile filename.md"
	execution.Turn.AgentProfileConfig, _ = json.Marshal(profileConfig)
	database := &runtimeStore{operations: &operations, execution: execution}
	workspaces := &runtimeWorkspace{operations: &operations}
	gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: agent.MCPEndpoint{
		Name: "omnigrex", URL: "http://mcp:8080/mcp",
		Headers: []agent.Header{{Name: "Authorization", Value: "Bearer mcp-secret"}},
	}}}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	client := &runtimeTurnConnection{operations: &operations}
	adapter := &runtimeTurnAdapter{operations: &operations, connection: client}
	states := &runtimeRetainedState{}
	credentials := &runtimeCredentialProvider{operations: &operations, credential: "fresh-repository-secret"}
	launcherConfig := agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: workspaces, Gateway: gateway, Execution: engineFactory,
		States: states, AgentAdapters: runtimeAdapters(adapter),
		DeveloperCredentials: credentials, ReviewerCredentials: credentials,
		MemoryBytes: 1024 << 20,
	}
	withoutMemory := launcherConfig
	withoutMemory.MemoryBytes = 0
	if _, err := agentturn.NewLauncher(withoutMemory); !errors.Is(err, agentturn.ErrInvalidLauncher) {
		t.Fatalf("NewLauncher() with no memory error = %v, want ErrInvalidLauncher", err)
	}
	for _, missing := range []string{"retained state", "agent adapters", "nil adapter", "Developer credentials", "Reviewer credentials"} {
		invalid := launcherConfig
		switch missing {
		case "retained state":
			invalid.States = nil
		case "agent adapters":
			invalid.AgentAdapters = nil
		case "nil adapter":
			invalid.AgentAdapters = runtimeAdapters(nil)
		case "Developer credentials":
			invalid.DeveloperCredentials = nil
		case "Reviewer credentials":
			invalid.ReviewerCredentials = nil
		}
		if _, err := agentturn.NewLauncher(invalid); !errors.Is(err, agentturn.ErrInvalidLauncher) {
			t.Fatalf("NewLauncher() with missing %s error = %v, want ErrInvalidLauncher", missing, err)
		}
	}
	launcher := runtimeLauncher(t, launcherConfig)
	var renewal agentturn.MCPRenewal

	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
		PublishMCPRenewal: func(published agentturn.MCPRenewal) {
			if !slices.Contains(operations, "mcp-register") {
				t.Fatal("MCP renewal published before registration")
			}
			renewal = published
		},
	})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	t.Cleanup(func() { _ = handle.Cleanup(context.Background()) })

	if len(operations) == 0 || operations[0] != "fence" {
		t.Fatalf("dependency operations = %v, want fence first", operations)
	}
	wantPrefix := []string{"fence", "execution", "profile", "workspace", "refresh", "mcp-register", "repository-credential", "docker-new", "docker-create", "turn-fence", "process-start", "fence", "session-prepare"}
	if !reflect.DeepEqual(operations, wantPrefix) {
		t.Fatalf("dependency operations = %v, want %v", operations, wantPrefix)
	}
	if workspaces.checkout.Revision != runtimeTestDefaultSHA {
		t.Errorf("workspace revision = %q, want default SHA", workspaces.checkout.Revision)
	}
	if workspaces.checkout.AssignmentID != runtimeTestAssignment || workspaces.checkout.Credential != runtimeTestCredential {
		t.Errorf("workspace checkout = %#v", workspaces.checkout)
	}
	if key := workspaces.preparationKey; key.AssignmentID != runtimeTestAssignment || key.AgentSessionID != runtimeTestSession ||
		key.AgentTurnID != runtimeTestTurn || key.ExecutionEpoch != 7 || key.RuntimeProfileName != "pi-rpc" || key.RuntimeProfileVersion != "v2" {
		t.Fatalf("workspace preparation lost full execution identity: %#v", key)
	}
	if gateway.scope.WorkflowID != runtimeTestWorkflow || gateway.scope.Role != workflow.RoleDeveloper ||
		gateway.scope.Branch != "omnigrex/issue-17" || gateway.scope.DefaultBranch != "trunk" || gateway.scope.HeadSHA != runtimeTestDefaultSHA ||
		gateway.scope.PullRequest != nil || gateway.scope.ExpiresAt != handle.CurrentLease().LeaseExpiresAt ||
		gateway.scope.ExpiresAt != lease.LeaseExpiresAt.Add(time.Minute) || gateway.scope.Lease.ExecutionEpoch != 7 {
		t.Errorf("MCP token scope = %#v", gateway.scope)
	}
	if renewal == nil {
		t.Fatal("Launch() did not publish MCP renewal authority")
	}
	renewedExpiry := handle.CurrentLease().LeaseExpiresAt.Add(time.Minute)
	if !renewal(renewedExpiry) || !slices.Equal(gateway.renewals, []time.Time{renewedExpiry}) {
		t.Fatalf("published MCP renewal expiries = %v, want %s", gateway.renewals, renewedExpiry)
	}
	gateway.drainStarted = make(chan struct{})
	gateway.drainRelease = make(chan struct{})
	closeResult := make(chan error, 1)
	go func() { closeResult <- handle.CloseMCP(context.Background()) }()
	<-gateway.drainStarted
	renewResult := make(chan bool, 1)
	go func() { renewResult <- renewal(renewedExpiry.Add(time.Minute)) }()
	select {
	case <-renewResult:
		t.Fatal("MCP renewal raced past an in-progress close")
	default:
	}
	close(gateway.drainRelease)
	if err := <-closeResult; err != nil {
		t.Fatalf("CloseMCP() error = %v", err)
	}
	if !<-renewResult || !slices.Equal(gateway.renewals, []time.Time{renewedExpiry}) {
		t.Fatalf("post-close MCP renewal reached gateway: %v", gateway.renewals)
	}

	spec := engineFactory.engine.spec
	if spec.MemoryBytes != 1073741824 || engineFactory.options.RuntimePolicy.MemoryBytes != spec.MemoryBytes {
		t.Errorf("Agent Turn policy/spec memory = %d/%d, want 1073741824", engineFactory.options.RuntimePolicy.MemoryBytes, spec.MemoryBytes)
	}
	wantLabels := map[string]string{
		dockerruntime.RuntimeProcessSessionLabel:    runtimeTestSession,
		dockerruntime.RuntimeProcessTurnLabel:       runtimeTestTurn,
		dockerruntime.RuntimeProcessAssignmentLabel: runtimeTestAssignment,
		dockerruntime.RuntimeProcessEpochLabel:      "7",
		"io.omnigrex.runtime-profile":               "pi-rpc/v2",
		"io.omnigrex.workflow":                      runtimeTestWorkflow,
		"io.omnigrex.execution-specification":       spec.Labels["io.omnigrex.execution-specification"],
	}
	if len(spec.Labels["io.omnigrex.execution-specification"]) != 64 {
		t.Fatal("execution specification digest is missing")
	}
	if !reflect.DeepEqual(spec.Labels, wantLabels) {
		t.Errorf("Docker labels = %#v, want %#v", spec.Labels, wantLabels)
	}
	wantVolumes := []dockerruntime.VolumeMount{
		{Name: "mise", Subpath: "assignment-" + runtimeTestAssignment + "/mise", Target: environment.DataDirectory},
		{Name: "runtime-state", Subpath: "assignment-" + runtimeTestAssignment + "/runtime-state", Target: "/state"},
		{Name: "mise", Subpath: "assignment-" + runtimeTestAssignment + "/tool-data", Target: filepath.Join(filepath.Dir(environment.DataDirectory), "omnigrex-tool-data")},
		{Name: "workspaces", Subpath: "assignment-" + runtimeTestAssignment + "/workspace", Target: "/workspace"},
	}
	if !reflect.DeepEqual(spec.Volumes, wantVolumes) {
		t.Errorf("Docker volumes = %#v, want %#v", spec.Volumes, wantVolumes)
	}
	if !slices.Equal(spec.Command, append([]string{"/usr/local/bin/omnigrex-environment", "run"}, runtimeProfile.Contract().Command...)) {
		t.Fatalf("runtime did not bootstrap in-environment: %v", spec.Command)
	}
	if credentials.calls != 1 || credentials.owner != "acme" || credentials.repository != "widgets" {
		t.Fatalf("post-checkout credential refresh = %#v, want one request for the bound repository", credentials)
	}
	for _, secret := range []string{runtimeTestCredential, credentials.credential, "mcp-secret"} {
		if strings.Contains(strings.Join(spec.Environment, "\n"), secret) || strings.Contains(stringMap(spec.Labels), secret) {
			t.Errorf("Docker contract contains non-provider credential %q", secret)
		}
	}
	if engineFactory.options.AgentNetwork != "omnigrex-agent" || !reflect.DeepEqual(engineFactory.options.RuntimePolicy.Labels, spec.Labels) {
		t.Errorf("Docker Engine options = %#v", engineFactory.options)
	}
	if !reflect.DeepEqual(adapter.request.MCP, gateway.registration.Server) {
		t.Errorf("Session MCP endpoint = %#v, want registered descriptor", adapter.request.MCP)
	}
	if strings.Contains(string(adapter.request.Credentials), credentials.credential) {
		t.Fatal("agent adapter received GitHub credentials")
	}
	if adapter.request.Lease.ExecutionEpoch != 7 || adapter.request.Session.ID != runtimeTestSession ||
		adapter.request.State.ParticipantID != runtimeTestAssignment || adapter.request.Transport != engineFactory.engine.process.transport ||
		adapter.request.ProfileName != "implementation-specialist" || !slices.Equal(adapter.request.Profile, execution.Turn.AgentProfileConfig) {
		t.Errorf("Session Prepare request = %#v", adapter.request)
	}
	if len(states.ensured) != 1 || states.ensured[0] != adapter.request.State || adapter.request.State != engineFactory.request.State ||
		adapter.request.State.RuntimeProfileSHA256 != runtimeProfile.ContentSHA256() || adapter.request.State.Image != runtimeTestImage {
		t.Fatal("retained state attachment lost the Session's exact Runtime Profile binding")
	}
	if handle.AgentConnection() != client || client.prompts != 0 {
		t.Errorf("Runtime handle = %#v", handle)
	}
}

func TestLauncherInjectsApprovedDiskBackedPathsIntoTurn(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	execution.Turn.TurnConfiguration = json.RawMessage(`{"directories":{"build":"turn","go-cache":"assignment"},"environment":{"TMPDIR":"build","GOTMPDIR":"build","GOCACHE":"go-cache"}}`)
	database := &runtimeStore{operations: &operations, execution: execution}
	workspaces := &runtimeWorkspace{operations: &operations}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: workspaces, Gateway: &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: agent.MCPEndpoint{Name: "omnigrex", URL: "http://mcp:8080/mcp"}}},
		Execution:                engineFactory,
		MemoryBytes:              1024 << 20,
		PathEnvironmentAllowlist: []string{"TMPDIR", "GOTMPDIR", "GOCACHE"},
	})
	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantScratch := filepath.Join(filepath.Dir(environment.DataDirectory), "omnigrex-tool-data/turn", runtimeTestTurn, "build")
	assertRuntimeEnvironment(t, engineFactory.engine.spec.Environment, map[string]string{
		"TMPDIR": wantScratch, "GOTMPDIR": wantScratch,
		"GOCACHE": filepath.Join(filepath.Dir(environment.DataDirectory), "omnigrex-tool-data/assignment/go-cache"),
	})
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workspaces.cleanedTurnID != runtimeTestTurn {
		t.Fatalf("Agent Turn scratch cleanup targeted %q, want %q", workspaces.cleanedTurnID, runtimeTestTurn)
	}
}

func TestLauncherLaunchesRecoveredDeveloperAtPublishedHeadWithoutReadyChangeProposal(t *testing.T) {
	operations := []string{}
	profile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, profile, workflow.RoleDeveloper, nil)
	execution.Publication = &store.AgentTurnPublication{
		HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA, BaseRef: "trunk",
		PullRequestID: 123, PullRequestNumber: 18, PullRequestNodeID: "PR_123",
		SourcePublishMutationID: "10000000-0000-4000-8000-000000000001",
		SourceOpenPRMutationID:  "10000000-0000-4000-8000-000000000002",
	}
	workspaces := &runtimeWorkspace{operations: &operations}
	gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: agent.MCPEndpoint{
		Name: "omnigrex", URL: "http://mcp:8080/mcp",
		Headers: []agent.Header{{Name: "Authorization", Value: "Bearer mcp-secret"}},
	}}}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store:     &runtimeStore{operations: &operations, execution: execution},
		Registry:  &runtimeRegistry{operations: &operations, runtimeProfile: profile},
		Workspace: workspaces, Gateway: gateway, Execution: &runtimeEngineFactory{operations: &operations},
		MemoryBytes: 1024 << 20,
	})
	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Cleanup(context.Background()) })
	if workspaces.checkout.Revision != runtimeTestPRHeadSHA || gateway.scope.HeadSHA != runtimeTestPRHeadSHA ||
		gateway.scope.PullRequest == nil || gateway.scope.PullRequest.Number != 18 || !gateway.scope.BranchExists || !gateway.scope.RecoveredPublication ||
		gateway.scope.Branch != "omnigrex/issue-17" {
		t.Fatalf("recovered checkout %#v, scope %#v", workspaces.checkout, gateway.scope)
	}
}

func TestLauncherUsesWorkingRevisionForBothRolesAndInEnvironmentTooling(t *testing.T) {
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	for _, testCase := range []struct {
		name string
		role workflow.Role
	}{
		{name: "returning Developer", role: workflow.RoleDeveloper},
		{name: "Reviewer", role: workflow.RoleReviewer},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			operations := []string{}
			runtimeProfile := runtimeLauncherProfile(t)
			execution, lease := runtimeExecutionContext(t, runtimeProfile, testCase.role, proposal)
			instructionsDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(instructionsDir, "common.md"), []byte("CURRENT_OPERATOR_GUIDANCE"), 0o600); err != nil {
				t.Fatal(err)
			}
			operator, err := agentinstructions.Load(instructionsDir, role.BuiltinPolicyCatalog())
			if err != nil {
				t.Fatal(err)
			}
			workspaces := &runtimeWorkspace{operations: &operations}
			gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: agent.MCPEndpoint{
				Name: "omnigrex", URL: "http://mcp:8080/mcp",
			}}}
			engineFactory := &runtimeEngineFactory{operations: &operations}
			adapter := &runtimeTurnAdapter{operations: &operations}
			launcher := runtimeLauncher(t, agentturn.LauncherConfig{
				Store:     &runtimeStore{operations: &operations, execution: execution},
				Registry:  &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
				Workspace: workspaces, Gateway: gateway, Execution: engineFactory,
				AgentAdapters:        runtimeAdapters(adapter),
				OperatorInstructions: operator,
			})

			handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
				Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
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
			if !slices.Equal(engineFactory.engine.spec.Command, append([]string{"/usr/local/bin/omnigrex-environment", "run"}, runtimeProfile.Contract().Command...)) {
				t.Fatal("tool setup was not delegated to the working checkout's environment")
			}
			if gateway.scope.PullRequest == nil || gateway.scope.PullRequest.ID != 61 || gateway.scope.PullRequest.Number != 23 ||
				gateway.scope.Branch != proposal.HeadRef || gateway.scope.HeadSHA != proposal.HeadSHA {
				t.Errorf("MCP Pull Request scope = %#v", gateway.scope)
			}
			if adapter.request.Stage.ID != execution.Turn.Stage || adapter.request.Purpose != execution.Turn.Purpose ||
				adapter.request.Policy.Role != testCase.role || !reflect.DeepEqual(adapter.request.Operator, operator) ||
				!slices.Equal(adapter.request.Profile, execution.Turn.AgentProfileConfig) {
				t.Fatal("launch omitted current deployment, Role, Profile, or Stage instructions")
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
		Execution: &runtimeEngineFactory{operations: &operations},
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
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
	database := &runtimeStore{operations: &operations, execution: execution, fenceErrors: []error{nil, store.ErrAgentTurnFenceLost}}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations},
		Gateway:   &runtimeGateway{operations: &operations}, Execution: &runtimeEngineFactory{operations: &operations},
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	want := []string{"fence", "execution", "profile", "workspace", "refresh"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("dependency operations = %v, want %v", operations, want)
	}
}

func TestLauncherDoesNotStartRuntimeProcessWhenLeaseBecomesStaleBeforeFenceAcquisition(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	database := &runtimeStore{operations: &operations, execution: execution, withFenceAttempted: make(chan struct{})}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations},
		Gateway:   &runtimeGateway{operations: &operations}, Execution: engineFactory,
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
			Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
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
	if engineFactory.engine.process.started != 0 || engineFactory.engine.process.removed != 0 || slices.Contains(operations, "session-prepare") {
		t.Fatalf("dependency operations = %v, want stopped resources retained after proof fence loss", operations)
	}
}

func TestLauncherHoldsAgentTurnFenceUntilRuntimeProcessStartFinishes(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	database := &runtimeStore{operations: &operations, execution: execution}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	engineFactory.engine.process.startEntered = startEntered
	engineFactory.engine.process.releaseStart = releaseStart
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations},
		Gateway:   &runtimeGateway{operations: &operations}, Execution: engineFactory,
	})

	launchResult := make(chan error, 1)
	go func() {
		handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
			Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
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
	database := &runtimeStore{operations: &operations, execution: execution}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	engineFactory.engine.process.startErr = store.ErrAgentTurnFenceLost
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations},
		Gateway:   &runtimeGateway{operations: &operations}, Execution: engineFactory,
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if engineFactory.engine.process.started != 1 || engineFactory.engine.process.removed != 1 || slices.Contains(operations, "session-prepare") {
		t.Fatalf("process lifecycle = started %d, removed %d, operations %v", engineFactory.engine.process.started, engineFactory.engine.process.removed, operations)
	}
}

func TestLauncherRevalidatesFenceImmediatelyAfterStartAndCleansBeforeReturn(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	database := &runtimeStore{operations: &operations, execution: execution, fenceErrors: []error{nil, nil, store.ErrAgentTurnFenceLost}}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations},
		Gateway:   &runtimeGateway{operations: &operations}, Execution: engineFactory,
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Launch() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if engineFactory.engine.process.started != 1 || engineFactory.engine.process.removed != 1 || slices.Contains(operations, "session-prepare") {
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
			execution.Assignment.RuntimeImageDigest = "registry.example/omnigrex/pi@sha256:" + strings.Repeat("b", 64)
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
				Execution: &runtimeEngineFactory{operations: &operations},
			})

			_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
				Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
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

func TestLauncherRejectsReviewerControlPathOverrideBeforeStartingDocker(t *testing.T) {
	operations := []string{}
	runtimeProfile := runtimeLauncherProfile(t)
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", HeadRef: "feature", HeadSHA: runtimeTestPRHeadSHA,
	}
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleReviewer, proposal)
	execution.Turn.TurnConfiguration = json.RawMessage(`{"directories":{"cache":"turn"},"environment":{"PI_CODING_AGENT_DIR":"cache"}}`)
	gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: agent.MCPEndpoint{Name: "omnigrex", URL: "http://mcp/mcp"}}}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store:     &runtimeStore{operations: &operations, execution: execution},
		Registry:  &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: &runtimeWorkspace{operations: &operations},
		Gateway:   gateway, Execution: &runtimeEngineFactory{operations: &operations},
	})

	_, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "unused",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if !errors.Is(err, agentturn.ErrRuntimeBinding) {
		t.Fatalf("Launch() error = %v, want ErrRuntimeBinding", err)
	}
	if gateway.drained != 0 || slices.Contains(operations, "docker-new") {
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close"}
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("close and cleanup operations = %v, want %v", operations, want)
	}
	if resources.gateway.drained != 1 {
		t.Fatalf("CloseAndDrain calls = %d, want one", resources.gateway.drained)
	}
}

func TestRuntimeHandleCaptureFailureRetainsEvidenceAndRetriesBeforeReviewerDiscard(t *testing.T) {
	operations := []string{}
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleReviewer, proposal, nil,
		func(resources *runtimeLaunchResources) {
			resources.store.execution.Turn.TurnConfiguration = json.RawMessage(`{"directories":{"scratch":"turn"},"environment":{"TMPDIR":"scratch"}}`)
		})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("terminal capture unavailable")
	resources.workspace.captureErr = failure
	operations = operations[:0]
	if err := handle.Cleanup(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("Cleanup() = %v, want capture failure", err)
	}
	process := &resources.engineFactory.engine.process
	if process.removed != 0 || resources.engineFactory.engine.closed != 0 || resources.workspace.discardedAssignmentID != "" ||
		resources.workspace.cleanedTurnID != "" {
		t.Fatalf("capture failure destroyed retry evidence: %v", operations)
	}
	wantRequest := execution.CaptureRequest{
		Workspace: execution.WorkspaceRef{Key: execution.Key{
			AssignmentID: runtimeTestAssignment, AgentSessionID: runtimeTestSession, AgentTurnID: runtimeTestTurn,
			ExecutionEpoch: 7, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2",
		}}, OperationID: runtimeTestTurn, Purpose: execution.TerminalCapture,
	}
	if !slices.Equal(resources.workspace.captureRequests, []execution.CaptureRequest{wantRequest}) {
		t.Fatalf("terminal capture identity = %#v, want %#v", resources.workspace.captureRequests, wantRequest)
	}
	resources.workspace.captureErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if process.stopped != 1 || process.removed != 1 || len(resources.workspace.captureRequests) != 2 ||
		resources.workspace.captureRequests[1] != wantRequest || resources.workspace.cleanedTurnID != runtimeTestTurn ||
		resources.workspace.discardedRef.Key != wantRequest.Workspace.Key {
		t.Fatalf("capture retry did not finish exactly once: %v", operations)
	}
	if !containsInOrder(operations, "mcp-close-and-drain", "process-stop", "workspace-capture", "process-remove", "workspace-discard") {
		t.Fatalf("invalid capture/remove/discard ordering: %v", operations)
	}
}

func TestRuntimeHandleRejectsMismatchedTerminalCaptureWithoutRemoval(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	resources.workspace.captureRef = execution.CaptureRef{
		Request: execution.CaptureRequest{Purpose: execution.PublicationCapture}, SHA256: strings.Repeat("a", 64),
	}
	if err := handle.Cleanup(context.Background()); !errors.Is(err, agentturn.ErrRuntimeBinding) {
		t.Fatalf("Cleanup() = %v, want invalid capture binding", err)
	}
	if resources.engineFactory.engine.process.removed != 0 || resources.engineFactory.engine.closed != 0 {
		t.Fatal("mismatched terminal evidence authorized destruction")
	}
	resources.workspace.captureRef = execution.CaptureRef{}
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeHandleFencesCaptureAdmissionAndAdoptionOutsideTransfer(t *testing.T) {
	for _, loseDuringCapture := range []bool{false, true} {
		t.Run(fmt.Sprint(loseDuringCapture), func(t *testing.T) {
			operations := []string{}
			handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
			if loseDuringCapture {
				resources.workspace.captureHook = func(context.Context) error {
					if resources.store.inCleanupFence {
						t.Fatal("capture transfer ran inside the cleanup transaction")
					}
					resources.store.withFenceErr = store.ErrAgentTurnFenceLost
					return nil
				}
			} else {
				resources.store.withFenceErr = store.ErrAgentTurnFenceLost
			}
			if err := handle.Cleanup(context.Background()); !errors.Is(err, store.ErrAgentTurnFenceLost) {
				t.Fatalf("Cleanup() = %v, want capture fence loss", err)
			}
			if resources.engineFactory.engine.process.removed != 0 || resources.engineFactory.engine.closed != 0 {
				t.Fatal("stale capture admission or adoption destroyed recovery evidence")
			}
			wantCaptures := 0
			if loseDuringCapture {
				wantCaptures = 1
			}
			if len(resources.workspace.captureRequests) != wantCaptures {
				t.Fatalf("capture calls = %d, want %d", len(resources.workspace.captureRequests), wantCaptures)
			}
			resources.store.withFenceErr = nil
			resources.workspace.captureHook = nil
			if err := handle.Cleanup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(resources.workspace.captureRequests) != wantCaptures+1 {
				t.Fatal("failed adoption retained an unfenced capture on the handle")
			}
		})
	}
}

func TestLauncherPreparationOnlyFailureDoesNotRequireTerminalCapture(t *testing.T) {
	operations := []string{}
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	failure := errors.New("Agent Session was never prepared")
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleReviewer, proposal, failure,
		func(resources *runtimeLaunchResources) {
			resources.workspace.captureErr = errors.New("no useful Turn exists to capture")
		})
	if handle != nil || !errors.Is(err, failure) || len(resources.workspace.captureRequests) != 0 ||
		resources.engineFactory.engine.process.removed != 1 || resources.workspace.discardedAssignmentID != runtimeTestAssignment {
		t.Fatalf("preparation-only cleanup = (%v, %v), operations %v", handle, err, operations)
	}
}

func TestLauncherPreparationFailureAfterDurableBindingRetainsCaptureSource(t *testing.T) {
	operations := []string{}
	prepareErr := errors.New("configuration failed after binding the Session")
	captureErr := errors.New("capture transport unavailable")
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, prepareErr,
		func(resources *runtimeLaunchResources) {
			resources.adapter.prepareHook = func() {
				resources.store.execution.Session.RuntimeCapabilities = json.RawMessage(`{"history_replay":true,"session_continuation":true,"state_format":"pi-session/v3"}`)
			}
			resources.workspace.captureErr = captureErr
		})
	if handle == nil || !errors.Is(err, prepareErr) || !errors.Is(err, captureErr) || resources.engineFactory.engine.process.removed != 0 {
		t.Fatalf("partially bound preparation = (%v, %v), operations %v", handle, err, operations)
	}
	if len(resources.workspace.captureRequests) != 1 {
		t.Fatal("partially bound Session was incorrectly exempted from terminal capture")
	}
	resources.workspace.captureErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resources.engineFactory.engine.process.removed != 1 {
		t.Fatal("capture retry did not release the partially prepared execution")
	}
}

func TestLauncherExistingSessionRetainsEvidenceAfterCreateOrStartFailure(t *testing.T) {
	for _, failCreate := range []bool{false, true} {
		t.Run(fmt.Sprint(failCreate), func(t *testing.T) {
			operations := []string{}
			captureErr := errors.New("stopped workspace capture unavailable")
			handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
				func(resources *runtimeLaunchResources) {
					resources.store.execution.Session.Status = store.AgentSessionActive
					resources.store.execution.Session.RuntimeCapabilities = json.RawMessage(`{"history_replay":true,"session_continuation":true,"state_format":"pi-session/v3"}`)
					resources.workspace.captureErr = captureErr
					if failCreate {
						resources.engineFactory.engine.createErr = execution.ErrUnknownEffect
					} else {
						resources.engineFactory.engine.process.startErr = execution.ErrUnknownEffect
					}
				})
			if handle == nil || !errors.Is(err, execution.ErrUnknownEffect) || !errors.Is(err, captureErr) {
				t.Fatalf("ambiguous existing-Session launch = (%v, %v)", handle, err)
			}
			process := &resources.engineFactory.engine.process
			if process.removed != 0 || resources.engineFactory.engine.closed != 0 || len(resources.workspace.captureRequests) != 1 {
				t.Fatalf("failed creation/start destroyed terminal evidence: %v", operations)
			}
			resources.workspace.captureErr = nil
			if err := handle.Cleanup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if process.stopped != 1 || process.removed != 1 || len(resources.workspace.captureRequests) != 2 {
				t.Fatalf("capture retry repeated teardown or missed evidence: %v", operations)
			}
		})
	}
}

func TestLauncherExistingSessionAmbiguousCreateWithoutHandleRequiresExactRecovery(t *testing.T) {
	operations := []string{}
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) {
			resources.store.execution.Session.Status = store.AgentSessionActive
			resources.store.execution.Session.RuntimeCapabilities = json.RawMessage(`{"history_replay":true,"session_continuation":true,"state_format":"pi-session/v3"}`)
			resources.engineFactory.engine.createErr = execution.ErrUnknownEffect
			resources.engineFactory.engine.createWithoutProcess = true
		})
	if handle == nil || !errors.Is(err, execution.ErrUnknownEffect) || resources.engineFactory.engine.absenceCalls != 0 || resources.engineFactory.engine.closed != 0 {
		t.Fatalf("ambiguous creation without handle = (%v, %v), operations %v", handle, err, operations)
	}
	if err := handle.Cleanup(context.Background()); err == nil || resources.engineFactory.engine.absenceCalls != 0 {
		t.Fatal("missing process handle authorized destruction without exact-key stop/capture")
	}
}

func TestLauncherCreatingSessionRetainsResourceUntilCleanupProofCommits(t *testing.T) {
	operations := []string{}
	proofErr := errors.New("preparation-only proof could not be committed")
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) {
			resources.engineFactory.engine.process.startErr = execution.ErrUnknownEffect
			resources.store.unpromptedCleanupErr = proofErr
		})
	if handle == nil || !errors.Is(err, proofErr) || resources.engineFactory.engine.process.removed != 0 || resources.store.unpromptedCleanupCalls != 1 {
		t.Fatalf("unprompted cleanup without durable proof = (%v, %v), operations %v", handle, err, operations)
	}
	resources.store.unpromptedCleanupErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resources.engineFactory.engine.process.removed != 1 || resources.store.unpromptedCleanupCalls != 2 || len(resources.workspace.captureRequests) != 0 {
		t.Fatalf("preparation-only cleanup did not retry proof before removal: %v", operations)
	}
}

func TestLauncherCreatingSessionAmbiguousCreateWithoutHandleCannotDeclareProof(t *testing.T) {
	operations := []string{}
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) {
			resources.engineFactory.engine.createErr = execution.ErrUnknownEffect
			resources.engineFactory.engine.createWithoutProcess = true
		})
	if handle == nil || !errors.Is(err, execution.ErrUnknownEffect) || resources.engineFactory.engine.absenceCalls != 0 || resources.store.unpromptedCleanupCalls != 0 {
		t.Fatalf("unknown first-Session creation declared absence/proof = (%v, %v), operations %v", handle, err, operations)
	}
}

func TestRuntimeHandleUnresolvedMCPDrainIsNotRetriedDuringCleanup(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	resources.gateway.drainErr = mcp.ErrMutationDrainUnresolved
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.CloseMCP(context.Background()); !errors.Is(err, mcp.ErrMutationDrainUnresolved) {
		t.Fatalf("CloseMCP() error = %v, want ErrMutationDrainUnresolved", err)
	}
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup operations = %v, want %v", operations, want)
	}
	if resources.gateway.drained != 1 {
		t.Fatalf("CloseAndDrain calls = %d, want one", resources.gateway.drained)
	}
}

func TestRuntimeHandleCleanupContinuesTeardownAndRetriesMCPDrain(t *testing.T) {
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup operations = %v, want %v", operations, want)
	}

	resources.gateway.drainErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want = []string{"mcp-close-and-drain", "agent-close", "process-stop", "mcp-close-and-drain", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup retry operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupRetainsReviewerEvidenceUntilMCPDrainCompletes(t *testing.T) {
	operations := []string{}
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	handle, resources, err := launchRuntimeForRoleCleanupFailure(t, &operations, workflow.RoleReviewer, proposal, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	resources.gateway.drainErr = context.DeadlineExceeded
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.workspace.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Cleanup() error = %v, want drain timeout", err)
	}
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("Reviewer cleanup operations = %v, want %v", operations, want)
	}
}

func TestRuntimeHandleCleanupDoesNotDiscardReviewerWorkspaceAfterFenceLoss(t *testing.T) {
	operations := []string{}
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
		BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
	}
	handle, resources, err := launchRuntimeForRoleCleanupFailure(t, &operations, workflow.RoleReviewer, proposal, nil)
	if err != nil {
		t.Fatal(err)
	}
	resources.store.withFenceErr = store.ErrAgentTurnFenceLost
	operations = operations[:0]
	resources.store.operations = &operations
	resources.workspace.operations = &operations
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("Cleanup() = %v, want lost fence", err)
	}
	if slices.Contains(operations, "workspace-discard") {
		t.Fatalf("lost fence discarded successor workspace: %v", operations)
	}
}

func TestLauncherRefusesToPromoteWorkspaceAfterTurnFenceLoss(t *testing.T) {
	operations := []string{}
	handle, _, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) {
			resources.workspace.requirePromotionFence = true
			resources.store.withFenceErr = store.ErrAgentTurnFenceLost
		})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) || handle != nil {
		t.Fatalf("Launch() = (%v, %v), want lost fence before launch", handle, err)
	}
	if slices.Contains(operations, "mise") || slices.Contains(operations, "docker-new") {
		t.Fatalf("stale workspace promotion progressed to Runtime Process: %v", operations)
	}
}

func TestLauncherRejectsWorkspaceReferenceForAnotherExecutionBeforeStartingRuntime(t *testing.T) {
	operations := []string{}
	handle, _, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) {
			resources.workspace.prepareRef = &execution.WorkspaceRef{Key: execution.Key{
				AssignmentID: runtimeTestAssignment, AgentSessionID: runtimeTestSession, AgentTurnID: runtimeTestTurn,
				ExecutionEpoch: 8, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2",
			}, Seed: runtimeTestDefaultSHA}
		})
	if handle != nil || !errors.Is(err, agentturn.ErrRuntimeBinding) {
		t.Fatalf("Launch() = (%v, %v), want wrong execution binding", handle, err)
	}
	if slices.Contains(operations, "mise") || slices.Contains(operations, "docker-new") {
		t.Fatalf("mismatched workspace progressed to execution: %v", operations)
	}
}

func TestLauncherRefusesToStartRepositorySetupAfterTurnFenceLoss(t *testing.T) {
	operations := []string{}
	handle, _, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) {
			resources.workspace.requirePromotionFence = true
			resources.store.withFenceErr = store.ErrAgentTurnFenceLost
		})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) || handle != nil {
		t.Fatalf("Launch() = (%v, %v), want lost fence before repository setup", handle, err)
	}
	if slices.Contains(operations, "refresh") || slices.Contains(operations, "docker-new") {
		t.Fatalf("stale mise provisioning progressed to Runtime Process: %v", operations)
	}
}

// A slow trusted checkout must not bypass the current-credential prelaunch gate.
func TestLauncherReacquiresCredentialAfterCheckoutBeforeDispatch(t *testing.T) {
	operations := []string{}
	clock := &expiryClock{now: time.Date(2026, time.October, 8, 7, 45, 0, 0, time.UTC)}
	cache, _ := newExpiryCache(t, clock, "launch", 5*time.Minute, time.Hour)
	provider := &phaseAdvancingCacheProvider{cache: cache, id: 78, clock: clock}
	primed, err := provider.RepositoryCredential(context.Background(), "acme", "widgets")
	if err != nil || primed != "launch-token-A" {
		t.Fatalf("prime launch token = %q, %v", primed, err)
	}
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, workflow.RoleDeveloper, nil)
	database := &runtimeStore{operations: &operations, execution: execution}
	workspaces := &runtimeWorkspace{operations: &operations}
	// The checkout performs unbounded I/O; expire the primed token there.
	workspaces.afterPrepare = func() { clock.Advance(6 * time.Minute) }
	gateway := &runtimeGateway{operations: &operations, registration: mcp.Registration{Server: agent.MCPEndpoint{
		Name: "omnigrex", URL: "http://mcp:8080/mcp",
		Headers: []agent.Header{{Name: "Authorization", Value: "Bearer mcp-secret"}},
	}}}
	engineFactory := &runtimeEngineFactory{operations: &operations}
	adapter := &runtimeTurnAdapter{operations: &operations}
	adapter.prepareHook = func() {
		if provider.calls != 2 {
			t.Fatalf("credential acquisitions at adapter preparation = %d, want prime plus post-checkout refresh", provider.calls)
		}
	}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: runtimeProfile},
		Workspace: workspaces, Gateway: gateway, Execution: engineFactory, AgentAdapters: runtimeAdapters(adapter),
		DeveloperCredentials: provider, ReviewerCredentials: provider,
	})
	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: primed, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	t.Cleanup(func() { _ = handle.Cleanup(context.Background()) })
	if workspaces.checkout.Credential != "launch-token-A" {
		t.Fatalf("workspace checkout credential = %q, want primed token A", workspaces.checkout.Credential)
	}
	fresh, err := cache.Token(context.Background(), 78)
	if err != nil || fresh != "launch-token-B" || provider.calls != 2 {
		t.Fatalf("prelaunch credential = %q, %v, acquisitions %d, want refreshed token B", fresh, err, provider.calls)
	}
	for _, credential := range []string{primed, fresh} {
		if strings.Contains(strings.Join(engineFactory.request.Environment, "\n"), credential) || strings.Contains(string(adapter.request.Credentials), credential) {
			t.Fatal("GitHub credential entered the Runtime Process or adapter")
		}
	}
}

func TestLauncherFailsSafeWhenCredentialRefreshFailsAfterCheckout(t *testing.T) {
	operations := []string{}
	authErr := errors.New("git: authentication failed: credentials expired")
	handle, _, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) {
			resources.credentials.err = authErr
		})
	if !errors.Is(err, authErr) || !strings.Contains(err.Error(), "refresh repository credential") || handle != nil {
		t.Fatalf("Launch() = (%v, %v), want credential refresh failure without a handle", handle, err)
	}
	if slices.Contains(operations, "docker-new") || slices.Contains(operations, "session-prepare") {
		t.Fatalf("failed credential refresh progressed to Runtime Process: %v", operations)
	}
	if !containsInOrder(operations, "workspace", "repository-credential", "mcp-close-and-drain") {
		t.Fatalf("launch operations = %v, want checkout, refresh, and authority cleanup", operations)
	}
}

func TestLauncherRequiresNonemptyFreshCredentialBeforeDispatch(t *testing.T) {
	operations := []string{}
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
		func(resources *runtimeLaunchResources) { resources.credentials.credential = " \t" })
	if err == nil || !strings.Contains(err.Error(), "repository credential is empty") || handle != nil || resources.credentials.calls != 1 {
		t.Fatalf("empty prelaunch credential = (%v, %v), acquisitions %d", handle, err, resources.credentials.calls)
	}
	if slices.Contains(operations, "docker-new") || slices.Contains(operations, "session-prepare") {
		t.Fatalf("empty credential authorized dispatch: %v", operations)
	}
}

func TestLauncherRedactsFreshCredentialsFromRefreshAndCleanupErrors(t *testing.T) {
	const fresh = "refreshed-github-token"
	for _, failRefresh := range []bool{false, true} {
		t.Run(fmt.Sprint(failRefresh), func(t *testing.T) {
			operations := []string{}
			handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, nil,
				func(resources *runtimeLaunchResources) {
					resources.credentials.credential = fresh
					if failRefresh {
						resources.credentials.err = errors.New("refresh exposed " + fresh)
					}
				})
			if failRefresh {
				if err == nil || handle != nil || strings.Contains(err.Error(), fresh) || !strings.Contains(err.Error(), "[REDACTED]") {
					t.Fatalf("credential acquisition error = (%v, %v)", handle, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			resources.engineFactory.engine.process.stopErr = errors.New("stop exposed " + fresh)
			if err := handle.Cleanup(t.Context()); err == nil || strings.Contains(err.Error(), fresh) || !strings.Contains(err.Error(), "[REDACTED]") {
				t.Fatalf("cleanup error = %v", err)
			}
			resources.engineFactory.engine.process.stopErr = nil
			if err := handle.Cleanup(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeHandleCleanupUsesFreshBoundedContextAfterDrainConsumesDeadline(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	resources.gateway.drainWait = true
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	if err := handle.Cleanup(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Cleanup() error = %v, want drain timeout", err)
	}
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("cleanup operations = %v, want %v", operations, want)
	}
	if resources.engineFactory.engine.process.stopContextErr != nil || resources.engineFactory.engine.process.removeContextErr != nil {
		t.Fatalf("teardown received canceled context: stop %v, remove %v", resources.engineFactory.engine.process.stopContextErr, resources.engineFactory.engine.process.removeContextErr)
	}
}

func TestRuntimeHandleCleanupRetriesAgentClosureWithoutRepeatingSuccessfulSteps(t *testing.T) {
	operations := []string{}
	handle, resources := launchRuntimeForCleanupTest(t, &operations, nil)
	closeErr := errors.New("agent close failed")
	resources.client.closeErr = closeErr
	operations = operations[:0]
	resources.gateway.operations = &operations
	resources.engineFactory.engine.operations = &operations
	resources.engineFactory.engine.process.operations = &operations
	resources.client.operations = &operations

	if err := handle.Cleanup(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("Cleanup() error = %v, want agent close failure", err)
	}
	resources.client.closeErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close", "agent-close"}
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

	if err := handle.Cleanup(context.Background()); !errors.Is(err, stopErr) {
		t.Fatalf("Cleanup() error = %v, want stop failure", err)
	}
	if resources.engineFactory.engine.process.removed != 0 || len(resources.workspace.captureRequests) != 0 {
		t.Fatal("unconfirmed stop must retain the environment without taking a terminal capture")
	}
	if slices.Contains(operations, "docker-close") {
		t.Fatalf("Docker Engine closed while process remains: %v", operations)
	}
	resources.engineFactory.engine.process.stopErr = nil
	resources.engineFactory.engine.process.removeErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close"}
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "turn-fence", "workspace-discard", "docker-close", "turn-fence", "workspace-discard"}
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close", "docker-close"}
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "docker-close"}
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "turn-fence", "workspace-discard", "docker-close"}
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
	want := []string{"mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "workspace-capture", "turn-fence", "process-remove", "process-remove", "turn-fence", "workspace-discard", "docker-close"}
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
	wantSuffix := []string{"session-prepare", "execution", "mcp-close-and-drain", "agent-close", "process-stop", "turn-fence", "unprompted-cleanup", "process-remove", "docker-close"}
	if len(operations) < len(wantSuffix) || !reflect.DeepEqual(operations[len(operations)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("failure operations = %v, want suffix %v", operations, wantSuffix)
	}
	if resources.gateway.drained != 1 || resources.client.closed != 1 || resources.engineFactory.engine.process.removed != 1 || resources.engineFactory.engine.closed != 1 {
		t.Errorf("failure cleanup did not release all resources")
	}
}

func TestLauncherFailureReturnsRecoverableHandleWhenCleanupCannotProveRemoval(t *testing.T) {
	operations := []string{}
	cleanupFailure := errors.New("remove failed with repository-secret provider-secret mcp-secret")
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(
		t, &operations, workflow.RoleDeveloper, nil, errors.New("prepare failed"),
		func(resources *runtimeLaunchResources) {
			resources.engineFactory.engine.process.removeErr = cleanupFailure
		},
	)
	if err == nil || handle == nil {
		t.Fatalf("Launch() = (%#v, %v), want recoverable handle and failure", handle, err)
	}
	if handle.AgentConnection() != nil {
		t.Fatal("failed launch returned a stale prompt capability")
	}
	for _, secret := range []string{runtimeTestCredential, runtimeTestProviderKey, "mcp-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Launch() cleanup error exposes credential %q: %v", secret, err)
		}
	}
	if resources.engineFactory.engine.process.removed != 1 || resources.engineFactory.engine.closed != 0 {
		t.Fatalf("initial cleanup = removed %d, engine closed %d", resources.engineFactory.engine.process.removed, resources.engineFactory.engine.closed)
	}

	resources.engineFactory.engine.process.removeErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("recoverable handle Cleanup() error = %v", err)
	}
	if resources.engineFactory.engine.process.removed != 2 || resources.engineFactory.engine.closed != 1 {
		t.Fatalf("retried cleanup = removed %d, engine closed %d", resources.engineFactory.engine.process.removed, resources.engineFactory.engine.closed)
	}
}

func TestLauncherPreservesOOMBeforeRemovingProcessAfterSessionResumeFails(t *testing.T) {
	operations := []string{}
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, io.EOF,
		func(resources *runtimeLaunchResources) {
			resources.engineFactory.engine.process.exitObservation = dockerruntime.ExitObservation{
				Exited: true, OOMKilled: true, ExitCode: 137, Resource: "container-oom",
			}
		})
	if handle != nil || !errors.Is(err, agentturn.ErrRuntimeOOMKilled) {
		t.Fatalf("Launch() = (%v, %v), want classified OOM with cleaned handle", handle, err)
	}
	if resources.store.oomContainerID != "container-oom" || resources.store.oomExitCode != 137 {
		t.Fatalf("saved OOM = (%q, %d)", resources.store.oomContainerID, resources.store.oomExitCode)
	}
	index := slices.Index(operations, "oom-record")
	if index < 0 || slices.Index(operations, "process-stop") <= index || slices.Index(operations, "process-remove") <= index {
		t.Fatalf("OOM evidence must be durable before process cleanup: %v", operations)
	}
}

func TestLauncherReportsOOMFirstConfirmedDuringCleanup(t *testing.T) {
	operations := []string{}
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, io.EOF,
		func(resources *runtimeLaunchResources) {
			resources.engineFactory.engine.process.observeResults = []dockerruntime.ExitObservation{
				{Running: true},
				{Exited: true, OOMKilled: true, ExitCode: 137, Resource: "late-oom"},
			}
		})
	if handle != nil || !errors.Is(err, agentturn.ErrRuntimeOOMKilled) || resources.store.oomContainerID != "late-oom" {
		t.Fatalf("Launch() = (%v, %v), saved container %q", handle, err, resources.store.oomContainerID)
	}
	if !containsInOrder(operations, "oom-record", "process-stop", "process-remove") {
		t.Fatalf("cleanup removed Runtime Process before recording OOM: %v", operations)
	}
}

func TestLauncherDoesNotRemoveOOMContainerIfEvidenceCannotBePersisted(t *testing.T) {
	operations := []string{}
	handle, resources, err := launchRuntimeForRoleCleanupFailureConfigured(t, &operations, workflow.RoleDeveloper, nil, io.EOF,
		func(resources *runtimeLaunchResources) {
			resources.engineFactory.engine.process.exitObservation = dockerruntime.ExitObservation{
				Exited: true, OOMKilled: true, ExitCode: 137, Resource: "container-oom",
			}
			resources.store.oomRecordErr = errors.New("database temporarily unavailable")
		})
	if err == nil || handle == nil || resources.engineFactory.engine.process.removed != 0 {
		t.Fatalf("Launch() = (%v, %v), removed %d processes", handle, err, resources.engineFactory.engine.process.removed)
	}
	resources.store.oomRecordErr = nil
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() retry error = %v", err)
	}
	if resources.engineFactory.engine.process.removed != 1 {
		t.Fatalf("removed processes after OOM persistence = %d", resources.engineFactory.engine.process.removed)
	}
}

type runtimeLaunchResources struct {
	store         *runtimeStore
	workspace     *runtimeWorkspace
	gateway       *runtimeGateway
	engineFactory *runtimeEngineFactory
	client        *runtimeTurnConnection
	adapter       *runtimeTurnAdapter
	credentials   *runtimeCredentialProvider
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
	return launchRuntimeForRoleCleanupFailureConfigured(t, operations, role, proposal, sessionErr, nil)
}

func launchRuntimeForRoleCleanupFailureConfigured(t *testing.T, operations *[]string, role workflow.Role, proposal *store.AgentTurnChangeProposal, sessionErr error, configure func(*runtimeLaunchResources)) (*agentturn.RuntimeHandle, *runtimeLaunchResources, error) {
	t.Helper()
	runtimeProfile := runtimeLauncherProfile(t)
	execution, lease := runtimeExecutionContext(t, runtimeProfile, role, proposal)
	database := &runtimeStore{operations: operations, execution: execution}
	workspaces := &runtimeWorkspace{operations: operations}
	gateway := &runtimeGateway{operations: operations, registration: mcp.Registration{Server: agent.MCPEndpoint{
		Name: "omnigrex", URL: "http://mcp:8080/mcp",
		Headers: []agent.Header{{Name: "Authorization", Value: "Bearer mcp-secret"}},
	}}}
	engineFactory := &runtimeEngineFactory{operations: operations}
	client := &runtimeTurnConnection{operations: operations}
	adapter := &runtimeTurnAdapter{operations: operations, connection: client, err: sessionErr}
	credentials := &runtimeCredentialProvider{operations: operations, credential: "fresh-repository-secret"}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: database, Registry: &runtimeRegistry{operations: operations, runtimeProfile: runtimeProfile},
		Workspace: workspaces,
		Gateway:   gateway, Execution: engineFactory,
		DeveloperCredentials: credentials, ReviewerCredentials: credentials,
		AgentAdapters:            runtimeAdapters(adapter),
		PathEnvironmentAllowlist: []string{"TMPDIR"},
	})
	resources := &runtimeLaunchResources{store: database, workspace: workspaces, gateway: gateway, engineFactory: engineFactory, client: client, adapter: adapter, credentials: credentials}
	if configure != nil {
		configure(resources)
	}
	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "omnigrex/issue-17",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"provider-secret"}}`),
	})
	return handle, resources, err
}

func runtimeLauncher(t *testing.T, config agentturn.LauncherConfig) *agentturn.Launcher {
	t.Helper()
	if config.MemoryBytes == 0 {
		config.MemoryBytes = 512 << 20
	}
	if config.States == nil {
		config.States = &runtimeRetainedState{}
	}
	if config.AgentAdapters == nil {
		config.AgentAdapters = runtimeAdapters(&runtimeTurnAdapter{operations: config.Store.(*runtimeStore).operations})
	}
	if config.DeveloperCredentials == nil {
		config.DeveloperCredentials = &runtimeCredentialProvider{operations: config.Store.(*runtimeStore).operations, credential: "fresh-repository-secret"}
	}
	if config.ReviewerCredentials == nil {
		config.ReviewerCredentials = &runtimeCredentialProvider{operations: config.Store.(*runtimeStore).operations, credential: "fresh-reviewer-secret"}
	}
	launcher, err := agentturn.NewLauncher(config)
	if err != nil {
		t.Fatalf("NewLauncher() error = %v", err)
	}
	return launcher
}

func runtimeLauncherProfile(t *testing.T) profile.Profile {
	t.Helper()
	value, err := profile.NewPiV2(runtimeTestImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewPiV2() error = %v", err)
	}
	return value
}

func runtimeExecutionContext(t *testing.T, runtimeProfile profile.Profile, role workflow.Role, proposal *store.AgentTurnChangeProposal) (store.AgentTurnExecutionContext, store.AgentTurnLease) {
	t.Helper()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	config := json.RawMessage(`{"instructions":"Work carefully.","model":"openai/gpt-5","name":"developer","path":".omnigrex/team/developer.md","permissions":{"bash":"allow","edit":"allow","read":"allow"},"role":"DEVELOPER","runtime":"pi-rpc/v2"}`)
	profileName := "developer"
	if role == workflow.RoleReviewer {
		profileName = "reviewer"
		config = json.RawMessage(`{"instructions":"Review carefully.","model":"openai/gpt-5","name":"reviewer","path":".omnigrex/team/reviewer.md","permissions":{"bash":"allow","edit":"deny","read":"allow"},"role":"REVIEWER","runtime":"pi-rpc/v2"}`)
	}
	stage := workflow.StageImplementation
	purpose := workflow.TurnPurposeInitialDevelopment
	if proposal != nil {
		purpose = workflow.TurnPurposeRequestedChanges
	}
	if role == workflow.RoleReviewer {
		stage = workflow.StageReview
		purpose = workflow.TurnPurposeReview
	}
	binding := store.ParticipantRuntimeBinding{
		AgentProfileName: profileName, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2",
		RuntimeProfileContentSHA256: runtimeProfile.ContentSHA256(), RuntimeImageDigest: runtimeProfile.Contract().Image,
	}
	turn := store.AgentTurn{
		AgentTurnSpec: store.AgentTurnSpec{AgentSessionID: runtimeTestSession, Stage: stage, Purpose: purpose, ControlRevision: 9, AgentProfileConfig: config},
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
		Assignment: store.AgentParticipant{ParticipantRuntimeBinding: binding, ID: runtimeTestAssignment, WorkflowID: runtimeTestWorkflow, Role: role, Generation: 1, Status: store.AgentParticipantActive, RuntimeStatePath: "assignment-" + runtimeTestAssignment + "/runtime-state"},
		Session: store.AgentSession{ID: runtimeTestSession, AgentAssignmentID: runtimeTestAssignment, RuntimeProfileName: binding.RuntimeProfileName,
			RuntimeProfileVersion: binding.RuntimeProfileVersion, RuntimeProfileContentSHA256: binding.RuntimeProfileContentSHA256,
			RuntimeImageDigest: binding.RuntimeImageDigest, RuntimeStatePath: "assignment-" + runtimeTestAssignment + "/runtime-state",
			Status: store.AgentSessionCreating, ControlOwner: store.SessionControlAutomation, ControlRevision: 9, NextExecutionEpoch: 8},
		Turn: turn,
	}
	return execution, lease
}

type runtimeStore struct {
	operations             *[]string
	execution              store.AgentTurnExecutionContext
	fenceErr               error
	fenceErrors            []error
	fenceCalls             int
	executionErr           error
	turnFence              sync.Mutex
	withFenceAttempted     chan struct{}
	withFenceAttemptOnce   sync.Once
	withFenceErr           error
	oomRecordErr           error
	oomContainerID         string
	oomExitCode            int
	inCleanupFence         bool
	unpromptedCleanupErr   error
	unpromptedCleanupCalls int
}

func (database *runtimeStore) RecordAgentTurnUnpromptedCleanup(ctx context.Context, lease store.AgentTurnLease) error {
	return database.WithAgentTurnCleanupFence(ctx, lease, func(context.Context) error {
		*database.operations = append(*database.operations, "unprompted-cleanup")
		database.unpromptedCleanupCalls++
		return database.unpromptedCleanupErr
	})
}

func (*runtimeStore) ExecutionLedger(store.AgentTurnLease) execution.Ledger { return nil }

func (database *runtimeStore) AgentTurnState(context.Context, store.AgentTurnLease) (execution.StateRef, error) {
	s := database.execution.Session
	return execution.StateRef{ID: s.AgentAssignmentID, ParticipantID: s.AgentAssignmentID, SessionID: s.ID,
		Backend: execution.BackendBinding{Family: "docker", Version: 1, Namespace: "runtime-test"}, Format: "pi-session/v3",
		RuntimeProfileSHA256: s.RuntimeProfileContentSHA256, Image: s.RuntimeImageDigest}, nil
}
func (*runtimeStore) MarkAgentTurnStateInitialized(context.Context, store.AgentTurnLease, execution.StateRef) error {
	return nil
}

func (database *runtimeStore) RecordAgentTurnRuntimeOOM(_ context.Context, _ store.AgentTurnLease, id string, code int) error {
	*database.operations = append(*database.operations, "oom-record")
	if database.oomRecordErr == nil {
		database.oomContainerID, database.oomExitCode = id, code
	}
	return database.oomRecordErr
}

func (database *runtimeStore) RefreshAgentTurnLease(_ context.Context, lease store.AgentTurnLease, extension time.Duration) (store.AgentTurnLease, error) {
	*database.operations = append(*database.operations, "refresh")
	if database.fenceCalls < len(database.fenceErrors) {
		err := database.fenceErrors[database.fenceCalls]
		database.fenceCalls++
		if err != nil {
			return store.AgentTurnLease{}, err
		}
	} else {
		database.fenceCalls++
		if database.fenceErr != nil {
			return store.AgentTurnLease{}, database.fenceErr
		}
	}
	lease.LeaseExpiresAt = lease.LeaseExpiresAt.Add(extension)
	return lease, nil
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
		database.withFenceAttemptOnce.Do(func() { close(database.withFenceAttempted) })
	}
	database.turnFence.Lock()
	defer database.turnFence.Unlock()
	if database.withFenceErr != nil {
		return database.withFenceErr
	}
	return operation(ctx)
}

func (database *runtimeStore) WithAgentTurnCleanupFence(ctx context.Context, lease store.AgentTurnLease, operation func(context.Context) error) error {
	database.inCleanupFence = true
	defer func() { database.inCleanupFence = false }()
	return database.WithAgentTurnFence(ctx, lease, operation)
}

func (database *runtimeStore) acquireRecoveryLock(attempted, acquired chan<- struct{}) {
	close(attempted)
	database.turnFence.Lock()
	close(acquired)
	database.turnFence.Unlock()
}

type runtimeCredentialProvider struct {
	operations        *[]string
	credential        string
	err               error
	calls             int
	owner, repository string
}

func (provider *runtimeCredentialProvider) RepositoryCredential(_ context.Context, owner, repository string) (string, error) {
	*provider.operations = append(*provider.operations, "repository-credential")
	provider.calls++
	provider.owner, provider.repository = owner, repository
	return provider.credential, provider.err
}

type runtimeRegistry struct {
	operations     *[]string
	runtimeProfile profile.Profile
	err            error
}

func (registry *runtimeRegistry) ResolveBinding(profile.Binding) (profile.Profile, error) {
	*registry.operations = append(*registry.operations, "profile")
	return registry.runtimeProfile, registry.err
}

type runtimeWorkspace struct {
	operations            *[]string
	checkout              workspace.Checkout
	requirePromotionFence bool
	cleanedTurnID         string
	workspaceErr          error
	discardedAssignmentID string
	discardErr            error
	captureErr            error
	captureRequests       []execution.CaptureRequest
	captureRef            execution.CaptureRef
	discardedRef          execution.WorkspaceRef
	preparationKey        execution.Key
	prepareRef            *execution.WorkspaceRef
	captureHook           func(context.Context) error
	afterPrepare          func()
}

func (lifecycle *runtimeWorkspace) Prepare(ctx context.Context, request workspace.Preparation) (execution.WorkspaceRef, error) {
	checkout := request.Checkout
	*lifecycle.operations = append(*lifecycle.operations, "workspace")
	lifecycle.checkout = checkout
	lifecycle.preparationKey = request.Key
	if lifecycle.afterPrepare != nil {
		lifecycle.afterPrepare()
	}
	if lifecycle.requirePromotionFence {
		if checkout.Fence == nil {
			return execution.WorkspaceRef{}, errors.New("workspace promotion is not fenced")
		}
		if err := checkout.Fence(ctx, func(context.Context) error { return nil }); err != nil {
			return execution.WorkspaceRef{}, err
		}
	}
	if lifecycle.prepareRef != nil {
		return *lifecycle.prepareRef, lifecycle.workspaceErr
	}
	return execution.WorkspaceRef{Key: request.Key, Seed: checkout.Revision}, lifecycle.workspaceErr
}

func (lifecycle *runtimeWorkspace) Capture(ctx context.Context, request execution.CaptureRequest) (execution.CaptureRef, error) {
	*lifecycle.operations = append(*lifecycle.operations, "workspace-capture")
	lifecycle.captureRequests = append(lifecycle.captureRequests, request)
	if lifecycle.captureHook != nil {
		if err := lifecycle.captureHook(ctx); err != nil {
			return execution.CaptureRef{}, err
		}
	}
	if lifecycle.captureErr != nil {
		return execution.CaptureRef{}, lifecycle.captureErr
	}
	if lifecycle.captureRef != (execution.CaptureRef{}) {
		return lifecycle.captureRef, nil
	}
	return execution.CaptureRef{Request: request, SHA256: strings.Repeat("a", 64)}, nil
}

func (lifecycle *runtimeWorkspace) PrepareTurnPaths(ctx context.Context, _, turnID string, configuration turnconfig.Configuration, fence workspace.WorkspaceFence) (map[string]string, error) {
	if len(configuration.Directories) != 0 && fence != nil {
		if err := fence(ctx, func(context.Context) error { return nil }); err != nil {
			return nil, err
		}
	}
	values := make(map[string]string)
	for name, directory := range configuration.Environment {
		prefix := filepath.Join(filepath.Dir(environment.DataDirectory), "omnigrex-tool-data", string(configuration.Directories[directory])) + "/"
		if configuration.Directories[directory] == turnconfig.Turn {
			prefix += turnID + "/"
		}
		values[name] = prefix + directory
	}
	return values, nil
}

func (lifecycle *runtimeWorkspace) CleanupTurnPaths(ctx context.Context, _, turnID string, fence workspace.WorkspaceFence) error {
	if fence != nil {
		return fence(ctx, func(context.Context) error { lifecycle.cleanedTurnID = turnID; return nil })
	}
	lifecycle.cleanedTurnID = turnID
	return nil
}

func (lifecycle *runtimeWorkspace) DiscardWorkspace(assignmentID string) error {
	*lifecycle.operations = append(*lifecycle.operations, "workspace-discard")
	lifecycle.discardedAssignmentID = assignmentID
	return lifecycle.discardErr
}

func (lifecycle *runtimeWorkspace) Discard(ctx context.Context, ref execution.WorkspaceRef, fence workspace.WorkspaceFence) error {
	lifecycle.discardedRef = ref
	if fence == nil {
		return lifecycle.DiscardWorkspace(ref.Key.AssignmentID)
	}
	return fence(ctx, func(context.Context) error { return lifecycle.DiscardWorkspace(ref.Key.AssignmentID) })
}

type runtimeGateway struct {
	operations   *[]string
	scope        mcp.TokenScope
	registration mcp.Registration
	err          error
	revoked      int
	drained      int
	drainErr     error
	drainWait    bool
	renewals     []time.Time
	drainStarted chan struct{}
	drainRelease chan struct{}
}

func (gateway *runtimeGateway) Register(scope mcp.TokenScope) (mcp.Registration, error) {
	*gateway.operations = append(*gateway.operations, "mcp-register")
	gateway.scope = scope
	return gateway.registration, gateway.err
}

func (gateway *runtimeGateway) Renew(_ mcp.Registration, expiresAt time.Time) bool {
	*gateway.operations = append(*gateway.operations, "mcp-renew")
	gateway.renewals = append(gateway.renewals, expiresAt)
	return true
}

func (gateway *runtimeGateway) Revoke(mcp.Registration) bool {
	*gateway.operations = append(*gateway.operations, "mcp-revoke")
	gateway.revoked++
	return true
}

func (gateway *runtimeGateway) CloseAndDrain(ctx context.Context, _ mcp.Registration) error {
	*gateway.operations = append(*gateway.operations, "mcp-close-and-drain")
	gateway.drained++
	if gateway.drainStarted != nil {
		close(gateway.drainStarted)
	}
	if gateway.drainRelease != nil {
		select {
		case <-gateway.drainRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if gateway.drainWait {
		<-ctx.Done()
		return ctx.Err()
	}
	return gateway.drainErr
}

var testDockerFactory = dockerruntime.ExecutionFactory{
	Network: "omnigrex-agent", WorkspaceVolume: "workspaces", RuntimeStateVolume: "runtime-state", MiseVolume: "mise",
}

type runtimeEngineFactory struct {
	request    execution.Request
	operations *[]string
	options    dockerruntime.EngineOptions
	engine     runtimeEngine
	err        error
}

func (factory *runtimeEngineFactory) CheckRequirements(requirements []execution.Requirement) error {
	return testDockerFactory.CheckRequirements(requirements)
}

func (factory *runtimeEngineFactory) New(request execution.Request, _ execution.Ledger) (agentturn.RuntimeEngine, error) {
	factory.request = request
	*factory.operations = append(*factory.operations, "docker-new")
	policy, _, err := testDockerFactory.Compile(request)
	if err != nil {
		return nil, err
	}
	factory.options = dockerruntime.EngineOptions{AgentNetwork: testDockerFactory.Network, RuntimePolicy: policy}
	factory.engine.operations = factory.operations
	if factory.engine.process.transport == nil {
		factory.engine.process.transport = &runtimeTransport{}
	}
	return &factory.engine, factory.err
}

func (engine *runtimeEngine) EnsureAbsent(context.Context) error {
	engine.absenceCalls++
	return nil
}

type runtimeEngine struct {
	operations           *[]string
	spec                 dockerruntime.Spec
	process              runtimeProcess
	createErr            error
	closed               int
	closeErr             error
	createWithoutProcess bool
	absenceCalls         int
}

func (engine *runtimeEngine) Create(_ context.Context, request execution.Request, _ io.Writer) (agentturn.RuntimeProcess, error) {
	*engine.operations = append(*engine.operations, "docker-create")
	_, spec, err := testDockerFactory.Compile(request)
	if err != nil {
		return nil, err
	}
	engine.spec = spec
	engine.process.operations = engine.operations
	if engine.createWithoutProcess {
		return nil, engine.createErr
	}
	return &engine.process, engine.createErr
}

func (engine *runtimeEngine) Close() error {
	*engine.operations = append(*engine.operations, "docker-close")
	engine.closed++
	return engine.closeErr
}

type runtimeProcess struct {
	operations       *[]string
	transport        io.ReadWriteCloser
	stopped          int
	removed          int
	stopErr          error
	removeErr        error
	startErr         error
	started          int
	startEntered     chan<- struct{}
	releaseStart     <-chan struct{}
	stopContextErr   error
	removeContextErr error
	exitObservation  dockerruntime.ExitObservation
	observeResults   []dockerruntime.ExitObservation
	observeErr       error
}

func (process *runtimeProcess) ObserveExit(context.Context) (dockerruntime.ExitObservation, error) {
	if len(process.observeResults) != 0 {
		result := process.observeResults[0]
		process.observeResults = process.observeResults[1:]
		return result, process.observeErr
	}
	return process.exitObservation, process.observeErr
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
func (process *runtimeProcess) Stop(ctx context.Context, _ time.Duration) error {
	*process.operations = append(*process.operations, "process-stop")
	process.stopped++
	process.stopContextErr = ctx.Err()
	return process.stopErr
}
func (process *runtimeProcess) Remove(ctx context.Context) error {
	*process.operations = append(*process.operations, "process-remove")
	process.removed++
	process.removeContextErr = ctx.Err()
	return process.removeErr
}

type runtimeTransport struct{}

func (*runtimeTransport) Read([]byte) (int, error)       { return 0, io.EOF }
func (*runtimeTransport) Write(data []byte) (int, error) { return len(data), nil }
func (*runtimeTransport) Close() error                   { return nil }

type runtimeTurnConnection struct {
	operations *[]string
	closed     int
	prompts    int
	closeErr   error
}

func (client *runtimeTurnConnection) Prompt(context.Context, store.AgentTurnLease, store.AgentSession, string, agentevent.Sink) (agent.Outcome, error) {
	client.prompts++
	return agent.Outcome{Status: agent.Completed}, nil
}
func (client *runtimeTurnConnection) Close() error {
	if client.operations != nil {
		*client.operations = append(*client.operations, "agent-close")
	}
	client.closed++
	return client.closeErr
}

type runtimeTurnAdapter struct {
	operations  *[]string
	request     session.TurnPreparation
	connection  *runtimeTurnConnection
	err         error
	prepareHook func()
}

func (*runtimeTurnAdapter) BuildProcess(p profile.Profile, key execution.Key, workflowID string, memory int64, tools map[string]string) (execution.Request, error) {
	return (pi.TurnAdapter{}).BuildProcess(p, key, workflowID, memory, tools)
}

func (preparer *runtimeTurnAdapter) Prepare(_ context.Context, request session.TurnPreparation) (session.TurnConnection, error) {
	*preparer.operations = append(*preparer.operations, "session-prepare")
	preparer.request = request
	if preparer.prepareHook != nil {
		preparer.prepareHook()
	}
	if preparer.connection == nil {
		preparer.connection = &runtimeTurnConnection{operations: preparer.operations}
	}
	return preparer.connection, preparer.err
}

func runtimeAdapters(adapter session.TurnAdapter) map[string]session.TurnAdapter {
	return map[string]session.TurnAdapter{"pi-rpc/v2": adapter}
}

type runtimeRetainedState struct {
	ensured []execution.StateRef
}

func (states *runtimeRetainedState) Ensure(_ context.Context, ref execution.StateRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	states.ensured = append(states.ensured, ref)
	return nil
}
func (*runtimeRetainedState) Inspect(context.Context, execution.StateRef) error { return nil }
func (*runtimeRetainedState) Collect(context.Context, execution.StateRef) error { return nil }

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
