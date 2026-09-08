//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store/migrations"
)

const migrationHistoryDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (name <> ''),
    checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`

var migrationFilesThrough13 = []string{
	"000001_bootstrap.sql",
	"000002_normalized_events.sql",
	"000003_durable_jobs_and_turn_fencing.sql",
	"000004_phase_five_acknowledgement_barriers.sql",
	"000005_single_live_workflow_successor.sql",
	"000006_agent_turn_preparation.sql",
	"000007_scoped_mutation_operations.sql",
	"000008_agent_turn_settlements.sql",
	"000009_workflow_action_exhaustion.sql",
	"000010_workflow_action_failure_barriers.sql",
	"000011_durable_closure_retention.sql",
	"000012_runtime_profile_compatibility.sql",
	"000013_tool_scoped_mutation_operations.sql",
}

func TestRunCommitsEachMigrationAndResumesAfterLateFailure(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	applyRecordedMigrations(t, ctx, pool, migrationFilesThrough13...)
	if _, err := pool.Exec(ctx, `
INSERT INTO webhook_deliveries (
    delivery_id, event_name, repository_id, repository_owner, repository_name,
    payload, status, attempt_count
)
VALUES ('15000000-0000-4000-8000-000000000001', 'push', 1, 'owner', 'repo', '{}'::bytea, 'PENDING', 7);

CREATE FUNCTION reject_attempt_backfill() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'forced late migration failure';
END
$$;

CREATE TRIGGER reject_attempt_backfill
BEFORE UPDATE ON webhook_deliveries
FOR EACH ROW EXECUTE FUNCTION reject_attempt_backfill()`); err != nil {
		t.Fatalf("prepare late migration failure: %v", err)
	}

	err := migrations.Run(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "apply migration 000015_backfill_webhook_delivery_attempts.sql") {
		t.Fatalf("Run() error = %v, want migration 000015 failure", err)
	}

	var latestVersion, attemptLimit int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latestVersion); err != nil {
		t.Fatalf("read migration history after failure: %v", err)
	}
	if latestVersion != 14 {
		t.Errorf("latest migration after failure = %d, want committed migration 14", latestVersion)
	}
	if err := pool.QueryRow(ctx, `SELECT max_attempts FROM webhook_deliveries WHERE delivery_id = '15000000-0000-4000-8000-000000000001'`).Scan(&attemptLimit); err != nil {
		t.Fatalf("read delivery after failed backfill: %v", err)
	}
	if attemptLimit != 3 {
		t.Errorf("max_attempts after failed backfill = %d, want rolled-back value 3", attemptLimit)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_attempt_backfill ON webhook_deliveries; DROP FUNCTION reject_attempt_backfill()`); err != nil {
		t.Fatalf("remove forced migration failure: %v", err)
	}

	retryPool := openPool(t, postgres.databaseURL(true))
	if err := migrations.Run(ctx, retryPool); err != nil {
		t.Fatalf("Run() after repairing late failure: %v", err)
	}
	var migrationCount int
	if err := retryPool.QueryRow(ctx, `SELECT max(version), count(*) FROM schema_migrations`).Scan(&latestVersion, &migrationCount); err != nil {
		t.Fatalf("read migration history after retry: %v", err)
	}
	if latestVersion != 17 || migrationCount != 17 {
		t.Errorf("migration history after retry = max %d, count %d; want max 17, count 17", latestVersion, migrationCount)
	}
	if err := retryPool.QueryRow(ctx, `SELECT max_attempts FROM webhook_deliveries WHERE delivery_id = '15000000-0000-4000-8000-000000000001'`).Scan(&attemptLimit); err != nil {
		t.Fatalf("read delivery after retry: %v", err)
	}
	if attemptLimit != 8 {
		t.Errorf("max_attempts after retry = %d, want 8", attemptLimit)
	}
}

