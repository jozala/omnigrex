//go:build integration

package migrations

import (
	"context"
	"testing"
	"time"
)

func TestParticipantHandoffUpgradePreservesWaitingProvenance(t *testing.T) {
	pool, _ := migrationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Minimal pre-upgrade ledger isolates the provenance backfill from unrelated
	// schema constraints. The production migration is executed unchanged.
	if _, err := pool.Exec(ctx, `
CREATE TABLE workflow_attempts (id uuid PRIMARY KEY, workflow_id uuid, active bool, human_handoff_reason text, completed_at timestamptz, attempt_number int);
CREATE TABLE jobs (id uuid PRIMARY KEY, workflow_attempt_id uuid);
CREATE TABLE agent_participants (id int PRIMARY KEY, workflow_id uuid, status text, state_deleted_at timestamptz, created_at timestamptz, reactivated_by_preparation_job_id uuid);
INSERT INTO workflow_attempts VALUES
 ('10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', FALSE, 'agent_blocked', '2026-10-27 08:00:00Z', 3),
 ('10000000-0000-4000-8000-000000000002', '20000000-0000-4000-8000-000000000001', FALSE, NULL, '2026-10-27 08:20:00Z', 4),
 ('10000000-0000-4000-8000-000000000003', '20000000-0000-4000-8000-000000000002', FALSE, 'agent_blocked', '2026-10-27 08:30:00Z', 5);
INSERT INTO jobs VALUES ('30000000-0000-4000-8000-000000000001', '10000000-0000-4000-8000-000000000002');
INSERT INTO agent_participants VALUES
 (1, '20000000-0000-4000-8000-000000000001', 'WAITING_FOR_HUMAN', NULL, '2026-10-27 07:00:00Z', NULL),
 (2, '20000000-0000-4000-8000-000000000001', 'WAITING_FOR_HUMAN', NULL, '2026-10-27 08:10:00Z', NULL),
 (3, '20000000-0000-4000-8000-000000000001', 'ACTIVE', NULL, '2026-10-27 07:00:00Z', NULL),
 (4, '20000000-0000-4000-8000-000000000001', 'WAITING_FOR_HUMAN', '2026-10-27 08:10:00Z', '2026-10-27 07:00:00Z', NULL),
 (5, '20000000-0000-4000-8000-000000000001', 'WAITING_FOR_HUMAN', NULL, '2026-10-27 07:00:00Z', '30000000-0000-4000-8000-000000000001'),
 (6, '20000000-0000-4000-8000-000000000004', 'WAITING_FOR_HUMAN', NULL, '2026-10-27 07:00:00Z', NULL);
`); err != nil {
		t.Fatal(err)
	}
	sql, err := Files.ReadFile("000003_participant_handoff_provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 6; id++ {
		var handoff string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(waiting_handoff_attempt_id::text, '') FROM agent_participants WHERE id = $1`, id).Scan(&handoff); err != nil {
			t.Fatal(err)
		}
		want := ""
		if id == 1 {
			want = "10000000-0000-4000-8000-000000000001"
		}
		if handoff != want {
			t.Errorf("Participant %d Handoff = %q, want %q", id, handoff, want)
		}
	}
}
