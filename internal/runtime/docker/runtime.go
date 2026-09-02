package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"
)

const (
	minimumDockerAPIVersion   = "1.45"
	defaultRuntimeMemoryBytes = 512 << 20
	defaultRuntimePIDsLimit   = 128
	subpathHelperMemoryBytes  = 128 << 20
	subpathHelperPIDsLimit    = 32
)

var ErrInvalidSpec = errors.New("invalid Runtime Process specification")

type Spec struct {
	Name        string
	Image       string
	User        string
	WorkingDir  string
	Command     []string
	Environment []string
	Labels      map[string]string
	Volumes     []VolumeMount
	Tmpfs       []TmpfsMount
	Network     string
	ExtraHosts  []string
	MemoryBytes int64
	PIDsLimit   int64
}

type VolumeMount struct {
	Name     string
	Subpath  string
	Target   string
	ReadOnly bool
}

type TmpfsMount struct {
	Target     string
	SizeBytes  int64
	Executable bool
}

type dockerAPI interface {
	ContainerCreate(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error)
	ContainerAttach(context.Context, string, mobyclient.ContainerAttachOptions) (mobyclient.ContainerAttachResult, error)
	ContainerStart(context.Context, string, mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error)
	ContainerStop(context.Context, string, mobyclient.ContainerStopOptions) (mobyclient.ContainerStopResult, error)
	ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult
	ContainerRemove(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error)
}

type Engine struct {
	client       *mobyclient.Client
	options      EngineOptions
	validateLock sync.Mutex
}

type EngineOptions struct {
	AgentNetwork     string
	AllowHostGateway bool
	RuntimePolicy    RuntimePolicy
}

type RuntimePolicy struct {
	User                          string
	WorkingDir                    string
	VolumeBindings                map[string]string
	RequiredVolumeTargets         []string
	RequiredWritableVolumeTargets []string
	AllowedTmpfsTargets           []string
	RequireVolumeSubpaths         bool
	RequiredEnvironment           map[string]string
	MaxTmpfsBytes                 int64
	MaxMemoryBytes                int64
	MaxPIDsLimit                  int64
}

func NewEngine(options EngineOptions) (*Engine, error) {
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Engine{client: client, options: cloneEngineOptions(options)}, nil
}

func cloneEngineOptions(options EngineOptions) EngineOptions {
	options.RuntimePolicy.VolumeBindings = maps.Clone(options.RuntimePolicy.VolumeBindings)
	options.RuntimePolicy.RequiredVolumeTargets = slices.Clone(options.RuntimePolicy.RequiredVolumeTargets)
	options.RuntimePolicy.RequiredWritableVolumeTargets = slices.Clone(options.RuntimePolicy.RequiredWritableVolumeTargets)
	options.RuntimePolicy.AllowedTmpfsTargets = slices.Clone(options.RuntimePolicy.AllowedTmpfsTargets)
	options.RuntimePolicy.RequiredEnvironment = maps.Clone(options.RuntimePolicy.RequiredEnvironment)
	return options
}

func (engine *Engine) Close() error {
	return engine.client.Close()
}

func (engine *Engine) Start(ctx context.Context, spec Spec, stderr io.Writer) (*Process, error) {
	if _, _, err := validateSpec(spec); err != nil {
		return nil, err
	}
	if err := validateResources(engine.options, spec); err != nil {
		return nil, err
	}
	if err := engine.validateAPI(ctx); err != nil {
		return nil, err
	}
	if err := prepareVolumeSubpaths(ctx, engine.client, spec); err != nil {
		return nil, err
	}
	return start(ctx, engine.client, spec, stderr)
}

func (engine *Engine) validateAPI(ctx context.Context) error {
	engine.validateLock.Lock()
	defer engine.validateLock.Unlock()
	ping, err := engine.client.Ping(ctx, mobyclient.PingOptions{
		NegotiateAPIVersion: true,
		ForceNegotiate:      true,
	})
	if err != nil {
		return fmt.Errorf("negotiate Docker API version: %w", err)
	}
	if err := validateAPIVersions(ping.APIVersion, engine.client.ClientVersion()); err != nil {
		return err
	}
	return nil
}

