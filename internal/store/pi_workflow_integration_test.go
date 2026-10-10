//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/config"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/turnconfig"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestPiControlledWorkflow(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_WORKFLOW_INTEGRATION") != "1" {
		t.Skip("explicit disposable Workflow qualification gate required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	root := os.Getenv("OMNIGREX_PI_WORKFLOW_ROOT")
	if root == "" {
		t.Fatal("qualification requires mounted isolated storage roots")
	}
	url := os.Getenv("OMNIGREX_PI_WORKFLOW_DATABASE_URL")
	secret := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(secret, []byte("qualification-only-password"), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := builtinStoreConfig(t)
	allowedPaths := []string{"TMPDIR", "GOTMPDIR", "GOCACHE", "GOPATH"}
	database, err := store.Open(ctx, url, secret, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	ip := qualificationAddress(t)
	endpoint := "http://" + net.JoinHostPort(ip, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)) + "/mcp"
	settings := config.Config{ExecutionBackend: os.Getenv("OMNIGREX_PI_BACKEND"), DockerAgentNetwork: os.Getenv("OMNIGREX_PI_WORKFLOW_NETWORK"), WorkspaceVolume: os.Getenv("OMNIGREX_PI_WORKFLOW_WORKSPACES"), RuntimeStateVolume: os.Getenv("OMNIGREX_PI_WORKFLOW_STATES"), MiseVolume: os.Getenv("OMNIGREX_PI_WORKFLOW_TOOLS"), WorkspaceRoot: filepath.Join(root, "workspaces"), RuntimeStateRoot: filepath.Join(root, "state"), MiseRoot: filepath.Join(root, "tools"), MicrosandboxExecutable: "/usr/local/bin/msb", MicrosandboxFirmware: "/usr/local/lib/libkrunfw.so.5.6.1", MicrosandboxHome: "/tmp/msb-workflow", MicrosandboxNamespace: uuid.NewString(), MicrosandboxCPUs: 1, MicrosandboxRootDiskMiB: 4096, MCPEndpointURL: endpoint, AgentTurnExecutionTurnTimeout: 3 * time.Minute}
	image, platform := workflowQualificationImage(t)
	settings.ShutdownTimeout = 10 * time.Second
	settings.AgentImageReference = image
	p, err := profile.NewPiV1(image, platform)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := profile.NewRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := composition.NewBackend(ctx, settings, p, database, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	localImage := os.Getenv("OMNIGREX_PI_WORKFLOW_LOCAL_IMAGE")
	if localImage != "" {
		states, err := docker.NewRetainedState(settings.RuntimeStateRoot, settings.RuntimeStateVolume, workflowFixtureStateSource{source: database, image: image, local: localImage})
		if err != nil {
			t.Fatal(err)
		}
		backend.Factory = docker.ExecutionFactory{Network: settings.DockerAgentNetwork, WorkspaceVolume: settings.WorkspaceVolume, RuntimeStateVolume: settings.RuntimeStateVolume, MiseVolume: settings.MiseVolume, States: states}
	}
	if err := database.BindExecutionBackend(ctx, backend.States.Binding()); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	seed := filepath.Join(t.TempDir(), "seed")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "/usr/bin/git", args...)
		cmd.Dir = dir
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Git: %v %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("", "init", "--bare", bare)
	run("", "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("Controlled Work Item\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "add", "README.md")
	run(seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Seed repository")
	head := run(seed, "rev-parse", "HEAD")
	run(seed, "push", bare, "HEAD:refs/heads/main")
	git := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexec /usr/bin/git -c 'url.file://"+bare+".insteadOf=https://github.com/owner/repo.git' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	workspaces, err := workspace.New(workspace.Options{WorkspaceRoot: settings.WorkspaceRoot, PublicationRoot: filepath.Join(root, "publication"), MiseRoot: settings.MiseRoot, CaptureRoot: filepath.Join(root, "state", "captures"), CaptureSource: backend.Workspaces, CaptureLedger: database, GitExecutable: git})
	if err != nil {
		t.Fatal(err)
	}
	github := &workflowQualificationGitHub{replayGatewayGitHub: &replayGatewayGitHub{branch: "omnigrex/issue-123", base: "main"}, bare: bare, base: head}
	toolBackend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: github, Credentials: replayGatewayCredentials{}, Publisher: workspaces, Workflow: mcp.LedgerWorkflowMutations{}, PriorIntents: database, Handoffs: database, Policies: configuration.Policies})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := mcp.New(mcp.Config{EndpointURL: endpoint, Store: database, Backend: toolBackend, Policies: configuration.Policies, LifecycleContext: ctx, MutationFinalizationTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	authority := &workflowAuthorityAudit{Gateway: gateway}
	launcher, err := agentturn.NewLauncher(agentturn.LauncherConfig{Store: database, Registry: profile.Resolver{Catalog: registry.Catalog, Source: database}, Workspace: workspaces, Gateway: authority, Execution: workflowFixtureFactory{Factory: backend.Factory, localImage: localImage}, States: backend.States, ACP: agentturn.ProductionACPFactory{}, Sessions: session.NewCoordinator(database), AgentAdapters: map[string]session.TurnAdapter{"pi-rpc/v1": pi.TurnAdapter{Store: database}}, MemoryBytes: 512 << 20, Policies: configuration.Policies, Definition: configuration.Reducer.Definition(), PathEnvironmentAllowlist: allowedPaths})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: github, Workspace: workspaces})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := agentturn.NewExecutionWorker(agentturn.ExecutionWorkerDependencies{Store: database, DeveloperCredentials: &integrationCredentialProvider{credential: "developer-token"}, ReviewerCredentials: &integrationCredentialProvider{credential: "reviewer-token"}, DefaultBranch: workflowQualificationBranch{head: head}, Launcher: launcher, Sessions: session.NewCoordinator(database), Outcomes: outcomes, Workspace: workspaces, PublicationRecovery: agentturn.NewPublicationRecovery(database, workspaces, github), Policies: configuration.Policies, Definition: configuration.Reducer.Definition()}, agentturn.ExecutionWorkerConfig{ClaimOwner: "qualified-workflow", LeaseDuration: 30 * time.Second, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, TurnTimeout: 3 * time.Minute, CleanupTimeout: 30 * time.Second, ConcurrencyLimit: 1, ProviderCredentialJSON: json.RawMessage(`{"controlled":{"type":"api_key","key":"workflow-only-provider-key"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	delivery := workflowDelivery(uuid.NewString())
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "trigger"), workflowLocator(), func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
		return workflow.TriggerEvent{EventMetadata: eventContext.Metadata, AttemptID: uuid.NewString(), AttemptNumber: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.branch = fmt.Sprintf("omnigrex/issue-%d", workflowLocator().IssueNumber)
	loader := agentprofile.NewLoader(workflowQualificationProfiles{head: head}, configuration.Policies, allowedPaths...)
	preparer := agentturn.NewPreparer(loader, agentprofile.SingletonSelector{}, registry, database)
	for _, wanted := range []workflow.State{workflow.StateReviewing, workflow.StatePRReady} {
		prepared, err := preparer.Prepare(ctx, agentturn.Request{Lease: claimPreparationJob(t, database, ctx), InstallationCredential: "developer-token", RepositoryOwner: "owner", RepositoryName: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		processed, err := worker.ProcessNext(ctx)
		if err != nil || !processed {
			t.Fatalf("real Pi execution: %t %v", processed, err)
		}
		toolRoot := filepath.Join(settings.MiseRoot, "assignment-"+prepared.Commit.Participant.ID, "tool-data")
		if _, err := os.Lstat(filepath.Join(toolRoot, "turn", prepared.Commit.Turn.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Turn scratch was not cleaned: %v", err)
		}
		for _, name := range []string{"go-build-cache", "go-modules"} {
			info, err := os.Stat(filepath.Join(toolRoot, "assignment", name))
			if err != nil || !info.IsDir() {
				t.Fatalf("assignment cache lost: %s %v", name, err)
			}
		}
		// Observe the human-facing Workflow state through its durable query surface.
		pool := openPool(t, url+"&password=qualification-only-password")
		var state workflow.State
		if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id=$1`, application.WorkflowID).Scan(&state); err != nil || state != wanted {
			t.Fatalf("Workflow: %s %v; want %s", state, err, wanted)
		}
	}
	participants, err := database.ListAgentParticipants(ctx, application.WorkflowID)
	if err != nil || len(participants) != 2 {
		t.Fatalf("Participants: %+v %v", participants, err)
	}
	for _, participant := range participants {
		durable, err := database.GetAgentSessionForParticipant(ctx, participant.ID)
		if err != nil || durable.RuntimeSessionID != durable.ID || durable.ACPSessionID != "" {
			t.Fatalf("durable Pi binding: %+v %v", durable, err)
		}
		bytes, err := os.ReadFile(filepath.Join(settings.RuntimeStateRoot, "assignment-"+participant.ID, "runtime-state", "sessions", durable.ID, "session.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"workflow-only-provider-key", "developer-token", "reviewer-token"} {
			if strings.Contains(string(bytes), secret) {
				t.Fatal("credentials retained in Session")
			}
		}
		for _, token := range authority.tokens {
			if strings.Contains(string(bytes), token) {
				t.Fatal("MCP authority entered retained Session")
			}
		}
		if participant.Role == workflow.RoleReviewer {
			paths, _ := workspaces.Paths(participant.ID)
			if _, err := os.Lstat(paths.Workspace); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Reviewer workspace was not discarded")
			}
		}
	}
	if github.openCalls != 1 || github.reviewCalls != 1 || github.review.ID == 0 || github.review.State != "APPROVED" {
		t.Fatalf("publication/review counts: opened=%d submitted=%d %+v", github.openCalls, github.reviewCalls, github.review)
	}
	t.Log("PASS: production Pi launch, fenced MCP publication, native Reviewer review and durable human-review transition")
}

type workflowAuthorityAudit struct {
	*mcp.Gateway
	mu     sync.Mutex
	tokens []string
}

func (g *workflowAuthorityAudit) Register(scope mcp.TokenScope) (mcp.Registration, error) {
	registration, err := g.Gateway.Register(scope)
	if err == nil {
		g.mu.Lock()
		for _, header := range registration.Server.Headers {
			if token, ok := strings.CutPrefix(header.Value, "Bearer "); ok {
				g.tokens = append(g.tokens, token)
			}
		}
		g.mu.Unlock()
	}
	return registration, err
}

func qualificationAddress(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ip, _, _ := net.ParseCIDR(address.String())
		if ip.To4() != nil && !ip.IsLoopback() {
			return ip.String()
		}
	}
	t.Fatal("no routable qualification address")
	return ""
}

