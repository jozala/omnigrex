package main

import (
	"context"
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/startup"
	"github.com/jozala/omnigrex/internal/store"
)

func TestReadNonemptyJSONObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	const credential = `{"provider":{"apiKey":"credential-sentinel"}}`
	if err := os.WriteFile(path, []byte(credential), 0o600); err != nil {
		t.Fatalf("write provider credential fixture: %v", err)
	}

	got, err := readNonemptyJSONObject(path)
	if err != nil {
		t.Fatalf("readNonemptyJSONObject() error = %v", err)
	}
	if string(got) != credential {
		t.Fatal("readNonemptyJSONObject() did not preserve the raw JSON")
	}
	zeroBytes(got)
}

func TestReadNonemptyJSONObjectRejectsInvalidContentWithoutDisclosure(t *testing.T) {
	for _, content := range []string{"", "null", "[]", "{}", `{"secret":"credential-sentinel"} trailing`} {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "provider.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write provider credential fixture: %v", err)
			}

			got, err := readNonemptyJSONObject(path)
			if err == nil || got != nil {
				t.Fatalf("readNonemptyJSONObject() = (%q, %v), want validation error", got, err)
			}
			if content != "" && strings.Contains(err.Error(), content) || strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("validation error disclosed file content: %v", err)
			}
		})
	}
}

