package docker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

const cleanerAssignmentID = "10000000-0000-4000-8000-000000000001"

func TestAssignmentRuntimeStateCleanerDeletesOnlyCanonicalTargetAndIsIdempotent(t *testing.T) {
	target := "assignment-" + cleanerAssignmentID + "/runtime-state"
	neighbor := "assignment-10000000-0000-4000-8000-000000000002/runtime-state"
	sibling := "assignment-" + cleanerAssignmentID + "/other"
	api := newAssignmentRuntimeStateCleanupFake(target, neighbor, sibling)
	cleaner, err := newAssignmentRuntimeStateCleaner(api, func() error { return nil }, assignmentRuntimeStateCleanerOptions())
	if err != nil {
		t.Fatalf("newAssignmentRuntimeStateCleaner() error = %v", err)
	}

	for range 2 {
		if err := cleaner.EnsureAbsent(context.Background(), cleanerAssignmentID, target); err != nil {
			t.Fatalf("EnsureAbsent() error = %v", err)
		}
	}
	if api.hasPath(target) || !api.hasPath(neighbor) || !api.hasPath(sibling) {
		t.Fatalf("paths after cleanup = %#v", api.snapshotPaths())
	}
	if api.volumeInspections != 2 || len(api.created) != 2 {
		t.Fatalf("cleanup calls = %d volume inspections, %d containers", api.volumeInspections, len(api.created))
	}
}

func TestAssignmentRuntimeStateCleanerSupportsConcurrentRetries(t *testing.T) {
	target := "assignment-" + cleanerAssignmentID + "/runtime-state"
	neighbor := "assignment-10000000-0000-4000-8000-000000000002/runtime-state"
	api := newAssignmentRuntimeStateCleanupFake(target, neighbor)
	cleaner, err := newAssignmentRuntimeStateCleaner(api, func() error { return nil }, assignmentRuntimeStateCleanerOptions())
	if err != nil {
		t.Fatal(err)
	}

	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- cleaner.EnsureAbsent(context.Background(), cleanerAssignmentID, target)
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent EnsureAbsent() error = %v", err)
		}
	}
	if api.hasPath(target) || !api.hasPath(neighbor) {
		t.Fatalf("paths after concurrent cleanup = %#v", api.snapshotPaths())
	}
}

func TestAssignmentRuntimeStateCleanerRetrySucceedsAfterDeletionWasNotAcknowledged(t *testing.T) {
	target := "assignment-" + cleanerAssignmentID + "/runtime-state"
	api := newAssignmentRuntimeStateCleanupFake(target)
	api.waitErrs = []error{errors.New("Docker connection lost after deletion"), nil}
	cleaner, err := newAssignmentRuntimeStateCleaner(api, func() error { return nil }, assignmentRuntimeStateCleanerOptions())
	if err != nil {
		t.Fatal(err)
	}

	if err := cleaner.EnsureAbsent(context.Background(), cleanerAssignmentID, target); err == nil {
		t.Fatal("first EnsureAbsent() error = nil, want lost acknowledgement")
	}
	if api.hasPath(target) {
		t.Fatal("target still exists after unacknowledged deletion")
	}
	if err := cleaner.EnsureAbsent(context.Background(), cleanerAssignmentID, target); err != nil {
		t.Fatalf("retry EnsureAbsent() error = %v", err)
	}
}

func TestAssignmentRuntimeStateCleanerRejectsArbitraryPathsAndCommandInjection(t *testing.T) {
	tests := []struct {
		name         string
		assignmentID string
		path         string
	}{
		{name: "relative neighbor", assignmentID: cleanerAssignmentID, path: "assignment-10000000-0000-4000-8000-000000000002/runtime-state"},
		{name: "traversal", assignmentID: cleanerAssignmentID, path: "assignment-" + cleanerAssignmentID + "/../runtime-state"},
		{name: "absolute", assignmentID: cleanerAssignmentID, path: "/runtime-state/assignment-" + cleanerAssignmentID + "/runtime-state"},
		{name: "assignment root", assignmentID: cleanerAssignmentID, path: "assignment-" + cleanerAssignmentID},
		{name: "injected path", assignmentID: cleanerAssignmentID, path: "assignment-" + cleanerAssignmentID + "/runtime-state; touch /tmp/pwned"},
		{name: "injected ID", assignmentID: cleanerAssignmentID + ";rm -rf /", path: "assignment-" + cleanerAssignmentID + ";rm -rf //runtime-state"},
		{name: "uppercase UUID", assignmentID: "10000000-0000-4000-8000-00000000000A", path: "assignment-10000000-0000-4000-8000-00000000000A/runtime-state"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newAssignmentRuntimeStateCleanupFake()
			cleaner, err := newAssignmentRuntimeStateCleaner(api, func() error { return nil }, assignmentRuntimeStateCleanerOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := cleaner.EnsureAbsent(context.Background(), test.assignmentID, test.path); !errors.Is(err, ErrInvalidAssignmentRuntimeStateCleanup) {
				t.Fatalf("EnsureAbsent() error = %v, want ErrInvalidAssignmentRuntimeStateCleanup", err)
			}
			if api.volumeInspections != 0 || len(api.created) != 0 {
				t.Fatal("invalid path reached Docker API")
			}
		})
	}
}

