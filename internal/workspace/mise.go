package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var ErrInvalidMiseEnvironment = errors.New("mise returned an invalid activation environment")

type MiseProvision struct {
	AssignmentID   string
	RepositoryURL  string
	Credential     string
	Revision       string
	ExecutionEpoch int64
	Fence          WorkspaceFence
}

type MiseActivation struct {
	DataDir        string
	SourceRevision string
	Environment    map[string]string
}

func (lifecycle *Lifecycle) ProvisionMise(ctx context.Context, provision MiseProvision) (MiseActivation, error) {
	paths, err := lifecycle.Paths(provision.AssignmentID)
	if err != nil {
		return MiseActivation{}, err
	}
	if err := validateRemote(provision.RepositoryURL); err != nil {
		return MiseActivation{}, err
	}
	if err := validateCredential(provision.Credential); err != nil {
		return MiseActivation{}, err
	}
	revision, err := normalizeObjectID(provision.Revision)
	if err != nil {
		return MiseActivation{}, err
	}
	assignmentRoot := filepath.Dir(paths.Mise)
	if err := ensureAssignmentDirectory(lifecycle.miseRoot, assignmentRoot); err != nil {
		return MiseActivation{}, fmt.Errorf("create assignment mise parent: %w", err)
	}
	release, err := acquireMiseProvisionLock(ctx, assignmentRoot)
	if err != nil {
		return MiseActivation{}, fmt.Errorf("serialize assignment mise provisioning: %w", err)
	}
	defer release()
	if err := claimMiseProvisionEpoch(ctx, assignmentRoot, provision.ExecutionEpoch, provision.Fence); err != nil {
		return MiseActivation{}, fmt.Errorf("claim assignment mise provisioning epoch: %w", err)
	}
	if err := ensureOwnedDirectory(paths.Mise, 0o755); err != nil {
		return MiseActivation{}, fmt.Errorf("create assignment mise data: %w", err)
	}
	if err := makeOwnedTreeDirectoriesWritable(ctx, paths.Mise); err != nil {
		return MiseActivation{}, fmt.Errorf("prepare assignment mise data for replacement: %w", err)
	}
	if err := removeOwnedDirectoryTree(ctx, paths.Mise); err != nil {
		return MiseActivation{}, fmt.Errorf("replace assignment mise data: %w", err)
	}
	if err := ensureOwnedDirectory(paths.Mise, 0o755); err != nil {
		return MiseActivation{}, fmt.Errorf("recreate assignment mise data: %w", err)
	}
	source := filepath.Join(assignmentRoot, "mise-source")
	if err := os.RemoveAll(source); err != nil {
		return MiseActivation{}, fmt.Errorf("replace trusted mise source: %w", err)
	}
	defer os.RemoveAll(source)
	if err := lifecycle.git(ctx, "clone trusted mise source", "", provision.Credential,
		"clone", "--no-checkout", "--origin=origin", "--config", "core.hooksPath=/dev/null", "--", provision.RepositoryURL, source); err != nil {
		return MiseActivation{}, err
	}
	if err := lifecycle.git(ctx, "set trusted mise source remote", source, "",
		"remote", "set-url", "origin", provision.RepositoryURL); err != nil {
		return MiseActivation{}, err
	}
	if err := lifecycle.git(ctx, "fetch trusted mise revision", source, provision.Credential,
		"fetch", "--force", "--no-tags", "origin", revision); err != nil {
		return MiseActivation{}, err
	}
	if err := lifecycle.git(ctx, "check out trusted mise revision", source, "",
		"checkout", "--detach", "--force", revision); err != nil {
		return MiseActivation{}, err
	}
	if err := lifecycle.git(ctx, "reset trusted mise revision", source, "",
		"reset", "--hard", revision); err != nil {
		return MiseActivation{}, err
	}

	isolation := miseIsolation(paths.Mise, source)
	activation := MiseActivation{DataDir: paths.Mise, SourceRevision: revision, Environment: runtimeMiseEnvironment(isolation)}
	configured, err := hasMiseConfiguration(source)
	if err != nil {
		return MiseActivation{}, err
	}
	if !configured {
		return activation, nil
	}
	if err := ensureOwnedDirectory(filepath.Join(paths.Mise, "tmp"), 0o700); err != nil {
		return MiseActivation{}, fmt.Errorf("create assignment mise temporary data: %w", err)
	}
	if _, err := lifecycle.mise(ctx, "install trusted repository tools", source, isolation, "install", "--yes"); err != nil {
		return MiseActivation{}, err
	}
	output, err := lifecycle.mise(ctx, "capture trusted repository tool environment", source, isolation, "env", "--json")
	if err != nil {
		return MiseActivation{}, err
	}
	var environment map[string]string
	if err := json.Unmarshal([]byte(output), &environment); err != nil {
		return MiseActivation{}, ErrInvalidMiseEnvironment
	}
	for name, value := range environment {
		if name == "" || strings.ContainsRune(name, '=') || strings.ContainsRune(name, '\x00') || strings.ContainsRune(value, '\x00') {
			return MiseActivation{}, ErrInvalidMiseEnvironment
		}
		if !provisioningOnlyMiseVariable(name) {
			activation.Environment[name] = value
		}
	}
	for name, value := range runtimeMiseEnvironment(isolation) {
		activation.Environment[name] = value
	}
	if _, err := inspectOwnedDirectory(paths.Mise); err != nil {
		return MiseActivation{}, err
	}
	temporary := filepath.Join(paths.Mise, "tmp")
	if exists, err := inspectOwnedDirectory(temporary); err != nil {
		return MiseActivation{}, fmt.Errorf("inspect assignment mise temporary data: %w", err)
	} else if exists {
		if err := makeOwnedTreeDirectoriesWritable(ctx, temporary); err != nil {
			return MiseActivation{}, fmt.Errorf("prepare assignment mise temporary data for removal: %w", err)
		}
		if err := removeOwnedDirectoryTree(ctx, temporary); err != nil {
			return MiseActivation{}, fmt.Errorf("remove assignment mise temporary data: %w", err)
		}
	}
	return activation, nil
}

