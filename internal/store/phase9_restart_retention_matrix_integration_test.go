//go:build integration

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/retention"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workflowaction"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestPhaseNinePreparationRestartBeforeAndAfterCommit(t *testing.T) {
	t.Run("before commit", func(t *testing.T) {
		databases, pool := openPhaseFiveStores(t, 1)
		database := databases[0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		application := triggerPreparationWorkflow(t, database, ctx,
			"7b000000-0000-4000-8000-000000000001", "7b000000-0000-4000-8000-000000000002")

		claimed := make(chan store.JobLease, 1)
		processCtx, stopProcess := context.WithCancel(ctx)
		worker := newPhaseNinePreparationWorker(t, database, "preparation-before-loss",
			integrationTurnPreparerFunc(func(ctx context.Context, request agentturn.Request) (agentturn.Result, error) {
				claimed <- request.Lease
				<-ctx.Done()
				return agentturn.Result{}, ctx.Err()
			}))
		result := make(chan phaseNineProcessResult, 1)
		go func() {
			processed, err := worker.ProcessNext(processCtx)
			result <- phaseNineProcessResult{processed: processed, err: err}
		}()
		lease := receivePhaseNine(t, ctx, claimed, "preparation claim")
		stopProcess()
		lost := receivePhaseNine(t, ctx, result, "stopped preparation worker")
		if !lost.processed || lost.err == nil || !strings.Contains(lost.err.Error(), context.Canceled.Error()) {
			t.Fatalf("ProcessNext() before commit loss = (%t, %v)", lost.processed, lost.err)
		}
		expirePhaseNineJob(t, pool, ctx, lease)
		if reclaimed, err := database.ReclaimExpiredJobs(ctx, 1); err != nil || reclaimed != 1 {
			t.Fatalf("ReclaimExpiredJobs() = (%d, %v), want one preparation", reclaimed, err)
		}

		replacement := newPhaseNineCommittingPreparationWorker(t, database, "preparation-before-replacement", nil)
		if processed, err := replacement.ProcessNext(ctx); err != nil || !processed {
			t.Fatalf("replacement ProcessNext() = (%t, %v)", processed, err)
		}
		assertPhaseNinePreparedOnce(t, pool, ctx, application.WorkflowID)
	})

	t.Run("after commit", func(t *testing.T) {
		databases, pool := openPhaseFiveStores(t, 1)
		database := databases[0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		application := triggerPreparationWorkflow(t, database, ctx,
			"7b000000-0000-4000-8000-000000000011", "7b000000-0000-4000-8000-000000000012")

		processCtx, stopProcess := context.WithCancel(ctx)
		worker := newPhaseNineCommittingPreparationWorker(t, database, "preparation-after-loss", stopProcess)
		processed, err := worker.ProcessNext(processCtx)
		if !processed || err != nil {
			t.Fatalf("ProcessNext() after commit loss = (%t, %v)", processed, err)
		}
		replacement := newPhaseNineCommittingPreparationWorker(t, database, "preparation-after-replacement", nil)
		if processed, err := replacement.ProcessNext(ctx); err != nil || processed {
			t.Fatalf("replacement ProcessNext() = (%t, %v), want idle", processed, err)
		}
		assertPhaseNinePreparedOnce(t, pool, ctx, application.WorkflowID)
	})
}

func TestPhaseNineActivePromptLossRetriesSameSessionInFreshRuntimeProcess(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	application := triggerPreparationWorkflow(t, database, ctx,
		"7b100000-0000-4000-8000-000000000001", "7b100000-0000-4000-8000-000000000002")
	root := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "restart-profile", "openai/restart")

	promptStarted := make(chan struct{})
	firstLauncher := &phaseNineExecutionLauncher{store: database, acpSessionID: "phase-nine-session"}
	firstPrompter := &phaseNineExecutionPrompter{prompt: func(ctx context.Context) (acp.PromptResponse, error) {
		close(promptStarted)
		<-ctx.Done()
		return acp.PromptResponse{}, ctx.Err()
	}}
	firstWorker := newPhaseNineExecutionWorker(t, database, firstLauncher, firstPrompter)
	processCtx, stopProcess := context.WithCancel(ctx)
	result := make(chan phaseNineProcessResult, 1)
	go func() {
		processed, err := firstWorker.ProcessNext(processCtx)
		result <- phaseNineProcessResult{processed: processed, err: err}
	}()
	select {
	case <-promptStarted:
	case early := <-result:
		t.Fatalf("execution worker stopped before prompting = (%t, %v)", early.processed, early.err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for active prompt: %v", ctx.Err())
	}
	stopProcess()
	lost := receivePhaseNine(t, ctx, result, "lost prompt process")
	if !lost.processed || !errors.Is(lost.err, context.Canceled) {
		t.Fatalf("first ProcessNext() = (%t, %v)", lost.processed, lost.err)
	}

	retry := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "restart-profile", "openai/restart")
	if retry.Session.ID != root.Session.ID || retry.Turn.RetryOfTurnID != root.Turn.ID {
		t.Fatalf("prepared retry = Session %s, retry of %s; want %s and %s",
			retry.Session.ID, retry.Turn.RetryOfTurnID, root.Session.ID, root.Turn.ID)
	}
	secondLauncher := &phaseNineExecutionLauncher{store: database, acpSessionID: "phase-nine-session"}
	secondWorker := newPhaseNineExecutionWorker(t, database, secondLauncher, &phaseNineExecutionPrompter{
		prompt: func(context.Context) (acp.PromptResponse, error) {
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
	})
	if processed, err := secondWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("fresh ProcessNext() = (%t, %v)", processed, err)
	}
	var acpSessionID string
	var turns, settlements int
	if err := pool.QueryRow(ctx, `SELECT acp_session_id FROM agent_sessions WHERE id = $1`, root.Session.ID).Scan(&acpSessionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_turns WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_turn_settlements WHERE workflow_id = $1)`, application.WorkflowID).Scan(&turns, &settlements); err != nil {
		t.Fatal(err)
	}
	if firstLauncher.calls != 1 || secondLauncher.calls != 1 || acpSessionID != "phase-nine-session" || turns != 2 || settlements != 2 {
		t.Fatalf("restart result = launches %d/%d, ACP %q, Turns %d, settlements %d",
			firstLauncher.calls, secondLauncher.calls, acpSessionID, turns, settlements)
	}
}

func TestPhaseNineInFlightMCPMutationRecoversAndReplaysWithoutSecondSideEffect(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	triggerPreparationWorkflow(t, database, ctx,
		"7b200000-0000-4000-8000-000000000001", "7b200000-0000-4000-8000-000000000002")
	root := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "mutation-profile", "openai/mutation")
	rootLease := acquireAndBindTurn(t, database, ctx, root, "mutation-session")
	if err := database.OpenMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, rootLease)
	if err != nil {
		t.Fatal(err)
	}
	backend := &phaseNineAmbiguousBackend{}
	rootGateway, rootRegistration := openPhaseNineGateway(t, database, backend, phaseNineGatewayScope(rootLease, execution))
	arguments := map[string]any{"operation_id": "restart-comment", "body": "durable once"}
	callReplayGatewayTool(t, rootGateway, rootRegistration, mcp.ToolCommentOnIssue, arguments, true)
	if err := rootGateway.CloseAndDrain(ctx, rootRegistration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, rootLease.JobLease.ID, rootLease.ID)
	if _, err := database.RecoverExpiredAgentTurn(ctx, rootLease.ID, rootLease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}

	stopWorker, err := agentturn.NewStopWorker(database, &phaseNineExactCleaner{}, phaseNineWorkspaceDiscarder{}, agentturn.StopWorkerConfig{
		ClaimOwner: "phase-nine-stop", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := stopWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("stop ProcessNext() = (%t, %v)", processed, err)
	}
	recoveryWorker, err := mcp.NewRecoveryWorker(database, phaseNineFoundReconciler{}, mcp.RecoveryWorkerConfig{
		ClaimOwner: "phase-nine-mutation-recovery", LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := recoveryWorker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("recovery ProcessNext() = (%t, %v)", processed, err)
	}

	var retry store.AgentTurnPreparationCommit
	preparationWorkerWithResult := newPhaseNinePreparationWorker(t, database, "mutation-retry-preparation-result",
		integrationTurnPreparerFunc(func(ctx context.Context, request agentturn.Request) (agentturn.Result, error) {
			commit, err := database.PrepareAgentTurn(ctx, request.Lease, preparationSpec("mutation-profile", "openai/mutation"))
			retry = commit
			return agentturn.Result{Commit: commit}, err
		}))
	if processed, err := preparationWorkerWithResult.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("retry preparation ProcessNext() = (%t, %v)", processed, err)
	}
	if retry.Session.ID != root.Session.ID || retry.Turn.RetryOfTurnID != root.Turn.ID {
		t.Fatalf("mutation retry = Session %s, retry of %s", retry.Session.ID, retry.Turn.RetryOfTurnID)
	}
	retryLease := acquireAndBindTurn(t, database, ctx, retry, "mutation-session")
	if err := database.OpenMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	retryExecution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil {
		t.Fatal(err)
	}
	retryGateway, retryRegistration := openPhaseNineGateway(t, database, backend, phaseNineGatewayScope(retryLease, retryExecution))
	callReplayGatewayTool(t, retryGateway, retryRegistration, mcp.ToolCommentOnIssue, arguments, false)
	if backend.executeCalls != 1 || backend.restoreCalls != 1 {
		t.Fatalf("backend calls after replay = execute %d, restore %d; want 1/1", backend.executeCalls, backend.restoreCalls)
	}
}

func TestPhaseNineSettlementCommitResponseLossDoesNotRecoverOrDuplicateSuccessor(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	application := triggerPreparationWorkflow(t, database, ctx,
		"7b300000-0000-4000-8000-000000000001", "7b300000-0000-4000-8000-000000000002")
	prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "settlement-profile", "openai/settlement")
	lossStore := &phaseNineSettlementResponseLossStore{Store: database}
	worker := newPhaseNineExecutionWorker(t, lossStore,
		&phaseNineExecutionLauncher{store: database, acpSessionID: "settlement-session"},
		&phaseNineExecutionPrompter{prompt: func(context.Context) (acp.PromptResponse, error) {
			return acp.PromptResponse{}, errors.New("runtime transport failed")
		}})
	processed, err := worker.ProcessNext(ctx)
	if !processed || !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("ProcessNext() with lost settlement response = (%t, %v)", processed, err)
	}

	replacement := newPhaseNineExecutionWorker(t, database,
		&phaseNineExecutionLauncher{store: database, acpSessionID: "unused"},
		&phaseNineExecutionPrompter{})
	if processed, err := replacement.ProcessNext(ctx); err != nil || processed {
		t.Fatalf("replacement ProcessNext() = (%t, %v), want idle", processed, err)
	}
	var settlements, successors, recoveries int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_turn_settlements WHERE workflow_id = $1),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN' AND agent_turn_settlement_id IS NOT NULL),
	       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND queue = 'agent-turn-recovery')`,
		application.WorkflowID).Scan(&settlements, &successors, &recoveries); err != nil {
		t.Fatal(err)
	}
	if settlements != 1 || successors != 1 || recoveries != 0 {
		t.Fatalf("lost response durability = settlements %d, successors %d, recoveries %d", settlements, successors, recoveries)
	}
}

