package agentturn_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestStopWorkerClaimsOnlyStopJobsAndAcknowledgesExactRuntimeAbsence(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want acknowledged stop", processed, err)
	}
	if durable.claimQueue != store.AgentTurnRecoveryQueue || durable.claimKind != store.StopStaleRuntimeJobKind ||
		durable.claimOwner != "runtime-stop-worker" || durable.claimDuration != 100*time.Millisecond {
		t.Fatalf("ClaimJobKind() = (%q, %q, %q, %s)", durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimDuration)
	}
	wantLabels := execution.Key{
		AssignmentID: lease.AgentParticipantID, AgentSessionID: lease.AgentSessionID,
		AgentTurnID: lease.AgentTurnID, ExecutionEpoch: 7,
		RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2",
	}
	if cleaner.callCount() != 1 || !reflect.DeepEqual(cleaner.labelsAt(0), wantLabels) {
		t.Fatalf("EnsureAbsent() labels = %#v, want %#v", cleaner.labelsAt(0), wantLabels)
	}
	if durable.acknowledgements != 1 || durable.acknowledged.ID != lease.ID || durable.acknowledged.LeaseToken != lease.LeaseToken {
		t.Fatalf("AcknowledgeRecoveredRuntimeStopped() calls = %d, lease %#v", durable.acknowledgements, durable.acknowledged)
	}
}

func TestStopWorkerPersistsRecoveredOOMBeforeRemovingContainer(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{exitObservation: execution.ExitObservation{
		Exited: true, OOMKilled: true, ExitCode: 137, Resource: "stopped-oom",
	}}
	durable.cleaner = cleaner
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)
	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed || durable.oomID != "stopped-oom" || durable.oomExitCode != 137 ||
		durable.cleanupCallsAtOOM != 0 || cleaner.callCount() != 1 {
		t.Fatalf("stop OOM recovery = (%t, %v), evidence %q/%d before %d cleanups", processed, err,
			durable.oomID, durable.oomExitCode, durable.cleanupCallsAtOOM)
	}
}

func TestStopWorkerKeepsContainerWhenOOMEvidenceCannotBeSaved(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, oomErr: errors.New("store unavailable")}
	cleaner := &recordingRuntimeCleaner{exitObservation: execution.ExitObservation{
		Exited: true, OOMKilled: true, ExitCode: 137, Resource: "stopped-oom",
	}}
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)
	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || cleaner.callCount() != 0 || durable.failureAcknowledgements != 1 {
		t.Fatalf("failed OOM persistence = (%t, %v), cleanup %d, retry %d", processed, err,
			cleaner.callCount(), durable.failureAcknowledgements)
	}
}

