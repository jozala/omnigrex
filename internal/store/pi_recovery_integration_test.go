//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/startup"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

// Every scenario uses the selected real backend and the production Pi adapter,
// Store, Launcher, MCP gateway, capture/publication service and recovery workers.
func TestPiFailureRecovery(t *testing.T) {
	for _, mode := range []string{
		"restart-during-creation", "restart-during-prompt", "lost-transport",
		"runtime-exit-137", "resource-exhaustion", "lease-expiry", "mcp-expiry",
		"cleanup-failure", "interrupted-publication-capture", "lost-terminal-mutation-response", "concurrent-turns", "startup-timeout",
	} {
		t.Run(mode, func(t *testing.T) {
			f := newPiWorkflowFixture(t, mode)
			switch mode {
			case "startup-timeout":
				f.testStartupTimeout(t)
			case "interrupted-publication-capture", "lost-terminal-mutation-response":
				f.testMutationFault(t)
			case "concurrent-turns":
				f.testConcurrentTurns(t)
			default:
				f.testExecutionFault(t)
			}
		})
	}
}

func (f *piWorkflowFixture) testStartupTimeout(t *testing.T) {
	prepared := f.prepare(t)
	processed, err := f.worker.ProcessNext(f.ctx)
	if !processed || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("startup timeout with unconfirmed stop: %t %v", processed, err)
	}
	if len(f.fault.executions) != 1 || f.fault.executions[0].key.AgentTurnID != prepared.Commit.Turn.ID || f.github.openCalls != 0 {
		t.Fatalf("startup timeout admitted prompt or lost execution identity: %+v", f.fault.executions)
	}
	// The Session is still CREATING. Live finalization cannot close mutation
	// admission under an active-Session fence, so recovery starts only after
	// the abandoned execution lease is expired and reconciled.
	job := agentTurnExecutionJob(t, f.pool(t), f.ctx, prepared.Commit.Turn)
	expireAgentTurnExecution(t, f.pool(t), f.ctx, job.ID, prepared.Commit.Turn.ID)
	recovery, err := f.database.RecoverExpiredAgentTurn(f.ctx, prepared.Commit.Turn.ID, prepared.Commit.Turn.ExecutionEpoch)
	if err != nil || recovery.StopRuntimeJobID == "" {
		t.Fatalf("startup timeout did not enqueue fenced stop: %+v %v", recovery, err)
	}
	lease := store.AgentTurnLease{AgentTurn: prepared.Commit.Turn}
	f.assertRecoveryBarrier(t, lease)
	f.recover(t, lease, true)
	f.assertSuccessorAllowed(t, lease)
}

type piWorkflowFixture struct {
	ctx                   context.Context
	database              *store.Store
	databaseURL, password string
	settings              config.Config
	runtime               profile.Profile
	backend               *composition.Backend
	fault                 *piWorkflowFault
	launcher              *agentturn.Launcher
	preparer              *agentturn.Preparer
	worker                *agentturn.ExecutionWorker
	workspaces            *workspace.Lifecycle
	github                *workflowQualificationGitHub
	authority             *workflowAuthorityAudit
	workflowID, head, git string
}

func isolatedPiWorkflowDatabase(t *testing.T, ctx context.Context) string {
	t.Helper()
	u, err := url.Parse(os.Getenv("OMNIGREX_PI_WORKFLOW_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("postgres", "qualification-only-password")
	admin := openPool(t, u.String())
	name := "pi_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("remove isolated qualification database: %v", err)
		}
	})
	u.Path = "/" + name
	u.User = url.User("postgres")
	return u.String()
}

func (f *piWorkflowFixture) pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return openPool(t, f.databaseURL+"&password=qualification-only-password")
}

