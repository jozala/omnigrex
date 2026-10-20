//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
	"go.opentelemetry.io/otel/trace"
)

type sdkPlatform struct{ options Options }

var sdkConfiguration struct {
	sync.Mutex
	home, executable, firmware string
}

func newPlatform(options Options) (platform, error) {
	sdkConfiguration.Lock()
	defer sdkConfiguration.Unlock()
	if sdkConfiguration.home != "" && (sdkConfiguration.home != options.Home || sdkConfiguration.executable != options.Executable || sdkConfiguration.firmware != options.Firmware) {
		return nil, execution.ErrBindingMismatch
	}
	if sdkConfiguration.home == "" {
		for name, value := range map[string]string{"MSB_HOME": options.Home, "MSB_PATH": options.Executable, "MSB_LIBKRUNFW_PATH": options.Firmware} {
			if err := os.Setenv(name, value); err != nil {
				return nil, err
			}
		}
		call := sdkCall("ResolveRuntime", metadata{})
		if _, err := sdk.ResolveRuntime(sdk.RuntimeConfig{Home: options.Home, MSBPath: options.Executable, LibkrunfwPath: options.Firmware}); err != nil {
			_ = sdkError(context.Background(), call, err)
			return nil, errors.New("Microsandbox SDK cannot resolve the installed runtime/firmware pair")
		}
		call = sdkCall("DefaultBackendInfo", metadata{})
		info, err := sdk.DefaultBackendInfo()
		_ = sdkError(context.Background(), call, err)
		if err != nil || info.Kind != sdk.BackendLocal {
			return nil, errors.New("Microsandbox SDK must select its local backend")
		}
		sdkConfiguration.home, sdkConfiguration.executable, sdkConfiguration.firmware = options.Home, options.Executable, options.Firmware
	}
	return sdkPlatform{options}, nil
}

func sdkError(ctx context.Context, call sdkCallDiagnostic, err error) error {
	if err == nil {
		return nil
	}
	// SDK source errors can contain guest output or configuration. Do not retain
	// or wrap them in public errors; preserve cancellation separately.
	var failure *sdk.Error
	kind := "Unknown"
	if errors.As(err, &failure) {
		kind = failure.Kind.String()
	}
	logSDKCallFailure(ctx, call, kind, err)
	return errors.Join(ctx.Err(), fmt.Errorf("Microsandbox SDK operation failed (%s)", kind))
}

func sdkLookupError(ctx context.Context, call sdkCallDiagnostic, err error) error {
	if sdk.IsKind(err, sdk.ErrSandboxNotFound) {
		return execution.ErrNotFound
	}
	return sdkError(ctx, call, err)
}

func fromSDK(ctx context.Context, handle *sdk.SandboxHandle) (metadata, error) {
	call := sdkCall("Config", metadata{ID: handle.ID(), Name: handle.Name()})
	config, err := handle.Config()
	if err != nil {
		return metadata{}, errors.Join(execution.ErrConflict, sdkError(ctx, call, err))
	}
	status := strings.ToLower(string(handle.Status()))
	if status != "" {
		status = strings.ToUpper(status[:1]) + status[1:]
	}
	return metadata{ID: handle.ID(), Name: handle.Name(), Status: status, Labels: config.Labels}, nil
}

func (driver sdkPlatform) Lookup(ctx context.Context, name string) (metadata, error) {
	call := sdkCall("GetSandbox", metadata{Name: name})
	handle, err := sdk.GetSandbox(ctx, name)
	if err != nil {
		return metadata{}, sdkLookupError(ctx, call, err)
	}
	return fromSDK(ctx, handle)
}
func (driver sdkPlatform) exact(ctx context.Context, meta metadata) (*sdk.SandboxHandle, error) {
	call := sdkCall("GetSandbox", meta)
	handle, err := sdk.GetSandbox(ctx, meta.Name)
	if err != nil {
		return nil, sdkError(ctx, call, err)
	}
	if handle.ID() != meta.ID {
		return nil, execution.ErrConflict
	}
	return handle, nil
}
func (driver sdkPlatform) live(ctx context.Context, meta metadata) (*sdk.Sandbox, error) {
	handle, err := driver.exact(ctx, meta)
	if err != nil {
		return nil, err
	}
	call := sdkCall("Connect", meta)
	sandbox, err := handle.Connect(ctx)
	return sandbox, sdkError(ctx, call, err)
}

