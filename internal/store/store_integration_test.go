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
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
)

const (
	postgresImage    = "postgres:18-alpine@sha256:b40d931bd0e7ce6eecc59a5a6ac3b3c04a01e559750e73e7086b6dbd7f8bf545"
	postgresUser     = "omnigrex"
	postgresPassword = "integration-secret"
	postgresDatabase = "omnigrex"
)

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
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			errors <- migrations.Run(ctx, pool)
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	var checksum []byte
	err := pool.QueryRow(ctx, `SELECT count(*), min(checksum) FROM schema_migrations`).Scan(&count, &checksum)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", count)
	}
	bootstrap, err := migrations.Files.ReadFile("000001_bootstrap.sql")
	if err != nil {
		t.Fatalf("read embedded bootstrap migration: %v", err)
	}
	wantChecksum := sha256.Sum256(bootstrap)
	if string(checksum) != string(wantChecksum[:]) {
		t.Errorf("stored checksum = %x, want SHA-256 %x", checksum, wantChecksum)
	}

	for _, table := range []string{
		"webhook_deliveries",
		"workflows",
		"workflow_attempts",
		"agent_assignments",
		"agent_sessions",
		"agent_turns",
		"change_proposals",
		"tool_invocations",
		"jobs",
	} {
		var exists bool
		err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("look up table %q: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q does not exist", table)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum = decode(repeat('00', 32), 'hex')`); err != nil {
		t.Fatalf("tamper with migration checksum: %v", err)
	}
	if err := migrations.Run(ctx, pool); err == nil || !strings.Contains(err.Error(), "differs from recorded history") {
		t.Errorf("Run() with changed migration history error = %v, want checksum mismatch error", err)
	}
}

func TestOpenUsesPasswordSecretAndReturnsReadyStore(t *testing.T) {
	postgres := startPostgres(t)
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword+"\n"), 0o600); err != nil {
		t.Fatalf("write password secret: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := database.Ready(ctx); err != nil {
		t.Errorf("Ready() error = %v", err)
	}
	pool := openPool(t, postgres.databaseURL(true))
	var migrationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("query migrations applied by Open(): %v", err)
	}
	if migrationCount != 1 {
		t.Errorf("migrations applied by Open() = %d, want 1", migrationCount)
	}
	database.Close()
	if err := database.Ready(ctx); err == nil {
		t.Error("Ready() after Close() error = nil, want closed-pool error")
	}
}

type postgresContainer struct {
	hostPort string
}

func (postgres postgresContainer) databaseURL(withPassword bool) string {
	user := url.User(postgresUser)
	if withPassword {
		user = url.UserPassword(postgresUser, postgresPassword)
	}
	return (&url.URL{
		Scheme:   "postgres",
		User:     user,
		Host:     net.JoinHostPort("127.0.0.1", postgres.hostPort),
		Path:     "/" + postgresDatabase,
		RawQuery: "sslmode=disable",
	}).String()
}

func startPostgres(t *testing.T) postgresContainer {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("find Docker CLI: %v", err)
	}

	name := fmt.Sprintf("omnigrex-postgres-test-%d", time.Now().UnixNano())
	args := []string{
		"run", "--detach", "--rm",
		"--name", name,
		"--user", "70:70",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/var/lib/postgresql:rw,noexec,nosuid,nodev,size=256m,uid=70,gid=70,mode=0700",
		"--tmpfs", "/var/run/postgresql:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0750",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0700",
		"--env", "POSTGRES_USER=" + postgresUser,
		"--env", "POSTGRES_PASSWORD=" + postgresPassword,
		"--env", "POSTGRES_DB=" + postgresDatabase,
		"--publish", "127.0.0.1::5432",
		postgresImage,
	}
	if output, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start %s: %v\n%s", postgresImage, err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("docker", "rm", "--force", name).CombinedOutput(); err != nil && !strings.Contains(string(output), "No such container") {
			t.Errorf("remove PostgreSQL container: %v\n%s", err, output)
		}
	})

	portOutput, err := exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("get PostgreSQL port: %v\n%s", err, portOutput)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(portOutput)))
	if err != nil {
		t.Fatalf("parse PostgreSQL port %q: %v", strings.TrimSpace(string(portOutput)), err)
	}
	postgres := postgresContainer{hostPort: port}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		config, configErr := pgxpool.ParseConfig(postgres.databaseURL(true))
		if configErr != nil {
			cancel()
			t.Fatalf("parse test database URL: %v", configErr)
		}
		pool, poolErr := pgxpool.NewWithConfig(ctx, config)
		if poolErr == nil {
			poolErr = pool.Ping(ctx)
			pool.Close()
		}
		cancel()
		if poolErr == nil {
			return postgres
		}
		time.Sleep(100 * time.Millisecond)
	}

	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	t.Fatalf("PostgreSQL did not become ready\n%s", logs)
	return postgresContainer{}
}

func openPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	return pool
}
