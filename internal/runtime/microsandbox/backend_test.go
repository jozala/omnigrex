package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func testRequest() execution.Request {
	return execution.Request{Key: execution.Key{AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001", AgentTurnID: "30000000-0000-4000-8000-000000000001", ExecutionEpoch: 1, RuntimeProfileName: "protocol", RuntimeProfileVersion: "v1"}, Image: "docker.io/library/alpine@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412", Platform: execution.Platform{OS: "linux", Architecture: "amd64"}, User: "10001:10001", WorkingDir: "/tmp", MemoryBytes: 512 << 20, PIDsLimit: 128, Command: []string{"/bin/sh", "-c", "echo protocol"}}
}
func testBackend(t *testing.T) (*Backend, *fakePlatform, *fakeLedger) {
	t.Helper()
	root := t.TempDir()
	driver := &fakePlatform{resources: map[string]metadata{}}
	ledger := &fakeLedger{records: map[execution.Key]execution.Reservation{}}
	backend := &Backend{options: Options{Home: filepath.Join(root, "control"), WorkspaceRoot: filepath.Join(root, "workspaces"), StateRoot: filepath.Join(root, "state"), ToolRoot: filepath.Join(root, "tools"), Namespace: "tests", CPUs: 1, RootDiskMiB: 4096, MaximumDuration: time.Minute}, platform: driver, ledger: ledger, inputs: newInputBindings()}
	return backend, driver, ledger
}

func TestUnknownCreationRequiresExactRecoveryAndRevokesFutureDispatch(t *testing.T) {
	ctx := context.Background()
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	driver.createErr = errors.New("lost response")
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(ctx, request, io.Discard); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("ambiguous creation=%v", err)
	}
	// Simulate restart: only the journal, ledger, and provider inventory survive.
	restarted := *backend
	if err := restarted.EnsureKeyAbsent(ctx, request.Key); err != nil {
		t.Fatal(err)
	}
	if driver.stops != 1 || driver.removes != 1 || !ledger.records[request.Key].Revoked {
		t.Fatal("exact environment was not stopped, removed, and revoked")
	}
	if _, err := engine.Create(ctx, request, io.Discard); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("revoked create=%v", err)
	}
	if driver.creates != 1 {
		t.Fatal("lost acknowledgement dispatched a replacement")
	}
}

func TestUnresolvedCreationAndFailedCleanupRemainUnknown(t *testing.T) {
	for _, phase := range []string{"not visible", "stop failed", "remove failed", "replacement"} {
		t.Run(phase, func(t *testing.T) {
			backend, driver, ledger := testBackend(t)
			request := testRequest()
			driver.createErr = errors.New("lost response")
			engine, _ := backend.New(request, ledger)
			_, _ = engine.Create(context.Background(), request, io.Discard)
			switch phase {
			case "not visible":
				driver.resources = map[string]metadata{}
			case "stop failed":
				driver.stopErr = errors.New("unavailable")
			case "remove failed":
				driver.removeErr = errors.New("unavailable")
			case "replacement":
				actual := driver.resources[nameFor(request.Key)]
				actual.ID = "other"
				driver.resources[actual.Name] = actual
				record := operation{Key: request.Key, Digest: requestDigest(request), ResourceID: "original", Created: true}
				if err := backend.persist(request.Key, &record); err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.EnsureKeyAbsent(context.Background(), request.Key); err == nil {
				t.Fatal("uncertain cleanup issued receipt")
			}
			if phase == "replacement" && (driver.stops > 0 || driver.removes > 0) {
				t.Fatal("stale cleanup acted on a replacement")
			}
		})
	}
}

func TestInventoryPaginationDuplicatesMalformedAndOwnership(t *testing.T) {
	backend, driver, _ := testBackend(t)
	request := testRequest()
	content, _ := json.Marshal(request.Key)
	labels := map[string]string{namespaceLabel: "tests", identityLabel: string(content), digestLabel: strings.Repeat("a", 64)}
	driver.pages = map[string][]metadata{"": {{ID: "one", Name: "one", Labels: labels}}, "next": {{ID: "two", Name: "two", Labels: labels}, {ID: "malformed", Name: "malformed", Labels: map[string]string{namespaceLabel: "tests"}}}}
	snapshot, err := backend.List(context.Background())
	if err != nil || len(snapshot.Duplicates) != 1 || len(snapshot.Malformed) != 1 || len(snapshot.Processes) != 0 {
		t.Fatalf("inventory=%+v, %v", snapshot, err)
	}
	driver.listErr = errors.New("second page unavailable")
	if snapshot, err := backend.List(context.Background()); err == nil || len(snapshot.Duplicates) > 0 {
		t.Fatal("partial scan returned authoritative inventory")
	}
	driver.resources["malformed"] = metadata{ID: "replacement", Name: "malformed", Labels: map[string]string{namespaceLabel: "tests"}}
	if err := backend.EnsureMalformedAbsent(context.Background(), snapshotMalformed("malformed", "malformed")); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("replaced malformed resource=%v", err)
	}
	if driver.removes != 0 {
		t.Fatal("removed same-name replacement")
	}
}