type Process struct {
	ID         string
	api        dockerAPI
	transport  *attachTransport
	demuxDone  chan struct{}
	demuxLock  sync.Mutex
	demuxErr   error
	removeLock sync.Mutex
	removed    bool
}

func (process *Process) Transport() io.ReadWriteCloser {
	return process.transport
}

func (process *Process) Wait(ctx context.Context) (int64, error) {
	wait := process.api.ContainerWait(ctx, process.ID, mobyclient.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	var status int64
	select {
	case response := <-wait.Result:
		status = response.StatusCode
		if response.Error != nil {
			return status, fmt.Errorf("wait for Runtime Process: %s", response.Error.Message)
		}
	case err := <-wait.Error:
		return 0, fmt.Errorf("wait for Runtime Process: %w", err)
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	select {
	case <-process.demuxDone:
		process.demuxLock.Lock()
		err := process.demuxErr
		process.demuxLock.Unlock()
		if err != nil && !process.transport.closed.Load() {
			return status, fmt.Errorf("demultiplex Runtime Process output: %w", err)
		}
	case <-ctx.Done():
		return status, ctx.Err()
	}
	if status != 0 {
		return status, fmt.Errorf("Runtime Process exited with status %d", status)
	}
	return status, nil
}

func (process *Process) Stop(ctx context.Context, timeout time.Duration) error {
	seconds := int(timeout.Round(time.Second) / time.Second)
	if timeout > 0 && seconds == 0 {
		seconds = 1
	}
	_, err := process.api.ContainerStop(ctx, process.ID, mobyclient.ContainerStopOptions{Timeout: &seconds})
	if err != nil {
		return fmt.Errorf("stop Runtime Process: %w", err)
	}
	return nil
}

func (process *Process) Remove(ctx context.Context) error {
	process.removeLock.Lock()
	defer process.removeLock.Unlock()
	if process.removed {
		return nil
	}
	_ = process.transport.Close()
	if _, err := process.api.ContainerRemove(ctx, process.ID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove Runtime Process: %w", err)
	}
	process.removed = true
	return nil
}

func start(ctx context.Context, api dockerAPI, spec Spec, stderr io.Writer) (*Process, error) {
	options, err := buildCreateOptions(spec)
	if err != nil {
		return nil, err
	}
	created, err := api.ContainerCreate(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("create Runtime Process: %w", err)
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = api.ContainerRemove(cleanupCtx, created.ID, mobyclient.ContainerRemoveOptions{Force: true})
	}

	attached, err := api.ContainerAttach(ctx, created.ID, mobyclient.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("attach Runtime Process: %w", err)
	}
	stdoutReader, stdoutWriter := io.Pipe()
	if stderr == nil {
		stderr = io.Discard
	}
	stderrSink := newAsyncWriter(stderr)
	demuxDone := make(chan struct{})
	process := &Process{
		ID:        created.ID,
		api:       api,
		transport: &attachTransport{reader: stdoutReader, attach: &attached.HijackedResponse},
		demuxDone: demuxDone,
	}
	go func() {
		_, copyErr := stdcopy.StdCopy(stdoutWriter, stderrSink, attached.Reader)
		_ = stdoutWriter.CloseWithError(copyErr)
		stderrSink.Close()
		process.demuxLock.Lock()
		process.demuxErr = copyErr
		process.demuxLock.Unlock()
		close(demuxDone)
	}()

	if _, err := api.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err != nil {
		attached.Close()
		_ = stdoutReader.Close()
		stderrSink.Close()
		cleanup()
		return nil, fmt.Errorf("start Runtime Process: %w", err)
	}

	return process, nil
}

func prepareVolumeSubpaths(ctx context.Context, api dockerAPI, spec Spec) error {
	options, err := buildSubpathCreateOptions(spec)
	if err != nil || options.Config == nil {
		return err
	}
	if err := runOneShotContainer(ctx, api, options, "assignment subpath initializer"); err != nil {
		return fmt.Errorf("initialize assignment volume subpaths: %w", err)
	}
	return nil
}

func buildSubpathCreateOptions(spec Spec) (mobyclient.ContainerCreateOptions, error) {
	uid, gid, err := validateSpec(spec)
	if err != nil {
		return mobyclient.ContainerCreateOptions{}, err
	}
	mounts := make([]mount.Mount, 0, len(spec.Volumes))
	paths := make([]string, 0, len(spec.Volumes)+1)
	paths = append(paths, "sh")
	for _, volume := range spec.Volumes {
		if volume.Subpath == "" {
			continue
		}
		root := "/volumes/" + strconv.Itoa(len(mounts))
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: volume.Name, Target: root})
		paths = append(paths, root+"/"+volume.Subpath)
	}
	if len(mounts) == 0 {
		return mobyclient.ContainerCreateOptions{}, nil
	}
	pids := int64(subpathHelperPIDsLimit)
	script := `set -eu
uid="$1"
gid="$2"
shift 2
for path do
  mkdir -p "$path"
  test "$(stat -c %u "$path")" = "$uid"
  test "$(stat -c %g "$path")" = "$gid"
done`
	return mobyclient.ContainerCreateOptions{
		Config: &container.Config{
			Image:      spec.Image,
			User:       spec.User,
			Entrypoint: []string{"sh", "-c"},
			Cmd:        append([]string{script, paths[0], uid, gid}, paths[1:]...),
			Labels:     map[string]string{"io.omnigrex.assignment-subpath-initializer": "true"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Mounts:         mounts,
			Resources: container.Resources{
				Memory:    subpathHelperMemoryBytes,
				PidsLimit: &pids,
			},
		},
	}, nil
}

func buildCreateOptions(spec Spec) (mobyclient.ContainerCreateOptions, error) {
	uid, gid, err := validateSpec(spec)
	if err != nil {
		return mobyclient.ContainerCreateOptions{}, err
	}
	if spec.Network == "" {
		spec.Network = "none"
	}
	if spec.MemoryBytes <= 0 {
		spec.MemoryBytes = defaultRuntimeMemoryBytes
	}
	if spec.PIDsLimit <= 0 {
		spec.PIDsLimit = defaultRuntimePIDsLimit
	}
	initProcess := true
	stopTimeout := 10
	pidsLimit := spec.PIDsLimit

	mounts := make([]mount.Mount, 0, len(spec.Volumes))
	for _, volume := range spec.Volumes {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   volume.Name,
			Target:   volume.Target,
			ReadOnly: volume.ReadOnly,
			VolumeOptions: &mount.VolumeOptions{
				Subpath: volume.Subpath,
			},
		})
	}
	tmpfs := make(map[string]string, len(spec.Tmpfs))
	for _, temporary := range spec.Tmpfs {
		options := []string{
			"rw",
			"nosuid",
			"nodev",
			"uid=" + uid,
			"gid=" + gid,
			"mode=0700",
		}
		if !temporary.Executable {
			options = append(options, "noexec")
		}
		if temporary.SizeBytes > 0 {
			options = append(options, "size="+strconv.FormatInt(temporary.SizeBytes, 10))
		}
		tmpfs[temporary.Target] = strings.Join(options, ",")
	}

	return mobyclient.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			User:         spec.User,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          false,
			OpenStdin:    true,
			StdinOnce:    true,
			Env:          spec.Environment,
			Cmd:          spec.Command,
			Image:        spec.Image,
			WorkingDir:   spec.WorkingDir,
			Labels:       spec.Labels,
			StopTimeout:  &stopTimeout,
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(spec.Network),
			CapDrop:        []string{"ALL"},
			ExtraHosts:     spec.ExtraHosts,
			ReadonlyRootfs: true,
			SecurityOpt:    []string{"no-new-privileges"},
			Tmpfs:          tmpfs,
			Resources: container.Resources{
				Memory:    spec.MemoryBytes,
				PidsLimit: &pidsLimit,
			},
			Mounts: mounts,
			Init:   &initProcess,
		},
	}, nil
}

