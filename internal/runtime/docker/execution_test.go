package docker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestExecutionCompileOwnsLabelsPolicyAndParticipantStorage(t *testing.T) {
	request := executionTestRequest()
	request.Attachments = []execution.Attachment{{Resource: "workspace", Target: "/workspace"}, {Resource: "state", Target: "/state"}, {Resource: "mise", Target: "/mise"}, {Resource: "tool-data", Target: "/tools"}}
	request.TemporaryDirectories = []execution.TemporaryDirectory{{Target: "/tmp", SizeBytes: 8 << 20}}
	factory := ExecutionFactory{Network: "agent-network", WorkspaceVolume: "workspace", RuntimeStateVolume: "state", MiseVolume: "mise"}
	policy, spec, err := factory.Compile(request)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "omnigrex-turn-"+request.Key.AgentTurnID+"-epoch-9" || spec.Network != factory.Network || spec.Labels[RuntimeProfileIdentityLabel] != "opencode-acp/v1" {
		t.Fatalf("compiled identity = %#v", spec)
	}
	if err := validateResources(EngineOptions{AgentNetwork: factory.Network, RuntimePolicy: policy}, spec); err != nil {
		t.Fatal(err)
	}
	for _, volume := range spec.Volumes {
		if !strings.HasPrefix(volume.Subpath, "assignment-"+request.Key.AssignmentID+"/") {
			t.Fatalf("unisolated volume: %#v", volume)
		}
	}
	original := slices.Clone(policy.Volumes)
	spec.Volumes[0].Name = "changed"
	spec.Labels[RuntimeProcessTurnLabel] = "changed"
	request.Command[0] = "changed"
	if !slices.Equal(policy.Volumes, original) || policy.Labels[RuntimeProcessTurnLabel] != executionTestRequest().Key.AgentTurnID || policy.Command[0] != "/bin/sh" {
		t.Fatal("mutable request/spec changed policy")
	}
	for _, attachment := range []execution.Attachment{{Resource: "host-socket", Target: "/docker.sock"}, {Resource: "workspace", Target: "/state"}} {
		invalid := executionTestRequest()
		invalid.Attachments = append(slices.Clone(request.Attachments), attachment)
		if _, _, err := factory.Compile(invalid); err == nil {
			t.Fatalf("accepted invalid attachment %#v", attachment)
		}
	}
}

func TestExecutionDigestExcludesCredentialsAndDetectsNonsecretChanges(t *testing.T) {
	request := executionTestRequest()
	request.Environment = []string{"OPENCODE_AUTH_CONTENT={\"key\":\"first\"}", "HOME=/home/opencode"}
	first := requestDigest(request)
	request.Environment[0] = "OPENCODE_AUTH_CONTENT={\"key\":\"second\"}"
	if requestDigest(request) != first {
		t.Fatal("credential bytes entered durable digest")
	}
	request.BootstrapImage = "registry.example/bootstrap@sha256:" + strings.Repeat("b", 64)
	if requestDigest(request) == first {
		t.Fatal("retained bootstrap source was not bound to the execution specification")
	}
	request.BootstrapImage = ""
	request.Command = []string{"different"}
	if requestDigest(request) == first {
		t.Fatal("changed execution content reused digest")
	}
}

func TestExecutionCompileRejectsUnsupportedRequirementsAndUnpinnedImages(t *testing.T) {
	factory := ExecutionFactory{Network: "none"}
	for _, requirement := range []execution.Requirement{execution.HardwareIsolation, execution.NestedContainers, "unknown"} {
		request := executionTestRequest()
		request.Requirements = []execution.Requirement{requirement}
		if _, _, err := factory.Compile(request); !errors.Is(err, execution.ErrUnsupportedRequirement) {
			t.Fatalf("requirement %q accepted: %v", requirement, err)
		}
	}
	request := executionTestRequest()
	request.Image = "alpine:latest"
	if _, _, err := factory.Compile(request); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("mutable image accepted: %v", err)
	}
	request = executionTestRequest()
	request.MemoryBytes = 0
	if _, _, err := factory.Compile(request); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("missing budget accepted: %v", err)
	}
	request = executionTestRequest()
	request.GoldenResources = true
	request.Requirements = []execution.Requirement{execution.ToolInstallation}
	if _, _, err := factory.Compile(request); !errors.Is(err, execution.ErrInvalidSpec) {
		t.Fatalf("unattached repository tool installation accepted: %v", err)
	}
	request.GoldenResources = false
	request.BootstrapImage = "registry.example/bootstrap:latest"
	if _, _, err := factory.Compile(request); !errors.Is(err, execution.ErrInvalidSpec) {
		t.Fatalf("mutable retained bootstrap image accepted: %v", err)
	}
}