func (driver sdkPlatform) Create(ctx context.Context, definition creation) (metadata, error) {
	request := definition.Request
	os, architecture, err := driver.Image(ctx, request.Image)
	if err != nil {
		return metadata{}, errors.Join(errNoCreationEffects, err)
	}
	expectedPlatform := request.Platform
	// Existing bounded diagnostics use the backend's qualified platform and
	// do not populate a Turn Request platform.
	if definition.Diagnostic && expectedPlatform == (execution.Platform{}) {
		expectedPlatform = execution.Platform{OS: "linux", Architecture: "amd64"}
	}
	if os != expectedPlatform.OS || architecture != expectedPlatform.Architecture {
		return metadata{}, errors.Join(errNoCreationEffects, execution.ErrBindingMismatch)
	}
	volumes := map[string]sdk.MountConfig{"/run/omnigrex": sdk.Mount.Tmpfs(sdk.TmpfsOptions{SizeMiB: 16})}
	for target, source := range definition.Mounts {
		volumes[target] = sdk.Mount.Bind(source, sdk.MountOptions{StatVirtualization: sdk.StatVirtualizationOff})
	}
	for _, directory := range request.TemporaryDirectories {
		volumes[directory.Target] = sdk.Mount.Tmpfs(sdk.TmpfsOptions{SizeMiB: uint32(directory.SizeBytes >> 20), Noexec: !directory.Executable, Nosuid: true, Nodev: true})
	}
	network := sdk.NetworkPolicy.FromProfiles(sdk.NetworkProfilePublic)
	if definition.Diagnostic {
		network = sdk.NetworkPolicy.None()
		if definition.DiagnosticNetwork {
			network = sdk.NetworkPolicy.FromProfiles(sdk.NetworkProfilePrivate, sdk.NetworkProfileHost)
		}
	}
	if !definition.Diagnostic && driver.options.MCPEndpoint != "" {
		rule, err := mcpRule(driver.options.MCPEndpoint)
		if err != nil {
			return metadata{}, errors.Join(errNoCreationEffects, err)
		}
		network.Rules = append([]sdk.PolicyRule{{Action: sdk.PolicyActionAllow, Direction: sdk.PolicyDirectionEgress, Destination: rule.host, Protocol: sdk.PolicyProtocolTCP, Port: rule.port}}, network.Rules...)
		disabled := false
		network.DNS = &sdk.DNSConfig{RebindProtection: &disabled}
	}
	maximumDuration := driver.options.MaximumDuration
	if definition.Diagnostic {
		maximumDuration = 5 * time.Minute
	}
	rootDisk := sdk.RootDisk.Managed(uint32(driver.options.RootDiskMiB))
	pullPolicy := sdk.PullPolicyNever
	if slices.Contains(request.Requirements, execution.NestedContainers) {
		rootDisk = sdk.RootDisk.Flat(sdk.RootDiskFlatOptions{SizeMiB: uint32(driver.options.RootDiskMiB)})
		pullPolicy = sdk.PullPolicyIfMissing
	}
	meta := metadata{Name: definition.Name, Labels: definition.Labels}
	call := sdkCall("CreateSandbox", meta)
	sandbox, err := sdk.CreateSandbox(ctx, definition.Name, sdk.WithImage(request.Image), sdk.WithMemory(uint32(request.MemoryBytes>>20)), sdk.WithCPUs(uint8(driver.options.CPUs)), sdk.WithRootDisk(rootDisk), sdk.WithUser(request.User), sdk.WithWorkdir(request.WorkingDir), sdk.WithEntrypoint("/bin/sh"), sdk.WithCmd(), sdk.WithMounts(volumes), sdk.WithLabels(definition.Labels), sdk.WithNetwork(network), sdk.WithPullPolicy(pullPolicy), sdk.WithMaxDuration(maximumDuration), sdk.WithDetached(), sdk.WithQuietLogs())
	if err != nil {
		return metadata{}, sdkError(ctx, call, err)
	}
	// Release ownership without stopping the VM. Durable inventory, not a Go
	// handle lifetime, owns its recovery and termination.
	if err := boundedNativeRelease(ctx, nativeReleaseWait, sdkRelease("Detach", meta, sandbox.Detach)); err != nil {
		return metadata{}, err
	}
	return driver.Lookup(ctx, definition.Name)
}

