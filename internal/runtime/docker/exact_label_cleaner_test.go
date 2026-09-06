package docker

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestExactLabelCleanerUsesAllExactFiltersAndAcceptsAbsence(t *testing.T) {
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{{}}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: 1500 * time.Millisecond})

	if err := cleaner.EnsureAbsent(context.Background(), exactRuntimeLabels()); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	if len(api.listOptions) != 1 || !api.listOptions[0].All {
		t.Fatalf("ContainerList() options = %#v", api.listOptions)
	}
	wantFilters := make(mobyclient.Filters).Add("label",
		RuntimeProcessAssignmentLabel+"="+exactRuntimeLabels()[RuntimeProcessAssignmentLabel],
		RuntimeProcessSessionLabel+"="+exactRuntimeLabels()[RuntimeProcessSessionLabel],
		RuntimeProcessTurnLabel+"="+exactRuntimeLabels()[RuntimeProcessTurnLabel],
		RuntimeProcessEpochLabel+"=9",
	)
	if !maps.EqualFunc(api.listOptions[0].Filters, wantFilters, maps.Equal) {
		t.Fatalf("ContainerList() filters = %#v, want %#v", api.listOptions[0].Filters, wantFilters)
	}
	if len(api.stopped) != 0 || len(api.removed) != 0 {
		t.Fatalf("absent cleanup mutated containers: stop %v remove %v", api.stopped, api.removed)
	}
}

func TestExactLabelCleanerIncludesRuntimeProfileWhenSupplied(t *testing.T) {
	labels := exactRuntimeLabels()
	labels[RuntimeProfileIdentityLabel] = "opencode-acp/v1"
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{{}}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})

	if err := cleaner.EnsureAbsent(context.Background(), labels); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	wantFilters := make(mobyclient.Filters).Add("label",
		RuntimeProcessAssignmentLabel+"="+labels[RuntimeProcessAssignmentLabel],
		RuntimeProcessSessionLabel+"="+labels[RuntimeProcessSessionLabel],
		RuntimeProcessTurnLabel+"="+labels[RuntimeProcessTurnLabel],
		RuntimeProcessEpochLabel+"=9",
		RuntimeProfileIdentityLabel+"=opencode-acp/v1",
	)
	if len(api.listOptions) != 1 || !maps.EqualFunc(api.listOptions[0].Filters, wantFilters, maps.Equal) {
		t.Fatalf("ContainerList() filters = %#v, want %#v", api.listOptions, wantFilters)
	}
}

func TestExactLabelCleanerStopsRemovesAndConfirmsMatchingContainersAreAbsent(t *testing.T) {
	labels := exactRuntimeLabels()
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{
		{Items: []container.Summary{{ID: "container-1", Labels: markedExactRuntimeLabels()}, {ID: "container-2", Labels: markedExactRuntimeLabels()}}},
		{},
	}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: 1500 * time.Millisecond})

	if err := cleaner.EnsureAbsent(context.Background(), labels); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	if len(api.listOptions) != 2 {
		t.Fatalf("ContainerList() calls = %d, want final absence check", len(api.listOptions))
	}
	if !maps.Equal(api.stopped, map[string]int{"container-1": 2, "container-2": 2}) {
		t.Fatalf("ContainerStop() calls = %#v", api.stopped)
	}
	if !maps.Equal(api.removed, map[string]mobyclient.ContainerRemoveOptions{
		"container-1": {Force: true}, "container-2": {Force: true},
	}) {
		t.Fatalf("ContainerRemove() calls = %#v", api.removed)
	}
}

func TestExactLabelCleanerRemovesCompleteLegacyContainer(t *testing.T) {
	labels := exactRuntimeLabels()
	legacy := maps.Clone(labels)
	legacy[RuntimeProfileIdentityLabel] = "opencode-acp/v1"
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{
		{Items: []container.Summary{{ID: "legacy", Labels: legacy}}},
		{},
	}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})

	if err := cleaner.EnsureAbsent(context.Background(), labels); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	if _, stopped := api.stopped["legacy"]; !stopped {
		t.Fatal("complete legacy Runtime Process was not stopped")
	}
	if _, removed := api.removed["legacy"]; !removed {
		t.Fatal("complete legacy Runtime Process was not removed")
	}
}

