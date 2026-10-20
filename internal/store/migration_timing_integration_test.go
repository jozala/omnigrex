//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store/migrations"
)

func TestMigrationTimingFreshAndExisting(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	for _, phase := range []string{"probe_migrations_fresh", "probe_migrations_existing"} {
		if !t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			finish := beginFixturePhase(t, postgres.fixture, phase)
			err := migrations.Run(ctx, pool)
			finish(err)
			if err != nil {
				t.Fatal(err)
			}
			if err := migrations.Check(ctx, pool); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if want := bundledMigrationCount(t); count != want {
				t.Fatalf("migration history = %d rows, want %d", count, want)
			}
		}) {
			return
		}
	}
}
