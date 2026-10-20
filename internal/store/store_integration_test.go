//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	postgresImage    = "postgres:18-alpine@sha256:b40d931bd0e7ce6eecc59a5a6ac3b3c04a01e559750e73e7086b6dbd7f8bf545"
	postgresUser     = "omnigrex"
	postgresPassword = "integration-secret"
	postgresDatabase = "omnigrex"
)

func builtinStoreConfig(t *testing.T) store.Config {
	t.Helper()
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	reducer, err := workflow.NewReducer(definition, workflow.BuiltinInfrastructureRetryLimit)
	if err != nil {
		t.Fatal(err)
	}
	return store.Config{Reducer: reducer, Policies: role.BuiltinPolicyCatalog()}
}

func storeConfigForReducer(t *testing.T, reducer workflow.Reducer) store.Config {
	t.Helper()
	policies, err := role.NewBuiltinPolicyCatalog(reducer.Definition().Roles())
	if err != nil {
		t.Fatal(err)
	}
	return store.Config{Reducer: reducer, Policies: policies}
}

func TestRunExecutesMigrationsExactlyOnceUnderConcurrentCalls(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	const callers = 8
	start := make(chan struct{})
	errors := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			errors <- migrations.Run(ctx, pool)
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if want := bundledMigrationCount(t); count != want {
		t.Fatalf("migration count = %d, want %d", count, want)
	}
	contents, err := migrations.Files.ReadFile("000001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(contents)
	var checksum []byte
	if err := pool.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = 1`).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if string(checksum) != string(want[:]) {
		t.Fatalf("migration checksum = %x, want %x", checksum, want)
	}
	for _, table := range []string{"webhook_deliveries", "workflows", "workflow_attempts", "agent_participants", "stage_assignments", "agent_sessions", "agent_turns", "change_proposals", "tool_invocations", "tool_invocation_replays", "jobs", "job_attempts", "agent_turn_slots", "job_normalized_events", "workflow_closure_barriers", "agent_turn_settlements", "workflow_github_effect_cleanups", "workflow_action_failures", "workflow_internal_events", "assignment_retention_generations", "assignment_retention_targets", "pi_runtime_profile_compatibility_results", "runtime_profile_contracts"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("table %q is missing", table)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum = decode(repeat('00', 32), 'hex')`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, pool); err == nil || !strings.Contains(err.Error(), "differs from recorded history") {
		t.Fatalf("changed history accepted: %v", err)
	}
}

func TestOpenUsesPasswordSecretAndReturnsReadyStore(t *testing.T) {
	postgres := startPostgres(t)
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	database, err := openTimedStore(t, ctx, postgres.databaseURL(false), passwordFile, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	pool := openPool(t, postgres.databaseURL(true))
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if want := bundledMigrationCount(t); count != want {
		t.Fatalf("migration count = %d, want %d", count, want)
	}
	database.Close()
	if err := database.Ready(ctx); err == nil {
		t.Fatal("closed database reported ready")
	}
}

func bundledMigrationCount(t *testing.T) int {
	t.Helper()
	files, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

func TestOpenReadOnlyDoesNotApplyMigrations(t *testing.T) {
	postgres := startPostgres(t)
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	database, err := store.OpenReadOnly(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CheckMigrations(ctx); err == nil {
		t.Fatal("empty database reported migrated")
	}
	pool := openPool(t, postgres.databaseURL(true))
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("read-only open created migration history")
	}
}

func TestOpenReadOnlyChecksCurrentMigrationHistory(t *testing.T) {
	postgres := startPostgres(t)
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	writable, err := openTimedStore(t, ctx, postgres.databaseURL(false), passwordFile, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	writable.Close()
	database, err := store.OpenReadOnly(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CheckMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	pool := openPool(t, postgres.databaseURL(true))
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET name = 'changed' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckMigrations(ctx); err == nil {
		t.Fatal("changed migration history accepted")
	}
}

type postgresContainer struct{ hostPort, fixture string }

func (postgres postgresContainer) databaseURL(withPassword bool) string {
	user := url.User(postgresUser)
	if withPassword {
		user = url.UserPassword(postgresUser, postgresPassword)
	}
	return (&url.URL{Scheme: "postgres", User: user, Host: net.JoinHostPort("127.0.0.1", postgres.hostPort), Path: "/" + postgresDatabase, RawQuery: "sslmode=disable"}).String()
}

func startPostgres(t *testing.T) postgresContainer {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("omnigrex-postgres-test-%d", time.Now().UnixNano())
	fixture := fmt.Sprint(fixtureTimingSequence.Add(1))
	args := []string{"run", "--detach", "--rm", "--name", name, "--user", "70:70", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/var/lib/postgresql:rw,noexec,nosuid,nodev,size=256m,uid=70,gid=70,mode=0700",
		"--tmpfs", "/var/run/postgresql:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0750",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0700",
		"--env", "POSTGRES_USER=" + postgresUser, "--env", "POSTGRES_PASSWORD=" + postgresPassword, "--env", "POSTGRES_DB=" + postgresDatabase,
		"--publish", "127.0.0.1::5432", postgresImage}
	finishStart := beginFixturePhase(t, fixture, "postgres_start")
	output, err := exec.Command("docker", args...).CombinedOutput()
	finishStart(err)
	if err != nil {
		t.Fatalf("start PostgreSQL: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		finishRemove := beginFixturePhase(t, fixture, "postgres_remove")
		output, err := exec.Command("docker", "rm", "--force", name).CombinedOutput()
		if strings.Contains(string(output), "No such container") {
			err = nil
		}
		finishRemove(err)
		if err != nil {
			t.Errorf("remove PostgreSQL: %v\n%s", err, output)
		}
	})
	finishPort := beginFixturePhase(t, fixture, "postgres_port")
	output, err = exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	if err != nil {
		finishPort(err)
		t.Fatalf("PostgreSQL port: %v %s", err, output)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(output)))
	finishPort(err)
	if err != nil {
		t.Fatal(err)
	}
	postgres := postgresContainer{hostPort: port, fixture: fixture}
	host := net.JoinHostPort("127.0.0.1", port)
	fixtureTimingHosts.Store(host, fixture)
	t.Cleanup(func() { fixtureTimingHosts.Delete(host) })
	finishReady := beginFixturePhase(t, fixture, "postgres_ready")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		config, err := pgxpool.ParseConfig(postgres.databaseURL(true))
		if err != nil {
			cancel()
			finishReady(err)
			t.Fatal(err)
		}
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err == nil {
			err = pool.Ping(ctx)
			pool.Close()
		}
		cancel()
		if err == nil {
			finishReady(nil)
			return postgres
		}
		time.Sleep(100 * time.Millisecond)
	}
	finishReady(context.DeadlineExceeded)
	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	t.Fatalf("PostgreSQL did not become ready\n%s", logs)
	return postgresContainer{}
}

func openPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}
