//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
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

func TestExecutionLauncherUsesSingleConnectionPool(t *testing.T) {
	testExecutionLauncherSingleConnectionPool(t, false, false, false)
}

func TestExecutionLauncherCaptureDoesNotHoldSingleConnectionFence(t *testing.T) {
	testExecutionLauncherSingleConnectionPool(t, true, false, false)
}

func TestCreatingSessionStartupCleanupProofSurvivesRestart(t *testing.T) {
	for _, loseProofFence := range []bool{false, true} {
		name := "admitted proof before removal"
		if loseProofFence {
			name = "lost lease denies proof and removal"
		}
		t.Run(name, func(t *testing.T) {
			testExecutionLauncherSingleConnectionPool(t, false, true, loseProofFence)
		})
	}
}

func testExecutionLauncherSingleConnectionPool(t *testing.T, capture, preparationOnly, loseProofFence bool) {
	t.Helper()
	postgres := startPostgres(t)
	password := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(password, []byte(postgresPassword), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, err := openTimedStore(t, ctx, postgres.databaseURL(false)+"&pool_max_conns=1", password, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	pool := openPool(t, postgres.databaseURL(true))
	runtimeProfile, err := profile.NewPiV2("registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	triggerPreparationWorkflow(t, database, ctx, "60000000-0000-4000-8000-000000009920", "60000000-0000-4000-8000-000000009921")
	spec := preparationSpec("single-connection-profile", "provider/test")
	updateStagePreparation(&spec, workflow.StageImplementation, func(preparation *store.ParticipantPreparation) {
		preparation.Binding = store.ParticipantRuntimeBinding{
			AgentProfileName: "developer", RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2",
			RuntimeProfileContentSHA256: runtimeProfile.ContentSHA256(), RuntimeImageDigest: runtimeProfile.Contract().Image,
		}
		preparation.RuntimeContract, _ = json.Marshal(runtimeProfile.Contract())
	})
	prepared, err := database.PrepareAgentTurn(ctx, claimPreparationJob(t, database, ctx), spec)
	if err != nil {
		t.Fatal(err)
	}
	turn := prepared.Turn
	lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "runtime", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !preparationOnly {
		bindPreparedRuntime(t, database, ctx, lease)
	}
	registry, err := profile.NewRegistry(runtimeProfile)
	if err != nil {
		t.Fatal(err)
	}
	factory := &singleConnectionFactory{observer: pool, whileProvisioning: func(ctx context.Context) error { return database.ValidateTurnFence(ctx, lease) }}
	startupErr := errors.New("first Session startup failed after main resource binding")
	if preparationOnly {
		factory.engine.startErr = startupErr
		if loseProofFence {
			factory.engine.observe = func(ctx context.Context) error {
				expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
				return nil
			}
		}
		factory.engine.beforeRemove = func(ctx context.Context) error {
			var proof bool
			if err := pool.QueryRow(ctx, `SELECT preparation_only_cleanup FROM execution_reservations
WHERE agent_turn_id = $1 AND execution_epoch = $2`, lease.ID, lease.ExecutionEpoch).Scan(&proof); err != nil {
				return err
			}
			if !proof {
				t.Fatal("main resource removed before cleanup proof committed")
			}
			return nil
		}
	}
	workspaces := singleConnectionWorkspace{}
	adapter := singleConnectionAdapter{prepareErr: errors.New("fixture Session preparation unavailable")}
	captures := 0
	if preparationOnly {
		workspaces.capture = func(context.Context) error {
			captures++
			return errors.New("unprompted startup should use durable cleanup proof")
		}
	}
	if capture {
		adapter.prepareErr = nil
		workspaces.capture = func(ctx context.Context) error {
			captures++
			// Reentering the real Store needs the same sole pool connection. This
			// deadlocks if capture is performed inside either cleanup fence.
			_, err := database.GetAgentTurnExecutionContext(ctx, lease)
			return err
		}
	}
	states, err := docker.NewRetainedState(t.TempDir(), "single-connection-state", database)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BindExecutionBackend(ctx, states.Binding()); err != nil {
		t.Fatal(err)
	}
	launcher, err := agentturn.NewLauncher(agentturn.LauncherConfig{Store: database, Registry: registry, Workspace: workspaces, Gateway: singleConnectionGateway{}, Execution: factory, AgentAdapters: map[string]session.TurnAdapter{"pi-rpc/v2": adapter}, States: states, DeveloperCredentials: &integrationCredentialProvider{credential: "fixture-credential"}, ReviewerCredentials: &integrationCredentialProvider{credential: "fixture-credential"}, MemoryBytes: 512 << 20})
	if err != nil {
		t.Fatal(err)
	}
	launchCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	handle, err := launcher.Launch(launchCtx, agentturn.LaunchRequest{Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/owner/repo.git", DefaultBranchName: "main", DefaultBranchSHA: "default-sha", InitialFeatureBranch: "feature", RepositoryCredential: "fixture-credential", ProviderCredentialJSON: json.RawMessage(`{}`)})
	if preparationOnly {
		if !errors.Is(err, startupErr) || factory.engine.removed == loseProofFence || captures != 0 {
			t.Fatalf("preparation-only launch = (%v, %v), removed %t, captures %d", handle, err, factory.engine.removed, captures)
		}
		if loseProofFence && (handle == nil || !errors.Is(err, store.ErrAgentTurnFenceLost)) || !loseProofFence && handle != nil {
			t.Fatalf("startup cleanup handle/fence = (%v, %v)", handle, err)
		}
		var proof bool
		var resource string
		if err := pool.QueryRow(ctx, `SELECT preparation_only_cleanup, resource_ref FROM execution_reservations
WHERE agent_turn_id = $1 AND execution_epoch = $2`, lease.ID, lease.ExecutionEpoch).Scan(&proof, &resource); err != nil || proof == loseProofFence || resource != "fixture-main" {
			t.Fatalf("durable startup proof/binding = (%t, %q, %v)", proof, resource, err)
		}
		if !loseProofFence {
			if _, err := pool.Exec(ctx, `UPDATE execution_reservations SET preparation_only_cleanup = false
WHERE agent_turn_id = $1 AND execution_epoch = $2`, lease.ID, lease.ExecutionEpoch); err == nil {
				t.Fatal("durable cleanup proof was mutable")
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
		}
		// No settlement happened. Restart after cleanup lost its lease or response
		// must resolve the retained proof instead of requiring a removed source.
		database.Close()
		database, err = openTimedStore(t, ctx, postgres.databaseURL(false)+"&pool_max_conns=1", password, builtinStoreConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
			t.Fatal(err)
		}
		job, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "startup-recovery", time.Minute)
		if err != nil || job == nil {
			t.Fatalf("claim startup recovery = (%v, %v)", job, err)
		}
		if required, err := database.AgentTurnTerminalCaptureRequired(ctx, *job); err != nil || required != loseProofFence {
			t.Fatalf("restarted startup capture requirement = (%t, %v), want %t", required, err, loseProofFence)
		}
		return
	}
	if capture {
		if err != nil || handle == nil {
			t.Fatalf("single-connection launch = (%v, %v)", handle, err)
		}
		if err := handle.Cleanup(launchCtx); err != nil || captures != 1 {
			t.Fatalf("single-connection capture cleanup = %v, captures %d", err, captures)
		}
		return
	}
	// A deliberately unavailable Session preparation ends the test after creation/start,
	// avoiding a model prompt while exercising the actual Launcher/Store boundary.
	if !errors.Is(err, adapter.prepareErr) || errors.Is(err, context.DeadlineExceeded) || !factory.engine.started {
		t.Fatalf("single-connection launch = %v, started %t", err, factory.engine.started)
	}
}

type singleConnectionFactory struct {
	engine            singleConnectionEngine
	observer          *pgxpool.Pool
	whileProvisioning func(context.Context) error
}

func (*singleConnectionFactory) CheckRequirements([]execution.Requirement) error { return nil }

func (factory *singleConnectionFactory) New(request execution.Request, ledger execution.Ledger) (execution.Engine, error) {
	factory.engine.ledger, factory.engine.key = ledger, request.Key
	factory.engine.observer = factory.observer
	factory.engine.whileProvisioning = factory.whileProvisioning
	return &factory.engine, nil
}

type singleConnectionEngine struct {
	ledger            execution.Ledger
	key               execution.Key
	started           bool
	observer          *pgxpool.Pool
	whileProvisioning func(context.Context) error
	startErr          error
	observe           func(context.Context) error
	beforeRemove      func(context.Context) error
	removed           bool
}

func (engine *singleConnectionEngine) Create(ctx context.Context, request execution.Request, _ io.Writer) (execution.Process, error) {
	if _, _, err := engine.ledger.ReserveExecution(ctx, request.Key, strings.Repeat("a", 64)); err != nil {
		return nil, err
	}
	if err := engine.ledger.BeginExecutionPhase(ctx, request.Key, execution.Main); err != nil {
		return nil, err
	}
	var phase string
	if err := engine.observer.QueryRow(ctx, `SELECT dispatch_phase FROM execution_reservations WHERE agent_turn_id = $1 AND execution_epoch = $2`, request.Key.AgentTurnID, request.Key.ExecutionEpoch).Scan(&phase); err != nil {
		return nil, err
	}
	if phase != string(execution.Main) {
		return nil, errors.New("dispatch intent was not committed before external creation")
	}
	if err := engine.whileProvisioning(ctx); err != nil {
		return nil, err
	}
	if err := engine.ledger.BindExecution(ctx, request.Key, "fixture-main"); err != nil {
		return nil, err
	}
	return engine, nil
}
func (engine *singleConnectionEngine) Start(context.Context) error {
	engine.started = true
	return engine.startErr
}
func (*singleConnectionEngine) Transport() io.ReadWriteCloser { return singleConnectionTransport{} }
func (engine *singleConnectionEngine) ObserveExit(ctx context.Context) (execution.ExitObservation, error) {
	if engine.observe != nil {
		if err := engine.observe(ctx); err != nil {
			return execution.ExitObservation{}, err
		}
		return execution.ExitObservation{Resource: "fixture-main", Exited: true, ExitCode: 1}, nil
	}
	return execution.ExitObservation{}, nil
}
func (*singleConnectionEngine) Stop(context.Context, time.Duration) error { return nil }
func (engine *singleConnectionEngine) Remove(ctx context.Context) error {
	if engine.beforeRemove != nil {
		if err := engine.beforeRemove(ctx); err != nil {
			return err
		}
	}
	engine.removed = true
	return nil
}
func (engine *singleConnectionEngine) EnsureAbsent(ctx context.Context) error {
	_, _, err := engine.ledger.RevokeExecution(ctx, engine.key)
	return err
}
func (*singleConnectionEngine) Close() error { return nil }

type singleConnectionTransport struct{}

func (singleConnectionTransport) Read([]byte) (int, error)       { return 0, io.EOF }
func (singleConnectionTransport) Write(data []byte) (int, error) { return len(data), nil }
func (singleConnectionTransport) Close() error                   { return nil }

type singleConnectionWorkspace struct {
	capture func(context.Context) error
}

func (singleConnectionWorkspace) Prepare(_ context.Context, request workspace.Preparation) (execution.WorkspaceRef, error) {
	return execution.WorkspaceRef{Key: request.Key, Seed: request.Checkout.Revision}, nil
}
func (workspaces singleConnectionWorkspace) Capture(ctx context.Context, request execution.CaptureRequest) (execution.CaptureRef, error) {
	if workspaces.capture != nil {
		if err := workspaces.capture(ctx); err != nil {
			return execution.CaptureRef{}, err
		}
	}
	return execution.CaptureRef{Request: request, SHA256: strings.Repeat("a", 64)}, nil
}
func (singleConnectionWorkspace) PrepareTurnPaths(context.Context, string, string, turnconfig.Configuration, workspace.WorkspaceFence) (map[string]string, error) {
	return nil, nil
}
func (singleConnectionWorkspace) CleanupTurnPaths(context.Context, string, string, workspace.WorkspaceFence) error {
	return nil
}
func (singleConnectionWorkspace) Discard(ctx context.Context, _ execution.WorkspaceRef, fence workspace.WorkspaceFence) error {
	return fence(ctx, func(context.Context) error { return nil })
}

type singleConnectionGateway struct{}

func (singleConnectionGateway) Register(mcp.TokenScope) (mcp.Registration, error) {
	return mcp.Registration{}, nil
}
func (singleConnectionGateway) Renew(mcp.Registration, time.Time) bool                { return true }
func (singleConnectionGateway) CloseAndDrain(context.Context, mcp.Registration) error { return nil }
func (singleConnectionGateway) Revoke(mcp.Registration) bool                          { return true }

type singleConnectionAdapter struct {
	prepareErr error
}

func (singleConnectionAdapter) BuildProcess(p profile.Profile, key execution.Key, workflowID string, memory int64, tools map[string]string) (execution.Request, error) {
	return (pi.TurnAdapter{}).BuildProcess(p, key, workflowID, memory, tools)
}

func (adapter singleConnectionAdapter) Prepare(context.Context, session.TurnPreparation) (session.TurnConnection, error) {
	if adapter.prepareErr != nil {
		return nil, adapter.prepareErr
	}
	return singleConnectionTurn{}, nil
}

type singleConnectionTurn struct{}

func (singleConnectionTurn) Prompt(context.Context, store.AgentTurnLease, store.AgentSession, string, agentevent.Sink) (agent.Outcome, error) {
	return agent.Outcome{Status: agent.Rejected}, errors.New("unexpected fixture prompt")
}
func (singleConnectionTurn) Close() error { return nil }
