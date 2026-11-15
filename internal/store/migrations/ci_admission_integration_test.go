//go:build integration

package migrations

import (
	"testing"
)

func TestCIAdmissionUpgradePreservesDeadlineAndRunningTurns(t *testing.T) {
	pool, _ := migrationPostgres(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
 CREATE TABLE workflow_attempts(id uuid PRIMARY KEY);
 CREATE TABLE agent_turns(id int, purpose text CONSTRAINT agent_turns_purpose_check CHECK(purpose='REVIEW'),status text);
 CREATE TABLE review_admission_requests(id uuid PRIMARY KEY,status text,release_evidence jsonb,deadline timestamptz,policy jsonb);
 INSERT INTO workflow_attempts VALUES('10000000-0000-4000-8000-000000000001');
 INSERT INTO agent_turns VALUES(1,'REVIEW','RUNNING');
 INSERT INTO review_admission_requests VALUES
 ('20000000-0000-4000-8000-000000000001','RELEASED','{"release_reason":"completed_ci"}','2026-10-27 12:30:00Z','{"timeout":1800000000000}'),
 ('20000000-0000-4000-8000-000000000002','ADMITTED','{}','2026-10-27 12:30:00Z','{}'),
 ('20000000-0000-4000-8000-000000000003','CANCELLED',NULL,'2026-10-27 12:30:00Z','{}');
 `); err != nil {
		t.Fatal(err)
	}
	sql, err := Files.ReadFile("000007_ci_qualified_admission.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	var waiting, admitted, cancelled, preserved, usage, running int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='WAITING' AND release_evidence IS NULL),count(*) FILTER(WHERE status='ADMITTED'),count(*) FILTER(WHERE status='CANCELLED'),count(*) FILTER(WHERE deadline='2026-10-27 12:30:00Z'),(SELECT ci_repair_usage FROM workflow_attempts),(SELECT count(*) FROM agent_turns WHERE status='RUNNING') FROM review_admission_requests`).Scan(&waiting, &admitted, &cancelled, &preserved, &usage, &running); err != nil {
		t.Fatal(err)
	}
	if waiting != 1 || admitted != 1 || cancelled != 1 || preserved != 3 || usage != 0 || running != 1 {
		t.Fatal(waiting, admitted, cancelled, preserved, usage, running)
	}
}