func TestStopWorkerNeverStartedCleanupRequiresDurableProofAndRemoval(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation execution.ExitObservation
		proofErr    error
		stopErr     error
		removeErr   error
		wantProof   bool
		wantRemoval bool
		wantSuccess bool
	}{
		{name: "proof authorizes preparation-only cleanup", observation: execution.ExitObservation{NeverStarted: true, Resource: "exact-created"}, wantProof: true, wantRemoval: true, wantSuccess: true},
		{name: "proof failure retains resource", observation: execution.ExitObservation{NeverStarted: true, Resource: "exact-created"}, proofErr: errors.New("proof unavailable"), wantProof: true},
		{name: "stop failure cannot admit proof", observation: execution.ExitObservation{NeverStarted: true, Resource: "exact-created"}, stopErr: execution.ErrUnknownEffect},
		{name: "removal failure cannot acknowledge", observation: execution.ExitObservation{NeverStarted: true, Resource: "exact-created"}, removeErr: execution.ErrUnknownEffect, wantProof: true, wantRemoval: true},
		{name: "missing resource is not proof", observation: execution.ExitObservation{NeverStarted: true}},
		{name: "exit contradicts no-start", observation: execution.ExitObservation{NeverStarted: true, Resource: "exact-created", Exited: true}},
		{name: "running contradicts no-start", observation: execution.ExitObservation{NeverStarted: true, Resource: "exact-created", Running: true}},
		{name: "OOM exit contradicts no-start", observation: execution.ExitObservation{NeverStarted: true, Resource: "exact-created", Exited: true, OOMKilled: true, ExitCode: 137}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := staleRuntimeLease()
			durable := &stopWorkerStore{lease: &lease, neverStartedErr: test.proofErr, cleanupContext: store.AgentTurnRuntimeCleanupContext{AssignmentID: lease.AgentParticipantID, Role: workflow.RoleDeveloper}}
			cleaner := &recordingRuntimeCleaner{exitObservation: test.observation, stopErr: test.stopErr}
			if test.removeErr != nil {
				cleaner.results = []error{test.removeErr}
			}
			workspaces := &recordingWorkspaceDiscarder{captureErr: errors.New("created resource must not be captured")}
			durable.cleaner, durable.workspaces = cleaner, workspaces
			worker, err := agentturn.NewStopWorker(durable, cleaner, workspaces, agentturn.StopWorkerConfig{
				ClaimOwner: "never-started-stop", LeaseDuration: time.Second, HeartbeatInterval: time.Millisecond,
				IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Millisecond,
				TerminalCaptureRequired: func(context.Context, store.JobLease) (bool, error) {
					if durable.neverStartedResource != "exact-created" || len(cleaner.stopLabels) != 1 {
						t.Fatal("capture exemption was consulted before fenced stop and durable proof")
					}
					return false, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			processed, err := worker.ProcessNext(t.Context())
			wantAcknowledgements := 0
			if test.wantSuccess {
				wantAcknowledgements = 1
			}
			if !processed || (err == nil) != test.wantSuccess || (durable.neverStartedResource != "") != test.wantProof || (cleaner.callCount() != 0) != test.wantRemoval || durable.acknowledgements != wantAcknowledgements || len(workspaces.captureRequests) != 0 || durable.oomID != "" {
				t.Fatalf("no-start cleanup = processed %t, error %v, proof %q, removals %d, acknowledgements %d, captures %d", processed, err, durable.neverStartedResource, cleaner.callCount(), durable.acknowledgements, len(workspaces.captureRequests))
			}
		})
	}
}

func TestStopWorkerDiscardsRecoveredReviewerWorkspaceBeforeAcknowledgement(t *testing.T) {
	lease := staleRuntimeLease()
	cleanup := store.AgentTurnRuntimeCleanupContext{
		AssignmentID:     lease.AgentParticipantID,
		Role:             workflow.RoleReviewer,
		DiscardWorkspace: true,
	}
	durable := &stopWorkerStore{lease: &lease, cleanupContext: cleanup}
	discarder := &recordingWorkspaceDiscarder{}
	worker := newStopWorkerWithWorkspace(t, durable, &recordingRuntimeCleaner{}, discarder, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want acknowledged Reviewer cleanup", processed, err)
	}
	if discarder.callCount() != 1 || discarder.assignmentAt(0) != cleanup.AssignmentID {
		t.Fatalf("DiscardWorkspace() calls = %d, assignment %q, want exactly %q", discarder.callCount(), discarder.assignmentAt(0), cleanup.AssignmentID)
	}
	if durable.acknowledgements != 1 || durable.discardSuccessesAtAcknowledgement != 1 {
		t.Fatalf("acknowledgements = %d after %d successful discards", durable.acknowledgements, durable.discardSuccessesAtAcknowledgement)
	}
}

func TestStopWorkerCapturesAfterConfirmedStopBeforeRemovalAndReviewerDiscard(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
		AssignmentID: lease.AgentParticipantID, Role: workflow.RoleReviewer, DiscardWorkspace: true,
	}}
	operations := []string{}
	cleaner := &recordingRuntimeCleaner{operations: &operations}
	workspaces := &recordingWorkspaceDiscarder{operations: &operations}
	worker := newStopWorkerWithWorkspace(t, durable, cleaner, workspaces, time.Second, time.Millisecond, time.Millisecond)
	if processed, err := worker.ProcessNext(context.Background()); !processed || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if !reflect.DeepEqual(operations, []string{"stop", "capture", "remove", "discard"}) {
		t.Fatalf("cleanup ordering = %v", operations)
	}
	wantKey := execution.Key{
		AssignmentID: lease.AgentParticipantID, AgentSessionID: lease.AgentSessionID, AgentTurnID: lease.AgentTurnID,
		ExecutionEpoch: lease.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2",
	}
	want := execution.CaptureRequest{
		Workspace: execution.WorkspaceRef{Key: wantKey}, OperationID: lease.AgentTurnID, Purpose: execution.TerminalCapture,
	}
	if len(workspaces.captureRequests) != 1 || workspaces.captureRequests[0] != want ||
		len(cleaner.stopLabels) != 1 || cleaner.stopLabels[0] != wantKey || workspaces.discardedRefs[0].Key != wantKey {
		t.Fatalf("stop/capture/discard lost epoch identity: captures %#v, stops %#v, discards %#v",
			workspaces.captureRequests, cleaner.stopLabels, workspaces.discardedRefs)
	}
}

func TestStopWorkerCaptureFailureRetainsEnvironmentAndReviewerWorkspaceForRetry(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
		AssignmentID: lease.AgentParticipantID, Role: workflow.RoleReviewer, DiscardWorkspace: true,
	}}
	failure := errors.New("capture transport unavailable")
	cleaner := &recordingRuntimeCleaner{}
	workspaces := &recordingWorkspaceDiscarder{captureErr: failure}
	worker := newStopWorkerWithWorkspace(t, durable, cleaner, workspaces, time.Second, time.Millisecond, time.Millisecond)
	if processed, err := worker.ProcessNext(context.Background()); !processed || !errors.Is(err, failure) {
		t.Fatalf("ProcessNext() = (%t, %v), want retryable capture failure", processed, err)
	}
	if cleaner.callCount() != 0 || workspaces.callCount() != 0 || durable.acknowledgements != 0 || durable.failureAcknowledgements != 1 {
		t.Fatal("capture failure destroyed evidence or acknowledged successful cleanup")
	}
	workspaces.captureErr = nil
	durable.lease = &lease
	if processed, err := worker.ProcessNext(context.Background()); !processed || err != nil {
		t.Fatalf("retry ProcessNext() = (%t, %v)", processed, err)
	}
	if len(workspaces.captureRequests) != 2 || workspaces.captureRequests[0] != workspaces.captureRequests[1] ||
		cleaner.callCount() != 1 || workspaces.callCount() != 1 || durable.acknowledgements != 1 {
		t.Fatal("recovery did not retry the same terminal observation before destruction")
	}
}

