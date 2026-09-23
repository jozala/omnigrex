package workspace_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/workspace"
)

func TestLifecycleProvisionsMiseOnlyFromSelectedTrustedRevision(t *testing.T) {
	fixture := newGitFixture(t)
	root := t.TempDir()
	t.Setenv("SENSITIVE_ORCHESTRATOR_SECRET", "must-not-reach-mise")
	logPath := filepath.Join(root, "mise.log")
	fakeMise := filepath.Join(root, "mise")
	writeExecutable(t, fakeMise, `#!/bin/sh
set -eu
test -z "${SENSITIVE_ORCHESTRATOR_SECRET:-}"
printf '%s|%s|' "$PWD" "$*" >> `+quoted(logPath)+`
tr -d '\n' < mise.toml >> `+quoted(logPath)+`
printf '\n' >> `+quoted(logPath)+`
if [ "$1" = "env" ]; then
  printf '{"PATH":"%s/installs/trusted/1/bin","TOOL_MODE":"trusted"}\n' "$MISE_DATA_DIR"
fi
`)
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"),
		MiseRoot: filepath.Join(root, "mise-data"), MiseExecutable: fakeMise,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := os.ReadFile(filepath.Join(paths.Workspace, "mise.toml"))
	if err != nil || !strings.Contains(string(visible), "poison") {
		t.Fatalf("feature mise.toml = %q, %v", visible, err)
	}
	maliciousConfig := []byte("[tools]\nnode = \"0.0.0-omnigrex-isolation-test\"\n[env]\nOMNIGREX_UNTRUSTED_MISE_CONFIG = \"evaluated\"\n")
	for _, name := range []string{"mise.toml", ".mise.toml", ".omnigrex-disabled-mise.toml", "none"} {
		if err := os.WriteFile(filepath.Join(paths.Workspace, name), maliciousConfig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, ".tool-versions"), []byte("node 0.0.0-omnigrex-isolation-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	activation, err := lifecycle.ProvisionMise(context.Background(), workspace.MiseProvision{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first,
	})
	if err != nil {
		t.Fatalf("ProvisionMise() error = %v", err)
	}
	if activation.DataDir != paths.Mise || activation.SourceRevision != fixture.first {
		t.Errorf("activation = %#v", activation)
	}
	if activation.Environment["MISE_DATA_DIR"] != paths.Mise || activation.Environment["TOOL_MODE"] != "trusted" ||
		activation.Environment["PATH"] != filepath.Join(paths.Mise, "installs", "trusted", "1", "bin") {
		t.Errorf("activation environment = %#v", activation.Environment)
	}
	wantRuntimeIsolation := map[string]string{
		"MISE_IGNORED_CONFIG_PATHS":             "/workspace",
		"MISE_LEGACY_VERSION_FILE":              "false",
		"MISE_OVERRIDE_CONFIG_FILENAMES":        "none",
		"MISE_OVERRIDE_TOOL_VERSIONS_FILENAMES": "none",
	}
	for name, want := range wantRuntimeIsolation {
		if got := activation.Environment[name]; got != want {
			t.Errorf("activation environment %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"HOME", "MISE_PROJECT_ROOT", "MISE_TRUSTED_CONFIG_PATHS", "XDG_CONFIG_HOME"} {
		if _, found := activation.Environment[name]; found {
			t.Errorf("activation exposes trusted provisioning environment %s", name)
		}
	}
	logContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logContent)
	if strings.Contains(log, "poison") || strings.Count(log, "trusted = \"1\"") != 2 {
		t.Errorf("mise evaluated unexpected configuration:\n%s", log)
	}
	if strings.Contains(log, paths.Workspace) {
		t.Errorf("mise ran in feature workspace:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(paths.Mise), "mise-source")); !os.IsNotExist(err) {
		t.Errorf("trusted source checkout remains: %v", err)
	}
	t.Run("pinned runtime mise ignores feature configuration", func(t *testing.T) {
		assertPinnedRuntimeMiseIsolation(t, paths, activation)
	})
}

func assertPinnedRuntimeMiseIsolation(t *testing.T, paths workspace.Paths, activation workspace.MiseActivation) {
	t.Helper()
	miseExecutable, err := exec.LookPath("mise")
	if err != nil {
		t.Skip("mise is not installed; runtime environment contract assertions still ran")
	}
	version := exec.Command(miseExecutable, "--version")
	output, err := version.Output()
	if err != nil || !strings.HasPrefix(string(output), "2026.8.10 ") {
		t.Skipf("mise 2026.8.10 is required for the runtime semantics check, got %q (%v)", strings.TrimSpace(string(output)), err)
	}

	environment := map[string]string{
		"HOME":            filepath.Join(activation.DataDir, "runtime-home"),
		"LANG":            "C",
		"LC_ALL":          "C",
		"MISE_OFFLINE":    "true",
		"XDG_CACHE_HOME":  filepath.Join(activation.DataDir, "runtime-xdg-cache"),
		"XDG_CONFIG_HOME": filepath.Join(activation.DataDir, "runtime-xdg-config"),
		"XDG_DATA_HOME":   filepath.Join(activation.DataDir, "runtime-xdg-data"),
		"XDG_STATE_HOME":  filepath.Join(activation.DataDir, "runtime-xdg-state"),
	}
	for name, value := range activation.Environment {
		environment[name] = value
	}
	// The host test directory represents the /workspace volume mount in the Runtime Process.
	environment["MISE_IGNORED_CONFIG_PATHS"] = paths.Workspace

	configOutput := runMise(t, miseExecutable, paths.Workspace, environment, "config", "ls")
	if strings.Contains(configOutput, paths.Workspace) {
		t.Fatalf("runtime mise discovered feature-workspace config:\n%s", configOutput)
	}
	envOutput := runMise(t, miseExecutable, paths.Workspace, environment, "env", "--json")
	var runtimeEnvironment map[string]string
	if err := json.Unmarshal([]byte(envOutput), &runtimeEnvironment); err != nil {
		t.Fatalf("decode runtime mise environment %q: %v", envOutput, err)
	}
	if got := runtimeEnvironment["OMNIGREX_UNTRUSTED_MISE_CONFIG"]; got != "" {
		t.Fatalf("runtime mise evaluated feature-workspace config: %q", got)
	}

	marker := filepath.Join(paths.Workspace, "runtime-mise-invoked")
	runMise(t, miseExecutable, paths.Workspace, environment, "x", "--", "/bin/sh", "-c",
		`test -z "${OMNIGREX_UNTRUSTED_MISE_CONFIG:-}" && test "$TOOL_MODE" = trusted && printf invoked > "$1"`,
		"runtime-check", marker)
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "invoked" {
		t.Fatalf("runtime mise invocation marker = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(activation.DataDir, "installs", "node")); !os.IsNotExist(err) {
		t.Fatalf("runtime mise triggered a feature-workspace tool install: %v", err)
	}
}

func runMise(t *testing.T, executable, directory string, environment map[string]string, arguments ...string) string {
	t.Helper()
	command := exec.Command(executable, arguments...)
	command.Dir = directory
	command.Env = make([]string, 0, len(environment))
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("mise %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestLifecycleProvisionMiseReplacesAgentCreatedDataWithoutFollowingSymlinks(t *testing.T) {
	fixture := newGitFixture(t)
	root := t.TempDir()
	fakeMise := filepath.Join(root, "mise")
	writeExecutable(t, fakeMise, `#!/bin/sh
set -eu
mkdir -p "$MISE_CACHE_DIR"
printf 'provisioned' > "$MISE_CACHE_DIR/marker"
if [ "$1" = "env" ]; then
  printf '{}\n'
fi
`)
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"),
		MiseRoot: filepath.Join(root, "mise-data"), MiseExecutable: fakeMise,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	externalMarker := filepath.Join(external, "marker")
	if err := os.WriteFile(externalMarker, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Mise, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(paths.Mise, "cache")); err != nil {
		t.Fatal(err)
	}

	if _, err := lifecycle.ProvisionMise(context.Background(), workspace.MiseProvision{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first,
	}); err != nil {
		t.Fatalf("ProvisionMise() error = %v", err)
	}
	content, err := os.ReadFile(externalMarker)
	if err != nil || string(content) != "outside" {
		t.Fatalf("external marker = %q, %v", content, err)
	}
	cacheInfo, err := os.Lstat(filepath.Join(paths.Mise, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !cacheInfo.IsDir() || cacheInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("provisioned cache mode = %v, want fresh directory", cacheInfo.Mode())
	}
	content, err = os.ReadFile(filepath.Join(paths.Mise, "cache", "marker"))
	if err != nil || string(content) != "provisioned" {
		t.Fatalf("assignment marker = %q, %v", content, err)
	}
}
