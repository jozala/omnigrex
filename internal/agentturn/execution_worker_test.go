package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

const (
	executionDeveloperRepositoryCredential = "developer-repository-secret"
	executionReviewerRepositoryCredential  = "reviewer-repository-secret"
	executionDeveloperProviderSecret       = "developer-provider-secret"
	executionReviewerProviderSecret        = "reviewer-provider-secret"
)

func TestExecutionWorkerSuccessSequenceAndDelegatedBlockedSettlement(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.outcomes.observation = executionObservation(workflow.TurnOutcomeBlocked, store.AgentTurnSucceeded)
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want processed success", processed, err)
	}
	want := []string{
		"claim", "heartbeat", "execution", "paths", "developer-credential", "default-branch", "launch",
		"execution", "open-admission", "prompt", "close-admission", "mcp-drain",
		"list-unsettled", "cleanup", "reconcile", "settle",
	}
	if got := fixture.operations.values(); !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
	if fixture.store.settled.Outcome != workflow.TurnOutcomeBlocked {
		t.Fatalf("settled observation = %#v", fixture.store.settled)
	}
	if fixture.outcomes.request.PromptResponse == nil || fixture.outcomes.request.PromptError != "" {
		t.Fatal("outcome reconciliation did not receive the prompt response")
	}
	content := fixture.prompter.request.Content
	if len(content) != 1 || content[0].Type != "text" || !json.Valid([]byte(content[0].Text)) {
		t.Fatalf("prompt content = %#v, want one JSON event envelope", content)
	}
}

func TestExecutionWorkerDelegatesInfrastructureSettlementObservation(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	promptErr := errors.New("ACP transport unavailable")
	fixture.prompter.err = promptErr
	fixture.outcomes.observation = executionObservation(workflow.TurnOutcomeInfrastructureFailed, store.AgentTurnFailed)
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, promptErr) {
		t.Fatalf("ProcessNext() = (%t, %v), want settled prompt failure", processed, err)
	}
	if fixture.outcomes.request.PromptResponse != nil || fixture.outcomes.request.PromptError != agentturn.PromptErrorFailure {
		t.Fatal("outcome reconciliation did not receive an infrastructure prompt failure")
	}
	if fixture.store.settled.Outcome != workflow.TurnOutcomeInfrastructureFailed || fixture.store.settled.Completion.Status != store.AgentTurnFailed {
		t.Fatalf("settled observation = %#v", fixture.store.settled)
	}
}

func TestExecutionWorkerHeartbeatLossCancelsPromptAndOnlyCleansRuntime(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.store.heartbeatErrAt = 2
	fixture.store.heartbeatErr = store.ErrAgentTurnFenceLost
	fixture.config.HeartbeatInterval = time.Millisecond
	fixture.prompter.prompt = func(ctx context.Context) (acp.PromptResponse, error) {
		<-ctx.Done()
		return acp.PromptResponse{}, ctx.Err()
	}
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want heartbeat fence loss", processed, err)
	}
	operations := fixture.operations.values()
	if !containsInOrder(operations, "prompt", "cleanup") {
		t.Fatalf("operations = %v, want prompt cancellation followed by cleanup", operations)
	}
	for _, forbidden := range []string{"close-admission", "list-unsettled", "reconcile", "settle", "recover"} {
		if contains(operations, forbidden) {
			t.Fatalf("operations = %v, fence loss must not perform %s", operations, forbidden)
		}
	}
}

func TestExecutionWorkerOperationFenceLossOnlyCleansRuntime(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.store.openErr = store.ErrAgentTurnFenceLost
	worker := fixture.worker(t)

	_, err := worker.ProcessNext(context.Background())
	if !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("ProcessNext() error = %v, want fence loss", err)
	}
	operations := fixture.operations.values()
	if !contains(operations, "cleanup") || contains(operations, "close-admission") || contains(operations, "settle") {
		t.Fatalf("operations after fence loss = %v", operations)
	}
}

func TestExecutionWorkerHandsAnyUnsettledMutationToRecovery(t *testing.T) {
	for _, state := range []store.MutationState{store.MutationUnknown, store.MutationReconciling} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
			fixture.store.unsettled = []store.MutationReservation{{ID: "mutation", State: state}}
			worker := fixture.worker(t)

			processed, err := worker.ProcessNext(context.Background())
			if err != nil || !processed {
				t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
			}
			operations := fixture.operations.values()
			if !containsInOrder(operations, "list-unsettled", "cleanup", "recover") || contains(operations, "reconcile") || contains(operations, "settle") {
				t.Fatalf("recovery handoff operations = %v", operations)
			}
		})
	}
}

func TestExecutionWorkerDoesNotSettleOrHandoffResidualInFlightMutation(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.config.CleanupTimeout = time.Millisecond
	fixture.config.HeartbeatInterval = 100 * time.Microsecond
	fixture.store.unsettledResults = [][]store.MutationReservation{
		{{ID: "mutation", State: store.MutationInFlight}},
		{{ID: "mutation", State: store.MutationReserved}},
		nil,
	}
	worker := fixture.worker(t)

	_, err := worker.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "list-unsettled") != 3 || countOperation(operations, "heartbeat") < 2 ||
		!containsInOrder(operations, "list-unsettled", "list-unsettled", "list-unsettled", "cleanup", "settle") {
		t.Fatalf("residual mutation wait operations = %v", operations)
	}
	if contains(operations, "recover") {
		t.Fatalf("terminally drained mutation unexpectedly entered recovery: %v", operations)
	}
}

func TestExecutionWorkerCleanupFailureHandsZeroMutationTurnToStopRecovery(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	cleanupErr := errors.New("Runtime Process removal unproven")
	fixture.runtime.cleanupResults = []error{cleanupErr, cleanupErr}
	fixture.config.CleanupTimeout = time.Millisecond
	fixture.config.HeartbeatInterval = 10 * time.Microsecond
	fixture.store.recoveryResults = []error{context.DeadlineExceeded, nil}
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, cleanupErr) {
		t.Fatalf("ProcessNext() = (%t, %v), want controlled cleanup recovery", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "cleanup") != 2 || countOperation(operations, "recover") != 2 || countOperation(operations, "heartbeat") < 2 ||
		!containsInOrder(operations, "list-unsettled", "cleanup", "cleanup", "recover", "recover") {
		t.Fatalf("cleanup recovery operations = %v", operations)
	}
	if contains(operations, "reconcile") || contains(operations, "settle") {
		t.Fatalf("cleanup failure directly reconciled or settled: %v", operations)
	}
}

func TestExecutionWorkerCleanupRetryCanProveAbsenceAndSettle(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	cleanupErr := errors.New("temporary Runtime Process cleanup failure")
	fixture.runtime.cleanupResults = []error{cleanupErr, nil}
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want normal settlement after cleanup retry", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "cleanup") != 2 || contains(operations, "recover") || !contains(operations, "settle") {
		t.Fatalf("cleanup retry operations = %v", operations)
	}
}

func TestExecutionWorkerHeartbeatLossDuringDrainFencesFinalization(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.config.CleanupTimeout = time.Second
	fixture.config.HeartbeatInterval = 100 * time.Microsecond
	fixture.store.heartbeatErrAfterOperation = "mcp-drain"
	fixture.store.heartbeatErr = store.ErrAgentTurnFenceLost
	fixture.runtime.drainWaits = 1
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want heartbeat fence loss", processed, err)
	}
	operations := fixture.operations.values()
	if !containsInOrder(operations, "close-admission", "mcp-drain", "heartbeat", "cleanup") {
		t.Fatalf("drain fence-loss operations = %v", operations)
	}
	for _, forbidden := range []string{"list-unsettled", "recover", "reconcile", "settle"} {
		if contains(operations, forbidden) {
			t.Fatalf("drain fence loss performed %s: %v", forbidden, operations)
		}
	}
}