func acquireMiseProvisionLock(ctx context.Context, assignmentRoot string) (func(), error) {
	path := filepath.Join(assignmentRoot, ".mise-provision.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int(stat.Uid) != os.Geteuid() {
		unix.Close(fd)
		return nil, ErrUnsafeAssignmentPath
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			unix.Close(fd)
			return nil, err
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			unix.Close(fd)
			return nil, err
		}
		select {
		case <-ctx.Done():
			unix.Close(fd)
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (lifecycle *Lifecycle) mise(ctx context.Context, operation, directory string, environment map[string]string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, lifecycle.miseExecutable, arguments...)
	command.Dir = directory
	command.Env = commandEnvironment("", "")
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		command.Env = append(command.Env, name+"="+environment[name])
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s: %s", operation, detail)
	}
	return strings.TrimSpace(string(output)), nil
}

func miseIsolation(dataDir, trustedSource string) map[string]string {
	temporary := filepath.Join(dataDir, "tmp")
	return map[string]string{
		"GOFLAGS":                   "-p=1 -modcacherw",
		"GOTMPDIR":                  temporary,
		"HOME":                      filepath.Join(dataDir, "home"),
		"MISE_CACHE_DIR":            filepath.Join(dataDir, "cache"),
		"MISE_CONFIG_DIR":           filepath.Join(dataDir, "config"),
		"MISE_DATA_DIR":             dataDir,
		"MISE_GLOBAL_CONFIG_FILE":   "/etc/omnigrex/mise-global.toml",
		"MISE_JOBS":                 "1",
		"MISE_PROJECT_ROOT":         trustedSource,
		"MISE_STATE_DIR":            filepath.Join(dataDir, "state"),
		"MISE_SYSTEM_CONFIG_FILE":   "/etc/omnigrex/mise-system.toml",
		"MISE_TMP_DIR":              temporary,
		"MISE_TRUSTED_CONFIG_PATHS": trustedSource,
		"TMPDIR":                    temporary,
		"XDG_CACHE_HOME":            filepath.Join(dataDir, "xdg-cache"),
		"XDG_CONFIG_HOME":           filepath.Join(dataDir, "xdg-config"),
		"XDG_DATA_HOME":             filepath.Join(dataDir, "xdg-data"),
		"XDG_STATE_HOME":            filepath.Join(dataDir, "xdg-state"),
	}
}

func runtimeMiseEnvironment(isolation map[string]string) map[string]string {
	environment := make(map[string]string, len(isolation)-1)
	for name, value := range isolation {
		if !provisioningOnlyMiseVariable(name) {
			environment[name] = value
		}
	}
	environment["MISE_IGNORED_CONFIG_PATHS"] = "/workspace"
	environment["MISE_LEGACY_VERSION_FILE"] = "false"
	environment["MISE_OVERRIDE_CONFIG_FILENAMES"] = "none"
	environment["MISE_OVERRIDE_TOOL_VERSIONS_FILENAMES"] = "none"
	return environment
}

func provisioningOnlyMiseVariable(name string) bool {
	switch name {
	case "HOME", "GOFLAGS", "GOTMPDIR", "MISE_JOBS", "MISE_PROJECT_ROOT", "MISE_TMP_DIR", "MISE_TRUSTED_CONFIG_PATHS", "TMPDIR":
		return true
	default:
		return strings.HasPrefix(name, "XDG_")
	}
}

func hasMiseConfiguration(root string) (bool, error) {
	for _, name := range []string{"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml", ".tool-versions"} {
		_, err := os.Lstat(filepath.Join(root, name))
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect trusted mise configuration: %w", err)
		}
	}
	return false, nil
}
