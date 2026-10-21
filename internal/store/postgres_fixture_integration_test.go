//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresFixturesShareServerButNotSchemaOrRows(t *testing.T) {
	first := startPostgres(t)
	second := startPostgres(t)
	if first.hostPort != second.hostPort {
		t.Fatal("ordinary database fixtures started separate PostgreSQL servers")
	}
	if first.database == second.database {
		t.Fatal("ordinary database fixtures reused an application database")
	}
	firstPool := openPool(t, first.databaseURL(true))
	secondPool := openPool(t, second.databaseURL(true))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, pool := range []*pgxpool.Pool{firstPool, secondPool} {
		var migrated bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations') IS NOT NULL").Scan(&migrated); err != nil {
			t.Fatal(err)
		}
		if migrated {
			t.Fatal("a fresh fixture was pre-migrated")
		}
		var durable bool
		if err := pool.QueryRow(ctx, "SELECT current_setting('fsync') = 'on' AND current_setting('synchronous_commit') = 'on' AND current_setting('full_page_writes') = 'on'").Scan(&durable); err != nil || !durable {
			t.Fatalf("fixture durability was disabled: %t %v", durable, err)
		}
	}
	if _, err := firstPool.Exec(ctx, "CREATE TABLE fixture_marker (id integer PRIMARY KEY, value text); INSERT INTO fixture_marker VALUES (1, 'first')"); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := secondPool.QueryRow(ctx, "SELECT to_regclass('public.fixture_marker') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("another fixture's schema leaked: %t %v", exists, err)
	}
	if _, err := secondPool.Exec(ctx, "CREATE TABLE fixture_marker (id integer PRIMARY KEY, value text); INSERT INTO fixture_marker VALUES (1, 'second')"); err != nil {
		t.Fatal(err)
	}
	for index, pool := range []*pgxpool.Pool{firstPool, secondPool} {
		var value string
		if err := pool.QueryRow(ctx, "SELECT value FROM fixture_marker WHERE id = 1").Scan(&value); err != nil || value != []string{"first", "second"}[index] {
			t.Fatalf("fixture rows are not isolated: %q %v", value, err)
		}
	}
}

func TestPostgresFixtureCleanupDropsOnlyItsDatabaseDespiteLeakedConnections(t *testing.T) {
	survivor := startPostgres(t)
	survivorPool := openPool(t, survivor.databaseURL(true))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := survivorPool.Exec(ctx, "CREATE TABLE survivor_marker (value text); INSERT INTO survivor_marker VALUES ('preserved')"); err != nil {
		t.Fatal(err)
	}
	parent := t
	var removed postgresFixture
	t.Run("temporary", func(t *testing.T) {
		removed = startPostgres(t)
		pool, err := pgxpool.New(ctx, removed.databaseURL(true))
		if err != nil {
			t.Fatal(err)
		}
		// Keep the client pool open until the parent finishes. Database cleanup
		// must terminate this fixture's connections without touching siblings.
		parent.Cleanup(pool.Close)
		if err := pool.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	})
	admin := sharedPostgres.server.admin
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", removed.database).Scan(&exists); err != nil || exists {
		t.Fatalf("temporary fixture database survived cleanup: %t %v", exists, err)
	}
	var value string
	if err := survivorPool.QueryRow(ctx, "SELECT value FROM survivor_marker").Scan(&value); err != nil || value != "preserved" {
		t.Fatalf("sibling database was damaged: %q %v", value, err)
	}
}

func TestPostgresFixturesAllocateIsolatedDatabasesConcurrently(t *testing.T) {
	for index := range 2 {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			t.Parallel()
			fixture := startPostgres(t)
			pool := openPool(t, fixture.databaseURL(true))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := pool.Exec(ctx, "CREATE TABLE concurrent_marker (id integer PRIMARY KEY); INSERT INTO concurrent_marker VALUES (1)"); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM concurrent_marker").Scan(&count); err != nil || count != 1 {
				t.Fatalf("concurrent fixture leaked rows: %d %v", count, err)
			}
		})
	}
}

func TestPostgresFixtureFailureStillRemovesTheSharedServer(t *testing.T) {
	name := postgresContainerName()
	// Establish exact parent ownership before the child starts. Even a child
	// killed during initialization cannot hide a container behind a late receipt.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		output, err := exec.CommandContext(cleanup, "docker", "rm", "--force", name).CombinedOutput()
		if err != nil && !strings.Contains(strings.ToLower(string(output)), "no such container") {
			t.Errorf("remove owned failure-helper container: %v\n%s", err, output)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPostgresFixtureFailureHelper$", "-test.v")
	command.Env = append(os.Environ(), "OMNIGREX_POSTGRES_FAILURE_HELPER="+name)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "controlled fixture failure") {
		t.Fatalf("helper did not exercise a normal test failure: %v\n%s", err, output)
	}
	output, err = exec.CommandContext(ctx, "docker", "inspect", name).CombinedOutput()
	if err == nil || !strings.Contains(strings.ToLower(string(output)), "no such object") {
		t.Fatalf("failed test left its PostgreSQL process behind: %v\n%s", err, output)
	}
}

func TestPostgresFixtureFailureHelper(t *testing.T) {
	name := os.Getenv("OMNIGREX_POSTGRES_FAILURE_HELPER")
	if name == "" {
		t.Skip("subprocess helper")
	}
	sharedPostgres.once.Do(func() {
		sharedPostgres.server, sharedPostgres.err = startNamedPostgresServer(t, name)
	})
	startPostgres(t)
	t.Fatal("controlled fixture failure")
}
