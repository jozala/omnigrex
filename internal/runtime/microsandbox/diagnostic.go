package microsandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const diagnosticLabel = "io.omnigrex.diagnostic"
const expiryLabel = "io.omnigrex.diagnostic.expiry"

func (backend *Backend) Inspect(ctx context.Context) error { return backend.Check(ctx) }

type diagnosticProcess struct {
	backend   *Backend
	meta      metadata
	transport *transport
}

func (backend *Backend) diagnostic(ctx context.Context, runtime profile.Profile, image string, memory int64, network bool) (process *diagnosticProcess, err error) {
	if err := backend.Available(ctx, image, runtime.Contract().Platform); err != nil {
		return nil, err
	}
	name := "omnigrex-diagnostic-" + strings.ToLower(rand.Text())
	process = &diagnosticProcess{backend: backend, meta: metadata{Name: name}, transport: newTransport()}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			err = errors.Join(err, process.Remove(cleanup))
		}
	}()
	request := execution.Request{Image: image, User: "10001:10001", WorkingDir: "/workspace", MemoryBytes: memory, TemporaryDirectories: []execution.TemporaryDirectory{{Target: "/workspace", SizeBytes: 16 << 20}}}
	setup := "set -eu\nchown 10001:10001 /workspace /run/omnigrex\n"
	contract := runtime.Contract()
	for _, mount := range contract.Mounts {
		if mount.Path != "/workspace" {
			request.TemporaryDirectories = append(request.TemporaryDirectories, execution.TemporaryDirectory{Target: mount.Path, SizeBytes: 64 << 20})
			setup += "chown 10001:10001 " + quote(mount.Path) + "\n"
		}
	}
	for _, temporary := range contract.Tmpfs {
		request.TemporaryDirectories = append(request.TemporaryDirectories, execution.TemporaryDirectory{Target: temporary.Path, SizeBytes: temporary.SizeBytes, Executable: temporary.Executable})
		setup += "chown 10001:10001 " + quote(temporary.Path) + "\n"
	}
	definition := creation{Name: name, Request: request, Diagnostic: true, DiagnosticNetwork: network, Labels: map[string]string{namespaceLabel: backend.options.Namespace, diagnosticLabel: "true", expiryLabel: strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)}}
	meta, err := backend.platform.Create(ctx, definition)
	if err != nil {
		return process, errors.Join(execution.ErrUnknownEffect, err)
	}
	process.meta = meta
	qualification, err := environment.QualificationScript("amd64", contract.Name)
	if err != nil {
		return process, err
	}
	if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-ec", setup + qualification}, "0:0", nil); err != nil {
		return process, err
	}
	return process, nil
}

func (backend *Backend) CheckRepositoryEnvironment(ctx context.Context, runtime profile.Profile) (err error) {
	if environment := runtime.Contract().RepositoryEnvironment; environment != nil {
		var requirements []execution.Requirement
		for _, value := range environment.Requirements {
			requirements = append(requirements, execution.Requirement(value))
		}
		if err := backend.CheckRequirements(requirements); err != nil {
			return err
		}
	}
	process, err := backend.diagnostic(ctx, runtime, runtime.Contract().Image, 256<<20, false)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, process.Remove(cleanup))
	}()
	return nil
}

func (backend *Backend) StartDiagnostic(ctx context.Context, runtime profile.Profile, image string, memory int64, extraHosts []string, stderr io.Writer) (execution.Process, error) {
	process, err := backend.diagnostic(ctx, runtime, image, memory, true)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (execution.Process, error) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return nil, errors.Join(err, process.Remove(cleanup))
	}
	for _, entry := range extraHosts {
		host, address, found := strings.Cut(entry, ":")
		if !found || strings.ContainsAny(host+address, "\n\x00") {
			return fail(execution.ErrInvalidSpec)
		}
		if err := backend.platform.Execute(ctx, process.meta, []string{"/bin/sh", "-ec", "printf '%s\\n' " + quote(address+" "+host) + " >> /etc/hosts"}, "0:0", nil); err != nil {
			return fail(err)
		}
	}
	command := []string{"/usr/bin/env", "-i"}
	for _, variable := range runtime.Contract().Environment {
		command = append(command, variable.Name+"="+variable.Value)
	}
	command = append(command, "/usr/local/bin/opencode", "acp")
	session, err := backend.platform.Stream(ctx, process.meta, command, "10001:10001")
	if err != nil {
		return fail(err)
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if err := process.transport.attach(session, stderr, nil); err != nil {
		return fail(err)
	}
	return process, nil
}

