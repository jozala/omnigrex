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

func TestRepositoryEnvironmentQualifiesResourcesAndInstallsWorkingRevisionTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	// A separately built repository image supplies one extra development tool.
	dockerfile := `FROM omnigrex/opencode:1.18.29
USER root
RUN printf '#!/bin/sh\nprintf "repository-tool-v1\\n"\n' > /usr/local/bin/repository-tool && chmod 0755 /usr/local/bin/repository-tool
USER 10001:10001
`
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := "omnigrex/environment-test:" + uuid.NewString()
	if output, err := exec.CommandContext(ctx, "docker", "build", "-t", tag, root).CombinedOutput(); err != nil {
		t.Fatalf("build repository image: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", tag).Run() })
	image, err := exec.CommandContext(ctx, "docker", "image", "inspect", tag, "--format", "{{.Id}} {{.Architecture}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(image))
	if len(fields) != 2 {
		t.Fatal("repository image identity is incomplete")
	}
	bound, err := profile.NewOpenCodeV1("registry.example/repository@"+fields[0], profile.Platform{OS: "linux", Arch: fields[1]})
	if err != nil {
		t.Fatal(err)
	}
	// Local image IDs are allowed only by this disposable integration fixture.
	qualify, err := environment.QualificationScript(fields[1])
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "/bin/sh", tag, "-ec", qualify).CombinedOutput(); err != nil {
		t.Fatalf("qualify repository image: %v\n%s", err, output)
	}
	// An image with valid ancestry and an unchanged version claim still fails
	// qualification if it replaces the actual pinned runtime executable.
	tamperedTag := tag + "-tampered"
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(dockerfile+"USER root\nRUN printf '#!/bin/sh\\nprintf \\\"1.18.29\\\\n\\\"\\n' > /usr/local/bin/opencode\nUSER 10001:10001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "docker", "build", "-t", tamperedTag, root).CombinedOutput(); err != nil {
		t.Fatalf("build tampered image: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", tamperedTag).Run() })
	if _, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "/bin/sh", tamperedTag, "-ec", qualify).CombinedOutput(); err == nil {
		t.Fatal("accepted a replaced golden runtime executable")
	}
	for _, role := range []opencode.Role{opencode.RoleDeveloper, opencode.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			fixture := newWorkspaceDockerFixture(t, nil)
			// Stop the seed writer before launching the next exact environment.
			if output, err := exec.CommandContext(ctx, "docker", "stop", fixture.id).CombinedOutput(); err != nil {
				t.Fatalf("stop fixture: %v %s", err, output)
			}
			seed := `set -eu
mkdir -p /workspace/assignment-` + fixture.key.AssignmentID + `/workspace
cd /workspace/assignment-` + fixture.key.AssignmentID + `/workspace
printf '[env]\nREVISION_TOOLS = "working-revision"\n[hooks]\npostinstall = "sh ./install.sh"\n' > mise.toml
printf 'set -eu\ntest "$(id -u)" = 10001\ntest -z "${OPENCODE_AUTH_CONTENT:-}"\ntest "$(repository-tool)" = repository-tool-v1\nprintf installed-in-environment > installation-proof\n' > install.sh
`
			if output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "10001:10001", "--mount", "type=volume,src="+fixture.volume+",dst=/workspace", "--entrypoint", "/bin/sh", tag, "-ec", seed).CombinedOutput(); err != nil {
				t.Fatalf("seed working tools: %v\n%s", err, output)
			}
			rendered, err := opencode.Render(role, opencode.Profile{Instructions: "Test repository environment.", Model: "openai/gpt-5", Steps: 1, Permissions: opencode.PermissionPolicy{"read": opencode.PermissionAllow}})
			if err != nil {
				t.Fatal(err)
			}
			key := fixture.key
			key.AgentTurnID = uuid.NewString()
			request, err := opencode.BuildProcess(bound, rendered, opencode.ProviderCredentials{Role: role, Content: []byte(`{}`)}, opencode.ProcessOptions{Key: key, MemoryBytes: 512 << 20})
			if err != nil {
				t.Fatal(err)
			}
			request.Image = fields[0]
			request.Command = []string{environment.Executable, "run", "acp"}
			request.Environment = environment.BindEnvironment(request.Environment)
			request.GoldenResources = true
			request.Requirements = []execution.Requirement{execution.ToolInstallation}
			factory := ExecutionFactory{Network: "none", WorkspaceVolume: fixture.volume, RuntimeStateVolume: fixture.volume, MiseVolume: fixture.volume}
			ledger := newExecutionTestLedger()
			engine, err := factory.New(request, ledger)
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			var stderr executionTestOutput
			process, err := engine.Create(ctx, request, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = process.Stop(context.Background(), time.Second); _ = process.Remove(context.Background()) }()
			if err := process.Start(ctx); err != nil {
				t.Fatal(err)
			}
			client := acp.NewClient(process.Transport(), acp.ClientOptions{})
			defer client.Close()
			if _, err := client.Initialize(ctx); err != nil {
				t.Fatalf("initialize after tool installation: %v\n%s", err, stderr.String())
			}
			check := `test "$(cat /workspace/installation-proof)" = installed-in-environment; tr '\000' '\n' < /proc/$(pgrep -f '^/usr/local/bin/opencode acp' | tail -n 1)/environ | grep '^REVISION_TOOLS=working-revision$'`
			output, err := exec.CommandContext(ctx, "docker", "exec", ledger.records[key].Resource, "/bin/sh", "-ec", check).CombinedOutput()
			if err != nil {
				t.Fatalf("working revision tool proof: %v\n%s", err, output)
			}
		})
	}
	// An unrelated image cannot qualify merely by claiming the runtime version.
	bad, _ := profile.NewOpenCodeV1("docker.io/library/alpine@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b", profile.Platform{OS: "linux", Arch: fields[1]})
	if err := CheckRepositoryEnvironment(ctx, bad); err == nil {
		t.Fatal("accepted image without golden resources")
	}
}
