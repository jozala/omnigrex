//go:build integration

package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestRetainedRepositoryEnvironmentContinuesSessionAndWorkingRevisionTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	// Model a retained pre-Step07 image: the pinned runtime and mise exist, but
	// neither the new bootstrap nor its qualification archive was packaged.
	dockerfile := `FROM omnigrex/opencode:1.18.29
USER root
RUN rm /usr/local/bin/omnigrex-environment /opt/omnigrex/opencode.tar.gz
USER 10001:10001
`
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := "omnigrex/retained-environment-test:" + uuid.NewString()
	if output, err := exec.CommandContext(ctx, "docker", "build", "-t", tag, root).CombinedOutput(); err != nil {
		t.Fatalf("build retained image: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", tag).Run() })
	identity, err := exec.CommandContext(ctx, "docker", "image", "inspect", tag, "--format", "{{.Id}} {{.Architecture}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(identity))
	if len(fields) != 2 {
		t.Fatal("retained image identity is incomplete")
	}
	deployment, err := exec.CommandContext(ctx, "docker", "image", "inspect", "omnigrex/opencode:1.18.29", "--format", "{{.Id}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapImage := strings.TrimSpace(string(deployment))
	current, err := profile.NewOpenCodeV1("registry.example/current@"+bootstrapImage, profile.Platform{OS: "linux", Arch: fields[1]})
	if err != nil {
		t.Fatal(err)
	}
	contract := current.Contract()
	contract.Image = "registry.example/retained@" + fields[0]
	contract.RepositoryEnvironment = nil
	legacy, err := profile.New(contract)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := profile.NewCatalog([]profile.Profile{current}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.ResolveBinding(legacy.Binding())
	if err != nil || resolved.Binding() != legacy.Binding() {
		t.Fatalf("resolve retained identity: %v", err)
	}
	fixture := newWorkspaceDockerFixture(t, nil)
	if output, err := exec.CommandContext(ctx, "docker", "stop", fixture.id).CombinedOutput(); err != nil {
		t.Fatalf("stop seed writer: %v\n%s", err, output)
	}
	rendered, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{Instructions: "Continue the retained Session.", Model: "openai/gpt-5", Steps: 1, Permissions: opencode.PermissionPolicy{"read": opencode.PermissionAllow}})
	if err != nil {
		t.Fatal(err)
	}
	factory := ExecutionFactory{Network: "none", WorkspaceVolume: fixture.volume, RuntimeStateVolume: fixture.volume, MiseVolume: fixture.volume}
	start := func(request execution.Request) (execution.Process, *acp.Client, string) {
		t.Helper()
		ledger := newExecutionTestLedger()
		engine, err := factory.New(request, ledger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = engine.Close() })
		var stderr executionTestOutput
		process, err := engine.Create(ctx, request, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = process.Stop(cleanupCtx, time.Second)
			_ = process.Remove(cleanupCtx)
		})
		if err := process.Start(ctx); err != nil {
			t.Fatal(err)
		}
		client := acp.NewClient(process.Transport(), acp.ClientOptions{})
		t.Cleanup(func() { _ = client.Close() })
		if _, err := client.Initialize(ctx); err != nil {
			t.Fatalf("initialize retained runtime: %v\n%s", err, stderr.String())
		}
		return process, client, ledger.records[request.Key].Resource
	}
	// Establish the original Session without any Step07 provisioning resources.
	key := fixture.key
	key.AgentTurnID = uuid.NewString()
	first, err := opencode.BuildProcess(legacy, rendered, opencode.ProviderCredentials{Role: opencode.RoleDeveloper, Content: []byte(`{}`)}, opencode.ProcessOptions{Key: key, MemoryBytes: 512 << 20})
	if err != nil {
		t.Fatal(err)
	}
	first.Image = fields[0] // Disposable local-ID fixture only.
	process, client, _ := start(first)
	created, err := client.CreateSession(ctx, acp.CreateSessionRequest{CWD: "/workspace"})
	if err != nil || created.ID == "" {
		t.Fatalf("create original Session: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := process.Stop(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := process.Remove(ctx); err != nil {
		t.Fatal(err)
	}

	// The next working revision installs a tool and attempts to replace assigned
	// cache paths. Neither the Participant image nor its Session may change.
	seed := `set -eu
cd /workspace/assignment-` + key.AssignmentID + `/workspace
printf '[env]\nREVISION_TOOLS = "next-working-revision"\nGOCACHE = "/workspace/poison-cache"\nGOPATH = "/tmp/poison-gopath"\n_.path = ["/home/opencode/.local/share/mise/revision-bin"]\n[hooks]\npostinstall = "sh ./install.sh"\n' > mise.toml
cat > install.sh <<'INSTALL'
set -eu
test "$(id -u)" = 10001
mkdir -p "$MISE_DATA_DIR/revision-bin"
printf '#!/bin/sh\nprintf "%%s\\n" next-working-revision\n' > "$MISE_DATA_DIR/revision-bin/revision-tool"
chmod 0755 "$MISE_DATA_DIR/revision-bin/revision-tool"
printf installed-in-retained-image > installation-proof
INSTALL
`
	if output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "10001:10001", "--mount", "type=volume,src="+fixture.volume+",dst=/workspace", "--entrypoint", "/bin/sh", tag, "-ec", seed).CombinedOutput(); err != nil {
		t.Fatalf("prepare next working revision: %v\n%s", err, output)
	}
	key.AgentTurnID = uuid.NewString()
	key.ExecutionEpoch++
	paths := map[string]string{"GOCACHE": "/home/opencode/.local/share/omnigrex-tool-data/assignment/go-cache", "GOPATH": "/home/opencode/.local/share/omnigrex-tool-data/assignment/go-path"}
	next, err := opencode.BuildProcess(resolved, rendered, opencode.ProviderCredentials{Role: opencode.RoleDeveloper, Content: []byte(`{}`)}, opencode.ProcessOptions{Key: key, MemoryBytes: 512 << 20, Environment: paths})
	if err != nil {
		t.Fatal(err)
	}
	next.Image = first.Image
	next.Command = []string{environment.RetainedBootstrapExecutable, "run", "acp"}
	next.Environment = environment.BindEnvironment(next.Environment)
	next.BootstrapImage = bootstrapImage
	next.Requirements = []execution.Requirement{execution.ToolInstallation}
	_, continued, resource := start(next)
	if err := continued.ContinueSession(ctx, acp.ContinueSessionRequest{SessionID: created.ID, CWD: "/workspace"}); err != nil {
		t.Fatalf("continue original Session after provisioning: %v", err)
	}
	check := `set -eu
test ! -e /usr/local/bin/omnigrex-environment
test "$(cat /workspace/installation-proof)" = installed-in-retained-image
pid="$(pgrep -f '^/usr/local/bin/opencode acp' | tail -n 1)"
tr '\000' '\n' < "/proc/$pid/environ" | grep '^GOCACHE=` + paths["GOCACHE"] + `$'
tr '\000' '\n' < "/proc/$pid/environ" | grep '^GOPATH=` + paths["GOPATH"] + `$'
tr '\000' '\n' < "/proc/$pid/environ" | grep '^PATH=/home/opencode/.local/share/mise/revision-bin:'
test -x /home/opencode/.local/share/mise/revision-bin/revision-tool
runtime_path="$(tr '\000' '\n' < "/proc/$pid/environ" | grep '^PATH=' | cut -d '=' -f 2-)"
test "$(PATH="$runtime_path" revision-tool)" = next-working-revision
`
	if output, err := exec.CommandContext(ctx, "docker", "exec", resource, "/bin/sh", "-ec", check).CombinedOutput(); err != nil {
		t.Fatalf("retained provisioning and final cache-path proof: %v\n%s", err, output)
	}
	actual, err := exec.CommandContext(ctx, "docker", "inspect", resource, "--format", "{{.Config.Image}}").Output()
	if err != nil || strings.TrimSpace(string(actual)) != first.Image || resolved.Binding() != legacy.Binding() {
		t.Fatalf("upgrade rebound retained image or contract: %s %v", actual, err)
	}
}
