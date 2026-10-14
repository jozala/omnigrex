// Package environment compiles and runs repository tooling inside an Execution Environment.
package environment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/turnconfig"
)

const Executable = "/usr/local/bin/omnigrex-environment"
const MiseExecutable = "/usr/local/bin/mise"

// DataDirectory is part of the immutable Pi Runtime Profile mount contract.
const DataDirectory = "/home/opencode/.local/share/mise"
const environmentKeys = "OMNIGREX_RUNTIME_ENV_KEYS"

// BindEnvironment identifies the control-plane entries that may reach the agent.
// Unrequested image ENV settings cannot enable runtime discovery or loaders.
func BindEnvironment(values []string) []string {
	names := make([]string, 0, len(values))
	for _, entry := range values {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	encoded, _ := json.Marshal(names)
	return append(values[:len(values):len(values)], environmentKeys+"="+string(encoded))
}

// Bootstrap installs from the prepared working checkout and returns a protected
// runtime environment. It never executes repository tools on the control plane.
func Bootstrap(ctx context.Context, directory, data, mise string, inherited []string, stderr io.Writer) (values []string, failure error) {
	setup := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": filepath.Join(data, "home"),
		"MISE_DATA_DIR": data, "MISE_CONFIG_DIR": filepath.Join(data, "config"),
		"MISE_CACHE_DIR": filepath.Join(data, "cache"), "MISE_STATE_DIR": filepath.Join(data, "state"),
		"MISE_GLOBAL_CONFIG_FILE":   "/etc/omnigrex/mise-global.toml",
		"MISE_SYSTEM_CONFIG_FILE":   "/etc/omnigrex/mise-system.toml",
		"MISE_TRUSTED_CONFIG_PATHS": directory, "MISE_PROJECT_ROOT": directory,
		"MISE_JOBS": "1", "GOFLAGS": "-p=1 -modcacherw",
		"TMPDIR": filepath.Join(data, "tmp"), "GOTMPDIR": filepath.Join(data, "tmp"), "MISE_TMP_DIR": filepath.Join(data, "tmp"),
		"XDG_CONFIG_HOME": filepath.Join(data, "xdg-config"), "XDG_CACHE_HOME": filepath.Join(data, "xdg-cache"),
		"XDG_DATA_HOME": filepath.Join(data, "xdg-data"), "XDG_STATE_HOME": filepath.Join(data, "xdg-state"),
	}
	var admitted []string
	for _, entry := range inherited {
		if content, found := strings.CutPrefix(entry, environmentKeys+"="); found {
			if json.Unmarshal([]byte(content), &admitted) != nil {
				return nil, errors.New("invalid environment admission")
			}
		}
	}
	if len(admitted) == 0 {
		return nil, errors.New("missing environment admission")
	}
	allowed := make(map[string]bool, len(admitted))
	for _, name := range admitted {
		allowed[name] = true
	}
	runtime := map[string]string{}
	for _, entry := range inherited {
		name, value, found := strings.Cut(entry, "=")
		if found && allowed[name] && !loaderVariable(name) {
			runtime[name] = value
		}
	}
	runtime["PATH"] = setup["PATH"]
	// Replace installation data every Turn, keeping separately attached tool-data
	// caches and Retained Runtime State untouched. Removal never follows links.
	if err := resetToolData(ctx, data); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(setup["TMPDIR"], 0o700); err != nil {
		return nil, errors.New("create repository tool scratch failed")
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := removeScratch(cleanupCtx, setup["TMPDIR"]); err != nil {
			values = nil
			failure = errors.Join(failure, errors.New("remove repository tool scratch failed"))
		} else if failure == nil && stderr != nil {
			_, _ = io.WriteString(stderr, "repository environment ready\n")
		}
	}()
	configured := false
	for _, name := range []string{"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml", ".tool-versions"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil {
			configured = true
		} else if !os.IsNotExist(err) {
			return nil, errors.New("inspect repository tool configuration failed")
		}
	}
	if configured {
		install := exec.CommandContext(ctx, mise, "install", "--yes")
		install.Dir, install.Env = directory, entries(setup)
		install.WaitDelay = time.Second
		// Setup output can contain arbitrary repository text. Keep it out of
		// protocol stdout and credential-bearing orchestrator diagnostics.
		install.Stdout, install.Stderr = io.Discard, io.Discard
		if err := install.Run(); err != nil {
			return nil, fmt.Errorf("repository tool installation failed: %w", safeCommandError(ctx, err))
		}
		command := exec.CommandContext(ctx, mise, "env", "--json")
		command.Dir, command.Env = directory, entries(setup)
		command.WaitDelay = time.Second
		var output boundedBuffer
		command.Stdout, command.Stderr = &output, io.Discard
		if err := command.Run(); err != nil {
			return nil, fmt.Errorf("repository tool activation failed: %w", safeCommandError(ctx, err))
		}
		var activation map[string]string
		if json.Unmarshal(output.Bytes(), &activation) != nil {
			return nil, errors.New("invalid repository tool activation")
		}
		for name, value := range activation {
			if name == "" || len(name) > 128 || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
				return nil, errors.New("invalid repository tool activation")
			}
			if name == "PATH" {
				for _, path := range strings.Split(value, ":") {
					if !filepath.IsAbs(path) || filepath.Clean(path) != path {
						return nil, errors.New("invalid repository tool PATH")
					}
				}
				runtime[name] = value
			} else if turnconfig.SafeVariable(name) {
				// Control-plane entries include allocated paths with specific storage
				// and cleanup lifetimes. Repository activation may only fill gaps.
				if !allowed[name] && name != "TMPDIR" && name != "GOTMPDIR" {
					runtime[name] = value
				}
			} else if trusted, present := setup[name]; !present || value != trusted {
				return nil, errors.New("repository tool activation attempted to override a protected variable")
			}
		}
	}
	for _, name := range []string{"MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_CONFIG_DIR", "MISE_STATE_DIR", "MISE_GLOBAL_CONFIG_FILE", "MISE_SYSTEM_CONFIG_FILE"} {
		runtime[name] = setup[name]
	}
	runtime["MISE_IGNORED_CONFIG_PATHS"] = directory
	runtime["MISE_LEGACY_VERSION_FILE"] = "false"
	runtime["MISE_OVERRIDE_CONFIG_FILENAMES"] = "none"
	runtime["MISE_OVERRIDE_TOOL_VERSIONS_FILENAMES"] = "none"
	return entries(runtime), nil
}

