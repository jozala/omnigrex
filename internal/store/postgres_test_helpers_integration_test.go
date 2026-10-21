//go:build integration

package store_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresFixture struct{ hostPort, database, fixture string }

func (postgres postgresFixture) databaseURL(withPassword bool) string {
	user := url.User(postgresUser)
	if withPassword {
		user = url.UserPassword(postgresUser, postgresPassword)
	}
	return (&url.URL{Scheme: "postgres", User: user, Host: net.JoinHostPort("127.0.0.1", postgres.hostPort), Path: "/" + postgres.database, RawQuery: "sslmode=disable"}).String()
}

type postgresServer struct {
	postgresFixture
	name  string
	admin *pgxpool.Pool
}

var sharedPostgres struct {
	once   sync.Once
	server *postgresServer
	err    error
}

// Only integration binaries get this TestMain. Startup is lazy so selecting
// non-database tests or the externally provisioned Pi Workflow needs no Docker.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedPostgres.server != nil {
		if err := sharedPostgres.server.close(postgresSuiteLogger{}); err != nil {
			fmt.Fprintf(os.Stderr, "remove shared PostgreSQL fixture: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

type postgresSuiteLogger struct{}

func (postgresSuiteLogger) Helper() {}
func (postgresSuiteLogger) Logf(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

// startPostgres gives each caller an empty database, never a pre-migrated
// template or a transaction shared with another fixture. Only the process is
// reused, and each application's pools still belong to its own test.
func startPostgres(t *testing.T) postgresFixture {
	t.Helper()
	sharedPostgres.once.Do(func() {
		sharedPostgres.server, sharedPostgres.err = startPostgresServer(t)
	})
	if sharedPostgres.err != nil {
		t.Fatalf("start shared PostgreSQL: %v", sharedPostgres.err)
	}
	server := sharedPostgres.server
	fixture := postgresFixture{hostPort: server.hostPort, fixture: fmt.Sprint(fixtureTimingSequence.Add(1))}
	fixture.database = "fixture_" + fixture.fixture
	// Register before attempting CREATE: a lost/cancelled response must not
	// leave a database behind until suite teardown. Names are uniquely owned
	// by this process and this fixture, never derived from external databases.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		finish := beginFixturePhase(t, fixture.fixture, "postgres_database_drop")
		_, err := server.admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgx.Identifier{fixture.database}.Sanitize()+" WITH (FORCE)")
		finish(err)
		if err != nil {
			t.Errorf("remove isolated PostgreSQL database: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	finish := beginFixturePhase(t, fixture.fixture, "postgres_database_create")
	_, err := server.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{fixture.database}.Sanitize()+" TEMPLATE template0")
	finish(err)
	if err != nil {
		t.Fatalf("create isolated PostgreSQL database: %v", err)
	}
	registerPostgresTiming(t, fixture)
	return fixture
}

// Startup, password, and cold-schema qualification retain fresh processes.
func startIsolatedPostgres(t *testing.T) postgresFixture {
	t.Helper()
	server, err := startPostgresServer(t)
	if server != nil {
		t.Cleanup(func() {
			if err := server.close(t); err != nil {
				t.Errorf("remove isolated PostgreSQL server: %v", err)
			}
		})
	}
	if err != nil {
		t.Fatalf("start isolated PostgreSQL: %v", err)
	}
	registerPostgresTiming(t, server.postgresFixture)
	return server.postgresFixture
}

func registerPostgresTiming(t *testing.T, fixture postgresFixture) {
	t.Helper()
	key := net.JoinHostPort("127.0.0.1", fixture.hostPort) + "\x00/" + fixture.database
	fixtureTimingDatabases.Store(key, fixture.fixture)
	t.Cleanup(func() { fixtureTimingDatabases.Delete(key) })
}

func startPostgresServer(logger fixtureTimingLogger) (*postgresServer, error) {
	return startNamedPostgresServer(logger, postgresContainerName())
}

func postgresContainerName() string {
	return "omnigrex-postgres-test-" + uuid.NewString()
}

func startNamedPostgresServer(logger fixtureTimingLogger, name string) (*postgresServer, error) {
	logger.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, err
	}
	fixture := fmt.Sprint(fixtureTimingSequence.Add(1))
	server := &postgresServer{
		postgresFixture: postgresFixture{database: postgresDatabase, fixture: fixture},
		name:            name,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"run", "--detach", "--rm", "--name", server.name, "--user", "70:70", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/var/lib/postgresql:rw,noexec,nosuid,nodev,size=256m,uid=70,gid=70,mode=0700",
		"--tmpfs", "/var/run/postgresql:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0750",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0700",
		"--env", "POSTGRES_USER=" + postgresUser, "--env", "POSTGRES_PASSWORD=" + postgresPassword, "--env", "POSTGRES_DB=" + postgresDatabase,
		"--publish", "127.0.0.1::5432", postgresImage,
		// Reusing the process must not let WAL grow beyond the bounded fixture
		// disk. Keep fsync and synchronous commits enabled, rather than hiding
		// durability behavior to accelerate tests.
		"postgres", "-c", "max_wal_size=64MB", "-c", "min_wal_size=32MB", "-c", "checkpoint_timeout=30s"}
	finish := beginFixturePhase(logger, fixture, "postgres_start")
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	finish(err)
	if err != nil {
		return server, fmt.Errorf("start PostgreSQL: %w\n%s", err, output)
	}
	finish = beginFixturePhase(logger, fixture, "postgres_port")
	output, err = exec.CommandContext(ctx, "docker", "port", server.name, "5432/tcp").CombinedOutput()
	if err != nil {
		finish(err)
		return server, fmt.Errorf("PostgreSQL port: %w\n%s", err, output)
	}
	_, server.hostPort, err = net.SplitHostPort(strings.TrimSpace(string(output)))
	finish(err)
	if err != nil {
		return server, err
	}
	configuration, err := pgxpool.ParseConfig(server.databaseURL(true))
	if err != nil {
		return server, err
	}
	configuration.MaxConns = 1
	server.admin, err = pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return server, err
	}
	finish = beginFixturePhase(logger, fixture, "postgres_ready")
	ready, cancelReady := context.WithTimeout(ctx, 30*time.Second)
	defer cancelReady()
	for {
		ping, cancelPing := context.WithTimeout(ready, time.Second)
		err = server.admin.Ping(ping)
		cancelPing()
		if err == nil {
			finish(nil)
			return server, nil
		}
		select {
		case <-ready.Done():
			finish(ready.Err())
			return server, fmt.Errorf("PostgreSQL readiness: %w", ready.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (server *postgresServer) close(logger fixtureTimingLogger) error {
	logger.Helper()
	if server.admin != nil {
		server.admin.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	finish := beginFixturePhase(logger, server.fixture, "postgres_remove")
	output, err := exec.CommandContext(ctx, "docker", "rm", "--force", server.name).CombinedOutput()
	if strings.Contains(strings.ToLower(string(output)), "no such container") {
		err = nil
	}
	finish(err)
	if err != nil {
		return fmt.Errorf("remove PostgreSQL: %w\n%s", err, output)
	}
	return nil
}
