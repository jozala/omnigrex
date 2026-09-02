package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"
)

const (
	readinessUser        = "10001:10001"
	readinessMemoryBytes = 128 << 20
	readinessPIDsLimit   = 32
)

var readinessSequence atomic.Uint64

type ReadinessProbeOptions struct {
	AgentNetwork       string
	WorkspaceVolume    string
	RuntimeStateVolume string
	MiseVolume         string
	AgentImage         string
}

type readinessAPI interface {
	Ping(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error)
	ClientVersion() string
	ImageInspect(context.Context, string, ...mobyclient.ImageInspectOption) (mobyclient.ImageInspectResult, error)
	NetworkInspect(context.Context, string, mobyclient.NetworkInspectOptions) (mobyclient.NetworkInspectResult, error)
	VolumeInspect(context.Context, string, mobyclient.VolumeInspectOptions) (mobyclient.VolumeInspectResult, error)
	ContainerCreate(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error)
	ContainerStart(context.Context, string, mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error)
	ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult
	ContainerRemove(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error)
}

type ReadinessProbe struct {
	api     readinessAPI
	options ReadinessProbeOptions
	close   func() error
}

func NewReadinessProbe(options ReadinessProbeOptions) (*ReadinessProbe, error) {
	if err := validateReadinessProbeOptions(options); err != nil {
		return nil, err
	}
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Docker readiness client: %w", err)
	}
	return &ReadinessProbe{api: client, options: options, close: client.Close}, nil
}

func newReadinessProbe(api readinessAPI, options ReadinessProbeOptions) (*ReadinessProbe, error) {
	if err := validateReadinessProbeOptions(options); err != nil {
		return nil, err
	}
	return &ReadinessProbe{api: api, options: options, close: func() error { return nil }}, nil
}

func validateReadinessProbeOptions(options ReadinessProbeOptions) error {
	values := map[string]string{
		"agent network":        options.AgentNetwork,
		"workspace volume":     options.WorkspaceVolume,
		"runtime-state volume": options.RuntimeStateVolume,
		"mise volume":          options.MiseVolume,
		"agent image":          options.AgentImage,
	}
	for name, value := range values {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("Docker readiness %s must be non-empty and trimmed", name)
		}
	}
	return nil
}

func (probe *ReadinessProbe) Close() error {
	return probe.close()
}

func (probe *ReadinessProbe) Check(ctx context.Context) error {
	ping, err := probe.api.Ping(ctx, mobyclient.PingOptions{
		NegotiateAPIVersion: true,
		ForceNegotiate:      true,
	})
	if err != nil {
		return fmt.Errorf("negotiate Docker API version: %w", err)
	}
	if err := validateAPIVersions(ping.APIVersion, probe.api.ClientVersion()); err != nil {
		return err
	}
	if _, err := probe.api.ImageInspect(ctx, probe.options.AgentImage); err != nil {
		return fmt.Errorf("inspect agent image %q: %w", probe.options.AgentImage, err)
	}
	if _, err := probe.api.NetworkInspect(ctx, probe.options.AgentNetwork, mobyclient.NetworkInspectOptions{}); err != nil {
		return fmt.Errorf("inspect agent network %q: %w", probe.options.AgentNetwork, err)
	}
	for _, volume := range []string{probe.options.WorkspaceVolume, probe.options.RuntimeStateVolume, probe.options.MiseVolume} {
		if _, err := probe.api.VolumeInspect(ctx, volume, mobyclient.VolumeInspectOptions{}); err != nil {
			return fmt.Errorf("inspect volume %q: %w", volume, err)
		}
	}

	subpath := "readiness-" + strconv.FormatUint(readinessSequence.Add(1), 10) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := probe.runContainer(ctx, probe.assignmentContainer(subpath, false)); err != nil {
		return errors.Join(
			fmt.Errorf("create assignment probe subpaths: %w", err),
			probe.cleanup(subpath),
		)
	}
	runtimeErr := probe.runContainer(ctx, probe.runtimeContainer(subpath))
	if runtimeErr != nil {
		runtimeErr = fmt.Errorf("exercise Runtime Process writable paths: %w", runtimeErr)
	}
	return errors.Join(runtimeErr, probe.cleanup(subpath))
}

