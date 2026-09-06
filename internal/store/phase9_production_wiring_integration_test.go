//go:build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/retention"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/startup"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workflowaction"
)

func TestPhaseNineProductionWiringRecoversBeforeExecutionAndConsumesClosureRetention(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	profile, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:"+strings.Repeat("a", 64),
		runtimeprofile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	binding := profile.Binding()

	expiredFixture := seedAgentSession(t, pool, 91)
	expiredTurn, err := database.AllocateAgentTurn(ctx, expiredFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	setFixtureRuntimeBinding(t, pool, expiredFixture, binding, binding)
	expiredJob := claimAgentTurnJob(t, database, ctx, expiredTurn, time.Second)
	if _, err := database.AcquireAgentTurn(ctx, expiredJob, expiredTurn.ControlRevision, "expired-runtime", time.Second, 2); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, expiredJob.ID, expiredTurn.ID)

	queuedFixture := seedAgentSession(t, pool, 92)
	queuedTurn, err := database.AllocateAgentTurn(ctx, queuedFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	setFixtureRuntimeBinding(t, pool, queuedFixture, binding, binding)

	runtimes := emptyStartupRuntimes{}
	startupReconciler, err := startup.NewReconciler(database, runtimes, runtimes, runtimes, startup.ReconcilerOptions{
		MaxPasses: 10, MaxRuntimeProcesses: 100, MaxRecoveries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := startupReconciler.Reconcile(ctx)
	if err != nil || result.RecoveredAgentTurns != 1 {
		t.Fatalf("startup Reconcile() = (%#v, %v)", result, err)
	}
	expiredIdentity := store.AgentTurnRuntimeIdentity{
		AssignmentID: expiredFixture.assignmentID, AgentSessionID: expiredFixture.sessionID,
		AgentTurnID: expiredTurn.ID, ExecutionEpoch: expiredTurn.ExecutionEpoch,
		RuntimeProfileName: binding.Name, RuntimeProfileVersion: binding.Version,
	}
	assertRuntimeDisposition(t, database, ctx, expiredIdentity, store.AgentTurnRuntimeRecovery)
	executionLease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, "ordinary-execution", time.Second, 2)
	if err != nil || !acquired || executionLease.ID != queuedTurn.ID {
		t.Fatalf("execution claim after startup recovery = (%#v, %t, %v), want queued Turn %s", executionLease, acquired, err, queuedTurn.ID)
	}

	closureFixture := seedAgentSession(t, pool, 93)
	makeFixtureRuntimePathCanonical(t, pool, closureFixture)
	prepareClosableFixture(t, pool, closureFixture)
	closureTurn, err := database.AllocateAgentTurn(ctx, closureFixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	setFixtureRuntimeBinding(t, pool, closureFixture, binding, binding)
	closureJob := claimAgentTurnJob(t, database, ctx, closureTurn, time.Second)
	if _, err := database.AcquireAgentTurn(ctx, closureJob, closureTurn.ControlRevision, "closure-runtime", time.Second, 2); err != nil {
		t.Fatal(err)
	}

	delivery := workflowDelivery("7a000000-0000-4000-8000-000000000001")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 93, 93, 93
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	closedAt := time.Now().UTC().Add(-2 * time.Hour)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: 93, IssueID: 93, IssueNumber: 93},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{EventMetadata: workflow.EventMetadata{
				ID: claim.DeliveryID, ObservedAt: closedAt, WorkItem: snapshot.WorkItem,
				ExpectedRevision: snapshot.Revision,
			}, ClosureID: "production-wiring-closure", RetainUntil: closedAt.Add(time.Hour), RetentionToken: "production-wiring-retention"})
		})
	if err != nil || application.State != workflow.StateClosing {
		t.Fatalf("close Workflow = (%#v, %v)", application, err)
	}

	closureCleaner := &wiringClosureCleaner{}
	workerConfig := workflowaction.ClosureWorkerConfig{
		LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	}
	workerConfig.ClaimOwner = "production-closure-stop"
	stopWorker, err := workflowaction.NewClosureStopWorker(database, closureCleaner, workerConfig)
	if err != nil {
		t.Fatal(err)
	}
	workerConfig.ClaimOwner = "production-closure-settlement"
	settlementWorker, err := workflowaction.NewClosureSettlementWorker(database, noMutationReconciler{}, workerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := stopWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("closure stop ProcessNext() = (%t, %v)", processed, err)
	}
	if processed, err := settlementWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("closure settlement ProcessNext() = (%t, %v)", processed, err)
	}
	if closureCleaner.calls != 1 {
		t.Fatalf("closure exact cleanup calls = %d, want 1", closureCleaner.calls)
	}

	catalog, err := runtimeprofile.NewCatalog([]runtimeprofile.Profile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	images := &wiringImageAvailability{err: errors.New("exact image absent")}
	availability, err := runtimeprofile.NewAvailabilityChecker(database, catalog, images)
	if err != nil {
		t.Fatal(err)
	}
	if err := availability.Check(ctx); !errors.Is(err, runtimeprofile.ErrProtectedImageUnavailable) {
		t.Fatalf("protected image readiness Check() error = %v", err)
	}
	if images.calls != 1 {
		t.Fatalf("protected image readiness calls = %d, want one distinct image/platform", images.calls)
	}

	retentionCleaner := &wiringRetentionCleaner{}
	retentionWorker, err := retention.NewWorker(database, retentionCleaner, retention.WorkerConfig{
		ClaimOwner: "production-retention", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := retentionWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("retention ProcessNext() = (%t, %v)", processed, err)
	}
	if retentionCleaner.assignmentID != closureFixture.assignmentID || retentionCleaner.path != canonicalRuntimePath(closureFixture.assignmentID) {
		t.Fatalf("retention cleanup = (%q, %q)", retentionCleaner.assignmentID, retentionCleaner.path)
	}
	var runtimeState string
	if err := pool.QueryRow(ctx, `SELECT desired_runtime_state FROM workflows WHERE id = $1`, closureFixture.workflowID).Scan(&runtimeState); err != nil {
		t.Fatal(err)
	}
	if runtimeState != "COLLECTED" {
		t.Fatalf("collected Workflow runtime state = %q, want COLLECTED", runtimeState)
	}
}

type emptyStartupRuntimes struct{}

func (emptyStartupRuntimes) List(context.Context) (startup.RuntimeInventorySnapshot, error) {
	return startup.RuntimeInventorySnapshot{}, nil
}

func (emptyStartupRuntimes) EnsureAbsent(context.Context, store.AgentTurnRuntimeIdentity) error {
	return nil
}

func (emptyStartupRuntimes) EnsureMalformedAbsent(context.Context, startup.MalformedManagedRuntimeProcess) error {
	return nil
}

type wiringClosureCleaner struct {
	calls int
}

func (cleaner *wiringClosureCleaner) EnsureAbsent(context.Context, map[string]string) error {
	cleaner.calls++
	return nil
}

type noMutationReconciler struct{}

func (noMutationReconciler) Reconcile(context.Context, store.AgentTurnMutationReconciliationContext, store.MutationReservation) (mcp.MutationReconciliationResult, error) {
	return mcp.MutationReconciliationResult{}, errors.New("unexpected mutation reconciliation")
}

type wiringImageAvailability struct {
	calls int
	err   error
}

func (images *wiringImageAvailability) Available(context.Context, string, runtimeprofile.Platform) error {
	images.calls++
	return images.err
}

type wiringRetentionCleaner struct {
	assignmentID string
	path         string
}

func (cleaner *wiringRetentionCleaner) EnsureAbsent(_ context.Context, assignmentID, path string) error {
	cleaner.assignmentID = assignmentID
	cleaner.path = path
	return nil
}
