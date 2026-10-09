package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// Inject lifecycle faults without changing the shared fake's behavior.
type lifecycleFaultPlatform struct {
	platform
	pause func(context.Context, metadata) error
	stop  func(context.Context, metadata) error
}

func (driver lifecycleFaultPlatform) Pause(ctx context.Context, actual metadata) error {
	if driver.pause != nil {
		return driver.pause(ctx, actual)
	}
	return driver.platform.Pause(ctx, actual)
}

func (driver lifecycleFaultPlatform) Stop(ctx context.Context, actual metadata) error {
	if driver.stop != nil {
		return driver.stop(ctx, actual)
	}
	return driver.platform.Stop(ctx, actual)
}

func lifecycleFixture(t *testing.T, started bool) (*Backend, *fakePlatform, execution.Request) {
	t.Helper()
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	request.Workspace = execution.WorkspaceRef{Key: request.Key}
	for _, resource := range []string{"workspace", "state"} {
		location := backend.location(request.Key, resource)
		if err := os.MkdirAll(location, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(location, "retained"), []byte(resource), 0600); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(t.Context(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := backend.locked(t.Context(), request.Key, func(record *operation) error {
		record.Started = started
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return backend, driver, request
}

func assertUnflushedRetained(t *testing.T, backend *Backend, request execution.Request) {
	t.Helper()
	if err := backend.locked(t.Context(), request.Key, func(record *operation) error {
		if !record.Stopped || record.Flushed || record.Removed || !record.Revoked {
			t.Fatalf("unflushed stop journal = %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sealed := false
	err := backend.ReadWorkspace(t.Context(), execution.CaptureRequest{Workspace: request.Workspace, Purpose: execution.TerminalCapture}, func(io.Reader) error {
		sealed = true
		return nil
	})
	if !errors.Is(err, execution.ErrUnknownEffect) || sealed {
		t.Fatalf("unflushed capture = %v, sealed = %t", err, sealed)
	}
	if err := backend.EnsureKeyAbsent(t.Context(), request.Key); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("unflushed removal = %v", err)
	}
	for _, resource := range []string{"workspace", "state"} {
		content, err := os.ReadFile(filepath.Join(backend.location(request.Key, resource), "retained"))
		if err != nil || string(content) != resource {
			t.Fatalf("retained %s = %q, %v", resource, content, err)
		}
	}
}

func TestRequiredFlushFailureStillConfirmsWholeEnvironmentStop(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	backend.platform = lifecycleFaultPlatform{platform: driver, pause: func(context.Context, metadata) error { return io.EOF }}
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); err != nil {
		t.Fatalf("force stop after failed flush = %v", err)
	}
	if driver.stops != 1 || driver.resources[nameFor(request.Key)].Status != "Stopped" {
		t.Fatal("failed flush left the whole VM executing")
	}
	observed, err := backend.ObserveKeyExit(t.Context(), request.Key)
	if err != nil || !observed.Exited || observed.Running || observed.NeverStarted || observed.ExitCodeKnown || observed.OOMKilled || observed.Scope != "execution-environment" {
		t.Fatalf("unknown force-stop cause = %+v, %v", observed, err)
	}
	assertUnflushedRetained(t, backend, request)
	if driver.removes != 0 {
		t.Fatal("failed writeback authorized disposal")
	}
}

func TestStalledRequiredFlushReservesStopTime(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var pauseDeadline time.Time
	backend.platform = lifecycleFaultPlatform{platform: driver,
		pause: func(ctx context.Context, _ metadata) error {
			pauseDeadline, _ = ctx.Deadline()
			<-ctx.Done()
			return ctx.Err()
		},
		stop: func(stopCtx context.Context, actual metadata) error {
			if err := stopCtx.Err(); err != nil {
				return err
			}
			if ctx.Err() != nil {
				t.Fatal("flush consumed the entire caller stop budget")
			}
			return driver.Stop(stopCtx, actual)
		},
	}
	if err := backend.EnsureKeyStopped(ctx, request.Key); err != nil {
		t.Fatalf("stalled pause cleanup = %v", err)
	}
	deadline, _ := ctx.Deadline()
	if pauseDeadline.IsZero() || deadline.Sub(pauseDeadline) < 700*time.Millisecond {
		t.Fatalf("flush deadline %v did not reserve stop time before %v", pauseDeadline, deadline)
	}
	assertUnflushedRetained(t, backend, request)
}

func TestCancellationDuringFlushStopsExactRevokedVMWithoutReceipt(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend.platform = lifecycleFaultPlatform{platform: driver,
		pause: func(ctx context.Context, _ metadata) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
		stop: func(ctx context.Context, actual metadata) error {
			if _, bounded := ctx.Deadline(); !bounded || ctx.Err() != nil {
				t.Fatal("revoked exact cleanup has no independent finite stop budget")
			}
			return driver.Stop(ctx, actual)
		},
	}
	if err := backend.EnsureKeyStopped(ctx, request.Key); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost fence stop receipt = %v", err)
	}
	if driver.resources[nameFor(request.Key)].Status != "Stopped" {
		t.Fatal("cancellation left the revoked VM running")
	}
	assertUnflushedRetained(t, backend, request)
}

func TestCrashedPreparationCanBeRemovedButStartedCrashCannot(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparation", true: "started"}[started], func(t *testing.T) {
			backend, driver, request := lifecycleFixture(t, started)
			actual := driver.resources[nameFor(request.Key)]
			actual.Status = "Crashed"
			driver.resources[actual.Name] = actual
			backend.platform = lifecycleFaultPlatform{platform: driver, stop: func(context.Context, metadata) error {
				t.Fatal("terminal crashed VM was sent a live Kill")
				return nil
			}}
			observed, err := backend.ObserveKeyExit(t.Context(), request.Key)
			if err != nil || observed.NeverStarted != !started || observed.Exited != started || observed.ExitCodeKnown || observed.OOMKilled || observed.Running {
				t.Fatalf("crashed exit = %+v, %v", observed, err)
			}
			if err := backend.EnsureKeyStopped(t.Context(), request.Key); err != nil {
				t.Fatalf("crashed stop = %v", err)
			}
			if started {
				assertUnflushedRetained(t, backend, request)
			} else if err := backend.EnsureKeyAbsent(t.Context(), request.Key); err != nil || len(driver.resources) != 0 {
				t.Fatalf("NeverStarted crashed removal = %v, resources = %v", err, driver.resources)
			}
		})
	}
}

func TestCrashAllowsOfflineWriterBoundaryOnlyForTerminalInventory(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	for _, status := range []string{"Stopped", "Crashed", "Running", "Paused", "Starting", "Draining", "Created", "Unknown"} {
		actual := driver.resources[nameFor(request.Key)]
		actual.Status = status
		driver.resources[actual.Name] = actual
		err := backend.CheckStateRecoveryBoundary(t.Context())
		wantAllowed := status == "Stopped" || status == "Crashed"
		if (err == nil) != wantAllowed {
			t.Fatalf("offline writer boundary for %s = %v", status, err)
		}
	}
}

func TestDuplicatePauseFailureDoesNotLeaveAnyVMRunningOrAuthorizeCapture(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	duplicate := driver.resources[nameFor(request.Key)]
	duplicate.Name, duplicate.ID = "duplicate", "duplicate-id"
	driver.resources[duplicate.Name] = duplicate
	backend.platform = lifecycleFaultPlatform{platform: driver, pause: func(_ context.Context, actual metadata) error {
		if actual.ID == duplicate.ID {
			return io.EOF
		}
		return nil
	}}
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); err != nil {
		t.Fatal(err)
	}
	if driver.stops != 2 {
		t.Fatalf("whole-key stops = %d", driver.stops)
	}
	for _, actual := range driver.resources {
		if actual.Status != "Stopped" {
			t.Fatalf("duplicate survived termination: %+v", actual)
		}
	}
	if err := backend.EnsureKeyAbsent(t.Context(), request.Key); !errors.Is(err, execution.ErrUnknownEffect) || driver.removes != 0 {
		t.Fatalf("unflushed duplicate removal = %v, removes = %d", err, driver.removes)
	}
}

func TestCrashedDuplicatesAndInventoryCleanupUseExactTerminalIdentity(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, false)
	main := driver.resources[nameFor(request.Key)]
	main.Status = "Crashed"
	driver.resources[main.Name] = main
	duplicate := main
	duplicate.Name, duplicate.ID = "duplicate", "duplicate-id"
	driver.resources[duplicate.Name] = duplicate
	backend.platform = lifecycleFaultPlatform{platform: driver, stop: func(context.Context, metadata) error {
		t.Fatal("crashed resource was treated as live")
		return nil
	}}
	if err := backend.EnsureKeyAbsent(t.Context(), request.Key); err != nil || len(driver.resources) != 0 {
		t.Fatalf("crashed preparation duplicates removal = %v", err)
	}
	for _, cleanup := range []string{"orphan", "malformed", "expired-diagnostic"} {
		actual := main
		actual.Name, actual.ID = cleanup, cleanup+"-id"
		if cleanup == "malformed" {
			actual.Labels = map[string]string{namespaceLabel: backend.options.Namespace}
		} else if cleanup == "expired-diagnostic" {
			actual.Labels = map[string]string{namespaceLabel: backend.options.Namespace, diagnosticLabel: "true", expiryLabel: "1"}
		}
		driver.resources[actual.Name] = actual
		var err error
		if cleanup == "orphan" {
			err = backend.EnsureOrphansAbsent(t.Context(), request.Key, []execution.Ref{resourceRef(actual)})
		} else if cleanup == "expired-diagnostic" {
			_, err = backend.List(t.Context())
		} else {
			err = backend.EnsureMalformedAbsent(t.Context(), snapshotMalformed(actual.Name, actual.ID))
		}
		if err != nil || len(driver.resources) != 0 {
			t.Fatalf("crashed %s removal = %v", cleanup, err)
		}
	}
}

func TestFlushFailureCannotForceStopSameNameReplacement(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	backend.platform = lifecycleFaultPlatform{platform: driver, pause: func(_ context.Context, actual metadata) error {
		actual.ID = "replacement-id"
		driver.resources[actual.Name] = actual
		return io.EOF
	}}
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("same-name replacement stop = %v", err)
	}
	if driver.stops != 0 || driver.removes != 0 {
		t.Fatal("cleanup acted on a same-name replacement")
	}
}