func (driver sdkPlatform) List(ctx context.Context, cursor string) ([]metadata, string, error) {
	options := []sdk.SandboxListOption{sdk.WithListLabels(map[string]string{namespaceLabel: driver.options.Namespace}), sdk.WithListLimit(100)}
	if cursor != "" {
		options = append(options, sdk.WithListCursor(cursor))
	}
	call := sdkCall("ListSandboxes", metadata{})
	page, err := sdk.ListSandboxesWith(ctx, options...)
	if err != nil {
		return nil, "", sdkError(ctx, call, err)
	}
	var entries []metadata
	for _, handle := range page.Sandboxes {
		meta, err := fromSDK(ctx, handle)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, meta)
	}
	next := ""
	if page.NextCursor != nil {
		next = *page.NextCursor
	}
	return entries, next, nil
}

func (driver sdkPlatform) Image(ctx context.Context, reference string) (string, string, error) {
	if !profile.IsExactRegistryImage(reference) {
		return "", "", execution.ErrInvalidSpec
	}
	call := sdkCall("Image.Get", metadata{})
	image, err := sdk.Image.Get(ctx, reference)
	if err != nil {
		return "", "", sdkError(ctx, call, err)
	}
	if image.Reference() != reference {
		return "", "", execution.ErrBindingMismatch
	}
	if err := verifyCachedImage(ctx, driver.options.Home, reference, image.ManifestDigest(), image.OS(), image.Architecture()); err != nil {
		return "", "", err
	}
	return image.OS(), image.Architecture(), nil
}

func (driver sdkPlatform) Execute(ctx context.Context, meta metadata, command []string, user string, input io.Reader) (result error) {
	sandbox, err := driver.live(ctx, meta)
	if err != nil {
		return err
	}
	call := sdkCall("ExecStream", meta)
	handle, err := sandbox.ExecStream(ctx, command[0], command[1:], sdk.WithExecUser(user), sdk.WithExecStdinPipe())
	if err != nil {
		return errors.Join(sdkError(ctx, call, err), boundedNativeRelease(ctx, nativeReleaseWait, sdkRelease("Detach", meta, sandbox.Detach)))
	}
	defer func() {
		result = errors.Join(result, boundedNativeRelease(ctx, nativeReleaseWait, sdkRelease("Exec.Close", meta, func(context.Context) error { return handle.Close() }), sdkRelease("Detach", meta, sandbox.Detach)))
	}()
	stdin := handle.TakeStdin()
	if stdin == nil {
		return execution.ErrUnknownEffect
	}
	if input != nil {
		content, err := io.ReadAll(io.LimitReader(input, 1<<20))
		if err != nil || len(content) >= 1<<20 {
			return execution.ErrInvalidSpec
		}
		call := sdkCall("Stdin.Write", meta)
		if _, err := stdin.WriteCtx(ctx, content); err != nil {
			return sdkError(ctx, call, err)
		}
	}
	if err := boundedNativeRelease(ctx, nativeReleaseWait, sdkRelease("Stdin.Close", meta, func(context.Context) error { return stdin.Close() })); err != nil {
		return err
	}
	for {
		call := sdkCall("Exec.Recv", meta)
		event, err := handle.Recv(ctx)
		if err != nil {
			return sdkError(ctx, call, err)
		}
		if event.Kind == sdk.ExecEventFailed || event.Kind == sdk.ExecEventStdinError {
			return execution.ErrUnknownEffect
		}
		if event.Kind == sdk.ExecEventExited {
			if event.ExitCode != 0 {
				return errors.New("Microsandbox guest setup failed")
			}
			return nil
		}
		if event.Kind == sdk.ExecEventDone {
			return execution.ErrUnknownEffect
		}
	}
}

func (driver sdkPlatform) Stream(ctx context.Context, meta metadata, command []string, user string) (stream, error) {
	sandbox, err := driver.live(ctx, meta)
	if err != nil {
		return nil, err
	}
	call := sdkCall("ExecStream", meta)
	handle, err := sandbox.ExecStream(ctx, command[0], command[1:], sdk.WithExecUser(user), sdk.WithExecStdinPipe())
	if err != nil {
		return nil, errors.Join(sdkError(ctx, call, err), boundedNativeRelease(ctx, nativeReleaseWait, sdkRelease("Detach", meta, sandbox.Detach)))
	}
	return &sdkStream{handle: handle, stdin: handle.TakeStdin(), sandbox: sandbox, resource: meta, ctx: context.WithoutCancel(ctx)}, nil
}