type workflowFixtureFactory struct {
	execution.Factory
	localImage string
}

func (f workflowFixtureFactory) New(request execution.Request, ledger execution.Ledger) (execution.Engine, error) {
	// Only the disposable image's model catalog changes; production Bootstrap
	// and all launch/state/MCP/capture/settlement boundaries stay in use.
	request.Command = append([]string(nil), request.Command...)
	request.Command[len(request.Command)-1] = "/opt/omnigrex/pi/qualification/turn.mjs"
	if f.localImage != "" {
		request.Image = f.localImage
		request.State.Image = f.localImage
	}
	engine, err := f.Factory.New(request, ledger)
	if err != nil {
		return nil, err
	}
	return workflowFixtureEngine{Engine: engine, request: request}, nil
}

type workflowFixtureStateSource struct {
	source       execution.StateMetadataSource
	image, local string
}

func (s workflowFixtureStateSource) StateMetadata(ctx context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	if ref.Image != s.local {
		return nil, false, execution.ErrBindingMismatch
	}
	ref.Image = s.image
	return s.source.StateMetadata(ctx, ref)
}

type workflowFixtureEngine struct {
	execution.Engine
	request execution.Request
}

func (e workflowFixtureEngine) Create(ctx context.Context, _ execution.Request, stderr io.Writer) (execution.Process, error) {
	return e.Engine.Create(ctx, e.request, stderr)
}