func TestImportRuntimeProfileCompatibilityResultsRejectsMalformedAndMismatchedFiles(t *testing.T) {
	target := mainTestRuntimeProfile(t, "b")
	importer := &compatibilityImporterFake{}
	malformed := filepath.Join(t.TempDir(), "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{"schema_version":"wrong","results":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importRuntimeProfileCompatibilityResults(context.Background(), importer, malformed, target); err == nil {
		t.Fatal("malformed compatibility result startup import error = nil")
	}
	if importer.calls != 0 {
		t.Fatalf("malformed compatibility result import calls = %d, want 0", importer.calls)
	}

	result := runtimeprofile.NewCompatibilityResult(mainTestRuntimeProfile(t, "a").Binding(),
		mainTestRuntimeProfile(t, "c").Binding(), runtimeprofile.Platform{OS: "linux", Arch: "amd64"},
		time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	encoded, err := runtimeprofile.EncodeCompatibilityResultsFile([]runtimeprofile.CompatibilityResult{result})
	if err != nil {
		t.Fatal(err)
	}
	mismatched := filepath.Join(t.TempDir(), "mismatched.json")
	if err := os.WriteFile(mismatched, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importRuntimeProfileCompatibilityResults(context.Background(), importer, mismatched, target); err == nil {
		t.Fatal("mismatched compatibility result startup import error = nil")
	}
	if importer.calls != 0 {
		t.Fatalf("mismatched compatibility result import calls = %d, want 0", importer.calls)
	}
}

func TestImportRuntimeProfileCompatibilityResultsImportsExactTargetAndIsOptional(t *testing.T) {
	target := mainTestRuntimeProfile(t, "b")
	importer := &compatibilityImporterFake{}
	if err := importRuntimeProfileCompatibilityResults(context.Background(), importer, "", target); err != nil {
		t.Fatalf("optional compatibility result import error = %v", err)
	}
	if importer.calls != 0 {
		t.Fatalf("optional compatibility result import calls = %d, want 0", importer.calls)
	}

	result := runtimeprofile.NewCompatibilityResult(mainTestRuntimeProfile(t, "a").Binding(), target.Binding(),
		runtimeprofile.Platform{OS: "linux", Arch: "amd64"}, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	encoded, err := runtimeprofile.EncodeCompatibilityResultsFile([]runtimeprofile.CompatibilityResult{result})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compatibility.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importRuntimeProfileCompatibilityResults(context.Background(), importer, path, target); err != nil {
		t.Fatalf("exact compatibility result import error = %v", err)
	}
	if importer.calls != 1 || len(importer.file.Results) != 1 || importer.file.Results[0] != result {
		t.Fatalf("compatibility import = calls %d, file %#v", importer.calls, importer.file)
	}
}

func TestStartupDockerAdapterTranslatesInventoryAndCleanupIdentities(t *testing.T) {
	identity := dockerruntime.RuntimeProcessIdentity{
		AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001",
		AgentTurnID: "30000000-0000-4000-8000-000000000001", ExecutionEpoch: 9,
		RuntimeProfile: dockerruntime.RuntimeProfileIdentity{Name: "opencode-acp", Version: "v1"},
	}
	overflow := identity
	overflow.ExecutionEpoch = math.MaxUint64
	inventory := &startupInventoryFake{snapshot: dockerruntime.RuntimeProcessInventorySnapshot{
		Processes: []dockerruntime.ManagedRuntimeProcess{{Identity: identity, ContainerID: "process"}},
		Duplicates: []dockerruntime.DuplicateManagedRuntimeProcess{{
			Identity: overflow, ContainerIDs: []string{"overflow-a", "overflow-b"},
		}},
		Malformed: []dockerruntime.MalformedManagedRuntimeProcess{{
			ContainerID: "malformed", Reason: dockerruntime.MalformedIncompleteIdentity,
		}},
	}}
	cleaner := &startupCleanerFake{}
	adapter := startupDockerAdapter{inventory: inventory, cleaner: cleaner}

	snapshot, err := adapter.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	wantIdentity := store.AgentTurnRuntimeIdentity{
		AssignmentID: identity.AssignmentID, AgentSessionID: identity.AgentSessionID,
		AgentTurnID: identity.AgentTurnID, ExecutionEpoch: 9,
		RuntimeProfileName: "opencode-acp", RuntimeProfileVersion: "v1",
	}
	if len(snapshot.Processes) != 1 || snapshot.Processes[0].Identity != wantIdentity || len(snapshot.Duplicates) != 0 {
		t.Fatalf("translated Runtime Processes = %#v, duplicates %#v", snapshot.Processes, snapshot.Duplicates)
	}
	if got := []string{snapshot.Malformed[0].ContainerID, snapshot.Malformed[1].ContainerID, snapshot.Malformed[2].ContainerID}; !slices.Equal(got, []string{"overflow-a", "overflow-b", "malformed"}) {
		t.Fatalf("translated malformed containers = %v", got)
	}

	if err := adapter.EnsureAbsent(context.Background(), wantIdentity); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	wantLabels := map[string]string{
		dockerruntime.RuntimeProcessAssignmentLabel: identity.AssignmentID,
		dockerruntime.RuntimeProcessSessionLabel:    identity.AgentSessionID,
		dockerruntime.RuntimeProcessTurnLabel:       identity.AgentTurnID,
		dockerruntime.RuntimeProcessEpochLabel:      "9",
		dockerruntime.RuntimeProfileIdentityLabel:   "opencode-acp/v1",
	}
	if !maps.Equal(cleaner.labels, wantLabels) {
		t.Fatalf("EnsureAbsent() labels = %#v, want %#v", cleaner.labels, wantLabels)
	}
	if err := adapter.EnsureMalformedAbsent(context.Background(), startup.MalformedManagedRuntimeProcess{ContainerID: "malformed"}); err != nil {
		t.Fatalf("EnsureMalformedAbsent() error = %v", err)
	}
	if cleaner.malformedContainerID != "malformed" {
		t.Fatalf("malformed cleanup container ID = %q", cleaner.malformedContainerID)
	}
}

func TestPrepareStartupValidatesBindingsBeforeReconciliation(t *testing.T) {
	events := []string{}
	bindings := startupBindingCheckerFake{check: func(context.Context) error {
		events = append(events, "bindings")
		return nil
	}}
	reconciler := startupReconcilerFake{reconcile: func(context.Context) (startup.ReconciliationResult, error) {
		events = append(events, "reconcile")
		return startup.ReconciliationResult{Passes: 2}, nil
	}}

	result, err := prepareStartup(context.Background(), time.Second, bindings, reconciler)
	if err != nil || result.Passes != 2 {
		t.Fatalf("prepareStartup() = (%#v, %v)", result, err)
	}
	if !slices.Equal(events, []string{"bindings", "reconcile"}) {
		t.Fatalf("startup order = %v", events)
	}

	configurationErr := errors.New("protected binding unavailable")
	bindings.check = func(context.Context) error { return configurationErr }
	reconciler.reconcile = func(context.Context) (startup.ReconciliationResult, error) {
		t.Fatal("reconciliation ran after binding validation failed")
		return startup.ReconciliationResult{}, nil
	}
	if _, err := prepareStartup(context.Background(), time.Second, bindings, reconciler); !errors.Is(err, configurationErr) {
		t.Fatalf("prepareStartup() error = %v, want protected binding failure", err)
	}
}

func TestCombinedReadinessIncludesProtectedImageAvailability(t *testing.T) {
	missingImage := errors.New("protected exact image absent")
	checks := []string{}
	readiness := combinedReadiness(time.Second,
		func(context.Context) error { checks = append(checks, "database"); return nil },
		func(context.Context) error { checks = append(checks, "docker"); return nil },
		func(context.Context) error { checks = append(checks, "protected-images"); return missingImage },
	)

	if err := readiness.Check(context.Background()); !errors.Is(err, missingImage) {
		t.Fatalf("Check() error = %v, want protected image failure", err)
	}
	if !slices.Equal(checks, []string{"database", "docker", "protected-images"}) {
		t.Fatalf("readiness checks = %v", checks)
	}
}

type startupInventoryFake struct {
	snapshot dockerruntime.RuntimeProcessInventorySnapshot
}

type compatibilityImporterFake struct {
	calls int
	file  runtimeprofile.CompatibilityResultsFile
}

func (importer *compatibilityImporterFake) ImportRuntimeProfileCompatibilityResults(_ context.Context, file runtimeprofile.CompatibilityResultsFile) error {
	importer.calls++
	importer.file = file
	return nil
}

func mainTestRuntimeProfile(t *testing.T, digestDigit string) runtimeprofile.Profile {
	t.Helper()
	value, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:"+strings.Repeat(digestDigit, 64),
		runtimeprofile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (inventory *startupInventoryFake) List(context.Context) (dockerruntime.RuntimeProcessInventorySnapshot, error) {
	return inventory.snapshot, nil
}

type startupCleanerFake struct {
	labels               map[string]string
	malformedContainerID string
}

func (cleaner *startupCleanerFake) EnsureAbsent(_ context.Context, labels map[string]string) error {
	cleaner.labels = maps.Clone(labels)
	return nil
}

func (cleaner *startupCleanerFake) EnsureManagedContainerAbsent(_ context.Context, containerID string) error {
	cleaner.malformedContainerID = containerID
	return nil
}

type startupBindingCheckerFake struct {
	check func(context.Context) error
}

func (checker startupBindingCheckerFake) CheckBindings(ctx context.Context) error {
	return checker.check(ctx)
}

type startupReconcilerFake struct {
	reconcile func(context.Context) (startup.ReconciliationResult, error)
}

func (reconciler startupReconcilerFake) Reconcile(ctx context.Context) (startup.ReconciliationResult, error) {
	return reconciler.reconcile(ctx)
}
