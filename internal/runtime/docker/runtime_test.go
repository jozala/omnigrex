package docker

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const testImage = "omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testWorkspacePath = "/workspace"

func TestBuildCreateOptionsAppliesRuntimeIsolation(t *testing.T) {
	options, err := buildCreateOptions(Spec{
		Name:       "omnigrex-agent-assignment-1",
		Image:      testImage,
		Platform:   Platform{OS: "linux", Architecture: "arm64"},
		User:       "10001:10001",
		WorkingDir: "/workspace",
		Command:    []string{"acp"},
		Environment: []string{
			"HOME=/home/opencode",
		},
		Labels: map[string]string{
			"io.omnigrex.assignment": "assignment-1",
		},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Volumes: []VolumeMount{
			{Name: "omnigrex-workspaces", Subpath: "assignment-1/workspace", Target: "/workspace"},
			{Name: "omnigrex-runtime-state", Subpath: "assignment-1/runtime-state", Target: "/home/opencode/.local/share/opencode"},
		},
		Tmpfs: []TmpfsMount{
			{Target: "/tmp/opencode", SizeBytes: 64 << 20},
		},
		MemoryBytes: 512 << 20,
		PIDsLimit:   128,
	})
	if err != nil {
		t.Fatalf("buildCreateOptions() error = %v", err)
	}

	if options.Config.User != "10001:10001" {
		t.Errorf("user = %q, want non-root identity", options.Config.User)
	}
	if options.Platform == nil || options.Platform.OS != "linux" || options.Platform.Architecture != "arm64" {
		t.Errorf("platform = %#v, want linux/arm64", options.Platform)
	}
	if !options.Config.AttachStdin || !options.Config.AttachStdout || !options.Config.AttachStderr || options.Config.Tty {
		t.Errorf("stdio configuration = %+v", options.Config)
	}
	if !options.HostConfig.ReadonlyRootfs {
		t.Error("root filesystem is writable")
	}
	if len(options.HostConfig.CapDrop) != 1 || options.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", options.HostConfig.CapDrop)
	}
	if len(options.HostConfig.SecurityOpt) != 1 || options.HostConfig.SecurityOpt[0] != "no-new-privileges" {
		t.Errorf("SecurityOpt = %v, want [no-new-privileges]", options.HostConfig.SecurityOpt)
	}
	if string(options.HostConfig.NetworkMode) != "none" {
		t.Errorf("network mode = %q, want none", options.HostConfig.NetworkMode)
	}
	if len(options.HostConfig.ExtraHosts) != 1 || options.HostConfig.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Errorf("ExtraHosts = %v", options.HostConfig.ExtraHosts)
	}
	if options.HostConfig.Memory != 512<<20 {
		t.Errorf("memory = %d, want %d", options.HostConfig.Memory, 512<<20)
	}
	if options.HostConfig.PidsLimit == nil || *options.HostConfig.PidsLimit != 128 {
		t.Errorf("PidsLimit = %v, want 128", options.HostConfig.PidsLimit)
	}
	if len(options.HostConfig.Mounts) != 2 {
		t.Fatalf("volume mounts = %d, want 2", len(options.HostConfig.Mounts))
	}
	if options.HostConfig.Mounts[0].VolumeOptions == nil || options.HostConfig.Mounts[0].VolumeOptions.Subpath != "assignment-1/workspace" {
		t.Errorf("workspace volume options = %+v", options.HostConfig.Mounts[0].VolumeOptions)
	}
	tmpfsOptions := options.HostConfig.Tmpfs["/tmp/opencode"]
	for _, required := range []string{"rw", "nosuid", "nodev", "noexec", "uid=10001", "gid=10001", "mode=0700", "size=67108864"} {
		if !strings.Contains(tmpfsOptions, required) {
			t.Errorf("tmpfs options = %q, missing %q", tmpfsOptions, required)
		}
	}
}

