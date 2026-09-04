package docker

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jozala/omnigrex/internal/store"
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
		store.RuntimeLabelAssignmentID+"="+exactRuntimeLabels()[store.RuntimeLabelAssignmentID],
		store.RuntimeLabelSessionID+"="+exactRuntimeLabels()[store.RuntimeLabelSessionID],
		store.RuntimeLabelTurnID+"="+exactRuntimeLabels()[store.RuntimeLabelTurnID],
		store.RuntimeLabelEpoch+"=9",
	)
	if !maps.EqualFunc(api.listOptions[0].Filters, wantFilters, maps.Equal) {
		t.Fatalf("ContainerList() filters = %#v, want %#v", api.listOptions[0].Filters, wantFilters)
	}
	if len(api.stopped) != 0 || len(api.removed) != 0 {
		t.Fatalf("absent cleanup mutated containers: stop %v remove %v", api.stopped, api.removed)
	}
}

func TestExactLabelCleanerStopsRemovesAndConfirmsMatchingContainersAreAbsent(t *testing.T) {
	labels := exactRuntimeLabels()
	api := &exactLabelCleanupFake{lists: []mobyclient.ContainerListResult{
		{Items: []container.Summary{{ID: "container-1", Labels: maps.Clone(labels)}, {ID: "container-2", Labels: maps.Clone(labels)}}},
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

func TestExactLabelCleanerRejectsMismatchedFilteredResultWithoutMutation(t *testing.T) {
	labels := exactRuntimeLabels()
	mismatch := maps.Clone(labels)
	mismatch[store.RuntimeLabelTurnID] = "30000000-0000-4000-8000-000000000099"
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
				lists:   []mobyclient.ContainerListResult{{Items: []container.Summary{{ID: "container-1", Labels: exactRuntimeLabels()}}}, {}},
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

func TestExactLabelCleanerValidatesIdentityEpochAndTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Nanosecond, maximumExactLabelCleanupTimeout + time.Microsecond} {
		if err := validateExactLabelCleanerOptions(ExactLabelCleanerOptions{StopTimeout: timeout}); !errors.Is(err, ErrInvalidExactLabelCleanup) {
			t.Errorf("validateExactLabelCleanerOptions(%s) error = %v", timeout, err)
		}
	}
	labels := exactRuntimeLabels()
	for _, mutate := range []func(map[string]string){
		func(labels map[string]string) { delete(labels, store.RuntimeLabelSessionID) },
		func(labels map[string]string) { labels["unexpected"] = "value" },
		func(labels map[string]string) { labels[store.RuntimeLabelAssignmentID] = "not-a-uuid" },
		func(labels map[string]string) { labels[store.RuntimeLabelEpoch] = "0" },
		func(labels map[string]string) { labels[store.RuntimeLabelEpoch] = "09" },
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
		store.RuntimeLabelAssignmentID: "10000000-0000-4000-8000-000000000001",
		store.RuntimeLabelSessionID:    "20000000-0000-4000-8000-000000000001",
		store.RuntimeLabelTurnID:       "30000000-0000-4000-8000-000000000001",
		store.RuntimeLabelEpoch:        "9",
	}
}

type exactLabelCleanupFake struct {
	lists       []mobyclient.ContainerListResult
	listOptions []mobyclient.ContainerListOptions
	listErr     error
	stopped     map[string]int
	stopErr     error
	removed     map[string]mobyclient.ContainerRemoveOptions
	removeErr   error
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