func TestPhaseNineCloseActiveTurnSurvivesStopAndSettlementProcessLoss(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 704)
	prepareClosableFixture(t, pool, fixture)
	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, time.Second)
	if _, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "closing-runtime", time.Second, 1); err != nil {
		t.Fatal(err)
	}
	phaseNineCloseWorkflow(t, database, ctx, fixture, 704,
		"7b400000-0000-4000-8000-000000000001", "phase-nine-close", "phase-nine-retain", time.Now().UTC().Add(time.Hour))

	cleaner := &phaseNineExactCleaner{}
	stopConfig := phaseNineClosureWorkerConfig("closure-stop-loss")
	stopWorker, err := workflowaction.NewClosureStopWorker(&phaseNineClosureStopResponseLossStore{Store: database}, cleaner, stopConfig)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := stopWorker.ProcessNext(ctx); !processed || !errors.Is(err, store.ErrClosureSettlementFenceLost) {
		t.Fatalf("stop ProcessNext() with response loss = (%t, %v)", processed, err)
	}
	stopConfig.ClaimOwner = "closure-stop-replacement"
	replacementStop, err := workflowaction.NewClosureStopWorker(database, cleaner, stopConfig)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := replacementStop.ProcessNext(ctx); err != nil || processed {
		t.Fatalf("replacement stop ProcessNext() = (%t, %v), want idle", processed, err)
	}

	settlementConfig := phaseNineClosureWorkerConfig("closure-settlement-loss")
	settlementWorker, err := workflowaction.NewClosureSettlementWorker(
		&phaseNineClosureSettlementResponseLossStore{Store: database}, noMutationReconciler{}, settlementConfig)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := settlementWorker.ProcessNext(ctx); !processed || !errors.Is(err, store.ErrClosureSettlementFenceLost) {
		t.Fatalf("settlement ProcessNext() with response loss = (%t, %v)", processed, err)
	}
	settlementConfig.ClaimOwner = "closure-settlement-replacement"
	replacementSettlement, err := workflowaction.NewClosureSettlementWorker(database, noMutationReconciler{}, settlementConfig)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := replacementSettlement.ProcessNext(ctx); err != nil || processed {
		t.Fatalf("replacement settlement ProcessNext() = (%t, %v), want idle", processed, err)
	}
	var workflowState string
	var stopJobs, settlementJobs, collectionJobs int
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&workflowState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind = 'STOP_AGENT_TURN' AND status = 'SUCCEEDED'),
       count(*) FILTER (WHERE kind = 'SETTLE_CLOSURE' AND status = 'SUCCEEDED'),
       count(*) FILTER (WHERE kind = 'COLLECT_ASSIGNMENTS')