func TestRunCancellationRollsBackCurrentMigrationAndReleasesLock(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	applyRecordedMigrations(t, ctx, pool, migrationFilesThrough13...)
	if _, err := pool.Exec(ctx, `
INSERT INTO webhook_deliveries (
    delivery_id, event_name, repository_id, repository_owner, repository_name,
    payload, status, attempt_count
)
VALUES ('15000000-0000-4000-8000-000000000002', 'push', 1, 'owner', 'repo', '{}'::bytea, 'PENDING', 7);

CREATE FUNCTION pause_attempt_backfill() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_sleep(30);
    RETURN NEW;
END
$$;

CREATE TRIGGER pause_attempt_backfill
BEFORE UPDATE ON webhook_deliveries
FOR EACH ROW EXECUTE FUNCTION pause_attempt_backfill()`); err != nil {
		t.Fatalf("prepare cancellable migration: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- migrations.Run(runCtx, pool)
	}()
	waitForMigrationVersion(t, ctx, pool, 14)
	cancelRun()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("canceled Run() error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Run() did not return")
	}

	var latestVersion, attemptLimit int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latestVersion); err != nil {
		t.Fatalf("read migration history after cancellation: %v", err)
	}
	if latestVersion != 14 {
		t.Errorf("latest migration after cancellation = %d, want committed migration 14", latestVersion)
	}
	if err := pool.QueryRow(ctx, `SELECT max_attempts FROM webhook_deliveries WHERE delivery_id = '15000000-0000-4000-8000-000000000002'`).Scan(&attemptLimit); err != nil {
		t.Fatalf("read delivery after cancellation: %v", err)
	}
	if attemptLimit != 3 {
		t.Errorf("max_attempts after cancellation = %d, want rolled-back value 3", attemptLimit)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER pause_attempt_backfill ON webhook_deliveries; DROP FUNCTION pause_attempt_backfill()`); err != nil {
		t.Fatalf("remove migration pause: %v", err)
	}

	retryPool := openPool(t, postgres.databaseURL(true))
	if err := migrations.Run(ctx, retryPool); err != nil {
		t.Fatalf("Run() after cancellation: %v", err)
	}
}

func TestRunSerializesConcurrentCallsAndReleasesLock(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))

	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			results <- migrations.Run(ctx, pool)
		}()
	}
	close(start)
	for range callers {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Run() error = %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var migrationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("read migration history: %v", err)
	}
	if migrationCount != 17 {
		t.Errorf("migration history count = %d, want 17", migrationCount)
	}

	observerPool := openPool(t, postgres.databaseURL(true))
	if err := migrations.Run(ctx, observerPool); err != nil {
		t.Fatalf("Run() from fresh pool after concurrent calls: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum = decode(repeat('00', 32), 'hex') WHERE version = 17`); err != nil {
		t.Fatalf("tamper with migration history: %v", err)
	}
	if err := migrations.Run(ctx, observerPool); err == nil || !strings.Contains(err.Error(), "migration 000017_validate_normalized_event_failures.sql differs from recorded history") {
		t.Errorf("Run() with changed migration history error = %v, want checksum mismatch", err)
	}
}

func waitForMigrationVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			t.Fatalf("wait for migration %d: %v", version, err)
		}
		if exists {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for migration %d: %v", version, ctx.Err())
		case <-ticker.C:
		}
	}
}

func applyRecordedMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, filenames ...string) {
	t.Helper()
	if _, err := pool.Exec(ctx, migrationHistoryDDL); err != nil {
		t.Fatalf("create migration history: %v", err)
	}
	for version, filename := range filenames {
		contents, err := migrations.Files.ReadFile(filename)
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
		checksum := sha256.Sum256(contents)
		name := strings.TrimSuffix(strings.SplitN(filename, "_", 2)[1], ".sql")
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, version+1, name, checksum[:]); err != nil {
			t.Fatalf("record migration %s: %v", filename, err)
		}
	}
}
