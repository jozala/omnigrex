package docker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestRuntimeProcessInventoryListsCompleteManagedProcesses(t *testing.T) {
	labels := managedRuntimeLabels()
	api := &runtimeProcessInventoryFake{result: mobyclient.ContainerListResult{Items: []container.Summary{
		{ID: "container-1", Labels: labels},
	}}}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })

	snapshot, err := inventory.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(api.options) != 2 || !api.options[0].All || !api.options[1].All {
		t.Fatalf("ContainerList() options = %#v", api.options)
	}
	wantFilters := []mobyclient.Filters{
		make(mobyclient.Filters).Add("label", RuntimeProcessMarkerLabel+"="+RuntimeProcessMarkerValue),
		make(mobyclient.Filters).Add("label",
			RuntimeProcessAssignmentLabel,
			RuntimeProcessSessionLabel,
			RuntimeProcessTurnLabel,
			RuntimeProcessEpochLabel,
			RuntimeProfileIdentityLabel,
		),
	}
	for index, want := range wantFilters {
		if !maps.EqualFunc(api.options[index].Filters, want, maps.Equal) {
			t.Fatalf("ContainerList() filters[%d] = %#v, want %#v", index, api.options[index].Filters, want)
		}
	}
	if len(snapshot.Processes) != 1 || len(snapshot.Duplicates) != 0 || len(snapshot.Malformed) != 0 {
		t.Fatalf("List() snapshot = %#v", snapshot)
	}
	process := snapshot.Processes[0]
	if process.ContainerID != "container-1" || process.Identity != (RuntimeProcessIdentity{
		AssignmentID:   "10000000-0000-4000-8000-000000000001",
		AgentSessionID: "20000000-0000-4000-8000-000000000001",
		AgentTurnID:    "30000000-0000-4000-8000-000000000001",
		ExecutionEpoch: 9,
		RuntimeProfile: RuntimeProfileIdentity{Name: "opencode-acp", Version: "v1"},
	}) {
		t.Fatalf("managed process = %#v", process)
	}
}

func TestRuntimeProcessInventoryDiscoversCompleteLegacyIdentityAndIgnoresUnrelatedContainers(t *testing.T) {
	legacy := managedRuntimeLabels()
	delete(legacy, RuntimeProcessMarkerLabel)
	api := &runtimeProcessInventoryFake{result: mobyclient.ContainerListResult{Items: []container.Summary{
		{ID: "legacy", Labels: legacy},
		{ID: "unrelated", Labels: map[string]string{"com.example.service": "true"}},
	}}}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })

	snapshot, err := inventory.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Processes) != 1 || snapshot.Processes[0].ContainerID != "legacy" ||
		len(snapshot.Duplicates) != 0 || len(snapshot.Malformed) != 0 {
		t.Fatalf("legacy snapshot = %#v", snapshot)
	}
}

func TestRuntimeProcessInventoryIgnoresUnprovenOwnership(t *testing.T) {
	markerFalse := managedRuntimeLabels()
	markerFalse[RuntimeProcessMarkerLabel] = "false"
	partialLegacy := managedRuntimeLabels()
	delete(partialLegacy, RuntimeProcessMarkerLabel)
	delete(partialLegacy, RuntimeProfileIdentityLabel)
	invalidLegacy := managedRuntimeLabels()
	delete(invalidLegacy, RuntimeProcessMarkerLabel)
	invalidLegacy[RuntimeProcessAssignmentLabel] = "not-a-uuid"
	items := []container.Summary{
		{ID: "assignment-only", Labels: map[string]string{RuntimeProcessAssignmentLabel: "10000000-0000-4000-8000-000000000001"}},
		{ID: "session-only", Labels: map[string]string{RuntimeProcessSessionLabel: "20000000-0000-4000-8000-000000000001"}},
		{ID: "turn-only", Labels: map[string]string{RuntimeProcessTurnLabel: "30000000-0000-4000-8000-000000000001"}},
		{ID: "epoch-only", Labels: map[string]string{RuntimeProcessEpochLabel: "9"}},
		{ID: "profile-only", Labels: map[string]string{RuntimeProfileIdentityLabel: "opencode-acp/v1"}},
		{ID: "marker-false", Labels: markerFalse},
		{ID: "partial-legacy", Labels: partialLegacy},
		{ID: "invalid-legacy", Labels: invalidLegacy},
	}
	api := &runtimeProcessInventoryFake{result: mobyclient.ContainerListResult{Items: items}}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })

	snapshot, err := inventory.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Processes) != 0 || len(snapshot.Duplicates) != 0 || len(snapshot.Malformed) != 0 {
		t.Fatalf("unproven ownership entered inventory: %#v", snapshot)
	}
}