func TestExactLabelCleanerLeavesUnprovenExactLabelMatchesUntouched(t *testing.T) {
	markerFalse := exactRuntimeLabels()
	markerFalse[RuntimeProfileIdentityLabel] = "opencode-acp/v1"
	markerFalse[RuntimeProcessMarkerLabel] = "false"
	partialLegacy := exactRuntimeLabels()
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{{Items: []container.Summary{
		{ID: "marker-false", Labels: markerFalse},
		{ID: "partial-legacy", Labels: partialLegacy},
	}}}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})

	if err := cleaner.EnsureAbsent(context.Background(), exactRuntimeLabels()); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	if len(api.stopped) != 0 || len(api.removed) != 0 {
		t.Fatalf("unproven containers were mutated: stop %#v remove %#v", api.stopped, api.removed)
	}
}

func TestExactLabelCleanerRejectsMismatchedFilteredResultWithoutMutation(t *testing.T) {
	labels := exactRuntimeLabels()
	mismatch := maps.Clone(labels)
	mismatch[RuntimeProcessTurnLabel] = "30000000-0000-4000-8000-000000000099"
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{{Items: []container.Summary{
		{ID: "matching", Labels: maps.Clone(labels)},
		{ID: "mismatched", Labels: mismatch},
	}}}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})

	if err := cleaner.EnsureAbsent(context.Background(), labels); err == nil {
		t.Fatal("EnsureAbsent() error = nil, want defensive mismatch rejection")
	}
	if len(api.stopped) != 0 || len(api.removed) != 0 {
		t.Fatalf("mismatched result mutated containers: stop %v remove %v", api.stopped, api.removed)
	}
}

func TestExactLabelCleanerRejectsDifferentRuntimeProfileWithoutMutation(t *testing.T) {
	labels := exactRuntimeLabels()
	labels[RuntimeProfileIdentityLabel] = "opencode-acp/v1"
	mismatch := maps.Clone(labels)
	mismatch[RuntimeProfileIdentityLabel] = "opencode-acp/v2"
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{{Items: []container.Summary{{
		ID: "different-profile", Labels: mismatch,
	}}}}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})

	if err := cleaner.EnsureAbsent(context.Background(), labels); err == nil {
		t.Fatal("EnsureAbsent() error = nil, want defensive Runtime Profile mismatch rejection")
	}
	if len(api.stopped) != 0 || len(api.removed) != 0 {
		t.Fatalf("different Runtime Profile was mutated: stop %v remove %v", api.stopped, api.removed)
	}
}

func TestExactLabelCleanerTreatsStopAndRemoveRacesAsSuccess(t *testing.T) {
	for _, test := range []struct {
		name      string
		stopErr   error
		removeErr error
		removed   bool
	}{
		{name: "gone before stop", stopErr: errdefs.ErrNotFound},
		{name: "already stopped", stopErr: errdefs.ErrNotModified, removed: true},
		{name: "gone before remove", removeErr: errdefs.ErrNotFound, removed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &exactLabelCleanupFake{
				lists:   []mobyclient.ContainerListResult{{Items: []container.Summary{{ID: "container-1", Labels: markedExactRuntimeLabels()}}}, {}},
				stopErr: test.stopErr, removeErr: test.removeErr,
			}
			cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})

			if err := cleaner.EnsureAbsent(context.Background(), exactRuntimeLabels()); err != nil {
				t.Fatalf("EnsureAbsent() error = %v", err)
			}
			_, removed := api.removed["container-1"]
			if removed != test.removed {
				t.Fatalf("ContainerRemove() called = %t, want %t", removed, test.removed)
			}
		})
	}
}

func TestExactLabelCleanerRevalidatesMalformedManagedContainerBeforeRemoval(t *testing.T) {
	api := &exactLabelCleanupFake{inspects: []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{
		ID:     "malformed-container",
		Config: &container.Config{Labels: map[string]string{RuntimeProcessMarkerLabel: RuntimeProcessMarkerValue}},
	}}}}
	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: 1500 * time.Millisecond})

	if err := cleaner.EnsureManagedContainerAbsent(context.Background(), "malformed-container"); err != nil {
		t.Fatalf("EnsureManagedContainerAbsent() error = %v", err)
	}
	if !maps.Equal(api.stopped, map[string]int{"malformed-container": 2}) ||
		!maps.Equal(api.removed, map[string]mobyclient.ContainerRemoveOptions{"malformed-container": {Force: true}}) {
		t.Fatalf("cleanup calls = stop %#v remove %#v", api.stopped, api.removed)
	}
}