FROM jobs WHERE workflow_id = $1`, fixture.workflowID).Scan(&stopJobs, &settlementJobs, &collectionJobs); err != nil {
		t.Fatal(err)
	}
	if cleaner.calls != 1 || workflowState != string(workflow.StateClosed) || stopJobs != 1 || settlementJobs != 1 || collectionJobs != 1 {
		t.Fatalf("closure restart result = cleanup %d, Workflow %s, Jobs %d/%d/%d",
			cleaner.calls, workflowState, stopJobs, settlementJobs, collectionJobs)
	}
}

func TestPhaseNineRetentionFinalizationResponseLossIsResolvedAsCollected(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 705)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	prepareClosableFixture(t, pool, fixture)
	observedAt := time.Now().UTC().Add(-2 * time.Hour)
	closeAndSettleWithoutTurn(t, database, ctx, fixture, 705,
		"7b500000-0000-4000-8000-000000000001", "retention-loss-close", "retention-loss-token",
		observedAt, observedAt.Add(time.Hour))
	cleaner := &integrationRetentionCleaner{}
	worker := newPhaseNineRetentionWorker(t, &phaseNineRetentionResponseLossStore{Store: database}, cleaner, "retention-loss")
	if processed, err := worker.ProcessNext(ctx); !processed || !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("ProcessNext() with finalization response loss = (%t, %v)", processed, err)
	}
	replacement := newPhaseNineRetentionWorker(t, database, cleaner, "retention-loss-replacement")
	if processed, err := replacement.ProcessNext(ctx); err != nil || processed {
		t.Fatalf("replacement ProcessNext() = (%t, %v), want idle", processed, err)
	}
	var runtimeState, generationState, jobState string
	if err := pool.QueryRow(ctx, `SELECT desired_runtime_state FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&runtimeState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT generation.status, job.status