func (probe *ReadinessProbe) cleanup(subpath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := probe.runContainer(ctx, probe.assignmentContainer(subpath, true)); err != nil {
		return fmt.Errorf("clean assignment probe subpaths: %w", err)
	}
	return nil
}

func (probe *ReadinessProbe) assignmentContainer(subpath string, remove bool) mobyclient.ContainerCreateOptions {
	script := `set -eu
subpath="$1"
for root in /volumes/workspace /volumes/runtime-state /volumes/mise; do
  if [ "$2" = remove ]; then
    rm -rf "$root/$subpath"
    continue
  fi
  mkdir "$root/$subpath"
  test "$(stat -c %u "$root/$subpath")" = 10001
  test "$(stat -c %g "$root/$subpath")" = 10001
  printf initialized > "$root/$subpath/.omnigrex-readiness"
done`
	action := "create"
	if remove {
		action = "remove"
	}
	return hardenedProbeContainer(probe.options.AgentImage, "none", script, []string{"sh", subpath, action}, []mount.Mount{
		volumeMount(probe.options.WorkspaceVolume, "/volumes/workspace", ""),
		volumeMount(probe.options.RuntimeStateVolume, "/volumes/runtime-state", ""),
		volumeMount(probe.options.MiseVolume, "/volumes/mise", ""),
	}, map[string]string{"/tmp": probeTmpfs(8 << 20)})
}

func (probe *ReadinessProbe) runtimeContainer(subpath string) mobyclient.ContainerCreateOptions {
	script := `set -eu
for path in /workspace /home/opencode/.local/share/opencode /home/opencode/.local/share/mise; do
  IFS= read -r marker < "$path/.omnigrex-readiness" || test -n "$marker"
  test "$marker" = initialized
  printf writable > "$path/.omnigrex-write-probe"
  rm "$path/.omnigrex-write-probe"
done
for path in /home/opencode/.cache /home/opencode/.config /home/opencode/.local/state /home/opencode/.opencode /tmp/opencode; do
  printf writable > "$path/.omnigrex-write-probe"
  rm "$path/.omnigrex-write-probe"
done
if touch /omnigrex-root-write-probe 2>/dev/null; then
  exit 1
fi`
	return hardenedProbeContainer(probe.options.AgentImage, probe.options.AgentNetwork, script, []string{"sh"}, []mount.Mount{
		volumeMount(probe.options.WorkspaceVolume, "/workspace", subpath),
		volumeMount(probe.options.RuntimeStateVolume, "/home/opencode/.local/share/opencode", subpath),
		volumeMount(probe.options.MiseVolume, "/home/opencode/.local/share/mise", subpath),
	}, map[string]string{
		"/home/opencode/.cache":       probeTmpfs(16 << 20),
		"/home/opencode/.config":      probeTmpfs(8 << 20),
		"/home/opencode/.local/state": probeTmpfs(8 << 20),
		"/home/opencode/.opencode":    probeTmpfs(8 << 20),
		"/tmp/opencode":               probeTmpfs(16 << 20),
	})
}

func volumeMount(source, target, subpath string) mount.Mount {
	return mount.Mount{
		Type:   mount.TypeVolume,
		Source: source,
		Target: target,
		VolumeOptions: &mount.VolumeOptions{
			Subpath: subpath,
		},
	}
}

func hardenedProbeContainer(image, network, script string, arguments []string, mounts []mount.Mount, tmpfs map[string]string) mobyclient.ContainerCreateOptions {
	pids := int64(readinessPIDsLimit)
	return mobyclient.ContainerCreateOptions{
		Config: &container.Config{
			Image:      image,
			User:       readinessUser,
			Entrypoint: []string{"sh", "-c"},
			Cmd:        append([]string{script}, arguments...),
			Labels:     map[string]string{"io.omnigrex.readiness-probe": "true"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(network),
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Mounts:         mounts,
			Tmpfs:          tmpfs,
			Resources: container.Resources{
				Memory:    readinessMemoryBytes,
				PidsLimit: &pids,
			},
		},
	}
}

func probeTmpfs(size int64) string {
	return "rw,nosuid,nodev,noexec,uid=10001,gid=10001,mode=0700,size=" + strconv.FormatInt(size, 10)
}

func (probe *ReadinessProbe) runContainer(ctx context.Context, options mobyclient.ContainerCreateOptions) error {
	return runOneShotContainer(ctx, probe.api, options, "readiness probe")
}
