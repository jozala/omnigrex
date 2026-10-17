//go:build integration

package store_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store/migrations"
)

func TestFreshBaselineContainsOnlyCurrentRuntimeStructures(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var identityFields, compatibilityTables []string
	if err := pool.QueryRow(ctx, `SELECT
    (SELECT array_agg(column_name ORDER BY column_name) FROM information_schema.columns
     WHERE table_schema = 'public' AND table_name = 'agent_sessions'
       AND (column_name LIKE '%session_id' OR column_name LIKE '%capabilities')),
    (SELECT array_agg(table_name ORDER BY table_name) FROM information_schema.tables
     WHERE table_schema = 'public' AND table_name LIKE '%runtime_profile_compatibility_results')`).Scan(&identityFields, &compatibilityTables); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(identityFields, []string{"runtime_capabilities", "runtime_session_id"}) || !slices.Equal(compatibilityTables, []string{"pi_runtime_profile_compatibility_results"}) {
		t.Fatalf("unsupported runtime schema: identity=%v compatibility=%v", identityFields, compatibilityTables)
	}
	var current bool
	if err := pool.QueryRow(ctx, `SELECT
    to_regclass('public.pi_runtime_profile_compatibility_results') IS NOT NULL
    AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
                AND table_name = 'agent_sessions' AND column_name = 'runtime_session_id')`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if !current {
		t.Fatal("baseline lacks current Session identity or Pi qualification storage")
	}
	var nullablePurpose bool
	if err := pool.QueryRow(ctx, `SELECT is_nullable = 'YES' FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'agent_turns' AND column_name = 'purpose'`).Scan(&nullablePurpose); err != nil {
		t.Fatal(err)
	}
	if nullablePurpose {
		t.Fatal("baseline permits an Agent Turn without its required purpose")
	}
}

func TestFreshBackendOwnershipHasOneImmutableWinner(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM execution_backend_binding`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh database already has %d backend bindings", count)
	}
	bindings := []execution.BackendBinding{{Family: "docker", Version: 1, Namespace: "first"}, {Family: "microsandbox", Version: 1, Namespace: "second"}}
	results := make([]error, len(bindings))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index, binding := range bindings {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[index] = databases[index].BindExecutionBackend(t.Context(), binding)
		}()
	}
	close(start)
	wait.Wait()
	winners := 0
	for index, result := range results {
		if result == nil {
			winners++
			if err := databases[index].BindExecutionBackend(t.Context(), bindings[index]); err != nil {
				t.Fatalf("same binding is not idempotent: %v", err)
			}
		} else if !errors.Is(result, execution.ErrBindingMismatch) {
			t.Fatalf("unexpected claim failure: %v", result)
		}
	}
	if winners != 1 {
		t.Fatalf("backend ownership winners = %d, want 1", winners)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM execution_backend_binding`); err == nil {
		t.Fatal("backend ownership could be deleted")
	}
}