FROM assignment_retention_generations AS generation
JOIN jobs AS job ON job.id = generation.collection_job_id
WHERE generation.workflow_id = $1`, fixture.workflowID).Scan(&generationState, &jobState); err != nil {
		t.Fatal(err)
	}
	if runtimeState != string(workflow.RuntimeStateCollected) || generationState != "COLLECTED" || jobState != string(store.JobSucceeded) || len(cleaner.callsSnapshot()) != 1 {
		t.Fatalf("resolved collection = runtime %s, generation %s, Job %s, cleanup calls %d",
			runtimeState, generationState, jobState, len(cleaner.callsSnapshot()))
	}
}

func TestPhaseNineCollectionRetainsPostgreSQLHistoryAndProductionPreparationCreatesFreshGeneration(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 706)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	prepareClosableFixture(t, pool, fixture)
	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	executionJob := claimAgentTurnJob(t, database, ctx, turn, time.Second)
	lease, err := database.AcquireAgentTurn(ctx, executionJob, turn.ControlRevision, "history-runtime", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "history-comment", ToolName: mcp.ToolCommentOnIssue,
		Request: json.RawMessage(`{"operation_id":"history-comment","body":"retained"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"comment_id":706}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "test terminal"}); err != nil {
		t.Fatal(err)
	}
	preparationBinding := preparationBindings()[workflow.RoleDeveloper]
	pinnedBinding := runtimeprofile.Binding{
		Name: preparationBinding.RuntimeProfileName, Version: preparationBinding.RuntimeProfileVersion,
		ContentSHA256: preparationBinding.RuntimeProfileContentSHA256, Image: preparationBinding.RuntimeImageDigest,
	}
	setFixtureRuntimeBinding(t, pool, fixture, pinnedBinding, pinnedBinding)
	observedAt := time.Now().UTC().Add(-2 * time.Hour)
	closeAndSettleWithoutTurn(t, database, ctx, fixture, 706,
		"7b600000-0000-4000-8000-000000000001", "history-close", "history-retention",
		observedAt, observedAt.Add(time.Hour))
	collector := newPhaseNineRetentionWorker(t, database, &integrationRetentionCleaner{}, "history-collector")
	if processed, err := collector.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("collection ProcessNext() = (%t, %v)", processed, err)
	}

	applyReopen(t, database, ctx, fixture, 706, "7b600000-0000-4000-8000-000000000002")
	applyTrigger(t, database, ctx, fixture, 706,
		"7b600000-0000-4000-8000-000000000003", "7b600000-0000-4000-8000-000000000004")
	var fresh store.AgentTurnPreparationCommit
	preparer := newPhaseNinePreparationWorker(t, database, "history-preparer",
		integrationTurnPreparerFunc(func(ctx context.Context, request agentturn.Request) (agentturn.Result, error) {
			commit, err := database.PrepareAgentTurn(ctx, request.Lease, preparationSpec("history-fresh", "openai/fresh"))
			fresh = commit
			return agentturn.Result{Commit: commit}, err
		}))
	if processed, err := preparer.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("fresh preparation ProcessNext() = (%t, %v)", processed, err)
	}
	var assignments, sessions, turns, mutations, executionJobs, executionAttempts int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_assignments WHERE id = $1),
       (SELECT count(*) FROM agent_sessions WHERE id = $2),
       (SELECT count(*) FROM agent_turns WHERE id = $3),
       (SELECT count(*) FROM tool_invocations WHERE id = $4),
       (SELECT count(*) FROM jobs WHERE id = $5),
       (SELECT count(*) FROM job_attempts WHERE job_id = $5)`,
		fixture.assignmentID, fixture.sessionID, turn.ID, mutation.ID, executionJob.ID).Scan(
		&assignments, &sessions, &turns, &mutations, &executionJobs, &executionAttempts); err != nil {
		t.Fatal(err)
	}
	if assignments != 1 || sessions != 1 || turns != 1 || mutations != 1 || executionJobs != 1 || executionAttempts != 1 ||
		fresh.Assignment.ID == fixture.assignmentID || fresh.Assignment.Generation != 2 || fresh.Session.ID == fixture.sessionID {
		t.Fatalf("retained history = rows %d/%d/%d/%d/%d/%d, fresh Assignment %s generation %d Session %s",
			assignments, sessions, turns, mutations, executionJobs, executionAttempts,
			fresh.Assignment.ID, fresh.Assignment.Generation, fresh.Session.ID)
	}
}

func TestPhaseNineHistoricalImageRemainsProtectedUntilCollectionFinalization(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 707)
	makeFixtureRuntimePathCanonical(t, pool, fixture)
	profile, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:"+strings.Repeat("7", 64),
		runtimeprofile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	binding := profile.Binding()
	setFixtureRuntimeBinding(t, pool, fixture, binding, binding)
	prepareClosableFixture(t, pool, fixture)
	observedAt := time.Now().UTC().Add(-2 * time.Hour)
	closeAndSettleWithoutTurn(t, database, ctx, fixture, 707,
		"7b700000-0000-4000-8000-000000000001", "image-close", "image-retention",
		observedAt, observedAt.Add(time.Hour))

	cleaner := newPhaseNineBlockingRetentionCleaner()
	worker := newPhaseNineRetentionWorker(t, database, cleaner, "image-collector")
	result := make(chan phaseNineProcessResult, 1)
	go func() {
		processed, err := worker.ProcessNext(ctx)
		result <- phaseNineProcessResult{processed: processed, err: err}
	}()
	receivePhaseNine(t, ctx, cleaner.deleted, "physical runtime-state deletion")
	catalog, err := runtimeprofile.NewCatalog([]runtimeprofile.Profile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	images := &phaseNineImageAvailability{}
	checker, err := runtimeprofile.NewAvailabilityChecker(database, catalog, images)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx); err != nil {
		t.Fatalf("Check() before collection finalization error = %v", err)
	}
	if images.calls != 1 {
		t.Fatalf("protected image checks before finalization = %d, want 1", images.calls)
	}
	close(cleaner.release)
	completed := receivePhaseNine(t, ctx, result, "collection finalization")
	if completed.err != nil || !completed.processed {
		t.Fatalf("collection ProcessNext() = (%t, %v)", completed.processed, completed.err)
	}
	if err := checker.Check(ctx); err != nil {
		t.Fatalf("Check() after collection finalization error = %v", err)
	}
	if images.calls != 1 {
		t.Fatalf("protected image checks after finalization = %d, want no additional check", images.calls)
	}
}

type phaseNineProcessResult struct {
	processed bool
	err       error
}

func receivePhaseNine[T any](t *testing.T, ctx context.Context, values <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-ctx.Done():
		var zero T
		t.Fatalf("timed out waiting for %s: %v", description, ctx.Err())
		return zero
	}
}

func newPhaseNinePreparationWorker(t *testing.T, workerStore agentturn.WorkerStore, owner string, preparer agentturn.TurnPreparer) *agentturn.Worker {
	t.Helper()
	worker, err := agentturn.NewWorker(workerStore,
		&integrationCredentialProvider{credential: "developer-token"},
		&integrationCredentialProvider{credential: "reviewer-token"}, preparer,
		agentturn.WorkerConfig{
			ClaimOwner: owner, LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
			IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
		})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func newPhaseNineCommittingPreparationWorker(t *testing.T, database *store.Store, owner string, afterCommit func()) *agentturn.Worker {
	t.Helper()
	return newPhaseNinePreparationWorker(t, database, owner,
		integrationTurnPreparerFunc(func(ctx context.Context, request agentturn.Request) (agentturn.Result, error) {
			commit, err := database.PrepareAgentTurn(ctx, request.Lease, preparationSpec("restart-profile", "openai/restart"))
			if err != nil {
				return agentturn.Result{}, err
			}
			if afterCommit != nil {
				afterCommit()
			}
			return agentturn.Result{Commit: commit}, nil
		}))
}

func expirePhaseNineJob(t *testing.T, pool *pgxpool.Pool, ctx context.Context, lease store.JobLease) {
	t.Helper()
	for _, query := range []string{
		`UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`,
		`UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1 AND attempt_number = $2`,
	} {
		args := []any{lease.ID}
		if strings.Contains(query, "attempt_number") {
			args = append(args, lease.Attempt)
		}
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("expire phase-nine Job: %v", err)
		}
	}
}

func assertPhaseNinePreparedOnce(t *testing.T, pool *pgxpool.Pool, ctx context.Context, workflowID string) {
	t.Helper()
	var assignments, sessions, turns, executionJobs int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_sessions AS session JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id WHERE assignment.workflow_id = $1),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN')`, workflowID).Scan(
		&assignments, &sessions, &turns, &executionJobs); err != nil {
		t.Fatal(err)
	}
	if assignments != 2 || sessions != 1 || turns != 1 || executionJobs != 1 {
		t.Fatalf("durable preparation rows = Assignments %d, Sessions %d, Turns %d, execution Jobs %d",
			assignments, sessions, turns, executionJobs)
	}
}

