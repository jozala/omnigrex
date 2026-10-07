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
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/turnconfig"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestExecutionLauncherUsesSingleConnectionPool(t *testing.T) {
	postgres := startPostgres(t)
	password := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(password, []byte(postgresPassword), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, err := store.Open(ctx, postgres.databaseURL(false)+"&pool_max_conns=1", password, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	pool := openPool(t, postgres.databaseURL(true))
	runtimeProfile, err := profile.NewOpenCodeV1("registry.example/omnigrex/opencode@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := seedAgentSessionWithRuntimeBindings(t, pool, 992, workflow.RoleDeveloper, runtimeProfile.Binding(), runtimeProfile.Binding())
	spec := fixture.turnSpec()
	spec.AgentProfileConfig = agentProfileConfig("developer", workflow.RoleDeveloper, "opencode-acp/v1", "provider/test", "", 10, "Test instructions.", nil)
	turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "runtime", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := profile.NewRegistry(runtimeProfile)
	if err != nil {
		t.Fatal(err)
	}
	factory := &singleConnectionFactory{observer: pool, whileProvisioning: func(ctx context.Context) error { return database.ValidateTurnFence(ctx, lease) }}
	launcher, err := agentturn.NewLauncher(agentturn.LauncherConfig{Store: database, Registry: registry, Workspace: singleConnectionWorkspace{}, Gateway: singleConnectionGateway{}, Execution: factory, ACP: singleConnectionACP{}, Sessions: singleConnectionSessions{}, MemoryBytes: 512 << 20})
	if err != nil {
		t.Fatal(err)
	}
	launchCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	_, err = launcher.Launch(launchCtx, agentturn.LaunchRequest{Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/owner/repo.git", DefaultBranchName: "main", DefaultBranchSHA: "default-sha", InitialFeatureBranch: "feature", RepositoryCredential: "fixture-credential", ProviderCredentialJSON: json.RawMessage(`{}`)})
	// A deliberately unavailable ACP client ends the test after creation/start,
	// avoiding a model prompt while exercising the actual Launcher/Store boundary.
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "attach ACP client") || !factory.engine.started {
		t.Fatalf("single-connection launch = %v, started %t", err, factory.engine.started)
	}
}

type singleConnectionFactory struct {
	engine            singleConnectionEngine
	observer          *pgxpool.Pool
	whileProvisioning func(context.Context) error
}

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
func (engine *singleConnectionEngine) Start(context.Context) error { engine.started = true; return nil }
func (*singleConnectionEngine) Transport() io.ReadWriteCloser      { return singleConnectionTransport{} }
func (*singleConnectionEngine) ObserveExit(context.Context) (execution.ExitObservation, error) {
	return execution.ExitObservation{}, nil
}
func (*singleConnectionEngine) Stop(context.Context, time.Duration) error { return nil }
func (*singleConnectionEngine) Remove(context.Context) error              { return nil }
func (engine *singleConnectionEngine) EnsureAbsent(ctx context.Context) error {
	_, _, err := engine.ledger.RevokeExecution(ctx, engine.key)
	return err
}
func (*singleConnectionEngine) Close() error { return nil }

type singleConnectionTransport struct{}

func (singleConnectionTransport) Read([]byte) (int, error)       { return 0, io.EOF }
func (singleConnectionTransport) Write(data []byte) (int, error) { return len(data), nil }
func (singleConnectionTransport) Close() error                   { return nil }

type singleConnectionWorkspace struct{}

func (singleConnectionWorkspace) PrepareWorkspace(context.Context, workspace.Checkout) (workspace.Paths, error) {
	return workspace.Paths{Workspace: "/fixture/workspace", Mise: "/fixture/mise"}, nil
}
func (singleConnectionWorkspace) ProvisionMise(_ context.Context, provision workspace.MiseProvision) (workspace.MiseActivation, error) {
	return workspace.MiseActivation{DataDir: "/fixture/mise", SourceRevision: provision.Revision}, nil
}
func (singleConnectionWorkspace) PrepareTurnPaths(context.Context, string, string, turnconfig.Configuration, workspace.WorkspaceFence) (map[string]string, error) {
	return nil, nil
}
func (singleConnectionWorkspace) CleanupTurnPaths(context.Context, string, string, workspace.WorkspaceFence) error {
	return nil
}
func (singleConnectionWorkspace) DiscardWorkspaceFenced(context.Context, string, int64, workspace.WorkspaceFence) error {
	return nil
}

type singleConnectionGateway struct{}

func (singleConnectionGateway) Register(mcp.TokenScope) (mcp.Registration, error) {
	return mcp.Registration{}, nil
}
func (singleConnectionGateway) Renew(mcp.Registration, time.Time) bool                { return true }
func (singleConnectionGateway) CloseAndDrain(context.Context, mcp.Registration) error { return nil }
func (singleConnectionGateway) Revoke(mcp.Registration) bool                          { return true }

type singleConnectionACP struct{}

func (singleConnectionACP) New(io.ReadWriteCloser, acp.ClientOptions) agentturn.RuntimeACPClient {
	return nil
}

type singleConnectionSessions struct{}

func (singleConnectionSessions) Prepare(context.Context, session.PrepareRequest) (session.Result, error) {
	return session.Result{}, nil
}
