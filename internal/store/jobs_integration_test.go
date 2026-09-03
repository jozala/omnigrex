//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
)

func TestJobEnqueueIsIdempotentAndRejectsConflictingDefinition(t *testing.T) {
	database, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workflowID := "10000000-0000-4000-8000-000000000001"
	seedWorkflow(t, pool, workflowID)

	spec := store.JobSpec{
		Queue:          "workflow",
		Kind:           "NORMALIZE_EVENT",
		Payload:        json.RawMessage(`{"delivery_id":"abc"}`),
		Priority:       7,
		MaxAttempts:    3,
		IdempotencyKey: "workflow:10000000-0000-4000-8000-000000000001:delivery:abc",
		WorkflowID:     workflowID,
	}
	first, inserted, err := database[0].EnqueueJob(ctx, spec)
	if err != nil {
		t.Fatalf("first EnqueueJob() error = %v", err)
	}
	if !inserted {
		t.Fatal("first EnqueueJob() inserted = false, want true")
	}
	second, inserted, err := database[0].EnqueueJob(ctx, spec)
	if err != nil {
		t.Fatalf("duplicate EnqueueJob() error = %v", err)
	}
	if inserted || second.ID != first.ID {
		t.Errorf("duplicate EnqueueJob() = (%q, %t), want existing %q and false", second.ID, inserted, first.ID)
	}

	spec.Priority++
	if _, _, err := database[0].EnqueueJob(ctx, spec); !errors.Is(err, store.ErrJobIdempotencyConflict) {
		t.Errorf("conflicting EnqueueJob() error = %v, want ErrJobIdempotencyConflict", err)
	}
	invalid := spec
	invalid.IdempotencyKey = "workflow:invalid"
	invalid.Payload = json.RawMessage(`{"broken"`)
	if _, _, err := database[0].EnqueueJob(ctx, invalid); err == nil {
		t.Error("EnqueueJob() with invalid JSON error = nil")
	}
	invalid = spec
	invalid.IdempotencyKey = "workflow:invalid-scope"
	invalid.WorkflowID = ""
	invalid.WorkflowAttemptID = "20000000-0000-4000-8000-000000000001"
	if _, _, err := database[0].EnqueueJob(ctx, invalid); err == nil {
		t.Error("EnqueueJob() with incomplete scope error = nil")
	}
	otherWorkflowID := "10000000-0000-4000-8000-000000000002"
	otherAttemptID := "20000000-0000-4000-8000-000000000002"
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ($1, 2, 'owner', 'repo', 2, 2, 'ACTIVE')`, otherWorkflowID); err != nil {
		t.Fatalf("seed second workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ($1, $2, 1, 'ACTIVE')`, otherAttemptID, otherWorkflowID); err != nil {
		t.Fatalf("seed second workflow attempt: %v", err)
	}
	invalid = spec
	invalid.IdempotencyKey = "workflow:invalid-cross-scope"
	invalid.WorkflowAttemptID = otherAttemptID
	if _, _, err := database[0].EnqueueJob(ctx, invalid); err == nil {
		t.Error("EnqueueJob() with cross-workflow scope error = nil")
	}
}

func TestJobClaimsAreDistinctAndPriorityOrderedUnderConcurrency(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for index := range 8 {
		_, _, err := databases[0].EnqueueJob(ctx, store.JobSpec{
			Queue:          "workers",
			Kind:           "TEST",
			Payload:        json.RawMessage(`{}`),
			Priority:       index,
			MaxAttempts:    1,
			IdempotencyKey: "claim-order-" + string(rune('a'+index)),
		})
		if err != nil {
			t.Fatalf("EnqueueJob(%d) error = %v", index, err)
		}
	}

	first, err := databases[0].ClaimJob(ctx, "workers", "priority-worker", time.Second)
	if err != nil || first == nil {
		t.Fatalf("first ClaimJob() = (%#v, %v), want highest-priority job", first, err)
	}
	if first.Priority != 7 {
		t.Errorf("first claimed priority = %d, want 7", first.Priority)
	}

	start := make(chan struct{})
	claims := make(chan *store.JobLease, 7)
	errs := make(chan error, 7)
	var wait sync.WaitGroup
	for index := range 7 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			claim, err := databases[index%len(databases)].ClaimJob(ctx, "workers", "worker", time.Second)
			claims <- claim
			errs <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ClaimJob() error = %v", err)
		}
	}
	seen := map[string]bool{first.ID: true}
	for claim := range claims {
		if claim == nil {
			t.Fatal("ClaimJob() = nil, want one of eight jobs")
		}
		if seen[claim.ID] {
			t.Errorf("job %s claimed more than once", claim.ID)
		}
		seen[claim.ID] = true
	}
	if len(seen) != 8 {
		t.Errorf("distinct claimed jobs = %d, want 8", len(seen))
	}
}