type phaseNineExecutionLauncher struct {
	store        *store.Store
	acpSessionID string
	calls        int
}

func (launcher *phaseNineExecutionLauncher) LaunchExecution(ctx context.Context, request agentturn.LaunchRequest) (agentturn.ExecutionRuntime, error) {
	if _, err := launcher.store.BindAgentSessionACP(ctx, request.Lease, launcher.acpSessionID, json.RawMessage(`{"resume":true}`)); err != nil {
		return nil, err
	}
	launcher.calls++
	return &phaseNineExecutionRuntime{lease: request.Lease, client: &phaseNinePromptClient{}}, nil
}

type phaseNineExecutionRuntime struct {
	lease  store.AgentTurnLease
	client session.PromptClient
}

func (runtime *phaseNineExecutionRuntime) CurrentLease() store.AgentTurnLease { return runtime.lease }
func (runtime *phaseNineExecutionRuntime) PromptClient() session.PromptClient { return runtime.client }
func (*phaseNineExecutionRuntime) RenewMCP(time.Time) bool                    { return true }
func (*phaseNineExecutionRuntime) CloseMCP(context.Context) error             { return nil }
func (*phaseNineExecutionRuntime) Cleanup(context.Context) error              { return nil }

type phaseNinePromptClient struct{}

func (*phaseNinePromptClient) SetAgentEventContext(agentevent.Context) {}
func (*phaseNinePromptClient) Prompt(context.Context, string, []acp.ContentBlock) (acp.PromptResponse, error) {
	return acp.PromptResponse{}, errors.New("prompt must pass through the execution prompter")
}

