//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
)

type jobSeed struct {
	Queue          string
	Kind           string
	Payload        json.RawMessage
	Priority       int
	MaxAttempts    int
	IdempotencyKey string
	WorkflowID     string
}

func insertJob(t *testing.T, pool *pgxpool.Pool, ctx context.Context, seed jobSeed) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `
INSERT INTO jobs (id, queue, kind, payload, priority, max_attempts, idempotency_key, workflow_id)
VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid)
RETURNING id::text`, seed.Queue, seed.Kind, seed.Payload, seed.Priority, seed.MaxAttempts,
		seed.IdempotencyKey, seed.WorkflowID).Scan(&id); err != nil {
		t.Fatalf("insert test job: %v", err)
	}
	return id
}

type storedJob struct {
	Status       store.JobStatus
	AvailableAt  time.Time
	AttemptCount int
	MaxAttempts  int
	LeaseToken   string
	CompletedAt  *time.Time
	Result       json.RawMessage
	LastError    string
}

func readStoredJob(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id string) storedJob {
	t.Helper()
	var job storedJob
	if err := pool.QueryRow(ctx, `
SELECT status, available_at, attempt_count, max_attempts, COALESCE(lease_token::text, ''),
       completed_at, result, COALESCE(last_error, '')
FROM jobs
WHERE id = $1`, id).Scan(&job.Status, &job.AvailableAt, &job.AttemptCount, &job.MaxAttempts,
		&job.LeaseToken, &job.CompletedAt, &job.Result, &job.LastError); err != nil {
		t.Fatalf("read test job: %v", err)
	}
	return job
}