func TestExecutionWorkerMapsPromptDeadlineAndCancellation(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*executionWorkerFixture)
		wantError  error
		wantClass  agentturn.PromptErrorClassification
		wantStatus store.AgentTurnStatus
	}{
		{
			name: "deadline", wantError: context.DeadlineExceeded, wantClass: agentturn.PromptErrorDeadline, wantStatus: store.AgentTurnTimedOut,
			configure: func(fixture *executionWorkerFixture) {
				fixture.config.TurnTimeout = time.Millisecond
				fixture.prompter.prompt = waitForPromptCancellation
			},
		},
		{
			name: "cancellation", wantError: context.Canceled, wantClass: agentturn.PromptErrorCancellation, wantStatus: store.AgentTurnInterrupted,
			configure: func(fixture *executionWorkerFixture) {
				fixture.prompter.prompt = func(ctx context.Context) (acp.PromptResponse, error) {
					return acp.PromptResponse{}, context.Canceled
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
			test.configure(fixture)
			fixture.outcomes.reconcile = func(request agentturn.OutcomeReconciliation) store.AgentTurnSettlementObservation {
				return executionObservation(workflow.TurnOutcomeInfrastructureFailed, mapPromptClassToStatus(request.PromptError))
			}
			worker := fixture.worker(t)

			_, err := worker.ProcessNext(context.Background())
			if !errors.Is(err, test.wantError) {
				t.Fatalf("ProcessNext() error = %v, want %v", err, test.wantError)
			}
			if fixture.outcomes.request.PromptError != test.wantClass || fixture.store.settled.Completion.Status != test.wantStatus {
				t.Fatalf("prompt class = %s, settled status = %s", fixture.outcomes.request.PromptError, fixture.store.settled.Completion.Status)
			}
		})
	}
}

func TestExecutionWorkerSettlesParentCancellationWhileHeartbeatRemainsLive(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.prompter.prompt = func(promptCtx context.Context) (acp.PromptResponse, error) {
		cancel()
		<-promptCtx.Done()
		return acp.PromptResponse{}, promptCtx.Err()
	}
	fixture.outcomes.reconcile = func(request agentturn.OutcomeReconciliation) store.AgentTurnSettlementObservation {
		return executionObservation(workflow.TurnOutcomeInfrastructureFailed, mapPromptClassToStatus(request.PromptError))
	}
	worker := fixture.worker(t)

	_, err := worker.ProcessNext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessNext() error = %v, want cancellation", err)
	}
	if fixture.outcomes.request.PromptError != agentturn.PromptErrorCancellation || fixture.store.settled.Completion.Status != store.AgentTurnInterrupted {
		t.Fatalf("cancellation reconciliation = class %s, status %s", fixture.outcomes.request.PromptError, fixture.store.settled.Completion.Status)
	}
	if !containsInOrder(fixture.operations.values(), "prompt", "cleanup", "reconcile", "settle") {
		t.Fatalf("cancellation finalization operations = %v", fixture.operations.values())
	}
}

func TestExecutionWorkerParentCancellationStillDrainsBeforeSettlement(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.config.CleanupTimeout = time.Millisecond
	fixture.config.HeartbeatInterval = 100 * time.Microsecond
	fixture.runtime.drainWaits = 1
	fixture.prompter.prompt = func(promptCtx context.Context) (acp.PromptResponse, error) {
		cancel()
		<-promptCtx.Done()
		return acp.PromptResponse{}, promptCtx.Err()
	}
	fixture.outcomes.reconcile = func(request agentturn.OutcomeReconciliation) store.AgentTurnSettlementObservation {
		return executionObservation(workflow.TurnOutcomeInfrastructureFailed, mapPromptClassToStatus(request.PromptError))
	}
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(ctx)
	if !processed || !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessNext() = (%t, %v), want settled parent cancellation", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "mcp-drain") != 2 || countOperation(operations, "heartbeat") < 2 ||
		!containsInOrder(operations, "prompt", "close-admission", "mcp-drain", "mcp-drain", "list-unsettled", "cleanup", "settle") {
		t.Fatalf("parent-canceled drain operations = %v", operations)
	}
}

func TestExecutionWorkerRetriesDrainTimeoutUntilAdmittedMutationCompletes(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.config.CleanupTimeout = time.Millisecond
	fixture.config.HeartbeatInterval = 100 * time.Microsecond
	fixture.runtime.drainWaits = 1
	worker := fixture.worker(t)

	_, err := worker.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	operations := fixture.operations.values()
	if !containsInOrder(operations, "close-admission", "mcp-drain", "heartbeat", "mcp-drain", "list-unsettled", "cleanup", "reconcile", "settle") {
		t.Fatalf("finalization operations = %v", operations)
	}
	if fixture.runtime.drainCallCount() != 2 {
		t.Fatalf("CloseMCP() calls = %d, want 2", fixture.runtime.drainCallCount())
	}
}

func TestExecutionWorkerUnresolvedDrainImmediatelyEntersControlledRecovery(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.runtime.drainResults = []error{mcp.ErrMutationDrainUnresolved}
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want controlled recovery", processed, err)
	}
	operations := fixture.operations.values()
	if fixture.runtime.drainCallCount() != 1 || !containsInOrder(operations, "close-admission", "mcp-drain", "list-unsettled", "cleanup", "recover") {
		t.Fatalf("unresolved drain operations = %v", operations)
	}
	if contains(operations, "reconcile") || contains(operations, "settle") {
		t.Fatalf("unresolved drain bypassed recovery: %v", operations)
	}
}

func TestExecutionWorkerRepairsTransientResidualMutationsBeforeRecovery(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	transient := errors.New("temporary mutation finalization failure")
	fixture.runtime.drainResults = []error{mcp.ErrMutationDrainUnresolved}
	fixture.store.unsettledResults = [][]store.MutationReservation{
		{
			{ID: "reserved", State: store.MutationReserved},
			{ID: "in-flight", State: store.MutationInFlight},
		},
		{
			{ID: "reserved", State: store.MutationReserved},
			{ID: "in-flight", State: store.MutationUnknown},
		},
	}
	fixture.store.failMutationResults = []error{transient, nil}
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want repaired recovery", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "fail-mutation") != 2 || countOperation(operations, "mark-unknown") != 1 ||
		!containsInOrder(operations, "mcp-drain", "list-unsettled", "fail-mutation", "mark-unknown", "list-unsettled", "fail-mutation", "cleanup", "recover") {
		t.Fatalf("residual mutation repair operations = %v", operations)
	}
	if contains(operations, "reconcile") || contains(operations, "settle") {
		t.Fatalf("UNKNOWN mutation bypassed mutation recovery: %v", operations)
	}
}

