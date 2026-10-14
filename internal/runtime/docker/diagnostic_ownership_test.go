package docker

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestDiagnosticInventoryIsInstallationScoped(t *testing.T) {
	labels := func(namespace string) map[string]string {
		return map[string]string{diagnosticMarkerLabel: "true", diagnosticNamespaceLabel: namespace, diagnosticProbeLabel: "probe", diagnosticExpiryLabel: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
	}
	api := &runtimeProcessInventoryFake{result: mobyclient.ContainerListResult{Items: []container.Summary{{ID: "owned", Labels: labels("installation-a")}, {ID: "foreign", Labels: labels("installation-b")}, {ID: "missing-owner", Labels: labels("")}}}}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })
	inventory.diagnosticNamespace = "installation-a"
	snapshot, err := inventory.List(context.Background())
	if err != nil || len(snapshot.Malformed) != 1 || snapshot.Malformed[0].ContainerID != "owned" || snapshot.Malformed[0].Reason != MalformedDiagnosticExpired {
		t.Fatalf("diagnostic inventory: %+v, %v", snapshot, err)
	}
}

func TestDiagnosticCleanupRevalidatesInstallationOwner(t *testing.T) {
	for _, namespace := range []string{"installation-a", "installation-b", ""} {
		t.Run(namespace, func(t *testing.T) {
			labels := map[string]string{RuntimeProcessMarkerLabel: RuntimeProcessMarkerValue, diagnosticMarkerLabel: "true", diagnosticNamespaceLabel: namespace, diagnosticProbeLabel: "probe", diagnosticExpiryLabel: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
			api := &exactLabelCleanupFake{inspects: []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{ID: "probe-resource", Config: &container.Config{Labels: labels}}}}}
			cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second, DiagnosticNamespace: "installation-a"})
			err := cleaner.EnsureManagedContainerAbsent(context.Background(), "probe-resource")
			if namespace == "installation-a" {
				if err != nil || len(api.removed) != 1 {
					t.Fatalf("owned cleanup: %v", err)
				}
			} else {
				if err == nil || len(api.removed) != 0 || len(api.stopped) != 0 {
					t.Fatalf("unowned cleanup changed resources: %v", err)
				}
			}
		})
	}
}