func TestExecutionCleanupKeepsAmbiguousCreationUnknownUntilResourceIsFound(t *testing.T) {
	key := executionTestRequest().Key
	ledger := newExecutionTestLedger()
	if _, _, err := ledger.ReserveExecution(context.Background(), key, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BeginExecutionPhase(context.Background(), key, execution.Main); err != nil {
		t.Fatal(err)
	}
	api := &exactLabelCleanupFake{}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
	if err := cleaner.EnsureKeyAbsent(context.Background(), key); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("unresolved create cleanup = %v", err)
	}
	if ledger.records[key].CreationSettled || len(api.removed) != 0 {
		t.Fatal("an empty scan was used to settle unresolved creation")
	}
	if _, _, err := ledger.ReserveExecution(context.Background(), key, strings.Repeat("a", 64)); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("revoked epoch accepted another create: %v", err)
	}
	labels := keyLabels(key)
	labels[RuntimeProcessMarkerLabel] = RuntimeProcessMarkerValue
	labels[executionDigestLabel] = strings.Repeat("a", 64)
	api.inspects = []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{ID: "late-resource", Config: &container.Config{Labels: labels}}}}
	api.lists = []mobyclient.ContainerListResult{{Items: []container.Summary{{ID: "late-resource", Labels: labels}}}, {}}
	if err := cleaner.EnsureKeyAbsent(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.removed["late-resource"]; !ok {
		t.Fatal("late create was not removed")
	}
	if ledger.records[key].Resource != "late-resource" || !ledger.records[key].Revoked {
		t.Fatal("lost creation evidence was not persisted")
	}
}

func TestExecutionCleanupDoesNotIssueReceiptOnFailureOrWrongOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*exactLabelCleanupFake)
	}{
		{"removal failure", func(api *exactLabelCleanupFake) { api.removeErr = errors.New("unavailable") }},
		{"wrong identity", func(api *exactLabelCleanupFake) {
			api.inspects[0].Container.Config.Labels[RuntimeProcessEpochLabel] = "10"
		}},
		{"wrong specification", func(api *exactLabelCleanupFake) {
			api.inspects[0].Container.Config.Labels[executionDigestLabel] = strings.Repeat("b", 64)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := executionTestRequest().Key
			ledger := newExecutionTestLedger()
			_, _, _ = ledger.ReserveExecution(context.Background(), key, strings.Repeat("a", 64))
			_ = ledger.BeginExecutionPhase(context.Background(), key, execution.Main)
			labels := keyLabels(key)
			labels[RuntimeProcessMarkerLabel], labels[executionDigestLabel] = RuntimeProcessMarkerValue, strings.Repeat("a", 64)
			api := &exactLabelCleanupFake{inspects: []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{ID: "resource", Config: &container.Config{Labels: labels}}}}, lists: []mobyclient.ContainerListResult{{Items: []container.Summary{{ID: "resource", Labels: maps.Clone(labels)}}}, {}}}
			test.mutate(api)
			cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
			if err := cleaner.EnsureKeyAbsent(context.Background(), key); err == nil {
				t.Fatal("uncertain cleanup issued a receipt")
			}
		})
	}
}

func TestExecutionKnownSetupFailureDoesNotLeaveUnresolvedCreation(t *testing.T) {
	for _, removeFails := range []bool{false, true} {
		api := &preparationFailureAPI{runtimeAPIFake: newRuntimeAPIFake()}
		if removeFails {
			api.removeErr = errors.New("remove unavailable")
		}
		failure := runOneShotContainer(context.Background(), api, mobyclient.ContainerCreateOptions{}, "setup")
		if failure == nil {
			t.Fatal("setup failure was lost")
		}
		key := executionTestRequest().Key
		ledger := newExecutionTestLedger()
		_, _, _ = ledger.ReserveExecution(context.Background(), key, strings.Repeat("a", 64))
		engine := &executionEngine{ledger: ledger, key: key}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		classified := engine.recordCreationFailure(ctx, failure)
		if errors.Is(classified, execution.ErrUnknownEffect) != removeFails {
			t.Fatalf("setup effects classification = %v, remove failed %t", classified, removeFails)
		}
		if !removeFails {
			cleaner := newExactLabelCleaner(&exactLabelCleanupFake{}, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
			if err := cleaner.EnsureKeyAbsent(context.Background(), key); err != nil {
				t.Fatalf("known empty execution could not be cleaned: %v", err)
			}
		}
	}
}

type preparationFailureAPI struct{ *runtimeAPIFake }

func (*preparationFailureAPI) ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	result <- container.WaitResponse{StatusCode: 1}
	return mobyclient.ContainerWaitResult{Result: result, Error: make(chan error)}
}