func TestCleanupStopsEveryDuplicateAndRecoversLostRemovalReceipt(t *testing.T) {
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	engine, _ := backend.New(request, ledger)
	if _, err := engine.Create(context.Background(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	duplicate := driver.resources[nameFor(request.Key)]
	duplicate.ID = "duplicate"
	duplicate.Name = "duplicate"
	driver.resources[duplicate.Name] = duplicate
	if err := backend.EnsureKeyStopped(context.Background(), request.Key); err != nil {
		t.Fatal(err)
	}
	for _, resource := range driver.resources {
		if resource.Status != "Stopped" {
			t.Fatal("duplicate environment survived stop")
		}
	}
	// The provider removes the exact main resource, but its acknowledgement is
	// lost. A retry can use authoritative absence after settled creation/stop.
	delete(driver.resources, nameFor(request.Key))
	if err := backend.EnsureKeyAbsent(context.Background(), request.Key); err != nil {
		t.Fatal(err)
	}
	if err := backend.EnsureKeyAbsent(context.Background(), request.Key); err != nil {
		t.Fatal("removal retry is not idempotent")
	}
	if len(driver.resources) != 0 {
		t.Fatal("duplicate disposable resources survived removal")
	}
}
func snapshotMalformed(name, id string) execution.MalformedProcess {
	return execution.MalformedProcess{Resource: resourceRef(metadata{Name: name, ID: id})}
}

func TestAdmissionRejectsUnsupportedOrUnsafeExecution(t *testing.T) {
	backend, _, ledger := testBackend(t)
	for _, mutate := range []func(*execution.Request){
		func(request *execution.Request) { request.Image = "alpine:latest" },
		func(request *execution.Request) { request.MemoryBytes = 1 << 52 },
		func(request *execution.Request) {
			request.TemporaryDirectories = []execution.TemporaryDirectory{{Target: "/tmp", SizeBytes: 1 << 52}}
		},
		func(request *execution.Request) { request.Requirements = []execution.Requirement{"host-docker-socket"} },
		func(request *execution.Request) { request.Platform.Architecture = "arm64" },
		func(request *execution.Request) {
			request.Attachments = []execution.Attachment{{Resource: "host-socket", Target: "/docker.sock"}}
		},
		func(request *execution.Request) {
			request.Attachments = []execution.Attachment{{Resource: "state", Target: "/run/omnigrex"}}
		},
		func(request *execution.Request) {
			request.State = execution.StateRef{Backend: execution.BackendBinding{Family: "docker"}}
		},
	} {
		request := testRequest()
		mutate(&request)
		if _, err := backend.New(request, ledger); err == nil {
			t.Fatal("unsafe specification accepted")
		}
	}
}

func TestDurableDigestExcludesProviderAndMCPSecrets(t *testing.T) {
	request := testRequest()
	request.Environment = []string{`OPENCODE_AUTH_CONTENT={"api_key":"first"}`, `OPENCODE_CONFIG_CONTENT={"mcp":{"omnigrex":{"headers":{"Authorization":"Bearer first"}}},"model":"test"}`}
	first := requestDigest(request)
	request.Environment[0] = `OPENCODE_AUTH_CONTENT={"api_key":"second"}`
	request.Environment[1] = `OPENCODE_CONFIG_CONTENT={"mcp":{"omnigrex":{"headers":{"Authorization":"Bearer second"}}},"model":"test"}`
	if requestDigest(request) != first {
		t.Fatal("ephemeral secret entered durable specification digest")
	}
	request.Environment[1] = `OPENCODE_CONFIG_CONTENT={"model":"changed"}`
	if requestDigest(request) == first {
		t.Fatal("nonsecret configuration change was ignored")
	}
}

func TestProcessSignalExitDoesNotFabricateCodeOrOOM(t *testing.T) {
	backend, _, ledger := testBackend(t)
	request := testRequest()
	engine, _ := backend.New(request, ledger)
	if _, err := engine.Create(context.Background(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := backend.locked(context.Background(), request.Key, func(record *operation) error {
		record.Started, record.ProcessExited = true, true
		record.ProcessExitCode = -1
		record.ProcessExitedAt = time.Now()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	observed, err := backend.ObserveKeyExit(context.Background(), request.Key)
	if err != nil || !observed.Exited || observed.ExitCodeKnown || observed.OOMKilled || observed.Scope != "runtime-process" {
		t.Fatalf("signal exit=%+v,%v", observed, err)
	}
}

func TestUnknownExitCauseAndMissingFlushRemainUnknown(t *testing.T) {
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	engine, _ := backend.New(request, ledger)
	_, err := engine.Create(context.Background(), request, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.locked(context.Background(), request.Key, func(record *operation) error { record.Started = true; return nil }); err != nil {
		t.Fatal(err)
	}
	actual := driver.resources[nameFor(request.Key)]
	actual.Status = "Stopped"
	driver.resources[actual.Name] = actual
	observed, err := backend.ObserveKeyExit(context.Background(), request.Key)
	if err != nil || !observed.Exited || observed.OOMKilled {
		t.Fatalf("exit=%+v, %v", observed, err)
	}
	err = backend.ReadWorkspace(context.Background(), execution.CaptureRequest{Workspace: execution.WorkspaceRef{Key: request.Key}, Purpose: execution.TerminalCapture}, func(io.Reader) error { t.Fatal("unflushed workspace was sealed"); return nil })
	if !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("unflushed terminal=%v", err)
	}
}

type fakePlatform struct {
	resources                              map[string]metadata
	pages                                  map[string][]metadata
	createErr, stopErr, removeErr, listErr error
	creates, stops, removes                int
}

func (driver *fakePlatform) Create(_ context.Context, definition creation) (metadata, error) {
	driver.creates++
	meta := metadata{ID: "original", Name: definition.Name, Status: "Running", Labels: definition.Labels}
	driver.resources[meta.Name] = meta
	return meta, driver.createErr
}
func (driver *fakePlatform) Lookup(_ context.Context, name string) (metadata, error) {
	meta, ok := driver.resources[name]
	if !ok {
		return metadata{}, execution.ErrNotFound
	}
	return meta, nil
}
func (driver *fakePlatform) List(_ context.Context, cursor string) ([]metadata, string, error) {
	if driver.pages == nil {
		var resources []metadata
		for _, resource := range driver.resources {
			resources = append(resources, resource)
		}
		return resources, "", nil
	}
	if cursor == "next" {
		return driver.pages[cursor], "", driver.listErr
	}
	return driver.pages[cursor], "next", nil
}
func (*fakePlatform) Image(context.Context, string) (string, string, error) {
	return "linux", "amd64", nil
}
func (*fakePlatform) Execute(context.Context, metadata, []string, string, io.Reader) error {
	return nil
}
func (*fakePlatform) Stream(context.Context, metadata, []string, string) (stream, error) {
	return nil, execution.ErrUnknownEffect
}
func (*fakePlatform) Pause(context.Context, metadata) error  { return nil }
func (*fakePlatform) Resume(context.Context, metadata) error { return nil }
func (driver *fakePlatform) Stop(_ context.Context, meta metadata) error {
	driver.stops++
	if driver.stopErr != nil {
		return driver.stopErr
	}
	meta.Status = "Stopped"
	driver.resources[meta.Name] = meta
	return nil
}
func (driver *fakePlatform) Remove(_ context.Context, meta metadata) error {
	driver.removes++
	if driver.removeErr != nil {
		return driver.removeErr
	}
	delete(driver.resources, meta.Name)
	return nil
}

type fakeLedger struct {
	sync.Mutex
	records     map[execution.Key]execution.Reservation
	reserveHook func()
	beginErr    error
}

func (ledger *fakeLedger) ReserveExecution(_ context.Context, key execution.Key, digest string) (execution.Reservation, bool, error) {
	ledger.Lock()
	defer ledger.Unlock()
	record, found := ledger.records[key]
	if found {
		if record.Revoked || record.Digest != digest {
			return record, false, execution.ErrConflict
		}
		return record, false, nil
	}
	record = execution.Reservation{Key: key, Digest: digest, Phase: execution.Reserved}
	ledger.records[key] = record
	if ledger.reserveHook != nil {
		ledger.reserveHook()
	}
	return record, true, nil
}
func (ledger *fakeLedger) BeginExecutionPhase(_ context.Context, key execution.Key, phase execution.Phase) error {
	ledger.Lock()
	defer ledger.Unlock()
	record := ledger.records[key]
	if record.Revoked {
		return execution.ErrConflict
	}
	record.Phase = phase
	ledger.records[key] = record
	return ledger.beginErr
}
func (ledger *fakeLedger) BindExecution(_ context.Context, key execution.Key, resource execution.Ref) error {
	ledger.Lock()
	defer ledger.Unlock()
	record := ledger.records[key]
	if record.Resource != "" && record.Resource != resource {
		return execution.ErrConflict
	}
	record.Resource = resource
	record.CreationSettled = true
	ledger.records[key] = record
	return nil
}
func (ledger *fakeLedger) RevokeExecution(_ context.Context, key execution.Key) (execution.Reservation, bool, error) {
	ledger.Lock()
	defer ledger.Unlock()
	record, found := ledger.records[key]
	record.Key = key
	record.Revoked = true
	ledger.records[key] = record
	return record, found, nil
}
func (ledger *fakeLedger) RecordNoExecution(_ context.Context, key execution.Key) error {
	ledger.Lock()
	defer ledger.Unlock()
	record := ledger.records[key]
	record.CreationSettled = true
	ledger.records[key] = record
	return nil
}
func (*fakeLedger) BindSetupResource(context.Context, execution.Key, execution.Ref) error {
	return execution.ErrInvalidSpec
}
func (*fakeLedger) RecordSetupStopped(context.Context, execution.Key, execution.Ref) error {
	return execution.ErrInvalidSpec
}