// StartProtocolDiagnostic gives the agent adapter the same isolated non-TTY
// streams as a Turn, using only disposable files and diagnostic ownership.
func (backend *Backend) StartProtocolDiagnostic(ctx context.Context, spec execution.DiagnosticSpec, stderr io.Writer) (process execution.Process, err error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Namespace != backend.options.Namespace {
		return nil, execution.ErrBindingMismatch
	}
	if err := backend.Available(ctx, spec.Image, profile.Platform{OS: spec.Platform.OS, Arch: spec.Platform.Architecture}); err != nil {
		return nil, err
	}
	name := "omnigrex-protocol-" + strings.ToLower(rand.Text())
	p := &diagnosticProcess{backend: backend, meta: metadata{Name: name}, transport: newTransport()}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			err = errors.Join(err, p.Remove(cleanup))
		}
	}()
	request := execution.Request{Image: spec.Image, Platform: spec.Platform, User: "10001:10001", WorkingDir: "/workspace", MemoryBytes: spec.MemoryBytes, PIDsLimit: 128, TemporaryDirectories: spec.TemporaryDirectories}
	meta, err := backend.platform.Create(ctx, creation{Name: name, Request: request, Diagnostic: true, DiagnosticNetwork: true, Labels: map[string]string{namespaceLabel: backend.options.Namespace, diagnosticLabel: "true", expiryLabel: strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)}})
	if err != nil {
		return nil, errors.Join(execution.ErrUnknownEffect, err)
	}
	p.meta = meta
	setup := "set -eu\n"
	for _, directory := range spec.TemporaryDirectories {
		setup += "chown 10001:10001 " + quote(directory.Target) + "\n"
	}
	if err = backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-ec", setup}, "0:0", nil); err != nil {
		return nil, err
	}
	command := append([]string{"/usr/bin/env", "-i"}, spec.Environment...)
	command = append(command, spec.Command...)
	session, err := backend.platform.Stream(ctx, meta, command, "10001:10001")
	if err != nil {
		return nil, err
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if err = p.transport.attach(session, stderr, nil); err != nil {
		return nil, err
	}
	return p, nil
}
func (*diagnosticProcess) Start(context.Context) error           { return nil }
func (process *diagnosticProcess) Transport() io.ReadWriteCloser { return process.transport }
func (*diagnosticProcess) ObserveExit(context.Context) (execution.ExitObservation, error) {
	return execution.ExitObservation{}, nil
}
func (process *diagnosticProcess) Stop(ctx context.Context, _ time.Duration) error {
	return process.backend.platform.Stop(ctx, process.meta)
}
func (process *diagnosticProcess) Remove(ctx context.Context) error {
	closeErr := process.transport.Close()
	if process.meta.ID == "" {
		meta, err := process.backend.inspect(ctx, process.meta.Name)
		if err != nil {
			return errors.Join(closeErr, err)
		}
		if meta.Labels[diagnosticLabel] != "true" || meta.Labels[namespaceLabel] != process.backend.options.Namespace {
			return execution.ErrConflict
		}
		process.meta = meta
	}
	if err := process.backend.platform.Stop(ctx, process.meta); err != nil {
		return errors.Join(closeErr, err)
	}
	return errors.Join(closeErr, process.backend.platform.Remove(ctx, process.meta))
}
