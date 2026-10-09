package microsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

const namespaceLabel = "io.omnigrex.microsandbox.namespace"
const identityLabel = "io.omnigrex.execution.key"
const digestLabel = "io.omnigrex.execution.digest"

type Backend struct {
	options  Options
	platform platform
	ledger   execution.Ledger
	inputs   *inputBindings
}

func New(options Options, ledger execution.Ledger) (*Backend, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	driver, err := newPlatform(options)
	if err != nil {
		return nil, err
	}
	return &Backend{options: options, platform: driver, ledger: ledger, inputs: newInputBindings()}, nil
}

func (backend *Backend) Close() error                    { backend.inputs.clear(); return nil }
func (backend *Backend) Check(ctx context.Context) error { return Check(ctx, backend.options) }
func (*Backend) CheckRequirements(requirements []execution.Requirement) error {
	for _, requirement := range requirements {
		switch requirement {
		case execution.ProcessIsolation, execution.HardwareIsolation, execution.ToolInstallation, execution.NestedContainers:
		default:
			return fmt.Errorf("%w: Microsandbox requirement %q", execution.ErrUnsupportedRequirement, requirement)
		}
	}
	return nil
}

func validKey(key execution.Key) bool {
	return uuidtext.ValidCanonicalNonNil(key.AssignmentID) && uuidtext.ValidCanonicalNonNil(key.AgentSessionID) && uuidtext.ValidCanonicalNonNil(key.AgentTurnID) && key.ExecutionEpoch > 0 && key.RuntimeProfileName != "" && key.RuntimeProfileVersion != ""
}

func nameFor(key execution.Key) string {
	return "omnigrex-" + key.AgentTurnID + "-" + strconv.FormatInt(key.ExecutionEpoch, 10)
}

func requestDigest(request execution.Request) string {
	request.Environment = slices.Clone(request.Environment)
	for index, entry := range request.Environment {
		name, content, _ := strings.Cut(entry, "=")
		if name == "OPENCODE_AUTH_CONTENT" {
			request.Environment[index] = name + "=<ephemeral>"
		}
		if name == "OPENCODE_CONFIG_CONTENT" {
			var configuration map[string]any
			if json.Unmarshal([]byte(content), &configuration) == nil {
				redactAuthorization(configuration)
				content, _ := json.Marshal(configuration)
				request.Environment[index] = name + "=" + string(content)
			}
		}
	}
	content, _ := json.Marshal(request)
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func redactAuthorization(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if strings.EqualFold(key, "authorization") {
				value[key] = "<ephemeral>"
			} else {
				redactAuthorization(child)
			}
		}
	case []any:
		for _, child := range value {
			redactAuthorization(child)
		}
	}
}

func (backend *Backend) New(request execution.Request, ledger execution.Ledger) (execution.Engine, error) {
	if err := backend.validate(request); err != nil {
		return nil, err
	}
	if ledger == nil {
		return nil, execution.ErrInvalidSpec
	}
	request.Command = slices.Clone(request.Command)
	request.Environment = slices.Clone(request.Environment)
	request.Attachments = slices.Clone(request.Attachments)
	request.TemporaryDirectories = slices.Clone(request.TemporaryDirectories)
	request.Requirements = slices.Clone(request.Requirements)
	bound, err := backend.inputs.bind(request)
	if err != nil {
		return nil, err
	}
	return &engine{backend: backend, request: request, ledger: ledger, digest: backend.specificationDigest(request), inputs: bound}, nil
}