func TestStopWorkerReusesSealedCaptureWhenRetryingFailedRemoval(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	failure := errors.New("removal outcome unavailable")
	cleaner := &recordingRuntimeCleaner{results: []error{failure}}
	workspaces := &recordingWorkspaceDiscarder{}
	worker := newStopWorkerWithWorkspace(t, durable, cleaner, workspaces, time.Second, time.Millisecond, time.Millisecond)
	if processed, err := worker.ProcessNext(context.Background()); !processed || !errors.Is(err, failure) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	sealed := workspaces.retainedCapture
	// A successful removal with a lost reply may leave no source on retry.
	workspaces.captureErr = errors.New("original environment is now missing")
	durable.lease = &lease
	if processed, err := worker.ProcessNext(context.Background()); !processed || err != nil {
		t.Fatalf("retry ProcessNext() = (%t, %v)", processed, err)
	}
	if workspaces.sealedCaptures != 1 || workspaces.retainedCapture != sealed || durable.acknowledgements != 1 {
		t.Fatal("removal retry replaced or lost sealed evidence")
	}
}

func TestStopWorkerMissingEnvironmentWithoutCaptureIsNotSuccessfulEvidence(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{}
	workspaces := &recordingWorkspaceDiscarder{captureErr: errors.New("environment and terminal capture missing")}
	worker := newStopWorkerWithWorkspace(t, durable, cleaner, workspaces, time.Second, time.Millisecond, time.Millisecond)
	if processed, err := worker.ProcessNext(context.Background()); !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want unavailable observation", processed, err)
	}
	if cleaner.callCount() != 0 || durable.acknowledgements != 0 || durable.failureAcknowledgements != 1 {
		t.Fatal("missing environment fabricated successful terminal evidence")
	}
}

func TestStopWorkerUnconfirmedStopCannotCaptureOrRemove(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{stopErr: execution.ErrUnknownEffect}
	workspaces := &recordingWorkspaceDiscarder{}
	worker := newStopWorkerWithWorkspace(t, durable, cleaner, workspaces, time.Second, time.Millisecond, time.Millisecond)
	if processed, err := worker.ProcessNext(context.Background()); !processed || !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	if len(workspaces.captureRequests) != 0 || cleaner.callCount() != 0 || durable.acknowledgements != 0 {
		t.Fatal("unknown stop authorized capture or destructive cleanup")
	}
}

func TestStopWorkerCannotAdoptCaptureAfterRecoveryFenceLoss(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{}
	workspaces := &recordingWorkspaceDiscarder{captureHook: func() {
		if durable.inCleanupFence {
			t.Fatal("recovered capture transfer ran inside the cleanup transaction")
		}
		durable.cleanupFenceErr = store.ErrAgentTurnRecoveryFenceLost
	}}
	worker := newStopWorkerWithWorkspace(t, durable, cleaner, workspaces, time.Second, time.Millisecond, time.Millisecond)
	if processed, err := worker.ProcessNext(context.Background()); !processed || !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want post-capture fence loss", processed, err)
	}
	if len(workspaces.captureRequests) != 1 || cleaner.callCount() != 0 || durable.acknowledgements != 0 || durable.failureAcknowledgements != 0 {
		t.Fatal("stale capture adoption removed resources or acknowledged the stale job")
	}
}

