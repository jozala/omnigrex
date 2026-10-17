//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestParticipantBaselineHasOneCanonicalIdentity(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var canonicalTable, legacyRelation, obsoleteColumns bool
	if err := pool.QueryRow(ctx, `SELECT
    EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass('public.agent_participants') AND relkind = 'r'),
    to_regclass('public.agent_assignments') IS NOT NULL,
    EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
            AND column_name IN ('agent_assignment_id', 'github_app_installation_id'))`).Scan(&canonicalTable, &legacyRelation, &obsoleteColumns); err != nil {
		t.Fatal(err)
	}
	if !canonicalTable || legacyRelation || obsoleteColumns {
		t.Fatalf("Participant schema has duplicate/obsolete identity: table=%t legacy=%t columns=%t", canonicalTable, legacyRelation, obsoleteColumns)
	}
}

func TestPrepareAgentTurnRequiresExactStagePreparation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000091", "62000000-0000-4000-8000-000000000091")
	lease := claimPreparationJob(t, database, ctx)
	spec := preparationSpec("missing-stage-profile", "openai/fixture")
	// A preparation for a different Stage cannot stand in for this Stage merely
	// because its Agent Profile performs the same Role.
	spec.Stages = map[workflow.StageID]store.ParticipantPreparation{workflow.StageID("another-implementation"): spec.Stages[workflow.StageImplementation]}
	if _, err := database.PrepareAgentTurn(ctx, lease, spec); err == nil || !strings.Contains(err.Error(), "no preparation for Stage") {
		t.Fatalf("missing exact Stage preparation = %v", err)
	}
	var participants, turns int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_participants), (SELECT count(*) FROM agent_turns)`).Scan(&participants, &turns); err != nil {
		t.Fatal(err)
	}
	if participants != 0 || turns != 0 {
		t.Fatalf("rejected Stage preparation created Participants/Turns: %d/%d", participants, turns)
	}
}
