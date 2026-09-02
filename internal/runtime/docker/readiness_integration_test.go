//go:build integration

package docker_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
)

func TestReadinessProbeExercisesRealDockerResources(t *testing.T) {
	image := environmentOrDefault("OMNIGREX_OPENCODE_IMAGE", "omnigrex/opencode:1.18.19")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	network := "omnigrex-readiness-agent-" + suffix
	workspace := "omnigrex-readiness-workspaces-" + suffix
	runtimeState := "omnigrex-readiness-runtime-state-" + suffix
	mise := "omnigrex-readiness-mise-" + suffix

	dockerCommand(t, "network", "create", network)
	t.Cleanup(func() { dockerCleanup(t, "network", "rm", network) })
	for _, volume := range []string{workspace, runtimeState, mise} {
		dockerCommand(t, "volume", "create", volume)
		volume := volume
		t.Cleanup(func() { dockerCleanup(t, "volume", "rm", "--force", volume) })
	}
	initializeVolumeRoots(t, image, workspace, runtimeState, mise)

	probe, err := dockerruntime.NewReadinessProbe(dockerruntime.ReadinessProbeOptions{
		AgentNetwork:       network,
		WorkspaceVolume:    workspace,
		RuntimeStateVolume: runtimeState,
		MiseVolume:         mise,
		AgentImage:         image,
	})
	if err != nil {
		t.Fatalf("NewReadinessProbe() error = %v", err)
	}
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Errorf("close readiness probe: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := probe.Check(ctx); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	assertReadinessCleanup(t, image, workspace, runtimeState, mise)
}

func initializeVolumeRoots(t *testing.T, image, workspace, runtimeState, mise string) {
	t.Helper()
	dockerCommand(t,
		"run", "--rm",
		"--user", "10001:10001",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", "none",
		"--mount", "type=volume,src="+workspace+",dst=/workspace",
		"--mount", "type=volume,src="+runtimeState+",dst=/home/opencode/.local/share/opencode",
		"--mount", "type=volume,src="+mise+",dst=/home/opencode/.local/share/mise",
		"--entrypoint", "sh",
		image,
		"-c", "for path in /workspace /home/opencode/.local/share/opencode /home/opencode/.local/share/mise; do touch \"$path/.volume-root-probe\" && rm \"$path/.volume-root-probe\"; done",
	)
}

func assertReadinessCleanup(t *testing.T, image, workspace, runtimeState, mise string) {
	t.Helper()
	dockerCommand(t,
		"run", "--rm",
		"--user", "10001:10001",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", "none",
		"--mount", "type=volume,src="+workspace+",dst=/workspace",
		"--mount", "type=volume,src="+runtimeState+",dst=/home/opencode/.local/share/opencode",
		"--mount", "type=volume,src="+mise+",dst=/home/opencode/.local/share/mise",
		"--entrypoint", "sh",
		image,
		"-c", "for root in /workspace /home/opencode/.local/share/opencode /home/opencode/.local/share/mise; do set -- \"$root\"/readiness-*; test ! -e \"$1\"; done",
	)
}

func dockerCommand(t *testing.T, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func dockerCleanup(t *testing.T, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput(); err != nil {
		t.Errorf("docker cleanup %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func environmentOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