func TestExactLabelCleanerRefusesMalformedContainerWhenOwnershipChangedAfterInventory(t *testing.T) {
	legacy := managedRuntimeLabels()
	delete(legacy, RuntimeProcessMarkerLabel)
	partialLegacy := map[string]string{RuntimeProcessTurnLabel: "malformed"}
	markerFalse := maps.Clone(partialLegacy)
	markerFalse[RuntimeProcessMarkerLabel] = "false"
	tests := []struct {
		name        string
		containerID string
		inspected   container.InspectResponse
	}{
		{name: "marker false", containerID: "marker-false", inspected: container.InspectResponse{ID: "marker-false", Config: &container.Config{Labels: markerFalse}}},
		{name: "unrelated single reserved label", containerID: "marker-absent", inspected: container.InspectResponse{ID: "marker-absent", Config: &container.Config{Labels: partialLegacy}}},
		{name: "marker absent complete legacy", containerID: "legacy", inspected: container.InspectResponse{ID: "legacy", Config: &container.Config{Labels: legacy}}},
		{name: "ID mismatch", containerID: "inventoried", inspected: container.InspectResponse{ID: "replacement", Config: &container.Config{Labels: map[string]string{RuntimeProcessMarkerLabel: RuntimeProcessMarkerValue}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &exactLabelCleanupFake{inspects: []mobyclient.ContainerInspectResult{{Container: test.inspected}}}
			cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})

			if err := cleaner.EnsureManagedContainerAbsent(context.Background(), test.containerID); !errors.Is(err, ErrInvalidExactLabelCleanup) {
				t.Fatalf("EnsureManagedContainerAbsent() error = %v, want ErrInvalidExactLabelCleanup", err)
			}
			if len(api.stopped) != 0 || len(api.removed) != 0 {
				t.Fatalf("changed container was mutated: stop %#v remove %#v", api.stopped, api.removed)
			}
		})
	}
}

func TestExactLabelCleanerLeavesContainerChangedAfterInventoryUntouched(t *testing.T) {
	api := &exactLabelCleanupFake{
		lists: []mobyclient.ContainerListResult{
			{Items: []container.Summary{{ID: "changed", Labels: map[string]string{RuntimeProcessMarkerLabel: RuntimeProcessMarkerValue}}}},
			{},
		},
		inspects: []mobyclient.ContainerInspectResult{{Container: container.InspectResponse{
			ID: "changed", Config: &container.Config{Labels: managedRuntimeLabels()},
		}}},
	}
	inventory := newRuntimeProcessInventory(api, func() error { return nil })
	snapshot, err := inventory.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Malformed) != 1 || snapshot.Malformed[0].ContainerID != "changed" {
		t.Fatalf("initial malformed inventory = %#v", snapshot)
	}

	cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})
	if err := cleaner.EnsureManagedContainerAbsent(context.Background(), snapshot.Malformed[0].ContainerID); !errors.Is(err, ErrInvalidExactLabelCleanup) {
		t.Fatalf("EnsureManagedContainerAbsent() error = %v, want ErrInvalidExactLabelCleanup", err)
	}
	if len(api.stopped) != 0 || len(api.removed) != 0 {
		t.Fatalf("container changed after inventory was mutated: stop %#v remove %#v", api.stopped, api.removed)
	}
}

