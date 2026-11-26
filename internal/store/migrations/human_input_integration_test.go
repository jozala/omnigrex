//go:build integration

package migrations

import (
	"encoding/json"
	"fmt"
	"testing"
	"testing/fstest"
)

func TestHumanInputUpgradeSeedsOnlyDurableLabelWork(t *testing.T) {
	pool, _ := migrationPostgres(t)
	ctx := t.Context()
	prefix := fstest.MapFS{}
	files, err := discover(Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range files {
		if m.version >= 8 {
			continue
		}
		prefix[fmt.Sprintf("%06d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
	}
	if err = run(ctx, pool, prefix); err != nil {
		t.Fatal(err)
	}
	statuses := []string{"PLANNING", "DEVELOPING", "REVIEWING", "PLAN_READY", "PR_READY", "NEEDS_HUMAN", "CLOSING", "CLOSED", "DORMANT"}
	for i, status := range statuses {
		id := fmt.Sprintf("80000000-0000-4000-8000-%012d", i+1)
		if _, err = pool.Exec(ctx, `INSERT INTO workflows(id,repository_id,repository_owner,repository_name,issue_id,issue_number,status,state_revision,clarification_waiting) VALUES($1,1,'owner','repo',$2,$2,$3,7,true);`, id, i+1, status); err != nil {
			t.Fatal(err)
		}
		state := fmt.Sprintf(`{"waiting":true,"rounds":[{"id":"R1","published":true}],"inputs":[{"body":"preserve history %d"}]}`, i)
		if _, err = pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, id, state); err != nil {
			t.Fatal(err)
		}
		if status == "DEVELOPING" {
			if _, err = pool.Exec(ctx, `INSERT INTO jobs(id,queue,kind,payload,max_attempts,workflow_id) VALUES(gen_random_uuid(),'workflow','PREPARE_AGENT_TURN','{"revision":7}',3,$1)`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	var before []byte
	if err = pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(c) ORDER BY workflow_id) FROM work_item_collaboration c`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = Run(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var after []byte
	var labels, preparations, turns, unchanged int
	if err = pool.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(c) ORDER BY workflow_id) FROM work_item_collaboration c),
 (SELECT count(*) FROM jobs WHERE kind='RECONCILE_GITHUB_LABELS' AND payload='{"revision":7,"human_input_only":true}'::jsonb AND idempotency_key LIKE 'human-input-upgrade:%' AND status='AVAILABLE' AND attempt_count=0),
 (SELECT count(*) FROM jobs WHERE kind='PREPARE_AGENT_TURN'),(SELECT count(*) FROM agent_turns),
 (SELECT count(*) FROM workflows WHERE clarification_waiting AND state_revision=7 AND NOT human_input_waiting AND human_input_presentation_revision=0)`).Scan(&after, &labels, &preparations, &turns, &unchanged); err != nil {
		t.Fatal(err)
	}
	var a, b any
	if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil {
		t.Fatal("invalid history snapshot")
	}
	if string(before) != string(after) || labels != len(statuses) || unchanged != len(statuses) || preparations != 1 || turns != 0 {
		t.Fatalf("upgrade changed authority/history or duplicated work: labels=%d preparations=%d turns=%d unchanged=%d", labels, preparations, turns, unchanged)
	}
}