type workflowQualificationProfiles struct{ head string }

func (p workflowQualificationProfiles) ResolveDefaultBranchCommit(context.Context, string, string, string) (string, error) {
	return p.head, nil
}
func (p workflowQualificationProfiles) ListRepositoryDirectoryFiles(_ context.Context, _, _, _, directory, _ string) ([]string, error) {
	if directory == ".omnigrex/team" {
		return []string{".omnigrex/team/developer.md", ".omnigrex/team/reviewer.md"}, nil
	}
	if directory == ".omnigrex" {
		return []string{turnconfig.Path}, nil
	}
	return nil, nil
}
func (p workflowQualificationProfiles) FetchRepositoryFile(_ context.Context, _, _, _, path, _ string) ([]byte, error) {
	if path == turnconfig.Path {
		return []byte(workflowQualificationTurnConfiguration), nil
	}
	role := "DEVELOPER"
	if strings.Contains(path, "reviewer") {
		role = "REVIEWER"
	}
	return []byte(fmt.Sprintf("---\nname: %s\nrole: %s\nruntime: pi-rpc/v1\nmodel: controlled/workflow\npermissions:\n  read: allow\n  bash: allow\n---\nPerform the controlled Work Item.\n", strings.ToLower(role), role)), nil
}

// Match this repository's approved Turn scratch and assignment cache requests.
const workflowQualificationTurnConfiguration = `version: 1
environment-paths:
  directories:
    build-tmp:
      lifecycle: turn
    go-build-cache:
      lifecycle: assignment
    go-modules:
      lifecycle: assignment
  environment:
    - name: TMPDIR
      directory: build-tmp
    - name: GOTMPDIR
      directory: build-tmp
    - name: GOCACHE
      directory: go-build-cache
    - name: GOPATH
      directory: go-modules
`