func (backend *Backend) specificationDigest(request execution.Request) string {
	content, _ := json.Marshal(struct {
		Request, Namespace, MCP string
		CPUs                    int
		RootDiskMiB             int64
		Duration                time.Duration
	}{requestDigest(request), backend.options.Namespace, backend.options.MCPEndpoint, backend.options.CPUs, backend.options.RootDiskMiB, backend.options.MaximumDuration})
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func (backend *Backend) validate(request execution.Request) error {
	if err := backend.CheckRequirements(request.Requirements); err != nil {
		return err
	}
	if !validKey(request.Key) || !profile.IsExactRegistryImage(request.Image) || request.Platform != (execution.Platform{OS: "linux", Architecture: "amd64"}) || request.User != "10001:10001" || request.MemoryBytes < 128<<20 || request.MemoryBytes%(1<<20) != 0 || request.PIDsLimit < 1 || len(request.Command) == 0 || !filepath.IsAbs(request.Command[0]) || !filepath.IsAbs(request.WorkingDir) || filepath.Clean(request.WorkingDir) != request.WorkingDir {
		return execution.ErrInvalidSpec
	}
	if request.MemoryBytes>>20 > int64(^uint32(0)) {
		return execution.ErrInvalidSpec
	}
	for _, argument := range request.Command {
		if strings.ContainsRune(argument, '\x00') {
			return execution.ErrInvalidSpec
		}
	}
	if request.BootstrapImage != "" {
		return fmt.Errorf("%w: legacy Docker bootstrap binding", execution.ErrBindingMismatch)
	}
	if slices.Contains(request.Requirements, execution.ToolInstallation) && !request.GoldenResources {
		return execution.ErrInvalidSpec
	}
	if request.Workspace != (execution.WorkspaceRef{}) && request.Workspace.Key != request.Key {
		return execution.ErrBindingMismatch
	}
	if request.State != (execution.StateRef{}) && (request.State.Validate() != nil || request.State.Backend != backend.Binding() || request.State.ParticipantID != request.Key.AssignmentID || request.State.SessionID != request.Key.AgentSessionID || request.State.Image != request.Image) {
		return execution.ErrBindingMismatch
	}
	seen, targets := map[string]bool{}, map[string]bool{}
	for _, attachment := range request.Attachments {
		if backend.location(request.Key, attachment.Resource) == "" || seen[attachment.Resource] || targets[attachment.Target] || !safeTarget(attachment.Target) {
			return execution.ErrInvalidSpec
		}
		seen[attachment.Resource], targets[attachment.Target] = true, true
	}
	if request.GoldenResources && (!seen["workspace"] || !seen["state"] || !seen["mise"] || !seen["tool-data"]) {
		return execution.ErrInvalidSpec
	}
	for _, directory := range request.TemporaryDirectories {
		if !safeTarget(directory.Target) || targets[directory.Target] || directory.SizeBytes < 1 || directory.SizeBytes%(1<<20) != 0 || directory.SizeBytes>>20 > int64(^uint32(0)) {
			return execution.ErrInvalidSpec
		}
		targets[directory.Target] = true
	}
	names := map[string]bool{}
	for _, entry := range request.Environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.ContainsAny(name, "=\x00\n") || strings.ContainsRune(value, '\x00') || names[name] {
			return execution.ErrInvalidSpec
		}
		names[name] = true
	}
	return nil
}

func safeTarget(target string) bool {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target || target == "/" || strings.ContainsAny(target, ":\x00\n") || target == "/run" {
		return false
	}
	for _, reserved := range []string{"/.msb", "/proc", "/sys", "/dev", "/run/omnigrex"} {
		if target == reserved || strings.HasPrefix(target, reserved+"/") {
			return false
		}
	}
	return true
}

func (backend *Backend) Binding() execution.BackendBinding {
	return execution.BackendBinding{Family: "microsandbox", Version: 1, Namespace: backend.options.Namespace}
}

func (backend *Backend) location(key execution.Key, resource string) string {
	participant := "assignment-" + key.AssignmentID
	switch resource {
	case "workspace":
		return filepath.Join(backend.options.WorkspaceRoot, participant, "workspace")
	case "state":
		return filepath.Join(backend.options.StateRoot, participant, "runtime-state")
	case "mise":
		return filepath.Join(backend.options.ToolRoot, participant, "mise")
	case "tool-data":
		return filepath.Join(backend.options.ToolRoot, participant, "tool-data")
	}
	return ""
}