func TestExecutionWorkerRetriesTransientReconciliationBeforeSettlement(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	transient := errors.New("temporary outcome observation failure")
	fixture.outcomes.results = []error{transient, nil}
	fixture.config.CleanupTimeout = time.Millisecond
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want settlement after reconciliation retry", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "reconcile") != 2 || countOperation(operations, "settle") != 1 ||
		contains(operations, "recover") {
		t.Fatalf("transient reconciliation operations = %v", operations)
	}
	if fixture.outcomes.boundedCalls != 2 {
		t.Fatalf("timeout-bounded reconciliation calls = %d, want 2", fixture.outcomes.boundedCalls)
	}
}

func TestExecutionWorkerRetriesTransientSettlementWithoutDuplicateSuccessor(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	transient := errors.New("temporary settlement failure")
	fixture.store.settleResults = []error{transient, nil}
	fixture.config.CleanupTimeout = time.Millisecond
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want settlement after retry", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "settle") != 2 || contains(operations, "recover") {
		t.Fatalf("transient settlement operations = %v", operations)
	}
	active, settlements, recoveries, successors, bounded := fixture.store.finalizationState()
	if active || settlements != 1 || recoveries != 0 || successors != 1 || bounded != 2 {
		t.Fatalf("transient settlement state = active %t, settlements %d, recoveries %d, successors %d, bounded calls %d",
			active, settlements, recoveries, successors, bounded)
	}
}

func TestExecutionWorkerExhaustedReconciliationEntersControlledRecovery(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	persistent := errors.New("persistent outcome observation failure")
	fixture.outcomes.results = []error{persistent, persistent, persistent}
	fixture.config.CleanupTimeout = time.Millisecond
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, persistent) {
		t.Fatalf("ProcessNext() = (%t, %v), want controlled reconciliation recovery", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "reconcile") != 3 || countOperation(operations, "recover") != 1 || contains(operations, "settle") {
		t.Fatalf("exhausted reconciliation operations = %v", operations)
	}
	active, settlements, recoveries, successors, _ := fixture.store.finalizationState()
	if active || settlements != 0 || recoveries != 1 || successors != 1 || !fixture.store.heartbeatStopped() {
		t.Fatalf("reconciliation recovery state = active %t, settlements %d, recoveries %d, successors %d, heartbeat stopped %t",
			active, settlements, recoveries, successors, fixture.store.heartbeatStopped())
	}
}

func TestExecutionWorkerMutationUnsettledReconciliationPreservesRecoverySemantics(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.outcomes.results = []error{store.ErrAgentTurnMutationsUnsettled}
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAgentTurnMutationsUnsettled) {
		t.Fatalf("ProcessNext() = (%t, %v), want mutation-unsettled recovery", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "reconcile") != 1 || countOperation(operations, "recover") != 1 || contains(operations, "settle") {
		t.Fatalf("mutation-unsettled reconciliation operations = %v", operations)
	}
}

func TestExecutionWorkerExhaustedSettlementEntersControlledRecovery(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	persistent := errors.New("persistent settlement failure")
	fixture.store.settleResults = []error{persistent, persistent, persistent}
	fixture.config.CleanupTimeout = time.Millisecond
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, persistent) {
		t.Fatalf("ProcessNext() = (%t, %v), want controlled settlement recovery", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "settle") != 3 || countOperation(operations, "recover") != 1 {
		t.Fatalf("exhausted settlement operations = %v", operations)
	}
	active, settlements, recoveries, successors, bounded := fixture.store.finalizationState()
	if active || settlements != 0 || recoveries != 1 || successors != 1 || bounded != 3 || !fixture.store.heartbeatStopped() {
		t.Fatalf("settlement recovery state = active %t, settlements %d, recoveries %d, successors %d, bounded calls %d, heartbeat stopped %t",
			active, settlements, recoveries, successors, bounded, fixture.store.heartbeatStopped())
	}
}

func TestExecutionWorkerRecognizesReleaseAfterAmbiguousSettlementCommit(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	ambiguous := errors.New("settlement response lost after commit")
	heartbeatAfterRelease := errors.New("heartbeat observed released authority")
	fixture.store.settleResults = []error{ambiguous, ambiguous, ambiguous}
	fixture.store.settleCommitOnError = true
	fixture.store.recoveryResults = []error{store.ErrAgentTurnFenceLost}
	fixture.store.heartbeatFailure = make(chan struct{})
	fixture.store.heartbeatErrAfterOperation = "recover"
	fixture.store.heartbeatErr = heartbeatAfterRelease
	fixture.config.CleanupTimeout = time.Millisecond
	fixture.config.HeartbeatInterval = 100 * time.Microsecond
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, ambiguous) || errors.Is(err, heartbeatAfterRelease) {
		t.Fatalf("ProcessNext() = (%t, %v), want recognized ambiguous settlement", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "settle") != 3 || countOperation(operations, "recover") != 1 {
		t.Fatalf("ambiguous settlement operations = %v", operations)
	}
	active, settlements, recoveries, successors, _ := fixture.store.finalizationState()
	if active || settlements != 1 || recoveries != 0 || successors != 1 || !fixture.store.heartbeatStopped() {
		t.Fatalf("ambiguous settlement state = active %t, settlements %d, recoveries %d, successors %d, heartbeat stopped %t",
			active, settlements, recoveries, successors, fixture.store.heartbeatStopped())
	}
}

func TestExecutionWorkerClassifiesHeartbeatFenceLossBeforeSettlementRecoveryAsReleased(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	ambiguous := errors.New("settlement response lost after commit")
	heartbeatAfterRelease := errors.New("heartbeat observed released authority")
	fixture.store.settleResults = []error{ambiguous}
	fixture.store.settleCommitOnError = true
	fixture.store.settleWaitForHeartbeat = true
	fixture.store.heartbeatFailure = make(chan struct{})
	fixture.store.heartbeatErrAfterOperation = "settle"
	fixture.store.heartbeatErr = heartbeatAfterRelease
	fixture.config.HeartbeatInterval = 100 * time.Microsecond
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, ambiguous) || errors.Is(err, heartbeatAfterRelease) {
		t.Fatalf("ProcessNext() = (%t, %v), want ambiguous settlement without post-release heartbeat failure", processed, err)
	}
	operations := fixture.operations.values()
	if countOperation(operations, "settle") != 1 || countOperation(operations, "recover") != 0 {
		t.Fatalf("ambiguous settlement race operations = %v", operations)
	}
	active, settlements, recoveries, successors, _ := fixture.store.finalizationState()
	if active || settlements != 1 || recoveries != 0 || successors != 1 || !fixture.store.heartbeatStopped() {
		t.Fatalf("ambiguous settlement race state = active %t, settlements %d, recoveries %d, successors %d, heartbeat stopped %t",
			active, settlements, recoveries, successors, fixture.store.heartbeatStopped())
	}
}

func TestExecutionWorkerRepairsAmbiguousReservationAndReleasesConcurrencySlot(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	database := newExecutionRunStore(fixture.store.lease, fixture.store.execution, 2)
	database.unsettled = []store.MutationReservation{{ID: "ambiguous-reservation", State: store.MutationReserved}}
	fixture.runtime.drainResults = []error{mcp.ErrMutationDrainUnresolved}
	dependencies := fixture.dependencies()
	dependencies.Store = database
	fixture.config.ConcurrencyLimit = 1
	worker, err := agentturn.NewExecutionWorker(dependencies, fixture.config)
	if err != nil {
		t.Fatalf("NewExecutionWorker() error = %v", err)
	}

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("first ProcessNext() = (%t, %v), want recovery", processed, err)
	}
	if got := database.failedMutationsSnapshot(); !reflect.DeepEqual(got, []string{"ambiguous-reservation"}) {
		t.Fatalf("failed residual mutations = %v", got)
	}
	if active := database.activeTurns(); active != 0 {
		t.Fatalf("active turns after recovery = %d, want released slot", active)
	}

	processed, err = worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("second ProcessNext() = (%t, %v), want released slot to admit next turn", processed, err)
	}
}

