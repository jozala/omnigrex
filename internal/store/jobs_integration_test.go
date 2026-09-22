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

func TestJobClaimsAreDistinctAndPriorityOrderedUnderConcurrency(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for index := range 8 {
		insertJob(t, pool, ctx, jobSeed{
			Queue:          "workers",
			Kind:           "TEST",
			Payload:        json.RawMessage(`{}`),
			Priority:       index,
			MaxAttempts:    1,
			IdempotencyKey: "claim-order-" + string(rune('a'+index)),
		})
	}

	first, err := databases[0].ClaimJobKind(ctx, "workers", "TEST", "priority-worker", time.Second)
	if err != nil || first == nil {
		t.Fatalf("first ClaimJobKind() = (%#v, %v), want highest-priority job", first, err)
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
			claim, err := databases[index%len(databases)].ClaimJobKind(ctx, "workers", "TEST", "worker", time.Second)
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
			t.Fatalf("ClaimJobKind() error = %v", err)
		}
	}
	seen := map[string]bool{first.ID: true}
	for claim := range claims {
		if claim == nil {
			t.Fatal("ClaimJobKind() = nil, want one of eight jobs")
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
	database, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, seed := range []jobSeed{
		{Queue: "workflow", Kind: "OTHER_ACTION", Payload: json.RawMessage(`{}`), Priority: 100, MaxAttempts: 1, IdempotencyKey: "other-action"},
		{Queue: "workflow", Kind: "TARGET_ACTION", Payload: json.RawMessage(`{}`), Priority: 1, MaxAttempts: 1, IdempotencyKey: "target-action"},
	} {
		insertJob(t, pool, ctx, seed)
	}

	target, err := database[0].ClaimJobKind(ctx, "workflow", "TARGET_ACTION", "target-worker", time.Second)
	if err != nil || target == nil || target.Kind != "TARGET_ACTION" {
		t.Fatalf("ClaimJobKind() = (%#v, %v), want TARGET_ACTION", target, err)
	}
	other, err := database[0].ClaimJobKind(ctx, "workflow", "OTHER_ACTION", "other-worker", time.Second)
	if err != nil || other == nil || other.Kind != "OTHER_ACTION" {
		t.Fatalf("ClaimJobKind() after filtered claim = (%#v, %v), want OTHER_ACTION", other, err)
	}
}

func TestClaimJobKindOnlyReclaimsMatchingExpiredKinds(t *testing.T) {
	database, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, kind := range []string{"TARGET_ACTION", "OTHER_ACTION"} {
		insertJob(t, pool, ctx, jobSeed{
			Queue: "workflow", Kind: kind, Payload: json.RawMessage(`{}`), MaxAttempts: 2, IdempotencyKey: "expired-" + kind,
		})
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
	storedOther := readStoredJob(t, pool, ctx, other.ID)
	if storedOther.Status != store.JobLeased || storedOther.AttemptCount != 1 {
		t.Errorf("nonmatching expired job = (%s, attempt %d), want untouched LEASED attempt 1", storedOther.Status, storedOther.AttemptCount)
	}
}

func TestJobFailureRetriesThenExhaustsAttempts(t *testing.T) {
	database, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobID := insertJob(t, pool, ctx, jobSeed{
		Queue: "retry", Kind: "TEST", Payload: json.RawMessage(`{}`), MaxAttempts: 2, IdempotencyKey: "retry-exhaustion",
	})
	first, err := database[0].ClaimJobKind(ctx, "retry", "TEST", "worker-a", time.Second)
	if err != nil || first == nil {
		t.Fatalf("first ClaimJobKind() = (%#v, %v), want lease", first, err)
	}
	if err := database[0].FailJob(ctx, *first, errors.New("temporary"), true, 0); err != nil {
		t.Fatalf("first FailJob() error = %v", err)
	}
	second, err := database[0].ClaimJobKind(ctx, "retry", "TEST", "worker-b", time.Second)
	if err != nil || second == nil {
		t.Fatalf("second ClaimJobKind() = (%#v, %v), want retry lease", second, err)
	}
	if second.Attempt != 2 {
		t.Errorf("retry attempt = %d, want 2", second.Attempt)
	}
	if err := database[0].FailJob(ctx, *second, errors.New("still broken"), true, 0); err != nil {
		t.Fatalf("second FailJob() error = %v", err)
	}
	if claim, err := database[0].ClaimJobKind(ctx, "retry", "TEST", "worker-c", time.Second); err != nil || claim != nil {
		t.Errorf("ClaimJobKind() after exhaustion = (%#v, %v), want (nil, nil)", claim, err)
	}
	got := readStoredJob(t, pool, ctx, jobID)
	if got.Status != store.JobFailed || got.AttemptCount != 2 {
		t.Errorf("exhausted job = (%s, %d attempts), want (FAILED, 2)", got.Status, got.AttemptCount)
	}
}

func TestJobExpiredWorkerIsRecoveredAndFenced(t *testing.T) {
	database, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	insertJob(t, pool, ctx, jobSeed{
		Queue: "recovery", Kind: "TEST", Payload: json.RawMessage(`{}`), MaxAttempts: 2, IdempotencyKey: "killed-worker",
	})
	deadWorker, err := database[0].ClaimJobKind(ctx, "recovery", "TEST", "dead-worker", 60*time.Millisecond)
	if err != nil || deadWorker == nil {
		t.Fatalf("first ClaimJobKind() = (%#v, %v), want lease", deadWorker, err)
	}
	time.Sleep(90 * time.Millisecond)
	recovered, err := database[0].ClaimJobKind(ctx, "recovery", "TEST", "replacement", time.Second)
	if err != nil || recovered == nil {
		t.Fatalf("recovery ClaimJobKind() = (%#v, %v), want lease", recovered, err)
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
	database, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobID := insertJob(t, pool, ctx, jobSeed{
		Queue: "heartbeat", Kind: "TEST", Payload: json.RawMessage(`{}`), MaxAttempts: 1, IdempotencyKey: "heartbeat-fence",
	})
	lease, err := database[0].ClaimJobKind(ctx, "heartbeat", "TEST", "worker", 80*time.Millisecond)
	if err != nil || lease == nil {
		t.Fatalf("ClaimJobKind() = (%#v, %v), want lease", lease, err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := database[0].HeartbeatJob(ctx, *lease, 180*time.Millisecond); err != nil {
		t.Fatalf("HeartbeatJob() error = %v", err)
	}
	time.Sleep(70 * time.Millisecond)
	if reclaimed, err := database[0].ClaimJobKind(ctx, "heartbeat", "TEST", "replacement", time.Second); err != nil || reclaimed != nil {
		t.Errorf("ClaimJobKind() during renewed lease = (%#v, %v), want (nil, nil)", reclaimed, err)
	}
	time.Sleep(130 * time.Millisecond)
	if reclaimed, err := database[0].ClaimJobKind(ctx, "heartbeat", "TEST", "replacement", time.Second); err != nil || reclaimed != nil {
		t.Fatalf("ClaimJobKind() after final expiry = (%#v, %v), want (nil, nil)", reclaimed, err)
	}
	got := readStoredJob(t, pool, ctx, jobID)
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
		database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile, builtinStoreConfig(t))
		cancel()
		if err != nil {
			t.Fatalf("store.Open() error = %v", err)
		}
		databases = append(databases, database)
		t.Cleanup(database.Close)
	}
	return databases, openPool(t, postgres.databaseURL(true))
}