func TestExactLabelCleanerValidatesIdentityEpochAndTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Nanosecond, maximumExactLabelCleanupTimeout + time.Microsecond} {
		if err := validateExactLabelCleanerOptions(ExactLabelCleanerOptions{StopTimeout: timeout}); !errors.Is(err, ErrInvalidExactLabelCleanup) {
			t.Errorf("validateExactLabelCleanerOptions(%s) error = %v", timeout, err)
		}
	}
	labels := exactRuntimeLabels()
	for _, mutate := range []func(map[string]string){
		func(labels map[string]string) { delete(labels, RuntimeProcessSessionLabel) },
		func(labels map[string]string) { labels["unexpected"] = "value" },
		func(labels map[string]string) { labels[RuntimeProcessAssignmentLabel] = "not-a-uuid" },
		func(labels map[string]string) { labels[RuntimeProcessEpochLabel] = "0" },
		func(labels map[string]string) { labels[RuntimeProcessEpochLabel] = "09" },
		func(labels map[string]string) { labels[RuntimeProfileIdentityLabel] = "invalid/profile/identity" },
	} {
		candidate := maps.Clone(labels)
		mutate(candidate)
		api := &exactLabelCleanupFake{}
		cleaner := newExactLabelCleaner(api, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: time.Second})
		if err := cleaner.EnsureAbsent(context.Background(), candidate); !errors.Is(err, ErrInvalidExactLabelCleanup) {
			t.Errorf("EnsureAbsent(%#v) error = %v, want ErrInvalidExactLabelCleanup", candidate, err)
		}
		if len(api.listOptions) != 0 {
			t.Fatal("invalid identity reached Docker API")
		}
	}
}

func TestExactLabelCleanerCloseUsesOwnedClientClose(t *testing.T) {
	closed := false
	cleaner := newExactLabelCleaner(&exactLabelCleanupFake{}, func() error {
		closed = true
		return nil
	}, ExactLabelCleanerOptions{StopTimeout: time.Second})
	if err := cleaner.Close(); err != nil || !closed {
		t.Fatalf("Close() = %v, closed %t", err, closed)
	}
}

func exactRuntimeLabels() map[string]string {
	return map[string]string{
		RuntimeProcessAssignmentLabel: "10000000-0000-4000-8000-000000000001",
		RuntimeProcessSessionLabel:    "20000000-0000-4000-8000-000000000001",
		RuntimeProcessTurnLabel:       "30000000-0000-4000-8000-000000000001",
		RuntimeProcessEpochLabel:      "9",
	}
}

func markedExactRuntimeLabels() map[string]string {
	labels := exactRuntimeLabels()
	labels[RuntimeProcessMarkerLabel] = RuntimeProcessMarkerValue
	return labels
}

type exactLabelCleanupFake struct {
	lists       []mobyclient.ContainerListResult
	listOptions []mobyclient.ContainerListOptions
	listErr     error
	inspects    []mobyclient.ContainerInspectResult
	inspectErr  error
	stopped     map[string]int
	stopErr     error
	removed     map[string]mobyclient.ContainerRemoveOptions
	removeErr   error
}

func (api *exactLabelCleanupFake) ContainerInspect(_ context.Context, _ string, _ mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
	if api.inspectErr != nil {
		return mobyclient.ContainerInspectResult{}, api.inspectErr
	}
	if len(api.inspects) == 0 {
		return mobyclient.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	result := api.inspects[0]
	api.inspects = api.inspects[1:]
	return result, nil
}

func (api *exactLabelCleanupFake) ContainerList(_ context.Context, options mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
	api.listOptions = append(api.listOptions, options)
	if api.listErr != nil {
		return mobyclient.ContainerListResult{}, api.listErr
	}
	if len(api.lists) == 0 {
		return mobyclient.ContainerListResult{}, nil
	}
	result := api.lists[0]
	api.lists = api.lists[1:]
	return result, nil
}

func (api *exactLabelCleanupFake) ContainerStop(_ context.Context, id string, options mobyclient.ContainerStopOptions) (mobyclient.ContainerStopResult, error) {
	if api.stopped == nil {
		api.stopped = make(map[string]int)
	}
	if options.Timeout != nil {
		api.stopped[id] = *options.Timeout
	}
	return mobyclient.ContainerStopResult{}, api.stopErr
}

func (api *exactLabelCleanupFake) ContainerRemove(_ context.Context, id string, options mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	if api.removed == nil {
		api.removed = make(map[string]mobyclient.ContainerRemoveOptions)
	}
	api.removed[id] = options
	return mobyclient.ContainerRemoveResult{}, api.removeErr
}