func TestExecutionWorkerPersistentResidualRepairStopsOnlyAfterFenceLoss(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	parent, cancelParent := context.WithCancel(context.Background())
	fixture.prompter.prompt = func(context.Context) (acp.PromptResponse, error) {
		cancelParent()
		return acp.PromptResponse{}, context.Canceled
	}
	fixture.runtime.drainResults = []error{mcp.ErrMutationDrainUnresolved}
	fixture.store.unsettled = []store.MutationReservation{{ID: "in-flight", State: store.MutationInFlight}}
	fixture.store.markUnknownErr = errors.New("persistent mutation storage failure")
	fixture.store.heartbeatErrAfterOperation = "mark-unknown"
	fixture.store.heartbeatErr = store.ErrAgentTurnFenceLost
	fixture.config.CleanupTimeout = time.Millisecond
	fixture.config.HeartbeatInterval = 100 * time.Microsecond
	worker := fixture.worker(t)

	result := make(chan error, 1)
	go func() {
		_, err := worker.ProcessNext(parent)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, store.ErrAgentTurnFenceLost) {
			t.Fatalf("ProcessNext() error = %v, want parent cancellation and fence loss", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("persistent mutation repair did not stop after fence loss")
	}
	operations := fixture.operations.values()
	if countOperation(operations, "mark-unknown") == 0 || contains(operations, "recover") || contains(operations, "settle") {
		t.Fatalf("persistent repair operations = %v", operations)
	}
}

func TestExecutionWorkerUsesRoleCredentialsAndDeterministicRepositoryInputs(t *testing.T) {
	tests := []struct {
		name               string
		role               workflow.Role
		wantRepository     string
		wantProviderSecret string
		wantCurrentHead    string
	}{
		{name: "Developer", role: workflow.RoleDeveloper, wantRepository: executionDeveloperRepositoryCredential, wantProviderSecret: executionDeveloperProviderSecret, wantCurrentHead: runtimeTestDefaultSHA},
		{name: "Reviewer", role: workflow.RoleReviewer, wantRepository: executionReviewerRepositoryCredential, wantProviderSecret: executionReviewerProviderSecret, wantCurrentHead: runtimeTestPRHeadSHA},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExecutionWorkerFixture(t, test.role)
			worker := fixture.worker(t)
			if _, err := worker.ProcessNext(context.Background()); err != nil {
				t.Fatalf("ProcessNext() error = %v", err)
			}
			request := fixture.launcher.request
			if request.RepositoryCredential != test.wantRepository || !strings.Contains(string(request.ProviderCredentialJSON), test.wantProviderSecret) {
				t.Fatal("launch credentials did not match the selected Role")
			}
			if request.RepositoryURL != "https://git.example.test/source/acme/widgets.git" || request.InitialFeatureBranch != "omnigrex/issue-17" {
				t.Fatalf("launch repository inputs = URL %q, branch %q", request.RepositoryURL, request.InitialFeatureBranch)
			}
			var envelope struct {
				CurrentHeadSHA string `json:"current_head_sha"`
			}
			if err := json.Unmarshal([]byte(fixture.prompter.request.Content[0].Text), &envelope); err != nil || envelope.CurrentHeadSHA != test.wantCurrentHead {
				t.Fatalf("event envelope current head = %q, error = %v", envelope.CurrentHeadSHA, err)
			}
			if test.role == workflow.RoleDeveloper && fixture.reviewerCredentials.calls != 0 || test.role == workflow.RoleReviewer && fixture.developerCredentials.calls != 0 {
				t.Fatalf("Role credential calls = Developer %d, Reviewer %d", fixture.developerCredentials.calls, fixture.reviewerCredentials.calls)
			}
			if fixture.defaultBranch.credential != test.wantRepository {
				t.Fatal("default branch resolver did not receive the selected Role credential")
			}
		})
	}
}

func TestExecutionWorkerDefaultsGitRemoteToGitHub(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.config.GitRemoteBaseURL = ""
	worker := fixture.worker(t)

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if got, want := fixture.launcher.request.RepositoryURL, "https://github.com/acme/widgets.git"; got != want {
		t.Fatalf("launch repository URL = %q, want %q", got, want)
	}
}

func TestExecutionWorkerDeepCopiesAndRedactsProviderCredentials(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	developerConfig := fixture.config.DeveloperProviderCredentialJSON
	worker := fixture.worker(t)
	if rendered := fmt.Sprintf("%#v", worker); strings.Contains(rendered, executionDeveloperProviderSecret) || strings.Contains(rendered, executionReviewerProviderSecret) {
		t.Fatalf("formatted worker leaks provider credentials: %s", rendered)
	}
	for index := range developerConfig {
		developerConfig[index] = 'x'
	}
	fixture.launcher.err = errors.New("failed with " + executionDeveloperProviderSecret + " and " + executionDeveloperRepositoryCredential)
	fixture.outcomes.observation = executionObservation(workflow.TurnOutcomeInfrastructureFailed, store.AgentTurnFailed)

	_, err := worker.ProcessNext(context.Background())
	if err == nil {
		t.Fatal("ProcessNext() error = nil")
	}
	for _, secret := range []string{executionDeveloperProviderSecret, executionDeveloperRepositoryCredential} {
		if strings.Contains(err.Error(), secret) || strings.Contains(fixture.outcomes.request.PromptDiagnostic, secret) {
			t.Fatalf("secret %q leaked in error %q or diagnostic %q", secret, err, fixture.outcomes.request.PromptDiagnostic)
		}
	}
	if !strings.Contains(fixture.outcomes.request.PromptDiagnostic, "[REDACTED]") {
		t.Fatalf("redacted diagnostic = %q", fixture.outcomes.request.PromptDiagnostic)
	}
	if !strings.Contains(string(fixture.launcher.request.ProviderCredentialJSON), executionDeveloperProviderSecret) {
		t.Fatal("launch provider credential did not use the constructor's private copy")
	}
}

func TestExecutionWorkerNoWorkDoesNothingElse(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.store.acquired = false
	worker := fixture.worker(t)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || processed {
		t.Fatalf("ProcessNext() = (%t, %v), want idle", processed, err)
	}
	if got := fixture.operations.values(); !reflect.DeepEqual(got, []string{"claim"}) {
		t.Fatalf("operations = %v", got)
	}
}

func TestExecutionWorkerRunReportsOnlyRedactedErrors(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.config.ConcurrencyLimit = 1
	fixture.launcher.err = errors.New("launch exposed " + executionDeveloperProviderSecret + " and " + executionDeveloperRepositoryCredential)
	fixture.outcomes.observation = executionObservation(workflow.TurnOutcomeInfrastructureFailed, store.AgentTurnFailed)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reported error
	fixture.config.OnError = func(err error) {
		reported = err
		cancel()
	}
	worker := fixture.worker(t)

	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want cancellation", err)
	}
	if reported == nil || strings.Contains(reported.Error(), executionDeveloperProviderSecret) || strings.Contains(reported.Error(), executionDeveloperRepositoryCredential) {
		t.Fatalf("OnError received unsafe error: %v", reported)
	}
}