func validateSpec(spec Spec) (string, string, error) {
	if !validImageDigest(spec.Image) {
		return "", "", fmt.Errorf("%w: image must be digest-qualified", ErrInvalidSpec)
	}
	if !filepath.IsAbs(spec.WorkingDir) || filepath.Clean(spec.WorkingDir) != spec.WorkingDir {
		return "", "", fmt.Errorf("%w: working directory must be an absolute clean path", ErrInvalidSpec)
	}
	uid, gid, found := strings.Cut(spec.User, ":")
	if !found {
		return "", "", fmt.Errorf("%w: user must contain a numeric UID and GID", ErrInvalidSpec)
	}
	uidNumber, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || uidNumber == 0 || strconv.FormatUint(uidNumber, 10) != uid {
		return "", "", fmt.Errorf("%w: UID must be a canonical non-zero integer", ErrInvalidSpec)
	}
	gidNumber, err := strconv.ParseUint(gid, 10, 32)
	if err != nil || gidNumber == 0 || strconv.FormatUint(gidNumber, 10) != gid {
		return "", "", fmt.Errorf("%w: GID must be a canonical non-zero integer", ErrInvalidSpec)
	}
	targets := make([]string, 0, len(spec.Volumes)+len(spec.Tmpfs))
	for _, volume := range spec.Volumes {
		if volume.Name == "" || !filepath.IsAbs(volume.Target) || filepath.Clean(volume.Target) != volume.Target {
			return "", "", fmt.Errorf("%w: invalid volume mount", ErrInvalidSpec)
		}
		if volume.Subpath != "" && (filepath.IsAbs(volume.Subpath) || filepath.Clean(volume.Subpath) != volume.Subpath || strings.HasPrefix(volume.Subpath, "..")) {
			return "", "", fmt.Errorf("%w: invalid volume subpath %q", ErrInvalidSpec, volume.Subpath)
		}
		targets = append(targets, volume.Target)
	}
	for _, temporary := range spec.Tmpfs {
		if !filepath.IsAbs(temporary.Target) || filepath.Clean(temporary.Target) != temporary.Target {
			return "", "", fmt.Errorf("%w: invalid tmpfs mount", ErrInvalidSpec)
		}
		if temporary.SizeBytes <= 0 {
			return "", "", fmt.Errorf("%w: tmpfs size must be positive", ErrInvalidSpec)
		}
		targets = append(targets, temporary.Target)
	}
	for index, target := range targets {
		for otherIndex := index + 1; otherIndex < len(targets); otherIndex++ {
			other := targets[otherIndex]
			if target == other || strings.HasPrefix(target, other+"/") || strings.HasPrefix(other, target+"/") {
				return "", "", fmt.Errorf("%w: overlapping writable targets %q and %q", ErrInvalidSpec, target, other)
			}
		}
	}
	if spec.Network == "host" || spec.Network == "default" || strings.HasPrefix(spec.Network, "container:") {
		return "", "", fmt.Errorf("%w: unsafe network mode %q", ErrInvalidSpec, spec.Network)
	}
	for _, host := range spec.ExtraHosts {
		if host != "host.docker.internal:host-gateway" {
			return "", "", fmt.Errorf("%w: unsafe extra host %q", ErrInvalidSpec, host)
		}
	}
	return uid, gid, nil
}