func TestStopWorkerPreparationOnlyCleanupUsesDurableCaptureExemption(t *testing.T) {
	lease := staleRuntimeLease()
	for _, fenceErr := range []error{nil, store.ErrAgentTurnRecoveryFenceLost} {
		t.Run(fmt.Sprint(fenceErr), func(t *testing.T) {
			durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
				AssignmentID: lease.AgentParticipantID, Role: workflow.RoleReviewer, DiscardWorkspace: true,
			}}
			cleaner := &recordingRuntimeCleaner{}
			workspaces := &recordingWorkspaceDiscarder{captureErr: errors.New("workspace was never materialized")}
			worker, err := agentturn.NewStopWorker(durable, cleaner, workspaces, agentturn.StopWorkerConfig{
				ClaimOwner: "runtime-stop-worker", LeaseDuration: time.Second, HeartbeatInterval: time.Millisecond,
				IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Millisecond,
				TerminalCaptureRequired: func(_ context.Context, claimed store.JobLease) (bool, error) {
					if len(cleaner.stopLabels) != 1 {
						t.Fatal("capture exemption was evaluated before stop/revocation")
					}
					if claimed.ID != lease.ID || claimed.LeaseToken != lease.LeaseToken {
						t.Fatal("capture exemption did not receive the recovery fence")
					}
					return false, fenceErr
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			processed, err := worker.ProcessNext(context.Background())
			if !processed || !errors.Is(err, fenceErr) {
				t.Fatalf("ProcessNext() = (%t, %v), want %v", processed, err, fenceErr)
			}
			if len(workspaces.captureRequests) != 0 {
				t.Fatal("preparation-only cleanup attempted terminal capture")
			}
			if fenceErr == nil {
				if cleaner.callCount() != 1 || workspaces.callCount() != 1 || durable.acknowledgements != 1 {
					t.Fatal("preparation-only cleanup failed to progress")
				}
			} else if len(cleaner.stopLabels) != 1 || cleaner.callCount() != 0 || workspaces.callCount() != 0 || durable.acknowledgements != 0 {
				t.Fatal("stale capture exemption authorized cleanup")
			}
		})
	}
}

func TestStopWorkerDoesNotDiscardReviewerWorkspaceAfterRecoveryFenceLoss(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
		AssignmentID: lease.AgentParticipantID, Role: workflow.RoleReviewer, DiscardWorkspace: true,
	}, cleanupFenceErr: store.ErrAgentTurnRecoveryFenceLost}
	discarder := &recordingWorkspaceDiscarder{}
	worker := newStopWorkerWithWorkspace(t, durable, &recordingRuntimeCleaner{}, discarder, 100*time.Millisecond, time.Millisecond, time.Millisecond)
	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want lost recovery fence", processed, err)
	}
	if discarder.callCount() != 0 || durable.acknowledgements != 0 || durable.failureAcknowledgements != 0 {
		t.Fatalf("stale stop Job discarded workspace or acknowledged: discards %d, successes %d, failures %d",
			discarder.callCount(), durable.acknowledgements, durable.failureAcknowledgements)
	}
}

func TestStopWorkerRetainsRecoveredDeveloperWorkspace(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
		AssignmentID: lease.AgentParticipantID,
		Role:         workflow.RoleDeveloper,
	}}
	discarder := &recordingWorkspaceDiscarder{}
	worker := newStopWorkerWithWorkspace(t, durable, &recordingRuntimeCleaner{}, discarder, 100*time.Millisecond, 5*time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want acknowledged Developer cleanup", processed, err)
	}
	if discarder.callCount() != 0 {
		t.Fatalf("DiscardWorkspace() calls = %d, want Developer workspace retained", discarder.callCount())
	}
	if durable.acknowledgements != 1 {
		t.Fatalf("acknowledgements = %d, want 1", durable.acknowledgements)
	}
}

func TestStopWorkerReviewerDiscardFailureConsumesOneClaimedAttempt(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContext: store.AgentTurnRuntimeCleanupContext{
		AssignmentID:     lease.AgentParticipantID,
		Role:             workflow.RoleReviewer,
		DiscardWorkspace: true,
	}}
	failure := errors.New("workspace temporarily unavailable")
	discarder := &recordingWorkspaceDiscarder{results: []error{failure}}
	worker := newStopWorkerWithWorkspace(t, durable, &recordingRuntimeCleaner{}, discarder, 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !errors.Is(err, failure) || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want one failed discard attempt", processed, err)
	}
	if discarder.callCount() != 1 || durable.claims.Load() != 1 {
		t.Fatalf("DiscardWorkspace()/ClaimJobKind() calls = (%d, %d), want (1, 1)", discarder.callCount(), durable.claims.Load())
	}
	if durable.failureAcknowledgements != 1 || durable.acknowledgements != 0 {
		t.Fatalf("failure/success acknowledgements = (%d, %d), want (1, 0)", durable.failureAcknowledgements, durable.acknowledgements)
	}
}

func TestStopWorkerCleanupContextFailureConsumesOneClaimedAttempt(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{
		lease: &lease,
		cleanupContext: store.AgentTurnRuntimeCleanupContext{
			AssignmentID: lease.AgentParticipantID,
			Role:         workflow.RoleDeveloper,
		},
		cleanupContextResults: []error{errors.New("database temporarily unavailable")},
	}
	worker := newStopWorker(t, durable, &recordingRuntimeCleaner{}, 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err == nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want one failed context-read attempt", processed, err)
	}
	if durable.cleanupContextCallCount() != 1 || durable.claims.Load() != 1 {
		t.Fatalf("cleanup context/claim calls = (%d, %d), want (1, 1)", durable.cleanupContextCallCount(), durable.claims.Load())
	}
	if durable.failureAcknowledgements != 1 || durable.acknowledgements != 0 {
		t.Fatalf("failure/success acknowledgements = (%d, %d), want (1, 0)", durable.failureAcknowledgements, durable.acknowledgements)
	}
}

