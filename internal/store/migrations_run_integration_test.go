//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store/migrations"
)

func TestRunRejectsIncompatibleHistoryBeforeApplyingSchema(t *testing.T) {
	for _, recorded := range []string{"(1, 'previous_baseline', decode(repeat('ab', 32), 'hex'))", "(37, 'unknown_release', decode(repeat('ab', 32), 'hex'))"} {
		t.Run(recorded[:2], func(t *testing.T) {
			postgres := startPostgres(t)
			pool := openPool(t, postgres.databaseURL(true))
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (version bigint PRIMARY KEY, name text NOT NULL, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE existing_deployment_marker (value text); INSERT INTO existing_deployment_marker VALUES ('preserve');
INSERT INTO schema_migrations (version, name, checksum) VALUES `+recorded); err != nil {
				t.Fatal(err)
			}
			if err := migrations.Run(ctx, pool); err == nil {
				t.Fatal("incompatible deployment history was accepted")
			}
			var marker string
			var created bool
			if err := pool.QueryRow(ctx, `SELECT (SELECT value FROM existing_deployment_marker), to_regclass('public.agent_sessions') IS NOT NULL`).Scan(&marker, &created); err != nil {
				t.Fatal(err)
			}
			if marker != "preserve" || created {
				t.Fatal("rejected history changed application state")
			}
		})
	}
}

func TestRunRollsBackFailedBaselineAndRetries(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_baseline_test() RETURNS event_trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_event_trigger_ddl_commands() WHERE object_identity = 'public.workspace_captures') THEN
        RAISE EXCEPTION 'controlled baseline failure';
    END IF;
END;
$$;
CREATE EVENT TRIGGER baseline_test_failure ON ddl_command_end WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION fail_baseline_test();`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, pool); err == nil || !strings.Contains(err.Error(), "controlled baseline failure") {
		t.Fatalf("baseline failure = %v", err)
	}
	var tables, history int
	if err := pool.QueryRow(ctx, `SELECT
    (SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('agent_sessions', 'workflows', 'workspace_captures')),
    (SELECT count(*) FROM schema_migrations)`).Scan(&tables, &history); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || history != 0 {
		t.Fatalf("failed baseline left tables=%d history=%d", tables, history)
	}
	if _, err := pool.Exec(ctx, `DROP EVENT TRIGGER baseline_test_failure; DROP FUNCTION fail_baseline_test()`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if err := migrations.Check(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func TestRunCancellationReleasesMigrationLock(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const lockID = int64(0x4f4d4e4947524558)
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		blocker.Release()
		t.Fatal(err)
	}
	bounded, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	err = migrations.Run(bounded, pool)
	stop()
	if err == nil {
		t.Fatal("migration lock wait ignored cancellation")
	}
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockID); err != nil {
		blocker.Release()
		t.Fatal(err)
	}
	blocker.Release()
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatalf("migration lock was not released: %v", err)
	}
}
