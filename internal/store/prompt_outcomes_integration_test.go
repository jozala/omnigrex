//go:build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jozala/omnigrex/internal/store/migrations"
)

func TestNeutralPromptMigrationPreservesHistoryAndEnforcesEvidenceShape(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var earlier []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "000036_" {
			earlier = append(earlier, entry.Name())
		}
	}
	if len(earlier) != 35 {
		t.Fatalf("pre-neutral migration count = %d, want 35", len(earlier))
	}
	applyRecordedMigrations(t, ctx, pool, earlier...)
	fixture := seedAgentSession(t, pool, 2300)
	const turnID = "96000000-0000-4000-8000-000000002300"
	if _, err := pool.Exec(ctx, `INSERT INTO agent_turns (
    id, workflow_id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
    operation_lineage_id, status, active, control_revision, purpose,
    agent_profile_commit_sha, agent_profile_content_sha256,
    prompt_stop_reason, prompt_recorded_at, outcome, completed_at
) VALUES ($1, $2, $3, $4, 1, 1, $1, 'FAILED', FALSE, 1, 'INITIAL_DEVELOPMENT',
          'historical-profile', decode(repeat('ab', 32), 'hex'),
          'end_turn', clock_timestamp(), '{"stop_reason":"end_turn"}', clock_timestamp())`,
		turnID, fixture.workflowID, fixture.sessionID, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	requireConstraint := func(err error, constraint string) {
		t.Helper()
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "23514" || postgresError.ConstraintName != constraint {
			t.Fatalf("constraint violation = %v, want %s", err, constraint)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE agent_turns SET prompt_stop_reason = 'completed' WHERE id = $1`, turnID)
	requireConstraint(err, "agent_turns_prompt_stop_reason_check")
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var historical bool
	if err := pool.QueryRow(ctx, `SELECT prompt_stop_reason = 'end_turn'
    AND outcome = '{"stop_reason":"end_turn"}'::jsonb FROM agent_turns WHERE id = $1`, turnID).Scan(&historical); err != nil || !historical {
		t.Fatalf("historical prompt was rewritten: preserved %t, error %v", historical, err)
	}
	for _, status := range []string{"completed", "limit_reached", "aborted"} {
		if _, err := pool.Exec(ctx, `UPDATE agent_turns SET prompt_stop_reason = $2 WHERE id = $1`, turnID, status); err != nil {
			t.Fatalf("neutral classification %q: %v", status, err)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE agent_turns SET prompt_stop_reason = 'unknown' WHERE id = $1`, turnID)
	requireConstraint(err, "agent_turns_prompt_stop_reason_check")
	_, err = pool.Exec(ctx, `UPDATE agent_turns SET prompt_error_class = 'FAILURE' WHERE id = $1`, turnID)
	requireConstraint(err, "agent_turns_prompt_evidence_shape_check")
	_, err = pool.Exec(ctx, `UPDATE agent_turns SET prompt_recorded_at = NULL WHERE id = $1`, turnID)
	requireConstraint(err, "agent_turns_prompt_evidence_shape_check")
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET prompt_stop_reason = NULL, prompt_error_class = 'DEADLINE' WHERE id = $1`, turnID); err != nil {
		t.Fatalf("neutral error classification: %v", err)
	}
}