func TestPreparationFlushFailureDoesNotPreventNeverStartedRemoval(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, false)
	backend.platform = lifecycleFaultPlatform{platform: driver, pause: func(context.Context, metadata) error { return io.EOF }}
	observed, err := backend.ObserveKeyExit(t.Context(), request.Key)
	if err != nil || !observed.NeverStarted {
		t.Fatalf("preparation exit = %+v, %v", observed, err)
	}
	if err := backend.EnsureKeyAbsent(t.Context(), request.Key); err != nil || len(driver.resources) != 0 || driver.stops != 1 || driver.removes != 1 {
		t.Fatalf("preparation-only failed-flush cleanup = %v, stops/removes = %d/%d", err, driver.stops, driver.removes)
	}
}

func TestFailedPrimaryStopStillAttemptsDuplicateTermination(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	main := driver.resources[nameFor(request.Key)]
	duplicate := main
	duplicate.Name, duplicate.ID = "duplicate", "duplicate-id"
	driver.resources[duplicate.Name] = duplicate
	backend.platform = lifecycleFaultPlatform{platform: driver, stop: func(ctx context.Context, actual metadata) error {
		if actual.ID == main.ID {
			return io.EOF
		}
		return driver.Stop(ctx, actual)
	}}
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("failed primary stop receipt = %v", err)
	}
	if driver.resources[duplicate.Name].Status != "Stopped" || driver.resources[main.Name].Status != "Running" || driver.removes != 0 {
		t.Fatal("failed primary stop abandoned duplicate termination or removed evidence")
	}
	if err := backend.locked(t.Context(), request.Key, func(record *operation) error {
		if record.Stopped || record.Removed || !record.Flushed {
			t.Fatalf("failed stop lost successful writeback evidence or issued receipt: %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLostForceStopReplyIsSettledByExactTerminalObservation(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	backend.platform = lifecycleFaultPlatform{platform: driver,
		pause: func(context.Context, metadata) error { return io.EOF },
		stop: func(ctx context.Context, actual metadata) error {
			if err := driver.Stop(ctx, actual); err != nil {
				return err
			}
			return io.EOF
		},
	}
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); err != nil {
		t.Fatalf("confirmed force stop with lost reply = %v", err)
	}
	assertUnflushedRetained(t, backend, request)
}

type unavailableTerminalWorkspace struct {
	*sealedStopWorkerWorkspace
	backend *Backend
}

func (workspaces unavailableTerminalWorkspace) Capture(ctx context.Context, request execution.CaptureRequest) (execution.CaptureRef, error) {
	workspaces.captureCalls++
	err := workspaces.backend.ReadWorkspace(ctx, request, func(io.Reader) error {
		return errors.New("unflushed evidence unexpectedly reached sealer")
	})
	return execution.CaptureRef{}, err
}

func TestStopWorkerRetainsUnflushedStartedVMWithoutAcknowledgement(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	backend.platform = lifecycleFaultPlatform{platform: driver, pause: func(context.Context, metadata) error { return io.EOF }}
	workspaces := &sealedStopWorkerWorkspace{}
	durable, _ := newNativeStopWorker(t, backend, request.Key, workspaces)
	// Exercise the real backend capture boundary using the existing worker/store
	// fixture; no fake sealed receipt can stand in for unavailable writeback.
	worker, err := agentturn.NewStopWorker(durable, backend, unavailableTerminalWorkspace{workspaces, backend}, agentturn.StopWorkerConfig{
		ClaimOwner: durable.lease.LeaseOwner, LeaseDuration: time.Minute, HeartbeatInterval: time.Second,
		IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessNext(t.Context())
	if !processed || !errors.Is(err, execution.ErrUnknownEffect) || len(durable.failures) != 1 {
		t.Fatalf("unavailable terminal corroboration = %t, %v, failures = %v", processed, err, durable.failures)
	}
	if len(durable.stopped) != 0 || durable.oomRecords != 0 || durable.neverStartedRecords != 0 || workspaces.scratchCleaned || workspaces.discards != 0 || driver.removes != 0 {
		t.Fatal("unflushed stop fabricated exit cause, acknowledged recovery, or authorized disposal")
	}
	if driver.resources[nameFor(request.Key)].Status != "Stopped" || workspaces.captureCalls != 1 {
		t.Fatal("unavailable writeback prevented confirmed VM termination or capture attempt")
	}
}

func TestConfirmedWritebackSurvivesCrashedTerminalStatus(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); err != nil {
		t.Fatal(err)
	}
	actual := driver.resources[nameFor(request.Key)]
	actual.Status = "Crashed"
	driver.resources[actual.Name] = actual
	sealed := false
	err := backend.ReadWorkspace(t.Context(), execution.CaptureRequest{Workspace: request.Workspace, Purpose: execution.TerminalCapture}, func(source io.Reader) error {
		sealed = true
		_, err := io.Copy(io.Discard, source)
		return err
	})
	if err != nil || !sealed {
		t.Fatalf("positively flushed terminal capture = %v, sealed = %t", err, sealed)
	}
}

func TestCrashedOtherEpochDoesNotBlockQuiescentWorkspaceCapture(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	otherKey := request.Key
	otherKey.ExecutionEpoch++
	content, err := json.Marshal(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	other := metadata{ID: "other-epoch", Name: nameFor(otherKey), Status: "Running", Labels: map[string]string{
		namespaceLabel: backend.options.Namespace,
		identityLabel:  string(content),
		digestLabel:    backend.specificationDigest(request),
	}}
	for _, status := range []string{"Running", "Crashed"} {
		other.Status = status
		driver.resources[other.Name] = other
		sealed := false
		err := backend.ReadWorkspace(t.Context(), execution.CaptureRequest{Workspace: request.Workspace, Purpose: execution.PublicationCapture}, func(source io.Reader) error {
			sealed = true
			_, err := io.Copy(io.Discard, source)
			return err
		})
		if status == "Running" && (!errors.Is(err, execution.ErrConflict) || sealed) {
			t.Fatalf("active other-epoch capture = %v, sealed = %t", err, sealed)
		}
		if status == "Crashed" && (err != nil || !sealed) {
			t.Fatalf("crashed other-epoch capture = %v, sealed = %t", err, sealed)
		}
	}
}

func TestMissingMainStillTerminatesRemainingUnflushedDuplicate(t *testing.T) {
	backend, driver, request := lifecycleFixture(t, true)
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); err != nil {
		t.Fatal(err)
	}
	duplicate := driver.resources[nameFor(request.Key)]
	duplicate.Name, duplicate.ID, duplicate.Status = "duplicate", "duplicate-id", "Running"
	driver.resources[duplicate.Name] = duplicate
	delete(driver.resources, nameFor(request.Key))
	backend.platform = lifecycleFaultPlatform{platform: driver, pause: func(context.Context, metadata) error { return io.EOF }}
	if err := backend.EnsureKeyStopped(t.Context(), request.Key); err != nil {
		t.Fatalf("missing primary duplicate stop = %v", err)
	}
	if driver.resources[duplicate.Name].Status != "Stopped" {
		t.Fatal("authoritative primary absence left a same-key VM executing")
	}
	if err := backend.EnsureKeyAbsent(t.Context(), request.Key); !errors.Is(err, execution.ErrUnknownEffect) || driver.removes != 0 {
		t.Fatalf("missing main/unflushed duplicate removal = %v, removes = %d", err, driver.removes)
	}
}