func TestBuildSubpathCreateOptionsUsesAssignmentIdentityAndVolumeRoots(t *testing.T) {
	options, err := buildSubpathCreateOptions(Spec{
		Image:      testImage,
		Platform:   Platform{OS: "linux", Architecture: "arm64"},
		User:       "10001:10001",
		WorkingDir: testWorkspacePath,
		Volumes: []VolumeMount{
			{Name: "omnigrex-workspaces", Subpath: "assignment-1/workspace", Target: testWorkspacePath},
			{Name: "omnigrex-runtime-state", Subpath: "assignment-1/runtime-state", Target: "/home/opencode/.local/share/opencode"},
		},
	})
	if err != nil {
		t.Fatalf("buildSubpathCreateOptions() error = %v", err)
	}
	if options.Config.User != "10001:10001" || options.HostConfig.NetworkMode != "none" {
		t.Errorf("subpath initializer identity/network = user %q network %q", options.Config.User, options.HostConfig.NetworkMode)
	}
	if options.Platform == nil || options.Platform.OS != "linux" || options.Platform.Architecture != "arm64" {
		t.Errorf("subpath initializer platform = %#v, want linux/arm64", options.Platform)
	}
	if !options.HostConfig.ReadonlyRootfs || len(options.HostConfig.CapDrop) != 1 || options.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("subpath initializer hardening = %+v", options.HostConfig)
	}
	if len(options.HostConfig.Mounts) != 2 {
		t.Fatalf("subpath initializer mounts = %d, want 2", len(options.HostConfig.Mounts))
	}
	for _, volume := range options.HostConfig.Mounts {
		if volume.VolumeOptions != nil && volume.VolumeOptions.Subpath != "" {
			t.Errorf("subpath initializer unexpectedly mounts a subpath: %+v", volume)
		}
	}
	command := strings.Join(options.Config.Cmd, " ")
	for _, expected := range []string{"10001 10001", "/volumes/0/assignment-1/workspace", "/volumes/1/assignment-1/runtime-state"} {
		if !strings.Contains(command, expected) {
			t.Errorf("subpath initializer command = %q, missing %q", command, expected)
		}
	}
}