func TestExecutionResolvesSurvivingSetupWithoutAttributingItsOOMToAgent(t *testing.T) {
	key := executionTestRequest().Key
	ledger := newExecutionTestLedger()
	_, _, _ = ledger.ReserveExecution(context.Background(), key, strings.Repeat("a", 64))
	_ = ledger.BeginExecutionPhase(context.Background(), key, execution.Setup)
	labels := keyLabels(key)
	labels[RuntimeProcessMarkerLabel], labels[executionDigestLabel] = RuntimeProcessMarkerValue, strings.Repeat("a", 64)
	labels["io.omnigrex.assignment-subpath-initializer"] = "true"
	api := &exactLabelCleanupFake{}
	api.inspectFn = func(id string) (mobyclient.ContainerInspectResult, error) {
		if id != "setup" && id != "omnigrex-turn-"+key.AgentTurnID+"-epoch-9-setup" {
			return mobyclient.ContainerInspectResult{}, errdefs.ErrNotFound
		}
		if _, removed := api.removed[id]; removed {
			return mobyclient.ContainerInspectResult{}, errdefs.ErrNotFound
		}
		return mobyclient.ContainerInspectResult{Container: container.InspectResponse{ID: "setup", Config: &container.Config{Labels: labels}, State: &container.State{Status: container.StateExited, ExitCode: 137, OOMKilled: true}}}, nil
	}
	api.lists = []mobyclient.ContainerListResult{{Items: []container.Summary{{ID: "setup", Labels: labels}}}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
	observed, err := cleaner.ObserveKeyExit(context.Background(), key)
	if err != nil || observed.Exited || observed.OOMKilled {
		t.Fatalf("setup was attributed an agent exit: %#v, %v", observed, err)
	}
	api.lists = []mobyclient.ContainerListResult{{Items: []container.Summary{{ID: "setup", Labels: labels}}}, {Items: []container.Summary{{ID: "setup", Labels: labels}}}, {}}
	if err := cleaner.EnsureKeyAbsent(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if ledger.records[key].SetupResource != "setup" || !ledger.records[key].CreationSettled {
		t.Fatal("surviving setup creation was not resolved")
	}
}

func TestExecutionRecoversLostSetupDeletionAcknowledgement(t *testing.T) {
	key := executionTestRequest().Key
	ledger := newExecutionTestLedger()
	digest := strings.Repeat("a", 64)
	_, _, _ = ledger.ReserveExecution(context.Background(), key, digest)
	spec := runtimeProcessSpec()
	spec.Labels = keyLabels(key)
	spec.Labels[executionDigestLabel] = digest
	spec.Volumes = []VolumeMount{{Name: "state", Subpath: "assignment-" + key.AssignmentID + "/runtime-state", Target: "/state"}}
	api := &lostSetupDeletionAPI{runtimeAPIFake: newRuntimeAPIFake()}
	failure := prepareExecutionSubpaths(context.Background(), api, spec, ledger, key, false, "")
	if failure == nil || api.removeCalls != 1 {
		t.Fatalf("lost setup deletion = %v, removes %d", failure, api.removeCalls)
	}
	engine := &executionEngine{ledger: ledger, key: key}
	_ = engine.recordCreationFailure(context.Background(), failure)
	// The initializer is already absent, and main creation was never dispatched.
	cleaner := newExactLabelCleaner(&exactLabelCleanupFake{}, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
	for range 2 {
		if err := cleaner.EnsureKeyAbsent(context.Background(), key); err != nil {
			t.Fatalf("lost deletion acknowledgement remains unresolved: %v", err)
		}
	}
}

type lostSetupDeletionAPI struct{ *runtimeAPIFake }

func (*lostSetupDeletionAPI) ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	result <- container.WaitResponse{}
	return mobyclient.ContainerWaitResult{Result: result, Error: make(chan error)}
}

func TestExecutionRetainsUnstartedSetupIfIdentityCannotBeRecorded(t *testing.T) {
	api := newRuntimeAPIFake()
	failure := runManagedOneShotContainer(context.Background(), api, mobyclient.ContainerCreateOptions{Name: "deterministic-setup"}, "setup",
		func(context.Context, string) error { return errors.New("ledger unavailable") }, nil)
	if failure == nil || api.startCalls != 0 || api.removeCalls != 0 {
		t.Fatalf("unrecorded setup = %v, starts %d, removes %d", failure, api.startCalls, api.removeCalls)
	}
}
func (api *lostSetupDeletionAPI) ContainerRemove(ctx context.Context, id string, options mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	_, _ = api.runtimeAPIFake.ContainerRemove(ctx, id, options)
	return mobyclient.ContainerRemoveResult{}, errors.New("setup deletion response lost")
}

func executionTestRequest() execution.Request {
	return execution.Request{
		Key:   execution.Key{AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001", AgentTurnID: "30000000-0000-4000-8000-000000000001", ExecutionEpoch: 9, RuntimeProfileName: "opencode-acp", RuntimeProfileVersion: "v1"},
		Image: testImage, Platform: execution.Platform{OS: "linux", Architecture: "arm64"}, User: "10001:10001", WorkingDir: "/workspace", Command: []string{"/bin/sh"}, MemoryBytes: 64 << 20, PIDsLimit: 32,
	}
}

type executionTestLedger struct {
	mutex   sync.Mutex
	records map[execution.Key]execution.Reservation
}

func newExecutionTestLedger() *executionTestLedger {
	return &executionTestLedger{records: make(map[execution.Key]execution.Reservation)}
}
func (ledger *executionTestLedger) ReserveExecution(_ context.Context, key execution.Key, digest string) (execution.Reservation, bool, error) {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	if record, ok := ledger.records[key]; ok {
		if record.Digest != digest || record.Revoked {
			return record, false, execution.ErrConflict
		}
		return record, false, nil
	}
	record := execution.Reservation{Key: key, Digest: digest, Phase: execution.Reserved}
	ledger.records[key] = record
	return record, true, nil
}
func (ledger *executionTestLedger) BeginExecutionPhase(_ context.Context, key execution.Key, phase execution.Phase) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record, ok := ledger.records[key]
	if !ok || record.Revoked || record.CreationSettled || (phase != execution.Setup && phase != execution.Main) ||
		(record.Phase != execution.Reserved && !(phase == execution.Main && record.Phase == execution.Setup && record.SetupSettled)) {
		return execution.ErrConflict
	}
	record.Phase = phase
	ledger.records[key] = record
	return nil
}
func (ledger *executionTestLedger) BindSetupResource(_ context.Context, key execution.Key, ref execution.Ref) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record, ok := ledger.records[key]
	if !ok || ref == "" || record.Phase != execution.Setup || record.SetupSettled || record.SetupResource != "" && record.SetupResource != ref {
		return execution.ErrConflict
	}
	record.SetupResource = ref
	ledger.records[key] = record
	return nil
}
func (ledger *executionTestLedger) RecordSetupStopped(_ context.Context, key execution.Key, ref execution.Ref) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record, ok := ledger.records[key]
	if !ok || ref == "" || record.SetupResource != ref {
		return execution.ErrConflict
	}
	record.SetupSettled = true
	ledger.records[key] = record
	return nil
}
func (ledger *executionTestLedger) BindExecution(_ context.Context, key execution.Key, ref execution.Ref) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record, ok := ledger.records[key]
	if !ok || ref == "" || record.Phase != execution.Main || record.Resource != "" && record.Resource != ref || record.Resource == "" && record.CreationSettled {
		return execution.ErrConflict
	}
	record.Resource = ref
	record.CreationSettled = true
	ledger.records[key] = record
	return nil
}

func (ledger *executionTestLedger) RecordNoExecution(ctx context.Context, key execution.Key) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record, ok := ledger.records[key]
	if !ok || record.Resource != "" || record.SetupResource != "" && !record.SetupSettled {
		return execution.ErrConflict
	}
	record.CreationSettled = true
	ledger.records[key] = record
	return nil
}
func (ledger *executionTestLedger) RevokeExecution(_ context.Context, key execution.Key) (execution.Reservation, bool, error) {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record, ok := ledger.records[key]
	if !ok {
		record = execution.Reservation{Key: key, Phase: execution.Reserved}
	}
	record.Revoked = true
	if record.Phase == execution.Reserved {
		record.CreationSettled = true
	}
	ledger.records[key] = record
	return record, true, nil
}