func TestStopWorkerReturnsCleanupContextFenceLossWithoutRetry(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, cleanupContextResults: []error{store.ErrAgentTurnRecoveryFenceLost}}
	worker := newStopWorker(t, durable, &recordingRuntimeCleaner{}, 100*time.Millisecond, time.Millisecond, time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, store.ErrAgentTurnRecoveryFenceLost) {
		t.Fatalf("ProcessNext() = (%t, %v), want prompt recovery fence loss", processed, err)
	}
	if durable.cleanupContextCallCount() != 1 || durable.acknowledgements != 0 {
		t.Fatalf("cleanup context/acknowledgement calls = (%d, %d), want (1, 0)", durable.cleanupContextCallCount(), durable.acknowledgements)
	}
}

func TestStopWorkerRuntimeCleanupFailureConsumesOneClaimedAttempt(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	failure := errors.New("Docker temporarily unavailable")
	cleaner := &recordingRuntimeCleaner{results: []error{failure}}
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !errors.Is(err, failure) || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want one failed cleanup attempt", processed, err)
	}
	if cleaner.callCount() != 1 {
		t.Fatalf("EnsureAbsent() calls = %d, want 1 within one claim", cleaner.callCount())
	}
	if durable.claims.Load() != 1 {
		t.Fatalf("ClaimJobKind() calls = %d, want one durable attempt", durable.claims.Load())
	}
	if durable.failureAcknowledgements != 1 || durable.acknowledgements != 0 || durable.failureDelay != 5*time.Millisecond {
		t.Fatalf("failure/success acknowledgements = (%d, %d), delay %s", durable.failureAcknowledgements, durable.acknowledgements, durable.failureDelay)
	}
}

func TestStopWorkerStopsHeartbeatBeforeFailureAcknowledgement(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	failure := errors.New("Docker unavailable")
	cleaner := &recordingRuntimeCleaner{results: []error{failure}, delay: 10 * time.Millisecond}
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || !errors.Is(err, failure) {
		t.Fatalf("ProcessNext() = (%t, %v)", processed, err)
	}
	atAcknowledgement := durable.heartbeatsAtFailureAcknowledgement
	if atAcknowledgement == 0 {
		t.Fatal("HeartbeatJob() did not run during cleanup")
	}
	time.Sleep(5 * time.Millisecond)
	if got := durable.heartbeatCount(); got != atAcknowledgement {
		t.Fatalf("HeartbeatJob() calls after failure acknowledgement = %d, want %d", got, atAcknowledgement)
	}
}