func TestAssignmentRuntimeStateCleanerUsesHardenedOneShotContainer(t *testing.T) {
	api := newAssignmentRuntimeStateCleanupFake()
	options := assignmentRuntimeStateCleanerOptions()
	cleaner, err := newAssignmentRuntimeStateCleaner(api, func() error { return nil }, options)
	if err != nil {
		t.Fatal(err)
	}
	path := "assignment-" + cleanerAssignmentID + "/runtime-state"
	if err := cleaner.EnsureAbsent(context.Background(), cleanerAssignmentID, path); err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 1 {
		t.Fatalf("created containers = %d", len(api.created))
	}
	created := api.created[0]
	if created.Platform == nil || created.Platform.OS != options.Platform.OS || created.Platform.Architecture != options.Platform.Architecture {
		t.Errorf("platform = %#v", created.Platform)
	}
	if created.Config.User != options.User || created.Config.Image != options.Image {
		t.Errorf("identity/image = %q %q", created.Config.User, created.Config.Image)
	}
	if created.HostConfig.NetworkMode != "none" || !created.HostConfig.ReadonlyRootfs {
		t.Errorf("network/root filesystem = %q/%t", created.HostConfig.NetworkMode, created.HostConfig.ReadonlyRootfs)
	}
	if !slices.Equal(created.HostConfig.CapDrop, []string{"ALL"}) || !slices.Equal(created.HostConfig.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("hardening = capabilities %v security %v", created.HostConfig.CapDrop, created.HostConfig.SecurityOpt)
	}
	if created.HostConfig.Resources.Memory != assignmentRuntimeStateCleanerMemoryBytes || created.HostConfig.Resources.PidsLimit == nil || *created.HostConfig.Resources.PidsLimit != assignmentRuntimeStateCleanerPIDsLimit {
		t.Errorf("resources = %#v", created.HostConfig.Resources)
	}
	if len(created.HostConfig.Mounts) != 1 || created.HostConfig.Mounts[0].Source != options.RuntimeStateVolume || created.HostConfig.Mounts[0].Target != assignmentRuntimeStateMount || created.HostConfig.Mounts[0].ReadOnly {
		t.Errorf("mounts = %#v", created.HostConfig.Mounts)
	}
	if len(created.Config.Cmd) != 3 || created.Config.Cmd[2] != cleanerAssignmentID {
		t.Fatalf("command = %#v", created.Config.Cmd)
	}
	if strings.Contains(created.Config.Cmd[0], cleanerAssignmentID) || strings.Contains(strings.Join(created.Config.Cmd, " "), path) {
		t.Fatalf("target path was interpolated into shell source: %#v", created.Config.Cmd)
	}
	for _, expected := range []string{"rm -rf --", "test ! -e", "test ! -L"} {
		if !strings.Contains(created.Config.Cmd[0], expected) {
			t.Errorf("cleanup script missing %q: %q", expected, created.Config.Cmd[0])
		}
	}
}