func TestRuntimeProcessInventoryQuarantinesMalformedManagedContainers(t *testing.T) {
	valid := managedRuntimeLabels()
	tests := []struct {
		name   string
		id     string
		mutate func(map[string]string)
		reason MalformedRuntimeProcessReason
	}{
		{name: "missing container ID", mutate: func(map[string]string) {}, reason: MalformedMissingContainerID},
		{name: "incomplete identity", id: "incomplete", mutate: func(labels map[string]string) { delete(labels, RuntimeProcessSessionLabel) }, reason: MalformedIncompleteIdentity},
		{name: "assignment UUID", id: "assignment", mutate: func(labels map[string]string) { labels[RuntimeProcessAssignmentLabel] = "not-a-uuid" }, reason: MalformedAssignmentID},
		{name: "noncanonical UUID", id: "uppercase", mutate: func(labels map[string]string) {
			labels[RuntimeProcessTurnLabel] = "30000000-0000-4000-8000-00000000000A"
		}, reason: MalformedTurnID},
		{name: "epoch zero", id: "zero", mutate: func(labels map[string]string) { labels[RuntimeProcessEpochLabel] = "0" }, reason: MalformedExecutionEpoch},
		{name: "epoch noncanonical", id: "epoch", mutate: func(labels map[string]string) { labels[RuntimeProcessEpochLabel] = "09" }, reason: MalformedExecutionEpoch},
		{name: "profile", id: "profile", mutate: func(labels map[string]string) { labels[RuntimeProfileIdentityLabel] = "opencode-acp/v1/extra" }, reason: MalformedRuntimeProfile},
	}

	items := make([]container.Summary, 0, len(tests))
	for _, test := range tests {
		labels := maps.Clone(valid)
		test.mutate(labels)
		items = append(items, container.Summary{ID: test.id, Labels: labels})
	}
	inventory := newRuntimeProcessInventory(&runtimeProcessInventoryFake{
		result: mobyclient.ContainerListResult{Items: items},
	}, func() error { return nil })
	snapshot, err := inventory.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(snapshot.Processes) != 0 || len(snapshot.Duplicates) != 0 || len(snapshot.Malformed) != len(tests) {
		t.Fatalf("List() snapshot = %#v", snapshot)
	}
	for _, test := range tests {
		if !slices.Contains(snapshot.Malformed, MalformedManagedRuntimeProcess{ContainerID: test.id, Reason: test.reason}) {
			t.Errorf("malformed report missing ID %q reason %q: %#v", test.id, test.reason, snapshot.Malformed)
		}
	}
}

func TestRuntimeProcessInventoryReportsDuplicatesWithoutReturningThemAsUnique(t *testing.T) {
	firstIdentity := managedRuntimeLabels()
	secondIdentity := managedRuntimeLabels()
	secondIdentity[RuntimeProcessTurnLabel] = "30000000-0000-4000-8000-000000000002"
	api := &runtimeProcessInventoryFake{result: mobyclient.ContainerListResult{Items: []container.Summary{
		{ID: "duplicate-b", Labels: maps.Clone(firstIdentity)},
		{ID: "unique", Labels: secondIdentity},
		{ID: "duplicate-a", Labels: maps.Clone(firstIdentity)},
	}}}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })

	snapshot, err := inventory.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(snapshot.Processes) != 1 || snapshot.Processes[0].ContainerID != "unique" {
		t.Fatalf("unique processes = %#v", snapshot.Processes)
	}
	if len(snapshot.Duplicates) != 1 || !slices.Equal(snapshot.Duplicates[0].ContainerIDs, []string{"duplicate-a", "duplicate-b"}) {
		t.Fatalf("duplicates = %#v", snapshot.Duplicates)
	}
	if snapshot.Duplicates[0].Identity.AgentTurnID != firstIdentity[RuntimeProcessTurnLabel] {
		t.Fatalf("duplicate identity = %#v", snapshot.Duplicates[0].Identity)
	}
}

func TestRuntimeProcessInventoryDoesNotTrustPartialOrFailedListings(t *testing.T) {
	api := &runtimeProcessInventoryFake{
		result: mobyclient.ContainerListResult{Items: []container.Summary{{ID: "partial", Labels: managedRuntimeLabels()}}},
		err:    errors.New("connection reset"),
	}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })

	snapshot, err := inventory.List(context.Background())
	if err == nil {
		t.Fatal("List() error = nil, want listing failure")
	}
	if len(snapshot.Processes) != 0 || len(snapshot.Duplicates) != 0 || len(snapshot.Malformed) != 0 {
		t.Fatalf("failed partial listing returned evidence: %#v", snapshot)
	}
}

func TestRuntimeProcessInventoryCloseUsesOwnedClientClose(t *testing.T) {
	closed := false
	inventory := newRuntimeProcessInventory(&runtimeProcessInventoryFake{}, func() error {
		closed = true
		return nil
	})
	if err := inventory.Close(); err != nil || !closed {
		t.Fatalf("Close() = %v, closed %t", err, closed)
	}
}

func managedRuntimeLabels() map[string]string {
	return map[string]string{
		RuntimeProcessMarkerLabel:     RuntimeProcessMarkerValue,
		RuntimeProcessAssignmentLabel: "10000000-0000-4000-8000-000000000001",
		RuntimeProcessSessionLabel:    "20000000-0000-4000-8000-000000000001",
		RuntimeProcessTurnLabel:       "30000000-0000-4000-8000-000000000001",
		RuntimeProcessEpochLabel:      "9",
		RuntimeProfileIdentityLabel:   "opencode-acp/v1",
	}
}

type runtimeProcessInventoryFake struct {
	result  mobyclient.ContainerListResult
	err     error
	options []mobyclient.ContainerListOptions
}

func (api *runtimeProcessInventoryFake) ContainerList(_ context.Context, options mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
	api.options = append(api.options, options)
	return api.result, api.err
}