func validateResources(options EngineOptions, spec Spec) error {
	policy := options.RuntimePolicy
	if spec.User != policy.User {
		return fmt.Errorf("%w: user %q does not match the Runtime Policy", ErrInvalidSpec, spec.User)
	}
	if spec.WorkingDir != policy.WorkingDir {
		return fmt.Errorf("%w: working directory %q does not match the Runtime Policy", ErrInvalidSpec, spec.WorkingDir)
	}
	network := spec.Network
	if network == "" {
		network = "none"
	}
	if network != "none" && network != options.AgentNetwork {
		return fmt.Errorf("%w: network %q is not the configured agent network", ErrInvalidSpec, network)
	}
	volumeTargets := make(map[string]VolumeMount, len(spec.Volumes))
	for _, volume := range spec.Volumes {
		expectedSource, allowed := policy.VolumeBindings[volume.Target]
		if !allowed || volume.Name != expectedSource {
			return fmt.Errorf("%w: volume %q is not allowed at %q", ErrInvalidSpec, volume.Name, volume.Target)
		}
		if policy.RequireVolumeSubpaths && volume.Subpath == "" {
			return fmt.Errorf("%w: assignment subpath is required for %q", ErrInvalidSpec, volume.Target)
		}
		volumeTargets[volume.Target] = volume
	}
	for index, volume := range spec.Volumes {
		for otherIndex := index + 1; otherIndex < len(spec.Volumes); otherIndex++ {
			other := spec.Volumes[otherIndex]
			if volume.Name == other.Name && (volume.Subpath == other.Subpath || strings.HasPrefix(volume.Subpath, other.Subpath+"/") || strings.HasPrefix(other.Subpath, volume.Subpath+"/")) {
				return fmt.Errorf("%w: overlapping subpaths in volume %q", ErrInvalidSpec, volume.Name)
			}
		}
	}
	for _, target := range policy.RequiredVolumeTargets {
		if _, present := volumeTargets[target]; !present {
			return fmt.Errorf("%w: required volume target %q is missing", ErrInvalidSpec, target)
		}
	}
	for _, target := range policy.RequiredWritableVolumeTargets {
		volume, present := volumeTargets[target]
		if !present || volume.ReadOnly {
			return fmt.Errorf("%w: volume target %q must be present and writable", ErrInvalidSpec, target)
		}
	}
	allowedTmpfs := make(map[string]struct{}, len(policy.AllowedTmpfsTargets))
	for _, target := range policy.AllowedTmpfsTargets {
		allowedTmpfs[target] = struct{}{}
	}
	for _, temporary := range spec.Tmpfs {
		if _, allowed := allowedTmpfs[temporary.Target]; !allowed {
			return fmt.Errorf("%w: tmpfs target %q is not allowed", ErrInvalidSpec, temporary.Target)
		}
		if policy.MaxTmpfsBytes > 0 && temporary.SizeBytes > policy.MaxTmpfsBytes {
			return fmt.Errorf("%w: tmpfs at %q exceeds the Runtime Policy limit", ErrInvalidSpec, temporary.Target)
		}
	}
	if len(spec.ExtraHosts) > 0 && !options.AllowHostGateway {
		return fmt.Errorf("%w: host gateway is not allowed", ErrInvalidSpec)
	}
	environment := make(map[string]string, len(spec.Environment))
	for _, entry := range spec.Environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			return fmt.Errorf("%w: invalid environment entry", ErrInvalidSpec)
		}
		environment[name] = value
	}
	for name, requiredValue := range policy.RequiredEnvironment {
		if environment[name] != requiredValue {
			return fmt.Errorf("%w: required environment %s is missing or invalid", ErrInvalidSpec, name)
		}
	}
	memoryBytes := spec.MemoryBytes
	if memoryBytes <= 0 {
		memoryBytes = defaultRuntimeMemoryBytes
	}
	if policy.MaxMemoryBytes > 0 && memoryBytes > policy.MaxMemoryBytes {
		return fmt.Errorf("%w: memory limit exceeds the Runtime Policy maximum", ErrInvalidSpec)
	}
	pidsLimit := spec.PIDsLimit
	if pidsLimit <= 0 {
		pidsLimit = defaultRuntimePIDsLimit
	}
	if policy.MaxPIDsLimit > 0 && pidsLimit > policy.MaxPIDsLimit {
		return fmt.Errorf("%w: PID limit exceeds the Runtime Policy maximum", ErrInvalidSpec)
	}
	return nil
}