func TestAssignmentRuntimeStateCleanerValidatesOptionsAndVolumeAvailability(t *testing.T) {
	base := assignmentRuntimeStateCleanerOptions()
	tests := []struct {
		name   string
		mutate func(*AssignmentRuntimeStateCleanerOptions)
	}{
		{name: "image", mutate: func(options *AssignmentRuntimeStateCleanerOptions) { options.Image = "omnigrex/opencode:latest" }},
		{name: "platform", mutate: func(options *AssignmentRuntimeStateCleanerOptions) { options.Platform.Architecture = "s390x" }},
		{name: "root user", mutate: func(options *AssignmentRuntimeStateCleanerOptions) { options.User = "0:0" }},
		{name: "volume path", mutate: func(options *AssignmentRuntimeStateCleanerOptions) { options.RuntimeStateVolume = "/tmp/state" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := base
			test.mutate(&options)
			if _, err := newAssignmentRuntimeStateCleaner(newAssignmentRuntimeStateCleanupFake(), func() error { return nil }, options); !errors.Is(err, ErrInvalidAssignmentRuntimeStateCleanup) {
				t.Fatalf("newAssignmentRuntimeStateCleaner() error = %v", err)
			}
		})
	}

	api := newAssignmentRuntimeStateCleanupFake()
	api.volumeErr = errors.New("volume missing")
	cleaner, err := newAssignmentRuntimeStateCleaner(api, func() error { return nil }, base)
	if err != nil {
		t.Fatal(err)
	}
	path := "assignment-" + cleanerAssignmentID + "/runtime-state"
	if err := cleaner.EnsureAbsent(context.Background(), cleanerAssignmentID, path); err == nil {
		t.Fatal("EnsureAbsent() error = nil, want volume inspection error")
	}
	if len(api.created) != 0 {
		t.Fatal("cleanup container created for unavailable volume")
	}
}

func assignmentRuntimeStateCleanerOptions() AssignmentRuntimeStateCleanerOptions {
	return AssignmentRuntimeStateCleanerOptions{
		Image:              testImage,
		Platform:           Platform{OS: "linux", Architecture: "arm64"},
		User:               "10001:10001",
		RuntimeStateVolume: "omnigrex-runtime-state",
	}
}

type assignmentRuntimeStateCleanupFake struct {
	mutex             sync.Mutex
	paths             map[string]struct{}
	created           []mobyclient.ContainerCreateOptions
	containers        map[string]mobyclient.ContainerCreateOptions
	volumeInspections int
	volumeErr         error
	waitErrs          []error
	waitIndex         int
	nextID            int
}

func newAssignmentRuntimeStateCleanupFake(paths ...string) *assignmentRuntimeStateCleanupFake {
	return &assignmentRuntimeStateCleanupFake{
		paths:      maps.Clone(sliceSet(paths)),
		containers: make(map[string]mobyclient.ContainerCreateOptions),
	}
}

func (api *assignmentRuntimeStateCleanupFake) VolumeInspect(context.Context, string, mobyclient.VolumeInspectOptions) (mobyclient.VolumeInspectResult, error) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.volumeInspections++
	return mobyclient.VolumeInspectResult{}, api.volumeErr
}

func (api *assignmentRuntimeStateCleanupFake) ContainerCreate(_ context.Context, options mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.nextID++
	id := "cleaner-" + string(rune('a'+api.nextID))
	api.created = append(api.created, options)
	api.containers[id] = options
	return mobyclient.ContainerCreateResult{ID: id}, nil
}

func (api *assignmentRuntimeStateCleanupFake) ContainerStart(_ context.Context, id string, _ mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	options := api.containers[id]
	assignmentID := options.Config.Cmd[len(options.Config.Cmd)-1]
	delete(api.paths, "assignment-"+assignmentID+"/runtime-state")
	return mobyclient.ContainerStartResult{}, nil
}

func (api *assignmentRuntimeStateCleanupFake) ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	errs := make(chan error, 1)
	api.mutex.Lock()
	var err error
	if api.waitIndex < len(api.waitErrs) {
		err = api.waitErrs[api.waitIndex]
	}
	api.waitIndex++
	api.mutex.Unlock()
	if err != nil {
		errs <- err
	} else {
		result <- container.WaitResponse{StatusCode: 0}
	}
	return mobyclient.ContainerWaitResult{Result: result, Error: errs}
}

func (api *assignmentRuntimeStateCleanupFake) ContainerRemove(_ context.Context, id string, _ mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	delete(api.containers, id)
	return mobyclient.ContainerRemoveResult{}, nil
}

func (api *assignmentRuntimeStateCleanupFake) hasPath(path string) bool {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	_, present := api.paths[path]
	return present
}

func (api *assignmentRuntimeStateCleanupFake) snapshotPaths() map[string]struct{} {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return maps.Clone(api.paths)
}

func sliceSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