func TestExecutionWorkerRunExecutesTwoBlockedTurnsAtLimitTwo(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	database := newExecutionRunStore(fixture.store.lease, fixture.store.execution, 2)
	credentials := newBlockingExecutionCredentials()
	worker := newExecutionRunWorker(t, fixture, database, credentials, 2, "execution-worker")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.Run(ctx) }()

	waitForExecutionEntries(t, credentials.entered, 2)
	if credentials.maxConcurrentCalls() != 2 || database.maxActiveTurns() != 2 {
		t.Fatalf("concurrency = credentials %d, durable turns %d; want 2", credentials.maxConcurrentCalls(), database.maxActiveTurns())
	}
	cancel()
	waitForExecutionRun(t, result, context.Canceled)
}

func TestExecutionWorkerRunLimitOneStaysSerial(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	database := newExecutionRunStore(fixture.store.lease, fixture.store.execution, 2)
	credentials := newBlockingExecutionCredentials()
	worker := newExecutionRunWorker(t, fixture, database, credentials, 1, "execution-worker")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.Run(ctx) }()

	waitForExecutionEntries(t, credentials.entered, 1)
	assertNoExecutionEntry(t, credentials.entered)
	credentials.release <- struct{}{}
	waitForExecutionEntries(t, credentials.entered, 1)
	if credentials.maxConcurrentCalls() != 1 || database.maxActiveTurns() != 1 {
		t.Fatalf("concurrency = credentials %d, durable turns %d; want 1", credentials.maxConcurrentCalls(), database.maxActiveTurns())
	}
	cancel()
	waitForExecutionRun(t, result, context.Canceled)
}

func TestExecutionWorkerRunCancellationExitsEveryProcessingLoop(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	baseStore := newExecutionRunStore(fixture.store.lease, fixture.store.execution, 0)
	database := &cancelBlockingExecutionStore{
		executionRunStore: baseStore,
		entered:           make(chan struct{}, 3),
		exited:            make(chan struct{}, 3),
	}
	credentials := newBlockingExecutionCredentials()
	worker := newExecutionRunWorker(t, fixture, database, credentials, 3, "execution-worker")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.Run(ctx) }()

	waitForExecutionEntries(t, database.entered, 3)
	cancel()
	waitForExecutionRun(t, result, context.Canceled)
	waitForExecutionEntries(t, database.exited, 3)
}

func TestExecutionWorkerRunSharedStoreGloballyGovernsClaims(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	database := newExecutionRunStore(fixture.store.lease, fixture.store.execution, 4)
	credentials := newBlockingExecutionCredentials()
	first := newExecutionRunWorker(t, fixture, database, credentials, 2, "execution-worker-1")
	second := newExecutionRunWorker(t, fixture, database, credentials, 2, "execution-worker-2")
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 2)
	go func() { results <- first.Run(ctx) }()
	go func() { results <- second.Run(ctx) }()

	waitForExecutionEntries(t, credentials.entered, 2)
	assertNoExecutionEntry(t, credentials.entered)
	if database.maxActiveTurns() != 2 || !database.onlyClaimLimit(2) {
		t.Fatalf("shared Store max active = %d, claim limits = %v", database.maxActiveTurns(), database.claimLimitsSnapshot())
	}
	cancel()
	waitForExecutionRun(t, results, context.Canceled)
	waitForExecutionRun(t, results, context.Canceled)
}

func TestNewExecutionWorkerValidatesConfiguration(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	validDependencies := fixture.dependencies()
	validConfig := fixture.config
	tests := []struct {
		name   string
		deps   agentturn.ExecutionWorkerDependencies
		config agentturn.ExecutionWorkerConfig
	}{
		{name: "nil dependency", deps: func() agentturn.ExecutionWorkerDependencies {
			value := validDependencies
			value.Store = nil
			return value
		}(), config: validConfig},
		{name: "empty owner", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig { value := validConfig; value.ClaimOwner = " "; return value }()},
		{name: "heartbeat not shorter", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig {
			value := validConfig
			value.HeartbeatInterval = value.LeaseDuration
			return value
		}()},
		{name: "zero timeout", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig { value := validConfig; value.TurnTimeout = 0; return value }()},
		{name: "invalid concurrency", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig { value := validConfig; value.ConcurrencyLimit = 0; return value }()},
		{name: "array Developer provider", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig {
			value := validConfig
			value.DeveloperProviderCredentialJSON = json.RawMessage(`[]`)
			return value
		}()},
		{name: "empty Reviewer provider", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig {
			value := validConfig
			value.ReviewerProviderCredentialJSON = json.RawMessage(`{}`)
			return value
		}()},
		{name: "non-HTTPS remote", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig {
			value := validConfig
			value.GitRemoteBaseURL = "http://git.example.test"
			return value
		}()},
		{name: "credentialed remote", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig {
			value := validConfig
			value.GitRemoteBaseURL = "https://credential-sentinel@git.example.test"
			return value
		}()},
		{name: "unclean remote path", deps: validDependencies, config: func() agentturn.ExecutionWorkerConfig {
			value := validConfig
			value.GitRemoteBaseURL = "https://git.example.test/source/../repos"
			return value
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker, err := agentturn.NewExecutionWorker(test.deps, test.config)
			if worker != nil || !errors.Is(err, agentturn.ErrInvalidExecutionWorker) {
				t.Fatalf("NewExecutionWorker() = (%#v, %v)", worker, err)
			}
			if strings.Contains(err.Error(), executionDeveloperProviderSecret) || strings.Contains(err.Error(), executionReviewerProviderSecret) {
				t.Fatalf("configuration error leaks provider credential: %v", err)
			}
			if strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("configuration error leaks remote credential: %v", err)
			}
		})
	}
}

func TestNewExecutionWorkerAllowsTurnTimeoutLongerThanRenewableLease(t *testing.T) {
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.config.LeaseDuration = 30 * time.Second
	fixture.config.HeartbeatInterval = 10 * time.Second
	fixture.config.TurnTimeout = 2 * time.Hour

	if _, err := agentturn.NewExecutionWorker(fixture.dependencies(), fixture.config); err != nil {
		t.Fatalf("NewExecutionWorker() error = %v", err)
	}
}

type executionWorkerFixture struct {
	operations           *executionOperations
	store                *executionStore
	developerCredentials *executionCredentialProvider
	reviewerCredentials  *executionCredentialProvider
	defaultBranch        *executionDefaultBranch
	launcher             *executionLauncher
	runtime              *executionRuntime
	prompter             *executionPrompter
	outcomes             *executionOutcomes
	workspace            *executionWorkspace
	config               agentturn.ExecutionWorkerConfig
}