func TestStopWorkerRunReportsFailureWithoutBusyLoop(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{results: []error{errors.New("Docker unavailable")}}
	reported := make(chan error, 1)
	worker, err := agentturn.NewStopWorker(durable, cleaner, &recordingWorkspaceDiscarder{}, agentturn.StopWorkerConfig{
		ClaimOwner: "runtime-stop-worker", LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: time.Millisecond, IdlePollInterval: 50 * time.Millisecond,
		CleanupRetryInterval: time.Millisecond, OnError: func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("OnError received nil")
		}
	case <-time.After(time.Second):
		t.Fatal("OnError was not called")
	}
	time.Sleep(5 * time.Millisecond)
	if claims := durable.claims.Load(); claims != 1 {
		t.Fatalf("ClaimJobKind() calls before idle delay elapsed = %d, want 1", claims)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestStopWorkerCancelsCleanupAndStopsPollingWhenHeartbeatFenceIsStale(t *testing.T) {
	lease := staleRuntimeLease()
	durable := &stopWorkerStore{lease: &lease, heartbeatErr: store.ErrJobLeaseLost}
	cleaner := &recordingRuntimeCleaner{blockUntilCanceled: true}
	durable.cleaner = cleaner
	worker := newStopWorker(t, durable, cleaner, 100*time.Millisecond, time.Millisecond, time.Millisecond)

	err := worker.Run(context.Background())
	if !errors.Is(err, store.ErrJobLeaseLost) {
		t.Fatalf("Run() error = %v, want ErrJobLeaseLost", err)
	}
	if !cleaner.wasCanceled() {
		t.Fatal("EnsureAbsent() context was not canceled after heartbeat fence loss")
	}
	if durable.claims.Load() != 1 {
		t.Fatalf("ClaimJobKind() calls = %d, want no polling after fence loss", durable.claims.Load())
	}
	if durable.acknowledgements != 0 {
		t.Fatalf("stale worker acknowledgements = %d, want zero", durable.acknowledgements)
	}
}

func TestStopWorkerReturnsIdleWithoutCleanup(t *testing.T) {
	durable := &stopWorkerStore{}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, time.Second, time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || processed {
		t.Fatalf("ProcessNext() = (%t, %v), want idle", processed, err)
	}
	if cleaner.callCount() != 0 || durable.acknowledgements != 0 {
		t.Fatalf("idle cleanup/acknowledgements = (%d, %d)", cleaner.callCount(), durable.acknowledgements)
	}
}

func TestStopWorkerRejectsInvalidJobIdentityWithoutCleanupOrAcknowledgement(t *testing.T) {
	lease := staleRuntimeLease()
	lease.AgentSessionID = "invalid-session"
	durable := &stopWorkerStore{lease: &lease}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, time.Second, time.Millisecond, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err == nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want invalid claimed job", processed, err)
	}
	if strings.Contains(err.Error(), lease.AgentSessionID) {
		t.Fatalf("ProcessNext() error exposes invalid identity: %v", err)
	}
	if cleaner.callCount() != 0 || durable.acknowledgements != 0 {
		t.Fatalf("invalid identity cleanup/acknowledgements = (%d, %d)", cleaner.callCount(), durable.acknowledgements)
	}
}

func TestStopWorkerRunPollsWhileNoJobIsAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	durable := &stopWorkerStore{}
	cleaner := &recordingRuntimeCleaner{}
	worker := newStopWorker(t, durable, cleaner, time.Second, time.Millisecond, time.Millisecond)
	go func() {
		for durable.claims.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	err := worker.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if durable.claims.Load() < 3 {
		t.Fatalf("ClaimJobKind() calls = %d, want idle polling", durable.claims.Load())
	}
	if cleaner.callCount() != 0 {
		t.Fatalf("EnsureAbsent() calls = %d without a job", cleaner.callCount())
	}
}

func TestNewStopWorkerValidatesConfiguration(t *testing.T) {
	valid := agentturn.StopWorkerConfig{
		ClaimOwner: "runtime-stop-worker", LeaseDuration: 2 * time.Microsecond,
		HeartbeatInterval: time.Microsecond, IdlePollInterval: time.Microsecond,
		CleanupRetryInterval: time.Microsecond,
	}
	for _, test := range []struct {
		name   string
		mutate func(*agentturn.StopWorkerConfig)
	}{
		{name: "empty owner", mutate: func(config *agentturn.StopWorkerConfig) { config.ClaimOwner = "" }},
		{name: "zero lease", mutate: func(config *agentturn.StopWorkerConfig) { config.LeaseDuration = 0 }},
		{name: "zero heartbeat", mutate: func(config *agentturn.StopWorkerConfig) { config.HeartbeatInterval = 0 }},
		{name: "heartbeat equals lease", mutate: func(config *agentturn.StopWorkerConfig) { config.HeartbeatInterval = config.LeaseDuration }},
		{name: "zero poll", mutate: func(config *agentturn.StopWorkerConfig) { config.IdlePollInterval = 0 }},
		{name: "zero cleanup retry", mutate: func(config *agentturn.StopWorkerConfig) { config.CleanupRetryInterval = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := agentturn.NewStopWorker(&stopWorkerStore{}, &recordingRuntimeCleaner{}, &recordingWorkspaceDiscarder{}, config); err == nil {
				t.Fatal("NewStopWorker() error = nil, want validation error")
			}
		})
	}
	var nilDiscarder *recordingWorkspaceDiscarder
	if _, err := agentturn.NewStopWorker(&stopWorkerStore{}, &recordingRuntimeCleaner{}, nilDiscarder, valid); err == nil {
		t.Fatal("NewStopWorker() with nil workspace discarder error = nil, want validation error")
	}
}

func newStopWorker(t *testing.T, durable *stopWorkerStore, cleaner *recordingRuntimeCleaner, lease, heartbeat, retry time.Duration) *agentturn.StopWorker {
	t.Helper()
	return newStopWorkerWithWorkspace(t, durable, cleaner, &recordingWorkspaceDiscarder{}, lease, heartbeat, retry)
}

func newStopWorkerWithWorkspace(t *testing.T, durable *stopWorkerStore, cleaner *recordingRuntimeCleaner, workspaces *recordingWorkspaceDiscarder, lease, heartbeat, retry time.Duration) *agentturn.StopWorker {
	t.Helper()
	durable.cleaner = cleaner
	durable.workspaces = workspaces
	if durable.cleanupContext.AssignmentID == "" && durable.lease != nil {
		durable.cleanupContext = store.AgentTurnRuntimeCleanupContext{
			AssignmentID: durable.lease.AgentParticipantID,
			Role:         workflow.RoleDeveloper,
		}
	}
	worker, err := agentturn.NewStopWorker(durable, cleaner, workspaces, agentturn.StopWorkerConfig{
		ClaimOwner: "runtime-stop-worker", LeaseDuration: lease, HeartbeatInterval: heartbeat,
		IdlePollInterval: time.Millisecond, CleanupRetryInterval: retry,
	})
	if err != nil {
		t.Fatalf("NewStopWorker() error = %v", err)
	}
	return worker
}

func staleRuntimeLease() store.JobLease {
	return store.JobLease{Job: store.Job{
		JobSpec: store.JobSpec{
			Queue: store.AgentTurnRecoveryQueue, Kind: store.StopStaleRuntimeJobKind,
			AgentParticipantID: "10000000-0000-4000-8000-000000000001",
			AgentSessionID:     "20000000-0000-4000-8000-000000000001",
			AgentTurnID:        "30000000-0000-4000-8000-000000000001",
			ExecutionEpoch:     7,
		},
		ID: "40000000-0000-4000-8000-000000000001", Status: store.JobLeased,
		AttemptCount: 1, LeaseOwner: "runtime-stop-worker", LeaseToken: "50000000-0000-4000-8000-000000000001",
	}, Attempt: 1}
}

type stopWorkerStore struct {
	mutex sync.Mutex
	lease *store.JobLease

	claims                             atomic.Int32
	claimQueue, claimKind, claimOwner  string
	claimDuration                      time.Duration
	heartbeats                         int
	heartbeatExtension                 time.Duration
	heartbeatErr                       error
	acknowledgements                   int
	acknowledged                       store.JobLease
	cleanupCallsAtAcknowledgement      int
	cleaner                            *recordingRuntimeCleaner
	cleanupContext                     store.AgentTurnRuntimeCleanupContext
	cleanupContextResults              []error
	cleanupContextCalls                int
	cleanupContextLease                store.JobLease
	cleanupFenceErr                    error
	discardCallsAtAcknowledgement      int
	discardSuccessesAtAcknowledgement  int
	workspaces                         *recordingWorkspaceDiscarder
	failureAcknowledgements            int
	failureAcknowledged                store.JobLease
	failureDelay                       time.Duration
	failureErr                         error
	failureAcknowledgementErr          error
	heartbeatsAtFailureAcknowledgement int
	oomID                              string
	oomExitCode                        int
	oomErr                             error
	cleanupCallsAtOOM                  int
	inCleanupFence                     bool
	neverStartedResource               string
	neverStartedErr                    error
}

func (durable *stopWorkerStore) RecordRecoveredRuntimeNeverStarted(_ context.Context, _ store.JobLease, resource string) error {
	durable.neverStartedResource = resource
	return durable.neverStartedErr
}

func (durable *stopWorkerStore) RecordRecoveredRuntimeOOM(_ context.Context, _ store.JobLease, id string, code int) error {
	durable.oomID, durable.oomExitCode = id, code
	if durable.cleaner != nil {
		durable.cleanupCallsAtOOM = durable.cleaner.callCount()
	}
	return durable.oomErr
}

func (durable *stopWorkerStore) ClaimJobKind(_ context.Context, queue, kind, owner string, duration time.Duration) (*store.JobLease, error) {
	durable.claims.Add(1)
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.claimQueue, durable.claimKind, durable.claimOwner, durable.claimDuration = queue, kind, owner, duration
	lease := durable.lease
	durable.lease = nil
	return lease, nil
}

func (durable *stopWorkerStore) HeartbeatJob(_ context.Context, _ store.JobLease, extension time.Duration) error {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.heartbeats++
	durable.heartbeatExtension = extension
	return durable.heartbeatErr
}

func (durable *stopWorkerStore) heartbeatCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.heartbeats
}