// The private journal is a dispatch barrier, not admission authority. It shares
// neither a mount nor a writable directory with the guest. Flock serializes all
// SDK dispatch and cleanup across orchestrator processes, including Start.
type operation struct {
	Key                                                  execution.Key
	Digest                                               string
	Created, Started, Revoked, Stopped, Removed, Flushed bool
	WorkspaceDevice, WorkspaceInode                      uint64
	ResourceID                                           string
	ProcessExited                                        bool
	ProcessExitCode                                      int
	ProcessExitedAt                                      time.Time
	PreparationStarted, Prepared                         bool
	InputOwner                                           string
	LaunchHistoryKnown                                   bool
}

func (backend *Backend) locked(ctx context.Context, key execution.Key, callback func(*operation) error) error {
	if !validKey(key) {
		return execution.ErrInvalidSpec
	}
	location := filepath.Join(backend.options.Home, "omnigrex-operations", nameFor(key))
	if err := os.MkdirAll(filepath.Dir(location), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(location+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			break
		} else if err != unix.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	record := operation{Key: key}
	content, err := os.ReadFile(location + ".json")
	if err == nil {
		if json.Unmarshal(content, &record) != nil || record.Key != key {
			return execution.ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result := callback(&record)
	return errors.Join(result, backend.persist(key, &record))
}

func (backend *Backend) persist(key execution.Key, record *operation) error {
	location := filepath.Join(backend.options.Home, "omnigrex-operations", nameFor(key))
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(location), ".operation-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(content)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err == nil {
		err = os.Rename(file.Name(), location+".json")
	}
	if err == nil {
		directory, openErr := os.Open(filepath.Dir(location))
		if openErr != nil {
			err = openErr
		} else {
			err = errors.Join(directory.Sync(), directory.Close())
		}
	}
	return err
}

func (backend *Backend) inspect(ctx context.Context, name string) (metadata, error) {
	meta, err := backend.platform.Lookup(ctx, name)
	if err != nil {
		return meta, errors.Join(execution.ErrUnknownEffect, err)
	}
	return meta, nil
}

func (backend *Backend) owned(observation metadata, key execution.Key, digest string) bool {
	content, _ := json.Marshal(key)
	return observation.ID != "" && observation.Name == nameFor(key) && observation.Labels[namespaceLabel] == backend.options.Namespace && observation.Labels[identityLabel] == string(content) && observation.Labels[digestLabel] == digest
}

func resourceRef(meta metadata) execution.Ref {
	content, _ := json.Marshal([]string{meta.Name, meta.ID})
	return "msb:" + base64.RawURLEncoding.EncodeToString(content)
}
func (backend *Backend) resolveRef(ctx context.Context, ref execution.Ref) (metadata, error) {
	if !strings.HasPrefix(ref, "msb:") {
		return metadata{}, execution.ErrInvalidSpec
	}
	content, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ref, "msb:"))
	var identity []string
	if err != nil || json.Unmarshal(content, &identity) != nil || len(identity) != 2 || identity[0] == "" || identity[1] == "" {
		return metadata{}, execution.ErrInvalidSpec
	}
	actual, err := backend.inspect(ctx, identity[0])
	if err != nil {
		return metadata{}, err
	}
	if actual.ID != identity[1] {
		return metadata{}, execution.ErrConflict
	}
	return actual, nil
}

type engine struct {
	backend *Backend
	request execution.Request
	ledger  execution.Ledger
	digest  string
	inputs  inputBinding
}

func (engine *engine) Close() error { return nil }
func (engine *engine) EnsureAbsent(ctx context.Context) error {
	return engine.backend.clean(ctx, engine.request.Key, engine.ledger, true)
}

