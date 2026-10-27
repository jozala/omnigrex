//go:build integration

package docker

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestRepositoryEnvironmentQualifiesResourcesAndInstallsWorkingRevisionTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	// A separately built repository image supplies one extra development tool.
	dockerfile := `FROM omnigrex/pi:1.4.0
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
	bound, err := profile.NewPiV2("registry.example/repository@"+fields[0], profile.Platform{OS: "linux", Arch: fields[1]})
	if err != nil {
		t.Fatal(err)
	}
	// Local image IDs are allowed only by this disposable integration fixture.
	qualify, err := environment.QualificationScript(fields[1], "pi-rpc", bound.Contract().GoldenResourceContract)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "/bin/sh", tag, "-ec", qualify).CombinedOutput(); err != nil {
		t.Fatalf("qualify repository image: %v\n%s", err, output)
	}
	// An image with valid ancestry and an unchanged version claim still fails
	// qualification if it replaces the actual pinned runtime executable.
	tamperedTag := tag + "-tampered"
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(dockerfile+"USER root\nRUN printf 'throw new Error(\"replaced runner\");\\n' > /opt/omnigrex/pi/runner.mjs\nUSER 10001:10001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "docker", "build", "-t", tamperedTag, root).CombinedOutput(); err != nil {
		t.Fatalf("build tampered image: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", tamperedTag).Run() })
	if _, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "/bin/sh", tamperedTag, "-ec", qualify).CombinedOutput(); err == nil {
		t.Fatal("accepted a replaced golden runtime executable")
	}
	for _, roleID := range []role.ID{role.Developer, role.Reviewer} {
		t.Run(string(roleID), func(t *testing.T) {
			fixture := newWorkspaceDockerFixture(t, nil)
			// Stop the seed writer before launching the next exact environment.
			if output, err := exec.CommandContext(ctx, "docker", "stop", fixture.id).CombinedOutput(); err != nil {
				t.Fatalf("stop fixture: %v %s", err, output)
			}
			seed := `set -eu
mkdir -p /workspace/assignment-` + fixture.key.AssignmentID + `/workspace
cd /workspace/assignment-` + fixture.key.AssignmentID + `/workspace
printf '[env]\nREVISION_TOOLS = "working-revision"\n[hooks]\npostinstall = "sh ./install.sh"\n' > mise.toml
printf 'set -eu\ntest "$(id -u)" = 10001\ntest ! -S /var/run/docker.sock\ntest ! -e /run/pi-agent/auth.json\ntest "$(repository-tool)" = repository-tool-v1\nprintf installed-in-environment > installation-proof\n' > install.sh
`
			if output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "10001:10001", "--mount", "type=volume,src="+fixture.volume+",dst=/workspace", "--entrypoint", "/bin/sh", tag, "-ec", seed).CombinedOutput(); err != nil {
				t.Fatalf("seed working tools: %v\n%s", err, output)
			}
			key := fixture.key
			key.AgentTurnID = uuid.NewString()
			request, err := (pi.TurnAdapter{}).BuildProcess(bound, key, "", 512<<20, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Image = fields[0]
			request.Command = []string{environment.Executable, "run", "/bin/sh", "-ec", `test "$(cat /workspace/installation-proof)" = installed-in-environment; test "$REVISION_TOOLS" = working-revision; test ! -S /var/run/docker.sock; printf 'working-revision-ready\n'; exec sleep 300`}
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
			stopRead := context.AfterFunc(ctx, func() { _ = process.Transport().Close() })
			defer stopRead()
			line, err := bufio.NewReader(process.Transport()).ReadString('\n')
			if err != nil || line != "working-revision-ready\n" {
				t.Fatalf("working revision tool proof: %q, %v\n%s", line, err, stderr.String())
			}
		})
	}
	// An unrelated image cannot qualify merely by claiming the runtime version.
	bad, _ := profile.NewPiV2("docker.io/library/alpine@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b", profile.Platform{OS: "linux", Arch: fields[1]})
	if err := CheckRepositoryEnvironment(ctx, bad); err == nil {
		t.Fatal("accepted image without golden resources")
	}
}