func TestBuildCreateOptionsRejectsUnsafeSpec(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
	}{
		{name: "missing image", spec: Spec{User: "10001:10001", WorkingDir: "/workspace"}},
		{name: "root user", spec: Spec{Image: testImage, Platform: Platform{OS: "linux", Architecture: "arm64"}, User: "0:0", WorkingDir: "/workspace"}},
		{name: "root user alias", spec: Spec{Image: testImage, Platform: Platform{OS: "linux", Architecture: "arm64"}, User: "00:00", WorkingDir: "/workspace"}},
		{name: "relative mount", spec: Spec{
			Image: testImage, Platform: Platform{OS: "linux", Architecture: "arm64"}, User: "10001:10001", WorkingDir: "/workspace",
			Volumes: []VolumeMount{{Name: "volume", Target: "workspace"}},
		}},
		{name: "missing platform", spec: Spec{Image: testImage, User: "10001:10001", WorkingDir: "/workspace"}},
		{name: "unsupported platform", spec: Spec{Image: testImage, Platform: Platform{OS: "linux", Architecture: "s390x"}, User: "10001:10001", WorkingDir: "/workspace"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildCreateOptions(test.spec)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("buildCreateOptions() error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestCompareAPIVersion(t *testing.T) {
	if compareAPIVersion("1.44", minimumDockerAPIVersion) >= 0 {
		t.Error("Docker API 1.44 should be rejected")
	}
	if compareAPIVersion("1.45", minimumDockerAPIVersion) < 0 {
		t.Error("Docker API 1.45 should be accepted")
	}
	if compareAPIVersion("1.51", minimumDockerAPIVersion) < 0 {
		t.Error("Docker API 1.51 should be accepted")
	}
}

func TestValidateAPIVersions(t *testing.T) {
	if err := validateAPIVersions("1.51", "1.51"); err != nil {
		t.Fatalf("validateAPIVersions() error = %v", err)
	}
	for _, versions := range [][2]string{{"", "1.51"}, {"1.44", "1.44"}, {"invalid", "1.51"}} {
		if err := validateAPIVersions(versions[0], versions[1]); err == nil {
			t.Errorf("validateAPIVersions(%q, %q) error = nil", versions[0], versions[1])
		}
	}
}

func TestValidateResourcesRejectsUnconfiguredIsolation(t *testing.T) {
	options := EngineOptions{
		AgentNetwork: "omnigrex-agent",
		RuntimePolicy: RuntimePolicy{
			Platform:   Platform{OS: "linux", Architecture: "arm64"},
			User:       "10001:10001",
			WorkingDir: testWorkspacePath,
			VolumeBindings: map[string]string{
				testWorkspacePath:                      "workspaces",
				"/home/opencode/.local/share/opencode": "runtime-state",
			},
			RequiredVolumeTargets:         []string{testWorkspacePath, "/home/opencode/.local/share/opencode"},
			RequiredWritableVolumeTargets: []string{testWorkspacePath, "/home/opencode/.local/share/opencode"},
			RequireVolumeSubpaths:         true,
			RequiredEnvironment:           map[string]string{"OPENCODE_AUTH_CONTENT": "{}"},
			MaxMemoryBytes:                512 << 20,
			MaxPIDsLimit:                  128,
		},
	}
	base := Spec{
		Platform:    Platform{OS: "linux", Architecture: "arm64"},
		User:        "10001:10001",
		WorkingDir:  testWorkspacePath,
		Network:     "omnigrex-agent",
		Environment: []string{"OPENCODE_AUTH_CONTENT={}"},
		Volumes: []VolumeMount{
			{Name: "workspaces", Subpath: "assignment/workspace", Target: testWorkspacePath},
			{Name: "runtime-state", Subpath: "assignment/runtime-state", Target: "/home/opencode/.local/share/opencode"},
		},
	}
	if err := validateResources(options, base); err != nil {
		t.Fatalf("validateResources() error = %v", err)
	}

	tests := []Spec{
		{Platform: Platform{OS: "linux", Architecture: "amd64"}, User: base.User, WorkingDir: base.WorkingDir, Environment: base.Environment, Network: base.Network, Volumes: base.Volumes},
		{User: base.User, WorkingDir: base.WorkingDir, Environment: base.Environment, Network: "bridge", Volumes: base.Volumes},
		{User: base.User, WorkingDir: base.WorkingDir, Environment: base.Environment, Network: base.Network, Volumes: []VolumeMount{
			{Name: "workspaces", Subpath: "assignment/workspace", Target: testWorkspacePath},
			{Name: "workspaces", Subpath: "assignment/runtime-state", Target: "/home/opencode/.local/share/opencode"},
		}},
		{User: base.User, WorkingDir: base.WorkingDir, Environment: base.Environment, Network: base.Network, Volumes: []VolumeMount{
			{Name: "workspaces", Subpath: "assignment/workspace", Target: testWorkspacePath},
			{Name: "unknown", Subpath: "assignment/runtime-state", Target: "/home/opencode/.local/share/opencode"},
		}},
		{User: base.User, WorkingDir: base.WorkingDir, Environment: base.Environment, Network: base.Network, Volumes: base.Volumes, MemoryBytes: 513 << 20},
		{User: base.User, WorkingDir: base.WorkingDir, Environment: base.Environment, Network: base.Network, Volumes: base.Volumes, PIDsLimit: 129},
	}
	for _, spec := range tests {
		if err := validateResources(options, spec); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("validateResources() error = %v, want ErrInvalidSpec", err)
		}
	}
}

func TestValidateResourcesEnforcesExactProcessPolicy(t *testing.T) {
	wantVolumes := []VolumeMount{{Name: "workspaces", Subpath: "assignment-1/workspace", Target: testWorkspacePath}}
	wantTmpfs := []TmpfsMount{{Target: "/tmp/opencode", SizeBytes: 64 << 20, Executable: true}}
	wantEnvironment := map[string]string{"HOME": "/home/opencode", "OPENCODE_CONFIG_CONTENT": "{}"}
	wantLabels := map[string]string{"io.omnigrex.assignment": "assignment-1"}
	options := EngineOptions{
		AgentNetwork: "omnigrex-agent",
		RuntimePolicy: RuntimePolicy{
			Image:       testImage,
			Platform:    Platform{OS: "linux", Architecture: "arm64"},
			User:        "10001:10001",
			WorkingDir:  testWorkspacePath,
			Command:     []string{"acp"},
			Volumes:     wantVolumes,
			Tmpfs:       wantTmpfs,
			Environment: wantEnvironment,
			Labels:      wantLabels,
			Network:     "omnigrex-agent",
			MemoryBytes: 512 << 20,
			PIDsLimit:   128,
		},
	}
	base := Spec{
		Image:       testImage,
		Platform:    Platform{OS: "linux", Architecture: "arm64"},
		User:        "10001:10001",
		WorkingDir:  testWorkspacePath,
		Command:     []string{"acp"},
		Environment: []string{"HOME=/home/opencode", "OPENCODE_CONFIG_CONTENT={}"},
		Labels:      maps.Clone(wantLabels),
		Volumes:     slices.Clone(wantVolumes),
		Tmpfs:       slices.Clone(wantTmpfs),
		Network:     "omnigrex-agent",
		MemoryBytes: 512 << 20,
		PIDsLimit:   128,
	}
	if err := validateResources(options, base); err != nil {
		t.Fatalf("validateResources() error = %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*Spec)
	}{
		{name: "image", mutate: func(spec *Spec) {
			spec.Image = "registry.example/other@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{name: "command", mutate: func(spec *Spec) { spec.Command = []string{"sh"} }},
		{name: "missing command", mutate: func(spec *Spec) { spec.Command = nil }},
		{name: "volume subpath", mutate: func(spec *Spec) { spec.Volumes[0].Subpath = "assignment-2/workspace" }},
		{name: "read-only volume", mutate: func(spec *Spec) { spec.Volumes[0].ReadOnly = true }},
		{name: "missing volume", mutate: func(spec *Spec) { spec.Volumes = nil }},
		{name: "tmpfs size", mutate: func(spec *Spec) { spec.Tmpfs[0].SizeBytes++ }},
		{name: "tmpfs executable", mutate: func(spec *Spec) { spec.Tmpfs[0].Executable = false }},
		{name: "missing tmpfs", mutate: func(spec *Spec) { spec.Tmpfs = nil }},
		{name: "extra tmpfs", mutate: func(spec *Spec) { spec.Tmpfs = append(spec.Tmpfs, TmpfsMount{Target: "/other", SizeBytes: 1}) }},
		{name: "duplicate same environment", mutate: func(spec *Spec) { spec.Environment = append(spec.Environment, "HOME=/home/opencode") }},
		{name: "duplicate conflicting environment", mutate: func(spec *Spec) { spec.Environment = append(spec.Environment, "HOME=/root") }},
		{name: "missing environment", mutate: func(spec *Spec) { spec.Environment = spec.Environment[1:] }},
		{name: "changed environment", mutate: func(spec *Spec) { spec.Environment[0] = "HOME=/root" }},
		{name: "extra environment", mutate: func(spec *Spec) { spec.Environment = append(spec.Environment, "OTHER=value") }},
		{name: "labels", mutate: func(spec *Spec) { spec.Labels["other"] = "value" }},
		{name: "network", mutate: func(spec *Spec) { spec.Network = "none" }},
		{name: "memory", mutate: func(spec *Spec) { spec.MemoryBytes-- }},
		{name: "PIDs", mutate: func(spec *Spec) { spec.PIDsLimit-- }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			spec := base
			spec.Command = slices.Clone(base.Command)
			spec.Environment = slices.Clone(base.Environment)
			spec.Labels = maps.Clone(base.Labels)
			spec.Volumes = slices.Clone(base.Volumes)
			spec.Tmpfs = slices.Clone(base.Tmpfs)
			test.mutate(&spec)
			if err := validateResources(options, spec); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("validateResources() error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestAsyncWriterDoesNotBlockRuntimeOutput(t *testing.T) {
	target := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	writer := newAsyncWriter(target)
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(target.release) })
		writer.Close()
	}()

	_, _ = writer.Write([]byte("first"))
	select {
	case <-target.started:
	case <-time.After(time.Second):
		t.Fatal("asynchronous writer did not start target write")
	}
	written := make(chan struct{})
	go func() {
		for range 1000 {
			_, _ = writer.Write([]byte("more stderr"))
		}
		close(written)
	}()
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("stderr backpressure blocked the runtime output path")
	}
	releaseOnce.Do(func() { close(target.release) })
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(target.String(), "dropped") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(target.String(), "dropped") {
		t.Fatal("stderr drop was not reported to the diagnostic sink")
	}
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mutex   sync.Mutex
	output  strings.Builder
}

func (writer *blockingWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() {
		close(writer.started)
	})
	<-writer.release
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	_, _ = writer.output.Write(data)
	return len(data), nil
}

func (writer *blockingWriter) String() string {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.output.String()
}
