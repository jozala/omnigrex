package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// BeginWorkflowGitHubLabelWrite commits a recovery obligation before any remote
// label operation. Only the same request's durable return can discharge it;
// replacement workers cannot prove that an expired request has stopped writing.
func (store *Store) BeginWorkflowGitHubLabelWrite(ctx context.Context, lease JobLease) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	job, err := lockFencedWorkflowJob(ctx, tx, lease, ReconcileGitHubLabelsJobKind, ErrWorkflowGitHubEffectFenceLost)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workflow_github_label_writes(lease_token,workflow_id,job_id)
 VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, lease.LeaseToken, job.WorkflowID, job.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recoverGitHubLabelWritesTx makes bounded progress on expired, uncertain remote
// writes. Success permits a later probe while the old request remains unresolved;
// failed/exhausted probes never receive a fresh retry budget. A lost process can
// leave uncertainty indefinitely, so one successful probe cannot retire it.
func recoverGitHubLabelWritesTx(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT pending.lease_token::text,pending.workflow_id::text,
 pending.probe_generation,(source.payload->>'revision')::bigint
 FROM workflow_github_label_writes pending
 JOIN workflows workflow ON workflow.id=pending.workflow_id
 JOIN jobs source ON source.id=pending.job_id
 JOIN job_attempts attempt ON attempt.lease_token=pending.lease_token
 LEFT JOIN jobs probe ON probe.id=pending.probe_job_id
 WHERE pending.next_probe_at<=clock_timestamp()
 AND (attempt.status<>'LEASED' OR attempt.lease_expires_at<=clock_timestamp())
 AND (probe.id IS NULL OR probe.status='SUCCEEDED')
 ORDER BY pending.next_probe_at,pending.lease_token
 LIMIT 16 FOR UPDATE OF workflow,pending SKIP LOCKED`)
	if err != nil {
		return err
	}
	type candidate struct {
		token, workflow      string
		generation, revision int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.token, &c.workflow, &c.generation, &c.revision); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		payload, _ := json.Marshal(map[string]any{"revision": c.revision, "human_input_only": true})
		id, err := insertIdempotentJobTx(ctx, tx, jobInsert{queue: WorkflowActionQueue, kind: ReconcileGitHubLabelsJobKind, payload: payload, maxAttempts: 3,
			idempotencyKey: fmt.Sprintf("human-input-uncertain:%s:%d", c.token, c.generation+1), scope: jobInsertScope{workflowID: c.workflow}})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE workflow_github_label_writes SET probe_job_id=$2,
   probe_generation=probe_generation+1,next_probe_at=clock_timestamp()+interval '1 minute'
   WHERE lease_token=$1`, c.token, id); err != nil {
			return err
		}
	}
	return nil
}