func (driver sdkPlatform) Pause(ctx context.Context, meta metadata) error {
	handle, err := driver.exact(ctx, meta)
	if err != nil {
		return err
	}
	call := sdkCall("PauseWithGuestFlush", meta)
	return sdkError(ctx, call, handle.PauseWithGuestFlush(ctx, sdk.GuestFlushRequired))
}
func (driver sdkPlatform) Resume(ctx context.Context, meta metadata) error {
	handle, err := driver.exact(ctx, meta)
	if err != nil {
		return err
	}
	call := sdkCall("Resume", meta)
	return sdkError(ctx, call, handle.Resume(ctx))
}
func (driver sdkPlatform) Stop(ctx context.Context, meta metadata) error {
	handle, err := driver.exact(ctx, meta)
	if err != nil {
		return err
	}
	if terminalEnvironmentStatus(string(handle.Status())) {
		return nil
	}
	call := sdkCall("Kill", meta)
	return sdkError(ctx, call, handle.Kill(ctx, sdk.WithKillTimeout(5*time.Second)))
}
func (driver sdkPlatform) Remove(ctx context.Context, meta metadata) error {
	handle, err := driver.exact(ctx, meta)
	if err != nil {
		return err
	}
	call := sdkCall("Remove", meta)
	return sdkError(ctx, call, handle.Remove(ctx))
}

type sdkStream struct {
	resource metadata
	ctx      context.Context
	handle   interface {
		Recv(context.Context) (*sdk.ExecEvent, error)
		Close() error
	}
	stdin   *sdk.ExecSink
	sandbox interface {
		Detach(context.Context) error
	}
	closeOnce sync.Once
	closeErr  error
}

func (stream *sdkStream) Next(ctx context.Context) (event, error) {
	ctx = stream.diagnosticContext(ctx)
	call := sdkCall("Exec.Recv", stream.resource)
	record, err := stream.handle.Recv(ctx)
	if err != nil {
		return event{}, sdkError(ctx, call, err)
	}
	result := event{Data: record.Data, ExitCode: record.ExitCode}
	switch record.Kind {
	case sdk.ExecEventStarted:
		result.Kind = "started"
	case sdk.ExecEventStdout:
		result.Kind = "stdout"
	case sdk.ExecEventStderr:
		result.Kind = "stderr"
	case sdk.ExecEventExited:
		result.Kind = "exited"
	case sdk.ExecEventDone:
		result.Kind = "done"
	case sdk.ExecEventFailed, sdk.ExecEventStdinError:
		result.Kind = "failed"
	}
	return result, nil
}
func (stream *sdkStream) Write(ctx context.Context, data []byte) (int, error) {
	ctx = stream.diagnosticContext(ctx)
	if stream.stdin == nil {
		return 0, io.ErrClosedPipe
	}
	call := sdkCall("Stdin.Write", stream.resource)
	n, err := stream.stdin.WriteCtx(ctx, data)
	return n, sdkError(ctx, call, err)
}
func (stream *sdkStream) Close() error {
	stream.closeOnce.Do(func() {
		ctx := stream.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		stream.closeErr = boundedNativeRelease(ctx, nativeReleaseWait, sdkRelease("Exec.Close", stream.resource, func(context.Context) error { return stream.handle.Close() }), sdkRelease("Detach", stream.resource, stream.sandbox.Detach))
	})
	return stream.closeErr
}

func (stream *sdkStream) diagnosticContext(ctx context.Context) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() && stream.ctx != nil {
		return trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(stream.ctx))
	}
	return ctx
}

// sdkRelease reports raw errors before boundedNativeRelease sanitizes them.
// A stuck destructor gets an attributable timeout, not a fabricated completion.
func sdkRelease(operation string, resource metadata, release func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		call := sdkCall(operation, resource)
		var once sync.Once
		report := func(err error) { once.Do(func() { _ = sdkError(ctx, call, err) }) }
		stop := context.AfterFunc(ctx, func() { report(ctx.Err()) })
		defer stop()
		err := release(ctx)
		report(err)
		return err
	}
}
