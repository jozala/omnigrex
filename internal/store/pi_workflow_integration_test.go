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
	"net/url"
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
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/stepbudget"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/turnconfig"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestPiControlledWorkflow(t *testing.T) {
	t.Run("default-branch-refresh", func(t *testing.T) {
		f := newPiWorkflowFixture(t, "long-blocked")
		source := f.prepare(t)
		if processed, err := f.worker.ProcessNext(f.ctx); err != nil || !processed {
			t.Fatalf("retained source Turn: %t %v", processed, err)
		}
		f.assertState(t, workflow.StateNeedsHuman)
		retained, err := f.database.GetAgentSessionForParticipant(f.ctx, source.Commit.Participant.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.fault.mode = "default-branch-refresh"
		f.github.advanceDefault = true
		delivery := workflowDelivery(uuid.NewString())
		delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
		claim := claimWorkflowDelivery(t, f.database, f.ctx, delivery)
		if _, err := f.database.CompleteWebhookTransition(f.ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "trigger"), workflowLocator(), func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: eventContext.Metadata, AttemptID: uuid.NewString(), AttemptNumber: 2}, nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, wanted := range []workflow.State{workflow.StateReviewing, workflow.StatePRReady} {
			prepared := f.prepare(t)
			if wanted == workflow.StateReviewing {
				if prepared.Commit.Participant.ID != source.Commit.Participant.ID {
					t.Fatal("refresh replaced retained Participant")
				}
				current, err := f.database.GetAgentSessionForParticipant(f.ctx, prepared.Commit.Participant.ID)
				if err != nil || current.ID != retained.ID || current.RuntimeSessionID != retained.RuntimeSessionID {
					t.Fatal("refresh replaced retained Session", err)
				}
			}
			processed, err := f.worker.ProcessNext(f.ctx)
			if err != nil || !processed {
				t.Fatalf("refresh Turn: %t %v", processed, err)
			}
			f.assertState(t, wanted)
			var count int
			if err := f.pool(t).QueryRow(f.ctx, `SELECT count(*) FROM tool_invocations WHERE agent_turn_id=$1 AND tool_name='refresh_default_branch' AND state='SUCCEEDED' AND result->>'status'='current'`, prepared.Commit.Turn.ID).Scan(&count); err != nil || count < 1 {
				t.Fatalf("refresh not reached through Pi MCP discovery: %d %v", count, err)
			}
		}
		participants, err := f.database.ListAgentParticipants(f.ctx, f.workflowID)
		if err != nil {
			t.Fatal(err)
		}
		for _, participant := range participants {
			session, err := f.database.GetAgentSessionForParticipant(f.ctx, participant.ID)
			if err != nil || session.RuntimeSessionID != session.ID {
				t.Fatal("Session binding changed", err)
			}
			if participant.RuntimeImageDigest != f.runtime.Binding().Image {
				t.Fatal("snapshot capability rebound Runtime Profile")
			}
			history, err := os.ReadFile(filepath.Join(f.settings.RuntimeStateRoot, "assignment-"+participant.ID, "runtime-state", "sessions", session.ID, "session.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range append([]string{"developer-token", "reviewer-token", "workflow-only-provider-key"}, f.authority.tokens...) {
				if strings.Contains(string(history), secret) {
					t.Fatal("snapshot credentials retained in Session")
				}
			}
		}
		if f.github.base == f.head {
			t.Fatal("default branch did not advance after preparation")
		}
		t.Log("PASS: Developer acquisition before PR and after publication, Developer merge and Reviewer read-only verification of acquired ancestry, ordinary handoff/verdict")
	})
	t.Run("long blocked completion", func(t *testing.T) {
		f := newPiWorkflowFixture(t, "long-blocked")
		prepared := f.prepare(t)
		processed, err := f.worker.ProcessNext(f.ctx)
		if err != nil || !processed {
			t.Fatalf("long blocked Pi execution: %t %v", processed, err)
		}
		f.assertState(t, workflow.StateNeedsHuman)
		var outcome string
		if err := f.pool(t).QueryRow(f.ctx, `SELECT workflow_outcome FROM agent_turn_settlements WHERE agent_turn_id=$1`, prepared.Commit.Turn.ID).Scan(&outcome); err != nil || outcome != string(workflow.TurnOutcomeBlocked) {
			t.Fatalf("successful blocked intent not reconciled: %s %v", outcome, err)
		}
	})
	f := newPiWorkflowFixture(t, "")
	ctx, database, settings := f.ctx, f.database, f.settings
	worker, workspaces, github, authority := f.worker, f.workspaces, f.github, f.authority
	gateCIReads := 0
	for _, wanted := range []workflow.State{workflow.StateReviewing, workflow.StatePRReady} {
		if wanted == workflow.StatePRReady {
			github.mutex.Lock()
			github.ciPending = true
			github.mutex.Unlock()
			lease := claimPreparationJob(t, database, ctx)
			_, err := f.preparer.Prepare(ctx, agentturn.Request{Lease: lease, InstallationCredential: "developer-token", RepositoryOwner: "owner", RepositoryName: "repo"})
			if !errors.Is(err, store.ErrReviewAdmissionPending) {
				t.Fatalf("pending CI admitted Reviewer: %v", err)
			}
			var reviewers, turns, executions int
			if err := f.pool(t).QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_participants WHERE workflow_id=$1 AND role='REVIEWER'),(SELECT count(*) FROM agent_turns WHERE workflow_id=$1 AND active),(SELECT count(*) FROM jobs WHERE workflow_id=$1 AND kind='RUN_AGENT_TURN')`, f.workflowID).Scan(&reviewers, &turns, &executions); err != nil {
				t.Fatal(err)
			}
			if reviewers != 0 || turns != 0 || executions != 1 || len(f.fault.executions) != 1 {
				t.Fatalf("CI wait allocated runtime work: reviewers=%d turns=%d executions=%d runtime processes=%d", reviewers, turns, executions, len(f.fault.executions))
			}
			if err := database.DeferReviewAdmission(ctx, lease, time.Second); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool(t).Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, lease.ID); err != nil {
				t.Fatal(err)
			}
			github.mutex.Lock()
			github.ciPending = false
			github.mutex.Unlock()
		}
		prepared := f.prepare(t)
		if wanted == workflow.StatePRReady {
			gateCIReads = github.ciReads
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
		f.assertState(t, wanted)
	}
	participants, err := database.ListAgentParticipants(ctx, f.workflowID)
	if err != nil || len(participants) != 2 {
		t.Fatalf("Participants: %+v %v", participants, err)
	}
	for _, participant := range participants {
		durable, err := database.GetAgentSessionForParticipant(ctx, participant.ID)
		if err != nil || durable.RuntimeSessionID != durable.ID {
			t.Fatalf("durable Pi binding: %+v %v", durable, err)
		}
		bytes, err := os.ReadFile(filepath.Join(settings.RuntimeStateRoot, "assignment-"+participant.ID, "runtime-state", "sessions", durable.ID, "session.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		longMessages := 0
		for _, line := range strings.Split(strings.TrimSpace(string(bytes)), "\n") {
			if len(line) > 1<<20 {
				t.Fatal("fixture exceeded durable Session entry bound")
			}
			if strings.Contains(line, strings.Repeat("LONG_WORKFLOW_BODY", 32000)) {
				longMessages++
			}
		}
		if longMessages != 6 || len(bytes) < 2<<20 {
			t.Fatalf("complete long transcript was not retained: messages=%d bytes=%d", longMessages, len(bytes))
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
			if !strings.Contains(string(bytes), "review_start") || !strings.Contains(string(bytes), "completed_ci") || !strings.Contains(string(bytes), "terminal_non_success") {
				t.Fatal("Reviewer prompt did not receive the actual non-success admission context")
			}

			paths, _ := workspaces.Paths(participant.ID)
			if _, err := os.Lstat(paths.Workspace); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Reviewer workspace was not discarded")
			}
		}
	}
	if github.ciReads-gateCIReads != 1 {
		t.Fatalf("existing Reviewer CI read was not exercised: %d", github.ciReads)
	}
	if github.openCalls != 1 || github.reviewCalls != 1 || github.review.ID == 0 || github.review.State != "APPROVED" {
		t.Fatalf("publication/review counts: opened=%d submitted=%d %+v", github.openCalls, github.reviewCalls, github.review)
	}
	t.Log("PASS: production Pi launch, fenced MCP publication, native Reviewer review and durable human-review transition")
	// These subtests are deliberately part of the existing both-backend CI
	// selection, not a new test name omitted by qualification regex filters.
	for _, value := range []string{"1", "3", "5"} {
		t.Run("step-budget-"+value, func(t *testing.T) {
			bounded := newPiWorkflowFixture(t, "")
			limit, _ := stepbudget.Parse(value)
			bounded.preparer.SetDefaultMaxSteps(limit)
			prepared := bounded.prepare(t)
			processed, err := bounded.worker.ProcessNext(bounded.ctx)
			if err != nil || !processed {
				t.Fatalf("bounded execution: %t %v", processed, err)
			}
			wanted := workflow.StateNeedsHuman
			if value == "5" {
				wanted = workflow.StateReviewing
			}
			bounded.assertState(t, wanted)
			var outcome []byte
			if err := bounded.pool(t).QueryRow(bounded.ctx, "SELECT outcome FROM agent_turns WHERE id=$1", prepared.Commit.Turn.ID).Scan(&outcome); err != nil {
				t.Fatal(err)
			}
			var evidence struct {
				Budget    stepbudget.Observation `json:"step_budget"`
				Exhausted bool                   `json:"step_budget_exhausted"`
			}
			if err := json.Unmarshal(outcome, &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Budget.Used != uint64(limit.Steps()) || !evidence.Budget.Summary || !evidence.Budget.Stopped || !evidence.Budget.Complete || evidence.Exhausted != (value != "5") {
				t.Fatalf("budget settlement %s", outcome)
			}
			if value != "5" {
				if next, err := bounded.worker.ProcessNext(bounded.ctx); err != nil || next {
					t.Fatalf("exhaustion retried: %t %v", next, err)
				}
			}
		})
	}

}

// Both backends run the real environment bootstrap from the working checkout.
// mise's postinstall hook spends longer than the ordinary 30s RPC deadline
// before Node is started, then the production Pi adapter admits a full Turn.
func TestPiSlowRepositoryBootstrap(t *testing.T) {
	f := newPiWorkflowFixture(t, "slow-bootstrap")
	prepared := f.prepare(t)
	started := time.Now()
	processed, err := f.worker.ProcessNext(f.ctx)
	if err != nil || !processed {
		t.Fatalf("delayed repository bootstrap: %t %v", processed, err)
	}
	if time.Since(started) < 30*time.Second {
		t.Fatal("repository installation did not exceed ordinary RPC budget")
	}
	if _, err := os.Stat(filepath.Join(f.settings.MiseRoot, "assignment-"+prepared.Commit.Participant.ID, "tool-data", "assignment", "slow-bootstrap-complete")); err != nil {
		t.Fatalf("repository install hook did not finish: %v", err)
	}
	f.assertState(t, workflow.StateReviewing)
	if f.github.openCalls != 1 {
		t.Fatalf("controlled prompt did not publish a Change Proposal: %d", f.github.openCalls)
	}
}

func newPiWorkflowFixture(t *testing.T, mode string) *piWorkflowFixture {
	t.Helper()
	if os.Getenv("OMNIGREX_PI_WORKFLOW_INTEGRATION") != "1" {
		t.Skip("explicit disposable Workflow qualification gate required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	t.Cleanup(cancel)
	root := os.Getenv("OMNIGREX_PI_WORKFLOW_ROOT")
	if root == "" {
		t.Fatal("qualification requires mounted isolated storage roots")
	}
	url := isolatedPiWorkflowDatabase(t, ctx)
	secret := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(secret, []byte("qualification-only-password"), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := builtinStoreConfig(t)
	allowedPaths := []string{"TMPDIR", "GOTMPDIR", "GOCACHE", "GOPATH"}
	database, err := openTimedStore(t, ctx, url, secret, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	fault := &piWorkflowFault{mode: mode}
	fault.continueReads = make(chan struct{})
	fault.reads = make(chan piReadObservation, 4)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	ip := qualificationAddress(t)
	endpoint := "http://" + net.JoinHostPort(ip, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)) + "/mcp"
	fault.controlURL = strings.TrimSuffix(endpoint, "/mcp") + "/qualification"
	settings := config.Config{ExecutionBackend: os.Getenv("OMNIGREX_PI_BACKEND"), DockerAgentNetwork: os.Getenv("OMNIGREX_PI_WORKFLOW_NETWORK"), WorkspaceVolume: os.Getenv("OMNIGREX_PI_WORKFLOW_WORKSPACES"), RuntimeStateVolume: os.Getenv("OMNIGREX_PI_WORKFLOW_STATES"), MiseVolume: os.Getenv("OMNIGREX_PI_WORKFLOW_TOOLS"), WorkspaceRoot: filepath.Join(root, "workspaces"), RuntimeStateRoot: filepath.Join(root, "state"), MiseRoot: filepath.Join(root, "tools"), MicrosandboxExecutable: "/usr/local/bin/msb", MicrosandboxFirmware: "/usr/local/lib/libkrunfw.so.5.6.1", MicrosandboxHome: "/tmp/msb-workflow", MicrosandboxNamespace: uuid.NewString(), MicrosandboxCPUs: 1, MicrosandboxRootDiskMiB: 4096, MCPEndpointURL: endpoint, AgentTurnExecutionTurnTimeout: 3 * time.Minute}
	image, platform := workflowQualificationImage(t)
	settings.ShutdownTimeout = 10 * time.Second
	p, err := profile.NewPiV2(image, platform)
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
	t.Cleanup(func() { _ = backend.Close() })
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
	if mode == "slow-bootstrap" || mode == "startup-timeout" {
		// Deliberately slow repository-defined installation, without downloading
		// tools or calling an external provider. The real Bootstrap runs this hook.
		content := "[hooks]\npostinstall = \"sleep 32; touch /home/omnigrex/.local/share/omnigrex-tool-data/assignment/slow-bootstrap-complete\"\n"
		if err := os.WriteFile(filepath.Join(seed, "mise.toml"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run(seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "add", ".")
	run(seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Seed repository")
	head := run(seed, "rev-parse", "HEAD")
	run(seed, "push", bare, "HEAD:refs/heads/main")
	git := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexec /usr/bin/git -c 'url.file://"+bare+".insteadOf=https://github.com/owner/repo.git' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	workspaces, err := workspace.New(workspace.Options{WorkspaceRoot: settings.WorkspaceRoot, PublicationRoot: filepath.Join(root, "publication"), MiseRoot: settings.MiseRoot, CaptureRoot: filepath.Join(root, "state", "captures"), CaptureSource: piFaultCaptureSource{WorkspaceSource: backend.Workspaces, fault: fault}, CaptureLedger: database, GitExecutable: git})
	if err != nil {
		t.Fatal(err)
	}
	github := &workflowQualificationGitHub{replayGatewayGitHub: &replayGatewayGitHub{branch: "omnigrex/issue-123", base: "main"}, bare: bare, base: head, advanceDefault: mode == "default-branch-refresh", loseReviewResponse: mode == "lost-terminal-mutation-response"}
	toolBackend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: github, Credentials: replayGatewayCredentials{}, Publisher: workspaces, Snapshots: workspaces, Installer: backend.Snapshots, Workflow: mcp.LedgerWorkflowMutations{}, PriorIntents: database, Handoffs: database, Policies: configuration.Policies})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := mcp.New(mcp.Config{EndpointURL: endpoint, Store: database, Backend: toolBackend, Policies: configuration.Policies, LifecycleContext: ctx, MutationFinalizationTimeout: 30 * time.Second, Now: fault.now})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if mode == "concurrent-turns" && request.URL.Path == "/qualification" {
			fault.serveReadBarrier(response, request)
			return
		}
		gateway.ServeHTTP(response, request)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	authority := &workflowAuthorityAudit{Gateway: gateway}
	factory := &workflowFixtureFactory{Factory: backend.Factory, localImage: localImage, fault: fault}
	startupTimeout := 2 * time.Minute
	if mode == "startup-timeout" {
		startupTimeout = 200 * time.Millisecond
	}
	launcher, err := agentturn.NewLauncher(agentturn.LauncherConfig{Store: database, Registry: profile.Resolver{Catalog: registry.Catalog, Source: database}, Workspace: workspaces, Gateway: authority, Execution: factory, States: backend.States, DeveloperCredentials: &integrationCredentialProvider{credential: "developer-token"}, ReviewerCredentials: &integrationCredentialProvider{credential: "reviewer-token"}, AgentAdapters: map[string]session.TurnAdapter{"pi-rpc/v2": pi.TurnAdapter{Store: database, StartupTimeout: startupTimeout}}, MemoryBytes: 512 << 20, Policies: configuration.Policies, Definition: configuration.Reducer.Definition(), PathEnvironmentAllowlist: allowedPaths})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: github, Workspace: workspaces, DeveloperCredentials: &integrationCredentialProvider{credential: "developer-token"}, ReviewerCredentials: &integrationCredentialProvider{credential: "reviewer-token"}})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := agentturn.NewExecutionWorker(agentturn.ExecutionWorkerDependencies{Store: database, DeveloperCredentials: &integrationCredentialProvider{credential: "developer-token"}, ReviewerCredentials: &integrationCredentialProvider{credential: "reviewer-token"}, DefaultBranch: workflowQualificationBranch{head: head}, Launcher: launcher, Outcomes: outcomes, Workspace: workspaces, PublicationRecovery: agentturn.NewPublicationRecoveryWithCredentials(database, workspaces, github, &integrationCredentialProvider{credential: "developer-token"}), Policies: configuration.Policies, Definition: configuration.Reducer.Definition()}, agentturn.ExecutionWorkerConfig{ClaimOwner: "qualified-workflow", LeaseDuration: 30 * time.Second, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, TurnTimeout: 3 * time.Minute, CleanupTimeout: 30 * time.Second, ConcurrencyLimit: 1, ProviderCredentialJSON: json.RawMessage(`{"controlled":{"type":"api_key","key":"workflow-only-provider-key"}}`)})
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
	preparer.SetReviewGate(&agentturn.ReviewGate{Store: database, Source: workflowQualificationProfiles{head: head}, Observer: github})
	f := &piWorkflowFixture{ctx: ctx, database: database, databaseURL: url, password: secret, settings: settings, runtime: p, backend: backend, fault: fault, launcher: launcher, preparer: preparer, worker: worker, workspaces: workspaces, github: github, authority: authority, workflowID: application.WorkflowID, head: head, git: git}
	t.Cleanup(func() {
		f.cleanup(t)
		_ = f.backend.Close()
		f.database.Close()
	})
	return f
}

type workflowAuthorityAudit struct {
	*mcp.Gateway
	mu            sync.Mutex
	tokens        []string
	registrations []mcp.Registration
}

func (g *workflowAuthorityAudit) Register(scope mcp.TokenScope) (mcp.Registration, error) {
	registration, err := g.Gateway.Register(scope)
	if err == nil {
		g.mu.Lock()
		g.registrations = append(g.registrations, registration)
		for _, header := range registration.Server.Headers {
			if token, ok := strings.CutPrefix(header.Value, "Bearer "); ok {
				g.tokens = append(g.tokens, token)
			}
		}
		g.mu.Unlock()
	}
	return registration, err
}

func (g *workflowAuthorityAudit) revokeAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, registration := range g.registrations {
		g.Revoke(registration)
	}
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
	fault      *piWorkflowFault
}

func (f workflowFixtureFactory) New(request execution.Request, ledger execution.Ledger) (execution.Engine, error) {
	// Only the disposable image's model catalog changes; production Bootstrap
	// and all launch/state/MCP/capture/settlement boundaries stay in use.
	request.Command = append([]string(nil), request.Command...)
	request.Command[len(request.Command)-1] = "/opt/omnigrex/pi/qualification/turn.mjs"
	if f.fault != nil && f.fault.mode != "" {
		values := make([]string, 0, len(request.Environment))
		for _, value := range request.Environment {
			if !strings.HasPrefix(value, "OMNIGREX_RUNTIME_ENV_KEYS=") {
				values = append(values, value)
			}
		}
		request.Environment = environment.BindEnvironment(append(values, "OMNIGREX_PI_WORKFLOW_FAULT="+f.fault.mode, "OMNIGREX_PI_WORKFLOW_CONTROL_URL="+f.fault.controlURL))
	}
	if f.localImage != "" {
		request.Image = f.localImage
		request.State.Image = f.localImage
	}
	engine, err := f.Factory.New(request, ledger)
	if err != nil {
		return nil, err
	}
	return workflowFixtureEngine{Engine: engine, request: request, fault: f.fault}, nil
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
	fault   *piWorkflowFault
}

func (e workflowFixtureEngine) Create(ctx context.Context, _ execution.Request, stderr io.Writer) (execution.Process, error) {
	process, err := e.Engine.Create(ctx, e.request, stderr)
	if process != nil && e.fault != nil {
		e.fault.executions = append(e.fault.executions, piFixtureExecution{process: process, key: e.request.Key})
		if e.fault.mode == "restart-during-creation" {
			// Creation has completed externally, but its response is lost.
			_ = process.Transport().Close()
			return nil, execution.ErrUnknownEffect
		}
		if e.fault.mode == "cleanup-failure" || e.fault.mode == "startup-timeout" ||
			e.fault.mode == "post-publication-cleanup" && len(e.fault.executions) == 1 {
			process = piFaultProcess{Process: process}
		}
	}
	return process, err
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
	return []byte(fmt.Sprintf("---\nname: %s\nrole: %s\nruntime: pi-rpc/v2\nmodel: controlled/workflow\npermissions:\n  read: allow\n  bash: allow\n---\nPerform the controlled Work Item.\n", strings.ToLower(role), role)), nil
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
	bare, base          string
	review              githubapi.Review
	reviewCalls         int
	ciReads             int
	ciPending           bool
	loseReviewResponse  bool
	advanceDefault      bool
	refreshObservations int
}

func (g *workflowQualificationGitHub) GetRepository(_ context.Context, _ string, owner, repo string) (githubapi.InstallationRepository, error) {
	return githubapi.InstallationRepository{ID: workflowLocator().RepositoryID, Owner: owner, Name: repo}, nil
}

func (g *workflowQualificationGitHub) ResolveDefaultBranch(ctx context.Context, credential, owner, repo string) (githubapi.DefaultBranch, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if credential != "developer-token" {
		return githubapi.DefaultBranch{}, errors.New("wrong acquisition authority")
	}
	if g.advanceDefault && g.refreshObservations == 0 {
		run := func(args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"--git-dir", g.bare}, args...)...)
			b, err := cmd.CombinedOutput()
			return strings.TrimSpace(string(b)), err
		}
		tree, err := run("rev-parse", g.base+"^{tree}")
		if err != nil {
			return githubapi.DefaultBranch{}, err
		}
		b, err := run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-p", g.base, "-m", "Advance default after workspace preparation")
		if err != nil {
			return githubapi.DefaultBranch{}, err
		}
		if _, err := run("update-ref", "refs/heads/main", b); err != nil {
			return githubapi.DefaultBranch{}, err
		}
		g.base = b
	}
	g.refreshObservations++
	return githubapi.DefaultBranch{Name: "main", CommitSHA: g.base}, nil
}

func (g *workflowQualificationGitHub) pull() githubapi.PullRequest {
	cmd := exec.Command("/usr/bin/git", "--git-dir", g.bare, "rev-parse", "refs/heads/"+g.branch)
	bytes, _ := cmd.Output()
	g.head = strings.TrimSpace(string(bytes))
	pr := g.pullRequest()
	pr.Base.SHA = g.base
	pr.Base.Repository = &struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}{ID: 9123, FullName: "owner/repo"}
	return pr
}
func (g *workflowQualificationGitHub) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.pull(), nil
}

func (*workflowQualificationGitHub) GetIssue(_ context.Context, credential, _, _ string, number int) (githubapi.Issue, error) {
	if credential != "developer-token" {
		return githubapi.Issue{}, errors.New("wrong read authority")
	}
	return githubapi.Issue{ID: workflowLocator().IssueID + int64(number) - workflowLocator().IssueNumber, Number: number, Title: "Scoped controlled Work Item", State: "open"}, nil
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
	if g.loseReviewResponse {
		return githubapi.Review{}, &url.Error{Op: "Post", URL: "https://api.github.invalid/reviews", Err: io.ErrUnexpectedEOF}
	}
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

func (g *workflowQualificationGitHub) DiscoverRequiredCI(_ context.Context, token, _, _, branch string) githubapi.CIInventory {
	if token != "developer-token" {
		return githubapi.CIInventory{Availability: githubapi.CIUnavailable}
	}
	return githubapi.CIInventory{Availability: githubapi.CIAvailable, Branch: branch, Requirements: []githubapi.CIRequirement{{Context: "build", Source: "classic"}}}
}
func (g *workflowQualificationGitHub) ObserveCI(_ context.Context, token, _, _, head string, repo int64) githubapi.CIObservation {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if token != "developer-token" {
		return githubapi.CIObservation{Availability: githubapi.CIUnavailable}
	}
	g.ciReads++
	state := "failure"
	if g.ciPending {
		state = "pending"
	}
	return githubapi.CIObservation{Availability: githubapi.CIAvailable, RepositoryID: repo, HeadSHA: head, Statuses: []githubapi.CommitStatus{{ID: 1, Context: "build", State: state}}}
}
func (g *workflowQualificationGitHub) VerifyCITarget(_ context.Context, _, _, _ string, repo, pr int64, number int, head, _, _ string) githubapi.CITargetMapping {
	return githubapi.CITargetMapping{RepositoryID: repo, PullRequestID: pr, PullRequestNumber: number, ReviewedHead: head, State: "head_only"}
}
func (*workflowQualificationGitHub) GetCheckRunMetadata(context.Context, string, string, string, int64) (githubapi.CheckRun, error) {
	return githubapi.CheckRun{}, errors.New("unexpected check metadata read")
}
