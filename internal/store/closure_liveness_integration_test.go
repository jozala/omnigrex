//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workflowaction"
)

func TestClosureSettlementWaitsDoNotConsumeFailureBudget(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := seedClosingWorkflowWithTurn(t, databases[0], pool, ctx, 951)
	config := closureLivenessConfig("settlement-waiter")
	settlementWorker, err := workflowaction.NewClosureSettlementWorker(databases[0], noMutationReconciler{}, config)
	if err != nil {
		t.Fatal(err)
	}

	for wait := 1; wait <= 5; wait++ {
		processed, err := settlementWorker.ProcessNext(ctx)
		if err != nil || !processed {
			t.Fatalf("settlement wait %d = (%t, %v)", wait, processed, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	stopWorker, err := workflowaction.NewClosureStopWorker(databases[0], &closureLivenessCleaner{}, closureLivenessConfig("stopper"))
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := stopWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("stop ProcessNext() = (%t, %v)", processed, err)
	}
	time.Sleep(2 * time.Millisecond)
	if processed, err := settlementWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("settlement after stop = (%t, %v)", processed, err)
	}

	var state string
	var attempts, handoffs int
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind = 'SETTLE_CLOSURE'),
       count(*) FILTER (WHERE kind = 'PUBLISH_HUMAN_HANDOFF' AND payload->>'reason' = 'closure_cleanup_exhausted')
FROM jobs AS job
LEFT JOIN job_attempts AS attempt ON attempt.job_id = job.id
WHERE job.workflow_id = $1`, fixture.workflowID).Scan(&attempts, &handoffs); err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.StateClosed) || attempts != 6 || handoffs != 0 {
		t.Fatalf("closure after waits = state %s, settlement attempts %d, handoffs %d", state, attempts, handoffs)
	}
}

func TestClosureStopFailureExhaustionPublishesOnceAndCleanupRecovers(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := seedClosingWorkflowWithTurn(t, databases[0], pool, ctx, 952)
	failure := errors.New("Docker temporarily unavailable")
	cleaner := &closureLivenessCleaner{results: []error{failure, failure, failure}}
	worker, err := workflowaction.NewClosureStopWorker(databases[0], cleaner, closureLivenessConfig("failing-stopper"))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		processed, err := worker.ProcessNext(ctx)
		if !processed || !errors.Is(err, failure) {
			t.Fatalf("stop attempt %d = (%t, %v)", attempt, processed, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if processed, err := worker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("recovering stop attempt = (%t, %v)", processed, err)
	}
	assertClosureSafetyContinuation(t, pool, ctx, fixture.workflowID, store.StopAgentTurnJobKind, 4)
	assertSafetyHandoffVisible(t, databases[0], ctx, workflow.StateClosing)
}

func TestClosureSettlementFailureExhaustionPublishesOnceAndSettlementRecovers(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := seedClosingWorkflowWithoutTurn(t, databases[0], pool, ctx, 953)
	failure := errors.New("database commit temporarily unavailable")
	workerStore := &closureSettlementFailureStore{Store: databases[0], results: []error{failure, failure, failure}}
	worker, err := workflowaction.NewClosureSettlementWorker(workerStore, noMutationReconciler{}, closureLivenessConfig("failing-settler"))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		processed, err := worker.ProcessNext(ctx)
		if !processed || !errors.Is(err, failure) {
			t.Fatalf("settlement attempt %d = (%t, %v)", attempt, processed, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if processed, err := worker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("recovering settlement attempt = (%t, %v)", processed, err)
	}
	assertClosureSafetyContinuation(t, pool, ctx, fixture.workflowID, store.SettleClosureJobKind, 4)
	assertSafetyHandoffVisible(t, databases[0], ctx, workflow.StateClosed)
}

func assertSafetyHandoffVisible(t *testing.T, database *store.Store, ctx context.Context, wantState workflow.State) {
	t.Helper()
	lease, err := database.ClaimWorkflowGitHubEffectJob(ctx, store.PublishHumanHandoffJobKind, "safety-handoff-test", time.Second)
	if err != nil || lease == nil {
		t.Fatalf("claim safety Human Handoff = (%#v, %v)", lease, err)
	}
	effect, err := database.GetWorkflowGitHubEffectContext(ctx, *lease)
	if err != nil {
		t.Fatal(err)
	}
	if !effect.SafetyDiagnostic || effect.State != wantState || effect.HandoffReason != "closure_cleanup_exhausted" || effect.HandoffDiagnostic == "" {
		t.Fatalf("safety Human Handoff context = %#v", effect)
	}
	if _, err := database.AcknowledgeWorkflowGitHubEffect(ctx, *lease, effect, json.RawMessage(`{"published":true}`)); err != nil {
		t.Fatalf("acknowledge safety Human Handoff = %v", err)
	}
}

func assertClosureSafetyContinuation(t *testing.T, pool *pgxpool.Pool, ctx context.Context, workflowID, kind string, wantAttempts int) {
	t.Helper()
	var state, jobStatus string
	var attempts, handoffs int
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, workflowID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE workflow_id = $1 AND kind = $2`, workflowID, kind).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts AS attempt JOIN jobs AS job ON job.id = attempt.job_id WHERE job.workflow_id = $1 AND job.kind = $2`, workflowID, kind).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF' AND payload->>'reason' = 'closure_cleanup_exhausted'`, workflowID).Scan(&handoffs); err != nil {
		t.Fatal(err)
	}
	if jobStatus != string(store.JobSucceeded) || attempts != wantAttempts || handoffs != 1 {
		t.Fatalf("continued %s = Workflow %s, Job %s, attempts %d, handoffs %d", kind, state, jobStatus, attempts, handoffs)
	}
}

func seedClosingWorkflowWithTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, number int) agentFixture {
	t.Helper()
	fixture := seedAgentSession(t, pool, number)
	prepareClosableFixture(t, pool, fixture)
	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, time.Second)
	if _, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "closure-liveness-runtime", time.Second, 1); err != nil {
		t.Fatal(err)
	}
	closeWorkflowForLiveness(t, database, ctx, fixture, number)
	return fixture
}

func seedClosingWorkflowWithoutTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, number int) agentFixture {
	t.Helper()
	fixture := seedAgentSession(t, pool, number)
	prepareClosableFixture(t, pool, fixture)
	closeWorkflowForLiveness(t, database, ctx, fixture, number)
	return fixture
}

func closeWorkflowForLiveness(t *testing.T, database *store.Store, ctx context.Context, fixture agentFixture, number int) {
	t.Helper()
	delivery := workflowDelivery(fmt.Sprintf("7c000000-0000-4000-8000-%012d", number))
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(number), int64(number), int64(number)
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	observedAt := time.Now().UTC()
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{EventMetadata: workflow.EventMetadata{
				ID: claim.DeliveryID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem,
				ExpectedRevision: snapshot.Revision,
			}, ClosureID: fmt.Sprintf("closure-liveness-%d", number), RetainUntil: observedAt.Add(time.Hour), RetentionToken: fmt.Sprintf("retention-liveness-%d", number)})
		})
	if err != nil || application.State != workflow.StateClosing {
		t.Fatalf("close Workflow = (%#v, %v)", application, err)
	}
}

func closureLivenessConfig(owner string) workflowaction.ClosureWorkerConfig {
	return workflowaction.ClosureWorkerConfig{
		ClaimOwner: owner, LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	}
}

type closureLivenessCleaner struct {
	mutex   sync.Mutex
	results []error
}

func (cleaner *closureLivenessCleaner) EnsureAbsent(context.Context, map[string]string) error {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	if len(cleaner.results) == 0 {
		return nil
	}
	result := cleaner.results[0]
	cleaner.results = cleaner.results[1:]
	return result
}

type closureSettlementFailureStore struct {
	*store.Store
	mutex   sync.Mutex
	results []error
}

func (database *closureSettlementFailureStore) CompleteClosureSettlement(ctx context.Context, lease store.JobLease) (store.ClosureSettlement, error) {
	database.mutex.Lock()
	if len(database.results) != 0 {
		result := database.results[0]
		database.results = database.results[1:]
		database.mutex.Unlock()
		return store.ClosureSettlement{}, result
	}
	database.mutex.Unlock()
	return database.Store.CompleteClosureSettlement(ctx, lease)
}