func (engine *engine) Create(ctx context.Context, request execution.Request, stderr io.Writer) (execution.Process, error) {
	if engine.backend.specificationDigest(request) != engine.digest || inputFingerprint(request) != engine.inputs.fingerprint {
		return nil, execution.ErrConflict
	}
	request = engine.request
	if err := engine.backend.Available(ctx, request.Image, profile.Platform{OS: request.Platform.OS, Arch: request.Platform.Architecture}); err != nil {
		return nil, err
	}
	var process *process
	err := engine.backend.locked(ctx, request.Key, func(record *operation) error {
		if record.Revoked || record.Removed || record.Started {
			return execution.ErrConflict
		}
		if record.InputOwner != "" && record.InputOwner != engine.inputs.owner {
			return execution.ErrConflict
		}
		record.InputOwner = engine.inputs.owner
		digest := engine.digest
		reservation, fresh, err := engine.ledger.ReserveExecution(ctx, request.Key, digest)
		if err != nil {
			return err
		}
		if reservation.Revoked || record.Digest != "" && record.Digest != digest {
			return execution.ErrConflict
		}
		// Only fresh Store admission can establish new no-start history. A lost
		// backend journal cannot authorize adopting an existing native execution.
		if !fresh && !record.LaunchHistoryKnown {
			return execution.ErrConflict
		}
		if fresh {
			record.LaunchHistoryKnown = true
		}
		record.Digest = digest
		if !fresh && reservation.Resource == "" {
			if reservation.CreationSettled {
				return execution.ErrConflict
			}
			actual, err := engine.backend.inspect(ctx, nameFor(request.Key))
			if err != nil || !engine.backend.owned(actual, request.Key, digest) {
				return errors.Join(execution.ErrUnknownEffect, err)
			}
			if err := engine.ledger.BindExecution(ctx, request.Key, resourceRef(actual)); err != nil {
				return err
			}
			record.Created = true
			record.ResourceID = actual.ID
		}
		if !record.Created {
			if request.GoldenResources {
				qualified, err := profile.NewOpenCodeV1(request.Image, profile.Platform{OS: request.Platform.OS, Arch: request.Platform.Architecture})
				if err != nil {
					return err
				}
				if err := engine.backend.CheckRepositoryEnvironment(ctx, qualified); err != nil {
					return err
				}
			}
			definition, err := engine.backend.createDefinition(request, digest)
			if err != nil {
				return err
			}
			if request.Workspace != (execution.WorkspaceRef{}) {
				var stat unix.Stat_t
				if err := unix.Lstat(engine.backend.location(request.Key, "workspace"), &stat); err != nil {
					return err
				}
				record.WorkspaceDevice, record.WorkspaceInode = uint64(stat.Dev), stat.Ino
			}
			if err := engine.backend.persist(request.Key, record); err != nil {
				return err
			}
			// All fallible local preparation is complete before dispatch admission.
			// Failure of this transaction still proves the provider was not called,
			// even if its SQL acknowledgement is ambiguous.
			if err := engine.ledger.BeginExecutionPhase(ctx, request.Key, execution.Main); err != nil {
				return engine.recordNoDispatch(ctx, request.Key, err)
			}
			actual, err := engine.backend.platform.Create(ctx, definition)
			if errors.Is(err, errNoCreationEffects) {
				return engine.recordNoDispatch(ctx, request.Key, err)
			}
			if err != nil || !engine.backend.owned(actual, request.Key, digest) {
				return errors.Join(execution.ErrUnknownEffect, err)
			}
			record.Created = true
			record.ResourceID = actual.ID
			if err := engine.ledger.BindExecution(ctx, request.Key, resourceRef(actual)); err != nil {
				return errors.Join(execution.ErrUnknownEffect, err)
			}
		}
		actual, err := engine.backend.inspect(ctx, nameFor(request.Key))
		if err != nil || !engine.backend.owned(actual, request.Key, digest) || actual.ID != record.ResourceID || actual.Status != "Running" {
			return errors.Join(execution.ErrConflict, err)
		}
		if !record.Prepared {
			if record.PreparationStarted {
				return execution.ErrUnknownEffect
			}
			record.PreparationStarted = true
			if err := engine.backend.persist(request.Key, record); err != nil {
				return err
			}
			if err := engine.backend.prepare(ctx, request); err != nil {
				return errors.Join(execution.ErrUnknownEffect, err)
			}
			record.Prepared = true
		}
		process = newProcess(engine, stderr)
		return nil
	})
	return process, err
}

var errNoCreationEffects = errors.New("Microsandbox creation was not dispatched")

func (engine *engine) recordNoDispatch(ctx context.Context, key execution.Key, failure error) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := engine.ledger.RecordNoExecution(recordCtx, key); err != nil {
		return errors.Join(execution.ErrUnknownEffect, failure, err)
	}
	return failure
}

