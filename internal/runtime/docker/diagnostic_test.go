package docker

import (
	"context"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestDiagnosticContractUsesDisposableStorageAndConfiguredBudgets(t *testing.T) {
	profile, err := profile.NewOpenCodeV1("registry.example/"+testImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	for _, memory := range []int64{512 << 20, 1024 << 20} {
		policy, spec, err := diagnosticContract(profile, testImage, DiagnosticOptions{Network: "agent-network", MemoryBytes: memory})
		if err != nil {
			t.Fatal(err)
		}
		if policy.MemoryBytes != memory || spec.MemoryBytes != memory || policy.PIDsLimit != 128 || spec.PIDsLimit != 128 {
			t.Fatal("diagnostic budgets diverged")
		}
		if len(spec.Volumes) != 0 || len(spec.Tmpfs) != len(profile.Contract().Tmpfs)+len(profile.Contract().Mounts) {
			t.Fatal("diagnostic attached retained storage")
		}
		created, err := buildCreateOptions(spec)
		if err != nil {
			t.Fatal(err)
		}
		if created.Config.Labels[RuntimeProcessMarkerLabel] != "" || created.Config.Labels[diagnosticMarkerLabel] != "true" || diagnosticCleanupEligible(created.Config.Labels, time.Now()) {
			t.Fatal("diagnostic received a Turn identity or invalid expiry")
		}
	}
	if _, _, err := diagnosticContract(profile, testImage, DiagnosticOptions{}); err == nil {
		t.Fatal("diagnostic accepted missing memory")
	}
}

func TestDiagnosticInventorySeparatesLiveProbesAndRevalidatesExpiredCleanup(t *testing.T) {
	live := map[string]string{diagnosticMarkerLabel: "true", diagnosticExpiryLabel: time.Now().Add(time.Minute).Format(time.RFC3339Nano)}
	expired := map[string]string{diagnosticMarkerLabel: "true", diagnosticExpiryLabel: time.Now().Add(-time.Minute).Format(time.RFC3339Nano)}
	api := &runtimeProcessInventoryFake{result: mobyclient.ContainerListResult{Items: []container.Summary{{ID: "live-probe", Labels: live}, {ID: "expired-probe", Labels: expired}}}}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })
	snapshot, err := inventory.List(context.Background())
	if err != nil || len(snapshot.Processes) != 0 || len(snapshot.Malformed) != 1 || snapshot.Malformed[0].ContainerID != "expired-probe" {
		t.Fatalf("diagnostic inventory = %#v, %v", snapshot, err)
	}
	cleanupAPI := &exactLabelCleanupFake{inspects: []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{ID: "expired-probe", Config: &container.Config{Labels: live}}}}}
	cleaner := newExactLabelCleaner(cleanupAPI, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})
	if err := cleaner.EnsureManagedContainerAbsent(context.Background(), "expired-probe"); err == nil || len(cleanupAPI.removed) != 0 {
		t.Fatal("live probe removed after stale expiry observation")
	}
	cleanupAPI.inspects = []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{ID: "expired-probe", Config: &container.Config{Labels: expired}}}}
	if err := cleaner.EnsureManagedContainerAbsent(context.Background(), "expired-probe"); err != nil {
		t.Fatal(err)
	}
}
