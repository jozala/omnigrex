//go:build integration

package migrations

import (
	"context"
	"testing"
	"time"
)

func TestReplanningUpgradeBindsOnlyEstablishedPlansWithoutChangingHistoryOrUsage(t *testing.T) {
	pool, _ := migrationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Execute the production migration unchanged against a minimal legacy ledger.
	if _, err := pool.Exec(ctx, `
CREATE TABLE workflow_attempts(id int PRIMARY KEY, review_usage int, infrastructure_failures int, active bool);
CREATE TABLE agent_turns(id int PRIMARY KEY, workflow_id int, purpose text CONSTRAINT agent_turns_purpose_check CHECK(purpose IN ('REVIEW')));
CREATE TABLE change_proposals(id int PRIMARY KEY, workflow_id int, ready_for_sha text);
CREATE TABLE change_proposal_reviews(id int PRIMARY KEY, change_proposal_id int, accepted bool);
CREATE TABLE agent_turn_settlements(workflow_outcome text CONSTRAINT agent_turn_settlements_workflow_outcome_check CHECK(workflow_outcome IN ('APPROVED')));
CREATE TABLE review_admission_requests(id int PRIMARY KEY, workflow_id int);
CREATE TABLE work_item_collaboration(workflow_id int PRIMARY KEY,state jsonb);
INSERT INTO workflow_attempts VALUES(1,2,1,true),(2,3,2,false),(3,0,0,true);
INSERT INTO work_item_collaboration VALUES
 (1,'{"planning":{"implementation":{"first_developer_turn_id":"developer","accepted_revision_id":"pair","authorization_id":"grant","actions":[{"id":"grant","kind":"implement"}]}}}'),
 (2,'{}'),
 (3,'{"planning":{"current_revision":"proposed","implementation":{}}}');
INSERT INTO agent_turns VALUES(1,1,'REVIEW'),(2,2,'REVIEW'),(3,3,'REVIEW');
INSERT INTO change_proposals VALUES(1,1,'same-head'),(2,2,'direct-head'),(3,3,null);
INSERT INTO change_proposal_reviews VALUES(1,1,true),(2,2,true);
INSERT INTO review_admission_requests VALUES(1,1),(2,2),(3,3);
INSERT INTO agent_turn_settlements VALUES('APPROVED');
`); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT jsonb_agg(state ORDER BY workflow_id)::text FROM work_item_collaboration`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	sql, err := Files.ReadFile("000007_replanning.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	var after string
	if err = pool.QueryRow(ctx, `SELECT jsonb_agg(state ORDER BY workflow_id)::text FROM work_item_collaboration`).Scan(&after); err != nil || before != after {
		t.Fatal("migration rewrote evidence or invented authority", before, after, err)
	}
	for id := 1; id <= 3; id++ {
		var turn, ready, admission string
		var returns, review, infra int
		var active bool
		if err = pool.QueryRow(ctx, `SELECT COALESCE(t.accepted_plan_revision_id,''),COALESCE(p.ready_for_plan_revision_id,''),COALESCE(r.accepted_plan_revision_id,''),a.autonomous_replanning_returns,a.review_usage,a.infrastructure_failures,a.active FROM workflow_attempts a JOIN agent_turns t ON t.id=a.id JOIN change_proposals p ON p.id=a.id JOIN review_admission_requests r ON r.id=a.id WHERE a.id=$1`, id).Scan(&turn, &ready, &admission, &returns, &review, &infra, &active); err != nil {
			t.Fatal(err)
		}
		want := ""
		if id == 1 {
			want = "pair"
		}
		if turn != want || ready != want || admission != want || returns != 0 || review != []int{2, 3, 0}[id-1] || infra != []int{1, 2, 0}[id-1] || active != (id != 2) {
			t.Fatal(id, turn, ready, admission, returns, review, infra, active)
		}
	}
	var planned, direct, head string
	var accepted int
	if err = pool.QueryRow(ctx, `SELECT COALESCE((SELECT accepted_plan_revision_id FROM change_proposal_reviews WHERE id=1),''),COALESCE((SELECT accepted_plan_revision_id FROM change_proposal_reviews WHERE id=2),''),(SELECT ready_for_sha FROM change_proposals WHERE id=1),(SELECT count(*) FROM change_proposal_reviews WHERE accepted)`).Scan(&planned, &direct, &head, &accepted); err != nil || planned != "pair" || direct != "" || head != "same-head" || accepted != 2 {
		t.Fatal(planned, direct, head, accepted, err)
	}
}