func validImageDigest(image string) bool {
	digest := image
	if _, value, found := strings.Cut(image, "@sha256:"); found {
		digest = value
	} else if strings.HasPrefix(image, "sha256:") {
		digest = strings.TrimPrefix(image, "sha256:")
	} else {
		return false
	}
	if len(digest) != 64 {
		return false
	}
	_, err := strconv.ParseUint(digest[:16], 16, 64)
	if err != nil {
		return false
	}
	for _, character := range digest[16:] {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func compareAPIVersion(left, right string) int {
	parse := func(version string) (int, int) {
		majorText, minorText, _ := strings.Cut(strings.TrimPrefix(version, "v"), ".")
		major, _ := strconv.Atoi(majorText)
		minor, _ := strconv.Atoi(minorText)
		return major, minor
	}
	leftMajor, leftMinor := parse(left)
	rightMajor, rightMinor := parse(right)
	if leftMajor != rightMajor {
		return leftMajor - rightMajor
	}
	return leftMinor - rightMinor
}

func validateAPIVersions(advertised, effective string) error {
	if !validAPIVersion(advertised) {
		return errors.New("Docker Engine did not advertise a valid API version")
	}
	if !validAPIVersion(effective) {
		return errors.New("Docker client negotiated an invalid API version")
	}
	if compareAPIVersion(advertised, minimumDockerAPIVersion) < 0 || compareAPIVersion(effective, minimumDockerAPIVersion) < 0 {
		return fmt.Errorf("Docker API %s is unsupported; require %s or newer", effective, minimumDockerAPIVersion)
	}
	return nil
}

func validAPIVersion(version string) bool {
	major, minor, found := strings.Cut(strings.TrimPrefix(version, "v"), ".")
	if !found || major == "" || minor == "" {
		return false
	}
	if _, err := strconv.ParseUint(major, 10, 16); err != nil {
		return false
	}
	if _, err := strconv.ParseUint(minor, 10, 16); err != nil {
		return false
	}
	return true
}

type attachTransport struct {
	reader    *io.PipeReader
	attach    *mobyclient.HijackedResponse
	writeLock sync.Mutex
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

type asyncWriter struct {
	target    io.Writer
	writes    chan []byte
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
}

func newAsyncWriter(target io.Writer) *asyncWriter {
	writer := &asyncWriter{
		target: target,
		writes: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (writer *asyncWriter) Write(data []byte) (int, error) {
	copyOfData := append([]byte(nil), data...)
	select {
	case writer.writes <- copyOfData:
	case <-writer.done:
	default:
		writer.dropped.Add(1)
	}
	return len(data), nil
}

func (writer *asyncWriter) Close() {
	writer.closeOnce.Do(func() {
		close(writer.done)
	})
}

func (writer *asyncWriter) run() {
	for {
		select {
		case data := <-writer.writes:
			if dropped := writer.dropped.Swap(0); dropped > 0 {
				_, _ = fmt.Fprintf(writer.target, "[omnigrex: dropped %d stderr chunks]\n", dropped)
			}
			_, _ = writer.target.Write(data)
		case <-writer.done:
			return
		}
	}
}

func (transport *attachTransport) Read(data []byte) (int, error) {
	return transport.reader.Read(data)
}

func (transport *attachTransport) Write(data []byte) (int, error) {
	transport.writeLock.Lock()
	defer transport.writeLock.Unlock()
	return transport.attach.Conn.Write(data)
}

func (transport *attachTransport) SetWriteDeadline(deadline time.Time) error {
	return transport.attach.Conn.SetWriteDeadline(deadline)
}

func (transport *attachTransport) Close() error {
	transport.closeOnce.Do(func() {
		transport.closed.Store(true)
		_ = transport.reader.Close()
		transport.closeErr = transport.attach.Conn.Close()
	})
	return transport.closeErr
}
