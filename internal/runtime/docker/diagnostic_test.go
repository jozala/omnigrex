package docker

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestDiagnosticInventorySeparatesLiveProbesAndRevalidatesExpiredCleanup(t *testing.T) {
	live := map[string]string{diagnosticMarkerLabel: "true", diagnosticNamespaceLabel: "installation", diagnosticExpiryLabel: time.Now().Add(time.Minute).Format(time.RFC3339Nano)}
	expired := map[string]string{diagnosticMarkerLabel: "true", diagnosticNamespaceLabel: "installation", diagnosticExpiryLabel: time.Now().Add(-time.Minute).Format(time.RFC3339Nano)}
	api := &runtimeProcessInventoryFake{result: mobyclient.ContainerListResult{Items: []container.Summary{{ID: "live-probe", Labels: live}, {ID: "expired-probe", Labels: expired}}}}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })
	inventory.diagnosticNamespace = "installation"
	snapshot, err := inventory.List(context.Background())
	if err != nil || len(snapshot.Processes) != 0 || len(snapshot.Malformed) != 1 || snapshot.Malformed[0].ContainerID != "expired-probe" {
		t.Fatalf("diagnostic inventory = %#v, %v", snapshot, err)
	}
	cleanupAPI := &exactLabelCleanupFake{inspects: []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{ID: "expired-probe", Config: &container.Config{Labels: live}}}}}
	cleaner := newExactLabelCleaner(cleanupAPI, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second, DiagnosticNamespace: "installation"})
	if err := cleaner.EnsureManagedContainerAbsent(context.Background(), "expired-probe"); err == nil || len(cleanupAPI.removed) != 0 {
		t.Fatal("live probe removed after stale expiry observation")
	}
	cleanupAPI.inspects = []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{ID: "expired-probe", Config: &container.Config{Labels: expired}}}}
	if err := cleaner.EnsureManagedContainerAbsent(context.Background(), "expired-probe"); err != nil {
		t.Fatal(err)
	}
}