func removeScratch(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(path)
	}
	if err := resetToolData(ctx, path); err != nil {
		return err
	}
	return os.Remove(path)
}

func loaderVariable(name string) bool {
	return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") ||
		name == "NODE_OPTIONS" || name == "BUN_OPTIONS" || name == "ENV" || name == "BASH_ENV" || name == "SHELLOPTS"
}

func resetToolData(ctx context.Context, data string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		return errors.New("create repository tool data failed")
	}
	info, err := os.Lstat(data)
	if err != nil || !info.IsDir() {
		return errors.New("unsafe repository tool data")
	}
	if err := filepath.WalkDir(data, func(path string, entry os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	}); err != nil {
		return errors.New("prepare repository tool data for replacement failed")
	}
	children, err := os.ReadDir(data)
	if err != nil {
		return errors.New("inspect repository tool data failed")
	}
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(data, child.Name())); err != nil {
			return errors.New("reset repository tool data failed")
		}
	}
	return nil
}

type boundedBuffer struct{ bytes.Buffer }

func (buffer *boundedBuffer) Write(content []byte) (int, error) {
	if buffer.Len()+len(content) > 120<<10 {
		return 0, errors.New("tool activation exceeds 120 KiB")
	}
	return buffer.Buffer.Write(content)
}

func safeCommandError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("exit status %d", exit.ExitCode())
	}
	return errors.New("cannot execute mise")
}

func entries(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}
