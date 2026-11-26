//go:build integration

package migrations

import (
	"fmt"
	"testing"
	"testing/fstest"
)

func TestLabelWriteObligationUpgradePreservesExistingEffects(t *testing.T) {
	pool, _ := migrationPostgres(t)
	ctx := t.Context()
	prefix := fstest.MapFS{}
	files, err := discover(Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range files {
		if m.version < 9 {
			prefix[fmt.Sprintf("%06d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if err = run(ctx, pool, prefix); err != nil {
		t.Fatal(err)
	}
	const workflowID = "80000000-0000-4000-8000-000000000901"
	const jobID = "80000000-0000-4000-8000-000000000902"
	if _, err = pool.Exec(ctx, `INSERT INTO workflows(id,repository_id,repository_owner,repository_name,issue_id,issue_number,status,state_revision)
 VALUES($1,1,'owner','repo',1,1,'CLOSED',7)`, workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO jobs(id,queue,kind,payload,max_attempts,idempotency_key,workflow_id,status,completed_at)
 VALUES($1,'workflow','RECONCILE_GITHUB_LABELS','{"revision":7,"human_input_only":true}',3,'existing-effect',$2,'SUCCEEDED',clock_timestamp())`, jobID, workflowID); err != nil {
		t.Fatal(err)
	}
	var before, after []byte
	if err = pool.QueryRow(ctx, `SELECT jsonb_build_object('workflows',(SELECT jsonb_agg(to_jsonb(w)) FROM workflows w),'jobs',(SELECT jsonb_agg(to_jsonb(j)) FROM jobs j))`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = Run(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var obligations, turns int
	if err = pool.QueryRow(ctx, `SELECT jsonb_build_object('workflows',(SELECT jsonb_agg(to_jsonb(w)) FROM workflows w),'jobs',(SELECT jsonb_agg(to_jsonb(j)) FROM jobs j)),(SELECT count(*) FROM workflow_github_label_writes),(SELECT count(*) FROM agent_turns)`).Scan(&after, &obligations, &turns); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || obligations != 0 || turns != 0 {
		t.Fatal("upgrade rewrote history or allocated work", obligations, turns)
	}
}