func (durable *stopWorkerStore) GetAgentTurnRuntimeCleanupContext(_ context.Context, lease store.JobLease) (store.AgentTurnRuntimeCleanupContext, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.cleanupContextCalls++
	durable.cleanupContextLease = lease
	var result error
	if len(durable.cleanupContextResults) != 0 {
		result = durable.cleanupContextResults[0]
		durable.cleanupContextResults = durable.cleanupContextResults[1:]
	}
	cleanup := durable.cleanupContext
	if cleanup.RuntimeProfileName == "" {
		cleanup.RuntimeProfileName = "pi-rpc"
		cleanup.RuntimeProfileVersion = "v2"
	}
	return cleanup, result
}

func (durable *stopWorkerStore) cleanupContextCallCount() int {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	return durable.cleanupContextCalls
}

func (durable *stopWorkerStore) WithRecoveredRuntimeCleanupFence(ctx context.Context, _ store.JobLease, operation func(context.Context) error) error {
	if durable.cleanupFenceErr != nil {
		return durable.cleanupFenceErr
	}
	durable.inCleanupFence = true
	defer func() { durable.inCleanupFence = false }()
	return operation(ctx)
}

func (durable *stopWorkerStore) AcknowledgeRecoveredRuntimeStopped(_ context.Context, lease store.JobLease) (store.AgentTurnRecovery, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.acknowledgements++
	durable.acknowledged = lease
	if durable.cleaner != nil {
		durable.cleanupCallsAtAcknowledgement = durable.cleaner.callCount()
	}
	if durable.workspaces != nil {
		durable.discardCallsAtAcknowledgement = durable.workspaces.callCount()
		durable.discardSuccessesAtAcknowledgement = durable.workspaces.successCount()
	}
	return store.AgentTurnRecovery{}, nil
}

func (durable *stopWorkerStore) AcknowledgeRecoveredRuntimeStopFailure(_ context.Context, lease store.JobLease, cause error, delay time.Duration) (store.AgentTurnRuntimeStopFailureAcknowledgement, error) {
	durable.mutex.Lock()
	defer durable.mutex.Unlock()
	durable.failureAcknowledgements++
	durable.failureAcknowledged = lease
	durable.failureErr = cause
	durable.failureDelay = delay
	durable.heartbeatsAtFailureAcknowledgement = durable.heartbeats
	return store.AgentTurnRuntimeStopFailureAcknowledgement{RetryScheduled: true}, durable.failureAcknowledgementErr
}