func newExecutionWorkerFixture(t *testing.T, role workflow.Role) *executionWorkerFixture {
	t.Helper()
	operations := &executionOperations{}
	runtimeProfile := runtimeLauncherProfile(t)
	var proposal *store.AgentTurnChangeProposal
	if role == workflow.RoleReviewer {
		proposal = &store.AgentTurnChangeProposal{
			ID: "60000000-0000-4000-8000-000000000001", PullRequestID: 61, PullRequestNumber: 23,
			BaseRef: "trunk", BaseSHA: runtimeTestDefaultSHA, HeadRef: "omnigrex/issue-17", HeadSHA: runtimeTestPRHeadSHA,
		}
	}
	execution, lease := runtimeExecutionContext(t, runtimeProfile, role, proposal)
	execution.Session.Status = store.AgentSessionActive
	execution.Session.ACPSessionID = "acp-session"
	execution.Turn.Purpose = workflow.TurnPurposeInitialDevelopment
	if role == workflow.RoleReviewer {
		execution.Turn.Purpose = workflow.TurnPurposeReview
	}
	lease.AgentTurn = execution.Turn
	lease.LeaseExpiresAt = time.Now().Add(2 * time.Hour)
	runtime := &executionRuntime{operations: operations, lease: lease, client: &executionPromptClient{}}
	fixture := &executionWorkerFixture{
		operations:           operations,
		store:                &executionStore{operations: operations, lease: lease, acquired: true, execution: execution, slotActive: true},
		developerCredentials: &executionCredentialProvider{operations: operations, operation: "developer-credential", credential: executionDeveloperRepositoryCredential},
		reviewerCredentials:  &executionCredentialProvider{operations: operations, operation: "reviewer-credential", credential: executionReviewerRepositoryCredential},
		defaultBranch:        &executionDefaultBranch{operations: operations, branch: githubapi.DefaultBranch{Name: "trunk", CommitSHA: runtimeTestDefaultSHA}},
		launcher:             &executionLauncher{operations: operations, runtime: runtime},
		runtime:              runtime,
		prompter:             &executionPrompter{operations: operations, response: acp.PromptResponse{StopReason: acp.StopReasonEndTurn}},
		outcomes:             &executionOutcomes{operations: operations, observation: executionObservation(workflow.TurnOutcomeBlocked, store.AgentTurnSucceeded)},
		workspace:            &executionWorkspace{operations: operations, paths: workspace.Paths{Workspace: "/workspace", Publication: "/publication", Mise: "/mise"}},
		config: agentturn.ExecutionWorkerConfig{
			ClaimOwner: "execution-worker", LeaseDuration: 2 * time.Hour, HeartbeatInterval: time.Hour,
			IdlePollInterval: time.Millisecond, TurnTimeout: time.Second, CleanupTimeout: time.Second,
			ConcurrencyLimit:                3,
			DeveloperProviderCredentialJSON: json.RawMessage(`{"openai":{"apiKey":"` + executionDeveloperProviderSecret + `"}}`),
			ReviewerProviderCredentialJSON:  json.RawMessage(`{"openai":{"apiKey":"` + executionReviewerProviderSecret + `"}}`),
			GitRemoteBaseURL:                "https://git.example.test/source/",
		},
	}
	return fixture
}

func (fixture *executionWorkerFixture) dependencies() agentturn.ExecutionWorkerDependencies {
	return agentturn.ExecutionWorkerDependencies{
		Store: fixture.store, DeveloperCredentials: fixture.developerCredentials,
		ReviewerCredentials: fixture.reviewerCredentials, DefaultBranch: fixture.defaultBranch,
		Launcher: fixture.launcher, Sessions: fixture.prompter, Outcomes: fixture.outcomes, Workspace: fixture.workspace,
	}
}

func (fixture *executionWorkerFixture) worker(t *testing.T) *agentturn.ExecutionWorker {
	t.Helper()
	worker, err := agentturn.NewExecutionWorker(fixture.dependencies(), fixture.config)
	if err != nil {
		t.Fatalf("NewExecutionWorker() error = %v", err)
	}
	return worker
}

func newExecutionRunWorker(t *testing.T, fixture *executionWorkerFixture, database agentturn.ExecutionWorkerStore, credentials agentturn.RepositoryCredentialProvider, limit int, owner string) *agentturn.ExecutionWorker {
	t.Helper()
	dependencies := fixture.dependencies()
	dependencies.Store = database
	dependencies.DeveloperCredentials = credentials
	dependencies.Outcomes = executionRunOutcomes{}
	config := fixture.config
	config.ClaimOwner = owner
	config.ConcurrencyLimit = limit
	config.OnError = func(error) {}
	worker, err := agentturn.NewExecutionWorker(dependencies, config)
	if err != nil {
		t.Fatalf("NewExecutionWorker() error = %v", err)
	}
	return worker
}

type executionRunStore struct {
	mutex       sync.Mutex
	lease       store.AgentTurnLease
	execution   store.AgentTurnExecutionContext
	remaining   int
	active      int
	maxActive   int
	claimLimits []int
	unsettled   []store.MutationReservation
	failed      []string
}

func newExecutionRunStore(lease store.AgentTurnLease, execution store.AgentTurnExecutionContext, turns int) *executionRunStore {
	return &executionRunStore{lease: lease, execution: execution, remaining: turns}
}

func (database *executionRunStore) ClaimAndAcquireAgentTurn(_ context.Context, _ string, _ time.Duration, limit int) (store.AgentTurnLease, bool, error) {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	database.claimLimits = append(database.claimLimits, limit)
	if database.remaining == 0 || database.active >= limit {
		return store.AgentTurnLease{}, false, nil
	}
	database.remaining--
	database.active++
	if database.active > database.maxActive {
		database.maxActive = database.active
	}
	return database.lease, true, nil
}

func (*executionRunStore) HeartbeatAgentTurn(context.Context, store.AgentTurnLease, time.Duration) error {
	return nil
}

func (database *executionRunStore) GetAgentTurnExecutionContext(context.Context, store.AgentTurnLease) (store.AgentTurnExecutionContext, error) {
	return database.execution, nil
}

func (*executionRunStore) OpenMutationAdmission(context.Context, store.AgentTurnLease) error {
	return nil
}

func (*executionRunStore) CloseMutationAdmission(context.Context, store.AgentTurnLease) error {
	return nil
}

func (database *executionRunStore) ListUnsettledMutations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error) {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	return append([]store.MutationReservation(nil), database.unsettled...), nil
}

func (database *executionRunStore) FailMutation(_ context.Context, _ store.AgentTurnLease, mutationID string, _ error) error {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	database.failed = append(database.failed, mutationID)
	for index, mutation := range database.unsettled {
		if mutation.ID == mutationID {
			database.unsettled = append(database.unsettled[:index], database.unsettled[index+1:]...)
			break
		}
	}
	return nil
}

func (*executionRunStore) MarkMutationUnknown(context.Context, store.AgentTurnLease, string, error) error {
	return nil
}

func (database *executionRunStore) BeginAgentTurnRecovery(context.Context, store.AgentTurnLease) (store.AgentTurnRecovery, error) {
	database.releaseTurn()
	return store.AgentTurnRecovery{}, nil
}

func (database *executionRunStore) SettleAgentTurn(context.Context, store.AgentTurnLease, store.AgentTurnSettlementObservation) (store.AgentTurnSettlement, error) {
	database.releaseTurn()
	return store.AgentTurnSettlement{}, nil
}

func (database *executionRunStore) releaseTurn() {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	if database.active > 0 {
		database.active--
	}
}

func (database *executionRunStore) maxActiveTurns() int {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	return database.maxActive
}

func (database *executionRunStore) activeTurns() int {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	return database.active
}

func (database *executionRunStore) failedMutationsSnapshot() []string {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	return append([]string(nil), database.failed...)
}

func (database *executionRunStore) onlyClaimLimit(want int) bool {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	for _, limit := range database.claimLimits {
		if limit != want {
			return false
		}
	}
	return len(database.claimLimits) != 0
}

func (database *executionRunStore) claimLimitsSnapshot() []int {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	return append([]int(nil), database.claimLimits...)
}