func TestClaimJobKindLeavesOtherQueueKindsAvailable(t *testing.T) {
	database, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, spec := range []store.JobSpec{
		{Queue: "workflow", Kind: "OTHER_ACTION", Payload: json.RawMessage(`{}`), Priority: 100, MaxAttempts: 1, IdempotencyKey: "other-action"},
		{Queue: "workflow", Kind: "TARGET_ACTION", Payload: json.RawMessage(`{}`), Priority: 1, MaxAttempts: 1, IdempotencyKey: "target-action"},
	} {
		if _, _, err := database[0].EnqueueJob(ctx, spec); err != nil {
			t.Fatalf("EnqueueJob(%s) error = %v", spec.Kind, err)
		}
	}

	target, err := database[0].ClaimJobKind(ctx, "workflow", "TARGET_ACTION", "target-worker", time.Second)
	if err != nil || target == nil || target.Kind != "TARGET_ACTION" {
		t.Fatalf("ClaimJobKind() = (%#v, %v), want TARGET_ACTION", target, err)
	}
	other, err := database[0].ClaimJob(ctx, "workflow", "other-worker", time.Second)
	if err != nil || other == nil || other.Kind != "OTHER_ACTION" {
		t.Fatalf("ClaimJob() after filtered claim = (%#v, %v), want OTHER_ACTION", other, err)
	}
}

func TestClaimJobKindOnlyReclaimsMatchingExpiredKinds(t *testing.T) {
	database, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, kind := range []string{"TARGET_ACTION", "OTHER_ACTION"} {
		if _, _, err := database[0].EnqueueJob(ctx, store.JobSpec{
			Queue: "workflow", Kind: kind, Payload: json.RawMessage(`{}`), MaxAttempts: 2, IdempotencyKey: "expired-" + kind,
		}); err != nil {
			t.Fatalf("EnqueueJob(%s) error = %v", kind, err)
		}
	}
	first, err := database[0].ClaimJobKind(ctx, "workflow", "TARGET_ACTION", "dead-target", 50*time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("ClaimJobKind(TARGET_ACTION) = (%#v, %v)", first, err)
	}
	other, err := database[0].ClaimJobKind(ctx, "workflow", "OTHER_ACTION", "dead-other", 50*time.Millisecond)
	if err != nil || other == nil {
		t.Fatalf("ClaimJobKind(OTHER_ACTION) = (%#v, %v)", other, err)
	}
	time.Sleep(75 * time.Millisecond)

	reclaimed, err := database[0].ClaimJobKind(ctx, "workflow", "TARGET_ACTION", "replacement", time.Second)
	if err != nil || reclaimed == nil || reclaimed.ID != first.ID || reclaimed.Attempt != 2 {
		t.Fatalf("replacement ClaimJobKind() = (%#v, %v), want TARGET_ACTION attempt 2", reclaimed, err)
	}
	storedOther, err := database[0].GetJob(ctx, other.ID)
	if err != nil {
		t.Fatalf("GetJob(OTHER_ACTION) error = %v", err)
	}
	if storedOther.Status != store.JobLeased || storedOther.AttemptCount != 1 {
		t.Errorf("nonmatching expired job = (%s, attempt %d), want untouched LEASED attempt 1", storedOther.Status, storedOther.AttemptCount)
	}
}

func TestJobFailureRetriesThenExhaustsAttempts(t *testing.T) {
	database, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, _, err := database[0].EnqueueJob(ctx, store.JobSpec{
		Queue: "retry", Kind: "TEST", Payload: json.RawMessage(`{}`), MaxAttempts: 2, IdempotencyKey: "retry-exhaustion",
	})
	if err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	first, err := database[0].ClaimJob(ctx, "retry", "worker-a", time.Second)
	if err != nil || first == nil {
		t.Fatalf("first ClaimJob() = (%#v, %v), want lease", first, err)
	}
	if err := database[0].FailJob(ctx, *first, errors.New("temporary"), true, 0); err != nil {
		t.Fatalf("first FailJob() error = %v", err)
	}
	second, err := database[0].ClaimJob(ctx, "retry", "worker-b", time.Second)
	if err != nil || second == nil {
		t.Fatalf("second ClaimJob() = (%#v, %v), want retry lease", second, err)
	}
	if second.Attempt != 2 {
		t.Errorf("retry attempt = %d, want 2", second.Attempt)
	}
	if err := database[0].FailJob(ctx, *second, errors.New("still broken"), true, 0); err != nil {
		t.Fatalf("second FailJob() error = %v", err)
	}
	if claim, err := database[0].ClaimJob(ctx, "retry", "worker-c", time.Second); err != nil || claim != nil {
		t.Errorf("ClaimJob() after exhaustion = (%#v, %v), want (nil, nil)", claim, err)
	}
	got, err := database[0].GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if got.Status != store.JobFailed || got.AttemptCount != 2 {
		t.Errorf("exhausted job = (%s, %d attempts), want (FAILED, 2)", got.Status, got.AttemptCount)
	}
}