type phaseNineExecutionPrompter struct {
	prompt func(context.Context) (acp.PromptResponse, error)
}

func (prompter *phaseNineExecutionPrompter) Prompt(ctx context.Context, _ session.PromptRequest) (acp.PromptResponse, error) {
	if prompter.prompt == nil {
		return acp.PromptResponse{}, errors.New("unexpected prompt")
	}
	return prompter.prompt(ctx)
}

type phaseNineDefaultBranch struct{}

func (phaseNineDefaultBranch) ResolveDefaultBranch(context.Context, string, string, string) (githubapi.DefaultBranch, error) {
	return githubapi.DefaultBranch{Name: "main", CommitSHA: strings.Repeat("1", 40)}, nil
}

type phaseNineWorkspace struct {
	paths workspace.Paths
}

func (resolver phaseNineWorkspace) Paths(string) (workspace.Paths, error) { return resolver.paths, nil }

type phaseNineOutcomeReconciler struct{}

func (phaseNineOutcomeReconciler) Reconcile(_ context.Context, request agentturn.OutcomeReconciliation) (store.AgentTurnSettlementObservation, error) {
	if request.PromptResponse == nil {
		return failedSettlementObservation("orchestrator process lost"), nil
	}
	return successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil), nil
}

func newPhaseNineExecutionWorker(t *testing.T, workerStore agentturn.ExecutionWorkerStore, launcher agentturn.ExecutionLauncher, prompter agentturn.ExecutionPrompter) *agentturn.ExecutionWorker {
	t.Helper()
	credential := &integrationCredentialProvider{credential: "repository-token"}
	worker, err := agentturn.NewExecutionWorker(agentturn.ExecutionWorkerDependencies{
		Store: workerStore, DeveloperCredentials: credential, ReviewerCredentials: credential,
		DefaultBranch: phaseNineDefaultBranch{}, Launcher: launcher, Sessions: prompter,
		Outcomes: phaseNineOutcomeReconciler{}, Workspace: phaseNineWorkspace{paths: workspace.Paths{Workspace: t.TempDir(), Publication: t.TempDir()}},
	}, agentturn.ExecutionWorkerConfig{
		ClaimOwner: "phase-nine-execution", LeaseDuration: 2 * time.Second, HeartbeatInterval: 200 * time.Millisecond,
		IdlePollInterval: time.Millisecond, TurnTimeout: 5 * time.Second, CleanupTimeout: 100 * time.Millisecond,
		ConcurrencyLimit: 1, DeveloperProviderCredentialJSON: json.RawMessage(`{"token":"developer"}`),
		ReviewerProviderCredentialJSON: json.RawMessage(`{"token":"reviewer"}`), GitRemoteBaseURL: "https://github.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

type phaseNineSettlementResponseLossStore struct {
	*store.Store
	mutex sync.Mutex
	lost  bool
}

func (database *phaseNineSettlementResponseLossStore) SettleAgentTurn(ctx context.Context, lease store.AgentTurnLease, observation store.AgentTurnSettlementObservation) (store.AgentTurnSettlement, error) {
	settlement, err := database.Store.SettleAgentTurn(ctx, lease, observation)
	if err != nil {
		return settlement, err
	}
	database.mutex.Lock()
	defer database.mutex.Unlock()
	if !database.lost {
		database.lost = true
		return store.AgentTurnSettlement{}, store.ErrAgentTurnFenceLost
	}
	return settlement, nil
}

type phaseNineAmbiguousBackend struct {
	executeCalls int
	restoreCalls int
}

func (backend *phaseNineAmbiguousBackend) Execute(context.Context, mcp.Invocation) (json.RawMessage, error) {
	backend.executeCalls++
	return nil, mcp.OutcomeUnknown(errors.New("response lost after side effect"))
}

func (backend *phaseNineAmbiguousBackend) RestoreMutationReplay(context.Context, mcp.Invocation, store.MutationReservation) error {
	backend.restoreCalls++
	return nil
}

func openPhaseNineGateway(t *testing.T, database mcp.Store, backend mcp.Backend, scope mcp.TokenScope) (*mcp.Gateway, mcp.Registration) {
	t.Helper()
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: database, Backend: backend,
		Now:                        func() time.Time { return scope.Lease.CreatedAt.Add(time.Millisecond) },
		MutationFenceCheckInterval: time.Hour, Random: bytes.NewReader(bytes.Repeat([]byte{42}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	replayGatewayRPC(t, gateway, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+mcp.ProtocolVersion+`","capabilities":{},"clientInfo":{"name":"phase-nine","version":"1"}}}`)
	replayGatewayRPC(t, gateway, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	return gateway, registration
}

func phaseNineGatewayScope(lease store.AgentTurnLease, execution store.AgentTurnExecutionContext) mcp.TokenScope {
	return mcp.TokenScope{
		Lease: lease, WorkflowID: execution.WorkflowID, Role: execution.Assignment.Role,
		Repository: mcp.RepositoryScope{ID: execution.Repository.ID, Owner: execution.Repository.Owner, Name: execution.Repository.Name},
		Issue:      mcp.IssueScope{ID: execution.Issue.ID, Number: execution.Issue.Number},
		Branch:     "omnigrex/issue-12", DefaultBranch: "main", HeadSHA: strings.Repeat("1", 40),
		ExpiresAt: lease.LeaseExpiresAt.Add(-time.Millisecond),
	}
}

type phaseNineFoundReconciler struct{}

func (phaseNineFoundReconciler) Reconcile(context.Context, store.AgentTurnMutationReconciliationContext, store.MutationReservation) (mcp.MutationReconciliationResult, error) {
	return mcp.MutationReconciliationResult{
		Disposition: mcp.ReconciliationFound,
		Outcome:     store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: json.RawMessage(`{"comment_id":720}`)},
	}, nil
}

type phaseNineExactCleaner struct {
	calls int
}

func (cleaner *phaseNineExactCleaner) EnsureAbsent(context.Context, map[string]string) error {
	cleaner.calls++
	return nil
}

type phaseNineWorkspaceDiscarder struct{}

func (phaseNineWorkspaceDiscarder) DiscardWorkspace(string) error { return nil }

type phaseNineClosureStopResponseLossStore struct {
	*store.Store
}

func (database *phaseNineClosureStopResponseLossStore) AcknowledgeClosureTurnStopped(ctx context.Context, lease store.JobLease) (store.ClosureSettlement, error) {
	settlement, err := database.Store.AcknowledgeClosureTurnStopped(ctx, lease)
	if err != nil {
		return settlement, err
	}
	return store.ClosureSettlement{}, store.ErrClosureSettlementFenceLost
}

type phaseNineClosureSettlementResponseLossStore struct {
	*store.Store
}

func (database *phaseNineClosureSettlementResponseLossStore) CompleteClosureSettlement(ctx context.Context, lease store.JobLease) (store.ClosureSettlement, error) {
	settlement, err := database.Store.CompleteClosureSettlement(ctx, lease)
	if err != nil {
		return settlement, err
	}
	return store.ClosureSettlement{}, store.ErrClosureSettlementFenceLost
}

func phaseNineClosureWorkerConfig(owner string) workflowaction.ClosureWorkerConfig {
	return workflowaction.ClosureWorkerConfig{
		ClaimOwner: owner, LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	}
}

func phaseNineCloseWorkflow(t *testing.T, database *store.Store, ctx context.Context, fixture agentFixture, number int, deliveryID, closureID, retentionToken string, retainUntil time.Time) {
	t.Helper()
	delivery := workflowDelivery(deliveryID)
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(number), int64(number), int64(number)
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"),
		store.WorkflowLocator{RepositoryID: int64(number), IssueID: int64(number), IssueNumber: int64(number)},
		func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{EventMetadata: workflow.EventMetadata{
				ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt, WorkItem: snapshot.WorkItem,
				ExpectedRevision: snapshot.Revision,
			}, ClosureID: closureID, RetainUntil: retainUntil, RetentionToken: retentionToken})
		})
	if err != nil || application.State != workflow.StateClosing {
		t.Fatalf("close Workflow = (%#v, %v)", application, err)
	}
}

type phaseNineRetentionResponseLossStore struct {
	*store.Store
}

func (database *phaseNineRetentionResponseLossStore) FinalizeAssignmentCollection(ctx context.Context, lease store.JobLease, targets []store.AssignmentCleanupTarget) (store.AssignmentCollection, error) {
	collection, err := database.Store.FinalizeAssignmentCollection(ctx, lease, targets)
	if err != nil {
		return collection, err
	}
	return store.AssignmentCollection{}, store.ErrAssignmentCollectionFenceLost
}

func newPhaseNineRetentionWorker(t *testing.T, workerStore retention.WorkerStore, cleaner retention.Cleaner, owner string) *retention.Worker {
	t.Helper()
	worker, err := retention.NewWorker(workerStore, cleaner, retention.WorkerConfig{
		ClaimOwner: owner, LeaseDuration: time.Second, HeartbeatInterval: 100 * time.Millisecond,
		IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

type phaseNineBlockingRetentionCleaner struct {
	deleted chan struct{}
	release chan struct{}
	once    sync.Once
}

func newPhaseNineBlockingRetentionCleaner() *phaseNineBlockingRetentionCleaner {
	return &phaseNineBlockingRetentionCleaner{deleted: make(chan struct{}), release: make(chan struct{})}
}

func (cleaner *phaseNineBlockingRetentionCleaner) EnsureAbsent(ctx context.Context, _, _ string) error {
	cleaner.once.Do(func() { close(cleaner.deleted) })
	select {
	case <-cleaner.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type phaseNineImageAvailability struct {
	calls int
}

func (images *phaseNineImageAvailability) Available(context.Context, string, runtimeprofile.Platform) error {
	images.calls++
	return nil
}