type workflowQualificationBranch struct{ head string }

func (b workflowQualificationBranch) ResolveDefaultBranch(context.Context, string, string, string) (githubapi.DefaultBranch, error) {
	return githubapi.DefaultBranch{Name: "main", CommitSHA: b.head}, nil
}

type workflowQualificationGitHub struct {
	*replayGatewayGitHub
	bare, base  string
	review      githubapi.Review
	reviewCalls int
}

func (g *workflowQualificationGitHub) pull() githubapi.PullRequest {
	cmd := exec.Command("/usr/bin/git", "--git-dir", g.bare, "rev-parse", "refs/heads/"+g.branch)
	bytes, _ := cmd.Output()
	g.head = strings.TrimSpace(string(bytes))
	pr := g.pullRequest()
	pr.Base.SHA = g.base
	return pr
}
func (g *workflowQualificationGitHub) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.pull(), nil
}
func (g *workflowQualificationGitHub) ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.openCalls == 0 {
		return nil, nil
	}
	return []githubapi.PullRequest{g.pull()}, nil
}
func (g *workflowQualificationGitHub) OpenPullRequest(_ context.Context, credential, _, _ string, request githubapi.OpenPullRequestRequest) (githubapi.PullRequest, error) {
	if credential != "developer-token" {
		return githubapi.PullRequest{}, errors.New("wrong publication authority")
	}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.openCalls++
	g.title = request.Title
	g.body = githubapi.JoinBodyParts(request.Body, fmt.Sprintf("Closes #%d", request.IssueNumber), request.Marker)
	return g.pull(), nil
}
func (g *workflowQualificationGitHub) SubmitReview(_ context.Context, credential, _, _ string, _ int, request githubapi.ReviewRequest) (githubapi.Review, error) {
	if credential != "reviewer-token" {
		return githubapi.Review{}, errors.New("wrong review authority")
	}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.reviewCalls++
	g.review = githubapi.Review{ID: 801, NodeID: "PRR_controlled", State: "APPROVED", CommitID: request.CommitID, Body: request.Body, User: githubapi.User{ID: 9702}, HTMLURL: "https://github.com/owner/repo/pull/23#review-801"}
	return g.review, nil
}
func (g *workflowQualificationGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.review.ID == 0 {
		return nil, nil
	}
	return []githubapi.Review{g.review}, nil
}

func (*workflowQualificationGitHub) ListPullRequestFiles(context.Context, string, string, string, int) ([]githubapi.PullRequestFile, error) {
	return nil, nil
}