func TestJobExpiredWorkerIsRecoveredAndFenced(t *testing.T) {
	database, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := database[0].EnqueueJob(ctx, store.JobSpec{
		Queue: "recovery", Kind: "TEST", Payload: json.RawMessage(`{}`), MaxAttempts: 2, IdempotencyKey: "killed-worker",
	})
	if err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	deadWorker, err := database[0].ClaimJob(ctx, "recovery", "dead-worker", 60*time.Millisecond)
	if err != nil || deadWorker == nil {
		t.Fatalf("first ClaimJob() = (%#v, %v), want lease", deadWorker, err)
	}
	time.Sleep(90 * time.Millisecond)
	recovered, err := database[0].ClaimJob(ctx, "recovery", "replacement", time.Second)
	if err != nil || recovered == nil {
		t.Fatalf("recovery ClaimJob() = (%#v, %v), want lease", recovered, err)
	}
	if recovered.Attempt != 2 || recovered.LeaseToken == deadWorker.LeaseToken {
		t.Errorf("recovery lease = %#v, want attempt 2 with a new token", recovered)
	}
	if err := database[0].CompleteJob(ctx, *deadWorker, json.RawMessage(`{"stale":true}`)); !errors.Is(err, store.ErrJobLeaseLost) {
		t.Errorf("CompleteJob() by expired owner error = %v, want ErrJobLeaseLost", err)
	}
	if err := database[0].CompleteJob(ctx, *recovered, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("CompleteJob() by replacement error = %v", err)
	}
}

func TestJobHeartbeatPreventsReclaimAndExpiredFinalAttemptCannotStrand(t *testing.T) {
	database, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, _, err := database[0].EnqueueJob(ctx, store.JobSpec{
		Queue: "heartbeat", Kind: "TEST", Payload: json.RawMessage(`{}`), MaxAttempts: 1, IdempotencyKey: "heartbeat-fence",
	})
	if err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	lease, err := database[0].ClaimJob(ctx, "heartbeat", "worker", 80*time.Millisecond)
	if err != nil || lease == nil {
		t.Fatalf("ClaimJob() = (%#v, %v), want lease", lease, err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := database[0].HeartbeatJob(ctx, *lease, 180*time.Millisecond); err != nil {
		t.Fatalf("HeartbeatJob() error = %v", err)
	}
	time.Sleep(70 * time.Millisecond)
	if reclaimed, err := database[0].ReclaimExpiredJobs(ctx, 10); err != nil || reclaimed != 0 {
		t.Errorf("ReclaimExpiredJobs() during renewed lease = (%d, %v), want (0, nil)", reclaimed, err)
	}
	time.Sleep(130 * time.Millisecond)
	if reclaimed, err := database[0].ReclaimExpiredJobs(ctx, 10); err != nil || reclaimed != 1 {
		t.Fatalf("ReclaimExpiredJobs() after final expiry = (%d, %v), want (1, nil)", reclaimed, err)
	}
	got, err := database[0].GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if got.Status != store.JobFailed || got.LeaseToken != "" {
		t.Errorf("expired final-attempt job = %#v, want FAILED without lease", got)
	}
	if err := database[0].HeartbeatJob(ctx, *lease, time.Second); !errors.Is(err, store.ErrJobLeaseLost) {
		t.Errorf("HeartbeatJob() after reclaim error = %v, want ErrJobLeaseLost", err)
	}
}

func openPhaseFiveStores(t *testing.T, count int) ([]*store.Store, *pgxpool.Pool) {
	t.Helper()
	postgres := startPostgres(t)
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatalf("write database password: %v", err)
	}
	databases := make([]*store.Store, 0, count)
	for range count {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
		cancel()
		if err != nil {
			t.Fatalf("store.Open() error = %v", err)
		}
		databases = append(databases, database)
		t.Cleanup(database.Close)
	}
	return databases, openPool(t, postgres.databaseURL(true))
}

func seedWorkflow(t *testing.T, pool *pgxpool.Pool, workflowID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ($1, 1, 'owner', 'repo', 1, 1, 'ACTIVE')`, workflowID)
	if err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
}
