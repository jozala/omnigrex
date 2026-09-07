package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestReadinessProbeChecksResourcesAndWritableSubpaths(t *testing.T) {
	api := &fakeReadinessAPI{clientVersion: "1.45"}
	probe, err := newReadinessProbe(api, testReadinessProbeOptions())
	if err != nil {
		t.Fatalf("newReadinessProbe() error = %v", err)
	}

	if err := probe.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if api.image != "omnigrex/opencode:qualified" {
		t.Errorf("inspected image = %q", api.image)
	}
	if api.network != "omnigrex-agent" {
		t.Errorf("inspected network = %q", api.network)
	}
	if len(api.volumes) != 3 {
		t.Fatalf("inspected volumes = %v, want three", api.volumes)
	}
	if len(api.created) != 3 {
		t.Fatalf("created containers = %d, want initializer, runtime, cleanup", len(api.created))
	}
	runtime := api.created[1]
	if runtime.Config.User != readinessUser || !runtime.HostConfig.ReadonlyRootfs {
		t.Errorf("runtime probe identity/hardening = user %q read-only %t", runtime.Config.User, runtime.HostConfig.ReadonlyRootfs)
	}
	if runtime.HostConfig.NetworkMode != "omnigrex-agent" {
		t.Errorf("runtime probe network = %q", runtime.HostConfig.NetworkMode)
	}
	if runtime.HostConfig.Resources.Memory != readinessMemoryBytes || runtime.HostConfig.Resources.PidsLimit == nil || *runtime.HostConfig.Resources.PidsLimit != readinessPIDsLimit {
		t.Errorf("runtime probe limits = %+v", runtime.HostConfig.Resources)
	}
	if len(runtime.HostConfig.CapDrop) != 1 || runtime.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("runtime probe capabilities = %v", runtime.HostConfig.CapDrop)
	}
	for _, volume := range runtime.HostConfig.Mounts {
		if volume.VolumeOptions == nil || volume.VolumeOptions.Subpath == "" {
			t.Errorf("runtime probe volume lacks subpath: %+v", volume)
		}
	}
}

func TestReadinessProbeInspectDoesNotCreateContainers(t *testing.T) {
	api := &fakeReadinessAPI{clientVersion: "1.45"}
	probe, err := newReadinessProbe(api, testReadinessProbeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Inspect(context.Background()); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if api.image != "omnigrex/opencode:qualified" || api.network != "omnigrex-agent" || len(api.volumes) != 3 {
		t.Fatalf("inspected resources = image %q, network %q, volumes %v", api.image, api.network, api.volumes)
	}
	if len(api.created) != 0 {
		t.Fatalf("Inspect() created %d containers", len(api.created))
	}
}

func TestReadinessProbeFailsWhenResourceIsUnavailable(t *testing.T) {
	api := &fakeReadinessAPI{clientVersion: "1.45", networkErr: errors.New("missing")}
	probe, err := newReadinessProbe(api, testReadinessProbeOptions())
	if err != nil {
		t.Fatalf("newReadinessProbe() error = %v", err)
	}

	err = probe.Check(context.Background())
	if err == nil {
		t.Fatal("Check() error = nil, want unavailable resource error")
	}
	if len(api.created) != 0 {
		t.Fatalf("created containers = %d, want none", len(api.created))
	}
}

func TestReadinessProbeReportsEveryUnavailableResource(t *testing.T) {
	api := &fakeReadinessAPI{
		clientVersion: "1.45",
		imageErr:      errors.New("missing image"),
		networkErr:    errors.New("missing network"),
		volumeErr:     errors.New("missing volume"),
	}
	probe, err := newReadinessProbe(api, testReadinessProbeOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = probe.Inspect(context.Background())
	if err == nil {
		t.Fatal("Inspect() error = nil, want aggregated errors")
	}
	for _, want := range []string{"missing image", "missing network", "missing volume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Inspect() error = %q, want %q", err, want)
		}
	}
	if len(api.volumes) != 3 {
		t.Errorf("inspected volumes = %v, want all three", api.volumes)
	}
}

