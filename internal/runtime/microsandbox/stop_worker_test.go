package microsandbox

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestStopWorkerRecoversLostRemovalAcknowledgementWithSealedCapture(t *testing.T) {
	ctx := t.Context()
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(ctx, request, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := backend.EnsureKeyStopped(ctx, request.Key); err != nil {
		t.Fatal(err)
	}
	if !ledger.records[request.Key].CreationSettled || driver.resources[nameFor(request.Key)].Status != "Stopped" {
		t.Fatal("fixture did not settle creation and confirm the main VM stopped")
	}

	// This logical receipt represents terminal evidence sealed before removal.
	// Capture below only loads it, like Lifecycle.Capture's CaptureLedger path;
	// it cannot obtain fresh evidence from the missing VM.
	sealed := execution.CaptureRef{Request: execution.CaptureRequest{
		Workspace: execution.WorkspaceRef{Key: request.Key}, OperationID: request.Key.AgentTurnID,
		Purpose: execution.TerminalCapture,
	}, SHA256: strings.Repeat("a", 64)}
	workspaces := &sealedStopWorkerWorkspace{sealed: sealed}
	// The provider removed the resource, but the caller lost the reply before
	// persisting Removed. Preserve the native journal's settled stop receipt.
	delete(driver.resources, nameFor(request.Key))
	if err := backend.locked(ctx, request.Key, func(record *operation) error {
		if !record.Created || !record.Stopped || record.Removed || record.ResourceID == "" {
			t.Fatalf("lost-removal fixture has no pending acknowledgement: %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertStopWorkerMissingExitUnknown(t, backend, request.Key)

	// Scratch cleanup must receive the native absence receipt, not merely an
	// empty inventory or an unknown exit observation.
	workspaces.beforeCleanup = func(ctx context.Context) error {
		return backend.locked(ctx, request.Key, func(record *operation) error {
			if !record.Stopped || !record.Removed {
				return execution.ErrUnknownEffect
			}
			return nil
		})
	}
	durable, worker := newNativeStopWorker(t, backend, request.Key, workspaces)
	processed, err := worker.ProcessNext(ctx)
	if !processed || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want successful removal reconciliation", processed, err)
	}
	if len(durable.stopped) != 1 || !reflect.DeepEqual(durable.stopped[0], durable.lease) || !durable.scratchCleanedAtAcknowledgement {
		t.Fatalf("stop acknowledgement = %+v, scratch cleaned before acknowledgement = %t", durable.stopped, durable.scratchCleanedAtAcknowledgement)
	}
	if len(durable.failures) != 0 || durable.oomRecords != 0 || durable.neverStartedRecords != 0 {
		t.Fatalf("missing exit caused failure/OOM/no-start attribution: failures %v, OOM %d, no-start %d", durable.failures, durable.oomRecords, durable.neverStartedRecords)
	}
	if !reflect.DeepEqual(workspaces.returnedCaptures, []execution.CaptureRef{sealed}) || workspaces.sealed != sealed {
		t.Fatalf("retained capture = %+v, want original sealed receipt %+v", workspaces.returnedCaptures, sealed)
	}
	if workspaces.scratchAssignment != request.Key.AssignmentID || workspaces.scratchTurn != request.Key.AgentTurnID || workspaces.discards != 0 {
		t.Fatalf("scratch cleanup identity = %q/%q, discards = %d", workspaces.scratchAssignment, workspaces.scratchTurn, workspaces.discards)
	}
	if len(driver.resources) != 0 || driver.creates != 1 || driver.stops != 1 || driver.removes != 0 || !ledger.records[request.Key].Revoked {
		t.Fatal("recovery recreated or acted on the missing VM instead of reconciling authoritative absence")
	}
	if durable.cleanupFences == 0 {
		t.Fatal("recovery bypassed the cleanup fence")
	}
}

func TestStopWorkerDoesNotAcknowledgeMissingUnresolvedCreation(t *testing.T) {
	ctx := t.Context()
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	driver.createErr = errors.New("creation acknowledgement lost")
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(ctx, request, io.Discard); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("Create() = %v, want unresolved creation", err)
	}
	delete(driver.resources, nameFor(request.Key))
	assertStopWorkerMissingExitUnknown(t, backend, request.Key)
	workspaces := &sealedStopWorkerWorkspace{}
	durable, worker := newNativeStopWorker(t, backend, request.Key, workspaces)
	processed, err := worker.ProcessNext(ctx)
	if !processed || !errors.Is(err, execution.ErrUnknownEffect) || !strings.Contains(err.Error(), "confirm stale Runtime Process stopped") {
		t.Fatalf("ProcessNext() = (%t, %v), want unresolved stop failure", processed, err)
	}
	if len(durable.stopped) != 0 || len(durable.failures) != 1 || !errors.Is(durable.failures[0], execution.ErrUnknownEffect) {
		t.Fatalf("unresolved cleanup acknowledgements: stopped %+v, failures %v", durable.stopped, durable.failures)
	}
	if durable.oomRecords != 0 || durable.neverStartedRecords != 0 || workspaces.captureCalls != 0 || workspaces.scratchCleaned || workspaces.discards != 0 {
		t.Fatal("unresolved creation fabricated evidence or authorized workspace cleanup")
	}
	reservation := ledger.records[request.Key]
	if reservation.CreationSettled || reservation.Phase != execution.Main || !reservation.Revoked {
		t.Fatalf("unresolved creation lost its revoked, unsettled reservation: %+v", reservation)
	}
	if err := backend.locked(ctx, request.Key, func(record *operation) error {
		if record.Stopped || record.Removed {
			t.Fatalf("unresolved creation issued a cleanup receipt: %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertStopWorkerMissingExitUnknown(t *testing.T, backend *Backend, key execution.Key) {
	t.Helper()
	observation, err := backend.ObserveKeyExit(t.Context(), key)
	if err != nil || observation != (execution.ExitObservation{}) {
		t.Fatalf("missing VM exit = %+v, %v, want unknown without exited/no-start/OOM evidence", observation, err)
	}
}

func newNativeStopWorker(t *testing.T, backend *Backend, key execution.Key, workspaces *sealedStopWorkerWorkspace) (*nativeStopWorkerStore, *agentturn.StopWorker) {
	t.Helper()
	durable := &nativeStopWorkerStore{key: key, workspaces: workspaces, lease: store.JobLease{Job: store.Job{
		JobSpec: store.JobSpec{
			Queue: store.AgentTurnRecoveryQueue, Kind: store.StopStaleRuntimeJobKind,
			AgentParticipantID: key.AssignmentID, AgentSessionID: key.AgentSessionID,
			AgentTurnID: key.AgentTurnID, ExecutionEpoch: key.ExecutionEpoch,
		},
		ID: "40000000-0000-4000-8000-000000000001", Status: store.JobLeased,
		LeaseOwner: "native-stop-worker", LeaseToken: "50000000-0000-4000-8000-000000000001",
		AttemptCount: 1,
	}, Attempt: 1}}
	worker, err := agentturn.NewStopWorker(durable, backend, workspaces, agentturn.StopWorkerConfig{
		ClaimOwner: durable.lease.LeaseOwner, LeaseDuration: time.Minute, HeartbeatInterval: time.Second,
		IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return durable, worker
}

type nativeStopWorkerStore struct {
	key                             execution.Key
	lease                           store.JobLease
	workspaces                      *sealedStopWorkerWorkspace
	claimed                         bool
	cleanupFences                   int
	stopped                         []store.JobLease
	failures                        []error
	oomRecords, neverStartedRecords int
	scratchCleanedAtAcknowledgement bool
}

var _ agentturn.StopWorkerStore = (*nativeStopWorkerStore)(nil)

func (durable *nativeStopWorkerStore) ClaimJobKind(_ context.Context, queue, kind, owner string, _ time.Duration) (*store.JobLease, error) {
	if queue != durable.lease.Queue || kind != durable.lease.Kind || owner != durable.lease.LeaseOwner {
		return nil, execution.ErrConflict
	}
	if durable.claimed {
		return nil, nil
	}
	durable.claimed = true
	lease := durable.lease
	return &lease, nil
}

func (durable *nativeStopWorkerStore) checkLease(lease store.JobLease) error {
	if !reflect.DeepEqual(lease, durable.lease) {
		return store.ErrAgentTurnRecoveryFenceLost
	}
	return nil
}

func (durable *nativeStopWorkerStore) HeartbeatJob(_ context.Context, lease store.JobLease, _ time.Duration) error {
	return durable.checkLease(lease)
}

func (durable *nativeStopWorkerStore) GetAgentTurnRuntimeCleanupContext(_ context.Context, lease store.JobLease) (store.AgentTurnRuntimeCleanupContext, error) {
	return store.AgentTurnRuntimeCleanupContext{
		AssignmentID: durable.key.AssignmentID, Role: workflow.RoleDeveloper,
		RuntimeProfileName: durable.key.RuntimeProfileName, RuntimeProfileVersion: durable.key.RuntimeProfileVersion,
	}, durable.checkLease(lease)
}

func (durable *nativeStopWorkerStore) WithRecoveredRuntimeCleanupFence(ctx context.Context, lease store.JobLease, operation func(context.Context) error) error {
	if err := durable.checkLease(lease); err != nil {
		return err
	}
	durable.cleanupFences++
	return operation(ctx)
}

func (durable *nativeStopWorkerStore) RecordRecoveredRuntimeOOM(_ context.Context, lease store.JobLease, _ string, _ int) error {
	durable.oomRecords++
	return durable.checkLease(lease)
}

func (durable *nativeStopWorkerStore) RecordRecoveredRuntimeNeverStarted(_ context.Context, lease store.JobLease, _ string) error {
	durable.neverStartedRecords++
	return durable.checkLease(lease)
}

func (durable *nativeStopWorkerStore) AcknowledgeRecoveredRuntimeStopped(_ context.Context, lease store.JobLease) (store.AgentTurnRecovery, error) {
	if err := durable.checkLease(lease); err != nil {
		return store.AgentTurnRecovery{}, err
	}
	durable.stopped = append(durable.stopped, lease)
	durable.scratchCleanedAtAcknowledgement = durable.workspaces.scratchCleaned
	return store.AgentTurnRecovery{}, nil
}

func (durable *nativeStopWorkerStore) AcknowledgeRecoveredRuntimeStopFailure(_ context.Context, lease store.JobLease, cause error, _ time.Duration) (store.AgentTurnRuntimeStopFailureAcknowledgement, error) {
	durable.failures = append(durable.failures, cause)
	return store.AgentTurnRuntimeStopFailureAcknowledgement{RetryScheduled: true}, durable.checkLease(lease)
}

type sealedStopWorkerWorkspace struct {
	sealed                         execution.CaptureRef
	returnedCaptures               []execution.CaptureRef
	captureCalls, discards         int
	beforeCleanup                  func(context.Context) error
	scratchAssignment, scratchTurn string
	scratchCleaned                 bool
}

var _ agentturn.WorkspaceDiscarder = (*sealedStopWorkerWorkspace)(nil)

func (workspaces *sealedStopWorkerWorkspace) Capture(_ context.Context, request execution.CaptureRequest) (execution.CaptureRef, error) {
	workspaces.captureCalls++
	if workspaces.sealed.Request != request || workspaces.sealed.SHA256 == "" {
		return execution.CaptureRef{}, execution.ErrNotFound
	}
	workspaces.returnedCaptures = append(workspaces.returnedCaptures, workspaces.sealed)
	return workspaces.sealed, nil
}

func (workspaces *sealedStopWorkerWorkspace) CleanupTurnPaths(ctx context.Context, assignmentID, turnID string, fence workspace.WorkspaceFence) error {
	return fence(ctx, func(ctx context.Context) error {
		if workspaces.beforeCleanup != nil {
			if err := workspaces.beforeCleanup(ctx); err != nil {
				return err
			}
		}
		workspaces.scratchAssignment, workspaces.scratchTurn = assignmentID, turnID
		workspaces.scratchCleaned = true
		return nil
	})
}

func (workspaces *sealedStopWorkerWorkspace) Discard(ctx context.Context, _ execution.WorkspaceRef, fence workspace.WorkspaceFence) error {
	return fence(ctx, func(context.Context) error {
		workspaces.discards++
		return nil
	})
}