type cancelBlockingExecutionStore struct {
	*executionRunStore
	entered chan struct{}
	exited  chan struct{}
}

func (database *cancelBlockingExecutionStore) ClaimAndAcquireAgentTurn(ctx context.Context, _ string, _ time.Duration, _ int) (store.AgentTurnLease, bool, error) {
	database.entered <- struct{}{}
	<-ctx.Done()
	database.exited <- struct{}{}
	return store.AgentTurnLease{}, false, ctx.Err()
}

type blockingExecutionCredentials struct {
	mutex         sync.Mutex
	entered       chan struct{}
	release       chan struct{}
	active        int
	maxConcurrent int
}

func newBlockingExecutionCredentials() *blockingExecutionCredentials {
	return &blockingExecutionCredentials{entered: make(chan struct{}, 16), release: make(chan struct{}, 16)}
}

func (credentials *blockingExecutionCredentials) RepositoryCredential(ctx context.Context, _, _ string) (string, error) {
	credentials.mutex.Lock()
	credentials.active++
	if credentials.active > credentials.maxConcurrent {
		credentials.maxConcurrent = credentials.active
	}
	credentials.mutex.Unlock()
	credentials.entered <- struct{}{}

	var err error
	select {
	case <-credentials.release:
		err = errors.New("controlled execution failure")
	case <-ctx.Done():
		err = ctx.Err()
	}
	credentials.mutex.Lock()
	credentials.active--
	credentials.mutex.Unlock()
	return "", err
}

func (credentials *blockingExecutionCredentials) maxConcurrentCalls() int {
	credentials.mutex.Lock()
	defer credentials.mutex.Unlock()
	return credentials.maxConcurrent
}

type executionRunOutcomes struct{}

func (executionRunOutcomes) Reconcile(context.Context, agentturn.OutcomeReconciliation) (store.AgentTurnSettlementObservation, error) {
	return executionObservation(workflow.TurnOutcomeInfrastructureFailed, store.AgentTurnFailed), nil
}

func waitForExecutionEntries(t *testing.T, entries <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-entries:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %d execution entries", count)
		}
	}
}

func assertNoExecutionEntry(t *testing.T, entries <-chan struct{}) {
	t.Helper()
	select {
	case <-entries:
		t.Fatal("unexpected concurrent execution entry")
	case <-time.After(50 * time.Millisecond):
	}
}

func waitForExecutionRun(t *testing.T, result <-chan error, want error) {
	t.Helper()
	select {
	case err := <-result:
		if !errors.Is(err, want) {
			t.Fatalf("Run() error = %v, want %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop")
	}
}

type executionOperations struct {
	mutex       sync.Mutex
	valuesSlice []string
}

func (operations *executionOperations) add(value string) {
	operations.mutex.Lock()
	defer operations.mutex.Unlock()
	operations.valuesSlice = append(operations.valuesSlice, value)
}

func (operations *executionOperations) values() []string {
	operations.mutex.Lock()
	defer operations.mutex.Unlock()
	return append([]string(nil), operations.valuesSlice...)
}

type executionStore struct {
	mutex                      sync.Mutex
	operations                 *executionOperations
	lease                      store.AgentTurnLease
	acquired                   bool
	execution                  store.AgentTurnExecutionContext
	heartbeatErrAt             int
	heartbeatErrAfterOperation string
	heartbeatErr               error
	heartbeats                 int
	openErr                    error
	closeErr                   error
	unsettled                  []store.MutationReservation
	unsettledResults           [][]store.MutationReservation
	failMutationResults        []error
	markUnknownResults         []error
	markUnknownErr             error
	recoveryResults            []error
	settleResults              []error
	settleCommitOnError        bool
	settleWaitForHeartbeat     bool
	settled                    store.AgentTurnSettlementObservation
	heartbeatCtx               context.Context
	slotActive                 bool
	settlementCommits          int
	recoveryCommits            int
	successors                 int
	settleBoundedCalls         int
	heartbeatFailure           chan struct{}
}

func (database *executionStore) ClaimAndAcquireAgentTurn(context.Context, string, time.Duration, int) (store.AgentTurnLease, bool, error) {
	database.operations.add("claim")
	return database.lease, database.acquired, nil
}

func (database *executionStore) HeartbeatAgentTurn(ctx context.Context, _ store.AgentTurnLease, _ time.Duration) error {
	database.operations.add("heartbeat")
	database.mutex.Lock()
	defer database.mutex.Unlock()
	if database.heartbeatCtx == nil {
		database.heartbeatCtx = ctx
	}
	database.heartbeats++
	if database.heartbeatErrAfterOperation != "" && contains(database.operations.values(), database.heartbeatErrAfterOperation) {
		if database.heartbeatFailure != nil {
			close(database.heartbeatFailure)
			database.heartbeatFailure = nil
		}
		return database.heartbeatErr
	}
	if database.heartbeatErrAt != 0 && database.heartbeats >= database.heartbeatErrAt {
		if database.heartbeatFailure != nil {
			close(database.heartbeatFailure)
			database.heartbeatFailure = nil
		}
		return database.heartbeatErr
	}
	return nil
}

func (database *executionStore) GetAgentTurnExecutionContext(context.Context, store.AgentTurnLease) (store.AgentTurnExecutionContext, error) {
	database.operations.add("execution")
	return database.execution, nil
}

func (database *executionStore) OpenMutationAdmission(context.Context, store.AgentTurnLease) error {
	database.operations.add("open-admission")
	return database.openErr
}

func (database *executionStore) CloseMutationAdmission(context.Context, store.AgentTurnLease) error {
	database.operations.add("close-admission")
	return database.closeErr
}

func (database *executionStore) ListUnsettledMutations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error) {
	database.operations.add("list-unsettled")
	if len(database.unsettledResults) != 0 {
		result := append([]store.MutationReservation(nil), database.unsettledResults[0]...)
		database.unsettledResults = database.unsettledResults[1:]
		return result, nil
	}
	return append([]store.MutationReservation(nil), database.unsettled...), nil
}

func (database *executionStore) FailMutation(context.Context, store.AgentTurnLease, string, error) error {
	database.operations.add("fail-mutation")
	database.mutex.Lock()
	defer database.mutex.Unlock()
	if len(database.failMutationResults) == 0 {
		return nil
	}
	result := database.failMutationResults[0]
	database.failMutationResults = database.failMutationResults[1:]
	return result
}

func (database *executionStore) MarkMutationUnknown(context.Context, store.AgentTurnLease, string, error) error {
	database.operations.add("mark-unknown")
	database.mutex.Lock()
	defer database.mutex.Unlock()
	if len(database.markUnknownResults) != 0 {
		result := database.markUnknownResults[0]
		database.markUnknownResults = database.markUnknownResults[1:]
		return result
	}
	return database.markUnknownErr
}

func (database *executionStore) BeginAgentTurnRecovery(context.Context, store.AgentTurnLease) (store.AgentTurnRecovery, error) {
	database.operations.add("recover")
	database.mutex.Lock()
	var result error
	if len(database.recoveryResults) != 0 {
		result = database.recoveryResults[0]
		database.recoveryResults = database.recoveryResults[1:]
	}
	if result == nil && database.recoveryCommits == 0 {
		database.recoveryCommits++
		database.successors++
	}
	if result == nil {
		database.slotActive = false
	}
	heartbeatFailure := database.heartbeatFailure
	database.mutex.Unlock()
	if result != nil && heartbeatFailure != nil {
		<-heartbeatFailure
	}
	return store.AgentTurnRecovery{}, result
}