func TestReadinessProbeCleansPartialInitializationAndReportsCleanupFailure(t *testing.T) {
	t.Run("partial initialization", func(t *testing.T) {
		api := &fakeReadinessAPI{clientVersion: "1.45", waitStatuses: []int64{1, 0}}
		probe, err := newReadinessProbe(api, testReadinessProbeOptions())
		if err != nil {
			t.Fatalf("newReadinessProbe() error = %v", err)
		}
		if err := probe.Check(context.Background()); err == nil {
			t.Fatal("Check() error = nil, want initialization failure")
		}
		if len(api.created) != 2 {
			t.Fatalf("created containers = %d, want initializer and cleanup", len(api.created))
		}
	})

	t.Run("cleanup failure", func(t *testing.T) {
		api := &fakeReadinessAPI{clientVersion: "1.45", waitStatuses: []int64{0, 0, 1}}
		probe, err := newReadinessProbe(api, testReadinessProbeOptions())
		if err != nil {
			t.Fatalf("newReadinessProbe() error = %v", err)
		}
		if err := probe.Check(context.Background()); err == nil {
			t.Fatal("Check() error = nil, want cleanup failure")
		}
	})
}

func TestReadinessProbeReportsContainerRemovalFailure(t *testing.T) {
	api := &fakeReadinessAPI{clientVersion: "1.45", removeErr: errors.New("removal denied")}
	probe, err := newReadinessProbe(api, testReadinessProbeOptions())
	if err != nil {
		t.Fatalf("newReadinessProbe() error = %v", err)
	}
	if err := probe.Check(context.Background()); err == nil {
		t.Fatal("Check() error = nil, want container removal failure")
	}
}

func TestReadinessProbeRejectsInvalidOptions(t *testing.T) {
	options := testReadinessProbeOptions()
	options.WorkspaceVolume = ""
	if _, err := newReadinessProbe(&fakeReadinessAPI{}, options); err == nil {
		t.Fatal("newReadinessProbe() error = nil, want invalid options error")
	}
}

func testReadinessProbeOptions() ReadinessProbeOptions {
	return ReadinessProbeOptions{
		AgentNetwork:       "omnigrex-agent",
		WorkspaceVolume:    "omnigrex-workspaces",
		RuntimeStateVolume: "omnigrex-runtime-state",
		MiseVolume:         "omnigrex-mise",
		AgentImage:         "omnigrex/opencode:qualified",
	}
}

type fakeReadinessAPI struct {
	clientVersion string
	imageErr      error
	networkErr    error
	volumeErr     error
	image         string
	network       string
	volumes       []string
	created       []mobyclient.ContainerCreateOptions
	waitStatuses  []int64
	waitIndex     int
	removeErr     error
}

func (api *fakeReadinessAPI) Ping(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error) {
	return mobyclient.PingResult{APIVersion: "1.45"}, nil
}

func (api *fakeReadinessAPI) ClientVersion() string {
	return api.clientVersion
}

func (api *fakeReadinessAPI) ImageInspect(_ context.Context, image string, _ ...mobyclient.ImageInspectOption) (mobyclient.ImageInspectResult, error) {
	api.image = image
	return mobyclient.ImageInspectResult{}, api.imageErr
}

func (api *fakeReadinessAPI) NetworkInspect(_ context.Context, network string, _ mobyclient.NetworkInspectOptions) (mobyclient.NetworkInspectResult, error) {
	api.network = network
	return mobyclient.NetworkInspectResult{}, api.networkErr
}

func (api *fakeReadinessAPI) VolumeInspect(_ context.Context, volume string, _ mobyclient.VolumeInspectOptions) (mobyclient.VolumeInspectResult, error) {
	api.volumes = append(api.volumes, volume)
	return mobyclient.VolumeInspectResult{}, api.volumeErr
}

func (api *fakeReadinessAPI) ContainerCreate(_ context.Context, options mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
	api.created = append(api.created, options)
	return mobyclient.ContainerCreateResult{ID: "container-" + string(rune('0'+len(api.created)))}, nil
}

func (api *fakeReadinessAPI) ContainerStart(context.Context, string, mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error) {
	return mobyclient.ContainerStartResult{}, nil
}

func (api *fakeReadinessAPI) ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	errs := make(chan error, 1)
	status := int64(0)
	if api.waitIndex < len(api.waitStatuses) {
		status = api.waitStatuses[api.waitIndex]
	}
	api.waitIndex++
	result <- container.WaitResponse{StatusCode: status}
	return mobyclient.ContainerWaitResult{Result: result, Error: errs}
}

func (api *fakeReadinessAPI) ContainerRemove(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	return mobyclient.ContainerRemoveResult{}, api.removeErr
}