type recordingRuntimeCleaner struct {
	mutex              sync.Mutex
	results            []error
	labels             []execution.Key
	blockUntilCanceled bool
	canceled           bool
	delay              time.Duration
	exitObservation    execution.ExitObservation
	stopErr            error
	stopLabels         []execution.Key
	operations         *[]string
}

func (cleaner *recordingRuntimeCleaner) EnsureKeyStopped(_ context.Context, key execution.Key) error {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	cleaner.stopLabels = append(cleaner.stopLabels, key)
	if cleaner.operations != nil {
		*cleaner.operations = append(*cleaner.operations, "stop")
	}
	return cleaner.stopErr
}

func (cleaner *recordingRuntimeCleaner) ObserveKeyExit(context.Context, execution.Key) (execution.ExitObservation, error) {
	return cleaner.exitObservation, nil
}

func (cleaner *recordingRuntimeCleaner) EnsureKeyAbsent(ctx context.Context, labels execution.Key) error {
	cleaner.mutex.Lock()
	if cleaner.operations != nil {
		*cleaner.operations = append(*cleaner.operations, "remove")
	}
	cleaner.labels = append(cleaner.labels, labels)
	block := cleaner.blockUntilCanceled
	var result error
	if len(cleaner.results) != 0 {
		result = cleaner.results[0]
		cleaner.results = cleaner.results[1:]
	}
	cleaner.mutex.Unlock()
	if cleaner.delay > 0 {
		timer := time.NewTimer(cleaner.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if block {
		<-ctx.Done()
		cleaner.mutex.Lock()
		cleaner.canceled = true
		cleaner.mutex.Unlock()
		return ctx.Err()
	}
	return result
}

func (cleaner *recordingRuntimeCleaner) callCount() int {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	return len(cleaner.labels)
}

func (cleaner *recordingRuntimeCleaner) labelsAt(index int) execution.Key {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	if index >= len(cleaner.labels) {
		return execution.Key{}
	}
	return cleaner.labels[index]
}

func (cleaner *recordingRuntimeCleaner) wasCanceled() bool {
	cleaner.mutex.Lock()
	defer cleaner.mutex.Unlock()
	return cleaner.canceled
}

type recordingWorkspaceDiscarder struct {
	mutex           sync.Mutex
	results         []error
	assignmentIDs   []string
	successes       int
	captureErr      error
	captureRequests []execution.CaptureRequest
	retainedCapture execution.CaptureRef
	sealedCaptures  int
	operations      *[]string
	discardedRefs   []execution.WorkspaceRef
	captureHook     func()
}

func (discarder *recordingWorkspaceDiscarder) Capture(_ context.Context, request execution.CaptureRequest) (execution.CaptureRef, error) {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	discarder.captureRequests = append(discarder.captureRequests, request)
	if discarder.captureHook != nil {
		discarder.captureHook()
	}
	if discarder.operations != nil {
		*discarder.operations = append(*discarder.operations, "capture")
	}
	if discarder.retainedCapture.Request == request {
		return discarder.retainedCapture, nil
	}
	if discarder.captureErr != nil {
		return execution.CaptureRef{}, discarder.captureErr
	}
	discarder.retainedCapture = execution.CaptureRef{Request: request, SHA256: strings.Repeat("a", 64)}
	discarder.sealedCaptures++
	return discarder.retainedCapture, nil
}

func (discarder *recordingWorkspaceDiscarder) DiscardWorkspace(assignmentID string) error {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	if discarder.operations != nil {
		*discarder.operations = append(*discarder.operations, "discard")
	}
	discarder.assignmentIDs = append(discarder.assignmentIDs, assignmentID)
	var result error
	if len(discarder.results) != 0 {
		result = discarder.results[0]
		discarder.results = discarder.results[1:]
	}
	if result == nil {
		discarder.successes++
	}
	return result
}

func (discarder *recordingWorkspaceDiscarder) Discard(ctx context.Context, ref execution.WorkspaceRef, fence workspace.WorkspaceFence) error {
	return fence(ctx, func(context.Context) error {
		discarder.discardedRefs = append(discarder.discardedRefs, ref)
		return discarder.DiscardWorkspace(ref.Key.AssignmentID)
	})
}

func (*recordingWorkspaceDiscarder) CleanupTurnPaths(ctx context.Context, _, _ string, fence workspace.WorkspaceFence) error {
	return fence(ctx, func(context.Context) error { return nil })
}

func (discarder *recordingWorkspaceDiscarder) callCount() int {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	return len(discarder.assignmentIDs)
}

func (discarder *recordingWorkspaceDiscarder) assignmentAt(index int) string {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	if index >= len(discarder.assignmentIDs) {
		return ""
	}
	return discarder.assignmentIDs[index]
}

func (discarder *recordingWorkspaceDiscarder) successCount() int {
	discarder.mutex.Lock()
	defer discarder.mutex.Unlock()
	return discarder.successes
}