func (database *executionStore) SettleAgentTurn(ctx context.Context, _ store.AgentTurnLease, observation store.AgentTurnSettlementObservation) (store.AgentTurnSettlement, error) {
	database.operations.add("settle")
	database.mutex.Lock()
	database.settled = observation
	if _, bounded := ctx.Deadline(); bounded {
		database.settleBoundedCalls++
	}
	var result error
	if len(database.settleResults) != 0 {
		result = database.settleResults[0]
		database.settleResults = database.settleResults[1:]
	}
	if result == nil || database.settleCommitOnError {
		if database.settlementCommits == 0 {
			database.settlementCommits++
			database.successors++
		}
		database.slotActive = false
	}
	heartbeatFailure := database.heartbeatFailure
	waitForHeartbeat := result != nil && database.settleWaitForHeartbeat && heartbeatFailure != nil
	database.mutex.Unlock()
	if waitForHeartbeat {
		select {
		case <-heartbeatFailure:
			<-ctx.Done()
		case <-ctx.Done():
			return store.AgentTurnSettlement{}, errors.Join(result, ctx.Err())
		}
	}
	if result != nil {
		return store.AgentTurnSettlement{}, result
	}
	return store.AgentTurnSettlement{}, nil
}

func (database *executionStore) finalizationState() (active bool, settlements, recoveries, successors, bounded int) {
	database.mutex.Lock()
	defer database.mutex.Unlock()
	return database.slotActive, database.settlementCommits, database.recoveryCommits, database.successors, database.settleBoundedCalls
}

func (database *executionStore) heartbeatStopped() bool {
	database.mutex.Lock()
	ctx := database.heartbeatCtx
	database.mutex.Unlock()
	if ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

type executionCredentialProvider struct {
	operations *executionOperations
	operation  string
	credential string
	calls      int
}

func (provider *executionCredentialProvider) RepositoryCredential(context.Context, string, string) (string, error) {
	provider.operations.add(provider.operation)
	provider.calls++
	return provider.credential, nil
}

type executionDefaultBranch struct {
	operations *executionOperations
	branch     githubapi.DefaultBranch
	credential string
}

func (resolver *executionDefaultBranch) ResolveDefaultBranch(_ context.Context, credential, _, _ string) (githubapi.DefaultBranch, error) {
	resolver.operations.add("default-branch")
	resolver.credential = credential
	return resolver.branch, nil
}

type executionLauncher struct {
	operations *executionOperations
	runtime    *executionRuntime
	request    agentturn.LaunchRequest
	err        error
}

func (launcher *executionLauncher) LaunchExecution(_ context.Context, request agentturn.LaunchRequest) (agentturn.ExecutionRuntime, error) {
	launcher.operations.add("launch")
	launcher.request = request
	launcher.request.ProviderCredentialJSON = append(json.RawMessage(nil), request.ProviderCredentialJSON...)
	return launcher.runtime, launcher.err
}

type executionRuntime struct {
	mutex          sync.Mutex
	operations     *executionOperations
	lease          store.AgentTurnLease
	client         session.PromptClient
	drainResults   []error
	drainWaits     int
	cleanupResults []error
}

func (runtime *executionRuntime) CurrentLease() store.AgentTurnLease { return runtime.lease }
func (runtime *executionRuntime) PromptClient() session.PromptClient { return runtime.client }
func (runtime *executionRuntime) CloseMCP(ctx context.Context) error {
	runtime.operations.add("mcp-drain")
	runtime.mutex.Lock()
	if runtime.drainWaits > 0 {
		runtime.drainWaits--
		runtime.mutex.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	defer runtime.mutex.Unlock()
	if len(runtime.drainResults) == 0 {
		return nil
	}
	result := runtime.drainResults[0]
	runtime.drainResults = runtime.drainResults[1:]
	return result
}
func (runtime *executionRuntime) Cleanup(context.Context) error {
	runtime.operations.add("cleanup")
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if len(runtime.cleanupResults) == 0 {
		return nil
	}
	result := runtime.cleanupResults[0]
	runtime.cleanupResults = runtime.cleanupResults[1:]
	return result
}
func (runtime *executionRuntime) drainCallCount() int {
	count := 0
	for _, operation := range runtime.operations.values() {
		if operation == "mcp-drain" {
			count++
		}
	}
	return count
}

type executionPromptClient struct{}

func (*executionPromptClient) SetAgentEventContext(agentevent.Context) {}
func (*executionPromptClient) Prompt(context.Context, string, []acp.ContentBlock) (acp.PromptResponse, error) {
	return acp.PromptResponse{}, errors.New("unexpected direct prompt")
}

type executionPrompter struct {
	operations *executionOperations
	request    session.PromptRequest
	response   acp.PromptResponse
	err        error
	prompt     func(context.Context) (acp.PromptResponse, error)
}

func (prompter *executionPrompter) Prompt(ctx context.Context, request session.PromptRequest) (acp.PromptResponse, error) {
	prompter.operations.add("prompt")
	prompter.request = request
	if prompter.prompt != nil {
		return prompter.prompt(ctx)
	}
	return prompter.response, prompter.err
}

type executionOutcomes struct {
	operations   *executionOperations
	request      agentturn.OutcomeReconciliation
	observation  store.AgentTurnSettlementObservation
	reconcile    func(agentturn.OutcomeReconciliation) store.AgentTurnSettlementObservation
	results      []error
	boundedCalls int
}

func (outcomes *executionOutcomes) Reconcile(ctx context.Context, request agentturn.OutcomeReconciliation) (store.AgentTurnSettlementObservation, error) {
	outcomes.operations.add("reconcile")
	outcomes.request = request
	if _, bounded := ctx.Deadline(); bounded {
		outcomes.boundedCalls++
	}
	var result error
	if len(outcomes.results) != 0 {
		result = outcomes.results[0]
		outcomes.results = outcomes.results[1:]
	}
	if outcomes.reconcile != nil {
		return outcomes.reconcile(request), result
	}
	return outcomes.observation, result
}

type executionWorkspace struct {
	operations *executionOperations
	paths      workspace.Paths
}

func (resolver *executionWorkspace) Paths(string) (workspace.Paths, error) {
	resolver.operations.add("paths")
	return resolver.paths, nil
}

func executionObservation(outcome workflow.TurnOutcome, status store.AgentTurnStatus) store.AgentTurnSettlementObservation {
	return store.AgentTurnSettlementObservation{
		ObservedAt: time.Now().UTC(), Outcome: outcome,
		Completion: store.AgentTurnCompletion{Status: status},
	}
}

func waitForPromptCancellation(ctx context.Context) (acp.PromptResponse, error) {
	<-ctx.Done()
	return acp.PromptResponse{}, ctx.Err()
}

func mapPromptClassToStatus(classification agentturn.PromptErrorClassification) store.AgentTurnStatus {
	switch classification {
	case agentturn.PromptErrorDeadline:
		return store.AgentTurnTimedOut
	case agentturn.PromptErrorCancellation:
		return store.AgentTurnInterrupted
	default:
		return store.AgentTurnFailed
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func countOperation(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}

func containsInOrder(values []string, sequence ...string) bool {
	index := 0
	for _, value := range values {
		if index < len(sequence) && value == sequence[index] {
			index++
		}
	}
	return index == len(sequence)
}
