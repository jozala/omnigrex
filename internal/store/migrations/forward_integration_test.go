//go:build integration

package migrations

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestForwardMigrationsPreserveCommittedPrefixAndRetryOnlyPendingSuffix(t *testing.T) {
	pool, _ := migrationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	initial := &fstest.MapFile{Data: []byte(`CREATE TABLE committed_prefix (value text); INSERT INTO committed_prefix VALUES ('once');`)}
	if err := run(ctx, pool, fstest.MapFS{"000001_initial.sql": initial}); err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{
		"000001_initial.sql": initial,
		"000002_upgrade.sql": {Data: []byte(`CREATE TABLE committed_upgrade (value text); INSERT INTO committed_upgrade VALUES ('once');`)},
		"000003_final.sql":   {Data: []byte(`CREATE TABLE final_stage (value text); INSERT INTO final_stage SELECT value FROM required_dependency;`)},
	}
	if err := run(ctx, pool, files); err == nil || !strings.Contains(err.Error(), "required_dependency") {
		t.Fatalf("late failure = %v", err)
	}
	var prefix, upgrade, history int
	var finalExists bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM committed_prefix), (SELECT count(*) FROM committed_upgrade), (SELECT count(*) FROM schema_migrations), to_regclass('public.final_stage') IS NOT NULL`).Scan(&prefix, &upgrade, &history, &finalExists); err != nil {
		t.Fatal(err)
	}
	if prefix != 1 || upgrade != 1 || history != 2 || finalExists {
		t.Fatalf("partial upgrade = prefix %d upgrade %d history %d final %t", prefix, upgrade, history, finalExists)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE required_dependency (value text); INSERT INTO required_dependency VALUES ('available');`); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, pool, files); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	var final string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM committed_prefix), (SELECT count(*) FROM committed_upgrade), (SELECT count(*) FROM schema_migrations), (SELECT value FROM final_stage)`).Scan(&prefix, &upgrade, &history, &final); err != nil {
		t.Fatal(err)
	}
	if prefix != 1 || upgrade != 1 || history != 3 || final != "available" {
		t.Fatalf("retry repeated committed work: %d %d %d %q", prefix, upgrade, history, final)
	}
}

func TestForwardMigrationCancellationDuringSQLRollsBackAndUnlocks(t *testing.T) {
	pool, databaseURL := migrationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	initial := &fstest.MapFile{Data: []byte(`CREATE TABLE stable_prefix (value text); INSERT INTO stable_prefix VALUES ('preserve');`)}
	if err := run(ctx, pool, fstest.MapFS{"000001_initial.sql": initial}); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	const executionGate = 24681357
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_lock($1)`, executionGate); err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{
		"000001_initial.sql": initial,
		"000002_pending.sql": {Data: []byte(fmt.Sprintf(`CREATE TABLE cancelled_suffix (value text); SELECT pg_advisory_xact_lock(%d); INSERT INTO cancelled_suffix VALUES ('finished');`, executionGate))},
	}
	interrupted, stop := context.WithCancel(ctx)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- run(interrupted, pool, files) }()
	for {
		var executing bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND objid = $1::oid AND NOT granted)`, executionGate).Scan(&executing); err != nil {
			t.Fatal(err)
		}
		if executing {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("runner exited before entering pending SQL: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("SQL cancellation succeeded unexpectedly")
		}
	case <-ctx.Done():
		t.Fatal("cancelled runner did not return")
	}
	var prefix, history int
	var suffixExists bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM stable_prefix), (SELECT count(*) FROM schema_migrations), to_regclass('public.cancelled_suffix') IS NOT NULL`).Scan(&prefix, &history, &suffixExists); err != nil {
		t.Fatal(err)
	}
	if prefix != 1 || history != 1 || suffixExists {
		t.Fatalf("cancellation did not roll back pending SQL: %d %d %t", prefix, history, suffixExists)
	}
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_unlock($1)`, executionGate); err != nil {
		t.Fatal(err)
	}
	fresh, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	retry, stopRetry := context.WithTimeout(ctx, 5*time.Second)
	defer stopRetry()
	if err := run(retry, fresh, files); err != nil {
		t.Fatalf("fresh connection could not resume after SQL cancellation: %v", err)
	}
	if err := fresh.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&history); err != nil || history != 2 {
		t.Fatalf("resumed migration history = %d, %v", history, err)
	}
}

func TestForwardMigrationsRejectNonPrefixHistoryBeforePendingSQL(t *testing.T) {
	pool, _ := migrationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	second := &fstest.MapFile{Data: []byte(`CREATE TABLE recorded_second (value text); INSERT INTO recorded_second VALUES ('preserve');`)}
	if err := run(ctx, pool, fstest.MapFS{"000002_second.sql": second}); err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{
		"000001_initial.sql": {Data: []byte(`CREATE TABLE forbidden_initial (value text);`)},
		"000002_second.sql":  second,
		"000003_pending.sql": {Data: []byte(`CREATE TABLE forbidden_pending (value text);`)},
	}
	if err := run(ctx, pool, files); err == nil {
		t.Fatal("non-prefix history was accepted")
	}
	var preserved string
	var mutated bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT value FROM recorded_second), to_regclass('public.forbidden_initial') IS NOT NULL OR to_regclass('public.forbidden_pending') IS NOT NULL`).Scan(&preserved, &mutated); err != nil {
		t.Fatal(err)
	}
	if preserved != "preserve" || mutated {
		t.Fatal("non-prefix rejection changed application schema/data")
	}
}

func migrationPostgres(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	const image = "postgres:18-alpine@sha256:b40d931bd0e7ce6eecc59a5a6ac3b3c04a01e559750e73e7086b6dbd7f8bf545"
	name := fmt.Sprintf("omnigrex-migration-engine-test-%d", time.Now().UnixNano())
	args := []string{"run", "--detach", "--rm", "--name", name, "--user", "70:70", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/var/lib/postgresql:rw,noexec,nosuid,nodev,size=256m,uid=70,gid=70,mode=0700", "--tmpfs", "/var/run/postgresql:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0750", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0700",
		"--env", "POSTGRES_USER=omnigrex", "--env", "POSTGRES_DB=omnigrex", "--env", "POSTGRES_PASSWORD=migration-fixture-only", "--publish", "127.0.0.1::5432", image}
	if output, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start migration database: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("docker", "rm", "--force", "--volumes", name).CombinedOutput(); err != nil {
			t.Errorf("remove migration database: %v\n%s", err, output)
		}
	})
	output, err := exec.CommandContext(t.Context(), "docker", "port", name, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("database port: %v\n%s", err, output)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatal(err)
	}
	databaseURL := (&url.URL{Scheme: "postgres", User: url.UserPassword("omnigrex", "migration-fixture-only"), Host: net.JoinHostPort("127.0.0.1", port), Path: "/omnigrex", RawQuery: "sslmode=disable"}).String()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for {
		if err := pool.Ping(ctx); err == nil {
			return pool, databaseURL
		}
		select {
		case <-ctx.Done():
			t.Fatal("migration database did not become ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