func (backend *Backend) createDefinition(request execution.Request, digest string) (creation, error) {
	content, _ := json.Marshal(request.Key)
	definition := creation{Request: request, Name: nameFor(request.Key), Labels: map[string]string{namespaceLabel: backend.options.Namespace, identityLabel: string(content), digestLabel: digest}, Mounts: map[string]string{}}
	// Persisted vendor configuration must never contain agent environment data.
	definition.Request.Environment = nil
	for _, attachment := range request.Attachments {
		location := backend.location(request.Key, attachment.Resource)
		if attachment.Resource == "workspace" || attachment.Resource == "state" {
			info, err := os.Lstat(location)
			if err != nil || !info.IsDir() {
				return creation{}, execution.ErrStateUnavailable
			}
		} else if err := os.MkdirAll(location, 0700); err != nil {
			return creation{}, err
		}
		// No stat overlay: host-created seeds and guest changes have one POSIX
		// representation, so captures and Session archives need no xattr decoder.
		definition.Mounts[attachment.Target] = location
	}
	return definition, nil
}

type endpointRule struct{ host, port string }

func mcpRule(value string) (endpointRule, error) {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return endpointRule{}, execution.ErrInvalidSpec
	}
	port := endpoint.Port()
	if port == "" {
		port = "80"
		if endpoint.Scheme == "https" {
			port = "443"
		}
	}
	return endpointRule{endpoint.Hostname(), port}, nil
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func (backend *Backend) prepare(ctx context.Context, request execution.Request) error {
	name := nameFor(request.Key)
	meta, err := backend.inspect(ctx, name)
	if err != nil {
		return err
	}
	ownership := "set -eu\nchown 10001:10001 /run/omnigrex; chmod 0700 /run/omnigrex\n"
	for _, directory := range request.TemporaryDirectories {
		ownership += "chown 10001:10001 " + quote(directory.Target) + "; chmod 0700 " + quote(directory.Target) + "\n"
	}
	if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-ec", ownership}, "0:0", nil); err != nil {
		return err
	}
	// Control-plane settings/credentials never enter the retained msb config.
	// The launch file is guest tmpfs, supplied over stdin, not argv or a mount.
	script := "#!/bin/sh\nexec env -i"
	hasPath := false
	for _, entry := range request.Environment {
		if strings.HasPrefix(entry, "PATH=") {
			hasPath = true
		}
	}
	if !hasPath {
		script += " " + quote("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	for _, entry := range request.Environment {
		script += " " + quote(entry)
	}
	for _, argument := range request.Command {
		script += " " + quote(argument)
	}
	script += "\n"
	if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-ec", "umask 077; cat > /run/omnigrex/launch"}, request.User, strings.NewReader(script)); err != nil {
		return err
	}
	setup := `set -eu
echo +pids > /sys/fs/cgroup/cgroup.subtree_control
mkdir -p /sys/fs/cgroup/microsandbox-workload
echo ` + strconv.FormatInt(request.PIDsLimit, 10) + ` > /sys/fs/cgroup/microsandbox-workload/pids.max
test "$(cat /sys/fs/cgroup/microsandbox-workload/pids.max)" = ` + strconv.FormatInt(request.PIDsLimit, 10) + "\n"
	if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-ec", setup}, "0:0", nil); err != nil {
		return fmt.Errorf("enforce guest workload PID budget: %w", err)
	}
	if slices.Contains(request.Requirements, execution.NestedContainers) {
		setup = `set -eu
command -v dockerd >/dev/null
command -v docker >/dev/null
test ! -S /var/run/docker.sock
dockerd --host unix:///var/run/docker.sock --cgroup-parent=/microsandbox-workload >/run/omnigrex/docker.log 2>&1 &
for attempt in $(seq 1 60); do
  if docker info >/dev/null 2>&1; then chmod 0666 /var/run/docker.sock; exit 0; fi
  sleep 1
done
exit 1
`
		if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-ec", setup}, "0:0", nil); err != nil {
			return fmt.Errorf("start private guest Docker daemon: %w", err)
		}
	}
	return nil
}