func (f *piWorkflowFixture) prepare(t *testing.T) agentturn.Result {
	t.Helper()
	prepared, err := f.preparer.Prepare(f.ctx, agentturn.Request{Lease: claimPreparationJob(t, f.database, f.ctx), InstallationCredential: "developer-token", RepositoryOwner: "owner", RepositoryName: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func (f *piWorkflowFixture) assertState(t *testing.T, wanted workflow.State) {
	t.Helper()
	var state workflow.State
	if err := f.pool(t).QueryRow(f.ctx, `SELECT status FROM workflows WHERE id=$1`, f.workflowID).Scan(&state); err != nil || state != wanted {
		t.Fatalf("Workflow: %s %v; want %s", state, err, wanted)
	}
}

func (f *piWorkflowFixture) launch(t *testing.T) (store.AgentTurnLease, *agentturn.RuntimeHandle, error) {
	t.Helper()
	f.prepare(t)
	lease, acquired, err := f.database.ClaimAndAcquireAgentTurn(f.ctx, "failure-qualification", time.Minute, 2)
	if err != nil || !acquired {
		t.Fatalf("acquire Turn: %t %v", acquired, err)
	}
	scope, err := f.database.GetAgentTurnExecutionContext(f.ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := f.launcher.Launch(f.ctx, agentturn.LaunchRequest{Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.com/owner/repo.git", DefaultBranchName: "main", DefaultBranchSHA: f.head, InitialFeatureBranch: fmt.Sprintf("omnigrex/issue-%d", scope.Issue.Number), RepositoryCredential: "developer-token", ProviderCredentialJSON: json.RawMessage(`{"controlled":{"type":"api_key","key":"workflow-only-provider-key"}}`)})
	if handle != nil {
		lease = handle.CurrentLease()
	}
	return lease, handle, err
}

func (f *piWorkflowFixture) prompt(t *testing.T, lease store.AgentTurnLease, handle *agentturn.RuntimeHandle, onAdmitted func()) (agent.Outcome, error) {
	t.Helper()
	execution, err := f.database.GetAgentTurnExecutionContext(f.ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.OpenMutationAdmission(f.ctx, lease); err != nil {
		t.Fatal(err)
	}
	definition := builtinStoreConfig(t).Reducer.Definition()
	envelope, err := agentturn.BuildEventEnvelope(execution, f.head, definition, builtinStoreConfig(t).Policies)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
	defer cancel()
	return handle.AgentConnection().Prompt(ctx, lease, execution.Session, envelope, &piAdmissionSink{admitted: onAdmitted})
}

func (f *piWorkflowFixture) testExecutionFault(t *testing.T) {
	lease, handle, err := f.launch(t)
	if f.fault.mode == "restart-during-creation" {
		if !errors.Is(err, execution.ErrUnknownEffect) || handle == nil {
			t.Fatalf("lost creation response did not retain recovery ownership: %v", err)
		}
	} else {
		if err != nil || handle == nil {
			t.Fatalf("launch: %v", err)
		}
		outcome, promptErr := f.prompt(t, lease, handle, func() {
			switch f.fault.mode {
			case "lost-transport", "restart-during-prompt", "lease-expiry", "cleanup-failure":
				_ = f.fault.executions[0].process.Transport().Close()
			case "mcp-expiry":
				f.fault.clockOffset.Store(int64(2 * time.Hour))
			}
		})
		if !outcome.Admitted {
			t.Fatalf("fault did not occur after prompt admission: %+v %v", outcome, promptErr)
		}
		if f.fault.mode != "mcp-expiry" && (outcome.Status != agent.Unknown || promptErr == nil) {
			t.Fatalf("fault invented settled execution: %+v %v", outcome, promptErr)
		}
		if f.fault.mode == "mcp-expiry" {
			f.assertTokenRejected(t, f.authority.tokens[0])
			var mutations int
			if err := f.pool(t).QueryRow(f.ctx, `SELECT count(*) FROM tool_invocations WHERE agent_turn_id=$1`, lease.ID).Scan(&mutations); err != nil || mutations != 0 {
				t.Fatalf("expired MCP authority admitted mutation: %d %v", mutations, err)
			}
		}
		if f.fault.mode == "lost-transport" || f.fault.mode == "cleanup-failure" || f.fault.mode == "restart-during-prompt" || f.fault.mode == "lease-expiry" {
			observation, err := f.fault.executions[0].process.ObserveExit(f.ctx)
			if err != nil || !observation.Running || observation.Exited || observation.OOMKilled {
				t.Fatalf("transport loss treated as stop: %+v %v", observation, err)
			}
		}
		if f.fault.mode == "runtime-exit-137" || f.fault.mode == "resource-exhaustion" {
			observation, err := f.fault.executions[0].process.ObserveExit(f.ctx)
			if err != nil || !observation.Exited || observation.Running || f.fault.mode == "runtime-exit-137" && observation.OOMKilled {
				t.Fatalf("runtime exit evidence: %+v %v", observation, err)
			}
		}
		if f.fault.mode == "cleanup-failure" {
			if err := handle.Cleanup(f.ctx); !errors.Is(err, execution.ErrUnknownEffect) {
				t.Fatalf("unconfirmed stop cleanup: %v", err)
			}
			if _, err := f.workspaces.TerminalCapture(f.ctx, f.fault.executions[0].key); err == nil {
				t.Fatal("unconfirmed stop created terminal evidence")
			}
		}
	}
	// Expire every element of the durable authority chain, then detach the old
	// control plane. The fresh Store/backend must recover only persisted facts.
	expireAgentTurnExecution(t, f.pool(t), f.ctx, lease.JobLease.ID, lease.ID)
	if f.fault.mode == "lease-expiry" {
		if err := f.database.ValidateTurnFence(f.ctx, lease); !errors.Is(err, store.ErrAgentTurnFenceLost) {
			t.Fatalf("stale fence: %v", err)
		}
		if err := f.database.OpenMutationAdmission(f.ctx, lease); !errors.Is(err, store.ErrAgentTurnFenceLost) {
			t.Fatalf("stale mutation admission: %v", err)
		}
	}
	if connection := handle.AgentConnection(); connection != nil {
		_ = connection.Close()
	}
	f.authority.revokeAll()
	f.database.Close()
	if err := f.backend.Close(); err != nil {
		t.Fatal(err)
	}
	f.database, err = store.Open(f.ctx, f.databaseURL, f.password, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	f.backend, err = composition.NewBackend(f.ctx, f.settings, f.runtime, f.database, f.database)
	if err != nil {
		t.Fatal(err)
	}
	f.workspaces, err = workspace.New(workspace.Options{WorkspaceRoot: f.settings.WorkspaceRoot, PublicationRoot: filepath.Join(os.Getenv("OMNIGREX_PI_WORKFLOW_ROOT"), "publication"), MiseRoot: f.settings.MiseRoot, CaptureRoot: filepath.Join(f.settings.RuntimeStateRoot, "captures"), CaptureSource: f.backend.Workspaces, CaptureLedger: f.database, GitExecutable: f.git})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := startup.NewReconciler(f.database, f.backend.Recovery, f.backend.Recovery, f.backend.Recovery, startup.ReconcilerOptions{MaxPasses: 10, MaxRuntimeProcesses: 100, MaxRecoveries: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.assertRecoveryBarrier(t, lease)
	f.recover(t, lease, true)
	if observation, err := f.backend.Cleaner.ObserveKeyExit(f.ctx, f.fault.executions[0].key); err != nil || observation.Running || observation.Exited || observation.OOMKilled {
		t.Fatalf("removed resource invented exit evidence: %+v %v", observation, err)
	}
	f.assertSuccessorAllowed(t, lease)
	if err := f.backend.Cleaner.EnsureKeyAbsent(f.ctx, f.fault.executions[0].key); err != nil {
		t.Fatalf("idempotent exact cleanup: %v", err)
	}
}

func (f *piWorkflowFixture) assertSuccessorAllowed(t *testing.T, lease store.AgentTurnLease) {
	t.Helper()
	recovery, err := f.database.CompleteAgentTurnRecovery(f.ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil || !recovery.SuccessorAllowed || recovery.SettlementID == "" {
		t.Fatalf("confirmed recovery did not release successor: %+v %v", recovery, err)
	}
	registry, err := profile.NewRegistry(f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	f.preparer = agentturn.NewPreparer(agentprofile.NewLoader(workflowQualificationProfiles{head: f.head}, builtinStoreConfig(t).Policies, "TMPDIR", "GOTMPDIR", "GOCACHE", "GOPATH"), agentprofile.SingletonSelector{}, registry, f.database)
	prepared := f.prepare(t)
	if prepared.Commit.Participant.ID != lease.AgentAssignmentID || prepared.Commit.Session.ID != lease.AgentSessionID || prepared.Commit.Turn.ExecutionEpoch <= lease.ExecutionEpoch {
		t.Fatal("successor changed retained ownership or reused stale epoch")
	}
	if _, _, err := f.database.ExecutionLedger(lease).ReserveExecution(f.ctx, f.fault.executions[0].key, strings.Repeat("a", 64)); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("stale execution dispatch admitted after recovery: %v", err)
	}
}

func (f *piWorkflowFixture) testMutationFault(t *testing.T) {
	prepared := f.prepare(t)
	processed, err := f.worker.ProcessNext(f.ctx)
	if f.fault.mode == "interrupted-publication-capture" {
		if !processed || !f.fault.captureInterrupted.Load() {
			t.Fatalf("publication capture fault not exercised: %t %v", processed, err)
		}
		if output, err := exec.CommandContext(f.ctx, "/usr/bin/git", "--git-dir", f.github.bare, "show-ref", "--verify", "refs/heads/"+f.github.branch).Output(); err == nil || len(output) != 0 {
			t.Fatal("partial workspace capture reached remote publication")
		}
		if f.github.openCalls != 0 {
			t.Fatal("partial capture opened Change Proposal")
		}
		key := f.fault.executions[0].key
		if _, found, err := f.database.LoadCapture(f.ctx, execution.CaptureRequest{Workspace: execution.WorkspaceRef{Key: key, Seed: f.head}, OperationID: f.fault.captureOperation, Purpose: execution.PublicationCapture}); err != nil || found {
			t.Fatalf("partial capture sealed: %t %v", found, err)
		}
		if _, err := f.workspaces.TerminalCapture(f.ctx, key); err != nil {
			t.Fatalf("capture interruption broke later terminal observation: %v", err)
		}
		lease := store.AgentTurnLease{AgentTurn: prepared.Commit.Turn}
		f.assertRecoveryBarrier(t, lease)
		f.recover(t, lease, false)
		worker := f.mutationWorker(t)
		for range 3 {
			if processed, err := worker.ProcessNext(f.ctx); !processed || !errors.Is(err, mcp.ErrMutationReconciliationUnresolved) {
				t.Fatalf("partial-capture bounded recovery: %t %v", processed, err)
			}
			time.Sleep(2 * time.Millisecond)
		}
		f.assertState(t, workflow.StateNeedsHuman)
		var handoffs, publications int
		if err := f.pool(t).QueryRow(f.ctx, `SELECT count(*) FROM jobs WHERE workflow_id=$1 AND kind='PUBLISH_HUMAN_HANDOFF' AND payload->>'reason'=$2`, f.workflowID, string(workflow.ReasonAgentTurnMutationReconciliationExhausted)).Scan(&handoffs); err != nil || handoffs != 1 {
			t.Fatalf("uncertainty did not produce one Human Handoff: %d %v", handoffs, err)
		}
		if err := f.pool(t).QueryRow(f.ctx, `SELECT count(*) FROM tool_invocations WHERE agent_turn_id=$1 AND tool_name='publish_changes'`, lease.ID).Scan(&publications); err != nil || publications != 1 {
			t.Fatalf("interrupted publication was blindly repeated: %d %v", publications, err)
		}
		if processed, err := worker.ProcessNext(f.ctx); processed || err != nil {
			t.Fatalf("exhausted recovery kept retrying: %t %v", processed, err)
		}
		return
	}
	if !processed || err != nil {
		t.Fatalf("prerequisite Developer Turn: %t %v", processed, err)
	}
	f.assertState(t, workflow.StateReviewing)
	prepared = f.prepare(t)
	processed, err = f.worker.ProcessNext(f.ctx)
	if !processed {
		t.Fatalf("Reviewer fault not processed: %v", err)
	}
	lease := store.AgentTurnLease{AgentTurn: prepared.Commit.Turn}
	var unknown int
	if err := f.pool(t).QueryRow(f.ctx, `SELECT count(*) FROM tool_invocations WHERE agent_turn_id=$1 AND tool_name='submit_review' AND state IN ('UNKNOWN','RECONCILING')`, lease.ID).Scan(&unknown); err != nil || unknown != 1 || f.github.reviewCalls != 1 {
		t.Fatalf("lost review response was repeated or lost: unknown=%d calls=%d %v", unknown, f.github.reviewCalls, err)
	}
	f.assertRecoveryBarrier(t, lease)
	f.recover(t, lease, false)
	worker := f.reconcileMutations(t)
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: f.database, GitHub: f.github, Workspace: f.workspaces})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := agentturn.NewTerminalCorroborationWorker(f.database, outcomes, &integrationCredentialProvider{credential: "developer-token"}, &integrationCredentialProvider{credential: "reviewer-token"}, f.workspaces, agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "terminal-recovery", Window: time.Minute, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.ProcessOne(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, workflow.StatePRReady)
	if f.github.reviewCalls != 1 {
		t.Fatal("recovery repeated native review")
	}
	if processed, err := worker.ProcessNext(f.ctx); processed || err != nil {
		t.Fatalf("recovery was not idempotent: %t %v", processed, err)
	}
}

func (f *piWorkflowFixture) reconcileMutations(t *testing.T) *mcp.RecoveryWorker {
	t.Helper()
	worker := f.mutationWorker(t)
	if processed, err := worker.ProcessNext(f.ctx); !processed || err != nil {
		t.Fatalf("read-only mutation recovery: %t %v", processed, err)
	}
	return worker
}

func (f *piWorkflowFixture) mutationWorker(t *testing.T) *mcp.RecoveryWorker {
	t.Helper()
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: f.github, Credentials: replayGatewayCredentials{}, Publications: f.workspaces, Policies: builtinStoreConfig(t).Policies})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := mcp.NewRecoveryWorker(f.database, reconciler, mcp.RecoveryWorkerConfig{ClaimOwner: "mutation-recovery", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func (f *piWorkflowFixture) testConcurrentTurns(t *testing.T) {
	first, firstHandle, err := f.launch(t)
	if err != nil {
		t.Fatal(err)
	}
	delivery := workflowDelivery(uuid.NewString())
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	delivery.IssueID++
	delivery.IssueNumber++
	claim := claimWorkflowDelivery(t, f.database, f.ctx, delivery)
	locator := workflowLocator()
	locator.IssueID++
	locator.IssueNumber++
	if _, err := f.database.CompleteWebhookTransition(f.ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "trigger"), locator, func(eventContext store.WorkflowEventContext) (workflow.Event, error) {
		return workflow.TriggerEvent{EventMetadata: eventContext.Metadata, AttemptID: uuid.NewString(), AttemptNumber: 1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	second, secondHandle, err := f.launch(t)
	if err != nil {
		t.Fatal(err)
	}
	if first.AgentSessionID == second.AgentSessionID || first.AgentAssignmentID == second.AgentAssignmentID || f.authority.tokens[0] == f.authority.tokens[1] {
		t.Fatal("concurrent Turns shared Session, Participant or MCP authority")
	}
	admitted := make(chan struct{}, 2)
	type result struct {
		outcome agent.Outcome
		err     error
	}
	done := make(chan result, 2)
	for _, turn := range []struct {
		lease  store.AgentTurnLease
		handle *agentturn.RuntimeHandle
	}{{first, firstHandle}, {second, secondHandle}} {
		go func() {
			outcome, err := f.prompt(t, turn.lease, turn.handle, func() { admitted <- struct{}{} })
			done <- result{outcome, err}
		}()
	}
	for range 2 {
		select {
		case <-admitted:
		case <-f.ctx.Done():
			t.Fatal("concurrent prompt admission timed out")
		}
	}
	f.expectScopedReads(t, "first", first, second)
	if err := firstHandle.Cleanup(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.assertTokenRejected(t, f.authority.tokens[0])
	if observation, err := f.fault.executions[1].process.ObserveExit(f.ctx); err != nil || !observation.Running {
		t.Fatalf("one Turn cleanup stopped its neighbor: %+v %v", observation, err)
	}
	if err := f.backend.Cleaner.EnsureKeyAbsent(f.ctx, f.fault.executions[0].key); err != nil {
		t.Fatal(err)
	}
	if observation, err := f.fault.executions[1].process.ObserveExit(f.ctx); err != nil || !observation.Running {
		t.Fatal("stale exact cleanup affected neighbor")
	}
	close(f.fault.continueReads)
	f.expectScopedReads(t, "second", second)
	if err := secondHandle.Cleanup(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct{ own, other store.AgentTurnLease }{{first, second}, {second, first}} {
		content, err := os.ReadFile(filepath.Join(f.settings.RuntimeStateRoot, "assignment-"+pair.own.AgentAssignmentID, "runtime-state", "sessions", pair.own.AgentSessionID, "session.jsonl"))
		if err != nil || !strings.Contains(string(content), pair.own.ID) || strings.Contains(string(content), pair.other.ID) {
			t.Fatalf("concurrent retained Session isolation failed: %v", err)
		}
	}
	for range 2 {
		select {
		case result := <-done:
			if !result.outcome.Admitted || result.outcome.Status != agent.Unknown || result.err == nil {
				t.Fatalf("detached concurrent prompt: %+v %v", result.outcome, result.err)
			}
		case <-f.ctx.Done():
			t.Fatal("concurrent transport did not detach")
		}
	}
}

func (f *piWorkflowFixture) assertRecoveryBarrier(t *testing.T, lease store.AgentTurnLease) {
	t.Helper()
	if _, err := f.database.CompleteAgentTurnRecovery(f.ctx, lease.ID, lease.ExecutionEpoch); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Fatalf("recovery advanced without confirmed stop: %v", err)
	}
	if replacement, acquired, err := f.database.ClaimAndAcquireAgentTurn(f.ctx, "replacement", time.Minute, 2); err != nil || acquired || replacement.ID != "" {
		t.Fatalf("replacement overlapped unresolved execution: %t %v", acquired, err)
	}
}

func (f *piWorkflowFixture) stopWorker(t *testing.T, cleaner agentturn.ExactRuntimeCleaner) *agentturn.StopWorker {
	t.Helper()
	worker, err := agentturn.NewStopWorker(f.database, cleaner, f.workspaces, agentturn.StopWorkerConfig{ClaimOwner: "fresh-stop-worker", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Microsecond, TerminalCaptureRequired: f.database.AgentTurnTerminalCaptureRequired})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func (f *piWorkflowFixture) recover(t *testing.T, lease store.AgentTurnLease, failStop bool) {
	t.Helper()
	if failStop {
		worker := f.stopWorker(t, piUnknownStopCleaner{Cleanup: f.backend.Cleaner})
		if processed, err := worker.ProcessNext(f.ctx); !processed || !errors.Is(err, execution.ErrUnknownEffect) {
			t.Fatalf("uncertain cleanup was acknowledged: %t %v", processed, err)
		}
		f.assertRecoveryBarrier(t, lease)
	}
	worker := f.stopWorker(t, f.backend.Cleaner)
	if processed, err := worker.ProcessNext(f.ctx); !processed || err != nil {
		t.Fatalf("fresh stop/capture/removal recovery: %t %v", processed, err)
	}
}

func (f *piWorkflowFixture) assertTokenRejected(t *testing.T, token string) {
	t.Helper()
	request, _ := http.NewRequestWithContext(f.ctx, http.MethodPost, f.settings.MCPEndpointURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired/revoked MCP token accepted: %d", response.StatusCode)
	}
}

type piWorkflowFault struct {
	mode               string
	clockOffset        atomic.Int64
	captureInterrupted atomic.Bool
	captureOperation   string
	executions         []piFixtureExecution
	controlURL         string
	continueReads      chan struct{}
	reads              chan piReadObservation
}

type piFixtureExecution struct {
	process execution.Process
	key     execution.Key
}

type piReadObservation struct {
	turnID, phase string
	issueNumber   int64
}

func (f *piWorkflowFault) serveReadBarrier(response http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	if query.Get("phase") == "continue" {
		select {
		case <-f.continueReads:
			response.WriteHeader(http.StatusOK)
		case <-request.Context().Done():
		}
		return
	}
	number, err := strconv.ParseInt(query.Get("issue"), 10, 64)
	if err != nil {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	select {
	case f.reads <- piReadObservation{turnID: query.Get("turn"), phase: query.Get("phase"), issueNumber: number}:
		response.WriteHeader(http.StatusOK)
	case <-request.Context().Done():
	}
}

func (f *piWorkflowFixture) expectScopedReads(t *testing.T, phase string, turns ...store.AgentTurnLease) {
	t.Helper()
	expected := make(map[string]int64, len(turns))
	for _, lease := range turns {
		scope, err := f.database.GetAgentTurnExecutionContext(f.ctx, lease)
		if err != nil {
			t.Fatal(err)
		}
		expected[lease.ID] = scope.Issue.Number
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for len(expected) > 0 {
		select {
		case read := <-f.fault.reads:
			if number, ok := expected[read.turnID]; !ok || read.phase != phase || read.issueNumber != number {
				t.Fatal("Pi process used another Turn's MCP authority")
			}
			delete(expected, read.turnID)
		case <-timer.C:
			t.Fatal("Pi process did not complete its scoped MCP read")
		case <-f.ctx.Done():
			t.Fatal(f.ctx.Err())
		}
	}
}

func (f *piWorkflowFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, execution := range f.fault.executions {
		_ = execution.process.Transport().Close()
		if err := f.backend.Cleaner.EnsureKeyAbsent(ctx, execution.key); err != nil {
			t.Errorf("exact fixture cleanup through live backend: %v", err)
		}
	}
}

func (f *piWorkflowFault) now() time.Time { return time.Now().Add(time.Duration(f.clockOffset.Load())) }

type piFaultProcess struct{ execution.Process }

func (piFaultProcess) Stop(context.Context, time.Duration) error { return execution.ErrUnknownEffect }

type piUnknownStopCleaner struct{ composition.Cleanup }

func (piUnknownStopCleaner) EnsureKeyStopped(context.Context, execution.Key) error {
	return execution.ErrUnknownEffect
}

type piFaultCaptureSource struct {
	execution.WorkspaceSource
	fault *piWorkflowFault
}

func (s piFaultCaptureSource) ReadWorkspace(ctx context.Context, request execution.CaptureRequest, consume func(io.Reader) error) error {
	if s.fault.mode == "interrupted-publication-capture" && request.Purpose == execution.PublicationCapture && s.fault.captureInterrupted.CompareAndSwap(false, true) {
		s.fault.captureOperation = request.OperationID
		return s.WorkspaceSource.ReadWorkspace(ctx, request, func(reader io.Reader) error {
			// Interrupt an actual quiesced transfer before its complete artifact can
			// be sealed, exercising the backend's suspension/resume cleanup too.
			if _, err := io.CopyN(io.Discard, reader, 512); err != nil {
				return err
			}
			return context.Canceled
		})
	}
	return s.WorkspaceSource.ReadWorkspace(ctx, request, consume)
}

type piAdmissionSink struct {
	once     sync.Once
	admitted func()
}

func (s *piAdmissionSink) Emit(_ context.Context, event agentevent.AgentEvent) error {
	if event.Kind == "prompt_admitted" && s.admitted != nil {
		s.once.Do(s.admitted)
	}
	return nil
}
